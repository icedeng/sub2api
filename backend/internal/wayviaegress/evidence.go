package wayviaegress

import (
	"context"
	"net"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Evidence 是本机实际 socket 证据，不声称上游观测或模型完成。
type Evidence struct {
	ID             int64     `json:"id"`
	Owner          string    `json:"-"`
	AccountID      int64     `json:"account_id"`
	OccurredAt     time.Time `json:"occurred_at"`
	RequestID      string    `json:"request_id"`
	ConnectionID   string    `json:"connection_id"`
	Purpose        string    `json:"purpose"`
	Protocol       string    `json:"protocol"`
	Event          string    `json:"event"`
	Source         string    `json:"source_ipv6"`
	Remote         string    `json:"remote_address"`
	Host           string    `json:"target_host"`
	Reused         bool      `json:"reused"`
	PolicyVersion  int64     `json:"policy_version"`
	BindingVersion int64     `json:"binding_version"`
	HTTPStatus     int       `json:"http_status"`
	ErrorCode      string    `json:"error_code"`
}

type evidenceTrace struct {
	mu      sync.Mutex
	value   Evidence
	manager *Manager
	route   Route
}

func newEvidenceTrace(m *Manager, r Route, host string) *evidenceTrace {
	return &evidenceTrace{manager: m, route: r, value: Evidence{Owner: r.Owner, AccountID: r.AccountID, RequestID: uuid.NewString(), Purpose: r.Purpose, Host: host, PolicyVersion: r.PolicyVersion, BindingVersion: r.BindingVersion}}
}
func (t *evidenceTrace) gotConn(info httptrace.GotConnInfo) {
	if info.Conn == nil {
		return
	}
	local, ok := info.Conn.LocalAddr().(*net.TCPAddr)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.value.Source = local.IP.String()
	t.value.Remote = info.Conn.RemoteAddr().String()
	t.value.Reused = info.Reused
	// TLS/指纹 TLS 包装后仍通过真实 socket 两端匹配，不能依赖外层 Conn 的具体类型。
	t.manager.mu.Lock()
	if t.route.epoch != nil {
		for c := range t.route.epoch.connections {
			if c.LocalAddr().String() == info.Conn.LocalAddr().String() && c.RemoteAddr().String() == info.Conn.RemoteAddr().String() {
				t.value.ConnectionID = c.id
				break
			}
		}
	}
	t.manager.mu.Unlock()
	if !local.IP.Equal(net.IP(t.route.Source.AsSlice())) {
		// 若某条传输路径绕过了受控拨号，真实连接不匹配时在发送业务前关门。
		_ = info.Conn.Close()
		t.route.ReportFailure(context.Background(), Error("EGRESS_SOURCE_CONFLICT", "真实业务连接的源地址与绑定不符"))
	}
}
func (t *evidenceTrace) snapshot() Evidence { t.mu.Lock(); defer t.mu.Unlock(); return t.value }
func (t *evidenceTrace) record(protocol, event string, status int, err error) {
	e := t.snapshot()
	e.Protocol = protocol
	e.Event = event
	e.HTTPStatus = status
	e.OccurredAt = time.Now().UTC()
	if e.Source == "" {
		e.Event = "connection_unavailable"
	}
	if err != nil {
		e.ErrorCode = evidenceError(err)
	}
	t.manager.enqueueEvidence(e)
}
func evidenceError(err error) string {
	code, _, _ := strings.Cut(safeError(err), ":")
	if code == "EGRESS_OPERATION_FAILED" {
		return "EGRESS_NETWORK_ERROR"
	}
	return code
}

// SocketCapture 仅把成功 WS 握手时的真实连接传给同一个 WS 对象。
// 后续发送复用它，不重新探测，不读取消息正文。
type SocketCapture struct {
	mu       sync.Mutex
	evidence Evidence
	manager  *Manager
	writes   atomic.Uint64
}
type captureKey struct{}

func WithSocketCapture(ctx context.Context) (context.Context, *SocketCapture) {
	c := &SocketCapture{}
	return context.WithValue(ctx, captureKey{}, c), c
}
func (c *SocketCapture) capture(t *evidenceTrace) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evidence = t.snapshot()
	c.manager = t.manager
}
func (c *SocketCapture) Sent(r Route, ctx context.Context, err error) {
	if c == nil || !r.Strict() {
		return
	}
	if err != nil {
		r.ReportFailure(ctx, err)
	}
	c.mu.Lock()
	e, m := c.evidence, c.manager
	c.mu.Unlock()
	if m == nil {
		return
	}
	e.RequestID = uuid.NewString()
	e.Purpose = "business"
	if _, purpose := AccountContext(ctx); purpose == "prewarm" {
		e.Purpose = "prewarm"
	}
	e.Protocol = "WS"
	e.OccurredAt = time.Now().UTC()
	e.Event = "ws_message_sent"
	e.Reused = c.writes.Add(1) > 1
	if err != nil {
		e.Event = "ws_send_failed"
		e.ErrorCode = evidenceError(err)
	}
	if e.Source == "" {
		e.Event = "connection_unavailable"
	}
	m.enqueueEvidence(e)
}

func (m *Manager) enqueueEvidence(e Evidence) {
	if m.evidenceQueue == nil || e.Owner == "" {
		return
	}
	select {
	case m.evidenceQueue <- e:
	default:
		m.evidenceDropped.Add(1)
	}
}
func (m *Manager) evidenceLoop(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastPrune := time.Time{}
	flush := func() {
		batch := make([]Evidence, 0, 128)
		for len(batch) < cap(batch) {
			select {
			case e := <-m.evidenceQueue:
				batch = append(batch, e)
			default:
				goto drained
			}
		}
	drained:
		prune := time.Since(lastPrune) > time.Minute
		// 空轮次没有访问数据库，不能据此把之前的保存故障显示为已恢复。
		if len(batch) == 0 && !prune {
			return
		}
		call, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.persistEvidence(call, batch, prune); err != nil {
			m.evidenceDropped.Add(uint64(len(batch)))
			m.evidenceUnavailable.Store(true)
		} else {
			m.evidenceUnavailable.Store(false)
			if prune {
				lastPrune = time.Now()
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			for len(m.evidenceQueue) > 0 {
				flush()
			}
			return
		case <-tick.C:
			flush()
		}
	}
}
func (m *Manager) persistEvidence(ctx context.Context, batch []Evidence, prune bool) error {
	if len(batch) == 0 && !prune {
		return nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range batch {
		_, err = tx.ExecContext(ctx, `INSERT INTO wayvia_egress_evidence(owner_id,account_id,occurred_at,request_id,connection_id,purpose,protocol,event,source_ipv6,remote_address,target_host,reused,policy_version,binding_version,http_status,error_code)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, e.Owner, e.AccountID, e.OccurredAt, e.RequestID, e.ConnectionID, e.Purpose, e.Protocol, e.Event, e.Source, e.Remote, e.Host, e.Reused, e.PolicyVersion, e.BindingVersion, e.HTTPStatus, e.ErrorCode)
		if err != nil {
			return err
		}
	}
	if prune {
		_, err = tx.ExecContext(ctx, `DELETE FROM wayvia_egress_evidence WHERE occurred_at<now()-interval '7 days'
		 OR id IN (SELECT id FROM (SELECT id,row_number() OVER(PARTITION BY owner_id ORDER BY id DESC) n FROM wayvia_egress_evidence) r WHERE n>200)
		 OR id IN (SELECT id FROM wayvia_egress_evidence ORDER BY id DESC OFFSET 50000)`)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

type EvidencePage struct {
	Items       []Evidence `json:"items"`
	Dropped     uint64     `json:"dropped_since_start"`
	Unavailable bool       `json:"persistence_unavailable"`
	Pending     int        `json:"pending"`
}

func (m *Manager) Evidence(ctx context.Context, id int64, limit int) (EvidencePage, error) {
	m.mu.Lock()
	a, p, err := m.rootLocked(id)
	m.mu.Unlock()
	out := EvidencePage{Items: []Evidence{}, Dropped: m.evidenceDropped.Load(), Unavailable: m.evidenceUnavailable.Load(), Pending: len(m.evidenceQueue)}
	if err != nil {
		return out, err
	}
	if !a.Eligible() || p.Owner == "" {
		return out, Error("EGRESS_POLICY_UNKNOWN", "账号没有固定出口策略")
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	// 母账号可查看同 owner 的影子请求；影子详情仅返回该影子的实际请求。
	rows, err := m.db.QueryContext(ctx, `SELECT id,account_id,occurred_at,request_id::text,connection_id,purpose,protocol,event,source_ipv6,remote_address,target_host,reused,policy_version,binding_version,http_status,error_code
	 FROM wayvia_egress_evidence WHERE owner_id=$1 AND ($2 OR account_id=$3) AND occurred_at>now()-interval '7 days' ORDER BY id DESC LIMIT $4`, p.Owner, a.ID == id, id, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Evidence
		if err = rows.Scan(&e.ID, &e.AccountID, &e.OccurredAt, &e.RequestID, &e.ConnectionID, &e.Purpose, &e.Protocol, &e.Event, &e.Source, &e.Remote, &e.Host, &e.Reused, &e.PolicyVersion, &e.BindingVersion, &e.HTTPStatus, &e.ErrorCode); err != nil {
			return out, err
		}
		out.Items = append(out.Items, e)
	}
	return out, rows.Err()
}
