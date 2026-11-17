package wayviahealth

import (
	"strings"
	"testing"
	"time"
)

func TestRenderHealthContent(t *testing.T) {
	zone := time.FixedZone("Asia/Shanghai", 8*3600)
	now := time.Date(2026, 11, 16, 23, 59, 59, 0, zone)
	for _, tt := range []struct{ name, template, want string }{
		{"empty", "", "hi"}, {"blank", " \r\n\t\n", "hi"},
		{"snapshot", "{{date}} {{TIME}} {{ datetime }} {{timestamp}}", "2026-11-16 23:59:59 2026-11-16 23:59:59 1794844799"},
		{"random bounds", "{{rand}} {{rand:100-1}} {{rand:7-7}} {{rand:0-2147483647}}", "9999 100 7 2147483647"},
		{"unknown literal", "{{nope}} {{rand:-1-3}} {{rand:1-2147483648}} {{time", "{{nope}} {{rand:-1-3}} {{rand:1-2147483648}} {{time"},
		{"no expressions", "{{1+2}} $(whoami) {{env:HOME}}", "{{1+2}} $(whoami) {{env:HOME}}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Render(tt.template, now, func(n int64) int64 { return n - 1 })
			if err != nil || r.Content != tt.want {
				t.Fatalf("got %q / %v; want %q", r.Content, err, tt.want)
			}
		})
	}
}

func TestHealthCandidatesAndIndependentRandomOccurrences(t *testing.T) {
	lines, err := Candidates(" a\r\n\r\na\n b ")
	if err != nil || len(lines) != 3 || lines[0] != "a" || lines[1] != "a" || lines[2] != "b" {
		t.Fatalf("candidates %#v / %v", lines, err)
	}
	calls := 0
	r, err := Render("ignored\n{{rand:1-3}} {{rand:1-3}}", time.Now(), func(n int64) int64 {
		calls++
		if calls == 1 {
			return 1
		}
		return int64(calls - 2)
	})
	if err != nil || r.Content != "1 2" || calls != 3 || r.Line != 2 {
		t.Fatalf("render %+v / %v calls %d", r, err, calls)
	}
}

func TestHealthContentLimits(t *testing.T) {
	for _, template := range []string{strings.Repeat("a", MaxTemplateBytes+1), strings.Repeat("a\n", 101), strings.Repeat("a", MaxContentBytes+1), strings.Repeat("a", MaxContentBytes-10) + "{{datetime}}"} {
		if err := Validate(template); err == nil {
			t.Fatalf("accepted invalid %d byte template", len(template))
		}
	}
	if err := Validate(strings.Repeat("a", MaxContentBytes)); err != nil {
		t.Fatal(err)
	}
}

func TestHealthPolicyTristateAndDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.ErrorRecheck || cfg.PeriodicEnabled || cfg.AutoRecover {
		t.Fatal("new probes must default off")
	}
	cfg.TestContent = "global"
	platform, account, plan, request := "platform", "account", "plan", "request"
	cfg.Platforms = []PlatformPolicy{{Platform: "openai", AccountType: "oauth", Override: Override{TestContent: &platform}}}
	p := Policy{Mode: "override", Override: Override{TestContent: &account}}
	for _, tt := range []struct {
		plan, explicit *string
		want, source   string
	}{{nil, nil, "account", "account"}, {&plan, nil, "plan", "scheduled"}, {&plan, &request, "request", "request"}} {
		_, got, source := Resolve(cfg, "openai", "oauth", "default", p, tt.plan, tt.explicit)
		if got != tt.want || source != tt.source {
			t.Fatalf("%q %q", got, source)
		}
	}
	empty := ""
	_, got, source := Resolve(cfg, "openai", "oauth", "default", p, &plan, &empty)
	if got != "" || source != "request" {
		t.Fatal("explicit empty must override all lower layers")
	}
	r, err := Render(got, time.Now(), nil)
	if err != nil || r.Content != "hi" {
		t.Fatal("explicit empty must render hi")
	}
}

func TestHealthWarningsIncludeUnselectedCandidates(t *testing.T) {
	r, err := Render("hello\n{{unknown}}\n{{time", time.Now(), func(int64) int64 { return 0 })
	if err != nil || r.Content != "hello" || len(r.Warnings) != 2 {
		t.Fatal(r, err)
	}
}
