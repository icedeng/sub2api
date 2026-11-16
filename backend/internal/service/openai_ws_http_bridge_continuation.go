package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 桥接只拥有当前连接的历史，不能把另一个响应的增量输入拼到最近一轮上。
type openAIWSHTTPBridgeContextError struct {
	code, param, message string
}

func (e *openAIWSHTTPBridgeContextError) Error() string { return e.message }

// IsOpenAIWSHTTPBridgeContextError 区分客户端上下文缺失与上游账号故障。
func IsOpenAIWSHTTPBridgeContextError(err error) bool {
	var contextErr *openAIWSHTTPBridgeContextError
	return errors.As(err, &contextErr)
}

func prepareOpenAIWSHTTPBridgeContinuation(payload []byte, previousResponseID string, history []json.RawMessage, historyExists bool) ([]byte, []json.RawMessage, bool, error) {
	root := gjson.ParseBytes(payload)
	if !root.IsObject() || hasDuplicateJSONObjectKeys(root) {
		return nil, nil, false, &openAIWSHTTPBridgeContextError{code: "invalid_request", param: "input", message: "Bridge payload must be an object with unambiguous fields."}
	}
	previous := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
	if previous != "" && (previous != previousResponseID || !historyExists) {
		return nil, nil, false, &openAIWSHTTPBridgeContextError{
			code: "previous_response_not_found", param: "previous_response_id",
			message: "Previous response context is unavailable on this HTTP bridge connection; resend the full input with previous_response_id unset.",
		}
	}
	items, exists, err := openAIWSExtractNormalizedInputSequence(payload)
	if err != nil {
		return nil, nil, false, err
	}
	for _, raw := range items {
		item := gjson.ParseBytes(raw)
		if item.IsObject() && hasDuplicateJSONObjectKeys(item) {
			return nil, nil, false, &openAIWSHTTPBridgeContextError{code: "invalid_request", param: "input", message: "Bridge input items must not contain duplicate fields."}
		}
		if id := item.Get("call_id"); id.Exists() && id.Type != gjson.String {
			return nil, nil, false, &openAIWSHTTPBridgeContextError{code: "invalid_request", param: "input", message: "Tool call_id must be a string."}
		}
	}
	coverage := AnalyzeToolCallOutputContextCoverageBytes(payload)
	needsHistory := previous != "" || (coverage.HasFunctionCallOutput && !coverage.ContextCoversAllCallIDs)
	originalItems := items
	if needsHistory && historyExists {
		items, err = restoreOpenAIWSHTTPBridgeReplayCallIDs(items, history)
		if err != nil {
			return nil, nil, false, err
		}
	}
	// 先补关联，再调用 replay 合并：合并会删除无结果配对的历史调用项。
	available := items
	if needsHistory && historyExists {
		available = combineOpenAIWSReplayItems(history, items)
	}
	items, err = resolveOpenAIWSHTTPBridgeToolOutputs(items, available)
	if err != nil {
		return nil, nil, false, err
	}
	merged, mergedExists := buildOpenAIWSReplayInputSequenceFromItems(history, historyExists, items, exists, needsHistory)
	if openAIWSRawItemsHasFunctionCallOutput(merged) && !openAIWSRawItemsHaveToolCallContextForOutputs(merged) {
		return nil, nil, false, &openAIWSHTTPBridgeContextError{
			code: "tool_call_context_missing", param: "input",
			message: "Tool outputs require matching tool calls in the full input; resend the full conversation after reconnecting.",
		}
	}
	// store=false 的 HTTP 请求无法依赖一个只有 ID、没有正文的服务端引用。
	for _, item := range merged {
		if gjson.GetBytes(item, "type").String() == "item_reference" {
			return nil, nil, false, &openAIWSHTTPBridgeContextError{
				code: "tool_call_context_missing", param: "input",
				message: "HTTP bridge requires full input items instead of unresolved item_reference entries.",
			}
		}
	}
	changed := needsHistory
	for i := range items {
		changed = changed || !bytes.Equal(items[i], originalItems[i])
	}
	if !changed {
		return payload, merged, mergedExists, nil
	}
	updated, err := setOpenAIWSPayloadInputSequence(payload, merged, mergedExists)
	return updated, merged, mergedExists, err
}

// 只有完整历史前缀一致时才恢复桥接先前补出的 ID，不能仅凭结果正文相同认领旧调用。
func restoreOpenAIWSHTTPBridgeReplayCallIDs(items, history []json.RawMessage) ([]json.RawMessage, error) {
	if len(history) == 0 || len(items) < len(history) {
		return items, nil
	}
	var restored []json.RawMessage
	calls := make(map[string]bool)
	for i, raw := range history {
		prior := gjson.ParseBytes(raw)
		id := strings.TrimSpace(prior.Get("call_id").String())
		current := gjson.ParseBytes(items[i])
		candidate := items[i]
		if isCodexToolCallOutputItemType(prior.Get("type").String()) && id != "" && calls[id] &&
			isCodexToolCallOutputItemType(current.Get("type").String()) && strings.TrimSpace(current.Get("call_id").String()) == "" {
			var err error
			candidate, err = sjson.SetBytes(candidate, "call_id", prior.Get("call_id").String())
			if err != nil {
				return nil, err
			}
		}
		if !bytes.Equal(bytes.TrimSpace(raw), bytes.TrimSpace(candidate)) &&
			!bytes.Equal(normalizeOpenAIWSJSONForCompareOrRaw(raw), normalizeOpenAIWSJSONForCompareOrRaw(candidate)) {
			// 任一历史项冲突都撤销暂存补齐，交回正常的唯一待处理调用校验。
			return items, nil
		}
		if !bytes.Equal(candidate, items[i]) {
			if restored == nil {
				restored = append([]json.RawMessage(nil), items...)
			}
			restored[i] = candidate
		}
		if isCodexToolCallContextItemType(prior.Get("type").String()) && id != "" {
			calls[id] = true
		}
	}
	if restored != nil {
		return restored, nil
	}
	return items, nil
}

// 缺省 call_id 仅能由唯一、尚未返回结果且类型/名称匹配的调用确定；不按数组位置猜测。
func resolveOpenAIWSHTTPBridgeToolOutputs(items, available []json.RawMessage) ([]json.RawMessage, error) {
	calls := make(map[string]gjson.Result)
	resolved := make(map[string]bool)
	ambiguous := make(map[string]bool)
	for _, raw := range available {
		item := gjson.ParseBytes(raw)
		id := strings.TrimSpace(item.Get("call_id").String())
		if id == "" {
			continue
		}
		switch {
		case isCodexToolCallContextItemType(item.Get("type").String()):
			if old, ok := calls[id]; ok && !bytes.Equal(normalizeOpenAIWSJSONForCompareOrRaw([]byte(old.Raw)), normalizeOpenAIWSJSONForCompareOrRaw(raw)) {
				ambiguous[id] = true
			}
			calls[id] = item
		case isCodexToolCallOutputItemType(item.Get("type").String()):
			resolved[id] = true
		}
	}
	result := append([]json.RawMessage(nil), items...)
	for i, raw := range items {
		item := gjson.ParseBytes(raw)
		typ := item.Get("type").String()
		if !isCodexToolCallOutputItemType(typ) || strings.TrimSpace(item.Get("call_id").String()) != "" {
			continue
		}
		var matches []string
		for id, call := range calls {
			if ambiguous[id] || resolved[id] || (call.Get("type").String()+"_output" != typ && !(typ == "tool_search_output" && call.Get("type").String() == "tool_search_call")) {
				continue
			}
			if name := item.Get("name").String(); name != "" && name != call.Get("name").String() {
				continue
			}
			if ns := item.Get("namespace").String(); ns != "" && ns != call.Get("namespace").String() {
				continue
			}
			matches = append(matches, id)
		}
		if len(matches) != 1 {
			return nil, &openAIWSHTTPBridgeContextError{
				code: "tool_call_context_missing", param: fmt.Sprintf("input.%d.call_id", i),
				message: "Tool output has no call_id and cannot be matched to exactly one pending tool call; resend its call_id and full tool-call context.",
			}
		}
		updated, err := sjson.SetBytes(raw, "call_id", matches[0])
		if err != nil {
			return nil, err
		}
		result[i] = updated
		resolved[matches[0]] = true
	}
	return result, nil
}
