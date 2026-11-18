package service

import (
	"context"
	"testing"
	"time"
)

type fixedUpstreamBalanceSample OpsUpstreamBalanceSample

func (s fixedUpstreamBalanceSample) ReadOpsUpstreamBalance(context.Context, string, *int64, time.Time) (OpsUpstreamBalanceSample, error) {
	return OpsUpstreamBalanceSample(s), nil
}

func TestUpstreamBalanceAlertWithUnknownWallets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		low       []int64
		unknown   int
		minimum   float64
		evaluable bool
	}{
		{name: "confirmed low triggers even with unknown wallets", low: []int64{7}, unknown: 1, minimum: 1, evaluable: true},
		{name: "unknown wallet cannot resolve the alert", unknown: 1, minimum: 1, evaluable: false},
		{name: "fresh healthy wallets allow resolution", minimum: 1, evaluable: true},
		{name: "unknown wallets cannot reach the threshold", low: []int64{7}, unknown: 1, minimum: 3, evaluable: true},
		{name: "unknown wallets could reach the threshold", low: []int64{7}, unknown: 1, minimum: 2, evaluable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &OpsAlertEvaluatorService{upstreamBalance: fixedUpstreamBalanceSample{LowAccountIDs: tc.low, UnknownCount: tc.unknown}}
			rule := &OpsAlertRule{Operator: ">=", Threshold: tc.minimum}
			_, ok := svc.readUpstreamBalanceMetric(context.Background(), rule, "", nil, time.Now())
			if ok != tc.evaluable {
				t.Fatalf("evaluable = %t, want %t", ok, tc.evaluable)
			}
		})
	}
}
