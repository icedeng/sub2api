package handler

// WS 首轮和续轮共用 HTTP 入口的窄范围规则。真正的工具输出仍由调用上下文校验，
// 只有已识别的桌面页面快照、自动任务和委派注入才还原为不可信的用户消息。
func normalizeCodexWSClientInput(body []byte) []byte {
	if normalized, changed := normalizeCodexPageContext(body); changed {
		body = normalized
	}
	if normalized, changed := normalizeCodexAutomationBootstrap(body); changed {
		body = normalized
	}
	if normalized, changed := normalizeCodexDelegationBootstrap(body); changed {
		body = normalized
	}
	return body
}
