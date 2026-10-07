package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"aiteam/internal/config"
	"aiteam/internal/server"
	"aiteam/internal/store"
)

// 观测域全链集成验收（B3-8，纯测试收口零实现代码——status.go 四处自含 SQL 的
// B3-8 收敛裁定见 store/status.go 各查询函数注）：in-process serve+临时库+CLI
// 命令函数+注入时钟，七段动线按文件序各一测试函数。夹具全量复用既有测试面
// （captureStdoutStderr/runWantCode/sendOK/statusSeed* 系/seedStatusFixtures/
// injectStatusClock/injectWindowClock/statusTokenJoin）。
//
// 七段与 AC 对照（b3-plan B3-8 节 × b3-spec AC10.x/11.x/12.x/5.1）：
//
//	段 1 TestIntegrationB3WatchThenSendHit         AC10.1（watch 后台→send→2s 内退 0 输出 HIT）
//	段 2 TestIntegrationB3WatchIdleKeepsPolling    AC10.2（空信箱持续轮询不退出+信号收尾）
//	段 3 TestIntegrationB3DeadSentinelZeroLoss     AC10.3（kill 残留哨兵→16s→dead+poll 全量零丢失）
//	段 4 TestIntegrationB3AckSuppressesHit         AC10.4（ack 后 watch 不再报已确认消息）
//	段 5 TestIntegrationB3ZeroContextGlobalDrill   AC11.4/V3 演练（零上下文 status --global 四问+<5s）
//	段 6 TestIntegrationB3HeartbeatAdvanceThenLost AC12.1/12.2（watch 循环期 last_seen 前进→停+推钟→LOST）
//	段 7 TestIntegrationB3PendingTracksSendAck     AC5.1 联动（send→pending+1→ack→归零）
//
// 与 B3-6 既有 TestWatchIntegrationHit 的分工：彼处=先种消息后 watch（首轮即命中，
// 验「位点缺行全量可见」语义）；本文件段 1=先 watch 后 send（空轮等待中被命中，
// 验轮询唤醒全时序）。两形态互补不重复。
//
// 本文件同时是「手工冒烟两终端形态」的观测域等价替代物（serve+watch 后台+send+
// status 的跨进程手工流程在此以进程内同链路自动化走全），真进程冒烟由主会话验收段执行。
//
// 时钟纪律：types.NowUTC/store.NowInTz 均包级注入点，注入/还原仅限主 goroutine
// 串行执行（watch goroutine 经 HTTP handler 间接读；本仓禁 -race 口径下以「先
// 注入后动作、动作完再注入」的顺序纪律保证无逻辑竞态，与时钟敏感用例 -count=10
// 压测互补验证）。

// b3T0 观测域集成段的基准时刻（注入钟起点；哨兵失活阈 15s、失联阈 900s 的推进
// 基准）。
const b3T0 = "2026-10-02T13:00:00Z"

// b3TPlus 基准时刻 +sec 秒（RFC3339 Z 形态保持——同形串字典序=时间序，可直接比较）。
func b3TPlus(sec int) string {
	t, err := time.Parse(time.RFC3339, b3T0)
	if err != nil {
		panic(fmt.Sprintf("基准时刻解析失败: %v", err)) // 常量写死不可达；守恒编译器边界
	}
	return t.Add(time.Duration(sec) * time.Second).Format(time.RFC3339)
}

// b3Counters 服务端请求计数（poll 轮数/DELETE 到达数）——watch 循环节奏与收尾
// 注销的观测面（httptest wrapper 层计数，atomic 供跨 goroutine 读）。
type b3Counters struct {
	poll int32 // POST .../sentinels/{id}/poll 到达次数
	del  int32 // DELETE /api/v1/sentinels/{id} 到达次数
}

// (cnt *b3Counters) loads 计数快照（一次读两值的取面收敛，禁散装 atomic.Load）。
func (cnt *b3Counters) loads() (poll, del int32) {
	return atomic.LoadInt32(&cnt.poll), atomic.LoadInt32(&cnt.del)
}

// startB3IntegrationServer 观测域集成环境：临时真文件库（WAL 语义禁 :memory: 同
// startRegTestServer 口径）+完整 server+httptest+引导域夹具，返回 store 句柄
// （库内直插/断言）、服务地址与请求计数。外层 wrapper 记录 poll/DELETE 到达数后
// 原样透传 inner。
func startB3IntegrationServer(t *testing.T) (*store.Store, string, *b3Counters) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "b3-itest.db"))
	if err != nil {
		t.Fatalf("打开临时 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cnt := new(b3Counters)
	inner := server.NewServer(st, config.Default(), server.Version)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/poll"):
			atomic.AddInt32(&cnt.poll, 1)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/sentinels/"):
			atomic.AddInt32(&cnt.del, 1)
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	if _, err := st.CreateProject(store.Project{Code: regAuthProject, Name: "引导项目"}); err != nil {
		t.Fatalf("seed 引导项目失败: %v", err)
	}
	if _, _, err := st.CreateColumnWithAudit(regAuthProject, store.Column{Code: regAuthColumn, Name: "引导栏目"},
		store.AuditEntry{Action: store.AuditColumnRegister, Detail: "{}"}); err != nil {
		t.Fatalf("seed 引导栏目失败: %v", err)
	}
	return st, ts.URL, cnt
}

// b3WatchArgs watch 身份四参（watch-sess/executor——与 send 方 cli-test-session
// controller 不同会话，status 断言段的身份请求不会刷新 watch 会话心跳，AC12.2 的
// LOST 构造依赖此隔离）。
func b3WatchArgs() []string {
	return integArgs(regAuthProject, regAuthColumn, "watch-sess", "executor")
}

// b3WaitForCond 以 20ms 节奏轮询 cond 直至成立（超时 fail 附上下文）——watch 真实
// 轮询节奏（--interval 100ms → 服务端回显 interval_sec=1 → 1s/轮）下库态变化的
// 确定性等待面，替代任意时长 sleep。
func b3WaitForCond(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时（%v）：%s", timeout, what)
}

// b3SessionLastSeen 读会话行 last_seen_at（AC12.1 断言面；行不存在返回空串交由
// 调用方 cond 继续等——注册请求经心跳中间件 upsert 落行）。
func b3SessionLastSeen(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	var got string
	if err := st.DB.QueryRow(
		`SELECT last_seen_at FROM sessions WHERE name = ?`, name).Scan(&got); err != nil {
		return "" // 行未落（注册请求未达）——调用方 cond 继续等
	}
	return got
}

// b3StatusGlobal 以引导域身份跑 status --global 退 0，返回 stdout（AC10.3/12.2
// 断言入口；身份=cli-test-session，与 watch 会话隔离见 b3WatchArgs 注）。
func b3StatusGlobal(t *testing.T, addr string) string {
	t.Helper()
	stdout, _ := runWantCode(t, "status --global", 0, func() int {
		return runStatusGlobal(context.Background(), addr)
	})
	return stdout
}

// TestIntegrationB3WatchThenSendHit 段 1（AC10.1）：watch 后台 goroutine 先起（空
// 信箱进入轮询等待）→ send 一条进其信箱 → watch 在 send 完成后 2s 内退出 0 且输出
// `HIT seq=<n> level=<l>`。D2 余量：生效轮询间隔=服务端回显 interval_sec=1s
// （--interval 100ms 向上取整到秒），最坏路径=send 后 1s+一轮 HTTP，2s 窗覆盖
// （时钟敏感用例，-count=10 压测口径见文件头）。--json 命中摘要形态由 B3-6
// TestWatchIntegrationHit 第二段锁定，此处不重复。
func TestIntegrationB3WatchThenSendHit(t *testing.T) {
	_, addr, cnt := startB3IntegrationServer(t)
	ctx := context.Background()
	sigCh := make(chan os.Signal, 1)
	root := t.TempDir()

	var code int
	var hitSeq int64
	stdout, stderr := captureStdoutStderr(t, func() {
		codeCh := make(chan int, 1)
		go func() {
			codeCh <- runWatchCmd(ctx, append(b3WatchArgs(), "--interval", "100ms"), addr, sigCh)
		}()
		// watch 完成注册+首轮空 poll 后再 send（150ms 远小于 1s 轮询间隔——保证
		// 「先等待后命中」的时序形态；sendOK 内层 capture 嵌套还原安全，其 OK 行
		// 不含 HIT 前缀不污染下方断言）。
		time.Sleep(150 * time.Millisecond)
		hitSeq, _ = sendOK(t, addr, root, "--to-role", "executor", "--body", "观测域唤醒指令")
		select {
		case code = <-codeCh:
		case <-time.After(2 * time.Second):
			t.Error("watch 在 send 后 2s 内未退出（AC10.1 判据失守）")
			return
		}
	})
	if code != 0 {
		t.Fatalf("watch 退出码 = %d（stderr: %q），期望 0（AC10.1 命中退出）", code, stderr)
	}
	if want := fmt.Sprintf("HIT seq=%d level=normal", hitSeq); !strings.Contains(stdout, want) {
		t.Errorf("watch 输出缺 %q\n全文: %q", want, stdout)
	}
	if _, del := cnt.loads(); del != 1 {
		t.Errorf("DELETE 到达次数 = %d，期望 1（watch 命中收尾注销）", del)
	}
}

// TestIntegrationB3WatchIdleKeepsPolling 段 2（AC10.2）：空信箱 watch 持续轮询不
// 退出——可控轮数断言（wrapper 计 poll 到达数，≥3 轮后命令仍未退出=不退出判据；
// 轮数可控=以计数器为闸而非任意 sleep）→ 信号注入收尾：SIGTERM → 退出 6+DELETE
// 恰 1 次到达（context/信号取消收尾路径的端到端）+全程零 HIT 输出。
func TestIntegrationB3WatchIdleKeepsPolling(t *testing.T) {
	_, addr, cnt := startB3IntegrationServer(t)
	ctx := context.Background()
	sigCh := make(chan os.Signal, 1)

	var code int
	stdout, stderr := captureStdoutStderr(t, func() {
		codeCh := make(chan int, 1)
		go func() {
			codeCh <- runWatchCmd(ctx, append(b3WatchArgs(), "--interval", "100ms"), addr, sigCh)
		}()
		// 可控轮数：等 3 轮 poll 到达（1s/轮，5s 超时兜底）——watch 仍在循环中。
		b3WaitForCond(t, 5*time.Second, "poll 轮数达 3", func() bool {
			poll, _ := cnt.loads()
			return poll >= 3
		})
		select {
		case c := <-codeCh:
			t.Errorf("空信箱 watch 提前退出 %d（AC10.2 持续轮询不退出判据失守）", c)
			return
		default: // codeCh 空=watch 仍阻塞循环=正确形态
		}
		// 信号收尾：注入 SIGTERM（runWatchCmd 的 sigCh=测试注入点，见
		// TestWatchSignalTerminates 同款）→ 尽力 DELETE 后退 6。
		sigCh <- syscall.SIGTERM
		select {
		case code = <-codeCh:
		case <-time.After(3 * time.Second):
			t.Error("SIGTERM 后 watch 3s 内未收尾退出")
			return
		}
	})
	if code != 6 {
		t.Fatalf("watch 收尾退出码 = %d（stderr: %q），期望 6（B3-T3 信号退出语义）", code, stderr)
	}
	if _, del := cnt.loads(); del != 1 {
		t.Errorf("DELETE 到达次数 = %d，期望 1（信号收尾注销恰一次）", del)
	}
	if strings.Contains(stdout, "HIT") {
		t.Errorf("空信箱 watch 输出含 HIT:\n%s", stdout)
	}
}

// TestIntegrationB3DeadSentinelZeroLoss 段 3（AC10.3）：kill 模拟=直插哨兵残留行
// （不经 DELETE 注销——kill -9 无机会收尾的服务端残照，b3-spec §六「哨兵残留行
// 不算死数据」）→ 时钟推进 16s（>15s 失活阈）→ status 该信箱 sentinel=dead*；
// B2 poll 命令复验零丢失：残留死哨兵不影响消息可见性，executor poll 全量拉到
// 种下两条（位点预置 0 → pending=2），位点不推进（AC10.4 幂等重拉语义另一面）。
func TestIntegrationB3DeadSentinelZeroLoss(t *testing.T) {
	st, addr, _ := startB3IntegrationServer(t)
	var projID, colID int64
	if err := st.DB.QueryRow(
		`SELECT p.id, c.id FROM projects p JOIN columns c ON c.project_id = p.id
		 WHERE p.code = ? AND c.code = ?`, regAuthProject, regAuthColumn).Scan(&projID, &colID); err != nil {
		t.Fatalf("定位引导域 id 失败: %v", err)
	}

	// kill 残留照：watch 会话行+哨兵行（last_ping_at=t0）直插，零 DELETE。
	sess := statusSeedSession(t, st, projID, colID, "watch-sess", "executor", b3T0)
	statusSeedSentinel(t, st, sess, colID, "executor", b3T0)
	seq1 := statusSeedMessage(t, st, colID, "direct", "executor", "controller-A@c01", "normal", b3T0)
	seq2 := statusSeedMessage(t, st, colID, "direct", "executor", "controller-A@c01", "normal", b3T0)

	// 时钟推进 16s（>15s 失活阈、<900s 失联阈——dead 而不 LOST，单变量归因）。
	injectStatusClock(t, b3TPlus(16))

	// status sentinel=dead*（§4.2 异常标记列；token 断言对齐 TestGlobalMode 口径）。
	stdout := b3StatusGlobal(t, addr)
	assertStatusTokens(t, "AC10.3 dead 信箱行",
		findLineTokens(t, stdout, "management c01 executor"),
		"management c01 executor pending=2 pos=0 sentinel=dead* -")

	// B2 poll 命令复验零丢失：两条种消息全量可得。
	pollOut, _ := runWantCode(t, "AC10.3 poll 复验", 0, func() int {
		return runPoll(context.Background(), b3WatchArgs(), addr)
	})
	assertContains(t, "AC10.3 poll 零丢失", pollOut,
		fmt.Sprintf("[%d]", seq1), fmt.Sprintf("[%d]", seq2), "# pending=2")
}

// findLineTokens 在 status 全局输出中定位含 anchor 前缀分词的首行（栏目行断言的
// 定位面——行序随项目/栏目自增 id 稳定但显式定位更抗夹具演化）。
func findLineTokens(t *testing.T, stdout, anchor string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if tok := statusTokenJoin(line); strings.HasPrefix(tok, anchor) {
			return tok
		}
	}
	t.Fatalf("输出中未找到 %q 行:\n%s", anchor, stdout)
	return ""
}

// TestIntegrationB3AckSuppressesHit 段 4（AC10.4）：watch 首轮命中（send 的消息被
// 报告、哨兵注销）→ executor 一键 ack 位点推进 → watch 二轮重注册再轮询：已确认
// 消息不再命中（空轮持续），信号收尾退 6、stdout 零 HIT。位点推进的权威断言在
// ack 输出行（OK position=<seq>），此处全链复验其 watch 侧效果。
func TestIntegrationB3AckSuppressesHit(t *testing.T) {
	_, addr, cnt := startB3IntegrationServer(t)
	ctx := context.Background()
	sigCh := make(chan os.Signal, 1)
	root := t.TempDir()
	seq, _ := sendOK(t, addr, root, "--to-role", "executor", "--body", "待确认指令")

	// 首轮 watch：命中退 0（同段 1 形态压缩——首轮 poll 即命中，无需等 send）。
	var code int
	stdout1, stderr1 := captureStdoutStderr(t, func() {
		codeCh := make(chan int, 1)
		go func() {
			codeCh <- runWatchCmd(ctx, append(b3WatchArgs(), "--interval", "100ms"), addr, sigCh)
		}()
		select {
		case code = <-codeCh:
		case <-time.After(2 * time.Second):
			t.Error("首轮 watch 2s 内未命中退出")
			return
		}
	})
	if code != 0 {
		t.Fatalf("首轮 watch 退出码 = %d（stderr: %q），期望 0", code, stderr1)
	}
	if want := fmt.Sprintf("HIT seq=%d level=normal", seq); !strings.Contains(stdout1, want) {
		t.Fatalf("首轮 watch 输出缺 %q：\n%s", want, stdout1)
	}

	// ack：executor 一键 ack → 位点推进到最大可见（=seq）。输出行权威断言。
	ackOut, _ := runWantCode(t, "executor 一键 ack", 0, func() int {
		return runAck(ctx, b3WatchArgs(), addr)
	})
	if want := fmt.Sprintf("OK position=%d\n", seq); ackOut != want {
		t.Fatalf("ack 输出 = %q，期望 %q", ackOut, want)
	}

	// 二轮 watch：重注册（首轮已注销）再轮询——已确认消息不再命中：等 2 轮空
	// poll 后仍运行，SIGTERM 收尾退 6、零 HIT。
	stdout2, stderr2 := captureStdoutStderr(t, func() {
		codeCh := make(chan int, 1)
		go func() {
			codeCh <- runWatchCmd(ctx, append(b3WatchArgs(), "--interval", "100ms"), addr, sigCh)
		}()
		b3WaitForCond(t, 5*time.Second, "二轮 poll 轮数达 2", func() bool {
			poll, _ := cnt.loads()
			return poll >= 2
		})
		select {
		case c := <-codeCh:
			if c != 6 {
				t.Errorf("二轮 watch 提前退出 %d（期望仅信号收尾退 6）", c)
			}
			return
		default:
		}
		sigCh <- syscall.SIGTERM
		select {
		case code = <-codeCh:
		case <-time.After(3 * time.Second):
			t.Error("二轮 SIGTERM 后 3s 内未收尾")
			return
		}
	})
	if code != 6 {
		t.Fatalf("二轮 watch 退出码 = %d（stderr: %q），期望 6", code, stderr2)
	}
	if strings.Contains(stdout2, "HIT") {
		t.Errorf("AC10.4 ack 后 watch 仍报已确认消息:\n%s", stdout2)
	}
}

// TestIntegrationB3ZeroContextGlobalDrill 段 5（AC11.4/V3 演练）：零上下文一条
// `status --global` 输出可答恢复四问（V3 验收演练形态）——
// ①各栏目状态（栏目行 sentinel 列 alive/dead* + sessions 行 OK/LOST）；
// ②待消费多少（pending=N 列）；
// ③被什么阻断（block 未回执清单段：计数+target 三段式+发送方+时刻）；
// ④当前允许阶段（窗标记 S4:WAITING(HH:MM-HH:MM)，未配置栏目 `-`=不受限）。
// 耗时判据 <5s（单请求聚合全程，b3-plan B3-8 原文口径）。夹具复用
// seedStatusFixtures（B3-7 §4.2 样例等价构造——断言行取关键行而非全 14 行逐字，
// 全量逐字对拍已在 TestGlobalMode 锁定，此处验「四问可答」+延迟面）。
func TestIntegrationB3ZeroContextGlobalDrill(t *testing.T) {
	addr, st := startResTestServer(t)
	_, _ = seedStatusFixtures(t, st)
	injectStatusClock(t, statusNow)
	injectWindowClock(t, "10:00") // S4 23:00-09:00 @10:00 → WAITING（夹具冻结口径）

	start := time.Now()
	stdout, stderr := runWantCode(t, "零上下文 status --global", 0, func() int {
		return runStatusGlobal(context.Background(), addr)
	})
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Errorf("status --global 耗时 %v ≥ 5s（AC11.4 判据失守）", elapsed)
	}
	if stderr != "" {
		t.Errorf("成功不应有 stderr 输出，实际 %q", stderr)
	}

	// 四问①+②：栏目行含角色/pending/pos/sentinel 全列（两条关键栏目行 token 断言，
	// alive/dead* 两态各一——各栏目「状态」与「待消费」同列可答）。
	assertStatusTokens(t, "四问①② alive 栏目行",
		findLineTokens(t, stdout, "proj-a 05 controller"),
		"proj-a 05 controller pending=3 pos=1 sentinel=alive S4:WAITING(23:00-09:00)")
	assertStatusTokens(t, "四问①② dead 栏目行",
		findLineTokens(t, stdout, "proj-a 06 controller"),
		"proj-a 06 controller pending=1 pos=5 sentinel=dead* -")
	// 四问① 会话健康面：sessions 行 LOST 标记（谁失联可答）。
	if tok := statusTokenJoin(findLineTokens(t, stdout, "sessions:")); !strings.Contains(tok, "executor-B@05 LOST(32m)*") {
		t.Errorf("sessions 行缺 LOST 标记: %q", tok)
	}
	// 四问③：阻断未回执段（计数+两清单行，清单位置：标题行后随缩进行）。
	assertContains(t, "四问③ block 计数", stdout, "block 未回执: 2 条")
	assertContains(t, "四问③ target 三段式", stdout, "proj-a/05/executor", "proj-a/06/executor")
	// 四问④：当前允许阶段——窗标记列（WAITING=窗内挂起）与未配置 `-`（不受限）。
	assertContains(t, "四问④ 窗标记", stdout, "S4:WAITING(23:00-09:00)")
	if !strings.Contains(statusTokenJoin(stdout), "proj-b 01 controller pending=0 pos=0 sentinel=none -") {
		t.Errorf("四问④ 未配置窗栏目标记 `-` 缺失:\n%s", stdout)
	}
	// 首行 generated_at=注入钟（零上下文=输出自带时刻，恢复现场第一要素）。
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	assertStatusTokens(t, "首行 generated_at", lines[0], "aiteam status "+statusNow)
}

// TestIntegrationB3HeartbeatAdvanceThenLost 段 6（AC12.1/12.2）：watch 循环期会话
// 心跳持续前进（#15 每轮 poll 经中间件+双续期两路刷新 sessions.last_seen_at=注入
// 钟——推进注入钟后下一轮 poll 库值跟随前进）→ 停止活动（SIGTERM 收尾）+时钟推进
// 920s（>默认失联阈 900s）→ status 该会话 LOST(15m)*（AC12.2 端到端，§4.2 样例
// 形态；身份隔离见 b3WatchArgs 注——status 请求不刷 watch 会话心跳）。
func TestIntegrationB3HeartbeatAdvanceThenLost(t *testing.T) {
	st, addr, cnt := startB3IntegrationServer(t)
	ctx := context.Background()
	sigCh := make(chan os.Signal, 1)
	injectStatusClock(t, b3T0)

	var code int
	_, stderr := captureStdoutStderr(t, func() {
		codeCh := make(chan int, 1)
		go func() {
			codeCh <- runWatchCmd(ctx, append(b3WatchArgs(), "--interval", "100ms"), addr, sigCh)
		}()
		// AC12.1 前半：注册落行（中间件 upsert，last_seen=t0）。
		b3WaitForCond(t, 5*time.Second, "watch-sess 会话行落库", func() bool {
			return b3SessionLastSeen(t, st, "watch-sess") == b3T0
		})
		// 推进注入钟 → 下一轮 poll 双路刷新 → last_seen 跟随前进（持续前进判据：
		// 值从 t0 变 t0+8s，非注册时刻的一次性定格）。
		injectStatusClock(t, b3TPlus(8))
		b3WaitForCond(t, 5*time.Second, "last_seen_at 推进至 t0+8s", func() bool {
			return b3SessionLastSeen(t, st, "watch-sess") == b3TPlus(8)
		})
		// 停止活动：SIGTERM 收尾（此后无人再刷 watch-sess 心跳）。
		sigCh <- syscall.SIGTERM
		select {
		case code = <-codeCh:
		case <-time.After(3 * time.Second):
			t.Error("SIGTERM 后 watch 3s 内未收尾退出")
			return
		}
	})
	if code != 6 {
		t.Fatalf("watch 收尾退出码 = %d（stderr: %q），期望 6", code, stderr)
	}
	if _, del := cnt.loads(); del != 1 {
		t.Errorf("DELETE 到达次数 = %d，期望 1（AC12.1 正常收尾注销）", del)
	}

	// AC12.2：停止活动+时钟推进 920s（>900s 默认失联阈）→ LOST(15m)*。
	injectStatusClock(t, b3TPlus(920))
	stdout := b3StatusGlobal(t, addr)
	if tok := statusTokenJoin(findLineTokens(t, stdout, "sessions:")); !strings.Contains(tok, "watch-sess@c01 LOST(15m)*") {
		t.Errorf("AC12.2 sessions 行缺 watch-sess LOST(15m)*: %q\n全文:\n%s", tok, stdout)
	}
}

// TestIntegrationB3PendingTracksSendAck 段 7（AC5.1 联动）：send → status 该栏目
// executor 信箱 pending 0→1（latest 同步出现）→ 一键 ack 位点推进 → pending 归零
// （latest 仍在=含已消费口径）。单栏目模式四项渲染的消费联动面。
func TestIntegrationB3PendingTracksSendAck(t *testing.T) {
	_, addr, _ := startB3IntegrationServer(t)
	ctx := context.Background()
	root := t.TempDir()

	// 基线：executor 信箱 pending=0 latest=-（预置位点行即信箱行出现面）。
	stdout, _ := runWantCode(t, "基线 status", 0, func() int {
		return runStatus(ctx, regIdentityArgs(), addr)
	})
	if tok := statusTokenJoin(findLineTokens(t, stdout, "management c01 executor")); tok != "management c01 executor pending=0 pos=0 latest=- sentinel=none -" {
		t.Errorf("基线 executor 行 = %q，期望 pending=0 latest=-", tok)
	}

	// send → pending+1 且 latest 出现（ts 为服务端真实墙钟，不可预测——断言
	// token 前缀序列到 level 为止）。
	seq, _ := sendOK(t, addr, root, "--to-role", "executor", "--body", "联动计数消息")
	stdout, _ = runWantCode(t, "send 后 status", 0, func() int {
		return runStatus(ctx, regIdentityArgs(), addr)
	})
	if tok := statusTokenJoin(findLineTokens(t, stdout, "management c01 executor")); !strings.HasPrefix(tok,
		fmt.Sprintf("management c01 executor pending=1 pos=0 latest=#%d normal", seq)) {
		t.Errorf("send 后 executor 行 = %q，期望 pending=1 latest=#%d", tok, seq)
	}

	// ack → pending 归零、pos=seq、latest 仍在（含已消费）。
	ackOut, _ := runWantCode(t, "一键 ack", 0, func() int {
		return runAck(ctx, b3WatchArgs(), addr)
	})
	if want := fmt.Sprintf("OK position=%d\n", seq); ackOut != want {
		t.Fatalf("ack 输出 = %q，期望 %q", ackOut, want)
	}
	stdout, _ = runWantCode(t, "ack 后 status", 0, func() int {
		return runStatus(ctx, regIdentityArgs(), addr)
	})
	if tok := statusTokenJoin(findLineTokens(t, stdout, "management c01 executor")); !strings.HasPrefix(tok,
		fmt.Sprintf("management c01 executor pending=0 pos=%d latest=#%d normal", seq, seq)) {
		t.Errorf("ack 后 executor 行 = %q，期望 pending=0 pos=%d latest=#%d", tok, seq, seq)
	}
}
