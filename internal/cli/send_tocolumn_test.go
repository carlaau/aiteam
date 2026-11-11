package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"aiteam/internal/types"
)

// send --to-column flag 面红笔锚（v0.4.2-b2 T6，M-43①，seq=869 建议 1）：服务端
// direct 形态早已支持 target.column（handler_messages_send.go direct 分支），
// CLI flag 面未暴露——send.go requestBody 硬取 ident.Column（同栏目定向）。
// 本文件只写测试断言（红笔纪律：修复前红=断言红，零实现改动；flag 名
// --to-column 与组合冲突文案以字符串字面量锚定，不引用尚不存在的新常量/新
// 类型/新函数）。绿笔落点预告（文案定稿，供对照）：组合冲突文案=
// 「--to-column 仅与 --to-role 组合（direct 定向目标栏目；与 --to-session/--bus
// 同给属用法错误）」——本文件锚稳定片段「仅与 --to-role 组合」，全句逐字锚
// 留给绿笔补充测试。
//
// 五锚分布（修复前红）：
//  1. TestSendToColumnRequestBodyWins——请求体 target 三键（kind/column/role），
//     column==--to-column 值且≠发送方 --column；
//  2. TestSendToColumnConflictWithSession——--to-column+--to-session 退 2 桩零
//     命中+文案含组合语义片段；
//  3. TestSendToColumnConflictWithBus——--to-column+--bus 同上（文案风格对齐
//     既有 --to-project+--bus）；
//  4. TestSendToColumnMirrorAndResolved——镜像行目标段=<project>/C/R、--json
//     resolved_target.column==C；
//  5. TestSendToColumnCrossProjectCombo——--to-project+--to-role+--to-column
//     组合合法（target 含 project/column/role 三键，防过收敛误拒）。
//
// 现状锚（修复前后都绿，防回归，独立成函数便于 -run 单独核验）：
// TestSendToColumnRequestBodyFallback（不传 --to-column → target.column==发送方
// --column）与 TestSendToColumnMirrorAndResolvedFallback（镜像段与
// resolved_target.column 维持发送方 --column）。
//
// 文件裁量留痕：独立新文件不并入 send_test.go——该文件已 1300+ 行且按批次增量
// 分节，本锚集独立成文件便于绿笔全量对照与单文件回滚（git checkout 即撤红）；
// 同包复用既有 helper（sendMirrorPath/readMirrorLines/parseOKLine/
// runWantCode/regIdentityArgs/assertResolvedTarget），零复制。

// sendToColumnStubServer send #9 httptest 桩（请求体捕获+命中计数）：POST
// /api/v1/messages 捕获最近一次请求体并回 201（data=sendMessageData 同构四键，
// seq/created_at 为镜像权威字段值）；其余方法/路径=分叉语义破坏即报错（
// watchRegStubServer 同款骨架）。返回服务地址供 runSend serverOverride 注入
// （startRegTestServer 同款返回面）。hits/lastBody 由调用方传指针回收；只当
// 纯 201 响应面用的用例（锚 4）传占位指针即可。
func sendToColumnStubServer(t *testing.T, hits *int, lastBody *string) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/messages" {
			*hits++
			b, _ := io.ReadAll(r.Body)
			*lastBody = string(b)
			types.WriteData(w, http.StatusCreated, map[string]any{
				"seq":        int64(1),
				"created_at": "2026-01-01T00:00:00Z",
				"level":      "normal",
				"kind":       "direct",
			})
			return
		}
		t.Errorf("send 桩误收请求: %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// sendStubPayload #9 请求体形状（§2.2 #9：target/level/body 三键）——红笔自备
// 解码结构（不依赖实现面类型，target 以 map 裸断言键值）。
type sendStubPayload struct {
	Target map[string]any `json:"target"`
	Level  string         `json:"level"`
	Body   string         `json:"body"`
}

// parseSendStubBody 解码桩捕获的请求体；零命中时 Fatal（无法断言即测试必红）。
func parseSendStubBody(t *testing.T, hits int, lastBody string) sendStubPayload {
	t.Helper()
	if hits != 1 {
		t.Fatalf("POST /api/v1/messages 命中 = %d，期望恰 1 次（零命中即无请求体可断言）", hits)
	}
	var p sendStubPayload
	if err := json.Unmarshal([]byte(lastBody), &p); err != nil {
		t.Fatalf("桩捕获请求体非合法 JSON: %v\n%s", err, lastBody)
	}
	return p
}

// ─────────────────────────────────────────────────────────────────────────────
// 锚 1：请求体落目标栏（--to-column 优先覆盖 requestBody 的 direct 列取值）
// ─────────────────────────────────────────────────────────────────────────────

// TestSendToColumnRequestBodyWins 锚 1（修复前红）：send --to-role executor
// --to-column c02 → POST #9 请求体 target.kind=="direct"、target.column==--to-column
// 值 "c02"（且≠发送方 --column regAuthColumn="c01"）、target.role==--to-role 值；
// 桩回 201 后退 0。修复前 target.column==发送方 --column → 三处断言红
// （column 值不等/与发送方同值），退 0 断言亦红（unknown flag 退 2）。
func TestSendToColumnRequestBodyWins(t *testing.T) {
	var hits int
	var lastBody string
	addr := sendToColumnStubServer(t, &hits, &lastBody)
	ctx := context.Background()

	runWantCode(t, "--to-column direct 退 0", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--to-column", "c02",
			"--body", "跨栏正文", "--no-mirror"), addr)
	})
	p := parseSendStubBody(t, hits, lastBody)
	if got, ok := p.Target["kind"].(string); !ok || got != "direct" {
		t.Errorf("target.kind = %v，期望 \"direct\"", p.Target["kind"])
	}
	gotCol, _ := p.Target["column"].(string)
	if gotCol != "c02" {
		t.Errorf("target.column = %q，期望 --to-column 值 \"c02\"（修复前=发送方栏 %q → 红笔锚）",
			gotCol, regAuthColumn)
	}
	if gotCol == regAuthColumn {
		t.Errorf("target.column = %q 与发送方 --column 同值（--to-column 未透传进请求体）", gotCol)
	}
	if got, ok := p.Target["role"].(string); !ok || got != "executor" {
		t.Errorf("target.role = %v，期望 --to-role 值 \"executor\"", p.Target["role"])
	}
}

// TestSendToColumnRequestBodyFallback 锚 1 现状锚（修复前后都绿，防回归）：
// 不传 --to-column → target.column==发送方 --column（§4.2「发到 --column 栏目的
// 该角色」既有同栏目定向口径逐字节不变）。
func TestSendToColumnRequestBodyFallback(t *testing.T) {
	var hits int
	var lastBody string
	addr := sendToColumnStubServer(t, &hits, &lastBody)
	ctx := context.Background()

	runWantCode(t, "不传 --to-column 同栏目 direct 退 0", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--body", "同栏正文", "--no-mirror"), addr)
	})
	p := parseSendStubBody(t, hits, lastBody)
	if got, ok := p.Target["kind"].(string); !ok || got != "direct" {
		t.Errorf("target.kind = %v，期望 \"direct\"", p.Target["kind"])
	}
	if got, _ := p.Target["column"].(string); got != regAuthColumn {
		t.Errorf("target.column = %q，期望发送方 --column %q（同栏目定向现状锚）", got, regAuthColumn)
	}
	if got, ok := p.Target["role"].(string); !ok || got != "executor" {
		t.Errorf("target.role = %v，期望 \"executor\"", p.Target["role"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 锚 2/3：组合冲突本地退 2（--to-column 仅与 --to-role 组合；与 --to-session/
// --bus 同给属用法错误，文案风格对齐既有 --to-project+--bus）
// ─────────────────────────────────────────────────────────────────────────────

// TestSendToColumnConflictWithSession 锚 2（修复前红）：--to-column+--to-session
// 同给 → 本地退 2 桩零命中（零网络），stderr 含「--to-column」与组合语义片段
// 「仅与 --to-role 组合」。修复前 --to-column 是未知 flag → parseFlags 报
// unknown flag 退 2——退码与零命中两要素修复前也绿，红笔判别面=文案片段
// （unknown flag/用法行均不含「仅与 --to-role 组合」）。
func TestSendToColumnConflictWithSession(t *testing.T) {
	var hits int
	var lastBody string
	addr := sendToColumnStubServer(t, &hits, &lastBody)
	ctx := context.Background()

	_, stderr := runWantCode(t, "--to-column+--to-session 退 2", 2, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-column", "c02", "--to-session", "executor-B",
			"--body", "x", "--no-mirror"), addr)
	})
	if hits != 0 {
		t.Errorf("桩命中 = %d，期望 0（本地退 2 零网络，AC「CLI 本地校验缺漏=退出 2 不发请求」）", hits)
	}
	if !strings.Contains(stderr, "--to-column") {
		t.Errorf("stderr = %q，期望含 \"--to-column\"（冲突文案指明该 flag）", stderr)
	}
	if !strings.Contains(stderr, "仅与 --to-role 组合") {
		t.Errorf("stderr = %q，期望含组合语义片段「仅与 --to-role 组合」（修复前=unknown flag 文案 → 红笔锚）", stderr)
	}
}

// TestSendToColumnConflictWithBus 锚 3（修复前红）：--to-column+--bus 同给 →
// 本地退 2 桩零命中，stderr 含「--to-column」与组合语义片段（同锚 2 判别面；
// bus=发送方栏目广播，无目标栏目域可锚，语义同 --to-project+--bus 拒绝）。
func TestSendToColumnConflictWithBus(t *testing.T) {
	var hits int
	var lastBody string
	addr := sendToColumnStubServer(t, &hits, &lastBody)
	ctx := context.Background()

	_, stderr := runWantCode(t, "--to-column+--bus 退 2", 2, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-column", "c02", "--bus",
			"--body", "x", "--no-mirror"), addr)
	})
	if hits != 0 {
		t.Errorf("桩命中 = %d，期望 0（本地退 2 零网络）", hits)
	}
	if !strings.Contains(stderr, "--to-column") {
		t.Errorf("stderr = %q，期望含 \"--to-column\"（冲突文案指明该 flag）", stderr)
	}
	if !strings.Contains(stderr, "仅与 --to-role 组合") {
		t.Errorf("stderr = %q，期望含组合语义片段「仅与 --to-role 组合」（修复前=unknown flag 文案 → 红笔锚）", stderr)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 锚 4：镜像行目标段与 --json resolved_target 解析面
// ─────────────────────────────────────────────────────────────────────────────

// TestSendToColumnMirrorAndResolved 锚 4（修复前红）：--to-role R --to-column C
// 发送成功（桩 201）→ ①镜像行目标段==`<project>/C/R`（mirrorLabel 的 direct 列
// 取值随 requestBody 同步，六字段体例同 TestSendDirectOK）；②--json 模式
// resolved_target.column==C（overview 解析面，键集恰五键断言复用
// assertResolvedTarget）。修复前两处取发送方 --column → 红。
func TestSendToColumnMirrorAndResolved(t *testing.T) {
	var hits int
	var lastBody string
	addr := sendToColumnStubServer(t, &hits, &lastBody)
	ctx := context.Background()

	// 半面①：镜像行目标段=<project>/C/R。
	root := t.TempDir()
	stdout, _ := runWantCode(t, "--to-column 镜像目标段", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--to-column", "c02",
			"--body", "镜像正文", "--mirror-root", root), addr)
	})
	seq, createdAt := parseOKLine(t, stdout)
	lines := readMirrorLines(t, sendMirrorPath(root, regAuthProject, regAuthColumn))
	if len(lines) != 2 {
		t.Fatalf("镜像应头注释+1 条消息共 2 行，实际 %d 行: %q", len(lines), lines)
	}
	parts := strings.Split(lines[1], " | ")
	if len(parts) != 6 {
		t.Fatalf("镜像行应拆出 6 字段，实际 %q", lines[1])
	}
	want := []string{
		strconv.FormatInt(seq, 10),           // seq=服务端分配（桩权威值）
		createdAt,                            // created_at=服务端返回值权威
		"normal",                             // level
		regAuthProject + "/c02/executor",     // 红笔锚：目标段取 --to-column 值（修复前=c01 → 红）
		regAuthSession + "@" + regAuthColumn, // sender_label（发送方身份）
		"镜像正文",                               // 单行正文原样
	}
	for i, w := range want {
		if parts[i] != w {
			t.Errorf("镜像字段 %d = %q，期望 %q", i, parts[i], w)
		}
	}

	// 半面②：--json resolved_target.column==C（键集恰五键）。
	rootJSON := t.TempDir()
	stdout, _ = runWantCode(t, "--to-column --json resolved_target", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--to-column", "c02", "--body", "json 正文",
			"--json", "--mirror-root", rootJSON), addr)
	})
	assertResolvedTarget(t, stdout, map[string]string{
		"kind": "direct", "project": regAuthProject, "column": "c02",
		"role": "executor", "level": "normal",
	})
}

// TestSendToColumnMirrorAndResolvedFallback 锚 4 现状锚（修复前后都绿，防回归）：
// 不传 --to-column → 镜像行目标段与 --json resolved_target.column 维持发送方
// --column（解析面/mirrorLabel 缺省回落口径不变）。
func TestSendToColumnMirrorAndResolvedFallback(t *testing.T) {
	var hits int
	var lastBody string
	addr := sendToColumnStubServer(t, &hits, &lastBody)
	ctx := context.Background()

	root := t.TempDir()
	stdout, _ := runWantCode(t, "不传 --to-column 镜像现状", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--body", "同栏镜像正文",
			"--mirror-root", root), addr)
	})
	seq, createdAt := parseOKLine(t, stdout)
	lines := readMirrorLines(t, sendMirrorPath(root, regAuthProject, regAuthColumn))
	if len(lines) != 2 {
		t.Fatalf("镜像应头注释+1 条消息共 2 行，实际 %d 行: %q", len(lines), lines)
	}
	parts := strings.Split(lines[1], " | ")
	if len(parts) != 6 {
		t.Fatalf("镜像行应拆出 6 字段，实际 %q", lines[1])
	}
	want := []string{
		strconv.FormatInt(seq, 10),
		createdAt,
		"normal",
		regAuthProject + "/" + regAuthColumn + "/executor", // 现状锚：目标段=发送方栏
		regAuthSession + "@" + regAuthColumn,
		"同栏镜像正文",
	}
	for i, w := range want {
		if parts[i] != w {
			t.Errorf("镜像字段 %d = %q，期望 %q", i, parts[i], w)
		}
	}

	rootJSON := t.TempDir()
	stdout, _ = runWantCode(t, "不传 --to-column --json 现状", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--body", "同栏 json 正文",
			"--json", "--mirror-root", rootJSON), addr)
	})
	assertResolvedTarget(t, stdout, map[string]string{
		"kind": "direct", "project": regAuthProject, "column": regAuthColumn,
		"role": "executor", "level": "normal",
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 锚 5：跨项目+跨栏组合合法性（防过收敛把 --to-project+--to-role+--to-column
// 组合误拒）
// ─────────────────────────────────────────────────────────────────────────────

// TestSendToColumnCrossProjectCombo 锚 5（修复前红）：--to-project proj-b
// --to-role executor --to-column c02 组合合法（201 退 0），请求体 target 含
// project/column/role 三键（kind=direct）——跨项目 direct 的落点=目标项目内
// --to-column 栏目（服务端 direct 形态三域齐收）。修复前 unknown flag → 退 2
// 零命中 → 退码与命中两断言红。
func TestSendToColumnCrossProjectCombo(t *testing.T) {
	var hits int
	var lastBody string
	addr := sendToColumnStubServer(t, &hits, &lastBody)
	ctx := context.Background()

	runWantCode(t, "跨项目+跨栏组合退 0", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-project", "proj-b", "--to-role", "executor", "--to-column", "c02",
			"--body", "跨项目跨栏正文", "--no-mirror"), addr)
	})
	p := parseSendStubBody(t, hits, lastBody)
	if got, ok := p.Target["kind"].(string); !ok || got != "direct" {
		t.Errorf("target.kind = %v，期望 \"direct\"", p.Target["kind"])
	}
	for _, key := range []string{"project", "column", "role"} {
		if _, ok := p.Target[key]; !ok {
			t.Errorf("target 缺 %q 键（跨项目+跨栏组合须三键齐收）", key)
		}
	}
	if got, _ := p.Target["project"].(string); got != "proj-b" {
		t.Errorf("target.project = %v，期望 \"proj-b\"", p.Target["project"])
	}
	if got, _ := p.Target["column"].(string); got != "c02" {
		t.Errorf("target.column = %v，期望 --to-column 值 \"c02\"", p.Target["column"])
	}
	if got, _ := p.Target["role"].(string); got != "executor" {
		t.Errorf("target.role = %v，期望 \"executor\"", p.Target["role"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 绿笔追加（b2-T6 验收线补充，红笔断言零改动）：全句逐字锚 + 帮助面锚。红笔
// 文件头预留的「全句逐字锚留给绿笔补充测试」在此落位。
// ─────────────────────────────────────────────────────────────────────────────

// TestSendToColumnConflictFullSentence 绿笔全句锚（runWantCode 风格）：两条组合
// 冲突文案逐字锚定（红笔锚 2/3 只锁「仅与 --to-role 组合」稳定片段，全句在此
// 防退化改写）；同给时本地退 2 桩零命中（零网络，与红笔锚 2/3 同判据）。
func TestSendToColumnConflictFullSentence(t *testing.T) {
	var hits int
	var lastBody string
	addr := sendToColumnStubServer(t, &hits, &lastBody)
	ctx := context.Background()

	const wantSession = "--to-column 仅与 --to-role 组合（direct 定向目标栏目；与 --to-session/--bus 同给属用法错误）"
	const wantBus = "--to-column 仅与 --to-role 组合（--bus 是发送方栏目广播，无目标栏目可锚）"

	_, stderr := runWantCode(t, "--to-column+--to-session 全句锚", 2, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-column", "c02", "--to-session", "executor-B",
			"--body", "x", "--no-mirror"), addr)
	})
	if hits != 0 {
		t.Errorf("桩命中 = %d，期望 0（本地退 2 零网络）", hits)
	}
	if !strings.Contains(stderr, wantSession) {
		t.Errorf("stderr = %q，期望含全句 %q（组合冲突文案逐字锚）", stderr, wantSession)
	}

	_, stderr = runWantCode(t, "--to-column+--bus 全句锚", 2, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-column", "c02", "--bus",
			"--body", "x", "--no-mirror"), addr)
	})
	if hits != 0 {
		t.Errorf("桩命中 = %d，期望 0（本地退 2 零网络）", hits)
	}
	if !strings.Contains(stderr, wantBus) {
		t.Errorf("stderr = %q，期望含全句 %q（组合冲突文案逐字锚）", stderr, wantBus)
	}
}

// TestSendToColumnHelpUsageLine 绿笔帮助面锚：send -h 退 0，usage 行含新增
// [--to-column <栏目code>] 段（排版位=--to-project 段之后、--body 段之前），
// 既有三选一括号段与身份尾缀原样在场。裁量留痕：StringVar 的 flag 描述文案
// （--to-role/--to-column 定稿文字）无运行时打印路径（fs.Usage 收敛为单行
// usage，全仓无 PrintDefaults），代码面锚定于 send.go 注册行原文字面量，
// 测试面锚定其唯一用户可见出口=-h usage 行。
func TestSendToColumnHelpUsageLine(t *testing.T) {
	ctx := context.Background()

	_, stderr := runWantCode(t, "send -h 退 0", 0, func() int {
		return runSend(ctx, []string{"-h"}, "")
	})
	const wantSegment = "[--to-project <项目code>] [--to-column <栏目code>] --body <text|->"
	if !strings.Contains(stderr, wantSegment) {
		t.Errorf("-h usage = %q，期望含 %q（--to-column 段插在 --to-project 段后）", stderr, wantSegment)
	}
	if !strings.Contains(stderr, "(--to-role <角色> | --bus | --to-session <会话名>)") {
		t.Errorf("-h usage = %q，期望三选一括号段原样在场", stderr)
	}
	if !strings.Contains(stderr, "身份四参 --project <code>") {
		t.Errorf("-h usage = %q，期望身份尾缀原样在场", stderr)
	}
}
