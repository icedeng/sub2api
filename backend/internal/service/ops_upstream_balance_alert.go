package service

import (
	"context"
	"fmt"
	"time"
)

const OpsAlertMetricUpstreamLowBalanceCount = "upstream_low_balance_account_count"

type OpsUpstreamBalanceSample struct {
	LowAccountIDs []int64
	UnknownCount  int
}

type OpsUpstreamBalanceReader interface {
	ReadOpsUpstreamBalance(ctx context.Context, platform string, groupID *int64, now time.Time) (OpsUpstreamBalanceSample, error)
}

func (s *OpsAlertEvaluatorService) readUpstreamBalanceMetric(ctx context.Context, rule *OpsAlertRule, platform string, groupID *int64, now time.Time) (OpsUpstreamBalanceSample, bool) {
	if s.upstreamBalance == nil || rule.Operator != ">=" || rule.Threshold < 1 {
		return OpsUpstreamBalanceSample{}, false
	}
	sample, err := s.upstreamBalance.ReadOpsUpstreamBalance(ctx, platform, groupID, now)
	if err != nil {
		return OpsUpstreamBalanceSample{}, false
	}
	// 已确认不足的账号可触发告警。未知账号可能改变判定时，保留原告警状态。
	low := len(sample.LowAccountIDs)
	known := float64(low) >= rule.Threshold || float64(low+sample.UnknownCount) < rule.Threshold
	return sample, known
}

func describeOpsUpstreamBalance(sample OpsUpstreamBalanceSample) string {
	const maxIDs = 20
	count := len(sample.LowAccountIDs)
	ids := sample.LowAccountIDs[:min(count, maxIDs)]
	description := fmt.Sprintf("Upstream wallet balance is below the configured account threshold for %d account(s). Account IDs: %v.", count, ids)
	if count > len(ids) {
		description += fmt.Sprintf(" %d additional account(s).", count-len(ids))
	}
	return description
}
