package types

import (
	"regexp"
	"testing"
	"time"
)

// nowUTCRe ISO8601 UTC 秒级格式（技术设计 §3.1 第 1 条）：2006-01-02T15:04:05Z。
var nowUTCRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

// TestNowUTCDefaultFormat 默认实现输出 ISO8601 UTC 秒级文本，且与当前 UTC 差值在容忍窗口内。
func TestNowUTCDefaultFormat(t *testing.T) {
	got := NowUTC()
	if !nowUTCRe.MatchString(got) {
		t.Fatalf("NowUTC() = %q，不符合 2006-01-02T15:04:05Z 格式", got)
	}
	parsed, err := time.Parse("2006-01-02T15:04:05Z", got)
	if err != nil {
		t.Fatalf("按既定格式解析自身输出失败: %v", err)
	}
	if d := time.Since(parsed).Abs(); d > 5*time.Second {
		t.Errorf("NowUTC() 与当前时间偏差 %v，超过 5s 容忍窗口", d)
	}
}

// TestNowUTCInjectable 包级变量可替换——后续批测试经此注入固定时间。
func TestNowUTCInjectable(t *testing.T) {
	orig := NowUTC
	defer func() { NowUTC = orig }()

	NowUTC = func() string { return "2026-01-01T00:00:00Z" }
	if got := NowUTC(); got != "2026-01-01T00:00:00Z" {
		t.Errorf("注入后 NowUTC() = %q，期望注入值", got)
	}
}
