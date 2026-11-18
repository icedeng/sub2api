package wayviabalance

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
)

type alertWallet struct {
	accountID     int64
	identity      string
	config        Config
	snapshot      *Snapshot
	lastAttemptAt *time.Time
	errorCode     string
}

func (m *Manager) ReadOpsUpstreamBalance(ctx context.Context, platform string, groupID *int64, now time.Time) (service.OpsUpstreamBalanceSample, error) {
	var sample service.OpsUpstreamBalanceSample
	if m == nil || !m.ready {
		return sample, ErrUnavailable
	}
	wallets, err := m.store.AlertWallets(ctx, platform, groupID)
	if err != nil {
		return sample, err
	}
	ids := make([]int64, 0, len(wallets))
	for _, wallet := range wallets {
		ids = append(ids, wallet.accountID)
	}
	accounts, err := m.accounts.GetByIDs(ctx, ids)
	if err != nil {
		return sample, err
	}
	accountsByID := make(map[int64]*service.Account, len(accounts))
	for _, account := range accounts {
		accountsByID[account.ID] = account
	}
	for _, wallet := range wallets {
		account := accountsByID[wallet.accountID]
		if !eligible(account) {
			continue
		}
		low, known := wallet.belowThreshold(account, now)
		switch {
		case !known:
			sample.UnknownCount++
		case low:
			sample.LowAccountIDs = append(sample.LowAccountIDs, wallet.accountID)
		}
	}
	return sample, nil
}

func (w alertWallet) belowThreshold(account *service.Account, now time.Time) (low, known bool) {
	if w.errorCode != "" || w.snapshot == nil || w.lastAttemptAt == nil ||
		w.snapshot.ReceivedAt.Before(*w.lastAttemptAt) || w.snapshot.ReceivedAt.After(now) ||
		!w.snapshot.FreshUntil.After(now) || w.identity == "" || w.identity != accountIdentity(account) {
		return false, false
	}
	wallet := w.snapshot.Wallet
	if wallet == nil || wallet.Unlimited || wallet.Unit == "" || wallet.Unit == "quota" ||
		w.snapshot.WalletUnavailableReason != "" {
		return false, false
	}
	amount, err := decimal.NewFromString(wallet.Amount)
	if err != nil {
		return false, false
	}
	threshold, err := decimal.NewFromString(w.config.LowBalanceThreshold)
	if err != nil || threshold.IsNegative() {
		return false, false
	}
	return amount.LessThan(threshold), true
}

func (s *store) AlertWallets(ctx context.Context, platform string, groupID *int64) ([]alertWallet, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.account_id, b.identity, b.config, b.snapshot, b.last_attempt_at, b.error_code
		FROM wayvia_upstream_balances b
		JOIN accounts a ON a.id = b.account_id
		WHERE a.deleted_at IS NULL AND a.type = 'apikey'
		  AND b.config->'enabled' = 'true'::jsonb
		  AND b.config->>'low_balance_threshold' <> ''
		  AND ($1 = '' OR a.platform = $1)
		  AND ($2::bigint IS NULL OR EXISTS (
		      SELECT 1 FROM account_groups ag WHERE ag.account_id = a.id AND ag.group_id = $2
		  ))
		ORDER BY b.account_id`, platform, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var wallets []alertWallet
	for rows.Next() {
		var w alertWallet
		var configJSON, snapshotJSON []byte
		if err := rows.Scan(&w.accountID, &w.identity, &configJSON, &snapshotJSON, &w.lastAttemptAt, &w.errorCode); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(configJSON, &w.config); err != nil {
			return nil, err
		}
		if len(snapshotJSON) > 0 {
			if err := json.Unmarshal(snapshotJSON, &w.snapshot); err != nil {
				return nil, err
			}
		}
		wallets = append(wallets, w)
	}
	return wallets, rows.Err()
}
