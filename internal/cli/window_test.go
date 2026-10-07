package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aiteam/internal/store"
)

// 时间窗命令族 CLI 测试（B4-6，in-process 端到端）：复用登记/资源族测试基座
// （startResTestServer/resIdentityArgs/captureStdoutStderr/closedServerAddr/
// runWantCode），断言退出码+stdout/stderr 文本——AC3.1~3.4 端到端 + 退出码
// 2/3/4（§4.7）。
//
// 时钟口径：#22 判窗取钟走 store.NowInTz 包级注入点（handler_window.go as_of 与
// InWindow 判定同源），injectWindowClock 固定钟面翻转 waiting/allowed（仅限串行
// 用例，save/restore 挂 Cleanup）；#21 写口审计钟走 types.NowUTC 另一注入点，
// 本组测试不依赖。
//
// 夹具口径：身份域一参两用（--project/--column 既是身份又是目标域，同 resource
// 组）；跨栏目夹具经 column register 纯端到端登记（身份域必须已登记的心跳死锁
// 同 resource_test.go 注）。

// injectWindowClock 固定服务端判窗钟面（store.NowInTz 包级注入点，#22 as_of 与
// InWindow 判定同源）。仅限串行用例：包级变量替换与并发读有竞态面。
func injectWindowClock(t *testing.T, hhmm string) {
	t.Helper()
	orig := store.NowInTz
	store.NowInTz = func(string) (string, error) { return hhmm, nil }
	t.Cleanup(func() { store.NowInTz = orig })
}

// TestSetAndNow AC3.1/3.2 端到端：set 覆盖式写单窗 → now 权威查询随注入钟面翻转
// waiting/allowed（窗外 15:00 挂起非报错、窗内 01:00 带窗值 allowed）；无窗阶段
// 不带窗值（#22 window null）。
func TestSetAndNow(t *testing.T) {
	addr, _ := startResTestServer(t)
	ctx := context.Background()

	stdout, stderr := runWantCode(t, "set S4", 0, func() int {
		return runWindowSet(ctx, append(regIdentityArgs(),
			"--stage", "S4", "--from", "23:00", "--to", "09:00"), addr)
	})
	if !strings.Contains(stdout, "OK window S4=23:00-09:00 (column c01)") {
		t.Errorf("set 输出 = %q，期望含 OK window S4=23:00-09:00 (column c01)（§4.5 输出格式）", stdout)
	}
	if stderr != "" {
		t.Errorf("set 成功不应有 stderr 输出，实际 %q", stderr)
	}

	// 窗外（15:00 ∉ 23:00-09:00）→ waiting（AC3.2 窗外挂起非报错）。
	injectWindowClock(t, "15:00")
	stdout, _ = runWantCode(t, "now 窗外", 0, func() int {
		return runWindowNow(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, "S4=waiting(23:00-09:00)") {
		t.Errorf("now 窗外输出 = %q，期望含 S4=waiting(23:00-09:00)（AC3.2）", stdout)
	}
	if !strings.Contains(stdout, "c01: ") {
		t.Errorf("now 输出 = %q，期望栏目行首缀 c01:（一栏目一行）", stdout)
	}
	if strings.Contains(stdout, "S0=allowed(") {
		t.Errorf("now 输出 = %q，无窗阶段 S0 不应带窗值（#22 window null）", stdout)
	}

	// 窗内（01:00 ∈ 跨午夜 23:00-09:00）→ allowed 带窗值（AC3.1）。
	injectWindowClock(t, "01:00")
	stdout, _ = runWantCode(t, "now 窗内", 0, func() int {
		return runWindowNow(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, "S4=allowed(23:00-09:00)") {
		t.Errorf("now 窗内输出 = %q，期望含 S4=allowed(23:00-09:00)（AC3.1）", stdout)
	}
}

// TestCrossMidnight AC3.3 跨午夜四判定端到端：两个跨午夜窗 × 窗内/窗外钟面各一，
// 判定与 store.InWindow 语义一致（start>end = [start,24:00)∪[00:00,end]）。
func TestCrossMidnight(t *testing.T) {
	addr, _ := startResTestServer(t)
	ctx := context.Background()

	setS4 := func(from, to string) {
		t.Helper()
		runWantCode(t, "set S4="+from+"-"+to, 0, func() int {
			return runWindowSet(ctx, append(regIdentityArgs(),
				"--stage", "S4", "--from", from, "--to", to), addr)
		})
	}
	nowContains := func(name, hhmm, want string) {
		t.Helper()
		injectWindowClock(t, hhmm)
		stdout, _ := runWantCode(t, name, 0, func() int {
			return runWindowNow(ctx, regIdentityArgs(), addr)
		})
		if !strings.Contains(stdout, want) {
			t.Errorf("%s 输出 = %q，期望含 %q", name, stdout, want)
		}
	}

	// 窗 13:00-01:00：23:00 含当前→窗内；02:00 不含→窗外。
	setS4("13:00", "01:00")
	nowContains("13:00-01:00@23:00 窗内", "23:00", "S4=allowed(13:00-01:00)")
	nowContains("13:00-01:00@02:00 窗外", "02:00", "S4=waiting(13:00-01:00)")

	// 窗 15:00-05:00（覆盖式重复 set）：20:00 含当前→窗内；10:00 不含→窗外。
	setS4("15:00", "05:00")
	nowContains("15:00-05:00@20:00 窗内", "20:00", "S4=allowed(15:00-05:00)")
	nowContains("15:00-05:00@10:00 窗外", "10:00", "S4=waiting(15:00-05:00)")
}

// TestProjectLevel 项目级默认窗（--project-level → #21 ?scope=project，column 维度
// 0）：输出尾缀 (project)；栏目级窗与项目级窗同屏——now 判定栏目级优先、缺席回落
// 项目级（§6.3 覆盖语义的端到端可见面）。
func TestProjectLevel(t *testing.T) {
	addr, _ := startResTestServer(t)
	ctx := context.Background()

	stdout, _ := runWantCode(t, "set 项目级", 0, func() int {
		return runWindowSet(ctx, append(regIdentityArgs(),
			"--project-level", "--stage", "S4", "--from", "23:00", "--to", "09:00"), addr)
	})
	if !strings.Contains(stdout, "OK window S4=23:00-09:00 (project)") {
		t.Errorf("项目级 set 输出 = %q，期望含 OK window S4=23:00-09:00 (project)", stdout)
	}

	// 栏目级覆盖窗 S3=09:00-18:00。
	runWantCode(t, "set 栏目级 S3", 0, func() int {
		return runWindowSet(ctx, append(regIdentityArgs(),
			"--stage", "S3", "--from", "09:00", "--to", "18:00"), addr)
	})

	// now@15:00：S3 栏目级窗内 allowed、S4 项目级窗外 waiting 同屏（now 判定态
	// 不标记层级——层级归属显式归 window list 走 #21 GET 原始行，两读口分工）。
	injectWindowClock(t, "15:00")
	stdout, _ = runWantCode(t, "now 同屏", 0, func() int {
		return runWindowNow(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, "S3=allowed(09:00-18:00)") {
		t.Errorf("now = %q，期望含栏目级 S3=allowed(09:00-18:00)", stdout)
	}
	if !strings.Contains(stdout, "S4=waiting(23:00-09:00)") {
		t.Errorf("now = %q，期望含项目级回落 S4=waiting(23:00-09:00)", stdout)
	}
}

// TestClearAndRepeat --clear 删除该窗（本批语义=清空该域，单窗场景等价）→ 该阶段
// 回 allowed 无窗值、list 面 (未配置)；重复 set=覆盖（新窗值替换旧窗值）。
func TestClearAndRepeat(t *testing.T) {
	addr, _ := startResTestServer(t)
	ctx := context.Background()

	runWantCode(t, "set S4", 0, func() int {
		return runWindowSet(ctx, append(regIdentityArgs(),
			"--stage", "S4", "--from", "23:00", "--to", "09:00"), addr)
	})
	injectWindowClock(t, "15:00")
	stdout, _ := runWantCode(t, "now 清空前", 0, func() int {
		return runWindowNow(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, "S4=waiting(23:00-09:00)") {
		t.Fatalf("清空前 now = %q，期望含 S4=waiting(23:00-09:00)", stdout)
	}

	// --clear：该域清空 → S4 回 allowed 且无窗值。
	stdout, _ = runWantCode(t, "clear", 0, func() int {
		return runWindowSet(ctx, append(regIdentityArgs(), "--clear"), addr)
	})
	if !strings.Contains(stdout, "OK window cleared (column c01)") {
		t.Errorf("clear 输出 = %q，期望含 OK window cleared (column c01)", stdout)
	}
	stdout, _ = runWantCode(t, "now 清空后", 0, func() int {
		return runWindowNow(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, "S4=allowed") || strings.Contains(stdout, "S4=allowed(") {
		t.Errorf("清空后 now = %q，期望 S4=allowed 无窗值", stdout)
	}
	stdout, _ = runWantCode(t, "list 清空后", 0, func() int {
		return runWindowList(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, "c01: (未配置)") {
		t.Errorf("清空后 list = %q，期望含 c01: (未配置)", stdout)
	}

	// 重复 set=覆盖：S3 先 09:00-18:00 后 22:00-06:00，now@12:00 由窗内翻窗外。
	runWantCode(t, "set S3 第一遍", 0, func() int {
		return runWindowSet(ctx, append(regIdentityArgs(),
			"--stage", "S3", "--from", "09:00", "--to", "18:00"), addr)
	})
	injectWindowClock(t, "12:00")
	stdout, _ = runWantCode(t, "now 覆盖前", 0, func() int {
		return runWindowNow(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, "S3=allowed(09:00-18:00)") {
		t.Errorf("now = %q，期望含 S3=allowed(09:00-18:00)", stdout)
	}
	runWantCode(t, "set S3 覆盖", 0, func() int {
		return runWindowSet(ctx, append(regIdentityArgs(),
			"--stage", "S3", "--from", "22:00", "--to", "06:00"), addr)
	})
	stdout, _ = runWantCode(t, "now 覆盖后", 0, func() int {
		return runWindowNow(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, "S3=waiting(22:00-06:00)") {
		t.Errorf("now = %q，期望含新窗值 S3=waiting(22:00-06:00)", stdout)
	}
	if strings.Contains(stdout, "09:00-18:00") {
		t.Errorf("now = %q，不应再含旧窗值 09:00-18:00（覆盖式）", stdout)
	}
}

// TestList list 新口径（B4-6b：技术设计 #21 修订行冻结口径——同 path 加 GET，
// 读当前域窗配置原始行）：层级归属显式（项目级段 `project: ...` 前置+栏目级段
// `<col>: ...`）、停用窗可见（`(disabled)` 尾缀）、无项目级窗时项目级段整段省略。
// 身份域一参两用口径不变（恒查身份栏目域）。夹具注：CLI set 每次单窗=域覆盖
// （#21 冻结语义），同域多窗与停用行（set 无 enabled=false 写面）均无 CLI 造法，
// 经 store 直插一次提交（startResTestServer 返回 st，同 resource_test 跨项目
// 夹具先例）。
func TestList(t *testing.T) {
	addr, st := startResTestServer(t)
	ctx := context.Background()

	// 栏目级域直插两行：S4 启用（跨午夜）+ S5 停用（管理面全量可见的读口验收）。
	ref, err := st.ResolveColumn(regAuthProject, regAuthColumn)
	if err != nil {
		t.Fatalf("解析身份栏目失败: %v", err)
	}
	if err := st.SetWindows(ref.ProjectID, ref.ColumnID, []store.WindowInput{
		{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1},
		{Stage: "S5", From: "01:00", To: "02:00", Enabled: 0},
	}, 0, "2026-03-04T05:06:07Z"); err != nil {
		t.Fatalf("直插栏目级窗夹具失败: %v", err)
	}
	// 项目级窗走 CLI（--project-level）。
	runWantCode(t, "set 项目级 S2", 0, func() int {
		return runWindowSet(ctx, append(regIdentityArgs(),
			"--project-level", "--stage", "S2", "--from", "10:00", "--to", "12:00"), addr)
	})

	// 层级归属显式：项目级段在前、栏目级段在后（stage 升序），停用行带 (disabled)。
	stdout, _ := runWantCode(t, "list 层级段", 0, func() int {
		return runWindowList(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, "project: S2=10:00-12:00") {
		t.Errorf("list = %q，期望含项目级段 project: S2=10:00-12:00（层级归属显式）", stdout)
	}
	if !strings.Contains(stdout, regAuthColumn+": S4=23:00-09:00 | S5=01:00-02:00(disabled)") {
		t.Errorf("list = %q，期望含栏目级段（升序+停用标记 (disabled)）", stdout)
	}
	// 项目级窗不再混入栏目级行（#22 判定态近似渲染的旧形态不得残留）。
	if strings.Contains(stdout, regAuthColumn+": S2=") {
		t.Errorf("list = %q，S2 属项目级段不应混入栏目级行", stdout)
	}

	// 项目级省略规则：清掉项目级窗后项目级段整段省略，栏目级段原样。
	runWantCode(t, "clear 项目级", 0, func() int {
		return runWindowSet(ctx, append(regIdentityArgs(), "--project-level", "--clear"), addr)
	})
	stdout, _ = runWantCode(t, "list 项目级省略", 0, func() int {
		return runWindowList(ctx, regIdentityArgs(), addr)
	})
	if strings.Contains(stdout, "project:") {
		t.Errorf("list = %q，项目级域空时项目级段应整段省略", stdout)
	}
	if !strings.Contains(stdout, regAuthColumn+": S4=23:00-09:00 | S5=01:00-02:00(disabled)") {
		t.Errorf("list = %q，清项目级不应影响栏目级段", stdout)
	}

	// 栏目隔离：另一身份栏目域空 → 栏目级段 (未配置)，且查不出 c01 行。
	runWantCode(t, "登记栏目05", 0, func() int {
		return runColumnRegister(ctx, append(regIdentityArgs(), "--code", "05"), addr)
	})
	stdout, _ = runWantCode(t, "list 05 空域", 0, func() int {
		return runWindowList(ctx, resIdentityArgs(regAuthProject, "05"), addr)
	})
	if !strings.Contains(stdout, "05: (未配置)") {
		t.Errorf("list = %q，期望含 05: (未配置)", stdout)
	}
	if strings.Contains(stdout, "c01:") || strings.Contains(stdout, "S4=23:00") {
		t.Errorf("list = %q，不应串入 c01 栏目级窗（恒查身份栏目域）", stdout)
	}
}

// TestClearPreservesOtherWindows --clear --stage 真单窗删除（#15 裁定核心验收，
// B4-6b #21 GET 落地解锁）：先 GET 当前域全量→剔除目标 stage→PUT 剩余全量——
// 多窗场景删单窗不再连坐其他窗（B4-6 域全清近似的升级验收）。
func TestClearPreservesOtherWindows(t *testing.T) {
	addr, st := startResTestServer(t)
	ctx := context.Background()

	// 同域两窗夹具（CLI set 单窗提交会互相覆盖，store 直插一次提交）。
	ref, err := st.ResolveColumn(regAuthProject, regAuthColumn)
	if err != nil {
		t.Fatalf("解析身份栏目失败: %v", err)
	}
	if err := st.SetWindows(ref.ProjectID, ref.ColumnID, []store.WindowInput{
		{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1},
		{Stage: "S3", From: "09:00", To: "18:00", Enabled: 1},
	}, 0, "2026-03-04T05:06:07Z"); err != nil {
		t.Fatalf("直插两窗夹具失败: %v", err)
	}

	// clear S4：剔除式删除，S3 连坐面即本用例的核心反证。
	stdout, _ := runWantCode(t, "clear S4", 0, func() int {
		return runWindowSet(ctx, append(regIdentityArgs(), "--clear", "--stage", "S4"), addr)
	})
	if !strings.Contains(stdout, "OK window S4 cleared (column "+regAuthColumn+")") {
		t.Errorf("clear S4 输出 = %q，期望含 OK window S4 cleared", stdout)
	}

	// now@12:00：S3 窗内 allowed 带窗值（保留）；S4 回 allowed 无窗值（已删）。
	injectWindowClock(t, "12:00")
	stdout, _ = runWantCode(t, "now 删S4后", 0, func() int {
		return runWindowNow(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, "S3=allowed(09:00-18:00)") {
		t.Errorf("now = %q，S3 窗应保留（12:00 窗内 allowed 带窗值）", stdout)
	}
	if !strings.Contains(stdout, "S4=allowed") || strings.Contains(stdout, "S4=allowed(") {
		t.Errorf("now = %q，期望 S4=allowed 无窗值（已删）", stdout)
	}

	// 再 clear S3 → 该域全空（剔除后剩余空=PUT items:[]）。
	runWantCode(t, "clear S3", 0, func() int {
		return runWindowSet(ctx, append(regIdentityArgs(), "--clear", "--stage", "S3"), addr)
	})
	stdout, _ = runWantCode(t, "list 域全空", 0, func() int {
		return runWindowList(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, regAuthColumn+": (未配置)") {
		t.Errorf("list = %q，期望栏目级域全空 (未配置)", stdout)
	}
	if strings.Contains(stdout, "S3=") || strings.Contains(stdout, "S4=") {
		t.Errorf("list = %q，两窗均应已删", stdout)
	}
}

// TestWindowGroupDispatch window 组级分发：空参数/未知动词/缺子参数 → 用法错误退 2
// （镜像 TestResourceGroupDispatch）。
func TestWindowGroupDispatch(t *testing.T) {
	var code int
	_, _ = captureStdoutStderr(t, func() { code = RunWindowGroup(nil) })
	if code != 2 {
		t.Errorf("空参数退出码 = %d，期望 2", code)
	}
	_, stderr := captureStdoutStderr(t, func() { code = RunWindowGroup([]string{"no-such"}) })
	if code != 2 {
		t.Errorf("未知动词退出码 = %d，期望 2", code)
	}
	if !strings.Contains(stderr, "no-such") {
		t.Errorf("未知动词 stderr = %q，期望回显动词", stderr)
	}
	_, _ = captureStdoutStderr(t, func() { code = RunWindowGroup([]string{"set"}) })
	if code != 2 {
		t.Errorf("缺子参数退出码 = %d，期望 2", code)
	}
}

// TestWindowServerDown 本地校验+不可达：now/list 服务不可达退 3；set 缺参/stage
// 非法/时间非法/--clear 互斥 → 本地退 2 不发请求（死地址注入证明，stderr 指明
// 违规参数）。
func TestWindowServerDown(t *testing.T) {
	dead := closedServerAddr(t)
	ctx := context.Background()

	runWantCode(t, "now 不可达", 3, func() int { return runWindowNow(ctx, regIdentityArgs(), dead) })
	runWantCode(t, "list 不可达", 3, func() int { return runWindowList(ctx, regIdentityArgs(), dead) })

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"缺 --stage", []string{"--from", "23:00", "--to", "09:00"}, "--stage"},
		{"缺 --from", []string{"--stage", "S4", "--to", "09:00"}, "--from"},
		{"缺 --to", []string{"--stage", "S4", "--from", "23:00"}, "--to"},
		{"stage 非法 X4", []string{"--stage", "X4", "--from", "23:00", "--to", "09:00"}, "--stage"},
		{"stage 非法 s4", []string{"--stage", "s4", "--from", "23:00", "--to", "09:00"}, "--stage"},
		{"from 9:00", []string{"--stage", "S4", "--from", "9:00", "--to", "09:00"}, "--from"},
		{"from 25:00", []string{"--stage", "S4", "--from", "25:00", "--to", "09:00"}, "--from"},
		{"to 23:5", []string{"--stage", "S4", "--from", "23:00", "--to", "23:5"}, "--to"},
		{"to 0900", []string{"--stage", "S4", "--from", "23:00", "--to", "0900"}, "--to"},
		{"--clear 同给 --from/--to", []string{"--clear", "--stage", "S4", "--from", "23:00", "--to", "09:00"}, "--clear"},
		{"--clear stage 非法", []string{"--clear", "--stage", "X4"}, "--stage"},
	}
	for _, tc := range cases {
		_, stderr := runWantCode(t, "set "+tc.name, 2, func() int {
			return runWindowSet(ctx, append(regIdentityArgs(), tc.args...), dead)
		})
		if !strings.Contains(stderr, tc.want) {
			t.Errorf("set %s stderr = %q，期望指明 %s", tc.name, stderr, tc.want)
		}
	}

	// 单窗删除先 GET（B4-6b #21 读口）：死地址下 GET 先失败透传退 3（证明请求链
	// 真启用，本地无静默降级路径）。
	runWantCode(t, "clear --stage 不可达", 3, func() int {
		return runWindowSet(ctx, append(regIdentityArgs(), "--clear", "--stage", "S4"), dead)
	})
}

// TestWindowServerRejects 服务端拒绝透传面：未登记栏目身份 → 心跳存在性短路 404
// column_not_found（middleware.go ②）经 client.APIError 透传退 4（now 读口同过
// 心跳链——豁免表仅 ping/version）。
func TestWindowServerRejects(t *testing.T) {
	addr, _ := startResTestServer(t)
	ctx := context.Background()
	_, stderr := runWantCode(t, "未登记栏目身份", 4, func() int {
		return runWindowNow(ctx, resIdentityArgs(regAuthProject, "ghost"), addr)
	})
	if !strings.Contains(stderr, "column_not_found") {
		t.Errorf("未登记栏目 stderr = %q，期望含 column_not_found（退 4）", stderr)
	}
}

// TestFormatWindowLines 渲染纯函数：now 段=window 非 nil 带括号窗值（窗内 allowed
// 与 waiting 同型）、nil 无窗值；按栏目分组一行一段（entries 原序线性扫描）；
// list 配置行渲染=项目级段前置（空省略）+栏目级段（空 (未配置)）+停用 (disabled)。
func TestFormatWindowLines(t *testing.T) {
	win := func(start, end string) *windowSpan { return &windowSpan{Start: start, End: end} }
	entries := []windowEntry{
		{Column: "c01", Stage: "S0", Status: "allowed"},
		{Column: "c01", Stage: "S4", Status: "waiting", Window: win("23:00", "09:00")},
		{Column: "05", Stage: "S2", Status: "allowed", Window: win("10:00", "12:00")},
		{Column: "05", Stage: "S3", Status: "waiting", Window: win("09:00", "18:00")},
	}
	nowLines := renderNowLines(entries)
	if len(nowLines) != 2 ||
		nowLines[0] != "c01: S0=allowed | S4=waiting(23:00-09:00)" ||
		nowLines[1] != "05: S2=allowed(10:00-12:00) | S3=waiting(09:00-18:00)" {
		t.Errorf("renderNowLines = %q，期望两栏目行（窗内 allowed 带窗值、nil 无窗值）", nowLines)
	}

	item := func(stage, from, to string, enabled bool) windowConfigItem {
		return windowConfigItem{Stage: stage, Start: from, End: to, Enabled: enabled}
	}
	// 层级段：项目级在前、栏目级在后，停用行 (disabled) 尾缀。
	cfgLines := renderConfigLines(
		[]windowConfigItem{item("S2", "10:00", "12:00", true)},
		[]windowConfigItem{item("S4", "23:00", "09:00", true), item("S5", "01:00", "02:00", false)},
		"c01")
	if len(cfgLines) != 2 ||
		cfgLines[0] != "project: S2=10:00-12:00" ||
		cfgLines[1] != "c01: S4=23:00-09:00 | S5=01:00-02:00(disabled)" {
		t.Errorf("renderConfigLines = %q，期望项目级段+栏目级段（停用标记）", cfgLines)
	}

	// 项目级空：项目级段整段省略。
	noProj := renderConfigLines(nil, []windowConfigItem{item("S4", "23:00", "09:00", true)}, "c01")
	if len(noProj) != 1 || noProj[0] != "c01: S4=23:00-09:00" {
		t.Errorf("renderConfigLines 项目级空 = %q，期望仅栏目级段", noProj)
	}

	// 栏目级空：`<col>: (未配置)`（项目级段仍输出）。
	noCol := renderConfigLines([]windowConfigItem{item("S2", "10:00", "12:00", true)}, nil, "c03")
	if len(noCol) != 2 || noCol[0] != "project: S2=10:00-12:00" || noCol[1] != "c03: (未配置)" {
		t.Errorf("renderConfigLines 栏目级空 = %q，期望项目级段+(未配置)", noCol)
	}

	// 双空：仅 (未配置) 行。
	bothEmpty := renderConfigLines(nil, nil, "c03")
	if len(bothEmpty) != 1 || bothEmpty[0] != "c03: (未配置)" {
		t.Errorf("renderConfigLines 双空 = %q，期望仅 c03: (未配置)", bothEmpty)
	}

	// 空输入零行形态（无 entries 时 now 零输出）。
	if got := renderNowLines(nil); len(got) != 0 {
		t.Errorf("renderNowLines(nil) = %q，期望零行", got)
	}
	if got := splitByColumn(nil); len(got) != 0 {
		t.Errorf("splitByColumn(nil) = %v，期望空", got)
	}
}

// TestSetPathEscape 特殊字符 code 的路径转义回归：服务端栏目/项目 code 不限字符
// 集（只 TrimSpace 非空），col?1 未转义会被 URL 解析在 ? 处截断致 PathValue 失真
// （404 或错域）——set 路径段同 register.go 先例 PathEscape，httptest 侧
// r.URL.Path 已解码应还原原值、RawQuery 不含截断产物。裸 httptest（无心跳中间
// 件）捕获请求：身份 code 任意填，返回 200 空体（client.Do resp nil 跳过解析）。
func TestSetPathEscape(t *testing.T) {
	var gotPath, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	ctx := context.Background()

	// 栏目级：路径两段均含特殊字符，无 query。
	runWantCode(t, "set 特殊字符 code", 0, func() int {
		return runWindowSet(ctx, []string{
			"--project", "p?1", "--column", "col?1",
			"--session", "s", "--role", "r",
			"--stage", "S4", "--from", "23:00", "--to", "09:00",
		}, ts.URL)
	})
	if gotPath != "/api/v1/projects/p?1/columns/col?1/windows" {
		t.Errorf("请求路径 = %q，期望解码后还原特殊字符 code（PathEscape 语义）", gotPath)
	}
	if gotQuery != "" {
		t.Errorf("query = %q，期望空（code 内 ? 不应截断出 query）", gotQuery)
	}

	// 项目级：scope=project query 与转义路径段共存。
	runWantCode(t, "set 项目级特殊字符 code", 0, func() int {
		return runWindowSet(ctx, []string{
			"--project", "p?1", "--column", "col?1",
			"--session", "s", "--role", "r",
			"--project-level", "--clear",
		}, ts.URL)
	})
	if gotPath != "/api/v1/projects/p?1/columns/col?1/windows" || gotQuery != "scope=project" {
		t.Errorf("请求路径 = %q query = %q，期望原路径 + scope=project", gotPath, gotQuery)
	}
}
