package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aiteam/internal/config"
	"aiteam/internal/server"
	"aiteam/internal/store"
)

// watch 命令测试（B3-6）：哨兵轮询循环。单元层走 runWatch 纯逻辑依赖注入桩
// （零真实 sleep——sleep/now 全桩化）；集成层 in-process serve + store 直插
// messages 行（AC10.1 等效压缩）。测试基座复用 register_test.go（regIdentityArgs/
// captureStdoutStderr/runWantCode/closedServerAddr 与引导域夹具常量）。

// watchStub runWatch 依赖桩（纯逻辑层）：pollScript 决定第 n 轮（n 从 1 起）poll
// 返回值；onSleep 在第 n 次 sleep 时被调（推进 fake 时钟/取消 ctx 模拟信号）；
// sleep 返回值取 ctx.Err()——ctx 取消即模拟「睡醒发现收尾信号」，与生产 sleep
// 实现的退出语义一致（Background ctx 恒 nil 不干扰非收尾用例）。now 返回 fakeNow。
type watchStub struct {
	pollScript func(n int) ([]watchHit, error)
	onSleep    func()
	fakeNow    time.Time

	pollN     int
	sleepN    int
	delN      int
	hookCalls []hookCall // b8-W1：runHook 桩捕获快照（depsWithHook 注入面）
	order     []string   // b8-W1：hook/del 执行序（钩子在 finish 前的执行点锚）
	out       bytes.Buffer
	errOut    bytes.Buffer // b8-W1：钩子提示面（deps.errOut 注入）
}

// deps 生成注入依赖集（ctx 必须与传给 runWatch 的同一个——sleep 桩返回其 Err()
// 模拟生产 sleep 的 ctx 取消退出语义；interval/maxWait/asJSON 按用例给定）。
func (s *watchStub) deps(ctx context.Context, interval, maxWait time.Duration, asJSON bool) watchDeps {
	return watchDeps{
		poll: func(context.Context) ([]watchHit, error) {
			s.pollN++
			return s.pollScript(s.pollN)
		},
		del: func(context.Context) error {
			s.delN++
			s.order = append(s.order, "del")
			return nil
		},
		sleep: func(context.Context, time.Duration) error {
			s.sleepN++
			if s.onSleep != nil {
				s.onSleep()
			}
			return ctx.Err()
		},
		now:      func() time.Time { return s.fakeNow },
		interval: interval,
		maxWait:  maxWait,
		asJSON:   asJSON,
		out:      &s.out,
	}
}

// depsWithHook deps 的 b8-W1 钩子注入扩展：onHit=--on-hit 命令行；hook=钩子行为
// 桩（nil=纯记录桩——捕获 cmdLine+env 进 hookCalls 并记 order 后成功返回）；errOut
// 接 s.errOut（钩子失败/超时提示的捕获面）。
func (s *watchStub) depsWithHook(ctx context.Context, interval, maxWait time.Duration, asJSON bool, onHit string, hook func(context.Context, string, []string) error) watchDeps {
	d := s.deps(ctx, interval, maxWait, asJSON)
	d.onHit = onHit
	d.errOut = &s.errOut
	d.runHook = func(hctx context.Context, cmdLine string, env []string) error {
		s.hookCalls = append(s.hookCalls, hookCall{cmdLine: cmdLine, env: env})
		s.order = append(s.order, "hook")
		if hook != nil {
			return hook(hctx, cmdLine, env)
		}
		return nil
	}
	return d
}

// TestWatchHit 命中路径：第 3 轮 poll 返回 hits → 逐 hit 输出
// `HIT seq=.. level=.. kind=.. target=..`（b7-W3 逐字格式）→ DELETE 恰 1 次 →
// 退 0；空轮期间 sleep 恰 2 次（B3-6 用例 1）。
func TestWatchHit(t *testing.T) {
	s := &watchStub{pollScript: func(n int) ([]watchHit, error) {
		if n < 3 {
			return []watchHit{}, nil
		}
		return []watchHit{{Seq: 885, Level: "important", Kind: "direct", TargetRole: "controller"}}, nil
	}}
	code := runWatch(context.Background(), s.deps(context.Background(), 5*time.Second, 0, false))
	if code != 0 {
		t.Errorf("命中退出码 = %d，期望 0", code)
	}
	if got, want := s.out.String(), "HIT seq=885 level=important kind=direct target=controller\n"; got != want {
		t.Errorf("命中输出 = %q，期望恰 %q", got, want)
	}
	if s.delN != 1 {
		t.Errorf("DELETE 调用数 = %d，期望 1（命中后注销）", s.delN)
	}
	if s.sleepN != 2 {
		t.Errorf("sleep 调用数 = %d，期望 2（前两轮空轮）", s.sleepN)
	}
}

// TestWatchHitKindTargetRender b7-W3 HIT 行逐字对拍（AC4 三 kind 标注齐+receipt
// 同面）：direct→target=角色值；chat/receipt→target=会话对话流；bus→target=
// 栏目广播仅总控（watchHitTarget 渲染规则经 report 文本行端到端）。
func TestWatchHitKindTargetRender(t *testing.T) {
	s := &watchStub{pollScript: func(int) ([]watchHit, error) {
		return []watchHit{
			{Seq: 1, Level: "important", Kind: "direct", TargetRole: "executor_A"},
			{Seq: 2, Level: "normal", Kind: "chat", TargetRole: ""},
			{Seq: 3, Level: "normal", Kind: "receipt", TargetRole: ""},
			{Seq: 4, Level: "important", Kind: "bus", TargetRole: ""},
		}, nil
	}}
	code := runWatch(context.Background(), s.deps(context.Background(), 5*time.Second, 0, false))
	if code != 0 {
		t.Fatalf("命中退出码 = %d，期望 0", code)
	}
	want := "HIT seq=1 level=important kind=direct target=executor_A\n" +
		"HIT seq=2 level=normal kind=chat target=会话对话流\n" +
		"HIT seq=3 level=normal kind=receipt target=会话对话流\n" +
		"HIT seq=4 level=important kind=bus target=栏目广播仅总控\n"
	if got := s.out.String(); got != want {
		t.Errorf("HIT 行逐字对拍失败\n得到: %q\n期望: %q", got, want)
	}
}

// TestWatchHitJSONKindTargetKeys b7-W3 --json 增键（AC4）：hits 元素 additive 携
// kind/target_role 结构值直出（target 渲染归文本行，json 侧不做字面替换）。
func TestWatchHitJSONKindTargetKeys(t *testing.T) {
	s := &watchStub{pollScript: func(int) ([]watchHit, error) {
		return []watchHit{{Seq: 9, Level: "block", Kind: "direct", TargetRole: "executor_A"}}, nil
	}}
	code := runWatch(context.Background(), s.deps(context.Background(), 5*time.Second, 0, true))
	if code != 0 {
		t.Fatalf("命中退出码 = %d，期望 0", code)
	}
	if got, want := s.out.String(), `{"hits":[{"seq":9,"level":"block","kind":"direct","target_role":"executor_A"}]}`+"\n"; got != want {
		t.Errorf("--json 命中输出 = %q，期望恰 %q", got, want)
	}
}

// TestWatchIdle 空轮不退出不输出（AC10.2 等效判据）：poll 恒空轮，第 50 轮后
// 取消 ctx（模拟集成收尾）→ 期间零 HIT/JSON 输出，sleep 与 poll 同轮次；
// 收尾路径尽力 DELETE 后退 6（ctx 取消与信号同口径，B3-T3）。
func TestWatchIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &watchStub{pollScript: func(int) ([]watchHit, error) { return []watchHit{}, nil }}
	s.onSleep = func() {
		if s.sleepN >= 50 {
			cancel()
		}
	}
	code := runWatch(ctx, s.deps(ctx, 5*time.Second, 0, false))
	if got := s.out.String(); strings.Contains(got, "HIT") || strings.Contains(got, "hits") {
		t.Errorf("空轮输出 = %q，期望无任何命中输出", got)
	}
	if s.pollN != 50 {
		t.Errorf("poll 轮次 = %d，期望 50（第 50 轮收尾前不退出）", s.pollN)
	}
	if s.sleepN != 50 {
		t.Errorf("sleep 次数 = %d，期望 50（与 poll 同节奏）", s.sleepN)
	}
	if code != 6 {
		t.Errorf("ctx 取消收尾退出码 = %d，期望 6（收尾路径）", code)
	}
	if s.delN != 1 {
		t.Errorf("收尾 DELETE 次数 = %d，期望 1", s.delN)
	}
}

// TestWatchTimeout --max-wait 到时（B3-T3 口径：先 DELETE 再退 6）：注入时钟
// 每轮 sleep 推进 30s，max-wait=30s → 第 2 轮 poll 后总等待 ≥30s → DELETE 恰 1 次
// 先于退出，退出码 6，无命中输出（B3-6 用例 3）。
func TestWatchTimeout(t *testing.T) {
	s := &watchStub{
		pollScript: func(int) ([]watchHit, error) { return []watchHit{}, nil },
		fakeNow:    time.Unix(0, 0),
	}
	s.onSleep = func() { s.fakeNow = s.fakeNow.Add(30 * time.Second) }
	code := runWatch(context.Background(), s.deps(context.Background(), 30*time.Second, 30*time.Second, false))
	if code != 6 {
		t.Errorf("超时退出码 = %d，期望 6（B3-T3）", code)
	}
	if s.delN != 1 {
		t.Errorf("超时 DELETE 次数 = %d，期望 1（先 DELETE 再退出）", s.delN)
	}
	if got := s.out.String(); got != "" {
		t.Errorf("超时输出 = %q，期望空", got)
	}
	if s.pollN != 2 {
		t.Errorf("poll 轮次 = %d，期望 2（首轮 elapsed=0 未达，睡后第 2 轮判定到时）", s.pollN)
	}
}

// TestWatchSignal SIGINT 收尾（桩层，语义=收尾逻辑调用 del 恰 1 次）：sleep 桩首次
// 触发即模拟信号处理函数 cancel ctx（生产命令壳 signal.Notify → cancel 同型），
// 循环经收尾路径 DELETE 恰 1 次后退 6。DELETE 是否真到达服务端由集成层
// TestWatchSignalTerminates 验证（收尾独立 ctx 的存在性桩层看不出来）。
func TestWatchSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &watchStub{pollScript: func(int) ([]watchHit, error) { return []watchHit{}, nil }}
	s.onSleep = cancel // 模拟 SIGINT → cancel
	code := runWatch(ctx, s.deps(ctx, 5*time.Second, 0, false))
	if code != 6 {
		t.Errorf("信号收尾退出码 = %d，期望 6", code)
	}
	if s.delN != 1 {
		t.Errorf("信号收尾 DELETE 次数 = %d，期望 1（尽力注销）", s.delN)
	}
	if s.pollN != 1 {
		t.Errorf("信号收尾 poll 轮次 = %d，期望 1（首轮空轮后睡中收尾）", s.pollN)
	}
}

// TestWatchFlagValidation 缺身份参 → 本地校验退 2 不发请求（§4.7）：serverOverride
// 指向已关闭端口，误发请求将得退出 3，实测 2 即证明本地拦截（B3-6 用例 5）。
func TestWatchFlagValidation(t *testing.T) {
	dead := closedServerAddr(t)
	ctx := context.Background()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"全缺", nil, "--project"},
		{"缺 --session", []string{"--project", regAuthProject, "--column", regAuthColumn, "--role", regAuthRole}, "--session"},
		{"缺 --role", []string{"--project", regAuthProject, "--column", regAuthColumn, "--session", regAuthSession}, "--role"},
	}
	for _, tc := range cases {
		_, stderr := runWantCode(t, "watch 缺身份 "+tc.name, 2, func() int {
			return runWatchCmd(ctx, tc.args, dead, nil)
		})
		if !strings.Contains(stderr, tc.want) {
			t.Errorf("watch 缺身份 %s stderr = %q，期望指明 %s", tc.name, stderr, tc.want)
		}
	}
}

// startWatchTestServer 集成测试环境：临时真文件库 + 完整 server + httptest +
// 引导域夹具（与 register_test.go startRegTestServer 同款，另返回 store 句柄供
// messages 直插与库内断言）。外层包 DELETE recorder：记录 DELETE /api/v1/sentinels/
// 到达服务端的次数（指针返回；读侧以 codeCh 收码后的 happens-before 为准——
// watch goroutine 内 DELETE 完成后才发送退出码）。
// 变参 pollHits（b8 补强）：传入非 nil *int 时逐次计数 POST .../poll 到达——供
// 「等首轮 poll 真到达再动作」的确定性同步（注册行落库≠注册响应已回客户端，其间
// 收信号走注册中断分叉）；缺省不传零开销，既有调用点不变。
func startWatchTestServer(t *testing.T, pollHits ...*int) (*store.Store, string, *int) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "watch-test.db"))
	if err != nil {
		t.Fatalf("打开临时 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	delHits := new(int)
	inner := server.NewServer(st, config.Default(), server.Version)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/sentinels/") {
			*delHits++
		}
		if len(pollHits) > 0 && pollHits[0] != nil &&
			r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/poll") {
			*pollHits[0]++
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
	return st, ts.URL, delHits
}

// insertWatchMailboxMessage 直插一条 direct 消息到 (project,column)+targetRole
// 信箱（messages 夹具直插，status_test.go fixtureMessage 同款 SQL；sender/
// target session id 均为 0=弱关联无 FK，无需 seed 会话）。返回 seq 供断言。
func insertWatchMailboxMessage(t *testing.T, st *store.Store, projID, colID int64, targetRole, level string) int64 {
	t.Helper()
	res, err := st.DB.Exec(
		`INSERT INTO messages (project_id, column_id, kind, target_role, target_session_id,
		   sender_session_id, sender_label, level, body, created_at)
		 VALUES (?, ?, 'direct', ?, 0, 0, 'watch-test', ?, 'watch 集成夹具', ?)`,
		projID, colID, targetRole, level, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("直插 direct 消息夹具失败: %v", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取消息 seq 失败: %v", err)
	}
	return seq
}

// locateGuideMailboxIDs 查引导域（regAuthProject/regAuthColumn）project/column
// id 对（watch 集成测试定位 messages 夹具插入坐标共用，质量审查修 4 收敛）。
func locateGuideMailboxIDs(t *testing.T, st *store.Store) (projID, colID int64) {
	t.Helper()
	if err := st.DB.QueryRow(
		`SELECT p.id, c.id FROM projects p JOIN columns c ON c.project_id = p.id
		 WHERE p.code = ? AND c.code = ?`, regAuthProject, regAuthColumn).Scan(&projID, &colID); err != nil {
		t.Fatalf("定位引导域 id 失败: %v", err)
	}
	return projID, colID
}

// runWatchAsync watch 阻塞循环放 goroutine、主 goroutine 等退出码的集成测试同步
// 骨架（质量审查修 4 收敛——2s 断言窗为超时保护非节奏 sleep；须在
// captureStdoutStderr 的 fn 内调用，捕获面与超时窗才作用于同一轮）。
func runWatchAsync(t *testing.T, args []string, addr string) int {
	t.Helper()
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runWatchCmd(context.Background(), args, addr, nil)
	}()
	select {
	case code := <-codeCh:
		return code
	case <-time.After(2 * time.Second):
		t.Error("watch 2s 内未退出")
		return -1
	}
}

// TestWatchIntegrationHit 集成层（AC10.1 等效压缩）：store 直插一条 direct 消息
// （目标信箱坐标与 watch 身份一致）→ in-process serve 上真注册/真轮询/真注销。
// D2 余量论证：位点无行 COALESCE 0 → 首轮 poll 即命中，注册→首轮 poll→DELETE
// 三请求全程远小于 2s 断言窗，--interval 100ms 仅为万一进入多轮时的节奏兜底。
// 第二段同消息再跑 --json：poll 纯读不推进位点（AC10.4），哨兵已注销可重新注册，
// 命中可复现——JSON 摘要形态 {"hits":[{"seq":..,"level":..}]} 锁定（B3-6 用例 6）。
func TestWatchIntegrationHit(t *testing.T) {
	st, addr, _ := startWatchTestServer(t)
	projID, colID := locateGuideMailboxIDs(t, st)
	seq := insertWatchMailboxMessage(t, st, projID, colID, regAuthRole, "important")

	// 第一段：文本输出（同步骨架见 runWatchAsync）。
	var code int
	stdout, stderr := captureStdoutStderr(t, func() {
		code = runWatchAsync(t, append(regIdentityArgs(), "--interval", "100ms"), addr)
	})
	if code != 0 {
		t.Fatalf("watch 集成退出码 = %d（stderr: %q），期望 0", code, stderr)
	}
	// b7-W3 锚更新：HIT 行扩为 `HIT seq=<n> level=<l> kind=<k> target=<t>`，
	// direct 分支 target=目标角色值（夹具 direct→regAuthRole=controller）。
	if want := fmt.Sprintf("HIT seq=%d level=important kind=direct target=%s\n", seq, regAuthRole); !strings.Contains(stdout, want) {
		t.Errorf("watch 集成输出 = %q，期望含 %q", stdout, want)
	}
	assertNoSentinelRows(t, st)

	// 第二段：--json 命中摘要（同消息可复现命中，见函数头注）。
	stdout, stderr = captureStdoutStderr(t, func() {
		code = runWatchAsync(t, append(regIdentityArgs(), "--interval", "100ms", "--json"), addr)
	})
	if code != 0 {
		t.Fatalf("watch --json 退出码 = %d（stderr: %q），期望 0", code, stderr)
	}
	// b7-W3 锚更新：--json hits 元素 additive 增 kind/target_role 两键（结构值
	// 直出，target 渲染归文本行）。
	want := fmt.Sprintf(`{"hits":[{"seq":%d,"level":"important","kind":"direct","target_role":%q}]}`, seq, regAuthRole)
	if !strings.Contains(stdout, want) {
		t.Errorf("watch --json 输出 = %q，期望含 %q", stdout, want)
	}
	assertNoSentinelRows(t, st)
}

// assertNoSentinelRows 断言哨兵表已清空（watch 收尾 DELETE 端到端落实）。
func assertNoSentinelRows(t *testing.T, st *store.Store) {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sentinels`).Scan(&n); err != nil {
		t.Fatalf("查询哨兵行数失败: %v", err)
	}
	if n != 0 {
		t.Errorf("sentinels 行数 = %d，期望 0（watch 收尾已注销）", n)
	}
}

// TestWatchSignalTerminates 集成层信号收尾（审查修复 2——防桩测掩盖真 DELETE）：
// 真进程内 watch 循环收到注入 SIGTERM → 收尾注销的 DELETE 真实到达服务端恰 1 次
// + 退出码 6（B3-T3「尽力 DELETE 后退 6」的端到端验证，收尾独立 ctx 生效的证据：
// 若 DELETE 用已取消 ctx，请求发不出去、delHits 恒 0）。信号经 buffered sigCh 注入
// （runWatchCmd 的 sigCh 参数=测试注入点）：send 落缓冲，无论 runWatchCmd 的
// signal.Notify 是否已挂上都不丢失、不阻塞。
func TestWatchSignalTerminates(t *testing.T) {
	pollHits := new(int)
	st, addr, delHits := startWatchTestServer(t, pollHits)
	ctx := context.Background()
	sigCh := make(chan os.Signal, 1)

	var code int
	stdout, stderr := captureStdoutStderr(t, func() {
		codeCh := make(chan int, 1)
		go func() {
			codeCh <- runWatchCmd(ctx, append(regIdentityArgs(), "--interval", "100ms"), addr, sigCh)
		}()
		// 先等注册完成（sentinels 行出现）、再等首轮 poll 真到达，之后才发信号：
		// 注册行落库≠注册响应已回客户端——其间收信号会取消在途注册请求走「注册
		// 中断」分叉（无 id 可删，delHits 恒 0 退 6），与本用例「循环值守中收信号」
		// 意图不符（该分叉另见 TestWatchSignalDuringRegister；check.sh 全量负载下
		// 曾实测复现）。首轮 poll 到达=注册响应必然已回（poll 属其后的循环轮次），
		// 此后收信号确定性落在循环值守中。轮询短睡是测试同步手段（与 2s 超时窗
		// 同类），非被测逻辑节奏。
		deadline := time.Now().Add(2 * time.Second)
		for {
			var n int
			if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sentinels`).Scan(&n); err == nil && n > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Error("watch 2s 内未完成哨兵注册")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		for *pollHits == 0 {
			if time.Now().After(deadline.Add(2 * time.Second)) {
				t.Error("watch 2s 内未到达首轮 poll")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		sigCh <- syscall.SIGTERM
		select {
		case code = <-codeCh:
		case <-time.After(2 * time.Second):
			t.Error("SIGTERM 后 watch 2s 内未退出")
			return
		}
	})
	if code != 6 {
		t.Fatalf("SIGTERM 收尾退出码 = %d（stderr: %q），期望 6", code, stderr)
	}
	if strings.Contains(stdout, "HIT") || strings.Contains(stdout, "hits") {
		t.Errorf("SIGTERM 收尾输出 = %q，期望无命中输出", stdout)
	}
	if *delHits != 1 {
		t.Errorf("收尾 DELETE 到达服务端次数 = %d，期望 1（独立收尾 ctx 真实发出）", *delHits)
	}
}

// TestWatchSignalDuringRegister 注册期收信号的分叉面：注册请求被信号 cancel 打断
// 的 HTTP 错误是信号面影非网络故障——按 B3-T3 退 6，不误报「服务不可达」（退 3）。
// 伪服务 handler 挂起至请求 ctx 断开（ctx 取消后 hc.Do 即刻失败，测试零真实等待）。
func TestWatchSignalDuringRegister(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // 挂起模拟慢注册；客户端 ctx 取消即刻短路
	}))
	t.Cleanup(ts.Close)
	sigCh := make(chan os.Signal, 1)

	var code int
	_, stderr := captureStdoutStderr(t, func() {
		codeCh := make(chan int, 1)
		go func() {
			codeCh <- runWatchCmd(context.Background(), regIdentityArgs(), ts.URL, sigCh)
		}()
		sigCh <- syscall.SIGTERM
		select {
		case code = <-codeCh:
		case <-time.After(2 * time.Second):
			t.Error("注册期 SIGTERM 后 2s 内未退出")
			return
		}
	})
	if code != 6 {
		t.Errorf("注册期信号退出码 = %d（stderr: %q），期望 6（B3-T3，非误报 3）", code, stderr)
	}
	if strings.Contains(stderr, "服务不可达") {
		t.Errorf("注册期信号 stderr = %q，不应误报服务不可达", stderr)
	}
}

// TestWatchRegisterIdempotentAlive #14 幂等分叉（信箱 #12，审查修复 3）：注册响应
// 200=同坐标已有活哨兵 → 「已有哨兵值班中」退 0，不进轮询循环、不 DELETE（哨兵在
// 别人手里值班，注销是破坏行为——断言伪服务只收到注册这一个请求）。伪服务手写
// {data} 包裹（与 types.WriteData 同形状）；201 新建路径由 TestWatchIntegrationHit
// 端到端覆盖。
func TestWatchRegisterIdempotentAlive(t *testing.T) {
	var regCalls, otherCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/sentinels" {
			regCalls++
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"sentinel_id":7,"interval_sec":5}}`))
			return
		}
		otherCalls++ // poll/DELETE 误发即暴露（分叉语义：不进循环不注销）
	}))
	t.Cleanup(ts.Close)

	stdout, stderr := runWantCode(t, "watch 幂等命中活哨兵", 0, func() int {
		return runWatchCmd(context.Background(), regIdentityArgs(), ts.URL, nil)
	})
	if !strings.Contains(stdout, "已有哨兵值班中") {
		t.Errorf("幂等命中输出 = %q，期望含「已有哨兵值班中」", stdout)
	}
	if regCalls != 1 {
		t.Errorf("注册请求数 = %d，期望 1", regCalls)
	}
	if otherCalls != 0 {
		t.Errorf("poll/DELETE 请求数 = %d，期望 0（幂等命中不进循环不注销）", otherCalls)
	}
	if stderr != "" {
		t.Errorf("幂等命中 stderr = %q，期望空", stderr)
	}
}

// TestWatchIntegrationServerDown 集成层错误面：服务不可达（注册阶段）→ 退 3
// （§4.7 UnreachableError 映射，注册是 watch 第一个网络请求）。
func TestWatchIntegrationServerDown(t *testing.T) {
	dead := closedServerAddr(t)
	_, stderr := runWantCode(t, "watch 服务不可达", 3, func() int {
		return runWatchCmd(context.Background(), regIdentityArgs(), dead, nil)
	})
	if !strings.Contains(stderr, "ERROR: 服务不可达") {
		t.Errorf("watch 不可达 stderr = %q，期望含 `ERROR: 服务不可达`", stderr)
	}
}

// TestWatchRunEntry 导出入口形态：RunWatch 与命令壳同参透传（main.go 挂载面，
// 编译期契约 + 缺身份退 2 烟测）。
func TestWatchRunEntry(t *testing.T) {
	if _, stderr := runWantCode(t, "RunWatch 缺身份", 2, func() int {
		return RunWatch([]string{"--server", "http://127.0.0.1:1"})
	}); !strings.Contains(stderr, "--project") {
		t.Errorf("RunWatch 缺身份 stderr = %q，期望指明 --project", stderr)
	}
}

// ───────────────────────── b8-W1 --on-hit 命中钩子 ─────────────────────────
//
// 桩层：runHook 注入桩捕获 cmdLine+env（零真实子进程）——AC1 执行/env 断言、AC2
// 超时提示面（watch 侧）、AC3 失败面+stdout 逐字对拍、AC6 零钩子锚。真路径（sh -c
// /cmd /c 薄封装：echo 透传/env 注入/同名键遮蔽防线/exit 1 失败/短值注入强杀）见
// TestWatchHookRunnerReal / TestWatchHookRunnerEnvWins / TestWatchHookRealTimeoutKill；
// flag→deps→真子进程端到端见 TestWatchHookIntegrationEchoEnv（stderr 回显捕获面）
// 与 TestWatchHookIntegrationFileDrop（字面落盘，质量审查补测 A）。

// hookCall 钩子执行桩捕获快照：cmdLine+env 原样留档（AC1 env 断言面）。
type hookCall struct {
	cmdLine string
	env     []string
}

// hookEnvValue 取 env 键值（首个 = 切分——AITEAM_HITS_JSON 的 JSON 值即使含 =
// 也不影响键识别；缺键返回空串）。
func hookEnvValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// TestWatchHookHitExec b8-W1 AC1（桩层）：命中后钩子恰执行 1 次、cmdLine 原样透传；
// env 含 AITEAM_HITS_JSON（可反序列化、全量元素 b7 四键 seq/level/kind/target_role
// 直出）+ AITEAM_HIT_SEQ / AITEAM_HIT_LEVEL = 首条便捷键；HIT 行照打 stdout（AI
// 侧宿主唤醒不被钩子耽误）；执行点=report 后 finish 前（order=[hook del]）；rc=0。
func TestWatchHookHitExec(t *testing.T) {
	s := &watchStub{pollScript: func(int) ([]watchHit, error) {
		return []watchHit{
			{Seq: 885, Level: "important", Kind: "direct", TargetRole: "controller"},
			{Seq: 886, Level: "normal", Kind: "chat", TargetRole: ""},
		}, nil
	}}
	code := runWatch(context.Background(),
		s.depsWithHook(context.Background(), 5*time.Second, 0, false, "notify-send 哨兵命中", nil))
	if code != 0 {
		t.Fatalf("命中+钩子退出码 = %d，期望 0（钩子不改 watch 退出码）", code)
	}
	if len(s.hookCalls) != 1 {
		t.Fatalf("钩子执行次数 = %d，期望 1", len(s.hookCalls))
	}
	if got := s.hookCalls[0].cmdLine; got != "notify-send 哨兵命中" {
		t.Errorf("钩子 cmdLine = %q，期望原样透传", got)
	}
	env := s.hookCalls[0].env
	raw := hookEnvValue(env, "AITEAM_HITS_JSON")
	var hits []watchHit
	if err := json.Unmarshal([]byte(raw), &hits); err != nil {
		t.Fatalf("AITEAM_HITS_JSON 不可反序列化: %v（raw=%q）", err, raw)
	}
	if len(hits) != 2 ||
		hits[0].Seq != 885 || hits[0].Level != "important" || hits[0].Kind != "direct" || hits[0].TargetRole != "controller" ||
		hits[1].Seq != 886 || hits[1].Level != "normal" || hits[1].Kind != "chat" {
		t.Errorf("AITEAM_HITS_JSON = %s，期望全量两元素 b7 四键直出", raw)
	}
	if got := hookEnvValue(env, "AITEAM_HIT_SEQ"); got != "885" {
		t.Errorf("AITEAM_HIT_SEQ = %q，期望首条 885", got)
	}
	if got := hookEnvValue(env, "AITEAM_HIT_LEVEL"); got != "important" {
		t.Errorf("AITEAM_HIT_LEVEL = %q，期望首条 important", got)
	}
	if got, want := s.out.String(),
		"HIT seq=885 level=important kind=direct target=controller\n"+
			"HIT seq=886 level=normal kind=chat target=会话对话流\n"; got != want {
		t.Errorf("命中 stdout = %q，期望恰 %q（HIT 行照打不被钩子影响）", got, want)
	}
	if len(s.order) != 2 || s.order[0] != "hook" || s.order[1] != "del" {
		t.Errorf("执行序 = %v，期望 [hook del]（钩子在 report 后 finish 前）", s.order)
	}
}

// TestWatchHookTimeout b8-W1 AC2（桩层·watch 侧面）：钩子挂死→强杀路径以
// errWatchHookTimeout 分类错误模拟（真实强杀分类与短值注入见 TestWatchHookReal-
// TimeoutKill——10s 上限经 timeout 参数注入 200ms 短值，不真等 10s）→ stderr 含
// 超时提示 + HIT 行照打 + finish（del）照走 + rc=0。桩层零真实等待。
func TestWatchHookTimeout(t *testing.T) {
	s := &watchStub{pollScript: func(int) ([]watchHit, error) {
		return []watchHit{{Seq: 1, Level: "block", Kind: "direct", TargetRole: "controller"}}, nil
	}}
	code := runWatch(context.Background(), s.depsWithHook(context.Background(), 5*time.Second, 0, false, "挂死钩子",
		func(context.Context, string, []string) error { return errWatchHookTimeout }))
	if code != 0 {
		t.Errorf("钩子超时退出码 = %d，期望 0（不改 watch 退出码）", code)
	}
	if got := s.errOut.String(); !strings.Contains(got, "命中钩子执行超时已强杀") {
		t.Errorf("钩子超时 stderr = %q，期望含全模板提示「命中钩子执行超时已强杀」（质量审查修 6 全模板锚）", got)
	}
	if got, want := s.out.String(), "HIT seq=1 level=block kind=direct target=controller\n"; got != want {
		t.Errorf("命中 stdout = %q，期望恰 %q（超时路径 HIT 行照打）", got, want)
	}
	if len(s.order) != 2 || s.order[0] != "hook" || s.order[1] != "del" {
		t.Errorf("执行序 = %v，期望 [hook del]（超时后 finish 照走）", s.order)
	}
}

// TestWatchHookFailure b8-W1 AC3（桩层）：钩子失败（桩返回 error）→ stderr 含失败
// 提示（含错误文本）+ rc=0；stdout 逐字与无钩子基线一致（对拍——钩子面零污染
// stdout 管道消费者，b7 同款哲学）。
func TestWatchHookFailure(t *testing.T) {
	hits := []watchHit{{Seq: 885, Level: "important", Kind: "direct", TargetRole: "controller"}}
	base := &watchStub{pollScript: func(int) ([]watchHit, error) { return hits, nil }}
	if code := runWatch(context.Background(), base.deps(context.Background(), 5*time.Second, 0, false)); code != 0 {
		t.Fatalf("无钩子基线退出码 = %d，期望 0", code)
	}
	s := &watchStub{pollScript: func(int) ([]watchHit, error) { return hits, nil }}
	code := runWatch(context.Background(), s.depsWithHook(context.Background(), 5*time.Second, 0, false, "will-fail-hook",
		func(context.Context, string, []string) error { return errors.New("boom: 推送通道不可达") }))
	if code != 0 {
		t.Errorf("钩子失败退出码 = %d，期望 0（不改 watch 退出码）", code)
	}
	if got := s.errOut.String(); !strings.Contains(got, "命中钩子执行失败") || !strings.Contains(got, "boom: 推送通道不可达") {
		t.Errorf("钩子失败 stderr = %q，期望含失败提示与错误文本", got)
	}
	if s.out.String() != base.out.String() {
		t.Errorf("钩子失败 stdout = %q，期望与无钩子基线 %q 逐字一致", s.out.String(), base.out.String())
	}
}

// TestWatchHookAbsent b8-W1 AC6 回归锚（桩层）：无 --on-hit（onHit 空串）——即使
// deps 误注入 runHook 也绝不执行（零钩子零开销），执行序仅 del，stdout 与既有命中
// 路径逐字一致（v0.1 语义零变化）。
func TestWatchHookAbsent(t *testing.T) {
	s := &watchStub{pollScript: func(int) ([]watchHit, error) {
		return []watchHit{{Seq: 4, Level: "normal", Kind: "bus", TargetRole: ""}}, nil
	}}
	d := s.deps(context.Background(), 5*time.Second, 0, false)
	d.runHook = func(context.Context, string, []string) error {
		t.Error("无 --on-hit 时钩子被执行（零钩子零开销失守）")
		return nil
	}
	code := runWatch(context.Background(), d)
	if code != 0 {
		t.Errorf("无钩子命中退出码 = %d，期望 0", code)
	}
	if got, want := s.out.String(), "HIT seq=4 level=normal kind=bus target=栏目广播仅总控\n"; got != want {
		t.Errorf("无钩子命中 stdout = %q，期望恰 %q（既有行为零变化）", got, want)
	}
	if len(s.order) != 1 || s.order[0] != "del" {
		t.Errorf("执行序 = %v，期望仅 [del]（零钩子零开销）", s.order)
	}
}

// TestWatchHookIntegrationEchoEnv b8-W1 AC1 集成面（进程内 httptest + 真二进制
// 路径——flag 解析 → deps 装配 → watchHookRunner 真子进程全链）：--on-hit 命令回显
// AITEAM_HITS_JSON（unix echo $VAR / windows set VAR——零重定向零引号，绕开 cmd /c
// 引号改写雷区），经「钩子自身 stdout 透传 watch stderr」管道落入捕获面，断言其
// 内容可反序列化且含 seq/level/kind/target（env 断言的端到端形态）+ HIT 行照打
// stdout（且 stdout 零 JSON 污染）+ rc=0。溯源注记：spec AC1 原文为「临时文件落盘」
// 形态，等价裁定以 stderr 回显替代；字面落盘用例另见
// TestWatchHookIntegrationFileDrop（质量审查补测 A 补齐）。
func TestWatchHookIntegrationEchoEnv(t *testing.T) {
	st, addr, _ := startWatchTestServer(t)
	projID, colID := locateGuideMailboxIDs(t, st)
	seq := insertWatchMailboxMessage(t, st, projID, colID, regAuthRole, "important")
	var hookCmd string
	if runtime.GOOS == "windows" {
		hookCmd = "set AITEAM_HITS_JSON" // cmd /c set VAR → `AITEAM_HITS_JSON=<值>`
	} else {
		hookCmd = "echo $AITEAM_HITS_JSON" // 紧凑 JSON 无空格，$VAR 展开安全
	}
	args := append(regIdentityArgs(), "--interval", "100ms", "--on-hit", hookCmd)

	var code int
	stdout, stderr := captureStdoutStderr(t, func() {
		code = runWatchAsync(t, args, addr)
	})
	if code != 0 {
		t.Fatalf("watch --on-hit 集成退出码 = %d（stderr: %q），期望 0", code, stderr)
	}
	if want := fmt.Sprintf("HIT seq=%d level=important kind=direct target=%s\n", seq, regAuthRole); !strings.Contains(stdout, want) {
		t.Errorf("watch --on-hit stdout = %q，期望含 %q（HIT 行照打）", stdout, want)
	}
	if strings.Contains(stdout, "AITEAM_HITS_JSON") {
		t.Errorf("watch --on-hit stdout = %q，不应含钩子输出（stdout 管道零污染）", stdout)
	}
	marker := "AITEAM_HITS_JSON="
	idx := strings.Index(stderr, marker)
	if idx < 0 {
		t.Fatalf("watch --on-hit stderr = %q，期望含钩子回显的 %q（钩子 stdout 透传 stderr）", stderr, marker)
	}
	rest := stderr[idx+len(marker):]
	start, end := strings.IndexByte(rest, '['), strings.LastIndexByte(rest, ']')
	if start < 0 || end <= start {
		t.Fatalf("钩子回显 AITEAM_HITS_JSON 无 JSON 数组体: %q", rest)
	}
	raw := rest[start : end+1] // 截取完整 JSON 数组（含括号，剥尾部回车/后续输出）
	var hits []watchHit
	if err := json.Unmarshal([]byte(raw), &hits); err != nil {
		t.Fatalf("钩子回显的 AITEAM_HITS_JSON 不可反序列化: %v（raw=%q）", err, raw)
	}
	if len(hits) != 1 || hits[0].Seq != seq || hits[0].Level != "important" ||
		hits[0].Kind != "direct" || hits[0].TargetRole != regAuthRole {
		t.Errorf("钩子回显 AITEAM_HITS_JSON = %s，期望单元素 seq=%d level=important kind=direct target_role=%s", raw, seq, regAuthRole)
	}
	assertNoSentinelRows(t, st)
}

// TestWatchHookRunnerReal b8-W1 runHook 默认实现真子进程路径（薄封装覆盖面）：
// echo 回显经 sh -c/cmd /c 透传到 errW（钩子自身 stdout 并入 watch stderr）；env
// 注入真子进程（AITEAM_HIT_SEQ 经 GOOS 分支 echo 语法取值）；exit 1 → 非 nil 且
// 非超时类错误（失败分类不误判）。
func TestWatchHookRunnerReal(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过真子进程用例")
	}
	var out bytes.Buffer
	run := watchHookRunner(&out, 10*time.Second)

	if err := run(context.Background(), "echo hook-passthrough-marker", nil); err != nil {
		t.Fatalf("真钩子 echo 执行失败: %v", err)
	}
	if !strings.Contains(out.String(), "hook-passthrough-marker") {
		t.Errorf("钩子输出透传 = %q，期望含 marker（钩子 stdout 并入 errW）", out.String())
	}

	out.Reset()
	envCmd := "echo $AITEAM_HIT_SEQ"
	if runtime.GOOS == "windows" {
		envCmd = "echo %AITEAM_HIT_SEQ%"
	}
	if err := run(context.Background(), envCmd, []string{"AITEAM_HIT_SEQ=42"}); err != nil {
		t.Fatalf("真钩子 env 回显执行失败: %v", err)
	}
	if !strings.Contains(out.String(), "42") {
		t.Errorf("钩子 env 注入回显 = %q，期望含 42（AITEAM_HIT_SEQ 注入真子进程）", out.String())
	}

	err := run(context.Background(), "exit 1", nil)
	if err == nil {
		t.Fatal("exit 1 钩子应返回错误")
	}
	if errors.Is(err, errWatchHookTimeout) {
		t.Errorf("exit 1 错误 = %v，不应误分类为超时", err)
	}
}

// TestWatchHookRealTimeoutKill b8-W1 AC2 真路径：timeout 注入 200ms 短值（不真等
// 10s），挂死命令（unix sleep / windows ping 自延时）→ errWatchHookTimeout 分类 +
// 快速返回（远小于挂死时长）。windows 注：CommandContext 强杀的是 cmd.exe 直子
// 进程，孙进程（ping）随平台行为可能残留至自然结束——本用例 ping 上限约 4s 自灭
// 且 >nul 2>&1 已断开管道，不拖 Wait、无输出泄漏。
func TestWatchHookRealTimeoutKill(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过真子进程强杀用例")
	}
	var out bytes.Buffer
	run := watchHookRunner(&out, 200*time.Millisecond)
	cmdLine := "sleep 5"
	if runtime.GOOS == "windows" {
		cmdLine = "ping -n 5 127.0.0.1 >nul 2>&1"
	}
	start := time.Now()
	err := run(context.Background(), cmdLine, nil)
	if !errors.Is(err, errWatchHookTimeout) {
		t.Fatalf("挂死钩子错误 = %v，期望 errWatchHookTimeout 分类", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("强杀耗时 %v，期望 200ms 量级快速返回", elapsed)
	}
}

// TestWatchHookRunnerEnvWins 质量审查修 1：父环境预置同名 AITEAM_ 注入键时注入值
// 必须胜出（unix execve 后 getenv 首匹配——不过滤底座则嵌套钩子/用户手 export 的
// 陈旧值遮蔽本次命中数据，静默错报）。三注入键逐一预置旧值 → 子进程回显断言为
// 注入新值且零 stale 残留。
func TestWatchHookRunnerEnvWins(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过真子进程用例")
	}
	t.Setenv("AITEAM_HITS_JSON", "stale-hits")
	t.Setenv("AITEAM_HIT_SEQ", "999")
	t.Setenv("AITEAM_HIT_LEVEL", "stale-level")
	var out bytes.Buffer
	run := watchHookRunner(&out, 10*time.Second)
	envCmd := "echo SEQ=$AITEAM_HIT_SEQ LEVEL=$AITEAM_HIT_LEVEL JSON=$AITEAM_HITS_JSON"
	if runtime.GOOS == "windows" {
		envCmd = "echo SEQ=%AITEAM_HIT_SEQ% LEVEL=%AITEAM_HIT_LEVEL% JSON=%AITEAM_HITS_JSON%"
	}
	if err := run(context.Background(), envCmd, []string{
		"AITEAM_HITS_JSON=" + `[{"seq":885,"level":"important","kind":"direct","target_role":"controller"}]`,
		"AITEAM_HIT_SEQ=885",
		"AITEAM_HIT_LEVEL=important",
	}); err != nil {
		t.Fatalf("钩子执行失败: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "SEQ=885") || !strings.Contains(got, "LEVEL=important") || !strings.Contains(got, `"seq":885`) {
		t.Errorf("钩子回显 = %q，期望三注入键均为新值（注入必生效）", got)
	}
	if strings.Contains(got, "stale") {
		t.Errorf("钩子回显 = %q，不应含父环境陈旧值（同名键被遮蔽）", got)
	}
}

// TestWatchHookTimeoutConstant 质量审查补测 B：10s 上限常量锚——防装配点改值
// 无报警（spec W1 冻结值 10s，改值须过 spec）。
func TestWatchHookTimeoutConstant(t *testing.T) {
	if watchHookTimeout != 10*time.Second {
		t.Errorf("watchHookTimeout = %v，期望 10s（spec W1 冻结值）", watchHookTimeout)
	}
}

// TestWatchHookIntegrationFileDrop b8-W1 AC1 字面落盘用例（质量审查补测 A——
// 等价裁定后可选补齐项，审查者仓外实证可行）：--on-hit 命令把 AITEAM_HITS_JSON
// 重定向落盘临时文件，读回归一（TrimSpace 剥 echo 尾随 CRLF）后断言内容可反序列
// 化且含 seq/level/kind/target 四键值 + HIT 行照打 + rc=0。windows 侧用 redirect-
// first 无引号形态（>%TEMP%\f echo %VAR%——Go exec 的 \" 转义 cmd 不识别，引号
// 包裹路径反被改写；临时目录路径含空格时跳过，本机/CI 均不含）。
func TestWatchHookIntegrationFileDrop(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过真子进程落盘用例")
	}
	st, addr, _ := startWatchTestServer(t)
	projID, colID := locateGuideMailboxIDs(t, st)
	seq := insertWatchMailboxMessage(t, st, projID, colID, regAuthRole, "important")
	drop := filepath.Join(t.TempDir(), "hook-env.json")
	var hookCmd string
	if runtime.GOOS == "windows" {
		if strings.ContainsRune(drop, ' ') {
			t.Skip("windows 重定向目标无法安全引号包裹（Go exec 的 \\\" 转义 cmd 不识别），临时目录含空格时跳过")
		}
		hookCmd = fmt.Sprintf(`>%s echo %%AITEAM_HITS_JSON%%`, drop)
	} else {
		hookCmd = fmt.Sprintf(`>"%s" echo "$AITEAM_HITS_JSON"`, drop)
	}
	args := append(regIdentityArgs(), "--interval", "100ms", "--on-hit", hookCmd)

	var code int
	stdout, _ := captureStdoutStderr(t, func() {
		code = runWatchAsync(t, args, addr)
	})
	if code != 0 {
		t.Fatalf("watch --on-hit 集成退出码 = %d，期望 0", code)
	}
	if want := fmt.Sprintf("HIT seq=%d level=important kind=direct target=%s\n", seq, regAuthRole); !strings.Contains(stdout, want) {
		t.Errorf("watch --on-hit stdout = %q，期望含 %q（HIT 行照打）", stdout, want)
	}
	raw, err := os.ReadFile(drop)
	if err != nil {
		t.Fatalf("钩子落盘文件读取失败: %v（钩子未执行或重定向失败）", err)
	}
	content := strings.TrimSpace(string(raw))
	var hits []watchHit
	if err := json.Unmarshal([]byte(content), &hits); err != nil {
		t.Fatalf("落盘 AITEAM_HITS_JSON 不可反序列化: %v（content=%q）", err, content)
	}
	if len(hits) != 1 || hits[0].Seq != seq || hits[0].Level != "important" ||
		hits[0].Kind != "direct" || hits[0].TargetRole != regAuthRole {
		t.Errorf("落盘 AITEAM_HITS_JSON = %s，期望单元素 seq=%d level=important kind=direct target_role=%s", content, seq, regAuthRole)
	}
	assertNoSentinelRows(t, st)
}

// ───────────────────────── b8-W2 --force 接管+持有者诊断 ─────────────────────────
//
// 桩层：200+holder 诊断打印形态（含无 holder 旧服务端回退）/ --force body 契约；
// 集成层：真 server 占位哨兵（API 注册后停 poll）→ 无 force 诊断退 0 不进循环（AC4）
// / --force 接管进循环+旧 id 404 自然退出+无占位 force=普通注册（AC5）。

// watchRegStubServer 幂等注册桩（可编程序响应体+请求体捕获）：regBody 捕获最后一次
// 注册请求 body，respData 为固定响应 JSON；非注册请求=分叉语义破坏（误进循环）即报错。
func watchRegStubServer(t *testing.T, respData string) (*httptest.Server, *string) {
	var regBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/sentinels" {
			b, _ := io.ReadAll(r.Body)
			regBody = string(b)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(respData))
			return
		}
		t.Errorf("poll/DELETE 误发请求: %s %s", r.Method, r.URL.Path)
	}))
	return ts, &regBody
}

// TestWatchRegisterHolderDiagnostics b8-W2①（AC4 CLI 桩面）：200+holder → 诊断行
// 「已有哨兵值班中（持有者=<会话名>，最后 ping Ns 前）——若为残留孤儿可 --force
// 接管」退 0 不进循环；200 无 holder（旧服务端）→ 既有「已有哨兵值班中」回落。
func TestWatchRegisterHolderDiagnostics(t *testing.T) {
	t.Run("holder诊断行", func(t *testing.T) {
		ts, _ := watchRegStubServer(t, `{"data":{"sentinel_id":7,"interval_sec":5,"holder":{"session":"watch-old","last_ping_at":"2026-10-02T13:00:00Z","age_sec":42}}}`)
		t.Cleanup(ts.Close)
		stdout, stderr := runWantCode(t, "watch 持有者诊断", 0, func() int {
			return runWatchCmd(context.Background(), regIdentityArgs(), ts.URL, nil)
		})
		want := "已有哨兵值班中（持有者=watch-old，最后 ping 42s 前）——若为残留孤儿可 --force 接管；若你是恢复/接手此会话（压缩续接/窗口重开），直接 --force——旧哨兵的输出通道已随旧宿主失联"
		if !strings.Contains(stdout, want) {
			t.Errorf("诊断输出 = %q，期望含 %q", stdout, want)
		}
		if stderr != "" {
			t.Errorf("诊断 stderr = %q，期望空", stderr)
		}
	})
	t.Run("无holder回落旧行", func(t *testing.T) {
		ts, _ := watchRegStubServer(t, `{"data":{"sentinel_id":7,"interval_sec":5}}`)
		t.Cleanup(ts.Close)
		stdout, _ := runWantCode(t, "watch 无 holder 回落", 0, func() int {
			return runWatchCmd(context.Background(), regIdentityArgs(), ts.URL, nil)
		})
		if !strings.Contains(stdout, "已有哨兵值班中") {
			t.Errorf("回落输出 = %q，期望含「已有哨兵值班中」", stdout)
		}
		if strings.Contains(stdout, "持有者=") {
			t.Errorf("回落输出 = %q，不应含持有者段（响应无 holder）", stdout)
		}
	})
	t.Run("负差回落旧行", func(t *testing.T) {
		// 服务端钟回拨防御（质量审查修 2）：age_sec<0 不拼持有者段——「最后 ping -3s 前」
		// 误导观感，回落既有单行文案（与 CLI status 命中后缀/看板徽标负差防御同姿态）。
		ts, _ := watchRegStubServer(t, `{"data":{"sentinel_id":7,"interval_sec":5,"holder":{"session":"watch-old","last_ping_at":"2026-10-02T13:00:00Z","age_sec":-3}}}`)
		t.Cleanup(ts.Close)
		stdout, _ := runWantCode(t, "watch 负差回落", 0, func() int {
			return runWatchCmd(context.Background(), regIdentityArgs(), ts.URL, nil)
		})
		if !strings.Contains(stdout, "已有哨兵值班中") {
			t.Errorf("负差输出 = %q，期望含「已有哨兵值班中」", stdout)
		}
		if strings.Contains(stdout, "持有者=") || strings.Contains(stdout, "-3s") {
			t.Errorf("负差输出 = %q，不应拼持有者段（负差回落旧行）", stdout)
		}
	})
}

// TestWatchForceFlagBody b8-W2② wire 契约（桩面）：--force → 注册请求体含
// "force":true；缺省 → 请求体不含 force 键（缺省不带 body 或仅 interval）。
func TestWatchForceFlagBody(t *testing.T) {
	t.Run("force带键", func(t *testing.T) {
		ts, regBody := watchRegStubServer(t, `{"data":{"sentinel_id":7,"interval_sec":5}}`)
		t.Cleanup(ts.Close)
		runWantCode(t, "watch --force body", 0, func() int {
			return runWatchCmd(context.Background(),
				append(regIdentityArgs(), "--force"), ts.URL, nil)
		})
		if !strings.Contains(*regBody, `"force":true`) {
			t.Errorf("--force 注册请求体 = %q，期望含 \"force\":true", *regBody)
		}
	})
	t.Run("缺省无force键", func(t *testing.T) {
		ts, regBody := watchRegStubServer(t, `{"data":{"sentinel_id":7,"interval_sec":5}}`)
		t.Cleanup(ts.Close)
		runWantCode(t, "watch 缺省 body", 0, func() int {
			return runWatchCmd(context.Background(), regIdentityArgs(), ts.URL, nil)
		})
		if strings.Contains(*regBody, "force") {
			t.Errorf("缺省注册请求体 = %q，不应含 force 键", *regBody)
		}
	})
}

// watchRegisterViaAPI 集成辅助：身份四头直调 #14 注册占位哨兵（停 poll 行即在
// 失活阈值内——AC4/AC5 的「孤儿活行」造点；B1 中间件隐式注册身份会话），返回 id。
func watchRegisterViaAPI(t *testing.T, addr string) int64 {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, addr+"/api/v1/sentinels", nil)
	if err != nil {
		t.Fatalf("构造注册请求失败: %v", err)
	}
	req.Header.Set("X-Aiteam-Project", regAuthProject)
	req.Header.Set("X-Aiteam-Column", regAuthColumn)
	req.Header.Set("X-Aiteam-Session", regAuthSession)
	req.Header.Set("X-Aiteam-Role", regAuthRole)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("API 注册占位哨兵失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("API 注册占位哨兵 status = %d，期望 201", resp.StatusCode)
	}
	var body struct {
		Data struct {
			SentinelID int64 `json:"sentinel_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Data.SentinelID <= 0 {
		t.Fatalf("解析注册响应失败: %v（id=%d）", err, body.Data.SentinelID)
	}
	return body.Data.SentinelID
}

// watchPollViaAPI 集成辅助：以 watch 身份调 #15（旧孤儿哨兵 404 断言面），返回状态码。
func watchPollViaAPI(t *testing.T, addr string, id int64) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/api/v1/sentinels/%d/poll?column=%s&role=%s&session=%s",
			addr, id, regAuthColumn, regAuthRole, regAuthSession), nil)
	if err != nil {
		t.Fatalf("构造 poll 请求失败: %v", err)
	}
	req.Header.Set("X-Aiteam-Project", regAuthProject)
	req.Header.Set("X-Aiteam-Column", regAuthColumn)
	req.Header.Set("X-Aiteam-Session", regAuthSession)
	req.Header.Set("X-Aiteam-Role", regAuthRole)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("API poll 失败: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestWatchIntegrationHolderDiagnostics AC4 集成面：占位哨兵（API 注册后停 poll，
// 行在 15s 阈值内）→ 新 watch 无 force：输出含持有者会话名+ping 年龄+接管指引，
// rc=0 不进循环（不 DELETE——占位行原样在库）。
func TestWatchIntegrationHolderDiagnostics(t *testing.T) {
	st, addr, delHits := startWatchTestServer(t)
	placeholderID := watchRegisterViaAPI(t, addr)

	var code int
	stdout, stderr := captureStdoutStderr(t, func() {
		code = runWatchAsync(t, regIdentityArgs(), addr)
	})
	if code != 0 {
		t.Fatalf("watch 幂等命中退出码 = %d（stderr: %q），期望 0", code, stderr)
	}
	for _, want := range []string{
		"已有哨兵值班中",
		"持有者=" + regAuthSession,
		"最后 ping ",
		"若为残留孤儿可 --force 接管",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("诊断输出 = %q，期望含 %q", stdout, want)
		}
	}
	// 不进循环锚：幂等命中不轮询——输出不得出现 HIT 行（命中即进循环的证据行）。
	if strings.Contains(stdout, "HIT") {
		t.Errorf("诊断输出 = %q，不应进入轮询循环（出现 HIT 行）", stdout)
	}
	if *delHits != 0 {
		t.Errorf("DELETE 到达次数 = %d，期望 0（幂等命中不注销他人哨兵）", *delHits)
	}
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sentinels WHERE id = ?`, placeholderID).Scan(&n); err != nil || n != 1 {
		t.Errorf("占位哨兵行应原样保留（err=%v, n=%d）", err, n)
	}
}

// TestWatchIntegrationForceTakeover AC5 集成面：同场景 --force → rc=0 进入轮询循环
// （HIT 退 0）；接管=旧删新插（轮询路径携新哨兵 id ≠ 旧 id）；旧哨兵 id 下次 poll 得
// 404 sentinel_not_found；无占位时 --force=普通注册（201 进循环，第二段）。
// 观测面裁量（反向代理捕获 poll id）：runWatchAsync 阻塞至 watch 退出——收尾注销
// 自删新哨兵行（v0.1 语义零变化，AC6 冻结锚），接管新 id 无法事后从库读。watch 流量
// 经单宿反向代理转发真 server，捕获 poll 路径哨兵 id（读侧 codeCh 收码 happens-before
// 免锁，startWatchTestServer 注释同口径）；注册/夹具/库断言面直连 addr 不经代理。
func TestWatchIntegrationForceTakeover(t *testing.T) {
	st, addr, _ := startWatchTestServer(t)
	projID, colID := locateGuideMailboxIDs(t, st)
	oldID := watchRegisterViaAPI(t, addr) // 占位哨兵（活行）
	seq := insertWatchMailboxMessage(t, st, projID, colID, regAuthRole, "important")

	var pollIDs []int64
	target, err := url.Parse(addr)
	if err != nil {
		t.Fatalf("解析真 server 地址失败: %v", err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// poll 路径 /api/v1/sentinels/<id>/poll → 捕获 id（watch 自报的新哨兵 id）。
		if strings.HasPrefix(r.URL.Path, "/api/v1/sentinels/") && strings.HasSuffix(r.URL.Path, "/poll") {
			if parts := strings.Split(r.URL.Path, "/"); len(parts) >= 5 {
				if id, err := strconv.ParseInt(parts[4], 10, 64); err == nil {
					pollIDs = append(pollIDs, id)
				}
			}
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)

	var code int
	stdout, stderr := captureStdoutStderr(t, func() {
		code = runWatchAsync(t, append(regIdentityArgs(), "--force", "--interval", "100ms"), proxy.URL)
	})
	if code != 0 {
		t.Fatalf("force watch 退出码 = %d（stderr: %q），期望 0（接管后进循环命中退 0）", code, stderr)
	}
	if want := fmt.Sprintf("HIT seq=%d level=important kind=direct target=%s\n", seq, regAuthRole); !strings.Contains(stdout, want) {
		t.Errorf("force watch 输出 = %q，期望含 %q（进循环证据）", stdout, want)
	}

	// 接管落库面：进循环 watch 轮询的新哨兵 id ≠ 旧 id（旧删新插——若 200 幂等
	// 复用旧 id，服务端即未接管，此锚与旧 id 404 锚联合锁定 Takeover 语义）。
	if len(pollIDs) == 0 || pollIDs[0] == oldID {
		t.Errorf("接管后 poll 路径哨兵 id = %v，期望非空且 ≠ 旧 id %d（旧删新插）", pollIDs, oldID)
	}

	// 旧哨兵 id 下次 poll → 404 sentinel_not_found（旧孤儿自然退出）。
	if got := watchPollViaAPI(t, addr, oldID); got != http.StatusNotFound {
		t.Errorf("旧哨兵 id poll status = %d，期望 404 sentinel_not_found", got)
	}

	// 无占位时 force=普通注册：watch 收尾已注销，再跑 --force 应 201 进循环照常命中。
	seq2 := insertWatchMailboxMessage(t, st, projID, colID, regAuthRole, "normal")
	stdout, stderr = captureStdoutStderr(t, func() {
		code = runWatchAsync(t, append(regIdentityArgs(), "--force", "--interval", "100ms"), proxy.URL)
	})
	if code != 0 {
		t.Fatalf("无占位 force watch 退出码 = %d（stderr: %q），期望 0", code, stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf("HIT seq=%d", seq2)) {
		t.Errorf("无占位 force watch 输出 = %q，期望含 seq=%d 命中（普通注册进循环）", stdout, seq2)
	}
	assertNoSentinelRows(t, st)
}
