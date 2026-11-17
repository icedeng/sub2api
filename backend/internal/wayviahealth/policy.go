package wayviahealth

import "fmt"

const SettingsKey = "wayvia_account_health_v1"
const PolicyKey = "wayvia_health_check"

type Override struct {
	Model       *string `json:"model,omitempty"`
	TestContent *string `json:"test_content"`
}

type PlatformPolicy struct {
	Platform    string `json:"platform"`
	AccountType string `json:"account_type"`
	Override
}

type Policy struct {
	Mode string `json:"mode"`
	Override
}

type Settings struct {
	Version           int64            `json:"version"`
	TestContent       string           `json:"test_content"`
	Concurrency       int              `json:"concurrency"`
	TimeoutSeconds    int              `json:"timeout_seconds"`
	Platforms         []PlatformPolicy `json:"platforms"`
	AccountTypes      []string         `json:"account_types"`
	GroupIDs          []int64          `json:"group_ids"`
	ErrorRecheck      bool             `json:"error_recheck"`
	PeriodicEnabled   bool             `json:"periodic_enabled"`
	IntervalMinutes   int              `json:"interval_minutes"`
	SkipRecentSuccess bool             `json:"skip_recent_success"`
	CooldownMinutes   int              `json:"cooldown_minutes"`
	AutoRecover       bool             `json:"auto_recover"`
	RetentionCount    int              `json:"retention_count"`
	RetentionDays     int              `json:"retention_days"`
}

func Defaults() Settings {
	return Settings{Concurrency: 1, TimeoutSeconds: 45, Platforms: []PlatformPolicy{}, AccountTypes: []string{"oauth"}, GroupIDs: []int64{}, IntervalMinutes: 60, SkipRecentSuccess: true, CooldownMinutes: 10, RetentionCount: 100, RetentionDays: 30}
}

func (s Settings) Validate() error {
	if s.Concurrency < 1 || s.Concurrency > 10 {
		return fmt.Errorf("并发需为 1–10")
	}
	if s.TimeoutSeconds < 10 || s.TimeoutSeconds > 120 {
		return fmt.Errorf("超时需为 10–120 秒")
	}
	if s.IntervalMinutes < 15 || s.IntervalMinutes > 10080 {
		return fmt.Errorf("周期需为 15–10080 分钟")
	}
	if s.CooldownMinutes < 10 || s.CooldownMinutes > 1440 {
		return fmt.Errorf("复检冷却需为 10–1440 分钟")
	}
	if s.RetentionCount < 1 || s.RetentionCount > 100 || s.RetentionDays < 1 || s.RetentionDays > 30 {
		return fmt.Errorf("历史上限为每账号 100 条、30 天")
	}
	if err := Validate(s.TestContent); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, p := range s.Platforms {
		if p.Platform != "openai" || (p.AccountType != "oauth" && p.AccountType != "apikey") {
			return fmt.Errorf("首版仅支持 OpenAI OAuth / API Key 文本检测")
		}
		if seen[p.AccountType] {
			return fmt.Errorf("平台与账号类型配置重复")
		}
		seen[p.AccountType] = true
		if err := p.Override.Validate(); err != nil {
			return err
		}
	}
	for _, t := range s.AccountTypes {
		if t != "oauth" && t != "apikey" {
			return fmt.Errorf("不支持的自动检测账号类型")
		}
	}
	if len(s.GroupIDs) > 1000 {
		return fmt.Errorf("分组范围过大")
	}
	for _, id := range s.GroupIDs {
		if id <= 0 {
			return fmt.Errorf("分组 ID 无效")
		}
	}
	return nil
}

func (o Override) Validate() error {
	if o.Model != nil && len(*o.Model) > 200 {
		return fmt.Errorf("模型名称过长")
	}
	if o.TestContent != nil {
		return Validate(*o.TestContent)
	}
	return nil
}

func (p Policy) Validate() error {
	if p.Mode != "inherit" && p.Mode != "exclude" && p.Mode != "override" {
		return fmt.Errorf("策略需为 inherit / exclude / override")
	}
	return p.Override.Validate()
}

// Resolve 保留 nil / 空字符串 / 自定义的三态语义。
func Resolve(s Settings, platform, accountType, defaultModel string, account Policy, plan, explicit *string) (model, template, source string) {
	model, template, source = defaultModel, s.TestContent, "global"
	for _, p := range s.Platforms {
		if p.Platform == platform && p.AccountType == accountType {
			if p.Model != nil && *p.Model != "" {
				model = *p.Model
			}
			if p.TestContent != nil {
				template, source = *p.TestContent, "platform"
			}
		}
	}
	if account.Mode == "override" {
		if account.Model != nil && *account.Model != "" {
			model = *account.Model
		}
		if account.TestContent != nil {
			template, source = *account.TestContent, "account"
		}
	}
	if plan != nil {
		template, source = *plan, "scheduled"
	}
	if explicit != nil {
		template, source = *explicit, "request"
	}
	return
}
