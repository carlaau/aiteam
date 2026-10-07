package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// B8-7 全链集成测试（b8-plan B8-7 / AC23.1+23.2+23.3 集成面 / AC24.1 复验）：
// in-process serve（临时库+随机端口）+ 临时真仓 + go build 真二进制，一个用例串过
// init（distributed+hook 安装）→ 真 commit 自动上报 → list 10 秒内可见 → 手动层
// 上报 OK progress=<id> → stale 流转。B8-1~B8-5 单元面已锚的用例形态（stale 两
// 阈值边界、两树 diff、hook 幂等/静默失败等）不在此重复——本文件只验「全链一次过」
// 与单元 mock 面拼不出的真实缝隙（真二进制子进程 + 真 git hook 触发 + 真 HTTP 链）。
//
// 复用同包既有测试辅助：initGitRepo/gitInRepo（init_test.go）、buildAiteamBin/
// hookPath（init_test.go）、requireOKProgress（progress_test.go）。

// 集成测试专属身份（与单元面 regAuth* 常量隔离，防串扰；seed 经真 CLI 登记而非
// 直插 store——登记链一并过真 HTTP）。
const (
	b8IntProject = "b8int"
	b8IntColumn  = "c01"
	b8IntRole    = "executor"
)

// b8IntTimeLayout 服务端时间文本格式（types 包内未导出常量的同源口径，仅 stale
// 段构造回拨时间戳用）。
const b8IntTimeLayout = "2006-01-02T15:04:05Z"

// startB8Serve 起一个 in-process serve（与生产 serve() 同一装配链：Load→Validate→
// Open→NewServer→Listen）：dbPath 处临时库 + 127.0.0.1:0 随机端口，ready 回调同步
// 真实地址。Cleanup 先关测试自开的 store 连接（后进先出）再 cancel serve（serve 收
// ctx 走优雅停机），并断言 serve 退出无错误。
func startB8Serve(t *testing.T, dbPath string) (addr string) {
	t.Helper()
	dir := filepath.Dir(dbPath)
	cfgPath := filepath.Join(dir, "aiteam-config.json")
	cfg := `{"_doc":"集成测试配置","listen":"127.0.0.1:0","db":{"type":"sqlite","path":"` +
		filepath.ToSlash(dbPath) + `"}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("写测试配置失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() { errCh <- serve(ctx, []string{"--config", cfgPath}, func(a string) { ready <- a }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("serve 未优雅退出: %v", err)
			}
		case <-time.After(shutdownGracePeriod + 5*time.Second):
			t.Error("serve 未在停机宽限期内退出")
		}
	})
	select {
	case addr = <-ready: // Listen 之后才回调，地址即刻可服务
		// ln.Addr() 是裸 host:port——包成 CLI 查找链认可的完整 URL 形态
		addr = "http://" + addr
	case err := <-errCh:
		t.Fatalf("serve 提前退出: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve 10 秒未就绪")
	}
	return addr
}

// seedB8Domain 引导域直插（B1-8 死锁裁定：身份与目标分离——第一个项目无法经 CLI
// 自举，登记身份四头须指向已登记域；与单元面 startRegTestServer 同款口径）。
// serve 未启动时先行插库（schema 迁移幂等），serve 随后打开同一文件库。
func seedB8Domain(t *testing.T, dbPath string) {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("seed 打开库失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.CreateProject(store.Project{Code: b8IntProject, Name: "B8 集成测试项目"}); err != nil {
		t.Fatalf("seed 引导项目失败: %v", err)
	}
	if _, _, err := st.CreateColumnWithAudit(b8IntProject, store.Column{Code: b8IntColumn, Name: "B8 集成栏目"},
		store.AuditEntry{Action: store.AuditColumnRegister, Detail: "{}"}); err != nil {
		t.Fatalf("seed 引导栏目失败: %v", err)
	}
}

// b8IntEnv 集成用例子进程环境：AITEAM_SERVER（CLI 查找链 env 级）+ AITEAM_* 身份
// 四参（post-commit hook 内展开成 flag 值）+ git 提交身份（不依赖全局 git config）。
func b8IntEnv(bin, addr, session string) []string {
	return append(os.Environ(),
		"AITEAM_BIN="+bin,
		"AITEAM_SERVER="+addr,
		"AITEAM_PROJECT="+b8IntProject,
		"AITEAM_COLUMN="+b8IntColumn,
		"AITEAM_SESSION="+session,
		"AITEAM_ROLE="+b8IntRole,
		"GIT_AUTHOR_NAME=b8-integration",
		"GIT_AUTHOR_EMAIL=b8-integration@test",
		"GIT_COMMITTER_NAME=b8-integration",
		"GIT_COMMITTER_EMAIL=b8-integration@test",
	)
}

// runB8Bin 在 dir 内跑真二进制子命令（env 注入），返回 stdout；非零退出经 t.Fatalf
// 判败（runB8BinOK 语义）；轮询窗口内预期可能失败的调用用 runB8BinTry。
func runB8Bin(t *testing.T, bin, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("二进制调用 %v 失败: %v\n%s", args, err, out)
	}
	return string(out)
}

// b8IntIdentity CLI 身份四参 flag（session 可换值——一参两用兼 #32 过滤）。
func b8IntIdentity(session string) []string {
	return []string{
		"--project", b8IntProject,
		"--column", b8IntColumn,
		"--session", session,
		"--role", b8IntRole,
	}
}

// TestIntegrationB8FullChain 全链一次过（顺序六段）：
//
//	① 引导域直插 + serve 就绪 + 真 CLI 连通验证（project list 走真 HTTP，
//	  身份隐式注册 seed-i1 会话——上报即心跳链第一环）；
//	② 临时真仓 aiteam init --topology distributed（真二进制）→ exit 0 + hook 安装
//	  在位 + dev-guide 拓扑节为 distributed 口径（AC24.1 集成面复验——两树 diff 等
//	  细粒度断言归 B8-4 单元面 TestTopology*，不重复）；
//	③ 真 commit（hook 自动层）→ 10 秒内 progress list 可见该 hash+branch（AC23.1）；
//	④ 手动层 aiteam progress --batch B8 --task B8-7 --tests pass → OK progress=<id>
//	  → list 立即可见且身份归属正确（AC23.2）；
//	⑤ stale 流转（AC23.3 集成面）：store 直连同一库把上报回拨 61 分钟 →
//	  LatestProgressBySession 注入 now 判黄；回拨 121 分钟判红——阈值边界单元面
//	  （B8-1 TestStaleFlags）已锚，此处只验「真实库上的链路输出」一次调用；
//	⑥ 上报即心跳收尾：seed/hook/manual 三会话均已被各自调用隐式注册。
func TestIntegrationB8FullChain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "b8-integration.db")
	seedB8Domain(t, dbPath) // ① 引导域（B1 死锁裁定：首个项目无法经 CLI 自举）
	addr := startB8Serve(t, dbPath)
	bin := buildAiteamBin(t)
	workDir := t.TempDir() // 仓外 CLI 调用的中性 cwd

	// ── ① 真 CLI 连通验证：project list 走真 HTTP，身份指向引导域（env 中的
	// AITEAM_* 身份仅供 hook 内展开，CLI 身份四参须显式 flag 携带）──
	seedEnv := b8IntEnv(bin, addr, "seed-i1")
	projList := runB8Bin(t, bin, workDir, seedEnv,
		append([]string{"project", "list"}, b8IntIdentity("seed-i1")...)...)
	if !strings.Contains(projList, b8IntProject) {
		t.Errorf("project list = %q，期望含引导项目 %s（真 CLI→HTTP 链不通）", projList, b8IntProject)
	}

	// ── ② 临时真仓 init --topology distributed（真二进制）──
	repo := t.TempDir()
	initGitRepo(t, repo)
	runB8Bin(t, bin, repo, seedEnv, "init",
		"--project", b8IntProject, "--name", "B8 集成脚手架", "--topology", "distributed")

	// hook 安装在位（AC23.1 前置面：文件存在 + 自动层特征）
	hookData, err := os.ReadFile(hookPath(repo))
	if err != nil {
		t.Fatalf("init 后 post-commit hook 未安装: %v", err)
	}
	for _, want := range []string{"#!/bin/sh", "rev-parse HEAD", "--abbrev-ref", "progress", "exit 0"} {
		if !strings.Contains(string(hookData), want) {
			t.Errorf("hook 脚本缺特征 %q\n%s", want, hookData)
		}
	}

	// dev-guide 拓扑节在位且为 distributed 口径（AC24.1 集成面：真跑 exit 0 + 节在位；
	// 互不残留/两树 diff 细粒度断言归 B8-4 单元面）
	base := readProjectFile(t, repo, "docs/dev-guide/base.md")
	for _, want := range []string{topoBegin, topoEnd, "push 频率", "fetch"} {
		if !strings.Contains(base, want) {
			t.Errorf("distributed 拓扑节缺特征 %q\n%s", want, base)
		}
	}

	// ── ③ 真 commit → hook 自动层 → 10 秒内 list 可见（AC23.1 全链）──
	hookEnv := b8IntEnv(bin, addr, "hook-i1")
	gitInRepo(t, repo, hookEnv, "-c", "user.name=b8-integration", "-c", "user.email=b8-integration@test",
		"commit", "--allow-empty", "-m", "B8 集成：hook 自动层")
	hash := gitInRepo(t, repo, nil, "rev-parse", "HEAD")
	branch := gitInRepo(t, repo, nil, "rev-parse", "--abbrev-ref", "HEAD")

	deadline := time.Now().Add(10 * time.Second) // AC23.1 原文窗口
	seen := ""
	for {
		seen = runB8Bin(t, bin, repo, hookEnv, append(
			[]string{"progress", "list"}, b8IntIdentity("hook-i1")...)...)
		if strings.Contains(seen, hash) && strings.Contains(seen, branch) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("10 秒内 progress list 未可见 hash=%s branch=%s，末次输出:\n%s", hash, branch, seen)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// ── ④ 手动层上报 → OK progress=<id> → list 立即可见且身份归属正确（AC23.2）──
	manualEnv := b8IntEnv(bin, addr, "manual-i1")
	reportArgs := append([]string{"progress", "--batch", "B8", "--task", "B8-7",
		"--tests", "pass", "--summary", "B8-7 全链集成收口"}, b8IntIdentity("manual-i1")...)
	out := runB8Bin(t, bin, repo, manualEnv, reportArgs...)
	requireOKProgress(t, "集成手动上报", out) // `OK progress=<正整数>` 退出 0（runB8Bin 已保证 0）

	// list 立即可见（无需轮询）：session 过滤（一参两用）下本会话行在、他会在话行不在
	// ——行内容含 manual-i1/B8-7/pass = 身份归属+业务字段落库正确。
	listOut := runB8Bin(t, bin, repo, manualEnv, append(
		[]string{"progress", "list"}, b8IntIdentity("manual-i1")...)...)
	if !strings.Contains(listOut, "manual-i1") || !strings.Contains(listOut, "B8-7") || !strings.Contains(listOut, "pass") {
		t.Errorf("手动层 list = %q，期望含 manual-i1/B8-7/pass（身份归属+落库）", listOut)
	}
	if strings.Contains(listOut, "hook-i1") {
		t.Errorf("手动层 list = %q，不应含 hook-i1 行（session 过滤隔离）", listOut)
	}

	// ── ⑤ stale 流转（AC23.3 集成面，store 直连同一库一次调用）──
	st, err := store.Open(dbPath) // serve 持另一连接；WAL+busy_timeout 并发安全
	if err != nil {
		t.Fatalf("直连集成库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	backdate := func(minutes int) {
		t.Helper()
		old := time.Now().UTC().Add(-time.Duration(minutes) * time.Minute).Format(b8IntTimeLayout)
		if _, err := st.DB.Exec(`UPDATE progress_reports SET created_at = ?`, old); err != nil {
			t.Fatalf("回拨 created_at 至 %d 分钟前失败: %v", minutes, err)
		}
		rows, err := st.LatestProgressBySession(types.NowUTC(), 3600) // stale_after 默认 60m
		if err != nil {
			t.Fatalf("LatestProgressBySession 失败: %v", err)
		}
		if len(rows) < 2 {
			t.Fatalf("每会话最新进度应含 hook-i1/manual-i1 两会话，实际 %d 行", len(rows))
		}
		want := store.ProgressStaleYellow // 61m > 1×60m 严格大于 → 黄
		if minutes > 120 {
			want = store.ProgressStaleRed // 121m > 2×60m → 红
		}
		for _, r := range rows {
			if r.Stale != want {
				t.Errorf("回拨 %d 分钟后会话 %s stale = %q，期望 %q", minutes, r.SessionName, r.Stale, want)
			}
		}
	}
	backdate(61)
	backdate(121)

	// ── ⑥ 上报即心跳（#31 挂会话中间件链）：三会话均被各自调用隐式注册——
	// seed/hook/manual 心跳面直查 store 收尾验证（now/timeout 取默认口径，仅验存在）。
	sessions, err := st.ListSessions(b8IntProject, types.NowUTC(), 900)
	if err != nil {
		t.Fatalf("ListSessions 失败: %v", err)
	}
	found := map[string]bool{}
	for _, s := range sessions {
		found[s.Name] = true
	}
	for _, want := range []string{"seed-i1", "hook-i1", "manual-i1"} {
		if !found[want] {
			t.Errorf("会话 %s 未隐式注册（上报即心跳链断裂），实存: %v", want, found)
		}
	}
}
