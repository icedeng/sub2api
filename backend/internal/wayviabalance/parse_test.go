package wayviabalance

import (
	"errors"
	"testing"
)

func TestParseSub2APIBalance(t *testing.T) {
	tests := []struct {
		name, body, wallet, key, unit string
		periods                       int
		err                           error
	}{
		{"wallet precision", `{"mode":"unrestricted","isValid":true,"balance":9007199254740993.123456789,"remaining":1,"unit":"USD"}`, "9007199254740993.123456789", "", "USD", 0, nil},
		{"zero wallet", `{"mode":"unrestricted","balance":0,"unit":"USD"}`, "0", "", "USD", 0, nil},
		{"negative wallet", `{"mode":"unrestricted","balance":"-0.12","unit":"USD"}`, "-0.12", "", "USD", 0, nil},
		{"remaining not wallet", `{"mode":"unrestricted","remaining":12,"unit":"USD"}`, "", "", "", 0, nil},
		{"limited key", `{"mode":"quota_limited","remaining":30,"balance":999,"quota":{"remaining":20,"unit":"USD"}}`, "", "20", "USD", 0, nil},
		{"period only", `{"mode":"quota_limited","rate_limits":[{"window":"5h","limit":10,"used":1,"remaining":9,"reset_at":"2026-11-16T00:00:00Z"}]}`, "", "", "", 1, nil},
		{"subscription", `{"mode":"unrestricted","remaining":99,"subscription":{"daily_usage_usd":2,"daily_limit_usd":10,"expires_at":"2026-12-01T00:00:00Z"}}`, "", "", "", 1, nil},
		{"html", `<html>login</html>`, "", "", "", 0, ErrInvalidResponse},
		{"business error", `{"mode":"unrestricted","isValid":false}`, "", "", "", 0, ErrBusiness},
		{"unknown profile", `{"remaining":0}`, "", "", "", 0, ErrUnsupported},
		{"bad numeric field", `{"mode":"unrestricted","balance":"NaN","unit":"USD"}`, "", "", "", 0, ErrInvalidResponse},
		{"huge exponent", `{"mode":"unrestricted","balance":1e999999999,"unit":"USD"}`, "", "", "", 0, ErrInvalidResponse},
		{"trailing json", `{"mode":"unrestricted"}{}`, "", "", "", 0, ErrInvalidResponse},
		{"malformed period", `{"mode":"quota_limited","rate_limits":[{"window":"5h","remaining":{}}]}`, "", "", "", 0, ErrInvalidResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := parseSub2API([]byte(tt.body))
			if !errors.Is(err, tt.err) {
				t.Fatalf("error=%v want=%v", err, tt.err)
			}
			if err != nil {
				return
			}
			if tt.wallet == "" && s.Wallet != nil {
				t.Fatalf("unexpected wallet: %+v", s.Wallet)
			}
			if tt.wallet != "" && (s.Wallet == nil || s.Wallet.Amount != tt.wallet || s.Wallet.Unit != tt.unit) {
				t.Fatalf("wallet=%+v", s.Wallet)
			}
			if tt.key == "" && s.KeyQuota != nil {
				t.Fatalf("unexpected key: %+v", s.KeyQuota)
			}
			if tt.key != "" && (s.KeyQuota == nil || s.KeyQuota.Amount != tt.key || s.KeyQuota.Unit != tt.unit) {
				t.Fatalf("key=%+v", s.KeyQuota)
			}
			if len(s.Periods) != tt.periods {
				t.Fatalf("periods=%+v", s.Periods)
			}
		})
	}
}

func TestParseSub2APIUnitsAndToday(t *testing.T) {
	s, err := parseSub2API([]byte(`{"mode":"unrestricted","balance":12,"usage":{"today":{"requests":0,"total_tokens":9007199254740993,"cost":99,"actual_cost":1.25}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Wallet.Amount != "" || s.Wallet.RawAmount != "12" || s.Wallet.Unit != "" {
		t.Fatalf("unknown unit was invented: %+v", s.Wallet)
	}
	if s.Today == nil || s.Today.Scope != "key" || s.Today.Tokens != "9007199254740993" || s.Today.Requests != "0" || s.Today.Cost.RawAmount != "1.25" {
		t.Fatalf("today=%+v", s.Today)
	}
	s, err = parseSub2API([]byte(`{"mode":"unrestricted","subscription":{"daily_usage_usd":2,"daily_limit_usd":10,"weekly_usage_usd":20,"weekly_limit_usd":10,"monthly_limit_usd":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Periods[0].Remaining.Amount != "8" || s.Periods[1].Remaining.Amount != "0" || s.Periods[2].Remaining != nil {
		t.Fatalf("period arithmetic=%+v", s.Periods)
	}
	if s.Periods[0].ResetsAt != "" {
		t.Fatal("reset time must not be invented")
	}
}

func TestParseNewAPIBalance(t *testing.T) {
	meta := &Metadata{QuotaPerUnit: "500000", DisplayType: "USD"}
	tests := []struct {
		name, body, wallet, key string
		unlimited               bool
		err                     error
	}{
		{"separate wallet", `{"code":true,"data":{"object":"token_usage","unlimited_quota":false,"total_available":15000000,"user_quota":{"total_available":41365000}}}`, "82.73", "30", false, nil},
		{"unlimited is not wallet", `{"code":true,"data":{"object":"token_usage","unlimited_quota":true,"total_available":-1}}`, "", "", true, nil},
		{"zero wallet", `{"success":true,"data":{"object":"token_usage","unlimited_quota":false,"total_available":0,"user_quota":{"total_available":0}}}`, "0", "0", false, nil},
		{"business false", `{"code":false,"data":{"object":"token_usage"}}`, "", "", false, ErrBusiness},
		{"contradictory error", `{"code":true,"success":false,"data":{"object":"token_usage"}}`, "", "", false, ErrBusiness},
		{"missing key amount", `{"code":true,"data":{"object":"token_usage","unlimited_quota":false}}`, "", "", false, ErrInvalidResponse},
		{"wrong object", `{"success":true,"data":{"total_available":1}}`, "", "", false, ErrUnsupported},
		{"invalid wallet", `{"code":true,"data":{"object":"token_usage","unlimited_quota":true,"user_quota":{"total_available":"Infinity"}}}`, "", "", false, ErrInvalidResponse},
		{"missing success", `{"data":{"object":"token_usage","unlimited_quota":true}}`, "", "", false, ErrUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := parseNewAPI([]byte(tt.body), meta)
			if !errors.Is(err, tt.err) {
				t.Fatalf("error=%v want=%v", err, tt.err)
			}
			if err != nil {
				return
			}
			if tt.wallet == "" && s.Wallet != nil {
				t.Fatalf("unexpected wallet %+v", s.Wallet)
			}
			if tt.wallet != "" && (s.Wallet == nil || s.Wallet.Amount != tt.wallet) {
				t.Fatalf("wallet=%+v", s.Wallet)
			}
			if s.KeyQuota == nil || s.KeyQuota.Unlimited != tt.unlimited || s.KeyQuota.Amount != tt.key {
				t.Fatalf("key=%+v", s.KeyQuota)
			}
		})
	}
}

func TestMetadataConversionsAndUnknownUnits(t *testing.T) {
	tests := []struct {
		name, body, amount, unit string
		err                      error
	}{
		{"USD", `{"success":true,"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`, "2", "USD", nil},
		{"CNY", `{"success":true,"data":{"quota_per_unit":500000,"quota_display_type":"CNY","usd_exchange_rate":7.2}}`, "14.4", "CNY", nil},
		{"custom", `{"success":true,"data":{"quota_per_unit":500000,"quota_display_type":"CUSTOM","custom_currency_symbol":"EUR","custom_currency_exchange_rate":0.9}}`, "1.8", "EUR", nil},
		{"raw quota", `{"success":true,"data":{"quota_per_unit":500000,"quota_display_type":"TOKENS"}}`, "1000000", "quota", nil},
		{"old unknown currency", `{"success":true,"data":{"quota_per_unit":500000,"display_in_currency":true}}`, "1000000", "quota", nil},
		{"missing exchange", `{"success":true,"data":{"quota_per_unit":500000,"quota_display_type":"CNY"}}`, "1000000", "quota", nil},
		{"invalid quota", `{"success":true,"data":{"quota_per_unit":0}}`, "", "", ErrInvalidResponse},
		{"missing quota", `{"success":true,"data":{}}`, "", "", ErrInvalidResponse},
		{"invalid exchange", `{"success":true,"data":{"quota_per_unit":500000,"quota_display_type":"CNY","usd_exchange_rate":"NaN"}}`, "", "", ErrInvalidResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, err := parseMetadata([]byte(tt.body))
			if !errors.Is(err, tt.err) {
				t.Fatalf("error=%v want=%v", err, tt.err)
			}
			if err != nil {
				return
			}
			m, err := parseNewAPIUser([]byte(`{"success":true,"data":{"id":42,"quota":1000000}}`), meta)
			if err != nil || m.Amount != tt.amount || m.Unit != tt.unit || m.RawAmount != "1000000" {
				t.Fatalf("metric=%+v err=%v", m, err)
			}
		})
	}
	m, err := parseNewAPIUser([]byte(`{"success":true,"data":{"quota":9007199254740993}}`), nil)
	if err != nil || m.Amount != "9007199254740993" || m.Unit != "quota" {
		t.Fatalf("missing metadata=%+v err=%v", m, err)
	}
}

func TestExtractNewAPIUserID(t *testing.T) {
	id, err := extractNewAPIUserID([]byte(`{"success":true,"data":{"id":42,"quota":1}}`))
	if err != nil || id != "42" {
		t.Fatalf("id=%s err=%v", id, err)
	}
	for _, raw := range []string{"null", "0", "-1", "1.2", "true", "\"Infinity\""} {
		if _, err := extractNewAPIUserID([]byte(`{"success":true,"data":{"id":` + raw + `}}`)); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("raw=%s err=%v", raw, err)
		}
	}
}
