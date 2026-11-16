package wayviabalance

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/shopspring/decimal"
)

var balanceNumberPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

func decodeBalanceObject(body []byte) (map[string]any, error) {
	if len(body) == 0 || len(body) > 256*1024 {
		return nil, ErrInvalidResponse
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var value map[string]any
	if err := dec.Decode(&value); err != nil || value == nil {
		return nil, ErrInvalidResponse
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, ErrInvalidResponse
	}
	for _, key := range []string{"success", "isValid"} {
		if v, exists := value[key]; exists {
			b, ok := v.(bool)
			if !ok {
				return nil, ErrInvalidResponse
			}
			if !b {
				return nil, ErrBusiness
			}
		}
	}
	if v, exists := value["error"]; exists && v != nil && v != "" {
		return nil, ErrBusiness
	}
	return value, nil
}

// balanceDecimal 在进入 decimal 前限制长度和指数，避免恶意数字扩大内存。
func balanceDecimal(value any) (decimal.Decimal, error) {
	var raw string
	switch v := value.(type) {
	case json.Number:
		raw = v.String()
	case string:
		raw = v
	default:
		return decimal.Zero, ErrInvalidResponse
	}
	if len(raw) == 0 || len(raw) > 96 || !balanceNumberPattern.MatchString(raw) {
		return decimal.Zero, ErrInvalidResponse
	}
	if i := strings.IndexAny(raw, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(raw[i+1:])
		if err != nil || exponent < -24 || exponent > 24 {
			return decimal.Zero, ErrInvalidResponse
		}
	}
	d, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.Zero, ErrInvalidResponse
	}
	return d, nil
}

func balanceUnit(value any) string {
	s, ok := value.(string)
	if !ok || len(s) > 24 || strings.TrimSpace(s) != s || strings.ContainsAny(s, "<>{}\\\"'") || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return ""
	}
	return s
}

func directMetric(value any, unit string) (*Metric, error) {
	if value == nil {
		return nil, nil
	}
	d, err := balanceDecimal(value)
	if err != nil {
		return nil, err
	}
	m := &Metric{RawAmount: d.String()}
	if unit != "" {
		m.Amount, m.Unit = d.String(), unit
	}
	return m, nil
}

func balanceObject(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	return m, ok && m != nil
}

func balanceTime(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC().Format(time.RFC3339Nano)
		}
	}
	d, err := balanceDecimal(value)
	if err != nil || !d.IsInteger() || !d.IsPositive() || d.GreaterThan(decimal.NewFromInt(253402300799)) {
		return ""
	}
	return time.Unix(d.IntPart(), 0).UTC().Format(time.RFC3339)
}

func parseSub2API(body []byte) (*Snapshot, error) {
	doc, err := decodeBalanceObject(body)
	if err != nil {
		return nil, err
	}
	mode, _ := doc["mode"].(string)
	if mode != "unrestricted" && mode != "quota_limited" {
		return nil, ErrUnsupported
	}
	s := &Snapshot{Provider: "sub2api", Periods: []Period{}, WalletUnavailableReason: "not_provided"}
	unit := balanceUnit(doc["unit"])
	if mode == "quota_limited" {
		if value := doc["quota"]; value != nil {
			quota, ok := balanceObject(value)
			if !ok {
				return nil, ErrInvalidResponse
			}
			quotaUnit := balanceUnit(quota["unit"])
			if quotaUnit == "" {
				quotaUnit = unit
			}
			s.KeyQuota, err = directMetric(quota["remaining"], quotaUnit)
			if err != nil {
				return nil, err
			}
		}
		if s.KeyQuota == nil {
			s.KeyQuota, err = directMetric(doc["remaining"], unit)
			if err != nil {
				return nil, err
			}
		}
		if value := doc["rate_limits"]; value != nil {
			limits, ok := value.([]any)
			if !ok || len(limits) > 16 {
				return nil, ErrInvalidResponse
			}
			for _, raw := range limits {
				limit, ok := balanceObject(raw)
				if !ok {
					return nil, ErrInvalidResponse
				}
				window, _ := limit["window"].(string)
				if window != "5h" && window != "1d" && window != "7d" {
					return nil, ErrInvalidResponse
				}
				periodUnit := balanceUnit(limit["unit"])
				if periodUnit == "" {
					periodUnit = unit
				}
				p, err := parseBalancePeriod(window, limit["remaining"], limit["used"], limit["limit"], periodUnit)
				if err != nil {
					return nil, err
				}
				p.ResetsAt, p.ExpiresAt = balanceTime(limit["reset_at"]), balanceTime(doc["expires_at"])
				s.Periods = append(s.Periods, p)
			}
		}
	} else {
		// unrestricted 同时用于订阅和钱包，remaining 不能作为钱包判据。
		if raw := doc["subscription"]; raw != nil {
			subscription, ok := balanceObject(raw)
			if !ok {
				return nil, ErrInvalidResponse
			}
			for _, scope := range []struct{ prefix, window string }{{"daily", "1d"}, {"weekly", "7d"}, {"monthly", "1mo"}} {
				used, limit := subscription[scope.prefix+"_usage_usd"], subscription[scope.prefix+"_limit_usd"]
				if used == nil && limit == nil {
					continue
				}
				// _usd 是上游契约明确给出的单位，不依赖本站汇率。
				p, err := parseBalancePeriod(scope.window, nil, used, limit, "USD")
				if err != nil {
					return nil, err
				}
				if used != nil && limit != nil {
					u, _ := balanceDecimal(used)
					l, _ := balanceDecimal(limit)
					if l.IsPositive() {
						p.Remaining = &Metric{Amount: decimal.Max(decimal.Zero, l.Sub(u)).String(), Unit: "USD"}
					}
				}
				p.ExpiresAt = balanceTime(subscription["expires_at"])
				// 没有明确重置时间就保留为空，不能按本地时区推算周期边界。
				p.ResetsAt = balanceTime(subscription[scope.prefix+"_reset_at"])
				s.Periods = append(s.Periods, p)
			}
			if len(s.Periods) == 0 && doc["remaining"] != nil {
				remaining, err := directMetric(doc["remaining"], unit)
				if err != nil {
					return nil, err
				}
				s.Periods = append(s.Periods, Period{Window: "subscription", Remaining: remaining, ExpiresAt: balanceTime(subscription["expires_at"])})
			}
		}
		s.Wallet, err = directMetric(doc["balance"], unit)
		if err != nil {
			return nil, err
		}
		if s.Wallet != nil {
			s.WalletUnavailableReason = ""
		}
	}
	s.Today, err = parseSub2APIToday(doc, unit)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func parseBalancePeriod(window string, remaining, used, limit any, unit string) (Period, error) {
	p := Period{Window: window}
	var err error
	if p.Remaining, err = directMetric(remaining, unit); err != nil {
		return p, err
	}
	if p.Used, err = directMetric(used, unit); err != nil {
		return p, err
	}
	if p.Limit, err = directMetric(limit, unit); err != nil {
		return p, err
	}
	return p, nil
}

func parseSub2APIToday(doc map[string]any, unit string) (*Today, error) {
	usage, ok := balanceObject(doc["usage"])
	if !ok {
		return nil, nil
	}
	raw, ok := balanceObject(usage["today"])
	if !ok {
		return nil, nil
	}
	today := &Today{Scope: "key"}
	for key, destination := range map[string]*string{"requests": &today.Requests, "total_tokens": &today.Tokens} {
		if raw[key] == nil {
			continue
		}
		d, err := balanceDecimal(raw[key])
		if err != nil || d.IsNegative() || !d.IsInteger() {
			return nil, ErrInvalidResponse
		}
		*destination = d.String()
	}
	// cost 是未乘用户倍率的价格；只展示明确的实际消费 actual_cost。
	var err error
	today.Cost, err = directMetric(raw["actual_cost"], unit)
	if err != nil {
		return nil, err
	}
	if today.Requests == "" && today.Tokens == "" && today.Cost == nil {
		return nil, nil
	}
	return today, nil
}

func newAPIData(body []byte) (map[string]any, error) {
	doc, err := decodeBalanceObject(body)
	if err != nil {
		return nil, err
	}
	if flag, exists := doc["code"]; exists {
		b, ok := flag.(bool)
		if !ok {
			return nil, ErrInvalidResponse
		}
		if !b {
			return nil, ErrBusiness
		}
	}
	if doc["code"] != true && doc["success"] != true {
		return nil, ErrUnsupported
	}
	data, ok := balanceObject(doc["data"])
	if !ok {
		return nil, ErrInvalidResponse
	}
	return data, nil
}

func parseNewAPI(body []byte, metadata *Metadata) (*Snapshot, error) {
	data, err := newAPIData(body)
	if err != nil {
		return nil, err
	}
	if data["object"] != "token_usage" {
		return nil, ErrUnsupported
	}
	s := &Snapshot{Provider: "newapi", Periods: []Period{}, Metadata: metadata, WalletUnavailableReason: "not_provided"}
	unlimited, ok := data["unlimited_quota"].(bool)
	if !ok {
		return nil, ErrInvalidResponse
	}
	if unlimited {
		s.KeyQuota = &Metric{Unlimited: true}
	} else {
		if data["total_available"] == nil {
			return nil, ErrInvalidResponse
		}
		s.KeyQuota, err = newAPIQuotaMetric(data["total_available"], metadata)
		if err != nil {
			return nil, err
		}
	}
	if raw := data["user_quota"]; raw != nil {
		userQuota, ok := balanceObject(raw)
		if !ok {
			return nil, ErrInvalidResponse
		}
		s.Wallet, err = newAPIQuotaMetric(userQuota["total_available"], metadata)
		if err != nil {
			return nil, err
		}
		if s.Wallet != nil {
			s.WalletUnavailableReason = ""
		}
	}
	return s, nil
}

func parseMetadata(body []byte) (*Metadata, error) {
	data, err := newAPIData(body)
	if err != nil {
		return nil, err
	}
	quota, err := balanceDecimal(data["quota_per_unit"])
	if err != nil || !quota.IsPositive() {
		return nil, ErrInvalidResponse
	}
	display, _ := data["quota_display_type"].(string)
	m := &Metadata{QuotaPerUnit: quota.String(), DisplayType: strings.ToUpper(display)}
	switch m.DisplayType {
	case "USD":
		m.CurrencySymbol = "USD"
	case "CNY":
		m.CurrencySymbol = "CNY"
		if data["usd_exchange_rate"] != nil {
			rate, err := balanceDecimal(data["usd_exchange_rate"])
			if err != nil || !rate.IsPositive() {
				return nil, ErrInvalidResponse
			}
			m.ExchangeRate = rate.String()
		}
	case "CUSTOM":
		m.CurrencySymbol = balanceUnit(data["custom_currency_symbol"])
		if data["custom_currency_exchange_rate"] != nil {
			rate, err := balanceDecimal(data["custom_currency_exchange_rate"])
			if err != nil || !rate.IsPositive() {
				return nil, ErrInvalidResponse
			}
			m.ExchangeRate = rate.String()
		}
	default:
		// display_in_currency 不能证明币种，旧版或未知显示规则只保留原始额度。
		m.DisplayType = "QUOTA"
	}
	return m, nil
}

func newAPIQuotaMetric(value any, metadata *Metadata) (*Metric, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := balanceDecimal(value)
	if err != nil {
		return nil, err
	}
	m := &Metric{RawAmount: raw.String(), Amount: raw.String(), Unit: "quota"}
	if metadata == nil {
		return m, nil
	}
	quota, err := balanceDecimal(metadata.QuotaPerUnit)
	if err != nil || !quota.IsPositive() {
		return nil, ErrInvalidResponse
	}
	var factor = decimal.NewFromInt(1)
	switch metadata.DisplayType {
	case "USD":
		m.Unit = "USD"
	case "CNY", "CUSTOM":
		unit := balanceUnit(metadata.CurrencySymbol)
		if unit == "" || metadata.ExchangeRate == "" {
			return m, nil
		}
		factor, err = balanceDecimal(metadata.ExchangeRate)
		if err != nil || !factor.IsPositive() {
			return nil, ErrInvalidResponse
		}
		m.Unit = unit
	default:
		return m, nil
	}
	// 固定精度避免循环小数无限展开；原始额度和换算依据仍完整保留。
	m.Amount = raw.Mul(factor).DivRound(quota, 18).String()
	return m, nil
}

func parseNewAPIUser(body []byte, metadata *Metadata) (*Metric, error) {
	data, err := newAPIData(body)
	if err != nil {
		return nil, err
	}
	if data["quota"] == nil {
		return nil, ErrInvalidResponse
	}
	return newAPIQuotaMetric(data["quota"], metadata)
}

func extractNewAPIUserID(body []byte) (string, error) {
	data, err := newAPIData(body)
	if err != nil {
		return "", err
	}
	id, err := balanceDecimal(data["id"])
	if err != nil || !id.IsInteger() || !id.IsPositive() || len(id.String()) > 20 {
		return "", ErrInvalidResponse
	}
	return id.String(), nil
}
