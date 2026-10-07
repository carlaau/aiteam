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
// 服务端分配的 seq 与服务端时间。
func parseOKLine(t *testing.T, stdout string) (int64, string) {
	t.Helper()
	line := strings.TrimSpace(stdout)
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
	// 解析「项目不存在: <目标code>」）与身份域（心跳中间件「项目未登记: <发送方
	// project>」）。发送方自身项目拼错+带 --to-project 时不得误报「目标项目不
	// 存在」（目标项目实存），rc=4 透传身份域文案（AC9.2 指明真实缺失层级）。
	_, stderr = runWantCode(t, "身份域坏项目+--to-project", 4, func() int {
		return runSend(ctx, sendArgs("--project", "ghost-a",
			"--to-project", crossBProject, "--to-role", "executor",
			"--body", "x", "--mirror-root", t.TempDir()), addr)
	})
	if !strings.Contains(stderr, "项目未登记") || !strings.Contains(stderr, "ghost-a") {
		t.Errorf("身份域坏项目 stderr = %q，期望透传「项目未登记: ghost-a」", stderr)
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

// TestNoMirror --no-mirror 与 --json：跳过双写不落文件；--json 输出与 #9 响应
// 同构（恰四键 seq/created_at/level/kind）。
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

	// --json：恰四键同构 + 字段值正确；默认双写照常（--json 不影响镜像）。
	stdout, _ = runWantCode(t, "--json", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--level", "important", "--body", "json 输出",
			"--json", "--mirror-root", root), addr)
	})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &raw); err != nil {
		t.Fatalf("--json 输出非合法 JSON: %v\n%s", err, stdout)
	}
	if len(raw) != 4 {
		t.Errorf("--json 键数 = %d，期望恰 4 键与 #9 响应同构（实际键 %v）", len(raw), raw)
	}
	for _, k := range []string{"seq", "created_at", "level", "kind"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("--json 缺键 %q（#9 响应同构）", k)
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
