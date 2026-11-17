package service

import (
	"strings"

	"github.com/tidwall/gjson"
)

// NormalizeOpenAIToolOutputCallIDs 仅从当前 HTTP 请求的完整调用上下文补齐唯一关联。
// 不查询其他连接的历史，也不推断 previous_response_id 隐含的调用。
func NormalizeOpenAIToolOutputCallIDs(payload []byte) ([]byte, bool, error) {
	root := parseRawJSONView(payload)
	if strings.TrimSpace(root.Get("previous_response_id").String()) != "" {
		return payload, false, nil
	}
	input := root.Get("input")
	if !input.IsArray() {
		return payload, false, nil
	}
	var missing, hasContext bool
	input.ForEach(func(_, item gjson.Result) bool {
		typ := item.Get("type").String()
		id := item.Get("call_id")
		if isCodexToolCallOutputItemType(typ) && (id.Type != gjson.String || strings.TrimSpace(id.String()) == "") {
			missing = true
		}
		if isCodexToolCallContextItemType(typ) && strings.TrimSpace(id.String()) != "" {
			hasContext = true
		}
		return !missing || !hasContext
	})
	if !missing || !hasContext {
		// 没有上下文的请求继续由原入口校验拒绝，不能凭空生成调用或 ID。
		return payload, false, nil
	}
	if !gjson.ValidBytes(payload) || !root.IsObject() || hasDuplicateJSONObjectKeys(root) {
		return nil, false, &openAIWSHTTPBridgeContextError{code: "invalid_request", param: "input", message: "Tool context must have unambiguous JSON fields."}
	}
	items, exists, err := openAIWSExtractNormalizedInputSequence(payload)
	if err != nil {
		return nil, false, err
	}
	for _, raw := range items {
		item := gjson.ParseBytes(raw)
		if item.IsObject() && hasDuplicateJSONObjectKeys(item) {
			return nil, false, &openAIWSHTTPBridgeContextError{code: "invalid_request", param: "input", message: "Tool context items must not contain duplicate fields."}
		}
		if id := item.Get("call_id"); id.Exists() && id.Type != gjson.String {
			return nil, false, &openAIWSHTTPBridgeContextError{code: "invalid_request", param: "input", message: "Tool call_id must be a string."}
		}
	}
	resolved, err := resolveOpenAIWSHTTPBridgeToolOutputs(items, items)
	if err != nil {
		return nil, false, err
	}
	updated, err := setOpenAIWSPayloadInputSequence(payload, resolved, exists)
	return updated, err == nil, err
}
