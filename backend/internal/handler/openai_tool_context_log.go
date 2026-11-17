package handler

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// 仅在校验拒绝时记录有限的输入结构，不记录用户正文、工具参数/输出或原始调用 ID。
func openAIToolContextLogFields(body []byte) []zap.Field {
	input := gjson.GetBytes(body, "input")
	types := make(map[string]int)
	missing := make([]map[string]any, 0)
	index, missingCount := 0, 0
	input.ForEach(func(_, item gjson.Result) bool {
		typ := item.Get("type").String()
		category := typ
		switch typ {
		case "message", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "tool_call", "local_shell_call", "tool_search_call", "tool_search_output", "mcp_tool_call", "mcp_tool_call_output", "item_reference", "reasoning", "compaction":
		default:
			category = "other"
		}
		types[category]++
		switch typ {
		case "function_call_output", "custom_tool_call_output", "tool_search_output", "mcp_tool_call_output":
			id := item.Get("call_id")
			if id.Type != gjson.String || strings.TrimSpace(id.String()) == "" {
				missingCount++
				if len(missing) < 16 {
					state := "invalid_type"
					if !id.Exists() {
						state = "absent"
					} else if id.Type == gjson.String {
						state = "blank"
					}
					output := item.Get("output")
					shape := output.Type.String()
					if output.IsArray() {
						shape = "array"
					} else if output.IsObject() {
						shape = "object"
					}
					missing = append(missing, map[string]any{
						"index": index, "type": category, "call_id_state": state,
						"name":         openAIToolContextLogLabel(item.Get("name").String(), false),
						"namespace":    openAIToolContextLogLabel(item.Get("namespace").String(), true),
						"output_shape": shape,
					})
				}
			}
		}
		index++
		return true
	})
	return []zap.Field{
		zap.Int("input_item_count", index),
		zap.Any("input_item_types", types),
		zap.Int("missing_tool_output_count", missingCount),
		zap.Any("missing_tool_outputs", missing),
		zap.Bool("has_previous_response_id", strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != ""),
	}
}

func openAIToolContextLogLabel(value string, namespace bool) string {
	if value == "" {
		return "absent"
	}
	if namespace {
		switch value {
		case "agent", "functions", "codex_app", "codex_tui", "multi_tool_use":
			return value
		}
	} else {
		switch value {
		case "browser_tabs", "browser_info", "goal_objective", "untrusted_context", "codex_apps_open_page", "automation_update", "create_thread", "send_message_to_thread":
			return value
		}
		if codexSelectedPageName.MatchString(value) {
			return "codex_apps_selected_page_part"
		}
	}
	// 未知名称可能含用户自定义数据；保留摘要用于聚合同类错误。
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("other:%x", sum[:6])
}
