package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestDefaults 验证零配置下全部字段的内建默认值（技术设计 §9.3 样例逐字段）。
func TestDefaults(t *testing.T) {
	cfg := Default()
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"listen", cfg.Listen, "0.0.0.0:8310"},
		{"db.type", cfg.DB.Type, "sqlite"},
		{"db.path", cfg.DB.Path, "db/aiteam.db"},
		{"auth.token_enabled", cfg.Auth.TokenEnabled, false},
		{"auth.token", cfg.Auth.Token, ""},
		{"session.heartbeat_timeout_sec", cfg.Session.HeartbeatTimeoutSec, 900},
		{"watch.poll_interval_sec", cfg.Watch.PollIntervalSec, 5},
		{"watch.sentinel_timeout_sec", cfg.Watch.SentinelTimeoutSec, 15},
		{"board.poll_interval_sec", cfg.Board.PollIntervalSec, 5},
		{"timezone", cfg.Timezone, "Local"},
	}
	for _, tc := range cases {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s = %v，期望 %v", tc.name, tc.got, tc.want)
		}
	}
}

// TestLoadPartial 验证部分覆盖语义：JSON 只写 listen 与 db.path 两个字段，
// 其余字段（含整个缺失的子结构与嵌套子结构里未写的字段）保持默认；
// _doc 等未知字段被 encoding/json 默认忽略，不报错。
func TestLoadPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
  "_doc": "aiteam 服务配置样例；全部字段可省略走默认值",
  "listen": "127.0.0.1:9000",
  "db": {"path": "/data/aiteam.db"}
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时配置文件失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) 返回错误: %v", path, err)
	}
	want := Default()
	want.Listen = "127.0.0.1:9000"
	want.DB.Path = "/data/aiteam.db"
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("部分覆盖结果不符:\n got = %+v\nwant = %+v", cfg, want)
	}
}

// TestLoadInvalidDBType 验证 db.type 白名单：postgres 能正常加载，
// 但 Validate 报错且错误信息含「db.type 一期仅 sqlite」（AC17.1 配套约束）。
func TestLoadInvalidDBType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{"db": {"type": "postgres"}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时配置文件失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) 返回错误: %v", path, err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("db.type=postgres 时 Validate 应返回错误，实际为 nil")
	}
	if !strings.Contains(err.Error(), "db.type 一期仅 sqlite") {
		t.Errorf("错误信息 %q 应包含 %q", err.Error(), "db.type 一期仅 sqlite")
	}
}

// TestLoadBOMStripped 固化 BOM 剥离：带 UTF-8 BOM 的配置文件（中文 Windows
// 记事本高发）加载成功且字段生效，不拒启。
func TestLoadBOMStripped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := "\xef\xbb\xbf" + `{"listen": "127.0.0.1:9001"}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时配置文件失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) 返回错误: %v", path, err)
	}
	want := Default()
	want.Listen = "127.0.0.1:9001"
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("BOM 配置加载结果不符:\n got = %+v\nwant = %+v", cfg, want)
	}
}

// TestLoadInvalidJSON 验证非法 JSON 拒启：返回错误且信息含「解析配置文件」，
// 同时不得返回半初始化配置。
func TestLoadInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{bad"), 0o644); err != nil {
		t.Fatalf("写临时配置文件失败: %v", err)
	}
	cfg, err := Load(path)
	if err == nil {
		t.Fatal("非法 JSON 时 Load 应返回错误，实际为 nil")
	}
	if !strings.Contains(err.Error(), "解析配置文件") {
		t.Errorf("错误信息 %q 应包含 %q", err.Error(), "解析配置文件")
	}
	if cfg != nil {
		t.Errorf("非法 JSON 时 cfg 应为 nil，实际 %+v", cfg)
	}
}

// TestStaleDefault 验证 progress.stale_after 默认值：空路径与配置文件缺
// progress 段两种零配置形态下，均保持内建默认 60m（b8-spec §1.1 冻结口径）。
func TestStaleDefault(t *testing.T) {
	// 空路径：纯内建默认
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") 返回错误: %v", err)
	}
	if cfg.Progress.StaleAfter != "60m" {
		t.Errorf("空路径 progress.stale_after = %q，期望 %q", cfg.Progress.StaleAfter, "60m")
	}
	// 配置文件存在但缺 progress 段：该子结构整体保持默认
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{"listen": "127.0.0.1:9002"}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时配置文件失败: %v", err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load(%q) 返回错误: %v", path, err)
	}
	if cfg.Progress.StaleAfter != "60m" {
		t.Errorf("缺 progress 段时 stale_after = %q，期望 %q", cfg.Progress.StaleAfter, "60m")
	}
}

// TestStaleCustom 验证 progress.stale_after 可覆盖：JSON 写 "30m" 后加载生效，
// 其余 progress 字段（auto_hook）不受影响保持默认。
func TestStaleCustom(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{"progress": {"stale_after": "30m"}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时配置文件失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) 返回错误: %v", path, err)
	}
	if cfg.Progress.StaleAfter != "30m" {
		t.Errorf("progress.stale_after = %q，期望 %q", cfg.Progress.StaleAfter, "30m")
	}
	if !cfg.Progress.AutoHook {
		t.Error("只覆盖 stale_after 时 auto_hook 应保持默认 true")
	}
}

// TestAutoHookDefault 验证 progress.auto_hook 开关：缺省保持默认 on（true），
// 显式 false 覆盖生效（b8-spec §1.1 / AC23.1 auto_hook=off 时 init 不装 hook）。
func TestAutoHookDefault(t *testing.T) {
	// 缺省（零配置）为 on
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") 返回错误: %v", err)
	}
	if !cfg.Progress.AutoHook {
		t.Error("缺省 progress.auto_hook 应为 true（默认 on）")
	}
	// 显式 false 生效
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{"progress": {"auto_hook": false}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时配置文件失败: %v", err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load(%q) 返回错误: %v", path, err)
	}
	if cfg.Progress.AutoHook {
		t.Error("显式 auto_hook=false 后应为 false")
	}
}

// TestLoadFileMissing 验证路径不存在 / 空路径时返回默认配置且无错误（AC17.2）。
func TestLoadFileMissing(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"路径不存在", filepath.Join(t.TempDir(), "no-such-config.json")},
		{"空路径", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(tc.path)
			if err != nil {
				t.Fatalf("Load(%q) 返回错误: %v", tc.path, err)
			}
			if want := Default(); !reflect.DeepEqual(cfg, want) {
				t.Errorf("Load(%q) 应返回默认配置:\n got = %+v\nwant = %+v", tc.path, cfg, want)
			}
		})
	}
}
