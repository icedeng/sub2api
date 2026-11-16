// Package wayviabalance 提供隔离的上游余额查询；不修改账号调度和计费。
package wayviabalance

import (
	"errors"
	"time"
)

var (
	ErrUnsupported     = errors.New("unsupported_response")
	ErrInvalidResponse = errors.New("invalid_response")
	ErrBusiness        = errors.New("upstream_business_error")
	ErrConfigChanged   = errors.New("config_changed")
	ErrUnavailable     = errors.New("balance_unavailable")
	ErrIneligible      = errors.New("account_ineligible")
	ErrInvalidConfig   = errors.New("invalid_balance_config")
)

type Config struct {
	Enabled             bool   `json:"enabled"`
	Provider            string `json:"provider"`
	AutoRefresh         bool   `json:"auto_refresh"`
	IntervalMinutes     int    `json:"interval_minutes"`
	LowBalanceThreshold string `json:"low_balance_threshold,omitempty"`
	QueryUserID         string `json:"query_user_id,omitempty"`
}

func defaultConfig() Config {
	return Config{Provider: "sub2api", AutoRefresh: true, IntervalMinutes: 5}
}

// Metric 使用十进制字符串传输，缺失、无限、真实零各有独立表达。
type Metric struct {
	Amount    string `json:"amount,omitempty"`
	Unit      string `json:"unit,omitempty"`
	RawAmount string `json:"raw_amount,omitempty"`
	Unlimited bool   `json:"unlimited,omitempty"`
}

type Period struct {
	Window    string  `json:"window"`
	Remaining *Metric `json:"remaining,omitempty"`
	Used      *Metric `json:"used,omitempty"`
	Limit     *Metric `json:"limit,omitempty"`
	ResetsAt  string  `json:"resets_at,omitempty"`
	ExpiresAt string  `json:"expires_at,omitempty"`
}

type Today struct {
	Requests string  `json:"requests,omitempty"`
	Tokens   string  `json:"tokens,omitempty"`
	Cost     *Metric `json:"cost,omitempty"`
	Scope    string  `json:"scope"`
}

type Metadata struct {
	QuotaPerUnit   string    `json:"quota_per_unit"`
	DisplayType    string    `json:"display_type"`
	CurrencySymbol string    `json:"currency_symbol,omitempty"`
	ExchangeRate   string    `json:"exchange_rate,omitempty"`
	FetchedAt      time.Time `json:"fetched_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type Snapshot struct {
	Provider                string    `json:"provider"`
	Path                    string    `json:"path"`
	Wallet                  *Metric   `json:"wallet,omitempty"`
	KeyQuota                *Metric   `json:"key_quota,omitempty"`
	Periods                 []Period  `json:"periods"`
	Today                   *Today    `json:"today,omitempty"`
	ReceivedAt              time.Time `json:"received_at"`
	FreshUntil              time.Time `json:"fresh_until"`
	WalletUnavailableReason string    `json:"wallet_unavailable_reason,omitempty"`
	Metadata                *Metadata `json:"metadata,omitempty"`
	WalletPath              string    `json:"wallet_path,omitempty"`
}

type State struct {
	AccountID            int64      `json:"account_id"`
	Eligible             bool       `json:"eligible"`
	Revision             int64      `json:"revision"`
	Config               Config     `json:"config"`
	QueryTokenConfigured bool       `json:"query_token_configured"`
	Status               string     `json:"status"`
	ErrorCode            string     `json:"error_code,omitempty"`
	LastAttemptAt        *time.Time `json:"last_attempt_at,omitempty"`
	NextRetryAt          *time.Time `json:"next_retry_at,omitempty"`
	Snapshot             *Snapshot  `json:"snapshot,omitempty"`
}

type UpdateRequest struct {
	ExpectedRevision int64   `json:"expected_revision"`
	Config           Config  `json:"config"`
	QueryToken       *string `json:"query_token,omitempty"`
	ClearQueryToken  bool    `json:"clear_query_token,omitempty"`
}

// record 仅用于模块持久化，不得直接返回管理接口或写入账号导出。
type record struct {
	Config            Config
	Revision          int64
	Identity          string
	QueryCiphertext   string
	QueryIdentity     string
	Snapshot          *Snapshot
	LastAttemptAt     *time.Time
	NextRetryAt       *time.Time
	ErrorCode         string
	FailureCount      int
	DetectedProvider  string
	DetectedPath      string
	PreviousAttemptAt *time.Time
}
