package wayviahealth

import (
	"context"
	"encoding/json"
	"errors"
)

var ErrStaleCredentials = errors.New("health result belongs to outdated credentials")

type credentialGuardKey struct{}

// WithCredentialGuard 给状态写入携带凭据快照，数据库 UPDATE 原子比较后才落状态。
func WithCredentialGuard(ctx context.Context, id int64, credentials map[string]any) context.Context {
	b, _ := json.Marshal(credentials)
	guards := map[int64]string{}
	if old, ok := ctx.Value(credentialGuardKey{}).(map[int64]string); ok {
		for k, v := range old {
			guards[k] = v
		}
	}
	guards[id] = string(b)
	return context.WithValue(ctx, credentialGuardKey{}, guards)
}

func CredentialGuard(ctx context.Context, id int64) *string {
	if guards, ok := ctx.Value(credentialGuardKey{}).(map[int64]string); ok {
		if value, ok := guards[id]; ok {
			return &value
		}
	}
	return nil
}

// CredentialGuards 同时保护影子与实际凭据账号，避免 owner 已换凭据后写入影子状态。
func CredentialGuards(ctx context.Context) *string {
	guards, ok := ctx.Value(credentialGuardKey{}).(map[int64]string)
	if !ok || len(guards) == 0 {
		return nil
	}
	values := map[int64]json.RawMessage{}
	for id, raw := range guards {
		values[id] = json.RawMessage(raw)
	}
	b, _ := json.Marshal(values)
	raw := string(b)
	return &raw
}
