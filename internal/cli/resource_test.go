package cli

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aiteam/internal/config"
	"aiteam/internal/server"
	"aiteam/internal/store"
)

// 资源命令族 CLI 测试（B4-3，in-process 端到端）：复用登记族测试基座（同包符号
// startRegTestServer/regIdentityArgs/captureStdoutStderr/closedServerAddr/
// runWantCode），断言退出码+stdout/stderr 文本——AC4.1~4.3、AC17.4 端到端 +
// 退出码 2/3/4（§4.7）。
//
// 夹具口径：资源归属域=身份四头域一参两用（--project/--column 既是身份又是归属，
// 对齐 column register 的 --project 一参两用模式）；跨栏目/跨项目夹具用登记族 CLI
// 命令纯端到端造出（column register / project register），不直插 store。

// startResTestServer 资源族测试环境：与 startRegTestServer 同构（临时真库+完整
// server+httptest+引导域夹具），但返回 store——跨项目过滤夹具需直插（纯 CLI 无法
// 在新项目下建首个栏目：column register 身份/父项目绑定，身份栏目必须先存在，
// B1 死锁同款；资源行仍走 CLI 端到端）。
func startResTestServer(t *testing.T) (string, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "res-test.db"))
	if err != nil {
		t.Fatalf("打开临时 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ts := httptest.NewServer(server.NewServer(st, config.Default(), server.Version))
	t.Cleanup(ts.Close)
	if _, err := st.CreateProject(store.Project{Code: regAuthProject, Name: "引导项目"}); err != nil {
		t.Fatalf("seed 引导项目失败: %v", err)
	}
	if _, _, err := st.CreateColumnWithAudit(regAuthProject, store.Column{Code: regAuthColumn, Name: "引导栏目"},
		store.AuditEntry{Action: store.AuditColumnRegister, Detail: "{}"}); err != nil {
		t.Fatalf("seed 引导栏目失败: %v", err)
	}
	return ts.URL, st
}

// resIdentityArgs 指定归属域的身份四参（资源归属 project/column=身份 project/column）。
func resIdentityArgs(project, column string) []string {
	return []string{
		"--project", project,
		"--column", column,
		"--session", regAuthSession,
		"--role", regAuthRole,
	}
}

// extractResourceID 从 register 成功输出（`OK resource=<id> type=… value=…`）提取
// 资源 id，供 release 等后续命令引用。
func extractResourceID(t *testing.T, stdout string) int64 {
	t.Helper()
	for _, f := range strings.Fields(stdout) {
		if idStr, ok := strings.CutPrefix(f, "resource="); ok {
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil {
				t.Fatalf("解析 resource id 失败: %v（stdout: %q）", err, stdout)
			}
			return id
		}
	}
	t.Fatalf("register 输出未含 resource=<id>: %q", stdout)
	return 0
}

// TestRegisterOK AC4.1 端到端（登记半程）：register 退出 0，输出
// `OK resource=<id> type=port value=8080`（§4.4 输出格式，id 为服务端自增不锁值）。
func TestRegisterOK(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()

	stdout, stderr := runWantCode(t, "resource register", 0, func() int {
		return runResourceRegister(ctx, append(regIdentityArgs(),
			"--type", "port", "--value", "8080", "--note", "网关端口"), addr)
	})
	if got := "OK resource="; !strings.Contains(stdout, got) {
		t.Errorf("register 输出 = %q，期望含 %q（§4.4 输出格式）", stdout, got)
	}
	if !strings.Contains(stdout, "type=port value=8080") {
		t.Errorf("register 输出 = %q，期望含 type=port value=8080", stdout)
	}
	if stderr != "" {
		t.Errorf("register 成功不应有 stderr 输出，实际 %q", stderr)
	}
}

// TestRegisterConflict AC4.2 端到端：同 type+同值全局冲突（D3 判定不含项目过滤，
// 跨栏目重复同值必冲突）→ 退出 4，stderr 透传服务端 message（指明冲突对象
// 「与 <项目>/<栏目> 在用的 resource#<id> 冲突」）。
// 跨栏目造法：CLI 登记栏目 05，身份切到 05 再登记同值——冲突对象指向 c01 首登行。
func TestRegisterConflict(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()

	// 首登：c01 下 port 8080。
	if _, stderr := runWantCode(t, "首登 port 8080", 0, func() int {
		return runResourceRegister(ctx, append(regIdentityArgs(),
			"--type", "port", "--value", "8080"), addr)
	}); stderr != "" {
		t.Fatalf("首登不应报错: %q", stderr)
	}

	// CLI 登记栏目 05 并把身份切到 05（心跳中间件要求身份栏目已登记）。
	runWantCode(t, "登记栏目05", 0, func() int {
		return runColumnRegister(ctx, append(regIdentityArgs(),
			"--project", regAuthProject, "--code", "05"), addr)
	})
	_, stderr := runWantCode(t, "05 域重复登记同值", 4, func() int {
		return runResourceRegister(ctx, append(resIdentityArgs(regAuthProject, "05"),
			"--type", "port", "--value", "8080"), addr)
	})
	if !strings.Contains(stderr, "与 management/c01 在用的 resource#") || !strings.Contains(stderr, "冲突") {
		t.Errorf("冲突 stderr = %q，期望指明冲突对象（AC4.2）", stderr)
	}
}

// TestTypeShorthand type 简写映射（§4.4）：account→account_range、data→data_range
// （映射后透传服务端，list 行 type 列=服务端规范名）；非法 type 值→本地用法错误
// 退 2 不发请求（serverOverride 指向死地址，若误发请求将得退出 3）。
func TestTypeShorthand(t *testing.T) {
	addr := startRegTestServer(t)
	dead := closedServerAddr(t)
	ctx := context.Background()

	// account → account_range：登记后 list --type account 过滤可见。
	runWantCode(t, "register account", 0, func() int {
		return runResourceRegister(ctx, append(regIdentityArgs(),
			"--type", "account", "--value", "acct:1000-1999"), addr)
	})
	stdout, _ := runWantCode(t, "list --type account", 0, func() int {
		return runResourceList(ctx, append(regIdentityArgs(), "--type", "account"), addr)
	})
	if !strings.Contains(stdout, "account_range") || !strings.Contains(stdout, "acct:1000-1999") {
		t.Errorf("list --type account 输出 = %q，期望含 account_range 映射行", stdout)
	}

	// data → data_range：同理。
	runWantCode(t, "register data", 0, func() int {
		return runResourceRegister(ctx, append(regIdentityArgs(),
			"--type", "data", "--value", "d1:5-10"), addr)
	})
	stdout, _ = runWantCode(t, "list --type data", 0, func() int {
		return runResourceList(ctx, append(regIdentityArgs(), "--type", "data"), addr)
	})
	if !strings.Contains(stdout, "data_range") || !strings.Contains(stdout, "d1:5-10") {
		t.Errorf("list --type data 输出 = %q，期望含 data_range 映射行", stdout)
	}
	if strings.Contains(stdout, "acct:1000-1999") {
		t.Errorf("list --type data 输出 = %q，不应含 account 行（type 过滤互斥）", stdout)
	}

	// 非法 type：register 本地退 2（stderr 指明 --type），死地址证明未发请求。
	if _, stderr := runWantCode(t, "register 非法 type", 2, func() int {
		return runResourceRegister(ctx, append(regIdentityArgs(),
			"--type", "bogus", "--value", "1"), dead)
	}); !strings.Contains(stderr, "--type") {
		t.Errorf("非法 type stderr = %q，期望指明 --type", stderr)
	}

	// list --type 非法值同口径本地退 2。
	if _, stderr := runWantCode(t, "list 非法 type", 2, func() int {
		return runResourceList(ctx, append(regIdentityArgs(), "--type", "bogus"), dead)
	}); !strings.Contains(stderr, "--type") {
		t.Errorf("list 非法 type stderr = %q，期望指明 --type", stderr)
	}
}

// TestReleaseAndRelist AC4.3 端到端：release 退出 0 输出 `OK released` → 默认 list
// 不见已释放行、--all 可见（released 状态+released_at 列）→ 同值可再登记。
func TestReleaseAndRelist(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()

	stdout, _ := runWantCode(t, "register port 9090", 0, func() int {
		return runResourceRegister(ctx, append(regIdentityArgs(),
			"--type", "port", "--value", "9090"), addr)
	})
	id := extractResourceID(t, stdout)

	stdout, _ = runWantCode(t, "release", 0, func() int {
		return runResourceRelease(ctx, append(regIdentityArgs(),
			"--id", strconv.FormatInt(id, 10)), addr)
	})
	if !strings.Contains(stdout, "OK released") {
		t.Errorf("release 输出 = %q，期望含 OK released（§4.4）", stdout)
	}

	// 默认 list 不见已释放；--all 可见且状态 released。
	stdout, _ = runWantCode(t, "list 默认", 0, func() int {
		return runResourceList(ctx, regIdentityArgs(), addr)
	})
	if strings.Contains(stdout, "9090") {
		t.Errorf("释放后默认 list = %q，不应含 9090（AC4.3）", stdout)
	}
	stdout, _ = runWantCode(t, "list --all", 0, func() int {
		return runResourceList(ctx, append(regIdentityArgs(), "--all"), addr)
	})
	if !strings.Contains(stdout, "9090 management c01 released") {
		t.Errorf("--all list = %q，期望含 released 行（AC4.3 历史可见）", stdout)
	}

	// 同值可再登记（冲突判定只查 in_use）。
	runWantCode(t, "同值再登记", 0, func() int {
		return runResourceRegister(ctx, append(regIdentityArgs(),
			"--type", "port", "--value", "9090"), addr)
	})

	// --all 与 --type 组合两键同时生效（审查第二轮补）：补登非 port 行作阴性对照，
	// 组合过滤下已释放 port 行（released）与再登记的在用 port 行（in_use）同现，
	// 非 port 行被 type 键滤掉。
	runWantCode(t, "补登 account 对照行", 0, func() int {
		return runResourceRegister(ctx, append(regIdentityArgs(),
			"--type", "account", "--value", "acct:1000-1999"), addr)
	})
	stdout, _ = runWantCode(t, "list --type port --all", 0, func() int {
		return runResourceList(ctx, append(regIdentityArgs(), "--type", "port", "--all"), addr)
	})
	if !strings.Contains(stdout, "9090 management c01 released") {
		t.Errorf("--type port --all = %q，期望含已释放 port 行（组合下历史可见）", stdout)
	}
	if !strings.Contains(stdout, "9090 management c01 in_use") {
		t.Errorf("--type port --all = %q，期望含再登记的在用 port 行", stdout)
	}
	if strings.Contains(stdout, "acct:1000-1999") {
		t.Errorf("--type port --all = %q，不应含非 port 行（type 键生效）", stdout)
	}
}

// TestReleaseServerRejects release 服务端拒绝面（AC4.3 写口错误分支）：不存在的
// id → 404 resource_not_found 原样透传退 4；同一 id 二次 release → 服务端幂等成功
// （store 口径：库留首次 released_at、审计不重复——HTTP 面实为 200 released 非
// 404，已对拍 resources.go ReleaseResource 幂等分支与 #19 handler）→ CLI 退 0。
func TestReleaseServerRejects(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()

	stdout, _ := runWantCode(t, "register", 0, func() int {
		return runResourceRegister(ctx, append(regIdentityArgs(),
			"--type", "port", "--value", "7070"), addr)
	})
	id := extractResourceID(t, stdout)
	runWantCode(t, "首次 release", 0, func() int {
		return runResourceRelease(ctx, append(regIdentityArgs(),
			"--id", strconv.FormatInt(id, 10)), addr)
	})

	// 不存在的 id：服务端 404 resource_not_found → 退出 4 透传。
	_, stderr := runWantCode(t, "release 不存在 id", 4, func() int {
		return runResourceRelease(ctx, append(regIdentityArgs(), "--id", "99999"), addr)
	})
	if !strings.Contains(stderr, "resource_not_found") {
		t.Errorf("不存在 id stderr = %q，期望含 resource_not_found（退 4）", stderr)
	}

	// 同一 id 二次 release：幂等成功 HTTP 200 → CLI 退 0（按实际行为断言）。
	if stdout, _ := runWantCode(t, "二次 release 幂等", 0, func() int {
		return runResourceRelease(ctx, append(regIdentityArgs(),
			"--id", strconv.FormatInt(id, 10)), addr)
	}); !strings.Contains(stdout, "OK released") {
		t.Errorf("二次 release 输出 = %q，期望含 OK released（幂等 200）", stdout)
	}
}

// TestListFilter AC4.1/4.4 端到端：--project/--type 过滤（--project 一参两用=身份
// 兼过滤，同 session list 口径）；行含归属项目/栏目/状态。
func TestListFilter(t *testing.T) {
	addr, st := startResTestServer(t)
	ctx := context.Background()

	// 夹具：c01 下 port 8080 + account acct:1000-1999（management 域）。
	runWantCode(t, "register 8080", 0, func() int {
		return runResourceRegister(ctx, append(regIdentityArgs(),
			"--type", "port", "--value", "8080"), addr)
	})
	runWantCode(t, "register acct", 0, func() int {
		return runResourceRegister(ctx, append(regIdentityArgs(),
			"--type", "account", "--value", "acct:1000-1999"), addr)
	})
	// 跨项目夹具：store 直插 proj-res 项目+栏目（纯 CLI 无法在新项目下建首个栏目，
	// 见 startResTestServer 注）；资源行仍走 CLI 端到端登记。
	if _, err := st.CreateProject(store.Project{Code: "proj-res", Name: "资源过滤副项目"}); err != nil {
		t.Fatalf("seed proj-res 失败: %v", err)
	}
	if _, _, err := st.CreateColumnWithAudit("proj-res", store.Column{Code: "c01", Name: "副项目栏目"},
		store.AuditEntry{Action: store.AuditColumnRegister, Detail: "{}"}); err != nil {
		t.Fatalf("seed proj-res/c01 失败: %v", err)
	}
	runWantCode(t, "register 9090", 0, func() int {
		return runResourceRegister(ctx, append(resIdentityArgs("proj-res", "c01"),
			"--type", "port", "--value", "9090"), addr)
	})

	// --project 过滤（身份 management）：只见本域两行（行含项目/栏目/状态）。
	stdout, _ := runWantCode(t, "list management 域", 0, func() int {
		return runResourceList(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, " port 8080 management c01 in_use\n") ||
		!strings.Contains(stdout, " account_range acct:1000-1999 management c01 in_use\n") {
		t.Errorf("list = %q，期望含 management 域两行（含项目/栏目/状态）", stdout)
	}
	if strings.Contains(stdout, "9090") {
		t.Errorf("list = %q，不应含 proj-res 域行", stdout)
	}

	// --type 过滤：只见 account 行。
	stdout, _ = runWantCode(t, "list --type account", 0, func() int {
		return runResourceList(ctx, append(regIdentityArgs(), "--type", "account"), addr)
	})
	if !strings.Contains(stdout, "acct:1000-1999") || strings.Contains(stdout, "8080") {
		t.Errorf("list --type account = %q，期望仅含 account 行", stdout)
	}

	// --project 过滤（身份 proj-res）：只见 9090 行。
	stdout, _ = runWantCode(t, "list proj-res 域", 0, func() int {
		return runResourceList(ctx, resIdentityArgs("proj-res", "c01"), addr)
	})
	if !strings.Contains(stdout, " port 9090 proj-res c01 in_use\n") || strings.Contains(stdout, "8080") {
		t.Errorf("list proj-res 域 = %q，期望仅含 9090 行", stdout)
	}
}

// TestResourceServerDown AC17.4 沿用+本地校验：服务不可达退 3（stderr 服务不可达
// 语义）；缺身份/缺必填参/--id 非正整数→本地退 2 不发请求（死地址注入证明）。
// 注：任务书用例名 TestServerDown 与 register_test.go 既有同名测试冲突（同包不可
// 重名），故加 Resource 前缀。
func TestResourceServerDown(t *testing.T) {
	dead := closedServerAddr(t)
	ctx := context.Background()

	stdout, stderr := runWantCode(t, "服务不可达", 3, func() int {
		return runResourceList(ctx, regIdentityArgs(), dead)
	})
	if !strings.Contains(stderr, "ERROR: 服务不可达") {
		t.Errorf("不可达 stderr = %q，期望含 `ERROR: 服务不可达`", stderr)
	}
	if stdout != "" {
		t.Errorf("不可达 stdout = %q，期望空", stdout)
	}

	// register 缺参族：死地址证明本地拦截（若误发请求将得退出 3）。
	cases := []struct {
		name string
		args []string
		want string // stderr 应指明的缺失/非法参数
	}{
		{"缺 --project", []string{"--column", regAuthColumn, "--session", regAuthSession, "--role", regAuthRole, "--type", "port", "--value", "1"}, "--project"},
		{"缺 --role", []string{"--project", regAuthProject, "--column", regAuthColumn, "--session", regAuthSession, "--type", "port", "--value", "1"}, "--role"},
		{"缺 --type", append(regIdentityArgs(), "--value", "1"), "--type"},
		{"缺 --value", append(regIdentityArgs(), "--type", "port"), "--value"},
	}
	for _, tc := range cases {
		_, stderr := runWantCode(t, "缺参 "+tc.name, 2, func() int {
			return runResourceRegister(ctx, tc.args, dead)
		})
		if !strings.Contains(stderr, tc.want) {
			t.Errorf("缺参 %s stderr = %q，期望指明 %s", tc.name, stderr, tc.want)
		}
	}

	// release --id：缺失/非正整数/非数字 → 本地退 2（缺失与非正整数走 fail() 报
	// "--id"；非数字由 flag 包解析错误承担，其错误行为 "-id" 单横线）。
	relCases := []struct {
		name string
		args []string
		want string // stderr 应指明的参数名
	}{
		{"缺 --id", regIdentityArgs(), "--id"},
		{"--id 零", append(regIdentityArgs(), "--id", "0"), "--id"},
		{"--id 负数", append(regIdentityArgs(), "--id", "-1"), "--id"},
		{"--id 非数字", append(regIdentityArgs(), "--id", "abc"), "-id"},
	}
	for _, tc := range relCases {
		_, stderr := runWantCode(t, "release "+tc.name, 2, func() int {
			return runResourceRelease(ctx, tc.args, dead)
		})
		if !strings.Contains(stderr, tc.want) {
			t.Errorf("release %s stderr = %q，期望指明 %s", tc.name, stderr, tc.want)
		}
	}
}

// TestResourceGroupDispatch resource 组级分发：空参数/未知动词/缺子参数 → 用法错误
// 退 2。同型 TestGroupDispatchUnknownVerb 的 map 为固定三键（project/column/session）
// 字面量、不含 resource 且该文件在禁改面，故本文件补同型覆盖。
func TestResourceGroupDispatch(t *testing.T) {
	var code int
	_, _ = captureStdoutStderr(t, func() { code = RunResourceGroup(nil) })
	if code != 2 {
		t.Errorf("空参数退出码 = %d，期望 2", code)
	}
	_, stderr := captureStdoutStderr(t, func() { code = RunResourceGroup([]string{"no-such"}) })
	if code != 2 {
		t.Errorf("未知动词退出码 = %d，期望 2", code)
	}
	if !strings.Contains(stderr, "no-such") {
		t.Errorf("未知动词 stderr = %q，期望回显动词", stderr)
	}
	// register 缺子参数（无 --type/--value 等）→ 本地校验退 2 不发请求。
	_, _ = captureStdoutStderr(t, func() { code = RunResourceGroup([]string{"register"}) })
	if code != 2 {
		t.Errorf("缺子参数退出码 = %d，期望 2", code)
	}
}

// TestFormatResourceRow 清单行渲染纯函数：固定六列 id type value project column
// status；released_at 空不输出第 7 列（在用行），非空追加（已释放行历史可见面）。
func TestFormatResourceRow(t *testing.T) {
	inUse := resourceResp{ID: 7, Type: "port", Value: "8080", Project: "management", Column: "c01", Status: "in_use"}
	if got := formatResourceRow(inUse); got != "7 port 8080 management c01 in_use" {
		t.Errorf("在用行 = %q，期望六列形态", got)
	}
	released := inUse
	released.Status = "released"
	released.ReleasedAt = "2026-10-02T00:00:00Z"
	if got := formatResourceRow(released); got != "7 port 8080 management c01 released 2026-10-02T00:00:00Z" {
		t.Errorf("已释放行 = %q，期望追加 released_at 第 7 列", got)
	}
}
