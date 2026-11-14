//go:build linux

package wayviaegress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

type agentRecord struct {
	AgentRequest
	Retired bool `json:"retired"`
}
type addressAgent struct {
	config  Config
	mu      sync.Mutex
	records map[string]agentRecord
}
type peerKey struct{}

// RunAgent 只能由宿主root启动；请求仅能操作固定网卡和批准前缀。
func RunAgent(ctx context.Context, c Config) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("地址辅助服务需要宿主root")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(c.AgentStateDir, 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.Socket), 0750); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(c.AgentStateDir, "agent.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("地址辅助服务已在运行")
	}
	a := &addressAgent{config: c, records: map[string]agentRecord{}}
	b, err := os.ReadFile(filepath.Join(c.AgentStateDir, "addresses.json"))
	if err == nil {
		if err = json.Unmarshal(b, &a.records); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if a.records == nil {
		return fmt.Errorf("invalid address manifest")
	}
	for address, record := range a.records {
		ip, err := netip.ParseAddr(address)
		if err != nil || c.excluded(ip) || record.NodeID != c.NodeID || record.Version < 1 {
			return fmt.Errorf("manifest/config mismatch")
		}
	}
	if err = os.Chmod(filepath.Dir(c.Socket), 0750); err != nil {
		return err
	}
	if info, err := os.Lstat(c.Socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("socket路径已有非socket文件")
		}
		if err = os.Remove(c.Socket); err != nil {
			return err
		}
	}
	l, err := net.Listen("unix", c.Socket)
	if err != nil {
		return err
	}
	defer l.Close()
	defer os.Remove(c.Socket)
	if err = os.Chown(filepath.Dir(c.Socket), 0, c.AppGID); err != nil {
		return err
	}
	if err = os.Chown(c.Socket, 0, c.AppGID); err != nil {
		return err
	}
	if err = os.Chmod(c.Socket, 0660); err != nil {
		return err
	}
	s := &http.Server{ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8192}
	s.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		u, ok := conn.(*net.UnixConn)
		if !ok {
			return ctx
		}
		raw, e := u.SyscallConn()
		if e != nil {
			return ctx
		}
		var cred *unix.Ucred
		_ = raw.Control(func(fd uintptr) { cred, _ = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
		if cred != nil {
			return context.WithValue(ctx, peerKey{}, cred.Uid)
		}
		return ctx
	}
	s.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, ok := r.Context().Value(peerKey{}).(uint32)
		if !ok || uid != c.AppUID {
			http.Error(w, "peer denied", 403)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/address" {
			http.NotFound(w, r)
			return
		}
		var q AgentRequest
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if err := d.Decode(&q); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		callCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := a.apply(callCtx, q); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ready":true}`))
	})
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdownCtx)
	}()
	err = s.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func (a *addressAgent) apply(ctx context.Context, q AgentRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	ip, err := netip.ParseAddr(q.Address)
	if err != nil || !publicIPv6(ip) || a.config.excluded(ip) {
		return fmt.Errorf("address denied")
	}
	if q.NodeID != a.config.NodeID || q.Version < 1 {
		return fmt.Errorf("node or version denied")
	}
	if _, err = uuid.Parse(q.Owner); err != nil {
		return fmt.Errorf("owner denied")
	}
	if _, err = uuid.Parse(q.Operation); err != nil {
		return fmt.Errorf("operation denied")
	}
	if q.Action != "prepare" && q.Action != "check" && q.Action != "retire" && q.Action != "restore" {
		return fmt.Errorf("action denied")
	}
	old, owned := a.records[ip.String()]
	if owned && (old.Owner != q.Owner || old.Version != q.Version) {
		return fmt.Errorf("ownership conflict")
	}
	primaryPresent, primaryReady, err := a.inspect(ctx, a.config.Primary)
	if err != nil || !primaryPresent || !primaryReady {
		return fmt.Errorf("primary IPv6 not ready")
	}
	present, ready, err := a.inspect(ctx, ip.String())
	if err != nil {
		return err
	}
	if q.Action == "check" {
		if !owned || !present || !ready || old.Retired {
			return fmt.Errorf("source not ready")
		}
		return nil
	}
	if q.Action == "retire" {
		if !owned {
			return fmt.Errorf("unowned address")
		}
		old.Retired = true
		a.records[ip.String()] = old
		// 自动退役保留地址与墓碑。实际移除需要停流后人工运维确认。
		return durableJSON(filepath.Join(a.config.AgentStateDir, "addresses.json"), a.records)
	}
	if q.Action == "restore" {
		if !owned {
			return fmt.Errorf("restore requires original ownership")
		}
		previous := old
		old.Retired = false
		old.AgentRequest = q
		a.records[ip.String()] = old
		if err = durableJSON(filepath.Join(a.config.AgentStateDir, "addresses.json"), a.records); err != nil {
			// 持久化未确认时继续保留内存墓碑，后续check不得误报可用。
			a.records[ip.String()] = previous
			return err
		}
	}
	if !owned {
		if present {
			return fmt.Errorf("refuse to adopt external address")
		}
		if len(a.records) >= a.config.MaxAddresses {
			return fmt.Errorf("address limit reached")
		}
		a.records[ip.String()] = agentRecord{AgentRequest: q}
		// 必须先记录所有权再添加地址，异常/重启不会把已添加地址误当作不存在。
		if err = durableJSON(filepath.Join(a.config.AgentStateDir, "addresses.json"), a.records); err != nil {
			delete(a.records, ip.String())
			return err
		}
	}
	if old.Retired {
		return fmt.Errorf("retired address cannot be reused")
	}
	if !present {
		if _, err = exec.CommandContext(ctx, "/usr/sbin/ip", "-6", "address", "add", ip.String()+"/128", "dev", a.config.Interface, "valid_lft", "forever", "preferred_lft", "0", "noprefixroute").CombinedOutput(); err != nil {
			return fmt.Errorf("address add failed")
		}
	}
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		present, ready, err = a.inspect(ctx, ip.String())
		if err != nil {
			return err
		}
		if present && ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
func (a *addressAgent) inspect(ctx context.Context, address string) (bool, bool, error) {
	b, err := exec.CommandContext(ctx, "/usr/sbin/ip", "-j", "-6", "address", "show", "dev", a.config.Interface).Output()
	if err != nil {
		return false, false, fmt.Errorf("interface inspection failed")
	}
	return inspectAddressJSON(b, address, address == a.config.Primary)
}
