package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// B6-5 集成验收（b6-plan §B6-5，纯测试收口零实现代码）：token 全链+复验。
// in-process serve（临时真库/httptest）+ CLI 命令函数直调，同 integration_b2/b8
// 范式。四测试按文件序，矩阵完整性=本文件独有价值——
//
//	TestTokenOffAllCommands    AC17.2 全命令面（token 关=22 命令无凭证逐个退 0）
//	TestTokenOnAllCommands     AC17.3 全命令面（开=无 token 逐命令 4+auth_required → 带 --token 全通）
//	TestConfigRestartEffect    AC17.1 复验（--config 的 listen/db.path 重启生效）
//	TestErrorSemanticsMatrix   AC17.4 全命令面（停服=3 / 本地参错=2 / 服务端 400=4 / 401=4 互不混淆）
//
// 消重边界（契约③「测试消重」）：单命令行为细节（输出逐字格式/位点/回执链/并
// 发压测/镜像行）归 B1/B2/B4 既有面；B6-2（cli_test.go）已锚 mapExitCode 纯函
// 数矩阵与 project list 单命令的 401 对照（TestAuthRequired401EndToEnd）/退 3
// （TestUnreachableStill3）——本文件不重复，独有覆盖=「全命令面 × token 三态 ×
// 错误语义三类」的矩阵完整性。B6-2 夹具 startTokenEnabledServer/clearLookupChainEnv
// 与 b2/integ 系辅助直接复用。
//
// 鉴权矩阵命令面裁量（feat/b6 实存；plan 原文提的 status/watch 尚未存在——按
// 实存命令面写，主会话报备总控）：走 HTTP、受 tokenAuth 管辖的 22 命令全入矩
// 阵——send/poll/ack/history + project 族 4 + column 族 4 + session list + audit
// + progress report/list + resource 族 3 + window 族 3。不入矩阵及理由：
//   - serve：服务本体非调用面，其配置行为由 TestConfigRestartEffect 单独覆盖；
//   - backup：本地运维命令不碰网络（b6-spec 显式边界声明，AC17.4 不可达语义
//     不适用，token 面同理）；
//   - version：main.go 纯本地打印 server.Version，无 HTTP 面，token 开关零感知；
//   - init：本地脚手架生成（写文件+git hook 安装），不发 HTTP（B8-7 集成已证）。

// b6AuthToken token 开态测试实例的启动密钥（startTokenEnabledServer 注入面）。
const b6AuthToken = "b6-integration-token"

// b6AuthCase 鉴权矩阵单命令用例：call 注入服务地址与凭证（token 空串=无凭证形
// 态）与镜像根（send 隔离用），返回 CLI 命令函数退出码；断言由矩阵 runner 统一
// 做（退出码+401 轮 stderr 错误码），用例只负责构参直调。
type b6AuthCase struct {
	name string
	call func(t *testing.T, addr, token, mirrorRoot string) int
}

// b6IdentityWithToken 身份四参 + 可选 --token flag（token 空串=查找链全空的无凭
// 证形态；flag 胜出语义见 cli_test.go clearLookupChainEnv 注）。
func b6IdentityWithToken(token string) []string {
	args := regIdentityArgs()
	if token != "" {
		args = append(args, "--token", token)
	}
	return args
}

// b6AuthMatrix 鉴权矩阵全命令面（22 条，表序即执行序=数据面依赖序）：身份恒挂
// 引导域 (management, c01)，目标实体用独立 code（p-b6/c-b6）——登记/更新/归档
// 全走自建实体不碰引导域；resID 为轮内状态（resource register 输出供 release
// 引用）。每个测试自取一份（闭包状态互不串扰）。
func b6AuthMatrix() []b6AuthCase {
	resID := 0
	return []b6AuthCase{
		{"project register", func(t *testing.T, addr, tok, root string) int {
			return runProjectRegister(context.Background(), append(b6IdentityWithToken(tok),
				"--code", "p-b6", "--name", "B6集成项目"), addr)
		}},
		{"project update", func(t *testing.T, addr, tok, root string) int {
			return runProjectUpdate(context.Background(), append(b6IdentityWithToken(tok),
				"--code", "p-b6", "--name", "B6集成项目改"), addr)
		}},
		{"project list", func(t *testing.T, addr, tok, root string) int {
			return runProjectList(context.Background(), b6IdentityWithToken(tok), addr)
		}},
		{"column register", func(t *testing.T, addr, tok, root string) int {
			return runColumnRegister(context.Background(), append(b6IdentityWithToken(tok),
				"--code", "c-b6", "--name", "B6集成栏目"), addr)
		}},
		{"column update", func(t *testing.T, addr, tok, root string) int {
			return runColumnUpdate(context.Background(), append(b6IdentityWithToken(tok),
				"--code", "c-b6", "--name", "B6集成栏目改"), addr)
		}},
		{"column list", func(t *testing.T, addr, tok, root string) int {
			return runColumnList(context.Background(), b6IdentityWithToken(tok), addr)
		}},
		{"column archive", func(t *testing.T, addr, tok, root string) int {
			return runColumnArchive(context.Background(), append(b6IdentityWithToken(tok),
				"--code", "c-b6"), addr)
		}},
		{"project archive", func(t *testing.T, addr, tok, root string) int {
			return runProjectArchive(context.Background(), append(b6IdentityWithToken(tok),
				"--code", "p-b6"), addr)
		}},
		{"send", func(t *testing.T, addr, tok, root string) int {
			return runSend(context.Background(), append(b6IdentityWithToken(tok),
				"--to-role", "executor", "--body", "B6集成消息", "--mirror-root", root), addr)
		}},
		{"poll", func(t *testing.T, addr, tok, root string) int {
			return runPoll(context.Background(), b6IdentityWithToken(tok), addr)
		}},
		{"ack", func(t *testing.T, addr, tok, root string) int {
			return runAck(context.Background(), b6IdentityWithToken(tok), addr)
		}},
		{"history", func(t *testing.T, addr, tok, root string) int {
			return runHistory(context.Background(), b6IdentityWithToken(tok), addr)
		}},
		{"session list", func(t *testing.T, addr, tok, root string) int {
			return runSessionList(context.Background(), b6IdentityWithToken(tok), addr)
		}},
		{"audit", func(t *testing.T, addr, tok, root string) int {
			return runAudit(context.Background(), b6IdentityWithToken(tok), addr)
		}},
		{"progress report", func(t *testing.T, addr, tok, root string) int {
			return runProgressReport(context.Background(), append(b6IdentityWithToken(tok),
				"--batch", "B6", "--task", "B6-5", "--tests", "pass", "--summary", "B6-5 集成验收"), addr)
		}},
		{"progress list", func(t *testing.T, addr, tok, root string) int {
			return runProgressList(context.Background(), b6IdentityWithToken(tok), addr)
		}},
		{"resource register", func(t *testing.T, addr, tok, root string) int {
			var stdout, stderr string
			var code int
			stdout, stderr = captureStdoutStderr(t, func() {
				code = runResourceRegister(context.Background(), append(b6IdentityWithToken(tok),
					"--type", "port", "--value", "8080"), addr)
			})
			if code == 0 {
				// 全通轮解析登记 id 供 release 引用；拒/断轮 stdout 空不解析
				// （resID 保持 0，release 走占位保 HTTP 层语义）。
				if _, err := fmt.Sscanf(stdout, "OK resource=%d", &resID); err != nil {
					t.Errorf("resource register 输出解析失败: %v（stdout: %q）", err, stdout)
				}
			} else {
				// 嵌套捕获分层：本 call 的 stderr 落在内层管道（runner 外层捕获
				// 不到），非 0 退出时回灌 os.Stderr——此时内层 defer 已还原为
				// 外层管道，runner 的 401 auth_required 断言靠它。
				fmt.Fprint(os.Stderr, stderr)
			}
			return code
		}},
		{"resource list", func(t *testing.T, addr, tok, root string) int {
			return runResourceList(context.Background(), b6IdentityWithToken(tok), addr)
		}},
		{"resource release", func(t *testing.T, addr, tok, root string) int {
			id := resID
			if id == 0 {
				id = 1 // 拒/断轮 register 未落库，占位正数过本地校验，让语义落在 HTTP 层（401/3）
			}
			return runResourceRelease(context.Background(), append(b6IdentityWithToken(tok),
				"--id", strconv.Itoa(id)), addr)
		}},
		{"window set", func(t *testing.T, addr, tok, root string) int {
			return runWindowSet(context.Background(), append(b6IdentityWithToken(tok),
				"--stage", "S4", "--from", "23:00", "--to", "09:00"), addr)
		}},
		{"window list", func(t *testing.T, addr, tok, root string) int {
			return runWindowList(context.Background(), b6IdentityWithToken(tok), addr)
		}},
		{"window now", func(t *testing.T, addr, tok, root string) int {
			return runWindowNow(context.Background(), b6IdentityWithToken(tok), addr)
		}},
	}
}

// TestTokenOffAllCommands AC17.2 全命令面：默认配置（config.Default，token 关）
// 下 22 命令逐个无凭证执行退 0——拷贝即跑零配置可用在全部命令面上收口（单命令
// 行为细节归 B1/B2/B4 既有面，此处只断「通」）。
func TestTokenOffAllCommands(t *testing.T) {
	clearLookupChainEnv(t)
	matrix := b6AuthMatrix()
	addr := startRegTestServer(t) // config.Default() 装配=token 关默认态
	root := t.TempDir()
	for _, tc := range matrix {
		runWantCode(t, "token关/"+tc.name, 0, func() int {
			return tc.call(t, addr, "", root)
		})
	}
}

// TestTokenOnAllCommands AC17.3 全命令面：token 开态两轮——先逐命令无 token 退 4
// 且 stderr 透传 auth_required（tokenAuth 先于 mux 与心跳，401 零触库，故拒轮
// 不留数据面）；再逐命令带 --token 全通退 0（B6-2 已锚 project list 单命令注入
// 链，此处铺满 22 命令面）。
func TestTokenOnAllCommands(t *testing.T) {
	clearLookupChainEnv(t)
	matrix := b6AuthMatrix()
	addr := startTokenEnabledServer(t, b6AuthToken)
	root := t.TempDir()

	for _, tc := range matrix {
		_, stderr := runWantCode(t, "401/"+tc.name, 4, func() int {
			return tc.call(t, addr, "", root)
		})
		assertContains(t, "401/"+tc.name+" stderr", stderr, "auth_required")
	}

	// 审查采纳（B6-5 复审建议 1）：「401 零触库」从链序结构性依赖升级为受测行
	// 为——拒轮后以一次性探测身份查 session list（GET /api/v1/sessions 不在心
	// 跳豁免面，带四头查询自身必 upsert 探测身份：探测行出现=查询链路在位），
	// 主身份 cli-test-session 缺席=拒轮 22 命令零 upsert（seed 只建项目/栏目不
	// 建会话）。若 tokenAuth 与心跳链序倒置/漏拦，主身份会被拒轮心跳写入，此处
	// 变红。
	probeIdent := []string{
		"--project", regAuthProject,
		"--column", regAuthColumn,
		"--session", "b6-zero-touch-probe",
		"--role", regAuthRole,
	}
	stdout, _ := runWantCode(t, "拒轮后零触库探测 session list", 0, func() int {
		return runSessionList(context.Background(), append(probeIdent, "--token", b6AuthToken), addr)
	})
	if strings.Contains(stdout, regAuthSession) {
		t.Errorf("拒轮后 session list 含主身份 %q：\n%s——401 拒轮触库（心跳未被 tokenAuth 拦截）", regAuthSession, stdout)
	}
	if !strings.Contains(stdout, "b6-zero-touch-probe") {
		t.Errorf("拒轮后 session list 缺探测身份自身行:\n%s——查询链路不在位，零触库断言失真", stdout)
	}

	for _, tc := range matrix {
		runWantCode(t, "带token/"+tc.name, 0, func() int {
			return tc.call(t, addr, b6AuthToken, root)
		})
	}

	// 对照面：全通轮 22 命令以主身份调用（上报即心跳 upsert）——session list
	// 此时可见主身份，与拒轮空面构成「拒/通触库分野」硬对照（对照查询自身用主
	// 身份，其 upsert 只刷新自身行，不影响断言面）。
	stdout, _ = runWantCode(t, "全通轮后 session list 对照", 0, func() int {
		return runSessionList(context.Background(), b6IdentityWithToken(b6AuthToken), addr)
	})
	if !strings.Contains(stdout, regAuthSession) {
		t.Errorf("全通轮后 session list 缺主身份 %q：\n%s——心跳 upsert 链断裂", regAuthSession, stdout)
	}
}

// startB6RestartableServe 起一个显式可控启停的 in-process serve（形态同
// startB8Serve 的装配链：--config 文件→serve(ctx)→ready 回调真实地址），差异点=
// stop 闭包供测试中途停机——AC17.1「重启生效」语义要求先停再起，B8 夹具的停机
// 挂在 t.Cleanup 无中途停能力，故按同形态自持（数据/超时口径逐行同源）。
// stop 幂等（sync.Once）且兜底挂 t.Cleanup：测试中途 Fatal 也不泄漏 serve goroutine。
func startB6RestartableServe(t *testing.T, cfgName, listen, dbPath string) (addr string, stop func()) {
	t.Helper()
	cfgPath := filepath.Join(filepath.Dir(dbPath), cfgName)
	cfg := `{"_doc":"B6-5 重启生效集成配置","listen":"` + listen + `","db":{"type":"sqlite","path":"` +
		filepath.ToSlash(dbPath) + `"}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("写测试配置失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() { errCh <- serve(ctx, []string{"--config", cfgPath}, func(a string) { ready <- a }) }()

	var once sync.Once
	stop = func() {
		once.Do(func() {
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
	}
	t.Cleanup(stop)

	select {
	case addr = <-ready:
		addr = "http://" + addr // 裸 host:port 包成完整 URL（startB8Serve 同款）
	case err := <-errCh:
		t.Fatalf("serve 提前退出: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve 10 秒未就绪")
	}
	return addr, stop
}

// b6AssertLoopbackAddr 断言 serve ready 地址的 host=127.0.0.1 且端口已实占非零
// ——listen 生效的确定性判据：内建默认 listen=0.0.0.0:8310，若 --config 的
// listen 未生效，host 会落 0.0.0.0/[::]（或 8310 撞车拒启），两种偏离都会在此红。
func b6AssertLoopbackAddr(t *testing.T, stage, addr string) {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(addr, "http://"))
	if err != nil {
		t.Fatalf("%s 地址 %q 解析失败: %v", stage, addr, err)
	}
	if host != "127.0.0.1" {
		t.Errorf("%s listen host = %q，期望 127.0.0.1（--config listen 未生效？内建默认为 0.0.0.0:8310）", stage, host)
	}
	if n, err := strconv.Atoi(port); err != nil || n <= 0 {
		t.Errorf("%s listen port = %q，期望已实占的正整数端口", stage, port)
	}
}

// TestConfigRestartEffect AC17.1 复验（B0 机制集成面）：--config 指定 listen/
// db.path 重启生效。两轮 serve 顺序（先停再起=重启语义）：cfgA（127.0.0.1:0 +
// 库 A）登记 proj-restart → 停机 → cfgB（127.0.0.1:0 + 全新库 B）——listen 判
// 据见 b6AssertLoopbackAddr；db.path 判据=A/B 数据隔离（B 空库查不到 A 的登记）
// +双库文件真实落盘。身份域借 B8 夹具 seedB8Domain（b8int/c01，形态同款口径：
// serve 未启动时先行插库，WAL 多连接并发安全）。
func TestConfigRestartEffect(t *testing.T) {
	clearLookupChainEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	dbA := filepath.Join(dir, "restart-a.db")
	dbB := filepath.Join(dir, "restart-b.db")
	seedB8Domain(t, dbA)
	seedB8Domain(t, dbB)
	ident := []string{
		"--project", b8IntProject,
		"--column", b8IntColumn,
		"--session", "b6-restart-i1",
		"--role", b8IntRole,
	}

	// ── serve1：cfgA 起服，listen/db.path(A) 生效面 ──
	addr1, stop1 := startB6RestartableServe(t, "restart-a.json", "127.0.0.1:0", dbA)
	b6AssertLoopbackAddr(t, "serve1", addr1)
	runWantCode(t, "serve1 登记 proj-restart", 0, func() int {
		return runProjectRegister(ctx, append(ident,
			"--code", "proj-restart", "--name", "重启生效验证A"), addr1)
	})
	stop1() // 先停再起=重启语义（stop 内断言优雅退出）
	if _, err := os.Stat(dbA); err != nil {
		t.Errorf("serve1 停机后库文件 %s 不存在: %v（db.path=A 未生效？）", dbA, err)
	}

	// ── serve2：cfgB（全新库 B）——A 的数据不过界 = db.path 切换生效 ──
	addr2, stop2 := startB6RestartableServe(t, "restart-b.json", "127.0.0.1:0", dbB)
	b6AssertLoopbackAddr(t, "serve2", addr2)
	stdout, _ := runWantCode(t, "serve2 project list", 0, func() int {
		return runProjectList(ctx, ident, addr2)
	})
	if strings.Contains(stdout, "proj-restart") {
		t.Errorf("serve2（库 B）list 查到了 serve1（库 A）的登记: %q——db.path 切换未生效", stdout)
	}
	runWantCode(t, "serve2 登记 proj-restart-b", 0, func() int {
		return runProjectRegister(ctx, append(ident,
			"--code", "proj-restart-b", "--name", "重启生效验证B"), addr2)
	})
	stdout, _ = runWantCode(t, "serve2 复查 list", 0, func() int {
		return runProjectList(ctx, ident, addr2)
	})
	assertContains(t, "serve2 复查 list", stdout, "proj-restart-b")
	stop2()
	if _, err := os.Stat(dbB); err != nil {
		t.Errorf("serve2 停机后库文件 %s 不存在: %v（db.path=B 未生效？）", dbB, err)
	}
}

// TestErrorSemanticsMatrix AC17.4 全命令面：三类错误路径互不混淆——
// 停服务=逐命令退 3（不可达；B6-2 只锚过 project list 单命令，此处铺满矩阵）；
// 本地参数错=退 2（不发请求）；服务端 400=退 4（invalid_role 经心跳中间件 ①′，
// mapExitCode 对 APIError 不分状态码）；401=退 4（token 开+无 token，全命令面已
// 由 TestTokenOnAllCommands 铺满，此处单命令收拢）。收拢断言：同一命令
// project list 在 2/3/4 三条件下退出码两两不等——用户动作可分辨（2=查参数、
// 3=查网络/进程、4=查 token/配置）。
func TestErrorSemanticsMatrix(t *testing.T) {
	clearLookupChainEnv(t)
	matrix := b6AuthMatrix()
	root := t.TempDir()

	// ① 停服务=全命令退 3：closedServerAddr=确定性连接拒绝（起后即关的 httptest），
	//    与 401 的「可达被拒」严格分流。
	dead := closedServerAddr(t)
	for _, tc := range matrix {
		runWantCode(t, "停服务/"+tc.name, 3, func() int {
			return tc.call(t, dead, "", root)
		})
	}

	// ② 本地参数错=退 2：project list 缺身份四参，本地校验短路不发请求
	//    （dead 地址无碍——请求根本不出门）。
	var code2 int
	_, stderr2 := captureStdoutStderr(t, func() {
		code2 = runProjectList(context.Background(), nil, dead)
	})
	if code2 != 2 {
		t.Errorf("缺身份参数退出码 = %d（stderr: %q），期望 2", code2, stderr2)
	}
	assertContains(t, "本地参错 stderr", stderr2, "缺少必填身份参数")

	// ③ 服务端 400=退 4：role 非法形态（CLI 本地只查非空），心跳中间件 ①′
	//    invalid_role 400 → APIError → 4。
	addr := startRegTestServer(t)
	badRole := []string{
		"--project", regAuthProject,
		"--column", regAuthColumn,
		"--session", regAuthSession,
		"--role", "bogus-role",
	}
	var code400 int
	_, stderr400 := captureStdoutStderr(t, func() {
		code400 = runProjectList(context.Background(), badRole, addr)
	})
	if code400 != 4 {
		t.Errorf("服务端 400 invalid_role 退出码 = %d（stderr: %q），期望 4", code400, stderr400)
	}
	assertContains(t, "invalid_role stderr", stderr400, "invalid_role")

	// ④ 401=退 4：token 开+无 token（对照 ③ 的可达被拒两态同码不同因——
	//    均为 4 是 §4.7 冻结口径「服务端拒绝=4」的收敛，非混淆；与 2/3 的分野
	//    才是 AC17.4 的语义区分面）。
	authAddr := startTokenEnabledServer(t, b6AuthToken)
	var code401 int
	_, _ = captureStdoutStderr(t, func() {
		code401 = runProjectList(context.Background(), regIdentityArgs(), authAddr)
	})
	if code401 != 4 {
		t.Errorf("401 无 token 退出码 = %d，期望 4（B6-2 已铺 stderr 透传面，此处只收退出码）", code401)
	}

	// ⑤ 收拢：同一命令三条件退出码两两不等（AC17.4「互不混淆」的硬判据）。
	//    不可达侧取矩阵首命令回抽代表（① 已断言全矩阵=3，此处只取码参战对照）。
	if code2 == code400 || code2 == code401 {
		t.Errorf("本地参错(%d) 与服务端拒(%d/%d) 混流", code2, code400, code401)
	}
	var code3 int
	captureStdoutStderr(t, func() {
		code3 = matrix[0].call(t, dead, "", root)
	})
	if code3 != 3 {
		t.Errorf("停服务回抽 %s 退出码 = %d，期望 3", matrix[0].name, code3)
	}
	if code3 == code2 || code3 == code400 {
		t.Errorf("不可达(%d) 与参错(%d)/服务端拒(%d) 混流", code3, code2, code400)
	}
}
