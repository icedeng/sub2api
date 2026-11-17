package service

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 复用已有响应观察器，只收集终止证据，不改写业务流或增加上游请求。
type healthTerminalEvidence struct {
	responsesCompleted bool
	chatFinished       bool
	failed             bool
}

func (e *healthTerminalEvidence) observe(payload []byte, eventType string) {
	if !gjson.ValidBytes(payload) {
		return
	}
	data := gjson.ParseBytes(payload)
	if value := data.Get("error"); value.Exists() && value.Type != gjson.Null {
		e.failed = true
	}
	if value := data.Get("response.error"); value.Exists() && value.Type != gjson.Null {
		e.failed = true
	}
	switch eventType {
	case "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
		e.failed = true
	case "response.completed", "response.done":
		status := data.Get("response.status").String()
		if status == "" || status == "completed" {
			e.responsesCompleted = true
		} else {
			e.failed = true
		}
	case "":
		if data.Get("object").String() == "response" {
			e.responsesCompleted = data.Get("status").String() == "completed"
		}
		choices := data.Get("choices").Array()
		// 多候选/工具协议不作为首版单模型文本探针的替代证据。
		if len(choices) > 1 {
			e.failed = true
			return
		}
		for _, choice := range choices {
			if choice.Get("index").Int() != 0 {
				e.failed = true
				continue
			}
			switch choice.Get("finish_reason").String() {
			case "stop", "content_filter":
				e.chatFinished = true
			case "":
			default:
				e.failed = true
			}
		}
	}
}

func (s *WayviaHealthService) observeForwardSuccess(ctx context.Context, c *gin.Context, a *Account, result *OpenAIForwardResult) {
	if result == nil || ctx.Err() != nil || c == nil || c.Request == nil || c.Request.Context().Err() != nil {
		return
	}
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil || observer.healthTerminal.failed {
		return
	}
	endpoint := result.UpstreamEndpoint
	if endpoint == "" {
		endpoint = GetActualOpenAIUpstreamEndpoint(c)
	}
	complete := false
	switch endpoint {
	case "/responses", "/v1/responses":
		complete = observer.healthTerminal.responsesCompleted
	case "/chat/completions", "/v1/chat/completions":
		complete = observer.healthTerminal.chatFinished
	}
	if !complete {
		return
	}
	copy := *result
	copy.UpstreamEndpoint = endpoint
	s.ObserveSuccess(ctx, a, &copy, true)
}
