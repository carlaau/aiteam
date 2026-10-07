package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"aiteam/internal/config"
	"aiteam/internal/server"
	"aiteam/internal/store"
)

// 登记命令族 CLI 测试（B1-8，in-process 端到端）：真 store + 真 server 挂
// httptest，CLI 命令函数注入其地址（serverOverride），断言退出码+stdout/stderr
// 文本——AC1.1/1.2/1.4、AC2.4、AC12.1/12.3 端到端 + 退出码 2/3/4（§4.7）。

// 引导域夹具（控制者裁定）：心跳中间件要求身份四头的 project/column 已登记
// （否则 404 短路），而登记命令的目标实体尚未存在——死锁。测试把身份四头挂
// 引导域 (management, c01)，目标实体是另一个项目/栏目。该死锁已上报总控
// （b1-spec 已知问题），CLI 参数面据此身份与目标分离：身份=全局四参 flag，
// 目标实体=--code flag（详见 register.go 实现注）。
const (
	regAuthProject = "management"
	regAuthColumn  = "c01"
	regAuthSession = "cli-test-session"
	regAuthRole    = "controller"
)

// startRegTestServer 装配登记族测试环境：临时目录真文件库（WAL 语义禁 :memory:）
// + 完整 server + httptest，并 seed 引导域夹具，返回服务地址供 serverOverride 注入。
// extraProjects（b5-W3 质量审收敛）：追加 seed 的额外项目域 code 列表——每项按
// 引导域同款骨架 seed 项目+与引导栏目同 code 的栏目（direct 跨项目落点=发送方
// --column 在目标项目的同名栏目，§4.2 既有口径），供双项目域用例（TestSendToProjectFlag，
// b5-spec §六：跨项目=同库两 project 行，单服务双项目域）。变参对既有单项目调用
// 面零破坏（AC9.4 语义不变）。
func startRegTestServer(t *testing.T, extraProjects ...string) string {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reg-test.db"))
	if err != nil {
		t.Fatalf("打开临时 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ts := httptest.NewServer(server.NewServer(st, config.Default(), server.Version))
	t.Cleanup(ts.Close)
	seedDomain := func(project, projName, colName string) {
		t.Helper()
		if _, err := st.CreateProject(store.Project{Code: project, Name: projName}); err != nil {
			t.Fatalf("seed 项目 %s 失败: %v", project, err)
		}
		if _, _, err := st.CreateColumnWithAudit(project, store.Column{Code: regAuthColumn, Name: colName},
			store.AuditEntry{Action: store.AuditColumnRegister, Detail: "{}"}); err != nil {
			t.Fatalf("seed 项目 %s 栏目 %s 失败: %v", project, regAuthColumn, err)
		}
	}
	seedDomain(regAuthProject, "引导项目", "引导栏目")
	for _, p := range extraProjects {
		seedDomain(p, p+"夹具", p+" 同名栏目")
	}
	return ts.URL
}

// regIdentityArgs 身份四参 flag 值（全局身份，凡带会话语义的命令必填——§4.1）。
func regIdentityArgs() []string {
	return []string{
		"--project", regAuthProject,
		"--column", regAuthColumn,
		"--session", regAuthSession,
		"--role", regAuthRole,
	}
}

// captureStdoutStderr 捕获 fn 执行期间的 stdout/stderr（CLI 命令函数直接写
// os.Stdout/os.Stderr，测试经管道替换捕获）。fn 完毕先还原再关写端，读端
// goroutine ReadAll 至 EOF 不死锁。
func captureStdoutStderr(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("建 stdout 管道失败: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("建 stderr 管道失败: %v", err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	readAll := func(r *os.File) <-chan string {
		ch := make(chan string, 1)
		go func() {
			b, _ := io.ReadAll(r)
			ch <- string(b)
		}()
		return ch
	}
	outCh, errCh := readAll(outR), readAll(errR)
	// defer 单一出口收尾（fn panic/Goexit——如 t.Fatalf 在 fn 内被调——同样执行）：
	// 先还原+关写端（触发读端 EOF），再读净，最后关读端。顺序颠倒会在 Windows 上
	// 把未读完的管道缓冲连同句柄一起关掉，输出随机截断（实测坑）。
	defer func() {
		os.Stdout, os.Stderr = origOut, origErr
		_ = outW.Close()
		_ = errW.Close()
		stdout, stderr = <-outCh, <-errCh
		_ = outR.Close()
		_ = errR.Close()
	}()
	fn()
	return
}

// closedServerAddr 返回一个已关闭 HTTP 服务的地址（确定性的连接拒绝来源，
// TestMissingIdentity 验证「未发请求」、TestServerDown 验证不可达语义共用）。
func closedServerAddr(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.NewServeMux())
	addr := ts.URL
	ts.Close()
	return addr
}

// runWantCode 断言单条命令的退出码为 want，返回捕获的 stdout/stderr。
func runWantCode(t *testing.T, name string, want int, run func() int) (string, string) {
	t.Helper()
	var code int
	stdout, stderr := captureStdoutStderr(t, func() { code = run() })
	if code != want {
		t.Errorf("%s 退出码 = %d（stderr: %q），期望 %d", name, code, stderr, want)
	}
	return stdout, stderr
}

// TestProjectRegisterList AC1.1 端到端：register 退出 0 输出
// `OK project=proj-a id=N` → project list 一行一项目可见（code/name/status/栏目数）。
func TestProjectRegisterList(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()

	stdout, stderr := runWantCode(t, "project register", 0, func() int {
		return runProjectRegister(ctx, append(regIdentityArgs(),
			"--code", "proj-a", "--name", "项目A", "--heartbeat-timeout", "600"), addr)
	})
	if got := "OK project=proj-a id="; !strings.Contains(stdout, got) {
		t.Errorf("register 输出 = %q，期望含 %q（§4.3 输出格式）", stdout, got)
	}
	if stderr != "" {
		t.Errorf("register 成功不应有 stderr 输出，实际 %q", stderr)
	}

	// list：一行一项目，格式 <code> <name> <status> <栏目数>（空格分隔，测试锁定）。
	stdout, _ = runWantCode(t, "project list", 0, func() int {
		return runProjectList(ctx, regIdentityArgs(), addr)
	})
	if want := "proj-a 项目A active 0\n"; !strings.Contains(stdout, want) {
		t.Errorf("project list 输出 = %q，期望含行 %q（AC1.1 list 可见）", stdout, want)
	}

	// name 缺省=code（§4.3 flag 文本承诺）：不带 --name 登记，落库 name=code，
	// list 行无双空格（空 name 会产出 "proj-bare  active 0"）。
	stdout, _ = runWantCode(t, "register 无 --name", 0, func() int {
		return runProjectRegister(ctx, append(regIdentityArgs(), "--code", "proj-bare"), addr)
	})
	if !strings.Contains(stdout, "OK project=proj-bare id=") {
		t.Errorf("无 --name register 输出 = %q，期望含 OK project=proj-bare", stdout)
	}
	stdout, _ = runWantCode(t, "project list 复查", 0, func() int {
		return runProjectList(ctx, regIdentityArgs(), addr)
	})
	if want := "proj-bare proj-bare active 0\n"; !strings.Contains(stdout, want) {
		t.Errorf("list 输出 = %q，期望含行 %q（name 缺省=code 落实）", stdout, want)
	}
}

// TestProjectDuplicate AC1.2：重复登记退出 4，stderr 原样透传 project_exists。
func TestProjectDuplicate(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()
	args := append(regIdentityArgs(), "--code", "proj-a", "--name", "项目A")

	if _, stderr := runWantCode(t, "首次 register", 0, func() int {
		return runProjectRegister(ctx, args, addr)
	}); stderr != "" {
		t.Fatalf("首次 register 不应报错: %q", stderr)
	}

	stdout, stderr := runWantCode(t, "重复 register", 4, func() int {
		return runProjectRegister(ctx, args, addr)
	})
	if !strings.Contains(stderr, "project_exists") {
		t.Errorf("重复 register stderr = %q，期望含 project_exists（AC1.2 退出 4+错误码透传）", stderr)
	}
	if strings.Contains(stdout, "OK") {
		t.Errorf("重复 register stdout = %q，不应有成功输出", stdout)
	}
}

// TestProjectUpdateArchive AC1.3 数据面+AC1.4 端到端：update/archive 退出 0，
// audit 命令可查两条留痕（project.update/project.archive 各一行）；归档后默认
// list 不可见、--all 可见 archived。
func TestProjectUpdateArchive(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()

	// 前置：登记 proj-b。
	runWantCode(t, "register proj-b", 0, func() int {
		return runProjectRegister(ctx, append(regIdentityArgs(), "--code", "proj-b", "--name", "项目B"), addr)
	})

	// update：改 name，退出 0（§4.3 输出 `OK`；审计留痕服务端同事务）。
	stdout, _ := runWantCode(t, "project update", 0, func() int {
		return runProjectUpdate(ctx, append(regIdentityArgs(), "--code", "proj-b", "--name", "新名B"), addr)
	})
	if !strings.Contains(stdout, "OK") {
		t.Errorf("project update 输出 = %q，期望含 OK", stdout)
	}

	// audit 查 project.update 留痕（AC1.4 端到端：CLI 写 → 服务端留痕 → CLI 可查）。
	stdout, _ = runWantCode(t, "audit project.update", 0, func() int {
		return runAudit(ctx, append(regIdentityArgs(), "--action", "project.update", "--limit", "10"), addr)
	})
	if !strings.Contains(stdout, "project.update") {
		t.Errorf("audit 输出 = %q，期望含 project.update 留痕行", stdout)
	}

	// archive：软删，退出 0 输出 `OK archived`。
	stdout, _ = runWantCode(t, "project archive", 0, func() int {
		return runProjectArchive(ctx, append(regIdentityArgs(), "--code", "proj-b"), addr)
	})
	if !strings.Contains(stdout, "OK archived") {
		t.Errorf("project archive 输出 = %q，期望含 OK archived（§4.3）", stdout)
	}
	stdout, _ = runWantCode(t, "audit project.archive", 0, func() int {
		return runAudit(ctx, append(regIdentityArgs(), "--action", "project.archive"), addr)
	})
	if !strings.Contains(stdout, "project.archive") {
		t.Errorf("audit 输出 = %q，期望含 project.archive 留痕行", stdout)
	}

	// 默认 list 不含归档项目；--all 全含且 status=archived（AC1.3 数据面经 CLI 呈现）。
	stdout, _ = runWantCode(t, "project list 默认", 0, func() int {
		return runProjectList(ctx, regIdentityArgs(), addr)
	})
	if strings.Contains(stdout, "proj-b") {
		t.Errorf("归档后默认 list 输出 = %q，不应含 proj-b", stdout)
	}
	stdout, _ = runWantCode(t, "project list --all", 0, func() int {
		return runProjectList(ctx, append(regIdentityArgs(), "--all"), addr)
	})
	if want := "proj-b 新名B archived 0\n"; !strings.Contains(stdout, want) {
		t.Errorf("--all list 输出 = %q，期望含行 %q", stdout, want)
	}
}

// TestColumnFamily AC2.4：column register/update/archive/list 同型（镜像 project 族）；
// 重复登记退出 4；归档后默认 list 不可见、--all 可见。
func TestColumnFamily(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()

	// register：输出 `OK column=05 id=N`（§4.3）。目标栏目挂引导项目下（--project
	// 一参两用=身份 project+父项目，同值自洽）；身份栏目 c01 ≠ 目标 05。
	stdout, _ := runWantCode(t, "column register", 0, func() int {
		return runColumnRegister(ctx, append(regIdentityArgs(),
			"--project", regAuthProject, "--code", "05", "--name", "栏目05"), addr)
	})
	if got := "OK column=05 id="; !strings.Contains(stdout, got) {
		t.Errorf("column register 输出 = %q，期望含 %q", stdout, got)
	}

	// 重复 → 退出 4（服务端 column_exists 透传）。
	_, stderr := runWantCode(t, "column 重复 register", 4, func() int {
		return runColumnRegister(ctx, append(regIdentityArgs(),
			"--project", regAuthProject, "--code", "05", "--name", "重复"), addr)
	})
	if !strings.Contains(stderr, "column_exists") {
		t.Errorf("重复 column register stderr = %q，期望含 column_exists", stderr)
	}

	// list：一行一栏目，格式 <code> <name> <status>。
	stdout, _ = runWantCode(t, "column list", 0, func() int {
		return runColumnList(ctx, append(regIdentityArgs(), "--project", regAuthProject), addr)
	})
	if want := "05 栏目05 active\n"; !strings.Contains(stdout, want) {
		t.Errorf("column list 输出 = %q，期望含行 %q", stdout, want)
	}

	// update：改 name 退出 0。
	if stdout, _ := runWantCode(t, "column update", 0, func() int {
		return runColumnUpdate(ctx, append(regIdentityArgs(),
			"--project", regAuthProject, "--code", "05", "--name", "栏目05改名"), addr)
	}); !strings.Contains(stdout, "OK") {
		t.Errorf("column update 输出不含 OK")
	}

	// archive：`OK archived`；默认 list 不可见、--all 见 archived。
	if stdout, _ := runWantCode(t, "column archive", 0, func() int {
		return runColumnArchive(ctx, append(regIdentityArgs(),
			"--project", regAuthProject, "--code", "05"), addr)
	}); !strings.Contains(stdout, "OK archived") {
		t.Errorf("column archive 输出不含 OK archived")
	}
	stdout, _ = runWantCode(t, "column list 默认", 0, func() int {
		return runColumnList(ctx, append(regIdentityArgs(), "--project", regAuthProject), addr)
	})
	if strings.Contains(stdout, "05 ") {
		t.Errorf("归档后默认 column list = %q，不应含栏目 05", stdout)
	}
	stdout, _ = runWantCode(t, "column list --all", 0, func() int {
		return runColumnList(ctx, append(regIdentityArgs(), "--project", regAuthProject, "--all"), addr)
	})
	if want := "05 栏目05改名 archived\n"; !strings.Contains(stdout, want) {
		t.Errorf("--all column list = %q，期望含行 %q", stdout, want)
	}
}

// TestSessionList AC12.1/12.3 端到端：带四头请求（register）隐式注册会话 →
// session list 一行一会话（name role alive last_seen_at，对齐 #27 响应字段），
// 刚心跳过的会话 alive=true。
func TestSessionList(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()

	// 带身份四头的写请求：中间件隐式注册会话（AC12.3）+ 刷心跳（AC12.1）。
	runWantCode(t, "register 触发隐式注册", 0, func() int {
		return runProjectRegister(ctx, append(regIdentityArgs(), "--code", "proj-s", "--name", "S"), addr)
	})

	// session list：--project 即身份 project（一参两用=过滤）。
	stdout, _ := runWantCode(t, "session list", 0, func() int {
		return runSessionList(ctx, regIdentityArgs(), addr)
	})
	want := regAuthSession + " " + regAuthRole + " true "
	if !strings.Contains(stdout, want) {
		t.Errorf("session list 输出 = %q，期望含行首 %q（name role alive last_seen_at）", stdout, want)
	}
}

// TestMissingIdentity §4.1/§4.7：身份四参缺漏 → CLI 本地校验退出 2 不发请求。
// serverOverride 指向已关闭端口：若误发请求将得退出 3，实测 2 即证明本地拦截。
func TestMissingIdentity(t *testing.T) {
	dead := closedServerAddr(t)
	ctx := context.Background()

	cases := []struct {
		name string
		args []string
		want string // stderr 应指明的缺失参数
	}{
		{"缺 --session", []string{"--project", regAuthProject, "--column", regAuthColumn, "--role", regAuthRole, "--code", "proj-x"}, "--session"},
		{"缺 --role", []string{"--project", regAuthProject, "--column", regAuthColumn, "--session", regAuthSession, "--code", "proj-x"}, "--role"},
		{"缺 --column", []string{"--project", regAuthProject, "--session", regAuthSession, "--role", regAuthRole, "--code", "proj-x"}, "--column"},
		{"缺 --project", []string{"--column", regAuthColumn, "--session", regAuthSession, "--role", regAuthRole, "--code", "proj-x"}, "--project"},
		{"全缺", []string{"--code", "proj-x"}, "--project"},
		{"缺目标 --code", regIdentityArgs(), "--code"},
	}
	for _, tc := range cases {
		_, stderr := runWantCode(t, "缺身份 "+tc.name, 2, func() int {
			return runProjectRegister(ctx, tc.args, dead)
		})
		if !strings.Contains(stderr, tc.want) {
			t.Errorf("缺身份 %s stderr = %q，期望指明 %s", tc.name, stderr, tc.want)
		}
	}

	// 查询类命令同受身份校验（project list 缺身份 → 2）。
	_, stderr := runWantCode(t, "list 缺身份", 2, func() int {
		return runProjectList(ctx, []string{"--session", regAuthSession}, dead)
	})
	if !strings.Contains(stderr, "--project") {
		t.Errorf("list 缺身份 stderr = %q，期望指明 --project", stderr)
	}
}

// TestServerDown AC17.4/§4.7：服务不可达 → 退出 3，stderr 形如
// `ERROR: 服务不可达 <原因>（server=<addr>）`。
func TestServerDown(t *testing.T) {
	dead := closedServerAddr(t)
	ctx := context.Background()

	stdout, stderr := runWantCode(t, "服务不可达", 3, func() int {
		return runProjectList(ctx, regIdentityArgs(), dead)
	})
	if !strings.Contains(stderr, "ERROR: 服务不可达") || !strings.Contains(stderr, "server=") {
		t.Errorf("不可达 stderr = %q，期望含 `ERROR: 服务不可达` 与 server=<addr>", stderr)
	}
	if stdout != "" {
		t.Errorf("不可达 stdout = %q，期望空", stdout)
	}
}

// TestSummarizeDetail detail 摘要纯函数：多行压缩+恰 48 rune 边界+中文安全截断
// （audit 输出行摘要列的截断防线——多字节切碎即非法 UTF-8，纯函数面最值得锁）。
func TestSummarizeDetail(t *testing.T) {
	// 恰 48 rune：原样保留不截断。
	s48 := strings.Repeat("中", 48)
	if got := summarizeDetail(s48); got != s48 {
		t.Errorf("48 rune 摘要 = %q，期望原样", got)
	}
	// 49 rune：截至 48+省略号（结果恰 49 rune），无多字节切碎。
	got := summarizeDetail(strings.Repeat("中", 49))
	if !strings.HasSuffix(got, "…") || utf8.RuneCountInString(got) != 49 {
		t.Errorf("49 rune 摘要 = %q（rune 数 %d），期望 48 rune+省略号", got, utf8.RuneCountInString(got))
	}
	// 换行/连续空白压成单行。
	if got := summarizeDetail("{\"a\":1}\n{\"b\":  2}"); strings.Contains(got, "\n") || strings.Contains(got, "  ") {
		t.Errorf("多行摘要 = %q，期望压缩为单行", got)
	}
}

// TestGroupDispatchUnknownVerb 组级分发：未知动词/缺二级动词 → 用法错误退 2
// （本地拒绝不发请求，stderr 回显动词）。
func TestGroupDispatchUnknownVerb(t *testing.T) {
	for name, run := range map[string]func([]string) int{
		"project": RunProjectGroup,
		"column":  RunColumnGroup,
		"session": RunSessionGroup,
	} {
		var code int
		_, stderr := captureStdoutStderr(t, func() { code = run([]string{"bogus"}) })
		if code != 2 {
			t.Errorf("%s 未知动词退出码 = %d，期望 2", name, code)
		}
		if !strings.Contains(stderr, "bogus") {
			t.Errorf("%s 未知动词 stderr = %q，期望回显动词", name, stderr)
		}
	}
	// 空参数（缺二级动词）同退 2。
	var code int
	_, _ = captureStdoutStderr(t, func() { code = RunProjectGroup(nil) })
	if code != 2 {
		t.Errorf("空参数退出码 = %d，期望 2", code)
	}
}

// TestParseFlagsRejectsPositional 位置参数拒绝：多余位置参数 → 用法错误退 2
// （§4.7；防 flag 包静默忽略手滑输入，对齐 serve 层同名防线）。
func TestParseFlagsRejectsPositional(t *testing.T) {
	fs := newFlagSet("aiteam project register", "用法: aiteam project register")
	var id identityFlags
	registerCommonFlags(fs, &id)
	fs.StringVar(new(string), "code", "", "目标 code") // 先定义 flag，确保命中的是位置参数而非未定义 flag
	proceed, code := parseFlags(fs, []string{"--code", "proj-a", "myconfig.json"})
	if proceed || code != 2 {
		t.Errorf("位置参数 proceed=%v code=%d，期望 false/2", proceed, code)
	}
}
