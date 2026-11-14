package wayviaegress

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
)

type Config struct {
	Prefix        string   `json:"prefix"`
	NodeID        string   `json:"node_id"`
	Interface     string   `json:"interface"`
	Primary       string   `json:"primary_ipv6"`
	Reserved      []string `json:"reserved_ipv6"`
	MaxAddresses  int      `json:"max_addresses"`
	Socket        string   `json:"socket"`
	StateDir      string   `json:"state_dir"`
	AgentStateDir string   `json:"agent_state_dir"`
	KeyFile       string   `json:"key_file"`
	KeyID         string   `json:"key_id"`
	ProbeURL      string   `json:"probe_url,omitempty"` // 兼容已部署的单地址配置。
	ProbeURLs     []string `json:"probe_urls,omitempty"`
	AppUID        uint32   `json:"app_uid"`
	AppGID        int      `json:"app_gid"`
}

func ReadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	p, err := netip.ParsePrefix(c.Prefix)
	if err != nil || p.Bits() != 64 || !publicIPv6(p.Addr()) || p != p.Masked() {
		return fmt.Errorf("需要规范公网 IPv6 /64 前缀")
	}
	a, err := netip.ParseAddr(c.Primary)
	if err != nil || !p.Contains(a) {
		return fmt.Errorf("普通主地址必须位于批准前缀")
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,64}$`).MatchString(c.NodeID) || !regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`).MatchString(c.Interface) || c.MaxAddresses < 1 || c.MaxAddresses > 65536 || c.AppUID == 0 || c.AppGID <= 0 {
		return fmt.Errorf("节点、网卡、非root应用身份和数量上限必填")
	}
	for _, s := range c.Reserved {
		if _, err := netip.ParseAddr(s); err != nil {
			return err
		}
	}
	for _, s := range []string{c.Socket, c.StateDir, c.AgentStateDir, c.KeyFile} {
		if !filepath.IsAbs(s) {
			return fmt.Errorf("持久路径必须是绝对路径")
		}
	}
	urls := c.probeURLs()
	if len(urls) == 0 || len(urls) > 8 {
		return fmt.Errorf("回显地址必须配置1至8个，按优先顺序排列")
	}
	seen := make(map[string]bool, len(urls))
	for i, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
			return fmt.Errorf("第%d个回显地址必须使用固定 HTTPS 443 地址", i+1)
		}
		if seen[raw] {
			return fmt.Errorf("回显地址不能重复")
		}
		seen[raw] = true
	}
	if c.StateDir == c.AgentStateDir || filepath.Dir(c.Socket) == c.StateDir {
		return fmt.Errorf("应用状态、辅助服务状态和socket目录必须分离")
	}
	if c.KeyID == "" {
		return fmt.Errorf("key_id 必填")
	}
	return nil
}

// 多地址列表优先；旧 probe_url 仅在未提供列表时生效，避免意外混入过期目标。
func (c Config) probeURLs() []string {
	if len(c.ProbeURLs) > 0 {
		return c.ProbeURLs
	}
	if c.ProbeURL != "" {
		return []string{c.ProbeURL}
	}
	return nil
}

func (c Config) excluded(a netip.Addr) bool {
	p, _ := netip.ParsePrefix(c.Prefix)
	if !p.Contains(a) || a == p.Addr() {
		return true
	}
	for _, s := range append([]string{c.Primary}, c.Reserved...) {
		if v, e := netip.ParseAddr(s); e == nil && a == v {
			return true
		}
	}
	return false
}

// 原子持久化先 fsync 文件，再 rename，再 fsync 目录，崩溃后不得丢失关门意图。
func durableJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".wayvia-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func removeDurable(path string) error {
	if e := os.Remove(path); e != nil && !os.IsNotExist(e) {
		return e
	}
	return syncDir(filepath.Dir(path))
}
