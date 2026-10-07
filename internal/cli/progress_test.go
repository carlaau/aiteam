package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"aiteam/internal/config"
	"aiteam/internal/server"
	"aiteam/internal/store"
)

// 进度上报域 CLI 测试（B8-3，in-process 端到端同 register_test 模式）：真 store +
// 真 server 挂 httptest，命令函数注入其地址（serverOverride），断言退出码+
// stdout/stderr 文本——AC23.2（上报 OK+list 立即可见+身份归属）+ 退出码 2/3/4
// 映射（§4.7）+ RunProgressGroup 分发（上报/list 两形态）。

// lastCapture 记录最近一次穿过中间件的请求（身份四头+query），并发安全——
// 「身份四参透传」「过滤参数透传」的直接对拍面。
type lastCapture struct {
	mu    sync.Mutex
	hdr   http.Header
	query url.Values
}

func (c *lastCapture) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.hdr = r.Header.Clone()
		c.query = r.URL.Query()
		c.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (c *lastCapture) snapshot() (http.Header, url.Values) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hdr, c.query
}

// startProgressTestServer 装配进度域测试环境：临时目录真文件库（WAL 语义禁
// :memory:）+ server 外包 capture 中间件 + httptest，seed 引导域夹具（同
// startRegTestServer 口径），返回服务地址与 capture 供断言。
func startProgressTestServer(t *testing.T) (addr string, cap *lastCapture) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "progress-test.db"))
	if err != nil {
		t.Fatalf("打开临时 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cap = &lastCapture{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.wrap(server.NewServer(st, config.Default(), server.Version)).ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	if _, err := st.CreateProject(store.Project{Code: regAuthProject, Name: "引导项目"}); err != nil {
		t.Fatalf("seed 引导项目失败: %v", err)
	}
	if _, _, err := st.CreateColumnWithAudit(regAuthProject, store.Column{Code: regAuthColumn, Name: "引导栏目"},
		store.AuditEntry{Action: store.AuditColumnRegister, Detail: "{}"}); err != nil {
		t.Fatalf("seed 引导栏目失败: %v", err)
	}
	return ts.URL, cap
}

// progressIdentityArgs 身份四参（session 可换值）：进度域用例要造多会话上报
// （过滤对拍），在引导域基础上换 --session。
func progressIdentityArgs(session string) []string {
	return []string{
		"--project", regAuthProject,
		"--column", regAuthColumn,
		"--session", session,
		"--role", regAuthRole,
	}
}

// requireOKProgress 断言 stdout 为 `OK progress=<正整数>` 形状（§4.2/#31 输出
// 契约），返回解析出的 id。
func requireOKProgress(t *testing.T, name, stdout string) int64 {
	t.Helper()
	line := strings.TrimSpace(stdout)
	f := strings.Fields(line)
	if len(f) != 2 || f[0] != "OK" || !strings.HasPrefix(f[1], "progress=") {
		t.Fatalf("%s 输出 = %q，期望形状 `OK progress=<id>`", name, stdout)
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(f[1], "progress="), 10, 64)
	if err != nil || id <= 0 {
		t.Fatalf("%s 输出 id 非正整数: %q", name, stdout)
	}
	return id
}

// TestProgressReport AC23.2 端到端：最小形态（用例面原文命令）与全参形态各上报
// 一条 → `OK progress=<id>` 退出 0 → list 立即可见且身份四参/落库归属正确
// （身份四头经 capture 对拍请求面，业务字段经 list 行对拍落库面）。
func TestProgressReport(t *testing.T) {
	addr, cap := startProgressTestServer(t)
	ctx := context.Background()

	// ① 最小形态：--batch B8 --task B8-3 --tests pass（AC23.2 原文形态）。
	stdout, stderr := runWantCode(t, "progress 最小上报", 0, func() int {
		return runProgressReport(ctx, append(progressIdentityArgs("s1"),
			"--batch", "B8", "--task", "B8-3", "--tests", "pass"), addr)
	})
	requireOKProgress(t, "progress 最小上报", stdout)
	if stderr != "" {
		t.Errorf("上报成功不应有 stderr 输出，实际 %q", stderr)
	}

	// ② 身份四参透传对拍请求面：四头=身份四参值（§4.1 每次调用转身份四头）。
	hdr, _ := cap.snapshot()
	for name, want := range map[string]string{
		"X-Aiteam-Project": regAuthProject,
		"X-Aiteam-Column":  regAuthColumn,
		"X-Aiteam-Session": "s1",
		"X-Aiteam-Role":    regAuthRole,
	} {
		if got := hdr.Get(name); got != want {
			t.Errorf("请求头 %s = %q，期望 %q（身份四参透传）", name, got, want)
		}
	}

	// ③ 全参形态：--commit/--branch/--summary 透传落库（list 行对拍）。
	if stdout, _ := runWantCode(t, "progress 全参上报", 0, func() int {
		return runProgressReport(ctx, append(progressIdentityArgs("s1"),
			"--batch", "B8", "--task", "B8-9", "--tests", "fail",
			"--commit", "abc1234", "--branch", "feat/b8", "--summary", "测试失败待修"), addr)
	}); requireOKProgress(t, "progress 全参上报", stdout) <= 0 {
		t.Errorf("全参上报 id 应为正整数")
	}

	// ④ list 立即可见（AC23.2）：两行都在，全参字段 commit/branch/tests/summary
	// 原样可见；最小形态行无 commit 值。
	stdout, _ = runWantCode(t, "progress list 复查", 0, func() int {
		return runProgressList(ctx, progressIdentityArgs("s1"), addr)
	})
	if !strings.Contains(stdout, "B8-3") || !strings.Contains(stdout, "B8-9") {
		t.Errorf("list 输出 = %q，期望含 B8-3/B8-9 两行（AC23.2 list 立即可见）", stdout)
	}
	for _, want := range []string{"abc1234", "feat/b8", "fail", "测试失败待修", "pass"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("list 输出 = %q，期望含字段 %q（落库透传）", stdout, want)
		}
	}
}

// TestProgressReportAutoLayer 自动层形态（b8-spec §1.1「batch/task 空」+B8-T2，
// 表 11 注释口径「batch/task 手动层携带自动层可空、commit_hash 自动层必有」）：
// --commit 非空且 batch/task 缺省 = post-commit hook 合法形态 → OK 退出 0 →
// list 可见该 hash+branch（batch/task 空列）；手动层语义（无 commit 时 batch/task
// 必填退 2）由 TestProgressReportValidation 锁定不变。
func TestProgressReportAutoLayer(t *testing.T) {
	addr, _ := startProgressTestServer(t)
	ctx := context.Background()

	stdout, stderr := runWantCode(t, "自动层上报", 0, func() int {
		return runProgressReport(ctx, append(progressIdentityArgs("hook-s1"),
			"--commit", "deadbeefcafe", "--branch", "feat/b8"), addr)
	})
	requireOKProgress(t, "自动层上报", stdout)
	if stderr != "" {
		t.Errorf("自动层上报成功不应有 stderr 输出，实际 %q", stderr)
	}

	// list 可见（hook 采值落库）：hash+branch 原样，batch/task 空列。
	listOut, _ := runWantCode(t, "自动层 list 复查", 0, func() int {
		return runProgressList(ctx, progressIdentityArgs("hook-s1"), addr)
	})
	if !strings.Contains(listOut, "deadbeefcafe") || !strings.Contains(listOut, "feat/b8") {
		t.Errorf("自动层 list = %q，期望含 hash+branch（AC23.1 数据面）", listOut)
	}
}

// TestProgressReportValidation --batch/--task 必填本地校验（spec 命令面形态）：
// 缺失退出 2 且不发请求（serverOverride 注入确定不可达地址——若误发请求会退 3，
// 断言 2 即证明本地拦截，§4.7「缺漏退 2 不发请求」）。
func TestProgressReportValidation(t *testing.T) {
	closed := closedServerAddr(t)
	ctx := context.Background()

	cases := []struct {
		name string
		args []string
		want string // stderr 须含的缺失 flag 名
	}{
		{"缺 --batch", append(progressIdentityArgs("s1"), "--task", "B8-3"), "--batch"},
		{"缺 --task", append(progressIdentityArgs("s1"), "--batch", "B8"), "--task"},
		{"两者全缺", progressIdentityArgs("s1"), "--batch"},
	}
	for _, tc := range cases {
		stdout, stderr := runWantCode(t, tc.name, 2, func() int {
			return runProgressReport(ctx, tc.args, closed)
		})
		if !strings.Contains(stderr, tc.want) {
			t.Errorf("%s stderr = %q，期望指明缺失 %s", tc.name, stderr, tc.want)
		}
		if strings.Contains(stdout, "OK") {
			t.Errorf("%s 不应有成功输出，stdout = %q", tc.name, stdout)
		}
	}
}

// TestProgressList #32 查询面：表头+数据行（列=冻结九键）；过滤参数透传
// （capture 对拍 query：project/session 一参两用=身份值，limit 显式透传）；
// --session 过滤隔离他行；--limit 1 倒序取最新。
func TestProgressList(t *testing.T) {
	addr, cap := startProgressTestServer(t)
	ctx := context.Background()

	// seed：s1 两条（B8-3 先、B8-9 后），s2 一条（B8-4）。
	report := func(session, task string) {
		t.Helper()
		runWantCode(t, "seed 上报 "+session+"/"+task, 0, func() int {
			return runProgressReport(ctx, append(progressIdentityArgs(session),
				"--batch", "B8", "--task", task, "--commit", "c-"+task), addr)
		})
	}
	report("s1", "B8-3")
	report("s1", "B8-9")
	report("s2", "B8-4")

	// ① 表头+数据行：表头=冻结九键列名；s1 视角只见 s1 两行（session 过滤透传），
	// s2 行不可见（过滤隔离）。
	stdout, _ := runWantCode(t, "list 表头+行", 0, func() int {
		return runProgressList(ctx, progressIdentityArgs("s1"), addr)
	})
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("list 输出 %d 行（%q），期望表头+2 数据行", len(lines), stdout)
	}
	const wantHeader = "id session batch task commit_hash branch test_status summary created_at"
	if lines[0] != wantHeader {
		t.Errorf("list 表头 = %q，期望 %q（列=冻结九键）", lines[0], wantHeader)
	}
	if !strings.Contains(stdout, "B8-3") || !strings.Contains(stdout, "B8-9") {
		t.Errorf("list 输出 = %q，期望含 s1 两行", stdout)
	}
	if strings.Contains(stdout, "B8-4") {
		t.Errorf("list 输出 = %q，不应含 s2 的 B8-4（session 过滤隔离）", stdout)
	}

	// ② 过滤参数透传对拍：project/session=身份值（一参两用），--limit 1 显式透传；
	// 倒序取最新（B8-9 在后报的更新——created_at 同秒靠 id DESC 决序）。
	stdout, _ = runWantCode(t, "list --limit 1", 0, func() int {
		return runProgressList(ctx, append(progressIdentityArgs("s1"), "--limit", "1"), addr)
	})
	lines = strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("list --limit 1 输出 %d 行，期望表头+1 数据行", len(lines))
	}
	if !strings.Contains(stdout, "B8-9") || strings.Contains(stdout, "B8-3") {
		t.Errorf("list --limit 1 = %q，期望倒序最新 B8-9", stdout)
	}
	_, q := cap.snapshot()
	if got := q.Get("project"); got != regAuthProject {
		t.Errorf("query project = %q，期望 %q（一参两用透传）", got, regAuthProject)
	}
	if got := q.Get("session"); got != "s1" {
		t.Errorf("query session = %q，期望 s1（一参两用透传）", got)
	}
	if got := q.Get("limit"); got != "1" {
		t.Errorf("query limit = %q，期望 1（显式透传）", got)
	}

	// ③ 换会话视角：s2 身份只见 B8-4。
	stdout, _ = runWantCode(t, "list s2 视角", 0, func() int {
		return runProgressList(ctx, progressIdentityArgs("s2"), addr)
	})
	if !strings.Contains(stdout, "B8-4") || strings.Contains(stdout, "B8-3") {
		t.Errorf("s2 视角 list = %q，期望仅含 B8-4", stdout)
	}
}

// TestProgressErrors 错误映射（§4.7 对拍 mapExitCode）：服务端 400（--tests 非法
// 枚举）与 404（身份栏目未登记，#31 冻结错误 column_not_found）→ 退出 4+错误码
// 透传 stderr；serve 不可达 → 退出 3。
func TestProgressErrors(t *testing.T) {
	addr, _ := startProgressTestServer(t)
	closed := closedServerAddr(t)
	ctx := context.Background()

	// ① --tests 非法值透传服务端 400 param_invalid → 退出 4（仓内惯例：可选字段
	// 枚举不本地重复校验，服务端拒绝原样透传）。
	_, stderr := runWantCode(t, "tests 非法枚举", 4, func() int {
		return runProgressReport(ctx, append(progressIdentityArgs("s1"),
			"--batch", "B8", "--task", "B8-3", "--tests", "badval"), addr)
	})
	if !strings.Contains(stderr, "test_status") {
		t.Errorf("--tests badval stderr = %q，期望含 test_status（服务端 400 信息）", stderr)
	}

	// ② 身份栏目未登记 → 中间件 404 column_not_found（§2.2 #31 冻结错误语义）→ 退 4。
	_, stderr = runWantCode(t, "身份栏目未登记", 4, func() int {
		return runProgressReport(ctx, []string{
			"--project", regAuthProject, "--column", "c99",
			"--session", "s1", "--role", regAuthRole,
			"--batch", "B8", "--task", "B8-3",
		}, addr)
	})
	if !strings.Contains(stderr, "column_not_found") {
		t.Errorf("未登记栏目 stderr = %q，期望含 column_not_found（#31 冻结错误）", stderr)
	}

	// ③ serve 不可达 → 退出 3+不可达语义文本。
	_, stderr = runWantCode(t, "serve 不可达", 3, func() int {
		return runProgressReport(ctx, append(progressIdentityArgs("s1"),
			"--batch", "B8", "--task", "B8-3"), closed)
	})
	if !strings.Contains(stderr, "服务不可达") {
		t.Errorf("不可达 stderr = %q，期望含「服务不可达」（AC17.4 语义）", stderr)
	}

	// ④ list 同款不可达映射（两命令共用 fail/mapExitCode）。
	runWantCode(t, "list 不可达", 3, func() int {
		return runProgressList(ctx, progressIdentityArgs("s1"), closed)
	})
}

// TestProgressGroupDispatch RunProgressGroup 分发（cli.go 注册行）：无二级动词=
// 上报、list=查询、空参=用法错误退 2、未知词走上报分支被位置参数校验拦下退 2。
// 服务地址经 --server flag 注入（查找链第一级胜出，免环境变量）。
func TestProgressGroupDispatch(t *testing.T) {
	addr, _ := startProgressTestServer(t)

	// ① 上报形态（无二级动词）。
	if stdout, _ := runWantCode(t, "组入口上报", 0, func() int {
		return RunProgressGroup(append([]string{"--server", addr}, append(progressIdentityArgs("s1"),
			"--batch", "B8", "--task", "B8-1")...))
	}); requireOKProgress(t, "组入口上报", stdout) <= 0 {
		t.Errorf("组入口上报 id 应为正整数")
	}

	// ② list 形态（二级动词 list 紧跟 progress——`aiteam progress list` 命令面形态）。
	stdout, _ := runWantCode(t, "组入口 list", 0, func() int {
		return RunProgressGroup(append([]string{"list", "--server", addr}, progressIdentityArgs("s1")...))
	})
	if !strings.Contains(stdout, "B8-1") {
		t.Errorf("组入口 list = %q，期望含刚上报的 B8-1", stdout)
	}

	// ③ 空参 → 用法错误退 2。
	runWantCode(t, "空参", 2, func() int { return RunProgressGroup(nil) })

	// ④ 未知词（非 list）走上报分支 → 位置参数校验退 2。
	runWantCode(t, "未知词", 2, func() int {
		return RunProgressGroup([]string{"--server", addr, "frobnicate"})
	})
}
