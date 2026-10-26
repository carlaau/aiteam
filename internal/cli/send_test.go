package cli

import (
	"aiteam/internal/mirror"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// send 命令 CLI 测试（B2-6，in-process 端到端）：真 store + 真 server 挂 httptest
// （复用 register_test.go 的引导域夹具 startRegTestServer），CLI 命令函数注入其
// 地址（serverOverride）；镜像根一律 --mirror-root 指向 t.TempDir()，测试零依赖
// 进程 CWD。覆盖 AC5.1/5.3 命令面、AC13.1~13.4、§4.2 flag 面、§8.3 先服务端后
// 镜像时序、退出码 0/2/3/4/5。
// b4-T1 增量（R1/AC1）：--dry-run 目标解析预览+默认发送输出摘要+--json
// resolved_target；既有输出断言更新均在豁免面（b4-spec §二.R1：仅 send 输出
// 断言）。
// b4-T2 增量（R2/AC2）：--to-session 下划线歧义告警（chat 形态 stderr 一行，
// 警示不拦截）——纯新增测试，既有断言零触碰。

// sendMirrorPath 按 §8.1 冻结契约拼镜像文件路径。路径面属冻结契约，测试独立
// 拼装（与 mirror 包内部拼法互为交叉验证，不依赖其未导出面）。
func sendMirrorPath(root, project, column string) string {
	return filepath.Join(root, ".aiteam", "mirror-"+project+"-"+column+".md")
}

// readMirrorLines 读镜像文件并按行拆分（去尾换行产生的空尾行）。
func readMirrorLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取镜像文件 %s 失败: %v", path, err)
	}
	s := strings.TrimRight(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// assertNoMirrorFile 断言镜像文件与 .aiteam 目录均不存在（§8.3「服务端失败绝不
// 触碰镜像文件」的最强形态：连首建目录都不产生）。
func assertNoMirrorFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("镜像文件 %s 不应存在（服务端失败零触碰），stat: %v", path, err)
	}
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf(".aiteam 目录 %s 不应被创建，stat: %v", dir, err)
	}
}

// injectStdin 用管道替换 os.Stdin：写入 content 后关写端（读端 ReadAll 至 EOF），
// Cleanup 还原。模拟 §4.1「stdin 管道自动读取」。
func injectStdin(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建 stdin 管道失败: %v", err)
	}
	if _, err := w.WriteString(content); err != nil {
		t.Fatalf("写 stdin 管道失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关 stdin 写端失败: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
	})
}

// parseOKLine 解析人类可读成功行 `OK seq=<n> time=<ts>`（§4.2 输出格式），返回
// 服务端分配的 seq 与服务端时间。b4-T1 起成功输出为两行（OK 行+目标解析摘要
// 行，b4-spec §二.R1 输出增强），OK 行恒为首行——取首行解析（豁免面更新：仅
// send 输出断言）。
func parseOKLine(t *testing.T, stdout string) (int64, string) {
	t.Helper()
	line := strings.TrimSpace(strings.SplitN(stdout, "\n", 2)[0])
	rest, ok := strings.CutPrefix(line, "OK seq=")
	if !ok {
		t.Fatalf("成功输出 = %q，期望 OK seq=<n> time=<ts> 形状", stdout)
	}
	seqStr, ts, ok := strings.Cut(rest, " time=")
	if !ok {
		t.Fatalf("成功输出 = %q，缺 time= 段", stdout)
	}
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil {
		t.Fatalf("成功输出 seq 非整数: %q（%v）", seqStr, err)
	}
	return seq, ts
}

// probeMessage #13 历史查询行（§2.2 #13 字段面子集，落库断言用）。
type probeMessage struct {
	Seq   int64  `json:"seq"`
	Kind  string `json:"kind"`
	Level string `json:"level"`
	Body  string `json:"body"`
}

// historyProbe 以身份四头 GET #13 查历史（project 必带、kind 可选过滤）。带四头
// 的 GET 同时经心跳中间件 upsert 会话——这也是测试预注册目标会话的零杂音手段
// （只 upsert sessions 行，不写 messages）。integration_b2_test.go 的 integHistory
// 泛化版消重收编（B2 整批自审 R3）：本 helper 退化为引导域定参薄封装。
func historyProbe(t *testing.T, addr, session, role, kind string) []probeMessage {
	t.Helper()
	extra := url.Values{}
	if kind != "" {
		extra.Set("kind", kind)
	}
	return integHistory(t, addr, regAuthProject, regAuthColumn, session, role, extra)
}

// TestSendDirectOK AC13.1 正向端到端：send direct → `OK seq=N time=...` 退 0，
// 镜像文件新增行含该 seq 且六字段逐项正确（§8.2 行格式）。
func TestSendDirectOK(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()

	body := "端口 8080 已占用，请改用 8081"
	stdout, stderr := runWantCode(t, "send direct", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--level", "block",
			"--body", body, "--mirror-root", root), addr)
	})
	if stderr != "" {
		t.Errorf("send 成功不应有 stderr 输出，实际 %q", stderr)
	}
	seq, createdAt := parseOKLine(t, stdout)
	if seq <= 0 || createdAt == "" {
		t.Fatalf("OK 行 seq/time 非法: seq=%d time=%q", seq, createdAt)
	}

	// AC13.1：send 返回 0 ⇔ 镜像新增行且含该 seq——读文件断言行内容六字段。
	lines := readMirrorLines(t, sendMirrorPath(root, regAuthProject, regAuthColumn))
	if len(lines) != 2 {
		t.Fatalf("镜像应含头注释+1 条消息共 2 行，实际 %d 行: %q", len(lines), lines)
	}
	if lines[0] != mirror.HeaderComment {
		t.Errorf("首行 = %q，期望头注释 %q", lines[0], mirror.HeaderComment)
	}
	parts := strings.Split(lines[1], " | ")
	if len(parts) != 6 {
		t.Fatalf("消息行应拆出 6 字段，实际 %q", lines[1])
	}
	want := []string{
		strconv.FormatInt(seq, 10), // seq=服务端分配
		createdAt,                  // created_at=服务端返回值权威（AC5.4）
		"block",                    // level
		regAuthProject + "/" + regAuthColumn + "/executor", // direct 目标摘要
		regAuthSession + "@" + regAuthColumn,               // sender_label
		body,                                               // 单行正文原样
	}
	for i, w := range want {
		if parts[i] != w {
			t.Errorf("镜像字段 %d = %q，期望 %q", i, parts[i], w)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 跨项目定向（b5-W3，--to-project flag 面，TestSendDirectOK 样板就近新增）
// ─────────────────────────────────────────────────────────────────────────────

// crossBProject/crossBSession B 域夹具常量。B 域经 startRegTestServer 变参 seed
// （项目+与 A 域同 code 的栏目——direct 跨项目落点=发送方 --column 在目标项目的
// 同名栏目，§4.2 既有口径，CLI flag 面无 --to-column；CLI column register 的
// --project 一参两用=身份 project+父项目，无法以 A 身份给 B 建栏目，故 seed 走
// store 直插收敛进族夹具）。
const (
	crossBProject = "proj-b"
	crossBSession = "executor-B"
)

// TestSendToProjectFlag b5-W3 跨项目定向 CLI 面（AC2/AC3）：①--to-project 给定 →
// 请求体 target JSON 含 project 键（双项目域实测：消息落 B 域可查、A 域不可见）
// 且镜像行追加第七字段 to_project、direct 摘要=目标项目 code 前缀（SP3）；
// ②--to-project+--bus 同给 → 本地退 2 零网络请求（死端口手法）；③--to-project
// 单给无 --to-role/--to-session → 既有「三选一缺选」退 2 天然覆盖；④404
// project_not_found → rc=4+冻结文案「目标项目不存在: <code>」（§4.2）。项目内
// 回归（AC9.4+SP2）：空白串 toProject 视同缺省 → 项目内落库+镜像行仍六字段。
func TestSendToProjectFlag(t *testing.T) {
	addr := startRegTestServer(t, crossBProject)
	root := t.TempDir()
	ctx := context.Background()
	sendArgs := func(extra ...string) []string {
		return append(append(regIdentityArgs(), "--mirror-root", root), extra...)
	}

	// ① 跨项目 direct（A→B）：rc=0；project 键端到端证明=消息落 B 域（B 侧身份
	// #13 可查恰 1 条）、A 域不可见。
	stdout, stderr := runWantCode(t, "跨项目 direct", 0, func() int {
		return runSend(ctx, sendArgs("--to-project", crossBProject,
			"--to-role", "executor", "--body", "跨项目指令"), addr)
	})
	if stderr != "" {
		t.Errorf("跨项目 send 成功不应有 stderr 输出，实际 %q", stderr)
	}
	seq, createdAt := parseOKLine(t, stdout)
	bQuery := url.Values{}
	bQuery.Set("kind", "direct")
	bMsgs := integHistory(t, addr, crossBProject, regAuthColumn, crossBSession, "executor", bQuery)
	if len(bMsgs) != 1 || bMsgs[0].Body != "跨项目指令" {
		t.Errorf("B 域 direct 落库 = %+v，期望恰 1 条 body=跨项目指令（project 键端到端证明）", bMsgs)
	}
	if aMsgs := historyProbe(t, addr, regAuthSession, regAuthRole, "direct"); len(aMsgs) != 0 {
		t.Errorf("A 域 direct = %+v，期望 0 条（跨项目消息落 B 域）", aMsgs)
	}

	// 镜像行（AC3/SP3）：第七字段 to_project=B code；direct 摘要=B/<栏目>/<角色>
	//（目标项目 code 前缀，SP3）。六字段拆分断言体例同 TestSendDirectOK。
	lines := readMirrorLines(t, sendMirrorPath(root, regAuthProject, regAuthColumn))
	if len(lines) != 2 {
		t.Fatalf("跨项目镜像应头注释+1 条消息共 2 行，实际 %d 行: %q", len(lines), lines)
	}
	parts := strings.Split(lines[1], " | ")
	if len(parts) != 7 {
		t.Fatalf("跨项目镜像行应拆出 7 字段（六字段+to_project additive），实际 %q", lines[1])
	}
	want := []string{
		strconv.FormatInt(seq, 10), // seq=服务端分配
		createdAt,                  // created_at=服务端返回值权威（AC5.4）
		"normal",                   // level
		crossBProject + "/" + regAuthColumn + "/executor", // SP3：目标项目 code 前缀
		regAuthSession + "@" + regAuthColumn,              // sender_label（发送方=A 域身份）
		"跨项目指令",                                           // 单行正文原样
		crossBProject,                                     // to_project 第七字段（additive）
	}
	for i, w := range want {
		if parts[i] != w {
			t.Errorf("跨项目镜像字段 %d = %q，期望 %q", i, parts[i], w)
		}
	}

	// ④ 目标项目不存在 → 404 project_not_found 经 CLI 特判：rc=4+冻结文案
	//「目标项目不存在: <code>」（§4.2；服务端透传文案为「项目不存在: X」），且
	// 镜像零触碰（§8.3 时序）。
	root404 := t.TempDir()
	_, stderr = runWantCode(t, "目标项目不存在", 4, func() int {
		return runSend(ctx, sendArgs("--to-project", "proj-x",
			"--to-role", "executor", "--body", "x", "--mirror-root", root404), addr)
	})
	if !strings.Contains(stderr, "目标项目不存在: proj-x") {
		t.Errorf("404 stderr = %q，期望含冻结文案「目标项目不存在: proj-x」", stderr)
	}
	assertNoMirrorFile(t, sendMirrorPath(root404, regAuthProject, regAuthColumn))

	// ④′ 归因判别（b5-W3 质量审修）：project_not_found 双发射源——目标域（跨项目
	// 解析「项目不存在: <目标code>」）与身份域（心跳中间件「身份挂靠项目 <发送方
	// project> 不存在」，M-40 自诊断文案）。发送方自身项目拼错+带 --to-project 时
	// 不得误报「目标项目不存在」（目标项目实存），rc=4 透传身份域文案（AC9.2 指
	// 明真实缺失层级）。
	_, stderr = runWantCode(t, "身份域坏项目+--to-project", 4, func() int {
		return runSend(ctx, sendArgs("--project", "ghost-a",
			"--to-project", crossBProject, "--to-role", "executor",
			"--body", "x", "--mirror-root", t.TempDir()), addr)
	})
	if !strings.Contains(stderr, "身份挂靠项目 ghost-a 不存在") {
		t.Errorf("身份域坏项目 stderr = %q，期望透传「身份挂靠项目 ghost-a 不存在」", stderr)
	}
	if strings.Contains(stderr, "目标项目不存在") {
		t.Errorf("身份域坏项目 stderr = %q，不应误报「目标项目不存在」（%s 实存，归因判别）", stderr, crossBProject)
	}

	// ② --to-project+--bus 同给 → 本地退 2 零网络请求（serverOverride 指死端口，
	// 误发请求将得退 3，实测 2 即证明本地拦截）。
	dead := closedServerAddr(t)
	_, stderr = runWantCode(t, "--to-project+--bus", 2, func() int {
		return runSend(ctx, sendArgs("--to-project", crossBProject,
			"--bus", "--body", "x"), dead)
	})
	if !strings.Contains(stderr, "--to-project") || !strings.Contains(stderr, "--bus") {
		t.Errorf("--to-project+--bus stderr = %q，期望指明两 flag 组合冲突", stderr)
	}

	// ③ --to-project 单给无 --to-role/--to-session → 既有「三选一缺选」退 2
	// 天然覆盖（错误指明三选一 flag）。
	_, stderr = runWantCode(t, "--to-project 单给", 2, func() int {
		return runSend(ctx, sendArgs("--to-project", crossBProject, "--body", "x"), dead)
	})
	if !strings.Contains(stderr, "--to-role") {
		t.Errorf("--to-project 单给 stderr = %q，期望三选一缺选文案", stderr)
	}

	// 项目内回归（AC9.4+SP2）：--to-project 纯空白 → 视同缺省不发跨项目键——
	// 消息落 A 域（项目内 direct）、镜像行仍六字段（additive 零破坏）。
	rootIn := t.TempDir()
	stdout, _ = runWantCode(t, "空白串视同缺省", 0, func() int {
		return runSend(ctx, sendArgs("--to-project", "  ",
			"--to-role", "executor", "--body", "项目内正文", "--mirror-root", rootIn), addr)
	})
	seqIn, createdIn := parseOKLine(t, stdout)
	if aMsgs := historyProbe(t, addr, regAuthSession, regAuthRole, "direct"); len(aMsgs) != 1 || aMsgs[0].Body != "项目内正文" {
		t.Errorf("空白串 toProject 项目内落库 = %+v，期望 A 域恰 1 条 body=项目内正文", aMsgs)
	}
	linesIn := readMirrorLines(t, sendMirrorPath(rootIn, regAuthProject, regAuthColumn))
	if len(linesIn) != 2 {
		t.Fatalf("项目内镜像应头注释+1 条消息共 2 行，实际 %d 行: %q", len(linesIn), linesIn)
	}
	partsIn := strings.Split(linesIn[1], " | ")
	if len(partsIn) != 6 {
		t.Fatalf("项目内镜像行应仍 6 字段（to_project 不追加，既有行格式零破坏），实际 %q", linesIn[1])
	}
	wantIn := []string{
		strconv.FormatInt(seqIn, 10),
		createdIn,
		"normal",
		regAuthProject + "/" + regAuthColumn + "/executor", // 项目内 direct 摘要（A 域前缀）
		regAuthSession + "@" + regAuthColumn,
		"项目内正文",
	}
	for i, w := range wantIn {
		if partsIn[i] != w {
			t.Errorf("项目内镜像字段 %d = %q，期望 %q", i, partsIn[i], w)
		}
	}
}

// TestSendThreeForms §4.2 目标三选一端到端：--to-role/--bus/--to-session 三形态
// 各自成功落库（#13 kind 过滤断言）；目标缺或多选 → 本地退 2 零 HTTP 请求。
func TestSendThreeForms(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	// sendArgs 完整重建参数切片（regIdentityArgs 每次新建 + 追加），复用零污染。
	sendArgs := func(extra ...string) []string {
		return append(append(regIdentityArgs(), "--mirror-root", root), extra...)
	}

	// 目标会话预注册（零消息杂音）：以 executor-B 身份 GET #13——心跳中间件
	// upsert 会话而不写 messages，chat 目标即已注册。
	historyProbe(t, addr, "executor-B", "executor", "")

	runWantCode(t, "send direct", 0, func() int {
		return runSend(ctx, sendArgs("--to-role", "executor", "--body", "direct 正文"), addr)
	})
	runWantCode(t, "send bus", 0, func() int {
		return runSend(ctx, sendArgs("--bus", "--body", "bus 正文"), addr)
	})
	runWantCode(t, "send chat", 0, func() int {
		return runSend(ctx, sendArgs("--to-session", "executor-B", "--body", "chat 正文"), addr)
	})

	// 三形态各自落库（#13 按项目+kind 端到端断言，恰 1 条且 body 吻合）。
	directs := historyProbe(t, addr, regAuthSession, regAuthRole, "direct")
	if len(directs) != 1 || directs[0].Body != "direct 正文" {
		t.Errorf("direct 落库 = %+v，期望恰 1 条 body=direct 正文", directs)
	}
	buses := historyProbe(t, addr, regAuthSession, regAuthRole, "bus")
	if len(buses) != 1 || buses[0].Body != "bus 正文" {
		t.Errorf("bus 落库 = %+v，期望恰 1 条 body=bus 正文", buses)
	}
	chats := historyProbe(t, addr, regAuthSession, regAuthRole, "chat")
	if len(chats) != 1 || chats[0].Body != "chat 正文" {
		t.Errorf("chat 落库 = %+v，期望恰 1 条 body=chat 正文", chats)
	}

	// 目标缺/多选 → 本地退 2 零 HTTP 请求：serverOverride 指向死端口，若误发
	// 请求将得退出 3，实测 2 即证明本地拦截（register_test 同款手法）。
	dead := closedServerAddr(t)
	_, stderr := runWantCode(t, "目标缺选", 2, func() int {
		return runSend(ctx, sendArgs("--body", "x"), dead)
	})
	if !strings.Contains(stderr, "--to-role") {
		t.Errorf("目标缺选 stderr = %q，期望指明三选一 flag", stderr)
	}
	for name, extra := range map[string][]string{
		"双选 role+bus":     {"--to-role", "executor", "--bus", "--body", "x"},
		"双选 bus+session":  {"--bus", "--to-session", "executor-B", "--body", "x"},
		"双选 role+session": {"--to-role", "executor", "--to-session", "executor-B", "--body", "x"},
		"三全选":             {"--to-role", "executor", "--bus", "--to-session", "executor-B", "--body", "x"},
	} {
		_, stderr := runWantCode(t, "目标多选 "+name, 2, func() int {
			return runSend(ctx, sendArgs(extra...), dead)
		})
		if !strings.Contains(stderr, "互斥") {
			t.Errorf("目标多选 %s stderr = %q，期望指明互斥", name, stderr)
		}
	}
}

// TestSendLocalValidation §4.1 本地校验两条判定的锁定用例：--level 白名单与
// TrimSpace 空白 body 均在零 HTTP 前提下退 2。死端口手法（closedServerAddr，
// TestSendThreeForms 同款）：若误发请求将得退出 3（不可达），实测 2 即证明
// 本地拦截、零网络往返。
func TestSendLocalValidation(t *testing.T) {
	dead := closedServerAddr(t)
	root := t.TempDir()
	ctx := context.Background()

	// --level 非法值 → 退 2，错误指明 --level 与合法枚举。
	_, stderr := runWantCode(t, "level 非法值", 2, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--level", "urgent",
			"--body", "x", "--mirror-root", root), dead)
	})
	if !strings.Contains(stderr, "--level") {
		t.Errorf("level 非法 stderr = %q，期望指明 --level", stderr)
	}

	// --body 纯空白 → 退 2（TrimSpace 空判定，与服务端 body_empty 口径一致），
	// 错误指明正文为空。
	_, stderr = runWantCode(t, "body 纯空白", 2, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--body", "   ",
			"--mirror-root", root), dead)
	})
	if !strings.Contains(stderr, "正文为空") {
		t.Errorf("空白 body stderr = %q，期望指明正文为空", stderr)
	}
}

// TestSendStdin §4.1 正文输入：--body - 显式读 stdin、无 --body 且 stdin 非终端
// 自动读取（多行正文经镜像单行化落行）、stdin 为终端且无 --body → 退 2。
func TestSendStdin(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()

	// 分支一：--body - 显式从 stdin 读（多行+制表符原样进入镜像单行化）。
	root1 := t.TempDir()
	injectStdin(t, "第一行\n第二行\t尾")
	stdout, _ := runWantCode(t, "send --body -", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--body", "-", "--mirror-root", root1), addr)
	})
	seq1, _ := parseOKLine(t, stdout)
	lines1 := readMirrorLines(t, sendMirrorPath(root1, regAuthProject, regAuthColumn))
	if len(lines1) != 2 {
		t.Fatalf("--body - 镜像应 2 行，实际 %d: %q", len(lines1), lines1)
	}
	// 单行化字面期望：换行→字面 \n、制表符→字面 \t（§8.2，一行=一条消息）。
	want1 := ` | 第一行\n第二行\t尾`
	if !strings.HasSuffix(lines1[1], want1) {
		t.Errorf("--body - 镜像行 = %q，期望以 %q 结尾（body 单行化）", lines1[1], want1)
	}

	// 分支二：无 --body 且 stdin 为管道 → 自动读取。
	root2 := t.TempDir()
	injectStdin(t, "管道自动正文")
	stdout2, _ := runWantCode(t, "send 管道自动读", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--mirror-root", root2), addr)
	})
	seq2, _ := parseOKLine(t, stdout2)
	if seq2 <= seq1 {
		t.Errorf("第二次 send seq=%d 应大于首次 %d（全局自增 AC5.1）", seq2, seq1)
	}
	lines2 := readMirrorLines(t, sendMirrorPath(root2, regAuthProject, regAuthColumn))
	if len(lines2) != 2 || !strings.HasSuffix(lines2[1], " | 管道自动正文") {
		t.Errorf("管道自动读镜像 = %q，期望含正文", lines2)
	}

	// 分支三：无 --body 且 stdin 为字符设备（DevNull 模拟终端语义）→ 本地退 2。
	// serverOverride 指死端口：若误发请求将得退 3，实测 2 即证明本地拦截；DevNull
	// 的 Stat 若不带 CharDevice 位（平台差异）则落「空读→正文为空」分支，同样退 2
	// 且 stderr 同样指明 --body——两实现路径断言等价。
	dead := closedServerAddr(t)
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", os.DevNull, err)
	}
	orig := os.Stdin
	os.Stdin = devNull
	// 恢复走 t.Cleanup（与 injectStdin 同款骨架），不再手写收尾。
	t.Cleanup(func() {
		os.Stdin = orig
		_ = devNull.Close()
	})
	_, stderr := runWantCode(t, "send 无 body 终端", 2, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--mirror-root", t.TempDir()), dead)
	})
	if !strings.Contains(stderr, "--body") {
		t.Errorf("无 body 终端 stderr = %q，期望指明 --body", stderr)
	}
}

// TestSendMirrorFail AC13.1 反向（§8.3 第 4 步）：镜像写失败 → 退 5 + stderr 含
// 「消息已入库 seq=...」语义与可粘贴补行；消息确已入库（#13 可查）；stdout 无
// 成功行（镜像失败绝不假成功）。
func TestSendMirrorFail(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()
	// 跨平台稳定失败注入（mirror 包 TestWriteFail 同款实测结论）：--mirror-root
	// 指向普通文件——Windows 的 chmod 只读不拦目录创建，只读目录注入不可靠。
	badRoot := filepath.Join(t.TempDir(), "ordinary-file")
	if err := os.WriteFile(badRoot, []byte("i am a file"), 0o644); err != nil {
		t.Fatalf("预置普通文件失败: %v", err)
	}

	body := "镜像要失败的正文"
	stdout, stderr := runWantCode(t, "镜像写失败", 5, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--body", body, "--mirror-root", badRoot), addr)
	})
	if stdout != "" {
		t.Errorf("镜像失败 stdout = %q，期望空（AC13.1 反向：镜像失败绝不退 0/无成功行）", stdout)
	}
	if !strings.Contains(stderr, "消息已入库 seq=") {
		t.Errorf("镜像失败 stderr = %q，期望含「消息已入库 seq=」（§8.3）", stderr)
	}
	// 可粘贴补行完整性：完整行含 target/sender/body 要素（seq 服务端分配，动态比对）。
	for _, frag := range []string{
		"手工补行: ",
		regAuthProject + "/" + regAuthColumn + "/executor",
		regAuthSession + "@" + regAuthColumn,
		body,
	} {
		if !strings.Contains(stderr, frag) {
			t.Errorf("镜像失败 stderr = %q，期望含可粘贴要素 %q", stderr, frag)
		}
	}
	// 退 5 语义前提：消息确实已入库（若未入库则「消息已入库」文案即谎言）。
	msgs := historyProbe(t, addr, regAuthSession, regAuthRole, "")
	found := false
	for _, m := range msgs {
		if m.Body == body {
			found = true
		}
	}
	if !found {
		t.Errorf("退 5 前提破坏：镜像失败但消息未入库（%+v）", msgs)
	}
}

// TestServerFailNoMirror §8.3 先服务端后镜像 + AC5.3：服务端 4xx（身份栏目未
// 登记 / chat 未注册会话）→ 退 4 且镜像零触碰；服务停机 → 退 3 + stderr。
func TestServerFailNoMirror(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()

	// 4xx 分支 A：身份栏目未登记 → 心跳中间件 404 column_not_found → 退 4。
	_, stderr := runWantCode(t, "未登记栏目", 4, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--column", "no-such-col", "--to-role", "executor",
			"--body", "x", "--mirror-root", root), addr)
	})
	if !strings.Contains(stderr, "column_not_found") {
		t.Errorf("未登记栏目 stderr = %q，期望含 column_not_found（错误码透传）", stderr)
	}
	assertNoMirrorFile(t, sendMirrorPath(root, regAuthProject, "no-such-col"))

	// 4xx 分支 B：chat 目标会话未注册 → 404 target_session_not_found → 退 4。
	_, stderr = runWantCode(t, "chat 未注册会话", 4, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-session", "ghost-session",
			"--body", "x", "--mirror-root", root), addr)
	})
	if !strings.Contains(stderr, "target_session_not_found") {
		t.Errorf("chat 未注册 stderr = %q，期望含 target_session_not_found", stderr)
	}

	// 服务停机 → 退 3 + stderr（§4.1 不可达语义）。
	dead := closedServerAddr(t)
	stdout, stderr := runWantCode(t, "服务停机", 3, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--body", "x", "--mirror-root", root), dead)
	})
	if !strings.Contains(stderr, "服务不可达") {
		t.Errorf("服务停机 stderr = %q，期望含服务不可达", stderr)
	}
	if stdout != "" {
		t.Errorf("服务停机 stdout = %q，期望空", stdout)
	}

	// §8.3 时序总断言：以上三分支后发送方镜像文件与 .aiteam 目录零触碰。
	assertNoMirrorFile(t, sendMirrorPath(root, regAuthProject, regAuthColumn))
}

// TestSendAgentConflictPassthrough b2-T2/R4 AC4④：服务端 409 agent_conflict 清障
// 文案经 CLI 零改动直透（透传层零改动=设计约束，本测试是锚定而非实现）——CLI
// fail→APIError.Error 逐字含服务端 message。双活格夹具=historyProbe 四头 GET 经
// 心跳中间件 upsert 两 executor 会话（零杂音预注册），再以 controller 身份
// --to-role executor 定向 → 退 4，stderr 含逐会话「最后活跃/后过期」段与旧窗
// 哨兵机制提示句；镜像零触碰（§8.3）。真实时钟下 elapsed 秒级抖动，断言仅锚
// 文案骨架不锚具体秒数（时间敏感面的 CLI 侧让位：精确值断言在 server 侧
// injectFixedClock 用例 TestSendDirectAgentConflictClearingHint）。
func TestSendAgentConflictPassthrough(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	for _, sess := range []string{"executor-P", "executor-Q"} {
		historyProbe(t, addr, sess, "executor", "")
	}

	_, stderr := runWantCode(t, "双活 409 透传", 4, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--body", "x", "--mirror-root", root), addr)
	})
	for _, want := range []string{
		"agent_conflict",
		"executor-P 最后活跃 ",
		"executor-Q 最后活跃 ",
		" 前、预计 ",
		" 后过期",
		"活=失联阈值内（项目级可调）",
		"被遗忘的旧窗哨兵仍在跑会持续续心跳不过期——关旧窗即自然过期",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q，不含 %q（R4 服务端清障文案 CLI 直透）", stderr, want)
		}
	}
	assertNoMirrorFile(t, sendMirrorPath(root, regAuthProject, regAuthColumn))
}

// ─────────────────────────────────────────────────────────────────────────────
// b2-T4 R3 bus 误用提示（b2-spec §二.R3/AC3，M-34①）
// ─────────────────────────────────────────────────────────────────────────────

// busMisuseHint R3 提示行逐字模板（b2-spec §二.R3 原文锚定，含全角标点）。
const busMisuseHint = "提示：bus 广播仅投递 controller 角色格信箱，执行者格收不到；发给执行者请用 --to-role executor_<标识>（direct 定向）"

// TestSendBusMisuseHint AC3 三形态 + 前缀口径边界：①executor 角色发 bus——发送
// 照常成功（警示不拒绝，M-11 同哲学）+stderr 含 spec 原文提示行（executor 与
// executor_<标识> 两 role 规约形态均命中——判定=非 controller 前缀）；②controller
// 角色发 bus——零提示；③--json stdout 零污染（合法 JSON 且不含提示文本）+stderr
// 仍含提示（提示面向人不因 --json 消失）。边界：非 bus（direct）任何角色零提示。
func TestSendBusMisuseHint(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()

	// ① executor_A（带后缀形态）发 bus：rc=0（bus 发送本身合法，服务端无角色
	// 闸）+ stderr 恰一行 spec 原文提示（成功路径 stderr 本零输出，提示行=唯一
	// stderr 内容——spec「追加一行」）。
	stdout, stderr := runWantCode(t, "executor_A 发 bus", 0, func() int {
		return runSend(ctx, append(identityArgsAs("executor-A", "executor_A"),
			"--bus", "--body", "执行者误用 bus", "--mirror-root", root), addr)
	})
	if seq, _ := parseOKLine(t, stdout); seq <= 0 {
		t.Errorf("executor_A 发 bus 应照常成功（警示不拒绝），stdout = %q", stdout)
	}
	if !strings.Contains(stderr, busMisuseHint) {
		t.Errorf("executor_A 发 bus stderr = %q，期望含 spec 原文提示行（AC3①）", stderr)
	}
	if stderr != busMisuseHint+"\n" {
		t.Errorf("executor_A 发 bus stderr = %q，期望恰一行提示无杂音", stderr)
	}

	// ①′ executor（无后缀形态）同样命中（前缀口径：不以 controller 起头即提示）。
	_, stderr = runWantCode(t, "executor 发 bus", 0, func() int {
		return runSend(ctx, append(identityArgsAs("executor-B", "executor"),
			"--bus", "--body", "无后缀执行者 bus", "--mirror-root", root), addr)
	})
	if !strings.Contains(stderr, busMisuseHint) {
		t.Errorf("executor 发 bus stderr = %q，期望含提示行（role 规约三形态之二）", stderr)
	}

	// ② controller 角色发 bus：零提示（bus 合法受众，M-34① 只拦「执行者格收
	// 不到」的误用面）。
	_, stderr = runWantCode(t, "controller 发 bus", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--bus", "--body", "总控的合法 bus", "--mirror-root", root), addr)
	})
	if strings.Contains(stderr, "提示：bus 广播") {
		t.Errorf("controller 发 bus stderr = %q，不应含提示（AC3②）", stderr)
	}

	// ③ --json：stdout 零污染（合法 JSON 且不含提示文本——机器可读面）+stderr
	// 仍含提示（stderr 通道，人不因 --json 失去警示）。
	stdout, stderr = runWantCode(t, "executor_A --json 发 bus", 0, func() int {
		return runSend(ctx, append(identityArgsAs("executor-C", "executor_A"),
			"--bus", "--body", "json 形态执行者 bus", "--json", "--mirror-root", root), addr)
	})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &raw); err != nil {
		t.Fatalf("--json stdout 非合法 JSON: %v\n%s", err, stdout)
	}
	if strings.Contains(stdout, busMisuseHint) {
		t.Errorf("--json stdout 含提示文本（机器可读面零污染，AC3③）: %q", stdout)
	}
	if !strings.Contains(stderr, busMisuseHint) {
		t.Errorf("--json stderr = %q，期望仍含提示行（提示不因 --json 消失）", stderr)
	}

	// 边界：非 bus（direct）任何角色零提示——executor_A 定向 controller 成功且
	// stderr 空（提示只锚 --bus 误用面，direct 本就是提示推荐的正确通道）。
	_, stderr = runWantCode(t, "executor_A direct", 0, func() int {
		return runSend(ctx, append(identityArgsAs("executor-A", "executor_A"),
			"--to-role", "controller", "--body", "执行者定向", "--mirror-root", root), addr)
	})
	if stderr != "" {
		t.Errorf("executor_A direct stderr = %q，期望空（非 bus 零提示）", stderr)
	}
}

// TestSendBusHintNotOnLocalFail S1 边界（b2-T4 质量审建议采纳）：buildClient
// 失败（仓库级 cli.json 损坏，同为退 2 用法类错误）路径 stderr 零提示——提示
// 位置须在 buildClient 之后，全部本地退 2 路径严格不掺提示行（错误输出单一
// 焦点）。夹具：chdirForTest 切入临时目录放损坏 .aiteam/cli.json（仓库级查找
// 链以进程 CWD 为基准，cli_test.go 注：包 CWD 下无该文件不设防，故 chdir 注入）
// +clearLookupChainEnv 清 env 两级（防机器级 AITEAM_SERVER 污染）+serverOverride
// 空串走完整五级查找链（非 httptest 注入直胜出）。
func TestSendBusHintNotOnLocalFail(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".aiteam"), 0o755); err != nil {
		t.Fatalf("预置 .aiteam 目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiteam", "cli.json"), []byte("not-json"), 0o644); err != nil {
		t.Fatalf("预置损坏 cli.json 失败: %v", err)
	}
	chdirForTest(t, dir)
	clearLookupChainEnv(t)

	// executor_A+--bus 触发提示条件，但 buildClient 在 cli.json 级 fail-fast
	// 退 2（ResolveServer 报「解析配置 … 失败」）——此路径 stderr 不得含提示行。
	_, stderr := runWantCode(t, "bus+坏 cli.json", 2, func() int {
		return runSend(context.Background(), append(identityArgsAs("executor-D", "executor_A"),
			"--bus", "--body", "x", "--mirror-root", t.TempDir()), "")
	})
	if !strings.Contains(stderr, "解析配置") {
		t.Errorf("坏 cli.json stderr = %q，期望含「解析配置」失败文案（fail-fast）", stderr)
	}
	if strings.Contains(stderr, "提示：bus 广播") {
		t.Errorf("坏 cli.json 退 2 路径 stderr = %q，不应含 bus 误用提示（S1：本地退 2 路径零提示）", stderr)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// b4-T1 R1 目标解析预览（b4-spec §二.R1/AC1：--dry-run+发送输出增强）
// ─────────────────────────────────────────────────────────────────────────────

// TestSendDryRunPreview AC1①+③(dry-run 半面)：--dry-run 解析目标输出全貌后
// 不发送。四形态逐一断言恰一行摘要（b4-spec §二.R1 措辞落位，四要素齐全：
// 项目/栏目/目标格/会话+kind/level）；零落库=真服务夹具下 dry-run 后 #13 全量
// 零条+镜像零触碰；零网络=死端口下照常 rc=0（若 dry-run 误发请求将得退 3，
// 实测 0 即证明 buildClient/POST 未达）。解析面=参数可确定信息：chat 的栏目/
// 角色格、目标域存在性属服务端面——dry-run 不做存在性往返（executor-B 会话
// 未注册、proj-b 项目未 seed 均照常解析，零网络语义边界；存在性防线由实发
// 的服务端校验兜底）。
func TestSendDryRunPreview(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()
	dead := closedServerAddr(t)
	root := t.TempDir() // dry-run 恒零落库：镜像根仅参数面占位
	dryArgs := func(extra ...string) []string {
		args := append(regIdentityArgs(), "--body", "x", "--dry-run")
		args = append(args, extra...)
		return append(args, "--mirror-root", root)
	}

	// direct：恰一行摘要+stderr 零输出+messages 零增长+镜像零触碰。
	stdout, stderr := runWantCode(t, "dry-run direct", 0, func() int {
		return runSend(ctx, dryArgs("--to-role", "executor", "--level", "block"), addr)
	})
	wantDirect := "目标解析：项目 " + regAuthProject + " 栏目 " + regAuthColumn + " → 角色格 executor｜direct + level block"
	if stdout != wantDirect+"\n" {
		t.Errorf("dry-run direct stdout = %q，期望恰一行 %q", stdout, wantDirect)
	}
	if stderr != "" {
		t.Errorf("dry-run direct stderr = %q，期望空", stderr)
	}
	if msgs := historyProbe(t, addr, regAuthSession, regAuthRole, ""); len(msgs) != 0 {
		t.Errorf("dry-run 后 messages = %+v，期望 0 条（零落库 AC1①）", msgs)
	}
	assertNoMirrorFile(t, sendMirrorPath(root, regAuthProject, regAuthColumn))

	// chat（死端口）：栏目/角色格=目标会话所属（服务端解析面），摘要如实标注；
	// 会话无需预注册。
	stdout, stderr = runWantCode(t, "dry-run chat", 0, func() int {
		return runSend(ctx, dryArgs("--to-session", "executor-B"), dead)
	})
	wantChat := "目标解析：项目 " + regAuthProject + " 栏目 会话所属（服务端解析） → 会话 executor-B｜chat + level normal"
	if stdout != wantChat+"\n" {
		t.Errorf("dry-run chat stdout = %q，期望恰一行 %q", stdout, wantChat)
	}
	if stderr != "" {
		t.Errorf("dry-run chat stderr = %q，期望空", stderr)
	}

	// bus（controller 身份，死端口）：项目级广播语义——全栏目→controller 格；
	// controller 合法受众零 bus 误用提示（b2-R3 判定面在 dry-run 同样生效）。
	stdout, stderr = runWantCode(t, "dry-run bus", 0, func() int {
		return runSend(ctx, dryArgs("--bus"), dead)
	})
	wantBus := "目标解析：项目 " + regAuthProject + " 栏目 全栏目（项目级广播） → 角色格 controller｜bus + level normal"
	if stdout != wantBus+"\n" {
		t.Errorf("dry-run bus stdout = %q，期望恰一行 %q", stdout, wantBus)
	}
	if stderr != "" {
		t.Errorf("dry-run bus stderr = %q，期望空（controller 合法受众零提示）", stderr)
	}

	// 跨项目 direct（死端口，proj-b 未 seed）：项目段切换为 --to-project 值。
	stdout, _ = runWantCode(t, "dry-run 跨项目", 0, func() int {
		return runSend(ctx, dryArgs("--to-project", "proj-b", "--to-role", "executor"), dead)
	})
	wantCross := "目标解析：项目 proj-b 栏目 " + regAuthColumn + " → 角色格 executor｜direct + level normal"
	if stdout != wantCross+"\n" {
		t.Errorf("dry-run 跨项目 stdout = %q，期望恰一行 %q", stdout, wantCross)
	}

	// --json（AC1③ dry-run 半面）：输出 resolved_target 对象——direct 键集恰
	// 五键（session omitempty 缺省，additive 键缺省口径同 pollAckData.acked_to）；
	// bus 键集恰四键（column/session 缺省+role=controller 唯一投递格）。
	stdout, _ = runWantCode(t, "dry-run --json", 0, func() int {
		return runSend(ctx, dryArgs("--to-role", "executor", "--level", "important", "--json"), dead)
	})
	assertDryRunJSON(t, stdout, map[string]string{
		"kind": "direct", "project": regAuthProject, "column": regAuthColumn,
		"role": "executor", "level": "important",
	})

	stdout, _ = runWantCode(t, "dry-run --json bus", 0, func() int {
		return runSend(ctx, dryArgs("--bus", "--json"), dead)
	})
	assertDryRunJSON(t, stdout, map[string]string{
		"kind": "bus", "project": regAuthProject, "role": "controller", "level": "normal",
	})
}

// assertDryRunJSON dry-run --json 断言：合法 JSON+键集恰=want（值逐键相等；
// 不在 want 的键=omitempty 缺省，键数不等即报）。
func assertDryRunJSON(t *testing.T, stdout string, want map[string]string) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &raw); err != nil {
		t.Fatalf("dry-run --json 非合法 JSON: %v\n%s", err, stdout)
	}
	if len(raw) != len(want) {
		t.Errorf("dry-run --json 键数 = %d（键 %v），期望恰 %d 键（键集 %v）", len(raw), raw, len(want), want)
	}
	for k, w := range want {
		v, ok := raw[k]
		if !ok {
			t.Errorf("dry-run --json 缺键 %q（实际键 %v）", k, raw)
			continue
		}
		if got := strings.Trim(string(v), `"`); got != w {
			t.Errorf("dry-run --json[%q] = %q，期望 %q", k, got, w)
		}
	}
}

// TestSendDryRunMatchesSend AC1④ 解析一致：同参数先 --dry-run（死端口=零网络）
// 后实发（真服务）——文本面 dry-run 输出与实发摘要行逐字节一致；--json 面
// dry-run 顶层对象与实发 resolved_target 子对象逐字节一致（同一结构序列化，
// 分叉即解析面分叉）。取 chat 形态（项目/会话/level 全要素在场，最能暴露
// 解析分叉）。
func TestSendDryRunMatchesSend(t *testing.T) {
	addr := startRegTestServer(t)
	dead := closedServerAddr(t)
	root := t.TempDir()
	ctx := context.Background()
	historyProbe(t, addr, "executor-B", "executor", "") // 预注册 chat 目标会话（实发半边需要）

	args := func(extra ...string) []string {
		args := append(regIdentityArgs(),
			"--to-session", "executor-B", "--level", "important", "--body", "一致性正文",
			"--mirror-root", root)
		return append(args, extra...)
	}
	dryOut, _ := runWantCode(t, "dry-run 先行", 0, func() int {
		return runSend(ctx, args("--dry-run"), dead)
	})
	sendOut, _ := runWantCode(t, "实发随后", 0, func() int {
		return runSend(ctx, args(), addr)
	})

	// 文本面：实发=OK 行+摘要行两行，OK 行可解析（首行）且摘要行与 dry-run
	// 输出逐字节一致（AC1④ 最强形态：同一渲染函数产出）。
	seq, ts := parseOKLine(t, sendOut)
	if seq <= 0 || ts == "" {
		t.Fatalf("实发 OK 行非法: seq=%d time=%q", seq, ts)
	}
	lines := strings.Split(strings.TrimRight(sendOut, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("实发输出应 OK 行+摘要行共 2 行，实际 %q", sendOut)
	}
	if dryLine := strings.TrimRight(dryOut, "\n"); lines[1] != dryLine {
		t.Errorf("摘要分叉（AC1④）：实发 %q ≠ dry-run %q", lines[1], dryLine)
	}

	// --json 面：实发 resolved_target 子对象 == dry-run 顶层对象（逐字节）。
	dryJSON, _ := runWantCode(t, "dry-run --json", 0, func() int {
		return runSend(ctx, args("--dry-run", "--json"), dead)
	})
	sendJSON, _ := runWantCode(t, "实发 --json", 0, func() int {
		return runSend(ctx, args("--json"), addr)
	})
	var sendRaw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(sendJSON)), &sendRaw); err != nil {
		t.Fatalf("实发 --json 非合法 JSON: %v\n%s", err, sendJSON)
	}
	rt, ok := sendRaw["resolved_target"]
	if !ok {
		t.Fatalf("实发 --json 缺 resolved_target 键（AC1③）: %s", sendJSON)
	}
	if string(rt) != strings.TrimSpace(dryJSON) {
		t.Errorf("resolved_target 分叉（AC1④）：实发 %s ≠ dry-run %s", rt, strings.TrimSpace(dryJSON))
	}
}

// TestSendOutputSummary AC1② 默认发送输出增强：`OK seq=N time=...` 行仍在
// （首行）+追加目标摘要行（同 dry-run 解析面）；stderr 零输出不变；镜像行
// 不受摘要段影响（§8.2 行格式冻结面在镜像文件——stdout 增强不外溢）。
func TestSendOutputSummary(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()

	stdout, stderr := runWantCode(t, "默认 send 输出增强", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--level", "block", "--body", "摘要段正文",
			"--mirror-root", root), addr)
	})
	if stderr != "" {
		t.Errorf("send 成功 stderr = %q，期望空（摘要段进 stdout）", stderr)
	}
	seq, ts := parseOKLine(t, stdout)
	if seq <= 0 || ts == "" {
		t.Fatalf("OK 行 seq/time 非法: seq=%d time=%q", seq, ts)
	}
	want := "目标解析：项目 " + regAuthProject + " 栏目 " + regAuthColumn + " → 角色格 executor｜direct + level block"
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 2 || lines[1] != want {
		t.Errorf("send 输出 = %q，期望 OK 行+摘要行（第二行 %q）", stdout, want)
	}
	if got := readMirrorLines(t, sendMirrorPath(root, regAuthProject, regAuthColumn)); len(got) != 2 {
		t.Errorf("镜像 = %q，期望头注释+1 消息行共 2 行（摘要段不进镜像）", got)
	}
}

// TestSendJSONResolvedTarget AC1③（实发半面）：--json 恰五键（#9 四键+
// resolved_target additive）+resolved_target 子对象按形态 omitempty——chat
// 缺 column/role（服务端解析面）、bus 缺 column/session 且 role=controller
// （唯一投递格）；值面与目标参数逐项吻合。
func TestSendJSONResolvedTarget(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	historyProbe(t, addr, "executor-B", "executor", "")

	chatOut, _ := runWantCode(t, "chat --json", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-session", "executor-B", "--level", "important", "--body", "x",
			"--json", "--mirror-root", root), addr)
	})
	assertResolvedTarget(t, chatOut, map[string]string{
		"kind": "chat", "project": regAuthProject, "session": "executor-B", "level": "important",
	})

	busOut, _ := runWantCode(t, "bus --json", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--bus", "--body", "x", "--json", "--mirror-root", root), addr)
	})
	assertResolvedTarget(t, busOut, map[string]string{
		"kind": "bus", "project": regAuthProject, "role": "controller", "level": "normal",
	})
}

// assertResolvedTarget 实发 --json 断言：顶层恰五键（#9 四键+resolved_target）
// +resolved_target 子对象键集恰=want（值逐键相等，omitempty 缺省键不在键集）。
func assertResolvedTarget(t *testing.T, stdout string, want map[string]string) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &raw); err != nil {
		t.Fatalf("send --json 非合法 JSON: %v\n%s", err, stdout)
	}
	if len(raw) != 5 {
		t.Errorf("send --json 键数 = %d（键 %v），期望恰 5 键（#9 四键+resolved_target）", len(raw), raw)
	}
	rtRaw, ok := raw["resolved_target"]
	if !ok {
		t.Fatalf("send --json 缺 resolved_target 键: %s", stdout)
	}
	var rt map[string]string
	if err := json.Unmarshal(rtRaw, &rt); err != nil {
		t.Fatalf("resolved_target 非合法 JSON 对象: %v（%s）", err, rtRaw)
	}
	if len(rt) != len(want) {
		t.Errorf("resolved_target 键集 = %v，期望恰 %v（omitempty 缺省键不在场）", rt, want)
	}
	for k, w := range want {
		if rt[k] != w {
			t.Errorf("resolved_target[%q] = %q，期望 %q", k, rt[k], w)
		}
	}
}

// TestSendDryRunBusHint 裁量留痕（b4 开工令）：dry-run 与实发共用同一 bus 误用
// 判定——非 controller 发 --bus 的 dry-run 同样 stderr 输出提示行（预览如实：
// 「这条 bus 执行者格收不到」在预览面就该看到），stdout 摘要照常（警示不拦截
// 哲学在预览面同构）。位置裁量：dry-run 零网络不经实发面的 buildClient 后置
// 提示位，判定在 dry-run 出口本地重放；dry-run 无本地退 2 出口（rc=0 成功
// 输出），不涉 b2-T4/S1「错误输出掺入提示」面——实发面提示位零变化
// （TestSendBusHintNotOnLocalFail 锚定不破）。
func TestSendDryRunBusHint(t *testing.T) {
	dead := closedServerAddr(t)
	ctx := context.Background()

	stdout, stderr := runWantCode(t, "executor_A dry-run bus", 0, func() int {
		return runSend(ctx, append(identityArgsAs("executor-E", "executor_A"),
			"--bus", "--body", "x", "--dry-run", "--mirror-root", t.TempDir()), dead)
	})
	if !strings.Contains(stderr, busMisuseHint) {
		t.Errorf("dry-run bus stderr = %q，期望含提示行（裁量：dry-run 提示与实发一致）", stderr)
	}
	if stderr != busMisuseHint+"\n" {
		t.Errorf("dry-run bus stderr = %q，期望恰一行提示无杂音", stderr)
	}
	wantBus := "目标解析：项目 " + regAuthProject + " 栏目 全栏目（项目级广播） → 角色格 controller｜bus + level normal"
	if stdout != wantBus+"\n" {
		t.Errorf("dry-run bus stdout = %q，期望恰一行 %q", stdout, wantBus)
	}

	// controller 半边零提示（判定面同实发：controller 前缀不命中）。
	_, stderr = runWantCode(t, "controller dry-run bus", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--bus", "--body", "x", "--dry-run", "--mirror-root", t.TempDir()), dead)
	})
	if strings.Contains(stderr, "提示：bus 广播") {
		t.Errorf("controller dry-run bus stderr = %q，不应含提示", stderr)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// b4-T2 R2 --to-session 歧义告警（b4-spec §二.R2/AC2：chat 形态下划线名
// stderr 告警一行，不拦截）
// ─────────────────────────────────────────────────────────────────────────────

// sessionAmbiguityHint R2 告警行逐字模板（b4-spec §二.R2 原文锚定，含全角
// 标点与破折号）。
const sessionAmbiguityHint = "提示：--to-session 值含下划线，形似角色名（role=executor_<标识>）；会话名惯用连字符——若本意是定向角色格请用 --to-role"

// TestSendSessionAmbiguityHint AC2 四形态+判据边界+dry-run 预览保真：①executor_A
// 形值触发 stderr 恰一行 spec 原文告警 ②executor-A 连字符惯用形零告警 ③告警
// 不拦截（发送照常成功 rc=0+seq>0，警示哲学同 M-11/b2-R3）④--json stdout 零
// 污染（合法 JSON 且不含告警文本）+stderr 仍含告警。判据边界（^[a-z]+_ 前缀
// 模式，spec 裁量①从简、接受低成本误报）：纯小写无下划线（executora）零告警、
// controller_A 形同样命中（模式覆盖性——controller_*/executor_* 全在告警面）、
// 大写形（Executor_A）不命中（判据忠实：^[a-z]+ 锚小写字母前缀）。dry-run 面
// 同构：歧义名 dry-run 同样告警（预览保真，与 T1 dry-run 面 bus hint 同哲学）
// ——边界用例走死端口零网络（dry-run 无存在性往返，无需预注册会话）。
func TestSendSessionAmbiguityHint(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	dead := closedServerAddr(t)
	// 预注册 chat 目标会话（实发面①~④需要——chat 目标会话须已注册 §2.2 #9；
	// executor_A 本身是合法会话名，注册后才有「发送照常成功」可断言）。
	historyProbe(t, addr, "executor_A", "executor_A", "")
	historyProbe(t, addr, "executor-A", "executor", "")

	// ① executor_A 形触发+③ 不拦截：rc=0+seq>0（chat 发送本身合法，告警只
	// 警示）+stderr 恰一行 spec 原文告警（成功路径 stderr 本零输出，告警行=
	// 唯一 stderr 内容——spec「告警一行」）。
	stdout, stderr := runWantCode(t, "chat executor_A", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-session", "executor_A", "--body", "歧义名正文", "--mirror-root", root), addr)
	})
	if seq, _ := parseOKLine(t, stdout); seq <= 0 {
		t.Errorf("executor_A chat 应照常成功（告警不拦截，AC2③），stdout = %q", stdout)
	}
	if !strings.Contains(stderr, sessionAmbiguityHint) {
		t.Errorf("executor_A chat stderr = %q，期望含 spec 原文告警行（AC2①）", stderr)
	}
	if stderr != sessionAmbiguityHint+"\n" {
		t.Errorf("executor_A chat stderr = %q，期望恰一行告警无杂音", stderr)
	}

	// ② executor-A 连字符惯用形：零告警（stderr 空）+照常成功。
	_, stderr = runWantCode(t, "chat executor-A", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-session", "executor-A", "--body", "连字符正文", "--mirror-root", root), addr)
	})
	if stderr != "" {
		t.Errorf("executor-A chat stderr = %q，期望空（连字符惯用形零告警，AC2②）", stderr)
	}

	// ④ --json：stdout 零污染（合法 JSON 且不含告警文本——机器可读面）+stderr
	// 仍含告警（告警面向人不因 --json 消失）。
	stdout, stderr = runWantCode(t, "chat executor_A --json", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-session", "executor_A", "--body", "json 形态歧义名", "--json",
			"--mirror-root", root), addr)
	})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &raw); err != nil {
		t.Fatalf("--json stdout 非合法 JSON: %v\n%s", err, stdout)
	}
	if strings.Contains(stdout, sessionAmbiguityHint) {
		t.Errorf("--json stdout 含告警文本（机器可读面零污染，AC2④）: %q", stdout)
	}
	if !strings.Contains(stderr, sessionAmbiguityHint) {
		t.Errorf("--json stderr = %q，期望仍含告警行（告警不因 --json 消失）", stderr)
	}

	// 边界①：纯小写无下划线（executora）零告警——模式要求下划线。
	_, stderr = runWantCode(t, "dry-run executora", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(), "--to-session", "executora",
			"--body", "x", "--dry-run", "--mirror-root", t.TempDir()), dead)
	})
	if strings.Contains(stderr, sessionAmbiguityHint) {
		t.Errorf("executora dry-run stderr = %q，不应含告警（无下划线不命中 ^[a-z]+_）", stderr)
	}

	// 边界②：controller_A 形同样命中（模式覆盖性——判据不偏袒 executor 族）。
	_, stderr = runWantCode(t, "dry-run controller_A", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(), "--to-session", "controller_A",
			"--body", "x", "--dry-run", "--mirror-root", t.TempDir()), dead)
	})
	if !strings.Contains(stderr, sessionAmbiguityHint) {
		t.Errorf("controller_A dry-run stderr = %q，期望含告警（controller_* 形同命中模式）", stderr)
	}

	// 边界③：大写形（Executor_A）不命中（判据忠实：^[a-z]+ 锚小写字母前缀，
	// 大写起头非本告警面）。
	_, stderr = runWantCode(t, "dry-run Executor_A", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(), "--to-session", "Executor_A",
			"--body", "x", "--dry-run", "--mirror-root", t.TempDir()), dead)
	})
	if strings.Contains(stderr, sessionAmbiguityHint) {
		t.Errorf("Executor_A dry-run stderr = %q，不应含告警（大写形不命中 ^[a-z]+_）", stderr)
	}

	// dry-run 面主断言（预览保真）：歧义名 dry-run 同样输出告警（stdout 摘要
	// 照常+stderr 恰一行告警——「这个会话名形似角色名」在预览面就该看到，
	// 与 T1 dry-run 面 bus hint 同哲学）。死端口=dry-run 零网络语义同时覆盖。
	stdout, stderr = runWantCode(t, "dry-run executor_A", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(), "--to-session", "executor_A",
			"--body", "x", "--dry-run", "--mirror-root", t.TempDir()), dead)
	})
	wantChat := "目标解析：项目 " + regAuthProject + " 栏目 会话所属（服务端解析） → 会话 executor_A｜chat + level normal"
	if stdout != wantChat+"\n" {
		t.Errorf("dry-run executor_A stdout = %q，期望恰一行 %q", stdout, wantChat)
	}
	if stderr != sessionAmbiguityHint+"\n" {
		t.Errorf("dry-run executor_A stderr = %q，期望恰一行告警（dry-run 告警与实发一致）", stderr)
	}
}

// TestNoMirror --no-mirror 与 --json：跳过双写不落文件；--json 键面=#9 四键
// +resolved_target（b4-T1 additive，恰五键）。
func TestNoMirror(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()

	// --no-mirror：成功退 0 不写镜像文件。
	stdout, _ := runWantCode(t, "no-mirror", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--body", "不落镜像",
			"--no-mirror", "--mirror-root", root), addr)
	})
	if seq, _ := parseOKLine(t, stdout); seq <= 0 {
		t.Errorf("no-mirror OK 行 = %q", stdout)
	}
	assertNoMirrorFile(t, sendMirrorPath(root, regAuthProject, regAuthColumn))

	// --json：恰五键（#9 四键+resolved_target additive，b4-T1 豁免面更新）+
	// 字段值正确；默认双写照常（--json 不影响镜像）。
	stdout, _ = runWantCode(t, "--json", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--level", "important", "--body", "json 输出",
			"--json", "--mirror-root", root), addr)
	})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &raw); err != nil {
		t.Fatalf("--json 输出非合法 JSON: %v\n%s", err, stdout)
	}
	if len(raw) != 5 {
		t.Errorf("--json 键数 = %d，期望恰 5 键（#9 四键+resolved_target，b4-T1 additive）（实际键 %v）", len(raw), raw)
	}
	for _, k := range []string{"seq", "created_at", "level", "kind", "resolved_target"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("--json 缺键 %q", k)
		}
	}
	var parsed struct {
		Seq       int64  `json:"seq"`
		CreatedAt string `json:"created_at"`
		Level     string `json:"level"`
		Kind      string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &parsed); err != nil {
		t.Fatalf("--json 解析失败: %v", err)
	}
	if parsed.Level != "important" || parsed.Kind != "direct" || parsed.Seq <= 0 || parsed.CreatedAt == "" {
		t.Errorf("--json 字段值 = %+v，期望 level=important kind=direct seq>0 created_at 非空", parsed)
	}
	if lines := readMirrorLines(t, sendMirrorPath(root, regAuthProject, regAuthColumn)); len(lines) != 2 {
		t.Errorf("--json 默认镜像应 2 行（双写照常），实际 %d: %q", len(lines), lines)
	}
}

// TestMirrorTampered AC13.4 机制面：镜像文件被外部删除/篡改后 send 照常成功
// （服务端判定不依赖镜像内容；删除后重建走首建语义，篡改后原样追加）。
func TestMirrorTampered(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	mirrorFile := sendMirrorPath(root, regAuthProject, regAuthColumn)
	sendOnce := func(body string) int64 {
		stdout, _ := runWantCode(t, "send "+body, 0, func() int {
			return runSend(ctx, append(regIdentityArgs(),
				"--to-role", "executor", "--body", body, "--mirror-root", root), addr)
		})
		seq, _ := parseOKLine(t, stdout)
		return seq
	}

	seq1 := sendOnce("篡改前一条")
	if lines := readMirrorLines(t, mirrorFile); len(lines) != 2 {
		t.Fatalf("首条后镜像应 2 行，实际 %d: %q", len(lines), lines)
	}

	// 外部删除：删镜像文件后 send 照常退 0，seq 照常自增（判定独立于镜像）。
	if err := os.Remove(mirrorFile); err != nil {
		t.Fatalf("删除镜像文件失败: %v", err)
	}
	seq2 := sendOnce("删除后一条")
	if seq2 <= seq1 {
		t.Errorf("删除镜像后 seq=%d 应大于 %d（服务端 seq 独立自增）", seq2, seq1)
	}
	lines := readMirrorLines(t, mirrorFile)
	if len(lines) != 2 || lines[0] != mirror.HeaderComment {
		t.Errorf("删除后重建镜像 = %q，期望头注释+1 行（首建语义重现）", lines)
	}
	if want := strconv.FormatInt(seq2, 10) + " | "; !strings.HasPrefix(lines[len(lines)-1], want) {
		t.Errorf("重建后尾行 = %q，期望以 seq=%d 开头", lines[len(lines)-1], seq2)
	}

	// 外部篡改：覆盖为垃圾内容后 send 照常退 0，新行原样追加不校验（§8.4）。
	if err := os.WriteFile(mirrorFile, []byte("外部篡改\n"), 0o644); err != nil {
		t.Fatalf("篡改镜像文件失败: %v", err)
	}
	seq3 := sendOnce("篡改后一条")
	if seq3 <= seq2 {
		t.Errorf("篡改后 seq=%d 应大于 %d", seq3, seq2)
	}
	lines = readMirrorLines(t, mirrorFile)
	if len(lines) != 2 {
		t.Fatalf("篡改后镜像应 2 行（垃圾行+追加行），实际 %d: %q", len(lines), lines)
	}
	if lines[0] != "外部篡改" {
		t.Errorf("篡改行被意外改动: %q", lines[0])
	}
	if want := strconv.FormatInt(seq3, 10) + " | "; !strings.HasPrefix(lines[1], want) {
		t.Errorf("篡改后追加行 = %q，期望以 seq=%d 开头", lines[1], seq3)
	}
}

// TestImportPurity AC13.3 架构判据测试化：internal/server + internal/store 的
// import 树（go list -deps 传递闭包）零 git 相关包、无 remote 调用路径。
//
// 实现方式裁量=go list -deps（可维护者：一条命令拿完整传递树，含跨模块传递
// 依赖；go/parser 手写扫描只能看直接 import，漏传递面）。判据两层：
//  1. 第三方模块白名单=go.mod require 全集（技术设计 §0 依赖白名单的最小闭包）
//     ——server/store 引用任何新增第三方模块即红，而一切 git 库（go-git/go-github/
//     libgit2/git2go 等）引入必是新模块，故本断言等价且强于「零 git 相关 import」
//     字面枚举，零漏报；go.mod require 变更时须同步本清单（失败信息已提示）。
//  2. aiteam 自有代码（本 module 传递包）不 import os/exec——堵死「spawn git
//     CLI 进程」的 remote 调用路径（git 库路径已由 1 封闭，git remote 两条可能
//     路径全断；server 自身的 net/http 属服务端入站面，不在本判据范围）。
//     裁量说明：传递树中 os/exec 实际仅由 modernc.org/libc（纯 Go sqlite 驱动
//     固有依赖）引入，属第三方内部实现，不在「代码走查可达路径」语义内，故
//     os/exec 禁令限定在 aiteam 自有包面（实测 server/store 直接 import 面零
//     os/exec，见下方运行断言）。
func TestImportPurity(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go 命令不可用（import 纯净断言依赖 go list）: %v", err)
	}
	// 每行三列：包路径、归属模块（STD=标准库）、直接 import 面（逗号分隔）。
	cmd := exec.Command(goBin, "list", "-deps",
		"-f", "{{.ImportPath}}\t{{if .Module}}{{.Module.Path}}{{else}}STD{{end}}\t{{join .Imports \",\"}}",
		"aiteam/internal/server", "aiteam/internal/store")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list 失败: %v\n%s", err, out)
	}
	// 白名单模块集（=go.mod require 全集 + 本 module aiteam，2026-10-02 对照；
	// 除 aiteam/sqlite 外均为 modernc.org/sqlite 的传递闭包）。
	allowed := map[string]bool{
		"aiteam":                           true, // 本 module（server/store 自身）
		"modernc.org/sqlite":               true, // 唯一直接依赖（技术设计 §0 白名单）
		"modernc.org/libc":                 true,
		"modernc.org/mathutil":             true,
		"modernc.org/memory":               true,
		"github.com/dustin/go-humanize":    true, // 以下均为 sqlite 传递闭包
		"github.com/google/uuid":           true,
		"github.com/mattn/go-isatty":       true,
		"github.com/ncruces/go-strftime":   true,
		"github.com/remyoudompheng/bigfft": true,
		"golang.org/x/exp":                 true,
		"golang.org/x/sys":                 true,
	}
	var violations []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		pkg, rest, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("go list 输出行形状异常: %q", line)
		}
		mod, imports, ok := strings.Cut(rest, "\t")
		if !ok {
			t.Fatalf("go list 输出行形状异常: %q", line)
		}
		if mod != "STD" && !allowed[mod] {
			violations = append(violations, pkg+"（模块 "+mod+" 不在 server/store 依赖白名单）")
		}
		// os/exec 禁令限定 aiteam 自有代码（裁量说明见函数头注 2）。
		if mod == "aiteam" && slices.Contains(strings.Split(imports, ","), "os/exec") {
			violations = append(violations, pkg+"（自有代码 import os/exec：可 spawn git 进程的 remote 调用路径）")
		}
	}
	if len(violations) > 0 {
		t.Errorf("AC13.3 违规：server/store import 树含非白名单包（若 go.mod require 已合法变更，请同步本测试白名单清单）:\n%s",
			strings.Join(violations, "\n"))
	}
}
