//go:build !linux && !darwin

package wayviaegress

import "fmt"

func (m *Manager) acquireLock() error { return fmt.Errorf("该平台不支持固定 IPv6 出口部署") }
