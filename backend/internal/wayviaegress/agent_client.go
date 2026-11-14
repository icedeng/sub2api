package wayviaegress

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type AgentRequest struct {
	Action    string `json:"action"`
	NodeID    string `json:"node_id"`
	Owner     string `json:"owner_id"`
	Operation string `json:"operation_id"`
	Address   string `json:"address"`
	Version   int64  `json:"binding_version"`
}

func (m *Manager) agent(ctx context.Context, action string, p Policy) error {
	if m.config.Socket == "" {
		return Error("EGRESS_SOURCE_UNREADY", "未配置宿主地址辅助服务")
	}
	b, _ := json.Marshal(AgentRequest{action, m.config.NodeID, p.Owner, p.OperationID, p.Source, p.BindingVersion})
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", m.config.Socket)
	}}
	defer tr.CloseIdleConnections()
	c := http.Client{Transport: tr, Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/address", bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return Error("EGRESS_SOURCE_UNREADY", "宿主地址辅助服务不可用")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return Error("EGRESS_SOURCE_UNREADY", "宿主辅助服务暂时不可用")
	}
	if resp.StatusCode != 200 {
		// 兼容已部署的文本协议；仅固定白名单为可重试，所有权/DAD 等异常停止恢复。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		reason := strings.TrimSpace(string(body))
		switch reason {
		case "source not ready", "primary IPv6 not ready", "address add failed", "interface inspection failed", "context deadline exceeded":
			return Error("EGRESS_SOURCE_UNREADY", "宿主 IPv6 暂未就绪")
		case "ownership conflict", "refuse to adopt external address", "DAD failed":
			return Error("EGRESS_SOURCE_CONFLICT", "地址所有权或重复地址检查失败，需人工检查")
		default:
			return Error("EGRESS_HELPER_DENIED", "宿主辅助服务拒绝操作，请检查配置及地址归属")
		}
	}
	return nil
}
