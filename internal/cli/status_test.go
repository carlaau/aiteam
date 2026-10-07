package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// status 命令 CLI 测试（B3-7，in-process 端到端）：复用资源族测试基座
// （startResTestServer 返回 addr+store、regIdentityArgs、runWantCode、
// captureStdoutStderr、closedServerAddr、injectWindowClock），断言退出码+
// stdout/stderr 文本——AC11.1 单栏目四项 / AC11.2 全局一行式（§4.2 样例逐行
// 对拍）/ AC12.2 端到端 LOST + --json 与 #17 响应 data 同构 + --all TODO 占位 +
// 退出码 2/3/4（§4.7）。
//
// 时钟纪律（双注入点，零真实墙钟）：types.NowUTC（RFC3339——#17 失联判定与
// generated_at）经 injectStatusClock 固定；store.NowInTz（判窗 HH:MM）经既有
// injectWindowClock 固定。渲染断言按空格分词序对拍（§4.2 样例多空格为对齐填充，
// 禁逐字节脆断言——b3-plan B3-7 任务书口径）。
//
// 夹具口径：消息/位点/会话/哨兵/窗/资源 store 直插（隔离被测面只经 HTTP，
// handler_status_test.go 同款夹具面在 CLI 包自建 helper——server 包 test helper
// 不跨包）；引导域 (management, c01) 作 CLI 身份域（心跳链要求身份栏目已登记，
// 单栏目模式一参两用即定位域），被观测实体 proj-a{05,06}/proj-b{01} 与身份域
// 解耦。

// statusNow status 域测试固定查询时钟（=§4.2 样例 generated_at 原值）。
const statusNow = "2026-10-02T13:04:05Z"

// injectStatusClock 固定服务端失联判定/generated_at 钟面（types.NowUTC 包级
// 注入点）。仅限串行用例：包级变量替换与并发读有竞态面。
func injectStatusClock(t *testing.T, now string) {
	t.Helper()
	orig := types.NowUTC
	types.NowUTC = func() string { return now }
	t.Cleanup(func() { types.NowUTC = orig })
}

// statusSeedProject 直插项目（store 公开 API），返回项目 id。
func statusSeedProject(t *testing.T, st *store.Store, code string) int64 {
	t.Helper()
	p, err := st.CreateProject(store.Project{Code: code, Name: "项目" + code})
	if err != nil {
		t.Fatalf("插入项目夹具 %q 失败: %v", code, err)
	}
	return p.ID
}

// statusSeedColumn 直插项目下栏目（store 公开 API），返回栏目 id。
func statusSeedColumn(t *testing.T, st *store.Store, projectCode, code string) int64 {
	t.Helper()
	c, err := st.CreateColumn(projectCode, store.Column{Code: code, Name: "栏目" + code})
	if err != nil {
		t.Fatalf("插入栏目夹具 %q 失败: %v", code, err)
	}
	return c.ID
}

// statusSeedSession 直插会话行（last_seen_at 可控——LOST 夹具核心；server 包
// seedSessionRow 同款 SQL），返回会话 id。
func statusSeedSession(t *testing.T, st *store.Store, projectID, columnID int64,
	name, role, lastSeenAt string) int64 {
	t.Helper()
	res, err := st.DB.Exec(
		`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, columnID, name, role, lastSeenAt, statusNow,
	)
	if err != nil {
		t.Fatalf("插入会话夹具 %q 失败: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取会话夹具 %q 自增 id 失败: %v", name, err)
	}
	return id
}

// statusSeedMessage 直插一条消息（sender_label 可控——block 清单行 sender 列
// 断言面；server 包 insertMessage 同款 SQL），返回全局 seq。
func statusSeedMessage(t *testing.T, st *store.Store, columnID int64, kind, targetRole string,
	senderLabel, level, createdAt string) int64 {
	t.Helper()
	res, err := st.DB.Exec(
		`INSERT INTO messages (project_id, column_id, kind, target_role, target_session_id,
		       sender_session_id, sender_label, level, body, created_at)
		 SELECT project_id, ?, ?, ?, 0, 0, ?, ?, 'hello', ? FROM columns WHERE id = ?`,
		columnID, kind, targetRole, senderLabel, level, createdAt, columnID,
	)
	if err != nil {
		t.Fatalf("插入 %s 消息夹具失败: %v", kind, err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取消息 seq 失败: %v", err)
	}
	return seq
}

// statusSeedPosition 设一行消费位点（upsert——CreateColumn 登记时已预置
// controller/executor 两行位点（InitColumnPositions，pos=当时全局 MAX(seq)），
// 夹具覆写目标 consumer 的值）。
func statusSeedPosition(t *testing.T, st *store.Store, columnID int64, consumer string, position int64) {
	t.Helper()
	if _, err := st.DB.Exec(
		`INSERT INTO ack_positions (project_id, column_id, consumer, position, updated_at)
		 SELECT project_id, ?, ?, ?, ? FROM columns WHERE id = ?
		 ON CONFLICT(column_id, consumer) DO UPDATE SET position = excluded.position`,
		columnID, consumer, position, statusNow, columnID,
	); err != nil {
		t.Fatalf("设置位点夹具失败: %v", err)
	}
}

// statusSeedSentinel 直插哨兵行（last_ping_at 可控——alive/dead 夹具核心；
// INSERT...SELECT 带出 project_id，server 包 seedSentinel 同款 SQL）。
func statusSeedSentinel(t *testing.T, st *store.Store, sessionID, columnID int64, role, pingAt string) {
	t.Helper()
	if _, err := st.DB.Exec(
		`INSERT INTO sentinels (session_id, project_id, column_id, role, started_at, last_ping_at)
		 SELECT ?, project_id, ?, ?, ?, ? FROM sessions WHERE id = ?`,
		sessionID, columnID, role, pingAt, pingAt, sessionID,
	); err != nil {
		t.Fatalf("插入哨兵 %s 夹具失败: %v", role, err)
	}
}

// statusTokenJoin 行分词归一（多空格压单空格）：§4.2 样例多空格为对齐填充，
// 断言按分词序列禁逐字节脆断言（b3-plan B3-7 任务书口径）。
func statusTokenJoin(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// assertStatusTokens 单行分词序对拍。
func assertStatusTokens(t *testing.T, name, got, want string) {
	t.Helper()
	if g, w := statusTokenJoin(got), statusTokenJoin(want); g != w {
		t.Errorf("%s 分词序 = %q，期望 %q", name, g, w)
	}
}

// seedStatusFixtures 全局模式对拍夹具（§4.2 样例形态等价构造——数值不复刻样例
// 字面，自增 seq/位点按夹具实际值断言）。预置位点语义：CreateColumn 登记即预置
// controller/executor 两行位点（pos=登记时全局 MAX(seq)=0）——每栏目恒两信箱行，
// 无活动的信箱行 pos=0 pending=0 sentinel=none 亦在列（信箱口径=位点行 ∪ 消息
// 出现面，b3-spec #17 行）：
//
//	proj-a/05  controller  pending=3 pos=1  sentinel=alive  S4:WAITING(23:00-09:00)
//	proj-a/05  executor    pending=0 pos=9  sentinel=none   S4:WAITING(23:00-09:00)
//	proj-a/06  controller  pending=1 pos=5  sentinel=dead*  -
//	proj-a/06  executor    pending=2 pos=0  sentinel=none   -（block06+已回执 block 待消费+预置位点行）
//	proj-b/01  controller  pending=0 pos=0  sentinel=none   -（预置行）
//	proj-b/01  executor    pending=0 pos=0  sentinel=none   -（预置行）
//	management/c01  controller+executor  pending=0 pos=0  sentinel=none  -（引导域预置行）
//	sessions: cli-test-session@c01 OK | controller-A@05 OK | executor-B@05 LOST(32m)*
//	          | controller-A@06 OK（项目序拉平：management 身份会话经心跳链 upsert 在列）
//	block 未回执 2 条（seq DESC：05 行先、06 行后——先插 06 行使其 seq 小）；另种
//	已回执 block 一条（06/executor，有回执行不入清单——cli 层 NOT EXISTS 保护）
//	resources in_use: port=1 account=2 data=1
//
// 返回 block 清单两 seq（05 行、06 行）供断言。
func seedStatusFixtures(t *testing.T, st *store.Store) (block05, block06 int64) {
	t.Helper()
	pidA := statusSeedProject(t, st, "proj-a")
	cid05 := statusSeedColumn(t, st, "proj-a", "05")
	cid06 := statusSeedColumn(t, st, "proj-a", "06")
	statusSeedProject(t, st, "proj-b")
	statusSeedColumn(t, st, "proj-b", "01")

	// 会话（插入序=输出序 ORDER BY s.id）：executor-B 距查询钟 1920s > 默认 900 → LOST。
	sessA05 := statusSeedSession(t, st, pidA, cid05, "controller-A", "controller", "2026-10-02T13:03:55Z")
	execB := statusSeedSession(t, st, pidA, cid05, "executor-B", "executor", "2026-10-02T12:32:05Z") // -1920s
	sessA06 := statusSeedSession(t, st, pidA, cid06, "controller-A", "controller", "2026-10-02T13:03:55Z")

	// proj-a/05：controller 信箱 4 条（位点=第 2 条 seq → pending=3）；executor
	// 信箱 2 条 normal+1 条 block（位点推进至 9=含 block 已消费未回执态——位点
	// 推进≠回执，block 未回执清单仍列；pending=0）。
	statusSeedPosition(t, st, cid05, "controller", 1)
	for range 4 {
		statusSeedMessage(t, st, cid05, "direct", "controller", "boss@05", "normal", "2026-10-02T12:50:00Z")
	}
	statusSeedMessage(t, st, cid05, "direct", "executor", "controller-A@05", "normal", "2026-10-02T12:55:00Z")
	statusSeedMessage(t, st, cid05, "direct", "executor", "controller-A@05", "normal", "2026-10-02T12:56:00Z")
	statusSeedPosition(t, st, cid05, "executor", 9)

	// proj-a/06：controller 信箱 1 条（位点 5 → pending=1 pos=5）。
	statusSeedPosition(t, st, cid06, "controller", 5)
	statusSeedMessage(t, st, cid06, "direct", "controller", "boss@06", "normal", "2026-10-02T12:58:00Z")

	// proj-b/01：无活动信箱——预置位点行（pos=0）即信箱行出现面，无需夹具。

	// 哨兵：05/controller 活（ping 距钟 5s ≤ 15 → alive）；06/controller 死
	// （ping 距钟 20s > 15 → dead*）；05/executor 无哨兵行 → none。
	statusSeedSentinel(t, st, sessA05, cid05, "controller", "2026-10-02T13:04:00Z")
	statusSeedSentinel(t, st, sessA06, cid06, "controller", "2026-10-02T13:03:45Z")

	// block 未回执两条（先 06 后 05 插——seq DESC 输出 05 行在前=§4.2 样例行序）。
	block06 = statusSeedMessage(t, st, cid06, "direct", "executor", "controller-A@06", "block", "2026-10-02T12:55:00Z")
	block05 = statusSeedMessage(t, st, cid05, "direct", "executor", "controller-A@05", "block", "2026-10-02T12:40:00Z")

	// 已回执 block 一条（cli 层 NOT EXISTS 谓词保护，B3-8 审查建议）：与 block06
	// 同信箱同角色（06/executor），造回执行后不入清单——下方清单计数断言仍 2 条
	// 即锁「已回执不入清单」语义（store 层 TestUnreceipted 已有保护，此为 cli 层
	// 补防）。回执方=executor-B（block 目标角色的会话身份，AC7.3 口径）；seq 最大
	// →06/executor 信箱 pending 计入（位点 0）、latest=本条（全局渲染无 latest 列
	// 不涉断言）。
	ackedBlock := statusSeedMessage(t, st, cid06, "direct", "executor", "controller-A@06", "block", "2026-10-02T12:58:00Z")
	if _, err := st.DB.Exec(
		`INSERT INTO message_receipts (message_seq, receipt_session_id, created_at)
		 VALUES (?, ?, ?)`, ackedBlock, execB, "2026-10-02T12:59:00Z"); err != nil {
		t.Fatalf("插入已回执 block 的回执行失败: %v", err)
	}

	// S4 跨午夜窗挂 05（as_of=10:00 → waiting）；06/01 无窗。
	if err := st.SetWindows(pidA, cid05, []store.WindowInput{
		{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1},
	}, sessA05, statusNow); err != nil {
		t.Fatalf("配置时间窗夹具失败: %v", err)
	}

	// in_use 资源：port×1 account×2 data×1（account 同型异值不冲突）。
	for _, r := range []struct{ rtype, value string }{
		{"port", "8080"},
		{"account_range", "acct:1000-1999"},
		{"account_range", "acct:2000-2999"},
		{"data_range", "seg:10-20"},
	} {
		if _, err := st.RegisterResource(pidA, cid05, r.rtype, r.value, "", sessA05, statusNow); err != nil {
			t.Fatalf("登记资源夹具 %s 失败: %v", r.rtype, err)
		}
	}
	return block05, block06
}

// runStatusGlobal 以引导域身份执行 status --global（serverOverride 注入测试地址）。
func runStatusGlobal(ctx context.Context, addr string, extra ...string) int {
	return runStatus(ctx, append(append(regIdentityArgs(), "--global"), extra...), addr)
}

// TestGlobalMode AC11.2 端到端：§4.2 样例逐行形态对拍（分词序）——首行
// `aiteam status <generated_at>`、栏目行（项目/栏目/角色/pending/pos/sentinel/
// 窗标记，无窗 `-`，dead 带 * 异常标记）、sessions 行（OK/LOST(分钟m)*）、
// block 未回执清单段（seq DESC）、resources in_use 行。
func TestGlobalMode(t *testing.T) {
	addr, st := startResTestServer(t)
	block05, block06 := seedStatusFixtures(t, st)
	injectStatusClock(t, statusNow)
	injectWindowClock(t, "10:00") // S4 23:00-09:00 窗外 → WAITING
	ctx := context.Background()

	stdout, stderr := runWantCode(t, "status --global", 0, func() int {
		return runStatusGlobal(ctx, addr)
	})
	if stderr != "" {
		t.Errorf("status --global 成功不应有 stderr 输出，实际 %q", stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 14 {
		t.Fatalf("全局输出行数 = %d，期望 14（首行+8 栏目信箱行+sessions+block 标题+2 清单+resources）：\n%s",
			len(lines), stdout)
	}

	// 首行：aiteam status <generated_at=注入钟>。
	assertStatusTokens(t, "首行", lines[0], "aiteam status "+statusNow)

	// 栏目行（projects ORDER BY code → columns project_id,code → mailboxes 角色
	// 字典序；引导域 management 两预置行也在列——overview 全量口径；预置位点行=
	// 无活动信箱行亦在列）。
	assertStatusTokens(t, "management/controller 行", lines[1],
		"management c01 controller pending=0 pos=0 sentinel=none -")
	assertStatusTokens(t, "management/executor 行", lines[2],
		"management c01 executor pending=0 pos=0 sentinel=none -")
	assertStatusTokens(t, "05/controller 行", lines[3],
		"proj-a 05 controller pending=3 pos=1 sentinel=alive S4:WAITING(23:00-09:00)")
	assertStatusTokens(t, "05/executor 行", lines[4],
		"proj-a 05 executor pending=0 pos=9 sentinel=none S4:WAITING(23:00-09:00)")
	assertStatusTokens(t, "06/controller 行", lines[5],
		"proj-a 06 controller pending=1 pos=5 sentinel=dead* -")
	assertStatusTokens(t, "06/executor 行", lines[6],
		"proj-a 06 executor pending=2 pos=0 sentinel=none -")
	assertStatusTokens(t, "proj-b controller 行", lines[7],
		"proj-b 01 controller pending=0 pos=0 sentinel=none -")
	assertStatusTokens(t, "proj-b executor 行", lines[8],
		"proj-b 01 executor pending=0 pos=0 sentinel=none -")

	// sessions 行：name@column OK | LOST(分钟m)*（lost_for_sec/60）。首项
	// cli-test-session@c01=本 CLI 请求经心跳链 upsert 的身份会话（projects 序
	// management 先于 proj-a；项目内 ORDER BY s.id）。
	assertStatusTokens(t, "sessions 行", lines[9],
		"sessions: cli-test-session@c01 OK | controller-A@05 OK | executor-B@05 LOST(32m)* | controller-A@06 OK")

	// block 未回执段：标题行计数 + 清单行（seq DESC → 05 行在前）。
	assertStatusTokens(t, "block 标题行", lines[10], "block 未回执: 2 条")
	assertStatusTokens(t, "block 清单 05 行", lines[11],
		"#"+strconv.FormatInt(block05, 10)+" proj-a/05/executor <- controller-A@05 (2026-10-02T12:40:00Z)")
	assertStatusTokens(t, "block 清单 06 行", lines[12],
		"#"+strconv.FormatInt(block06, 10)+" proj-a/06/executor <- controller-A@06 (2026-10-02T12:55:00Z)")
	// 清单行两空格行首缩进=清单从属关系的结构语义（§4.2 样例），不在「空格
	// 对齐填充」豁免面——原文前缀断言保护（分词断言压全部空白测不到缩进，
	// 审查 M1 变异实证：删缩进分词断言全绿=静默回归）。
	for _, line := range lines[11:13] {
		if !strings.HasPrefix(line, "  #") {
			t.Errorf("block 清单行缺两空格行首缩进（§4.2 结构语义）：%q", line)
		}
	}

	// resources in_use 行：rtype 规范名转 CLI 简写、固定序 port/account/data。
	assertStatusTokens(t, "resources 行", lines[13],
		"resources in_use: port=1 account=2 data=1")
}

// TestColumnMode AC11.1 端到端：缺省=单栏目模式（身份 project/column 一参两用
// 即定位域，端点 mode=column）——四项渲染（待消费数/最新摘要/哨兵活性/时间窗
// 状态）+ block 未回执段；不串其他栏目/项目、不输出 sessions/resources 段。
func TestColumnMode(t *testing.T) {
	addr, st := startResTestServer(t)
	seedStatusFixtures(t, st) // 串台源：proj-a/proj-b 全量在库

	// 身份域 management/c01 四项夹具：controller 信箱 2 条（位点=第 1 条 seq——
	// 动态取值，勿随 seedStatusFixtures 消息总数硬编码，已回执 block 补种时踩过
	// 该坑）→ pending=1、latest=第 2 条+活哨兵（ping 距钟 5s → alive）+S3 同日窗
	// （as_of=10:00 窗内 → ALLOWED）+block 未回执一条（target=executor，亦计入
	// executor 信箱 pending/latest——block 待消费口径）。引导域 c01 已存在，
	// 直接解析 id（重复 Create 撞唯一约束）。
	var projMID, cid int64
	if err := st.DB.QueryRow(
		`SELECT p.id, c.id FROM projects p JOIN columns c ON c.project_id = p.id
		 WHERE p.code = ? AND c.code = ?`, regAuthProject, regAuthColumn,
	).Scan(&projMID, &cid); err != nil {
		t.Fatalf("解析引导域 id 失败: %v", err)
	}
	firstCtrl := statusSeedMessage(t, st, cid, "direct", "controller", "boss@c01", "normal", "2026-10-02T12:50:00Z")
	latest := statusSeedMessage(t, st, cid, "direct", "controller", "boss@c01", "normal", "2026-10-02T12:51:00Z")
	statusSeedPosition(t, st, cid, "controller", firstCtrl)
	fixSess := statusSeedSession(t, st, projMID, cid, "fixture-S", "controller", statusNow)
	statusSeedSentinel(t, st, fixSess, cid, "controller", "2026-10-02T13:04:00Z")
	if err := st.SetWindows(projMID, cid, []store.WindowInput{
		{Stage: "S3", From: "09:00", To: "18:00", Enabled: 1},
	}, fixSess, statusNow); err != nil {
		t.Fatalf("配置时间窗夹具失败: %v", err)
	}
	blockSeq := statusSeedMessage(t, st, cid, "direct", "executor", "boss@c01", "block", "2026-10-02T12:40:00Z")

	injectStatusClock(t, statusNow)
	injectWindowClock(t, "10:00")
	ctx := context.Background()

	stdout, _ := runWantCode(t, "status 单栏目", 0, func() int {
		return runStatus(ctx, regIdentityArgs(), addr)
	})
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("单栏目输出行数 = %d，期望 7（首行+2 信箱行+block 标题+3 清单）：\n%s", len(lines), stdout)
	}

	assertStatusTokens(t, "首行", lines[0], "aiteam status "+statusNow)
	// 四项行：pending（待消费）+ pos + latest（最新摘要）+ sentinel（哨兵活性）+ 窗标记。
	assertStatusTokens(t, "controller 四项行", lines[1],
		"management c01 controller pending=1 pos="+strconv.FormatInt(firstCtrl, 10)+
			" latest=#"+strconv.FormatInt(latest, 10)+
			" normal 2026-10-02T12:51:00Z sentinel=alive S3:ALLOWED(09:00-18:00)")
	// executor 行：预置位点行+本域 block 消息待消费计入（block seq>位点 0 →
	// pending=1，latest=该 block——「最新到达」口径），窗标记随栏目 S3 同源在列。
	assertStatusTokens(t, "executor 四项行", lines[2],
		"management c01 executor pending=1 pos=0 latest=#"+strconv.FormatInt(blockSeq, 10)+
			" block 2026-10-02T12:40:00Z sentinel=none S3:ALLOWED(09:00-18:00)")
	// block 段走端点全局清单（store 冻结口径无 :me 维度——单栏目模式原样透出，
	// seq DESC：本域 block 最先）。
	assertStatusTokens(t, "block 标题行", lines[3], "block 未回执: 3 条")
	assertStatusTokens(t, "block 清单本域行", lines[4],
		"#"+strconv.FormatInt(blockSeq, 10)+" management/c01/executor <- boss@c01 (2026-10-02T12:40:00Z)")

	// 不串台（信箱行区段逐行前缀判——block 全局清单合法含外域 target，不作
	// 此断言面）：信箱行不得出现其他项目/栏目；sessions/resources 段不渲染。
	for _, line := range lines[1:3] {
		if strings.HasPrefix(line, "proj-a") || strings.HasPrefix(line, "proj-b") {
			t.Errorf("单栏目信箱行串台外域：%q", line)
		}
	}
	for _, banned := range []string{"sessions:", "resources in_use"} {
		if strings.Contains(stdout, banned) {
			t.Errorf("单栏目输出不应含 %q：\n%s", banned, stdout)
		}
	}
}

// TestLost AC12.2 端到端：注入时钟推进跨越失联阈值（默认 900s）——同一会话
// 由 OK 翻转为 LOST(分钟m)*（lost_for_sec/60），全局模式可见。
func TestLost(t *testing.T) {
	addr, st := startResTestServer(t)
	injectWindowClock(t, "10:00")
	ctx := context.Background()

	pid := statusSeedProject(t, st, "proj-lost")
	cid := statusSeedColumn(t, st, "proj-lost", "05")
	statusSeedSession(t, st, pid, cid, "watcher-W", "executor", "2026-10-02T12:54:05Z") // 距 T 差 600s

	// 钟 T：差 600s ≤ 900 → alive → OK。
	injectStatusClock(t, statusNow)
	stdout, _ := runWantCode(t, "阈值内 OK", 0, func() int { return runStatusGlobal(ctx, addr) })
	if !strings.Contains(stdout, "watcher-W@05 OK") {
		t.Errorf("600s 差输出应含 watcher-W@05 OK：\n%s", stdout)
	}

	// 钟 T+3120s：差 3720s > 900 → lost_for_sec=3720 → LOST(62m)*。
	injectStatusClock(t, "2026-10-02T13:56:05Z")
	stdout, _ = runWantCode(t, "超阈值 LOST", 0, func() int { return runStatusGlobal(ctx, addr) })
	if !strings.Contains(stdout, "watcher-W@05 LOST(62m)*") {
		t.Errorf("3720s 差输出应含 watcher-W@05 LOST(62m)*（AC12.2 端到端）：\n%s", stdout)
	}
}

// TestAllFlag --all：全局模式合法 flag——请求 URL 追加 all=1（#17 扩面参数，
// 总控 #20 裁定，消 TODO(B3-7-all) 占位）：输出含 archived 项目行（其下栏目信箱
// /会话照常在列）；缺省 --global 不含（冻结口径不变）；--global --all --json
// 组合同样含 archived（projects 数组 code+status 断言）；单栏目模式给 --all =
// 用法冲突退 2 不发请求（死地址注入证明）。
func TestAllFlag(t *testing.T) {
	addr, st := startResTestServer(t)
	seedStatusFixtures(t, st)
	// archived 项目夹具（扩面断言面）：proj-old + active 栏目 01（登记即预置
	// controller/executor 两行位点=信箱行出现面）+ 一只活会话，登记后归档。
	pidOld := statusSeedProject(t, st, "proj-old")
	cidOld := statusSeedColumn(t, st, "proj-old", "01")
	statusSeedSession(t, st, pidOld, cidOld, "zombie-Z", "executor", "2026-10-02T13:03:00Z")
	if err := st.ArchiveProject("proj-old"); err != nil {
		t.Fatalf("归档 proj-old 夹具失败: %v", err)
	}
	dead := closedServerAddr(t)
	injectStatusClock(t, statusNow)
	injectWindowClock(t, "10:00")
	ctx := context.Background()

	// 缺省 --global：archived 项目不在输出（冻结口径不变），无 stderr 提示。
	stdout, stderr := runWantCode(t, "缺省全局", 0, func() int {
		return runStatusGlobal(ctx, addr)
	})
	if strings.Contains(stdout, "proj-old") || strings.Contains(stdout, "zombie-Z") {
		t.Errorf("缺省全局输出不应含 archived 项目：\n%s", stdout)
	}
	if stderr != "" {
		t.Errorf("缺省全局 stderr = %q，期望空", stderr)
	}

	// --global --all：archived 项目行在列（信箱行+zombie-Z 会话），无 stderr
	//（TODO 占位提示行随接线删除——stderr 恒空）。
	stdout, stderr = runWantCode(t, "--all 全局", 0, func() int {
		return runStatusGlobal(ctx, addr, "--all")
	})
	// 预置位点=登记时全局 MAX(seq)（随 seedStatusFixtures 消息数浮动——动态读
	// 防夹具漂移脆断言）。
	var presetPos int64
	if err := st.DB.QueryRow(
		`SELECT p.position FROM ack_positions p JOIN columns c ON c.id = p.column_id
		 WHERE c.project_id = ? AND c.code = ? AND p.consumer = 'controller'`,
		pidOld, "01",
	).Scan(&presetPos); err != nil {
		t.Fatalf("读 proj-old 预置位点失败: %v", err)
	}
	foundArchived := false
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		// 渲染为两空格分隔——按分词定位（proj-old <col> controller 行），禁逐字节
		// 脆匹配（b3-plan B3-7 断言口径）。
		toks := strings.Fields(line)
		if len(toks) > 3 && toks[0] == "proj-old" && toks[2] == "controller" {
			assertStatusTokens(t, "archived 项目信箱行", line,
				"proj-old 01 controller pending=0 pos="+strconv.FormatInt(presetPos, 10)+
					" sentinel=none -")
			foundArchived = true
		}
	}
	if !foundArchived {
		t.Errorf("--all 全局输出应含 archived 项目 proj-old 信箱行：\n%s", stdout)
	}
	if !strings.Contains(stdout, "zombie-Z@01 OK") {
		t.Errorf("--all 全局输出应含 archived 项目会话 zombie-Z@01 OK：\n%s", stdout)
	}
	if stderr != "" {
		t.Errorf("--all 全局 stderr = %q，期望空（呈批提示行已随接线删除）", stderr)
	}

	// --global --all --json 组合：projects 数组含 archived code（status=archived）。
	stdout, _ = runWantCode(t, "--all --json 组合", 0, func() int {
		return runStatusGlobal(ctx, addr, "--all", "--json")
	})
	var js map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &js); err != nil {
		t.Fatalf("--all --json stdout 非合法 JSON: %v（%q）", err, stdout)
	}
	projects, ok := js["projects"].([]any)
	if !ok {
		t.Fatalf("--all --json projects 缺失或非数组: %v", js["projects"])
	}
	hasArchived := false
	for _, p := range projects {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if pm["code"] == "proj-old" {
			hasArchived = pm["status"] == "archived"
		}
	}
	if !hasArchived {
		t.Errorf("--all --json projects 应含 status=archived 的 proj-old: %v", projects)
	}

	// 单栏目模式给 --all：本地用法冲突退 2。
	_, stderr = runWantCode(t, "单栏目 --all 冲突", 2, func() int {
		return runStatus(ctx, append(regIdentityArgs(), "--all"), dead)
	})
	if !strings.Contains(stderr, "--all") {
		t.Errorf("单栏目 --all stderr = %q，期望指明冲突对象", stderr)
	}
}

// TestStatusJSON --json 与 #17 响应 data 同构（全局与单栏目两模式）：CLI 输出
// 反序列化后与直连端点 data 深等（键集+值面，re-marshal 往返无损）。
func TestStatusJSON(t *testing.T) {
	addr, st := startResTestServer(t)
	seedStatusFixtures(t, st)
	injectStatusClock(t, statusNow)
	injectWindowClock(t, "10:00")
	ctx := context.Background()

	// 直连端点取权威 data（带四头走心跳链，同 CLI 请求路径）。
	fetchData := func(query string) map[string]any {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, addr+"/api/v1/status?"+query, nil)
		if err != nil {
			t.Fatalf("构造端点请求失败: %v", err)
		}
		for k, v := range map[string]string{
			"X-Aiteam-Project": regAuthProject, "X-Aiteam-Column": regAuthColumn,
			"X-Aiteam-Session": regAuthSession, "X-Aiteam-Role": regAuthRole,
		} {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("端点请求失败: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("端点 status = %d（body: %s）", resp.StatusCode, body)
		}
		var wrapped struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(body, &wrapped); err != nil {
			t.Fatalf("端点响应解析失败: %v", err)
		}
		return wrapped.Data
	}

	runJSON := func(name string, args []string) map[string]any {
		t.Helper()
		stdout, _ := runWantCode(t, name, 0, func() int {
			return runStatus(ctx, args, addr)
		})
		var got map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &got); err != nil {
			t.Fatalf("%s --json 输出非合法 JSON: %v（%q）", name, err, stdout)
		}
		return got
	}

	// 全局：CLI --json ≡ 端点 overview data。
	globalGot := runJSON("全局 --json", append(regIdentityArgs(), "--global", "--json"))
	if want := fetchData("mode=overview"); !jsonDeepEqual(globalGot, want) {
		t.Errorf("全局 --json 与 #17 data 不同构：\ngot  %v\nwant %v", globalGot, want)
	}
	// b3-W2 就近显式锚（深等之上单键保护）：sessions 数组元素含
	// unread_mailbox/unread_dialog 键——JSON 原文级断言（runJSON 反序列化自 CLI
	// stdout 原文，键存在即镜像 DTO 序列化面透出；两键恒在无 omitempty，
	// store.SessionEntry 同口径）。
	projects, ok := globalGot["projects"].([]any)
	if !ok {
		t.Fatalf("全局 --json projects 缺失或非数组: %v", globalGot["projects"])
	}
	checked := 0
	for _, p := range projects {
		pm, _ := p.(map[string]any)
		sessions, _ := pm["sessions"].([]any)
		for _, s := range sessions {
			sm, _ := s.(map[string]any)
			if _, ok := sm["unread_mailbox"]; !ok {
				t.Errorf("sessions 元素缺 unread_mailbox 键: %v", sm)
			}
			if _, ok := sm["unread_dialog"]; !ok {
				t.Errorf("sessions 元素缺 unread_dialog 键: %v", sm)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatalf("全局 --json 零会话元素，unread 键断言无从执行（夹具漂移）")
	}
	// 单栏目：CLI --json ≡ 端点 column data。
	if got, want := runJSON("单栏目 --json", append(regIdentityArgs(), "--json")),
		fetchData("mode=column&project="+regAuthProject+"&column="+regAuthColumn); !jsonDeepEqual(got, want) {
		t.Errorf("单栏目 --json 与 #17 data 不同构：\ngot  %v\nwant %v", got, want)
	}
}

// jsonDeepEqual JSON 值深等：两侧各 re-marshal 归一化后字符串比较（键序归一、
// 数值形态归一——避免 map 遍历序与 int/float 形态敏感）。
func jsonDeepEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

// TestStatusUsage 用法与错误面（§4.7）：缺身份任一参退 2 不发请求；死地址退 3
// （不可达）；未登记栏目单栏目模式 → 端点 404 column_not_found 透传退 4。
func TestStatusUsage(t *testing.T) {
	dead := closedServerAddr(t)
	ctx := context.Background()

	// 缺身份四参逐项：本地退 2（validateIdentity 惯例——ErrUsage）。
	for _, drop := range []string{"--project", "--column", "--session", "--role"} {
		args := regIdentityArgs()
		for i, a := range args {
			if a == drop {
				args = append(args[:i], args[i+2:]...)
				break
			}
		}
		_, stderr := runWantCode(t, "缺 "+drop, 2, func() int {
			return runStatus(ctx, args, dead)
		})
		if !strings.Contains(stderr, drop) {
			t.Errorf("缺 %s stderr = %q，期望指明缺失 flag", drop, stderr)
		}
	}

	// 死地址不可达退 3。
	addr, _ := startResTestServer(t)
	runWantCode(t, "死地址全局", 3, func() int { return runStatusGlobal(ctx, dead) })

	// 未登记栏目单栏目模式：身份 --column 指向 ghost → 端点 404 → 退 4。
	_, stderr := runWantCode(t, "未登记栏目", 4, func() int {
		return runStatus(ctx, resIdentityArgs(regAuthProject, "ghost"), addr)
	})
	if !strings.Contains(stderr, "column_not_found") {
		t.Errorf("未登记栏目 stderr = %q，期望含 column_not_found（退 4）", stderr)
	}
}

// TestRenderStatusEdge 渲染纯函数边界（构造 DTO 直调，不经 HTTP）：多窗 stage
// 字典序空格连排+allowed 大写同构、无会话省略 sessions 行、零未回执仅标题行、
// 空 by_type 资源行兜底形态、单栏目 latest 摘要三要素。
func TestRenderStatusEdge(t *testing.T) {
	ov := &statusData{
		GeneratedAt: statusNow,
		Projects: []statusProject{{
			Code: "proj-x", Name: "项目x", Status: "active",
			Columns: []statusColumn{{
				Code: "05", Status: "active",
				Mailboxes: []statusMailbox{
					{Role: "controller", Pending: 1, Position: 2,
						Latest:   &statusLatest{Seq: 9, Level: "important", CreatedAt: "2026-10-02T13:00:00Z"},
						Sentinel: "none"},
				},
				Windows: map[string]statusWindowPhase{
					"S5": {Status: "allowed", Start: "08:00", End: "12:00"},
					"S4": {Status: "waiting", Start: "23:00", End: "09:00"},
				},
			}},
			Sessions: []statusSession{},
		}},
		// 一条未回执（清单行在列——行首缩进结构语义的断言面，审查 M1）。
		BlockUnreceipted: []statusBlock{{
			Seq: 12, Level: "block", Target: "proj-x/05/executor",
			Sender: "boss@05", CreatedAt: "2026-10-02T12:40:00Z",
		}},
		ResourcesSummary: statusResources{InUseCount: 0, ByType: map[string]int64{}},
	}

	lines := renderGlobalLines(ov)
	if len(lines) != 5 {
		t.Fatalf("边界输出行数 = %d，期望 5（首行+栏目行+block 标题+1 清单+resources；无会话省略 sessions 行）：%q",
			len(lines), lines)
	}
	// 多窗按 stage 字典序空格连排；allowed 大写同构。
	assertStatusTokens(t, "多窗栏目行", lines[1],
		"proj-x 05 controller pending=1 pos=2 sentinel=none S4:WAITING(23:00-09:00) S5:ALLOWED(08:00-12:00)")
	// 标题行计数 + 清单行：两空格行首缩进原文前缀断言（结构语义不在分词豁免面，
	// 审查 M1）；清单行内容分词序对拍。
	assertStatusTokens(t, "block 标题行", lines[2], "block 未回执: 1 条")
	assertStatusTokens(t, "block 清单行", lines[3],
		"#12 proj-x/05/executor <- boss@05 (2026-10-02T12:40:00Z)")
	if !strings.HasPrefix(lines[3], "  #") {
		t.Errorf("block 清单行缺两空格行首缩进（§4.2 结构语义）：%q", lines[3])
	}
	// 空 by_type：资源行以 in_use_count 兜底。
	assertStatusTokens(t, "resources 行", lines[4], "resources in_use: 0")

	// 单栏目渲染同源边界：latest 摘要三要素 + 多窗连排同型 + block 清单行缩进。
	colLines := renderColumnLines(ov, "proj-x", "05")
	if len(colLines) != 4 {
		t.Fatalf("单栏目边界输出行数 = %d，期望 4：%q", len(colLines), colLines)
	}
	assertStatusTokens(t, "单栏目四项行", colLines[1],
		"proj-x 05 controller pending=1 pos=2 latest=#9 important 2026-10-02T13:00:00Z sentinel=none "+
			"S4:WAITING(23:00-09:00) S5:ALLOWED(08:00-12:00)")
	assertStatusTokens(t, "单栏目 block 标题行", colLines[2], "block 未回执: 1 条")
	if !strings.HasPrefix(colLines[3], "  #") {
		t.Errorf("单栏目 block 清单行缺两空格行首缩进：%q", colLines[3])
	}
}

// ===== b8-W3 CLI status 命中留痕渲染测试 =====

// TestStatusSessionHitSuffix b8-W3/AC8 CLI 面纯函数锚：会话行 OK/LOST 态后有值时
// 追加「·命中Ns前」（Ns=generated_at−sentinel_last_hit_at 秒差，响应值同源不取
// 本地时钟）；无值不加缀；负差（钟面回拨）/时间串不可解析=防御性不加缀。
func TestStatusSessionHitSuffix(t *testing.T) {
	ov := &statusData{
		GeneratedAt: "2026-10-02T13:04:05Z",
		Projects: []statusProject{{
			Code: "proj-a", Columns: []statusColumn{},
			Sessions: []statusSession{
				{Name: "hit-a", Column: "05", Alive: true, SentinelLastHitAt: "2026-10-02T13:03:35Z"},   // 30s 前
				{Name: "idle-b", Column: "05", Alive: true},                                             // 无值
				{Name: "clock-c", Column: "05", Alive: true, SentinelLastHitAt: "2026-10-02T13:05:35Z"}, // 未来=负差
				{Name: "bad-d", Column: "05", Alive: true, SentinelLastHitAt: "not-a-time"},             // 不可解析
			},
		}},
	}
	lines := renderGlobalLines(ov)
	var sess string
	for _, l := range lines {
		if strings.HasPrefix(l, "sessions:") {
			sess = l
		}
	}
	if sess == "" {
		t.Fatalf("渲染输出缺 sessions 行: %v", lines)
	}
	for _, want := range []string{"hit-a@05 OK·命中30s前", "idle-b@05 OK", "clock-c@05 OK", "bad-d@05 OK"} {
		if !strings.Contains(sess, want) {
			t.Errorf("sessions 行 = %q，期望含 %q", sess, want)
		}
	}
	if strings.Contains(sess, "命中0s前") || strings.Contains(sess, "命中-") {
		t.Errorf("sessions 行 = %q，负差/异常值不应渲染命中后缀", sess)
	}
	if strings.Contains(sess, "idle-b@05 OK·") || strings.Contains(sess, "clock-c@05 OK·") || strings.Contains(sess, "bad-d@05 OK·") {
		t.Errorf("sessions 行 = %q，无值/负差/不可解析会话不应带命中后缀", sess)
	}
}

// TestStatusGlobalHitSuffixE2E b8-W3/AC8 端到端：直插会话留痕列后 status --global
// 输出同口径（有值会话行含命中文案，无值不含）——CLI 镜像字段↔#17 响应接线面。
func TestStatusGlobalHitSuffixE2E(t *testing.T) {
	addr, st := startResTestServer(t)
	seedStatusFixtures(t, st)
	// executor-B（LOST 夹具会话）置留痕=注入钟前 45s；其余会话无值对照。
	if _, err := st.DB.Exec(`UPDATE sessions SET sentinel_last_hit_at = ? WHERE name = 'executor-B'`,
		"2026-10-02T13:03:20Z"); err != nil {
		t.Fatalf("预置留痕列失败: %v", err)
	}
	injectStatusClock(t, statusNow)
	injectWindowClock(t, "10:00")
	ctx := context.Background()

	stdout, _ := runWantCode(t, "status --global 命中后缀", 0, func() int {
		return runStatusGlobal(ctx, addr)
	})
	if !strings.Contains(stdout, "executor-B@05 LOST(32m)*·命中45s前") {
		t.Errorf("status --global 输出缺 executor-B 命中后缀行：\n%s", stdout)
	}
	if strings.Contains(stdout, "controller-A@05 OK·命中") {
		t.Errorf("status --global 输出 controller-A@05 不应带命中后缀（无值）：\n%s", stdout)
	}
}
