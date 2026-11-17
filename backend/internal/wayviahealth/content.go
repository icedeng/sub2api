// Package wayviahealth contains the independent, side-effect-free health policy rules.
package wayviahealth

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MaxTemplateBytes = 16 * 1024
const MaxContentBytes = 2 * 1024

var variable = regexp.MustCompile(`\{\{([^{}]*)\}\}`)
var randomRange = regexp.MustCompile(`^rand:([0-9]+)-([0-9]+)$`)

type Rendered struct {
	Content  string   `json:"content"`
	Line     int      `json:"line"`
	Timezone string   `json:"timezone"`
	Warnings []string `json:"warnings"`
}

// Candidates 保留重复行的权重，按字节限制上游输入，不静默截断。
func Candidates(template string) ([]string, error) {
	if len(template) > MaxTemplateBytes {
		return nil, fmt.Errorf("测活内容不能超过 16 KiB")
	}
	template = strings.ReplaceAll(strings.ReplaceAll(template, "\r\n", "\n"), "\r", "\n")
	lines := []string{}
	for _, line := range strings.Split(template, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > MaxContentBytes {
			return nil, fmt.Errorf("测活内容每行不能超过 2 KiB")
		}
		lines = append(lines, line)
	}
	if len(lines) > 100 {
		return nil, fmt.Errorf("测活内容不能超过 100 个有效候选行")
	}
	if len(lines) == 0 {
		lines = append(lines, "hi")
	}
	return lines, nil
}

func bounds(name string) (int64, int64, bool) {
	if name == "rand" {
		return 0, 9999, true
	}
	m := randomRange.FindStringSubmatch(name)
	if m == nil {
		return 0, 0, false
	}
	a, e1 := strconv.ParseInt(m[1], 10, 32)
	b, e2 := strconv.ParseInt(m[2], 10, 32)
	if e1 != nil || e2 != nil {
		return 0, 0, false
	}
	if a > b {
		a, b = b, a
	}
	return a, b, true
}

// Render 每次逻辑测试只调用一次；时钟和随机源可注入，重试复用结果。
func Render(template string, now time.Time, random func(int64) int64) (Rendered, error) {
	lines, err := Candidates(template)
	if err != nil {
		return Rendered{}, err
	}
	if random == nil {
		random = rand.Int64N
	}
	i := 0
	if len(lines) > 1 {
		i = int(random(int64(len(lines))))
	}
	r := Rendered{Line: i + 1, Timezone: now.Location().String(), Warnings: []string{}}
	// 预览应提示全部候选行的问题，不能因本次随机未选中而隐藏无效变量。
	for index, line := range lines {
		for _, raw := range variable.FindAllString(line, -1) {
			name := strings.ToLower(strings.TrimSpace(raw[2 : len(raw)-2]))
			if name == "time" || name == "date" || name == "datetime" || name == "timestamp" {
				continue
			}
			if _, _, ok := bounds(name); !ok {
				r.Warnings = append(r.Warnings, fmt.Sprintf("候选 %d：无法识别的变量已保留：%s", index+1, raw))
			}
		}
		remaining := variable.ReplaceAllString(line, "")
		if strings.Contains(remaining, "{{") || strings.Contains(remaining, "}}") {
			r.Warnings = append(r.Warnings, fmt.Sprintf("候选 %d：未闭合的变量已保留原文", index+1))
		}
	}
	r.Content = variable.ReplaceAllStringFunc(lines[i], func(raw string) string {
		name := strings.ToLower(strings.TrimSpace(raw[2 : len(raw)-2]))
		switch name {
		case "time":
			return now.Format("15:04:05")
		case "date":
			return now.Format("2006-01-02")
		case "datetime":
			return now.Format("2006-01-02 15:04:05")
		case "timestamp":
			return strconv.FormatInt(now.Unix(), 10)
		}
		if a, b, ok := bounds(name); ok {
			if a == b {
				return strconv.FormatInt(a, 10)
			}
			return strconv.FormatInt(a+random(b-a+1), 10)
		}
		return raw
	})
	if len(r.Content) > MaxContentBytes {
		return Rendered{}, fmt.Errorf("展开后的测活内容不能超过 2 KiB")
	}
	return r, nil
}

// Validate 校验所有候选的最坏展开长度，不能靠随机预览漏过某一行。
func Validate(template string) error {
	lines, err := Candidates(template)
	if err != nil {
		return err
	}
	for _, line := range lines {
		_, err = Render(line, time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC), func(n int64) int64 { return n - 1 })
		if err != nil {
			return err
		}
	}
	return nil
}
