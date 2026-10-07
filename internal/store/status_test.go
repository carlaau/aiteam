// status_test.go —— B3-3 status 聚合 store 测试（规格：技术设计 §2.3 响应结构 /
// §3.6 关键 SQL / §3.7 九条往返基线 / §7.2 失联判定 / §7.3~§7.4 哨兵三态；b3-plan B3-3）。
//
// 时钟纪律：now 全部字符串注入（复用 sentinels_test.go 的 tsOffset 基准），零真实墙钟。
// 夹具纪律：messages/ack_positions/resources/sentinels 直插（B2/B4 未合并无公开 API，
// feat/b3 分层允许直插）；fixtureObservation 共享夹具已上移 store_test.go（B3-1 审查登记项）。
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"modernc.org/sqlite"
)

// ===== 直插夹具原子件（多项目多栏目夹具由各测试自行组装）=====

// fixtureProject 直插项目行，返回 id（status/code/name/hb 四面可控）。
func fixtureProject(t *testing.T, s *Store, code, name, status string, hbTimeoutSec int64) int64 {
	t.Helper()
	t0 := tsOffset(0)
	res, err := s.DB.Exec(
		`INSERT INTO projects (code, name, status, heartbeat_timeout_sec, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`, code, name, status, hbTimeoutSec, t0, t0)
	if err != nil {
		t.Fatalf("插入项目 %s 夹具失败: %v", code, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取项目 %s 自增 id 失败: %v", code, err)
	}
	return id
}

// fixtureColumn 直插栏目行，返回 id。
func fixtureColumn(t *testing.T, s *Store, projectID int64, code, name, status string) int64 {
	t.Helper()
	t0 := tsOffset(0)
	res, err := s.DB.Exec(
		`INSERT INTO columns (project_id, code, name, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`, projectID, code, name, status, t0, t0)
	if err != nil {
		t.Fatalf("插入栏目 %s 夹具失败: %v", code, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取栏目 %s 自增 id 失败: %v", code, err)
	}
	return id
}

// fixtureSession 直插会话行，返回 id（lastSeen 为相对基准时刻的注入串）。
func fixtureSession(t *testing.T, s *Store, projectID, columnID int64, name, role, lastSeen string) int64 {
	t.Helper()
	res, err := s.DB.Exec(
		`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`, projectID, columnID, name, role, lastSeen, tsOffset(0))
	if err != nil {
		t.Fatalf("插入会话 %s 夹具失败: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取会话 %s 自增 id 失败: %v", name, err)
	}
	return id
}

// fixtureMessage 直插消息行，返回 seq（append-only，seq 全局自增——测试经返回值记录，
// 不硬编码具体数值防夹具顺序改动全崩）。
func fixtureMessage(t *testing.T, s *Store, projectID, columnID int64, kind, targetRole string,
	targetSessionID, senderSessionID int64, senderLabel, level, body, createdAt string) int64 {
	t.Helper()
	res, err := s.DB.Exec(
		`INSERT INTO messages (project_id, column_id, kind, target_role, target_session_id,
		   sender_session_id, sender_label, level, body, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, columnID, kind, targetRole, targetSessionID, senderSessionID, senderLabel, level, body, createdAt)
	if err != nil {
		t.Fatalf("插入消息夹具失败: %v", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取消息自增 seq 失败: %v", err)
	}
	return seq
}

// fixturePosition 直插信箱/对话位点行（consumer=角色名 或 'chat:session:<id>'）。
func fixturePosition(t *testing.T, s *Store, projectID, columnID int64, consumer string, position int64) {
	t.Helper()
	if _, err := s.DB.Exec(
		`INSERT INTO ack_positions (project_id, column_id, consumer, position, updated_at)
		 VALUES (?, ?, ?, ?, ?)`, projectID, columnID, consumer, position, tsOffset(0)); err != nil {
		t.Fatalf("插入位点 %s 夹具失败: %v", consumer, err)
	}
}

// fixtureSentinel 直插哨兵行（last_ping_at 可控——活性混合夹具；绕过 RegisterSentinel
// 隔离被测面，同 sentinels_test.go TestSentinelLiveness 先例）。
func fixtureSentinel(t *testing.T, s *Store, sessionID, columnID int64, role, pingAt string) int64 {
	t.Helper()
	res, err := s.DB.Exec(
		`INSERT INTO sentinels (session_id, project_id, column_id, role, started_at, last_ping_at)
		 SELECT ?, project_id, ?, ?, ?, ? FROM sessions WHERE id = ?`,
		sessionID, columnID, role, pingAt, pingAt, sessionID)
	if err != nil {
		t.Fatalf("插入哨兵 %s 夹具失败: %v", role, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取哨兵自增 id 失败: %v", err)
	}
	return id
}

// fixtureReceipt 直插阻断回执行。
func fixtureReceipt(t *testing.T, s *Store, messageSeq, receiptSessionID int64) {
	t.Helper()
	if _, err := s.DB.Exec(
		`INSERT INTO message_receipts (message_seq, receipt_session_id, created_at)
		 VALUES (?, ?, ?)`, messageSeq, receiptSessionID, tsOffset(0)); err != nil {
		t.Fatalf("插入回执夹具失败: %v", err)
	}
}

// fixtureResource 直插资源行（B4 CRUD 未合并，直插隔离被测面——b3-plan B3-3 口径）。
func fixtureResource(t *testing.T, s *Store, projectID, columnID int64,
	rtype, value, prefix string, start, end int64, status string, createdBy int64) {
	t.Helper()
	if _, err := s.DB.Exec(
		`INSERT INTO resources (project_id, column_id, rtype, value, range_prefix, range_start,
		   range_end, note, status, created_by, created_at, released_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, NULL)`,
		projectID, columnID, rtype, value, prefix, start, end, status, createdBy, tsOffset(0)); err != nil {
		t.Fatalf("插入资源 %s 夹具失败: %v", value, err)
	}
}

// mailboxAt 从 overview 中取（项目code, 栏目code, 角色）对应信箱视图（断言辅助）。
func mailboxAt(t *testing.T, ov Overview, projectCode, columnCode, role string) MailboxOverview {
	t.Helper()
	pi := slices.IndexFunc(ov.Projects, func(p ProjectOverview) bool { return p.Code == projectCode })
	if pi < 0 {
		t.Fatalf("overview 缺项目 %s", projectCode)
	}
	ci := slices.IndexFunc(ov.Projects[pi].Columns, func(c ColumnOverview) bool { return c.Code == columnCode })
	if ci < 0 {
		t.Fatalf("项目 %s 缺栏目 %s", projectCode, columnCode)
	}
	mi := slices.IndexFunc(ov.Projects[pi].Columns[ci].Mailboxes, func(m MailboxOverview) bool { return m.Role == role })
	if mi < 0 {
		t.Fatalf("项目 %s 栏目 %s 缺信箱角色 %s", projectCode, columnCode, role)
	}
	return ov.Projects[pi].Columns[ci].Mailboxes[mi]
}

// ===== TestBuildOverview：§2.3 逐字段结构 + archived 域排除 + generated_at 注入 + windows 段锚定 =====

func TestBuildOverview(t *testing.T) {
	s := openTemp(t)
	now := tsOffset(0)

	// 三项目：a=active 默认阈值 / b=active 项目级 60 / c=archived（整域排除）/ d=active 零栏目零会话（空态形态）。
	projA := fixtureProject(t, s, "proj-a", "项目A", "active", 900)
	projB := fixtureProject(t, s, "proj-b", "项目B", "active", 60)
	projC := fixtureProject(t, s, "proj-c", "项目C", "archived", 900)
	fixtureProject(t, s, "proj-d", "项目D", "active", 900)
	col05 := fixtureColumn(t, s, projA, "05", "栏目05", "active")
	col06 := fixtureColumn(t, s, projA, "06", "栏目06", "active")
	fixtureColumn(t, s, projA, "09", "栏目09", "archived")        // archived 栏目排除
	col08 := fixtureColumn(t, s, projC, "08", "栏目08", "active") // archived 项目下 active 栏目——项目级排除连带
	col07 := fixtureColumn(t, s, projB, "07", "栏目07", "active")

	// 会话三只：05/06 各一（proj-a），07 一（proj-b）。
	execSess := fixtureSession(t, s, projA, col05, "executor-B", "executor", tsOffset(-100))
	fixtureSession(t, s, projA, col06, "controller-A", "controller", tsOffset(-10))
	fixtureSession(t, s, projB, col07, "executor-C", "executor", tsOffset(-30))

	// 位点两行（05 的 controller/executor，均用真实 seq 量级）+ 一行 archived 栏目位点（装配必须丢弃）
	// + 一行 chat 位点（信箱口径排除）。
	fixturePosition(t, s, projA, col05, "controller", 880)
	fixturePosition(t, s, projC, col08, "executor", 5)
	fixturePosition(t, s, projA, col05, fmt.Sprintf("chat:session:%d", execSess), 999)

	// 消息三条 direct→executor（seq 自增；位点=mB → pending=1，latest=mC）。
	// level 取 normal/important（block 面归 TestUnreceipted，避免混入未回执清单破坏空清单断言）。
	fixtureMessage(t, s, projA, col05, "direct", "executor", 0, execSess, "controller-A@05", "normal", "m1", tsOffset(-50))
	mB := fixtureMessage(t, s, projA, col05, "direct", "executor", 0, execSess, "controller-A@05", "important", "m2", tsOffset(-40))
	mC := fixtureMessage(t, s, projA, col05, "direct", "executor", 0, execSess, "controller-A@05", "normal", "m3", tsOffset(-30))
	fixturePosition(t, s, projA, col05, "executor", mB)

	// 哨兵一只 alive（05 executor 信箱）。
	fixtureSentinel(t, s, execSess, col05, "executor", tsOffset(-5))

	// B3-5 接线夹具：窗配置（05 栏目 S4 跨午夜/S5 同日/S6 停用）+ progress 上报
	// （上报于基准前 60s——查询 now=基准 → 差 60s 正常档；时钟注入仅覆盖写路径，
	// BuildOverview 的 now 为显式传参不受影响）。
	if err := s.SetWindows(projA, col05, []WindowInput{
		{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1},
		{Stage: "S5", From: "08:00", To: "12:00", Enabled: 1},
		{Stage: "S6", From: "08:00", To: "12:00", Enabled: 0}, // 停用：压缩视图不列
	}, execSess, tsOffset(0)); err != nil {
		t.Fatalf("配置时间窗夹具失败: %v", err)
	}
	injectFixedClock(t, tsOffset(-60))
	if _, err := s.InsertProgress(ProgressReport{
		SessionID: execSess, Batch: "B3", Task: "B3-5", CommitHash: "abc1234",
	}); err != nil {
		t.Fatalf("上报进度夹具失败: %v", err)
	}

	ov, err := s.BuildOverview(now, OverviewOpts{WindowAsOf: "10:00"})
	if err != nil {
		t.Fatalf("BuildOverview 失败: %v", err)
	}

	// 顶层面：generated_at=注入时钟；archived 项目整域排除（proj-c 不在）；active 空项目在列。
	if ov.GeneratedAt != now {
		t.Errorf("generated_at = %s，期望注入时钟 %s", ov.GeneratedAt, now)
	}
	if len(ov.Projects) != 3 {
		t.Fatalf("projects 数 = %d，期望 3（proj-c archived 排除，proj-d 空项目在列）: %+v", len(ov.Projects), ov.Projects)
	}
	if ov.Projects[0].Code != "proj-a" || ov.Projects[1].Code != "proj-b" || ov.Projects[2].Code != "proj-d" {
		t.Errorf("projects 顺序/内容 = (%s, %s, %s)，期望 (proj-a, proj-b, proj-d)（ORDER BY code）",
			ov.Projects[0].Code, ov.Projects[1].Code, ov.Projects[2].Code)
	}
	if ov.Projects[0].Code != "proj-a" || ov.Projects[1].Code != "proj-b" {
		t.Errorf("projects 顺序/内容 = (%s, %s)，期望 (proj-a, proj-b)（ORDER BY code）",
			ov.Projects[0].Code, ov.Projects[1].Code)
	}

	// §2.3 项目层字段：code/name/status + columns/sessions 子结构。
	pa := ov.Projects[0]
	if pa.Name != "项目A" || pa.Status != "active" {
		t.Errorf("proj-a 字段 = (%s, %s)，期望 (项目A, active)", pa.Name, pa.Status)
	}
	// archived 栏目（09）排除；archived 项目下的 active 栏目（08）连带排除。
	if len(pa.Columns) != 2 {
		t.Fatalf("proj-a columns 数 = %d，期望 2（09 archived 排除）: %+v", len(pa.Columns), pa.Columns)
	}
	if pa.Columns[0].Code != "05" || pa.Columns[0].Status != "active" {
		t.Errorf("columns[0] = (%s, %s)，期望 (05, active)", pa.Columns[0].Code, pa.Columns[0].Status)
	}
	if pa.Columns[1].Code != "06" {
		t.Errorf("columns[1] = %s，期望 06", pa.Columns[1].Code)
	}

	// §2.3 信箱层字段：角色集=位点行驱动（controller+executor；chat 位点不产生信箱）。
	mb := mailboxAt(t, ov, "proj-a", "05", "executor")
	if mb.Pending != 1 {
		t.Errorf("executor 信箱 pending = %d，期望 1（位点 %d 之后的 direct）", mb.Pending, mB)
	}
	if mb.Position != mB {
		t.Errorf("executor 信箱 position = %d，期望位点 %d", mb.Position, mB)
	}
	if mb.Latest == nil {
		t.Fatal("executor 信箱 latest = nil，期望最新一条 direct")
	}
	if mb.Latest.Seq != mC || mb.Latest.Level != "normal" || mb.Latest.CreatedAt != tsOffset(-30) {
		t.Errorf("executor 信箱 latest = %+v，期望 (seq=%d, normal, %s)", mb.Latest, mC, tsOffset(-30))
	}
	if mb.Sentinel != "alive" {
		t.Errorf("executor 信箱 sentinel = %s，期望 alive", mb.Sentinel)
	}
	mbc := mailboxAt(t, ov, "proj-a", "05", "controller")
	if mbc.Pending != 0 || mbc.Latest != nil || mbc.Sentinel != "none" {
		t.Errorf("controller 信箱 = %+v，期望 pending 0/latest nil（无消息）/sentinel none（无哨兵）", mbc)
	}
	if got := len(pa.Columns[0].Mailboxes); got != 2 {
		t.Errorf("栏目 05 信箱数 = %d，期望 2（chat: 位点不进信箱角色集）", got)
	}

	// §2.3 windows 压缩视图（B3-5 接线）：只列已配置启用阶段；as_of=10:00 判定
	// S4 跨午夜窗外=waiting、S5 同日窗内=allowed、S6 停用不列；06 未配置=空 map。
	c05 := pa.Columns[0]
	if got := c05.Windows["S4"]; got.Status != "waiting" || got.Start != "23:00" || got.End != "09:00" {
		t.Errorf("05.Windows[S4] = %+v，期望 {waiting 23:00 09:00}（as_of=10:00 跨午夜窗外）", got)
	}
	if got := c05.Windows["S5"]; got.Status != "allowed" || got.Start != "08:00" || got.End != "12:00" {
		t.Errorf("05.Windows[S5] = %+v，期望 {allowed 08:00 12:00}（as_of=10:00 窗内）", got)
	}
	if _, ok := c05.Windows["S6"]; ok {
		t.Errorf("05.Windows[S6] 出现，停用窗不进压缩视图（AC3.4 视同未配置）: %+v", c05.Windows)
	}
	if len(c05.Windows) != 2 {
		t.Errorf("05.Windows 条目数 = %d，期望 2", len(c05.Windows))
	}
	if c06 := pa.Columns[1]; c06.Windows == nil || len(c06.Windows) != 0 {
		t.Errorf("06.Windows = %+v，期望空 map（未配置栏目键恒在非 nil）", c06.Windows)
	}

	// §2.3 会话层字段（详判定归 TestSessionLost）。
	if len(pa.Sessions) != 2 {
		t.Fatalf("proj-a sessions 数 = %d，期望 2", len(pa.Sessions))
	}
	se := pa.Sessions[0]
	if se.Name != "executor-B" || se.Column != "05" || se.Role != "executor" ||
		se.LastSeenAt != tsOffset(-100) || !se.Alive {
		t.Errorf("sessions[0] = %+v，期望 executor-B/05/executor/%s/alive", se, tsOffset(-100))
	}
	if se.ID != execSess {
		t.Errorf("sessions[0].id = %d，期望 %d（#27 口径带 id）", se.ID, execSess)
	}

	// FR23 progress 段（AC23.3）：挂最近上报会话；无上报会话不出键（omitempty）。
	if se.Progress == nil {
		t.Fatal("executor-B 缺 progress 段（AC23.3 装配缺失）")
	}
	if se.Progress.CommitHash != "abc1234" || se.Progress.Batch != "B3" || se.Progress.Task != "B3-5" ||
		se.Progress.CreatedAt != tsOffset(-60) || se.Progress.Stale != ProgressStaleNone {
		t.Errorf("executor-B progress = %+v，期望 {abc1234 B3 B3-5 %s 正常档}",
			se.Progress, tsOffset(-60))
	}
	if pa.Sessions[1].Progress != nil {
		t.Errorf("controller-A progress = %+v，期望 nil（无上报不出键）", pa.Sessions[1].Progress)
	}
	pb := ov.Projects[1]
	if len(pb.Sessions) != 1 || pb.Sessions[0].Name != "executor-C" || !pb.Sessions[0].Alive {
		t.Errorf("proj-b sessions = %+v，期望 executor-C alive（30s < 项目级 60s 阈值）", pb.Sessions)
	}

	// block_unreceipted/resources_summary 结构在位：空夹具下空清单+零计数。
	if ov.BlockUnreceipted == nil || len(ov.BlockUnreceipted) != 0 {
		t.Errorf("block_unreceipted = %+v，期望空数组（非 nil）", ov.BlockUnreceipted)
	}
	if ov.ResourcesSummary.InUseCount != 0 || len(ov.ResourcesSummary.ByType) != 0 {
		t.Errorf("resources_summary = %+v，期望零值空 map", ov.ResourcesSummary)
	}

	// 空项目形态（proj-d）：零栏目零会话，子结构均为空数组非 nil。
	pd := ov.Projects[2]
	if pd.Name != "项目D" || pd.Status != "active" {
		t.Errorf("proj-d 字段 = (%s, %s)，期望 (项目D, active)", pd.Name, pd.Status)
	}
	if pd.Columns == nil || len(pd.Columns) != 0 {
		t.Errorf("proj-d columns = %+v，期望空数组（非 nil）", pd.Columns)
	}
	if pd.Sessions == nil || len(pd.Sessions) != 0 {
		t.Errorf("proj-d sessions = %+v，期望空数组（非 nil）", pd.Sessions)
	}

	// 空态 json 形态锚定：[]/{} 而非 null（§2.3 结构键恒在）。
	b, err := json.Marshal(ov)
	if err != nil {
		t.Fatalf("marshal overview 失败: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"block_unreceipted":[]`, `"by_type":{}`, `"sentinels":[]`,
		`"columns":[]`, `"sessions":[]`} {
		if !strings.Contains(got, want) {
			t.Errorf("overview json 缺 %s 形态（空态须为数组/对象字面量非 null）:\n%s", want, got)
		}
	}
	// windows 段锚定（B3-5 接线反转）：压缩视图条目形态照 §2.3 示例（key=stage，
	// value={status,start,end}）+ 未配置栏目空对象（键恒在非 null）。
	for _, want := range []string{
		`"windows":{"S4":{"status":"waiting","start":"23:00","end":"09:00"}`,
		`"windows":{}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("overview json 缺 %s 形态（§2.3 windows 压缩视图）:\n%s", want, got)
		}
	}
	// progress 段 json 形态：五键齐 + 无上报会话无 progress 键（omitempty）。
	for _, want := range []string{
		`"progress":{"commit_hash":"abc1234","batch":"B3","task":"B3-5","created_at":"` + tsOffset(-60) + `","stale":""}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("overview json 缺 %s 形态（AC23.3 progress 段）:\n%s", want, got)
		}
	}

	// 单栏目模式：同函数带过滤——仅 proj-a/05 输出，往返与结构不变（windows 同源装配）。
	ov1, err := s.BuildOverview(now, OverviewOpts{
		Column:     &ColumnScope{ProjectCode: "proj-a", ColumnCode: "05"},
		WindowAsOf: "10:00",
	})
	if err != nil {
		t.Fatalf("单栏目 BuildOverview 失败: %v", err)
	}
	if len(ov1.Projects) != 1 || ov1.Projects[0].Code != "proj-a" {
		t.Fatalf("单栏目 projects = %+v，期望仅 proj-a", ov1.Projects)
	}
	if len(ov1.Projects[0].Columns) != 1 || ov1.Projects[0].Columns[0].Code != "05" {
		t.Errorf("单栏目 columns = %+v，期望仅 05", ov1.Projects[0].Columns)
	}
	if got := ov1.Projects[0].Columns[0].Windows["S4"]; got.Status != "waiting" {
		t.Errorf("单栏目 windows[S4] = %+v，期望 waiting（窗装配与全量模式同源）", got)
	}
}

// ===== TestBuildOverviewIncludeArchived：all=1 扩面（#17 all 参数，总控 #20 裁定）=====

// TestBuildOverviewIncludeArchived IncludeArchived=true：archived 项目在列
// （ORDER BY code 不变、status 原样透出），其下 active 栏目/信箱/会话照常聚合
// （archived 栏目仍按自身状态排除——--all 语义域=项目生命周期态）；缺省零值面
// 不变（archived 整域排除，TestBuildOverview 已对拍，此处缺省对照独立复核防
// 扩面实现误染缺省）。
func TestBuildOverviewIncludeArchived(t *testing.T) {
	s := openTemp(t)
	now := tsOffset(0)

	// proj-a active（05 active 栏目+会话）对照 / proj-z archived（01 active 栏目
	// +02 archived 栏目+会话+位点行）扩面断言主体。
	projA := fixtureProject(t, s, "proj-a", "项目A", "active", 900)
	projZ := fixtureProject(t, s, "proj-z", "项目Z", "archived", 900)
	col05 := fixtureColumn(t, s, projA, "05", "栏目05", "active")
	col01 := fixtureColumn(t, s, projZ, "01", "栏目01", "active")
	fixtureColumn(t, s, projZ, "02", "栏目02", "archived") // archived 栏目：不随项目扩面放行
	fixtureSession(t, s, projA, col05, "executor-A", "executor", tsOffset(-10))
	fixtureSession(t, s, projZ, col01, "zombie-Z", "executor", tsOffset(-10))
	fixturePosition(t, s, projZ, col01, "executor", 7) // 位点行=proj-z 信箱出现面（无消息 pending=0）

	// 缺省（零值 opts）：archived 整域排除——扩面未误染缺省面。
	ov, err := s.BuildOverview(now, OverviewOpts{})
	if err != nil {
		t.Fatalf("缺省 BuildOverview 失败: %v", err)
	}
	if len(ov.Projects) != 1 || ov.Projects[0].Code != "proj-a" {
		t.Fatalf("缺省 projects = %+v，期望仅 proj-a（archived 整域排除不受扩面实现误染）", ov.Projects)
	}

	// IncludeArchived=true：proj-z 在列，ORDER BY code 不变，status 原样透出。
	ovAll, err := s.BuildOverview(now, OverviewOpts{IncludeArchived: true})
	if err != nil {
		t.Fatalf("扩面 BuildOverview 失败: %v", err)
	}
	if len(ovAll.Projects) != 2 ||
		ovAll.Projects[0].Code != "proj-a" || ovAll.Projects[1].Code != "proj-z" {
		t.Fatalf("扩面 projects = %+v，期望 [proj-a proj-z]（ORDER BY code 不变）", ovAll.Projects)
	}
	pz := ovAll.Projects[1]
	if pz.Status != "archived" || pz.Name != "项目Z" {
		t.Errorf("proj-z 字段 = (%s, %s)，期望 (项目Z, archived)（status 原样透出）", pz.Name, pz.Status)
	}
	// archived 项目下 active 栏目（01）在列；archived 栏目（02）仍排除。
	if len(pz.Columns) != 1 || pz.Columns[0].Code != "01" {
		t.Fatalf("proj-z columns = %+v，期望仅 01（02 archived 栏目按自身状态仍排除）", pz.Columns)
	}
	// 信箱照常聚合（位点行出现面：executor pos=7、pending=0）。
	mbz := mailboxAt(t, ovAll, "proj-z", "01", "executor")
	if mbz.Position != 7 || mbz.Pending != 0 {
		t.Errorf("proj-z executor 信箱 = %+v，期望 position=7 pending=0（无消息）", mbz)
	}
	// 会话照常聚合（archived 项目会话随扩面在列）。
	if len(pz.Sessions) != 1 || pz.Sessions[0].Name != "zombie-Z" || !pz.Sessions[0].Alive {
		t.Errorf("proj-z sessions = %+v，期望 zombie-Z alive（archived 项目会话随扩面在列）", pz.Sessions)
	}
}

// ===== TestPendingCounts：direct 按角色（位点之后）/ bus 仅 controller / chat 不进信箱 / latest 含已消费 =====

func TestPendingCounts(t *testing.T) {
	s := openTemp(t)
	now := tsOffset(0)

	projA := fixtureProject(t, s, "proj-a", "项目A", "active", 900)
	col05 := fixtureColumn(t, s, projA, "05", "栏目05", "active")
	execSess := fixtureSession(t, s, projA, col05, "executor-B", "executor", tsOffset(-10))
	ctlSess := fixtureSession(t, s, projA, col05, "controller-A", "controller", tsOffset(-5))

	// 消息编排（seq 按插入顺序自增，全部经返回值记录）：
	//  m1,m2: direct→executor —— m2 及之前=已消费（executor 位点=m2）
	//  m3,m4: direct→executor 未消费
	//  m5:    direct→controller（controller 位点=m4 → m5 未消费）
	//  m6,m7: bus（挂 col-05）——仅 controller 信箱计入；m7 未消费（位点 m4<m7）
	//  m8:    chat→executor 会话；m9: receipt→controller 会话——对话流，不进任何信箱
	fixtureMessage(t, s, projA, col05, "direct", "executor", 0, ctlSess, "controller-A@05", "normal", "d1", tsOffset(-300))
	m2 := fixtureMessage(t, s, projA, col05, "direct", "executor", 0, ctlSess, "controller-A@05", "important", "d2", tsOffset(-200))
	fixtureMessage(t, s, projA, col05, "direct", "executor", 0, ctlSess, "controller-A@05", "normal", "d3", tsOffset(-150))
	m4 := fixtureMessage(t, s, projA, col05, "direct", "executor", 0, ctlSess, "controller-A@05", "block", "d4", tsOffset(-100))
	fixtureMessage(t, s, projA, col05, "direct", "controller", 0, ctlSess, "controller-A@05", "normal", "dc", tsOffset(-90))
	m6 := fixtureMessage(t, s, projA, col05, "bus", "", 0, ctlSess, "controller-A@05", "important", "b1", tsOffset(-80))
	m7 := fixtureMessage(t, s, projA, col05, "bus", "", 0, ctlSess, "controller-A@05", "block", "b2", tsOffset(-70))
	fixtureMessage(t, s, projA, col05, "chat", "", execSess, ctlSess, "controller-A@05", "normal", "c1", tsOffset(-60))
	fixtureMessage(t, s, projA, col05, "receipt", "", ctlSess, execSess, "system", "normal", "receipt: seq", tsOffset(-50))

	fixturePosition(t, s, projA, col05, "executor", m2)   // m3/m4 未消费
	fixturePosition(t, s, projA, col05, "controller", m6) // direct m5/bus m6 已消费，m7 未消费
	fixturePosition(t, s, projA, col05, "worker", 0)      // 位点行存在零消息 → 信箱角色集仍含 worker
	// chat 位点（consumer=chat:session:<id>）→ 信箱角色集排除（对话流非信箱维度）。
	fixturePosition(t, s, projA, col05, fmt.Sprintf("chat:session:%d", execSess), 999)

	// 多项目隔离面：proj-b 同 code 栏目 "05"+一条 bus——bus 只进发送项目信箱，
	// 同 code 栏目各归各项目互不串（GROUP BY project_id 隔离分支覆盖）。
	projB := fixtureProject(t, s, "proj-b", "项目B", "active", 900)
	colB05 := fixtureColumn(t, s, projB, "05", "项目B栏目05", "active")
	bCtlSess := fixtureSession(t, s, projB, colB05, "controller-B", "controller", tsOffset(-5))
	fixturePosition(t, s, projB, colB05, "controller", 0)
	busB := fixtureMessage(t, s, projB, colB05, "bus", "", 0, bCtlSess, "controller-B@05", "normal", "b-b", tsOffset(-40))

	ov, err := s.BuildOverview(now, OverviewOpts{})
	if err != nil {
		t.Fatalf("BuildOverview 失败: %v", err)
	}

	// executor 信箱：pending=2（m3,m4 位点之后）；bus/chat/receipt 一律不计入。
	mbExec := mailboxAt(t, ov, "proj-a", "05", "executor")
	if mbExec.Pending != 2 {
		t.Errorf("executor pending = %d，期望 2（m3,m4；bus 对非 controller 不计）", mbExec.Pending)
	}
	if mbExec.Position != m2 {
		t.Errorf("executor position = %d，期望 %d", mbExec.Position, m2)
	}
	if mbExec.Latest == nil || mbExec.Latest.Seq != m4 || mbExec.Latest.Level != "block" {
		t.Errorf("executor latest = %+v，期望 (seq=%d, block)——该信箱最新一条 direct", mbExec.Latest, m4)
	}

	// controller 信箱：pending=1（bus m7；direct m5/bus m6 已消费不计）——bus 仅 controller 计入。
	mbCtl := mailboxAt(t, ov, "proj-a", "05", "controller")
	if mbCtl.Pending != 1 {
		t.Errorf("controller pending = %d，期望 1（bus m7；direct m5/bus m6 已消费不计；chat/receipt 不进信箱口径）", mbCtl.Pending)
	}
	if mbCtl.Position != m6 {
		t.Errorf("controller position = %d，期望 %d", mbCtl.Position, m6)
	}
	// latest=bus m7（direct/bus 两分支最新，不限位点——m6 已消费仍是最新之一，m7 更新）。
	if mbCtl.Latest == nil || mbCtl.Latest.Seq != m7 || mbCtl.Latest.Level != "block" || mbCtl.Latest.CreatedAt != tsOffset(-70) {
		t.Errorf("controller latest = %+v，期望 (seq=%d, block, %s)——bus 与 direct 取最新", mbCtl.Latest, m7, tsOffset(-70))
	}

	// worker 信箱：位点行驱动入集，零消息零待消费。
	mbWorker := mailboxAt(t, ov, "proj-a", "05", "worker")
	if mbWorker.Pending != 0 || mbWorker.Position != 0 || mbWorker.Latest != nil {
		t.Errorf("worker 信箱 = %+v，期望 pending 0/position 0/latest nil", mbWorker)
	}

	// 多项目隔离：proj-b 的 bus 只进 proj-b 信箱；同 code 栏目 "05" 各归各项目互不串。
	mbBCtl := mailboxAt(t, ov, "proj-b", "05", "controller")
	if mbBCtl.Pending != 1 || mbBCtl.Position != 0 {
		t.Errorf("proj-b/05/controller = (pending %d, position %d)，期望 (1, 0)——bus 只进发送项目", mbBCtl.Pending, mbBCtl.Position)
	}
	if mbBCtl.Latest == nil || mbBCtl.Latest.Seq != busB {
		t.Errorf("proj-b/05/controller latest = %+v，期望 seq=%d（仅本项目 bus）", mbBCtl.Latest, busB)
	}
	if got := mailboxAt(t, ov, "proj-a", "05", "controller"); got.Pending != 1 {
		t.Errorf("proj-a/05/controller pending = %d，期望仍 1（proj-b 的 bus 不串入）", got.Pending)
	}
	// bus 不产生 executor 信箱：proj-b/05 无 executor 位点无 direct 消息 → 不出现。
	if ov.Projects[1].Columns[0].Mailboxes != nil && len(ov.Projects[1].Columns[0].Mailboxes) != 1 {
		t.Errorf("proj-b/05 信箱数 = %d，期望 1（仅 controller；bus 不产生 executor 信箱）",
			len(ov.Projects[1].Columns[0].Mailboxes))
	}

	// 信箱总数=3：controller/executor/worker——chat: 位点不产生信箱。
	mails := ov.Projects[0].Columns[0].Mailboxes
	if len(mails) != 3 {
		t.Errorf("信箱总数 = %d，期望 3（chat: 位点排除面）: %+v", len(mails), mails)
	}
	// 角色字典序稳定输出。
	roles := []string{mails[0].Role, mails[1].Role, mails[2].Role}
	if !slices.Equal(roles, []string{"controller", "executor", "worker"}) {
		t.Errorf("信箱角色序 = %v，期望 [controller executor worker]", roles)
	}

	// latest 含已消费锚定：位点推到 m7 之后重跑——pending 清零，latest 仍=各自最新一条（已消费照显）。
	if _, err := s.DB.Exec(`UPDATE ack_positions SET position = ? WHERE column_id = ? AND consumer IN ('executor','controller')`,
		m7, col05); err != nil {
		t.Fatalf("推进位点失败: %v", err)
	}
	ov2, err := s.BuildOverview(now, OverviewOpts{})
	if err != nil {
		t.Fatalf("位点推进后重跑失败: %v", err)
	}
	if got := mailboxAt(t, ov2, "proj-a", "05", "executor"); got.Pending != 0 {
		t.Errorf("位点全消费后 executor pending = %d，期望 0", got.Pending)
	}
	if got := mailboxAt(t, ov2, "proj-a", "05", "controller"); got.Latest == nil || got.Latest.Seq != m7 {
		t.Errorf("位点全消费后 controller latest = %+v，期望仍为 seq=%d（已消费照显）", got.Latest, m7)
	}
}

// ===== TestSessionLost：默认 900 / 项目级 60 覆盖 / 恰等边界不 lost / lost_for_sec 值 =====

func TestSessionLost(t *testing.T) {
	s := openTemp(t)
	now := tsOffset(0)

	projA := fixtureProject(t, s, "proj-a", "项目A", "active", 900) // 默认阈值面（opts 零值）
	projB := fixtureProject(t, s, "proj-b", "项目B", "active", 60)  // 项目级覆盖面
	projC := fixtureProject(t, s, "proj-c", "项目C", "archived", 0) // archived 项目会话不判定展示
	colA := fixtureColumn(t, s, projA, "05", "栏目05", "active")
	colB := fixtureColumn(t, s, projB, "07", "栏目07", "active")
	colC := fixtureColumn(t, s, projC, "08", "栏目08", "active")

	// proj-a（阈值 900）：超 1s lost / 恰等不 lost（`>` 严格）/ 阈内 alive。
	fixtureSession(t, s, projA, colA, "s-lost", "executor", tsOffset(-901))
	fixtureSession(t, s, projA, colA, "s-eq", "executor", tsOffset(-900))
	fixtureSession(t, s, projA, colA, "s-alive", "executor", tsOffset(-899))
	// proj-b（阈值 60）：超 1s lost / 恰等不 lost。
	fixtureSession(t, s, projB, colB, "s-b-lost", "controller", tsOffset(-61))
	fixtureSession(t, s, projB, colB, "s-b-eq", "controller", tsOffset(-60))
	// archived 项目：会话行存在但不进聚合。
	fixtureSession(t, s, projC, colC, "s-archived", "executor", tsOffset(-99999))

	ov, err := s.BuildOverview(now, OverviewOpts{})
	if err != nil {
		t.Fatalf("BuildOverview 失败: %v", err)
	}

	sessionAt := func(projectCode, name string) SessionEntry {
		t.Helper()
		pi := slices.IndexFunc(ov.Projects, func(p ProjectOverview) bool { return p.Code == projectCode })
		if pi < 0 {
			t.Fatalf("overview 缺项目 %s", projectCode)
		}
		si := slices.IndexFunc(ov.Projects[pi].Sessions, func(x SessionEntry) bool { return x.Name == name })
		if si < 0 {
			t.Fatalf("项目 %s 缺会话 %s", projectCode, name)
		}
		return ov.Projects[pi].Sessions[si]
	}

	cases := []struct {
		name        string
		session     string
		project     string
		wantAlive   bool
		wantLostFor int64
		wantColumn  string
	}{
		{"超阈1s_lost", "s-lost", "proj-a", false, 901, "05"},
		{"恰等900_不lost", "s-eq", "proj-a", true, 900, "05"},
		{"阈内_alive", "s-alive", "proj-a", true, 899, "05"},
		{"项目级60覆盖_超1s_lost", "s-b-lost", "proj-b", false, 61, "07"},
		{"项目级60覆盖_恰等不lost", "s-b-eq", "proj-b", true, 60, "07"},
	}
	for _, c := range cases {
		got := sessionAt(c.project, c.session)
		if got.Alive != c.wantAlive {
			t.Errorf("%s alive = %v，期望 %v", c.name, got.Alive, c.wantAlive)
		}
		if got.LostForSec != c.wantLostFor {
			t.Errorf("%s lost_for_sec = %d，期望 %d", c.name, got.LostForSec, c.wantLostFor)
		}
		if got.Column != c.wantColumn {
			t.Errorf("%s column = %s，期望 %s", c.name, got.Column, c.wantColumn)
		}
	}
	if pi := slices.IndexFunc(ov.Projects, func(p ProjectOverview) bool { return p.Code == "proj-c" }); pi >= 0 {
		t.Errorf("archived 项目 proj-c 不应出现在聚合（会话不判定展示）")
	}
}

// ===== TestSentinelStates：mailboxes[].sentinel 三态（alive/dead/none）+ sessions[].sentinels 装配 =====

func TestSentinelStates(t *testing.T) {
	s := openTemp(t)
	now := tsOffset(0)

	projA := fixtureProject(t, s, "proj-a", "项目A", "active", 900)
	col05 := fixtureColumn(t, s, projA, "05", "栏目05", "active")
	col06 := fixtureColumn(t, s, projA, "06", "栏目06", "active")
	execSess := fixtureSession(t, s, projA, col05, "executor-B", "executor", tsOffset(-10))
	execSess2 := fixtureSession(t, s, projA, col05, "executor-B2", "executor", tsOffset(-10))
	fixtureSession(t, s, projA, col05, "controller-A", "controller", tsOffset(-10)) // 无哨兵会话面
	ctl2Sess := fixtureSession(t, s, projA, col06, "controller-C06", "controller", tsOffset(-10))

	// 三态编排：05/executor=alive+dead 混合（任一 alive→alive；两哨兵分属两会话——
	// UNIQUE(session,column,role) 同会话同信箱单行）；06/controller=全 dead→dead；
	// 05/controller=无哨兵→none。sentinel timeout 取默认 15（5s 前 alive / 60s 前 dead）。
	// 信箱角色集由位点行驱动（哨兵行不产生信箱——b3-spec #17 口径仅位点∪消息两来源）。
	fixturePosition(t, s, projA, col05, "executor", 0)
	fixturePosition(t, s, projA, col05, "controller", 0)
	fixturePosition(t, s, projA, col06, "controller", 0)
	aliveID := fixtureSentinel(t, s, execSess, col05, "executor", tsOffset(-5))
	deadID := fixtureSentinel(t, s, execSess2, col05, "executor", tsOffset(-60))
	fixtureSentinel(t, s, ctl2Sess, col06, "controller", tsOffset(-60))

	ov, err := s.BuildOverview(now, OverviewOpts{})
	if err != nil {
		t.Fatalf("BuildOverview 失败: %v", err)
	}

	if got := mailboxAt(t, ov, "proj-a", "05", "executor").Sentinel; got != "alive" {
		t.Errorf("05/executor sentinel = %s，期望 alive（任一哨兵存活即 alive）", got)
	}
	if got := mailboxAt(t, ov, "proj-a", "06", "controller").Sentinel; got != "dead" {
		t.Errorf("06/controller sentinel = %s，期望 dead（有行全 dead）", got)
	}
	if got := mailboxAt(t, ov, "proj-a", "05", "controller").Sentinel; got != "none" {
		t.Errorf("05/controller sentinel = %s，期望 none（无哨兵行）", got)
	}

	// sessions[].sentinels 装配（复用活性清单按 session 分组——#27 口径 id/role/alive）；
	// 同信箱两只哨兵分属 executor-B/executor-B2，各自挂回各自会话=分组正确性锚定。
	sessionByName := func(name string) SessionEntry {
		t.Helper()
		for _, se := range ov.Projects[0].Sessions {
			if se.Name == name {
				return se
			}
		}
		t.Fatalf("缺会话 %s", name)
		return SessionEntry{}
	}
	execEntry := sessionByName("executor-B")
	execEntry2 := sessionByName("executor-B2")
	if len(execEntry.Sentinels) != 1 || execEntry.Sentinels[0].ID != aliveID || !execEntry.Sentinels[0].Alive {
		t.Errorf("executor-B 哨兵 = %+v，期望仅 alive 行 %d", execEntry.Sentinels, aliveID)
	}
	if len(execEntry2.Sentinels) != 1 || execEntry2.Sentinels[0].ID != deadID || execEntry2.Sentinels[0].Alive {
		t.Errorf("executor-B2 哨兵 = %+v，期望仅 dead 行 %d", execEntry2.Sentinels, deadID)
	}
	for _, st := range append(execEntry.Sentinels, execEntry2.Sentinels...) {
		if st.Role != "executor" {
			t.Errorf("哨兵 %d role = %s，期望 executor", st.ID, st.Role)
		}
	}

	for _, se := range ov.Projects[0].Sessions {
		if se.Name == "controller-A" && len(se.Sentinels) != 0 {
			t.Errorf("controller-A 会话哨兵数 = %d，期望 0（空数组非 nil）", len(se.Sentinels))
		}
	}
}

// ===== TestUnreceipted：block 未回执列出 / 已回执排除 / 非 block 排除 =====

func TestUnreceipted(t *testing.T) {
	s := openTemp(t)
	now := tsOffset(0)

	projA := fixtureProject(t, s, "proj-a", "项目A", "active", 900)
	col05 := fixtureColumn(t, s, projA, "05", "栏目05", "active")
	sender := fixtureSession(t, s, projA, col05, "controller-A", "controller", tsOffset(-10))
	rcpt := fixtureSession(t, s, projA, col05, "executor-B", "executor", tsOffset(-5))

	// b1: block 未回执 → 列出；b2: block 已回执 → 排除；n1/i1: 非 block → 排除；
	// b3: 另一发送方 block 未回执 → 列出（seq DESC 排序：b3 最先）。
	b1 := fixtureMessage(t, s, projA, col05, "direct", "executor", rcpt, sender, "controller-A@05", "block", "halt-1", tsOffset(-100))
	b2 := fixtureMessage(t, s, projA, col05, "direct", "executor", rcpt, sender, "controller-A@05", "block", "halt-2", tsOffset(-90))
	fixtureMessage(t, s, projA, col05, "direct", "executor", rcpt, sender, "controller-A@05", "normal", "plain", tsOffset(-80))
	fixtureMessage(t, s, projA, col05, "direct", "executor", rcpt, sender, "controller-A@05", "important", "urg", tsOffset(-70))
	b3 := fixtureMessage(t, s, projA, col05, "direct", "controller", sender, rcpt, "executor-B@05", "block", "halt-3", tsOffset(-60))
	fixtureReceipt(t, s, b2, rcpt)

	ov, err := s.BuildOverview(now, OverviewOpts{})
	if err != nil {
		t.Fatalf("BuildOverview 失败: %v", err)
	}

	if len(ov.BlockUnreceipted) != 2 {
		t.Fatalf("block_unreceipted 数 = %d，期望 2（b2 已回执 + 两非 block 排除）: %+v",
			len(ov.BlockUnreceipted), ov.BlockUnreceipted)
	}
	// ORDER BY seq DESC：b3(seq 大) 在前。
	first, second := ov.BlockUnreceipted[0], ov.BlockUnreceipted[1]
	if first.Seq != b3 || second.Seq != b1 {
		t.Errorf("清单 seq = (%d, %d)，期望 DESC (%d, %d)", first.Seq, second.Seq, b3, b1)
	}
	if first.Target != "proj-a/05/controller" || first.Sender != "executor-B@05" ||
		first.Level != "block" || first.CreatedAt != tsOffset(-60) {
		t.Errorf("b3 字段 = %+v，期望 target=proj-a/05/controller sender=executor-B@05 block %s", first, tsOffset(-60))
	}
	if second.Target != "proj-a/05/executor" || second.Sender != "controller-A@05" {
		t.Errorf("b1 字段 = %+v，期望 target=proj-a/05/executor sender=controller-A@05", second)
	}
}

// ===== TestResourcesSummary：空表={in_use_count:0, by_type:{}}（空 map 非 null）/ 有行计数 =====

func TestResourcesSummary(t *testing.T) {
	s := openTemp(t)
	now := tsOffset(0)

	projA := fixtureProject(t, s, "proj-a", "项目A", "active", 900)
	col05 := fixtureColumn(t, s, projA, "05", "栏目05", "active")
	creator := fixtureSession(t, s, projA, col05, "controller-A", "controller", tsOffset(-10))

	// 空表形态：零计数 + 空 map（marshal 为 {} 非 null——TestBuildOverview 已锚定 json，此处锚定值）。
	ovEmpty, err := s.BuildOverview(now, OverviewOpts{})
	if err != nil {
		t.Fatalf("空表 BuildOverview 失败: %v", err)
	}
	if ovEmpty.ResourcesSummary.InUseCount != 0 {
		t.Errorf("空表 in_use_count = %d，期望 0", ovEmpty.ResourcesSummary.InUseCount)
	}
	if ovEmpty.ResourcesSummary.ByType == nil || len(ovEmpty.ResourcesSummary.ByType) != 0 {
		t.Errorf("空表 by_type = %+v，期望非 nil 空 map", ovEmpty.ResourcesSummary.ByType)
	}

	// 有行：port×3 + account_range×2 + data_range×1（in_use 共 6）+ released port 1（不计）。
	fixtureResource(t, s, projA, col05, "port", "8080", "", 8080, 8080, "in_use", creator)
	fixtureResource(t, s, projA, col05, "port", "8081", "", 8081, 8081, "in_use", creator)
	fixtureResource(t, s, projA, col05, "port", "8082", "", 8082, 8082, "in_use", creator)
	fixtureResource(t, s, projA, col05, "account_range", "acct:1000-1999", "acct:", 1000, 1999, "in_use", creator)
	fixtureResource(t, s, projA, col05, "account_range", "acct:2000-2999", "acct:", 2000, 2999, "in_use", creator)
	fixtureResource(t, s, projA, col05, "data_range", "data:10-20", "data:", 10, 20, "in_use", creator)
	fixtureResource(t, s, projA, col05, "port", "9090", "", 9090, 9090, "released", creator)

	ov, err := s.BuildOverview(now, OverviewOpts{})
	if err != nil {
		t.Fatalf("有行 BuildOverview 失败: %v", err)
	}
	sum := ov.ResourcesSummary
	if sum.InUseCount != 6 {
		t.Errorf("in_use_count = %d，期望 6（released 不计）", sum.InUseCount)
	}
	want := map[string]int64{"port": 3, "account_range": 2, "data_range": 1}
	if len(sum.ByType) != len(want) {
		t.Fatalf("by_type = %+v，期望 %v", sum.ByType, want)
	}
	for k, v := range want {
		if sum.ByType[k] != v {
			t.Errorf("by_type[%s] = %d，期望 %d", k, sum.ByType[k], v)
		}
	}
}

// ===== TestRoundtripBudget：§3.7 机械闸——聚合全程 ≤13 条查询且不随栏目数增长 =====
//
// 基线构成（b3-W1 增补后上调 12→13，主会话呈报总控入账）：§3.7 基线 9 条
// + B4 窗聚合接线（ListWindowStatus 栏目清单+窗行两查）+ FR23 progress 增补
// （LatestProgressBySession 一查）+ Q1 聚合 1（b3-W1 会话未读单条 IN 查询）= 13；
// 灵魂断言「不随栏目数增长」（n2==n1）不变。
//
// 查询计数钩子：测试侧 driver/conn/stmt 三层 wrapper（sql.OpenDB 自定义 connector），
// 只拦 Query 类入口计数（BuildOverview 纯读）；生产代码零改动。

// countingDriver 计数 wrapper 的 connector：底层复用 modernc 驱动实例（零值可用）。
type countingConnector struct {
	dsn string
	n   *int
}

// countingInnerDriver 共享底层驱动实例（connector 每连接经其 Open 产 driver.Conn）。
var countingInnerDriver = &sqlite.Driver{}

func (c *countingConnector) Connect(context.Context) (driver.Conn, error) {
	cn, err := countingInnerDriver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &countingConn{inner: cn, n: c.n}, nil
}

func (c *countingConnector) Driver() driver.Driver { return countingInnerDriver }

// countingConn 包装连接：Query 类入口计数（conn 直接查询与 prepared 语句两路径互斥不双计
// ——database/sql 语义：QueryerContext 非 ErrSkip 失败即返回，不再走 prepared）。
type countingConn struct {
	inner driver.Conn
	n     *int
}

func (c *countingConn) Prepare(query string) (driver.Stmt, error) {
	st, err := c.inner.Prepare(query)
	if err != nil {
		return nil, err
	}
	return countingStmt{inner: st, n: c.n}, nil
}

func (c *countingConn) Close() error              { return c.inner.Close() }
func (c *countingConn) Begin() (driver.Tx, error) { return c.inner.Begin() }

func (c *countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	p, ok := c.inner.(driver.ConnPrepareContext)
	if !ok {
		return c.Prepare(query)
	}
	st, err := p.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return countingStmt{inner: st, n: c.n}, nil
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	*c.n++
	return q.QueryContext(ctx, query, args)
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return e.ExecContext(ctx, query, args)
}

// countingStmt 包装语句：prepared 路径 Query 类入口计数。
type countingStmt struct {
	inner driver.Stmt
	n     *int
}

func (s countingStmt) Close() error  { return s.inner.Close() }
func (s countingStmt) NumInput() int { return s.inner.NumInput() }

func (s countingStmt) Exec(args []driver.Value) (driver.Result, error) { return s.inner.Exec(args) }

func (s countingStmt) Query(args []driver.Value) (driver.Rows, error) {
	*s.n++
	return s.inner.Query(args)
}

func (s countingStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.inner.(driver.StmtExecContext).ExecContext(ctx, args)
}

func (s countingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	*s.n++
	return s.inner.(driver.StmtQueryContext).QueryContext(ctx, args)
}

// openCountingStore 构造带查询计数的 store（真文件库+生产同款 DSN PRAGMA；计数指针由调用方持有）。
func openCountingStore(t *testing.T) (*Store, *int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aiteam.db")
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)"
	n := new(int)
	db := sql.OpenDB(&countingConnector{dsn: dsn, n: n})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{DB: db}
	if err := s.migrate(); err != nil { // 迁移发生在计数窗口之前，不计入
		t.Fatalf("计数库迁移失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, n
}

func TestRoundtripBudget(t *testing.T) {
	s, counter := openCountingStore(t)
	now := tsOffset(0)

	projA := fixtureProject(t, s, "proj-a", "项目A", "active", 900)
	col05 := fixtureColumn(t, s, projA, "05", "栏目05", "active")
	col06 := fixtureColumn(t, s, projA, "06", "栏目06", "active")
	execSess := fixtureSession(t, s, projA, col05, "executor-B", "executor", tsOffset(-10))
	ctlSess := fixtureSession(t, s, projA, col06, "controller-A", "controller", tsOffset(-10))
	fixturePosition(t, s, projA, col05, "executor", 1)
	fixturePosition(t, s, projA, col05, "controller", 1)
	fixturePosition(t, s, projA, col06, "controller", 1)
	fixtureMessage(t, s, projA, col05, "direct", "executor", 0, ctlSess, "c", "normal", "m", tsOffset(-5))
	fixtureMessage(t, s, projA, col05, "bus", "", 0, ctlSess, "c", "normal", "b", tsOffset(-4))
	fixtureSentinel(t, s, execSess, col05, "executor", tsOffset(-5))

	// 第一轮计数（窗口内只跑聚合；WindowAsOf 显式传入=handler 实跑形态，窗聚合
	// 两查计入基线）。
	*counter = 0
	if _, err := s.BuildOverview(now, OverviewOpts{WindowAsOf: "12:00"}); err != nil {
		t.Fatalf("第一轮聚合失败: %v", err)
	}
	n1 := *counter
	t.Logf("BuildOverview 往返实跑 = %d 条（基线上限 13：§3.7 基线 9+窗接线 2+progress 1+Q1 聚合 1）", n1)
	if n1 > 13 {
		t.Fatalf("聚合查询数 = %d，超过基线上限 13（§3.7 基线 9+窗接线 2+progress 1+Q1 聚合 1）", n1)
	}

	// 加栏目+位点+消息后重跑：计数不得增长（禁按栏目循环查——§3.7 尾注）。
	col07 := fixtureColumn(t, s, projA, "07", "栏目07", "active")
	fixturePosition(t, s, projA, col07, "controller", 1)
	fixturePosition(t, s, projA, col07, "executor", 1)
	fixtureMessage(t, s, projA, col07, "direct", "executor", 0, ctlSess, "c", "normal", "m2", tsOffset(-3))
	fixtureMessage(t, s, projA, col07, "direct", "controller", 0, ctlSess, "c", "normal", "m3", tsOffset(-2))

	*counter = 0
	if _, err := s.BuildOverview(now, OverviewOpts{WindowAsOf: "12:00"}); err != nil {
		t.Fatalf("第二轮聚合失败: %v", err)
	}
	n2 := *counter
	if n2 != n1 {
		t.Errorf("加栏目后查询数 = %d，第一轮 = %d——聚合不随栏目数增长（§3.7）", n2, n1)
	}
	if n2 > 13 {
		t.Errorf("聚合查询数 = %d，超过上限 13", n2)
	}
	t.Logf("往返账实测：两栏目 %d 条 / 三栏目 %d 条（基线 13：§3.7 基线 9+窗接线 2+progress 1+Q1 聚合 1）", n1, n2)
}

// ===== TestUnreadDialogMatchesPollPredicate / TestSessionEntryUnreadKeys：b3-W1 会话未读两分项 =====

// dialogVisibleCount poll 谓词第三分支（§3.6 逐字）按会话的可见计数——Q1 对拍的
// 期望值源：PollVisible 传该会话真实对话位点（ack_positions 行值，缺行按 0=全量
// 可见），返回集里 kind∈(chat,receipt) 的条数即 poll 口径的未读对话数。信箱位点
// 传 0 全开（direct/bus 分支命中也被 kind 过滤丢弃，不污染对拍面）。
func dialogVisibleCount(t *testing.T, s *Store, projectID, columnID int64, role string, sessionID int64) int64 {
	t.Helper()
	pos, _, found := positionRow(t, s, columnID, DialogConsumer(sessionID))
	if !found {
		pos = 0 // 缺行=COALESCE 0（Q1 LEFT JOIN 同口径）
	}
	msgs, err := s.PollVisible(projectID, columnID, role, sessionID, 0, pos, 0)
	if err != nil {
		t.Fatalf("PollVisible(session=%d) 失败: %v", sessionID, err)
	}
	var n int64
	for _, m := range msgs {
		if m.Kind == MessageKindChat || m.Kind == MessageKindReceipt {
			n++
		}
	}
	return n
}

// TestUnreadDialogMatchesPollPredicate b3-W1 T1：BuildOverview 的 unread_dialog
// （Q1 冻结 SQL 聚合）与 poll 谓词第三分支逐会话机械对拍——把「未读角标与 poll
// 拉取同口径」从注释约定升级为机械守门：任一侧口径漂移（聚合漏排自发、位点比较
// 误用 >=、receipt 漏计）即红。三态造数：未 ack（位点行缺失=全量可见）/已 ack
// （位点行存在，seq>position 才计）/自发（sender==target 不计）。先例体例=
// TestAdvanceOmittedMatchesPollPredicate（positions_test.go，Important 1）。
func TestUnreadDialogMatchesPollPredicate(t *testing.T) {
	s := openTemp(t)
	now := tsOffset(0)

	projA := fixtureProject(t, s, "proj-a", "项目A", "active", 900)
	col05 := fixtureColumn(t, s, projA, "05", "栏目05", "active")
	sNoAck := fixtureSession(t, s, projA, col05, "exe-noack", "executor", tsOffset(-10)) // 无对话位点行
	sAcked := fixtureSession(t, s, projA, col05, "exe-acked", "executor", tsOffset(-10)) // 有对话位点行
	ctl := fixtureSession(t, s, projA, col05, "controller-A", "controller", tsOffset(-10))

	// 消息编排（seq 插入序自增）：
	//  c1: chat→sNoAck（board 发——未 ack 全量可见）
	//  c2: chat→sAcked（board 发——位点推到 c2，已消费不计）
	//  c3: chat→sAcked（board 发——seq>位点，计入）
	//  c4: chat→sAcked 自发（sender==target，不计）
	//  r1: receipt→sNoAck（receipt kind 同计）
	//  d1: direct→executor（信箱口径，不进 unread_dialog——kind 过滤天然排除）
	//  x1: chat→ctl（他会话的对话流，不串入 executor 两会话）
	fixtureMessage(t, s, projA, col05, "chat", "", sNoAck, 0, "board-user:张三", "normal", "c1", tsOffset(-60))
	c2 := fixtureMessage(t, s, projA, col05, "chat", "", sAcked, 0, "board-user:张三", "normal", "c2", tsOffset(-50))
	fixtureMessage(t, s, projA, col05, "chat", "", sAcked, 0, "board-user:张三", "normal", "c3", tsOffset(-40))
	fixtureMessage(t, s, projA, col05, "chat", "", sAcked, sAcked, "exe-acked@05", "normal", "c4-self", tsOffset(-35))
	fixtureMessage(t, s, projA, col05, "receipt", "", sNoAck, 0, "system", "normal", "r1", tsOffset(-30))
	fixtureMessage(t, s, projA, col05, "direct", "executor", 0, ctl, "controller-A@05", "normal", "d1", tsOffset(-25))
	fixtureMessage(t, s, projA, col05, "chat", "", ctl, 0, "board-user:张三", "normal", "x1", tsOffset(-20))

	// sAcked 对话位点推到 c2：c2 已消费、c3 未读。
	fixturePosition(t, s, projA, col05, DialogConsumer(sAcked), c2)

	ov, err := s.BuildOverview(now, OverviewOpts{})
	if err != nil {
		t.Fatalf("BuildOverview 失败: %v", err)
	}

	// 逐会话机械对拍：overview 每个会话 unread_dialog == poll 第三分支计数。
	checked := 0
	for _, p := range ov.Projects {
		for _, se := range p.Sessions {
			want := dialogVisibleCount(t, s, projA, col05, se.Role, se.ID)
			if se.UnreadDialog != want {
				t.Errorf("会话 %s unread_dialog = %d，poll 谓词对拍期望 %d", se.Name, se.UnreadDialog, want)
			}
			checked++
		}
	}
	if checked != 3 {
		t.Fatalf("对拍会话数 = %d，期望 3（exe-noack/exe-acked/controller-A）", checked)
	}

	// 硬值锚定（对拍之外锁语义，防两侧同漂）：sNoAck=2（c1+r1）/ sAcked=1（仅 c3）/
	// ctl=1（x1——board 发给 ctl 的 chat 对 ctl 非自发）。
	byName := map[string]int64{}
	for _, p := range ov.Projects {
		for _, se := range p.Sessions {
			byName[se.Name] = se.UnreadDialog
		}
	}
	for name, want := range map[string]int64{"exe-noack": 2, "exe-acked": 1, "controller-A": 1} {
		if byName[name] != want {
			t.Errorf("会话 %s unread_dialog = %d，期望 %d", name, byName[name], want)
		}
	}
}

// TestSessionEntryUnreadKeys b3-W1 T2：unread_mailbox 与 MailboxOverview.pending
// 同（栏目,角色）格同值（一格两人取同值）；零消息会话两键=0 且 JSON 键恒在
// （无 omitempty——序列化断言锁形态，零值也透出）。
func TestSessionEntryUnreadKeys(t *testing.T) {
	s := openTemp(t)
	now := tsOffset(0)

	projA := fixtureProject(t, s, "proj-a", "项目A", "active", 900)
	col05 := fixtureColumn(t, s, projA, "05", "栏目05", "active")
	col06 := fixtureColumn(t, s, projA, "06", "栏目06", "active")
	ctl := fixtureSession(t, s, projA, col05, "controller-A", "controller", tsOffset(-10))
	exe1 := fixtureSession(t, s, projA, col05, "executor-B", "executor", tsOffset(-10)) // 一格两人之乙
	fixtureSession(t, s, projA, col05, "executor-C", "executor", tsOffset(-10))         // 一格两人之丙
	fixtureSession(t, s, projA, col06, "observer-Q", "observer", tsOffset(-10))         // 零消息会话

	// 05/executor 格：direct 两条+位点推到第一条 → pending=1；chat→exe1 一条 →
	// exe1 unread_dialog=1（对话位点 per-session，exe2 不计）。
	d1 := fixtureMessage(t, s, projA, col05, "direct", "executor", 0, ctl, "controller-A@05", "normal", "d1", tsOffset(-60))
	fixtureMessage(t, s, projA, col05, "direct", "executor", 0, ctl, "controller-A@05", "normal", "d2", tsOffset(-50))
	fixtureMessage(t, s, projA, col05, "chat", "", exe1, 0, "board-user:张三", "normal", "c1", tsOffset(-40))
	fixturePosition(t, s, projA, col05, "executor", d1)

	ov, err := s.BuildOverview(now, OverviewOpts{})
	if err != nil {
		t.Fatalf("BuildOverview 失败: %v", err)
	}
	sessionByName := func(name string) SessionEntry {
		t.Helper()
		for _, p := range ov.Projects {
			for _, se := range p.Sessions {
				if se.Name == name {
					return se
				}
			}
		}
		t.Fatalf("缺会话 %s", name)
		return SessionEntry{}
	}

	// 信箱侧锚定：05/executor pending=1（unread_mailbox 的映射源值）。
	mb := mailboxAt(t, ov, "proj-a", "05", "executor")
	if mb.Pending != 1 {
		t.Fatalf("05/executor pending = %d，期望 1（位点 %d 之后一条 direct）", mb.Pending, d1)
	}

	// 一格两人取同值：exe1/exe2 的 unread_mailbox 都=mb.Pending；unread_dialog 按
	// 会话独立（chat 只发给了 exe1）。
	e1, e2 := sessionByName("executor-B"), sessionByName("executor-C")
	if e1.UnreadMailbox != mb.Pending || e2.UnreadMailbox != mb.Pending {
		t.Errorf("一格两人 unread_mailbox = (%d, %d)，期望同值=%d（pending 按（栏目,角色）映射）",
			e1.UnreadMailbox, e2.UnreadMailbox, mb.Pending)
	}
	if e1.UnreadDialog != 1 || e2.UnreadDialog != 0 {
		t.Errorf("unread_dialog = (exe1 %d, exe2 %d)，期望 (1, 0)（对话位点 per-session）",
			e1.UnreadDialog, e2.UnreadDialog)
	}

	// 零消息会话（06/observer：无位点行无消息，信箱不出现）：两键=0 且 JSON 键
	// 恒在（无 omitempty——零值也序列化）。
	q := sessionByName("observer-Q")
	if q.UnreadMailbox != 0 || q.UnreadDialog != 0 {
		t.Errorf("零消息会话两键 = (%d, %d)，期望 (0, 0)", q.UnreadMailbox, q.UnreadDialog)
	}
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal SessionEntry 失败: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"unread_mailbox":0`, `"unread_dialog":0`} {
		if !strings.Contains(got, want) {
			t.Errorf("SessionEntry json 缺 %s 形态（两键恒在非 omit）:\n%s", want, got)
		}
	}

	// controller 对照：05/controller 无 direct/bus → unread_mailbox=0；无 chat → 0。
	if c := sessionByName("controller-A"); c.UnreadMailbox != 0 || c.UnreadDialog != 0 {
		t.Errorf("controller-A 两键 = (%d, %d)，期望 (0, 0)", c.UnreadMailbox, c.UnreadDialog)
	}
}

// TestOverviewSentinelLastHit b8-spec W3/AC7 store 装配面：querySessions 扩列
// （s.sentinel_last_hit_at 随往返 6 带出，零额外查询）——有留痕会话
// SessionEntry.SentinelLastHitAt=留痕值，未命中会话该键为空串（键恒在缺省空串）。
func TestOverviewSentinelLastHit(t *testing.T) {
	s := openTemp(t)
	projID := fixtureProject(t, s, "proj-a", "项目A", ProjectStatusActive, 900)
	col05 := fixtureColumn(t, s, projID, "05", "栏目05", ColumnStatusActive)
	sessHit := fixtureSession(t, s, projID, col05, "watch-a", "executor", tsOffset(-10))
	fixtureSession(t, s, projID, col05, "watch-b", "observer", tsOffset(-10)) // 未命中对照

	if _, err := s.DB.Exec(`UPDATE sessions SET sentinel_last_hit_at = ? WHERE id = ?`,
		tsOffset(12), sessHit); err != nil {
		t.Fatalf("预置留痕列失败: %v", err)
	}

	ov, err := s.BuildOverview(tsOffset(20), OverviewOpts{})
	if err != nil {
		t.Fatalf("BuildOverview 失败: %v", err)
	}
	got := map[string]string{}
	for _, p := range ov.Projects {
		for _, se := range p.Sessions {
			got[se.Name] = se.SentinelLastHitAt
		}
	}
	if got["watch-a"] != tsOffset(12) {
		t.Errorf("watch-a SentinelLastHitAt = %q，期望 %s", got["watch-a"], tsOffset(12))
	}
	if got["watch-b"] != "" {
		t.Errorf("watch-b SentinelLastHitAt = %q，期望空串（未命中会话）", got["watch-b"])
	}
}
