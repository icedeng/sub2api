//go:build !linux

package wayviaegress

import (
	"context"
	"fmt"
)

func RunAgent(context.Context, Config) error {
	return fmt.Errorf("wayvia-egress-agent仅支持Linux宿主机")
}
