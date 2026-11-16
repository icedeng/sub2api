package service

import (
	"context"
	"encoding/json"
	"errors"
	"runtime/debug"
	"sync"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIWSClientReaderKey = "openai_ws_client_pending_reader"

// 只保留一个在途读取，并跨账号重试复用，避免桥接退出后两个 goroutine 抢读同一连接。
type openAIWSClientReader struct {
	pending <-chan openAIWSClientReadResult
}

func startOpenAIWSClientRead(conn *coderws.Conn) <-chan openAIWSClientReadResult {
	ch := make(chan openAIWSClientReadResult, 1)
	go func() {
		typ, payload, err := conn.Read(context.Background())
		ch <- openAIWSClientReadResult{messageType: typ, payload: payload, err: err}
	}()
	return ch
}

func openAIWSClientReaderFromContext(c *gin.Context) *openAIWSClientReader {
	if value, ok := c.Get(openAIWSClientReaderKey); ok {
		if reader, ok := value.(*openAIWSClientReader); ok {
			return reader
		}
	}
	reader := &openAIWSClientReader{}
	c.Set(openAIWSClientReaderKey, reader)
	return reader
}

func (r *openAIWSClientReader) start(conn *coderws.Conn) <-chan openAIWSClientReadResult {
	if r.pending == nil {
		r.pending = startOpenAIWSClientRead(conn)
	}
	return r.pending
}

func (r *openAIWSClientReader) restore(result openAIWSClientReadResult) {
	ch := make(chan openAIWSClientReadResult, 1)
	ch <- result
	r.pending = ch
}

func openAIWSHTTPBridgeInterruptReply(payload []byte, responseID string) ([]byte, bool) {
	root := gjson.ParseBytes(payload)
	idValue, modeValue := root.Get("response_id"), root.Get("mode")
	requested, mode := idValue.String(), modeValue.String()
	accepted := root.IsObject() && !hasDuplicateJSONObjectKeys(root) && idValue.Type == gjson.String && requested != "" && requested == responseID &&
		(!modeValue.Exists() || (modeValue.Type == gjson.String && (mode == "" || mode == "discard_partial_items")))
	typ := "response.interrupt.failed"
	if accepted {
		typ = "response.interrupt.accepted"
	}
	event, _ := json.Marshal(map[string]any{"type": typ, "response_id": requested})
	return event, accepted
}

type openAIWSHTTPBridgeTurnOutcome struct {
	result *OpenAIForwardResult
	err    error
}

// done 事件也可能携带 incomplete/in_progress 项；中断模式只保留已完成正文。
func openAIWSHTTPBridgeCompletedOutput(collector *openAIWSToolCallReplayCollector) []json.RawMessage {
	items := collector.AllItems()
	complete := items[:0]
	for _, item := range items {
		status := gjson.GetBytes(item, "status").String()
		if status == "" || status == "completed" {
			complete = append(complete, item)
		}
	}
	return complete
}

// 同一互斥锁串行化中断与输出，确保 accepted 后不再写旧轮次的增量或第二个终态。
func runOpenAIWSHTTPBridgeControlledTurn(ctx context.Context, conn *coderws.Conn, reader *openAIWSClientReader, model string, write func([]byte) error, run func(context.Context, func([]byte) error) (*OpenAIForwardResult, error)) (*OpenAIForwardResult, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var responseID string
	var interrupted, terminal bool
	sequence := int64(-1)
	createdAt := time.Now().Unix()
	collector := &openAIWSToolCallReplayCollector{}
	writer := func(message []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if interrupted {
			return nil
		}
		typ, _, _ := parseOpenAIWSEventEnvelope(message)
		id := gjson.GetBytes(message, "response.id").String()
		if id != "" {
			responseID = id
		}
		if value := gjson.GetBytes(message, "sequence_number"); value.Exists() && value.Int() > sequence {
			sequence = value.Int()
		}
		if value := gjson.GetBytes(message, "response.created_at"); value.Int() > 0 {
			createdAt = value.Int()
		}
		if err := write(message); err != nil {
			return err
		}
		collector.AddEvent(typ, message)
		terminal = terminal || isOpenAIWSTerminalEvent(typ)
		return nil
	}
	done := make(chan openAIWSHTTPBridgeTurnOutcome, 1)
	go func() {
		var outcome openAIWSHTTPBridgeTurnOutcome
		defer func() {
			// 新 goroutine 不受 HTTP handler 的 recover 保护，异常须交回主循环关闭连接。
			if recover() != nil {
				logOpenAIWSModeWarn("ingress_ws_http_bridge_panic stack=%s", debug.Stack())
				outcome.err = NewOpenAIWSClientCloseError(coderws.StatusInternalError, "websocket http bridge internal error", errors.New("websocket http bridge turn panicked"))
			}
			done <- outcome
		}()
		outcome.result, outcome.err = run(turnCtx, writer)
	}()
	readDone := reader.start(conn)
	var controlErr error
	for {
		select {
		case outcome := <-done:
			if controlErr != nil {
				return outcome.result, controlErr
			}
			if interrupted {
				if outcome.result == nil {
					outcome.result = &OpenAIForwardResult{Model: model, OpenAIWSMode: true, Stream: true}
				}
				outcome.result.RequestID = responseID
				outcome.result.UpstreamTerminalEvent = "response.incomplete"
				outcome.result.ClientInterrupted = true
				outcome.result.wsReplayInput = openAIWSHTTPBridgeCompletedOutput(collector)
				outcome.result.wsReplayInputExists = true
				outcome.result.wsAccountFailoverReplayInput = outcome.result.wsReplayInput
				return outcome.result, nil
			}
			return outcome.result, outcome.err
		case frame := <-readDone:
			reader.pending = nil
			if frame.err != nil || !gjson.ValidBytes(frame.payload) || gjson.GetBytes(frame.payload, "type").String() != "response.interrupt" {
				// 下一次 create 留给主循环串行处理；客户端断开时保留既有用量排空行为。
				reader.restore(frame)
				readDone = nil
				continue
			}
			mu.Lock()
			reply, accepted := openAIWSHTTPBridgeInterruptReply(frame.payload, responseID)
			if accepted && !terminal && !interrupted {
				interrupted = true
				cancel()
			}
			controlErr = write(reply)
			if controlErr == nil && accepted && interrupted && !terminal {
				sequence++
				output := openAIWSHTTPBridgeCompletedOutput(collector)
				if output == nil {
					output = []json.RawMessage{}
				}
				event, _ := json.Marshal(map[string]any{
					"type": "response.incomplete", "sequence_number": sequence,
					"response": map[string]any{"id": responseID, "object": "response", "model": model, "created_at": createdAt,
						"status": "incomplete", "output": output, "incomplete_details": map[string]string{"reason": "interrupted"}},
				})
				controlErr = write(event)
				terminal = true
			}
			mu.Unlock()
			if controlErr != nil {
				cancel()
				readDone = nil
			} else {
				readDone = reader.start(conn)
			}
		case <-ctx.Done():
			cancel()
			outcome := <-done
			return outcome.result, errors.Join(outcome.err, context.Cause(ctx))
		}
	}
}
