//go:build linux || darwin

package wayviaegress

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func (m *Manager) acquireLock() error {
	if err := os.MkdirAll(m.config.StateDir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(m.config.StateDir, "instance.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("同节点已有活动实例，拒绝运行固定 IPv6 出口: %w", err)
	}
	m.lock = f
	return nil
}
