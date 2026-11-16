package wayviabalance

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const redisPrefix = "wayvia:balance:v1:"

// owner 校验确保旧请求不会释放其他副本后续取得的租约。
var releaseScript = redis.NewScript(`if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0`)
var extendCooldownScript = redis.NewScript(`local old = tonumber(redis.call('GET',KEYS[1]) or '0'); local next = tonumber(ARGV[1]); if next > old then redis.call('SET',KEYS[1],ARGV[1],'PX',ARGV[2]); return next end return old`)

type queryError struct {
	code    string
	retryAt time.Time
}

func (e *queryError) Error() string { return e.code }

func (m *Manager) lock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	if m.redis == nil {
		return nil, false, ErrUnavailable
	}
	owner := uuid.NewString()
	key = redisPrefix + key
	ok, err := m.redis.SetNX(ctx, key, owner, ttl).Result()
	if err != nil || !ok {
		return nil, ok, err
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		// 租约自带过期；释放失败不把凭据或 Redis 连接信息回传前端。
		_ = releaseScript.Run(releaseCtx, m.redis, []string{key}, owner).Err()
	}, true, nil
}

func (m *Manager) cooldown(ctx context.Context, site string) (time.Time, error) {
	if m.redis == nil {
		return time.Time{}, ErrUnavailable
	}
	value, err := m.redis.Get(ctx, redisPrefix+"cooldown:"+site).Int64()
	if err == redis.Nil {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(value).UTC(), nil
}

func (m *Manager) cool(ctx context.Context, site string, until time.Time) error {
	ttl := time.Until(until)
	if ttl <= 0 {
		return nil
	}
	return extendCooldownScript.Run(ctx, m.redis, []string{redisPrefix + "cooldown:" + site}, until.UnixMilli(), ttl.Milliseconds()+1000).Err()
}

// acquireSite 把站点初始并发保守限制为 1；同站点所有账号和副本共用。
func (m *Manager) acquireSite(ctx context.Context, site string) (func(), error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		until, err := m.cooldown(ctx, site)
		if err != nil {
			return nil, &queryError{code: "coordination_unavailable"}
		}
		if until.After(time.Now()) {
			return nil, &queryError{code: "rate_limited", retryAt: until}
		}
		release, ok, err := m.lock(ctx, "site:"+site, 30*time.Second)
		if err != nil {
			return nil, &queryError{code: "coordination_unavailable"}
		}
		if ok {
			// 获取名额前等待期间，其他请求可能刚写入冷却。
			until, err = m.cooldown(ctx, site)
			if err != nil || until.After(time.Now()) {
				release()
				if err != nil {
					return nil, &queryError{code: "coordination_unavailable"}
				}
				return nil, &queryError{code: "rate_limited", retryAt: until}
			}
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, &queryError{code: "request_failed"}
		case <-ticker.C:
		}
	}
}

func retryAfter(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil && !strings.HasPrefix(value, "+") {
		// 不把长冷却截成一年；超出 duration 表达范围的头视为无效输入。
		if seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Time{}
		}
		return now.Add(time.Duration(seconds) * time.Second)
	}
	return parseRetryDate(value, now)
}

// metadataCache 仅保存已筛选字段；失败退避单独存储，不冒充有效元数据。
func (m *Manager) cachedMetadata(ctx context.Context, key string) (*Metadata, error) {
	value, err := m.redis.Get(ctx, redisPrefix+"metadata:"+key).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var metadata Metadata
	if len(value) > 4096 || json.Unmarshal(value, &metadata) != nil || !metadata.ExpiresAt.After(time.Now()) {
		return nil, nil
	}
	// 缓存也视为非可信输入；无效换算依据不能进入金额运算。
	quota, err := balanceDecimal(metadata.QuotaPerUnit)
	if err != nil || !quota.IsPositive() {
		return nil, nil
	}
	if metadata.ExchangeRate != "" {
		rate, err := balanceDecimal(metadata.ExchangeRate)
		if err != nil || !rate.IsPositive() {
			return nil, nil
		}
	}
	return &metadata, nil
}
