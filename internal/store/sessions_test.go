package store

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seedSession 直插一行 sessions 夹具，返回自增 id（不依赖 CRUD 方法）。
func seedSession(t *testing.T, s *Store, projectID, columnID int64, name, role, lastSeenAt, createdAt string) int64 {
	t.Helper()
	res, err := s.DB.Exec(
		`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, columnID, name, role, lastSeenAt, createdAt,
	)
	if err != nil {
		t.Fatalf("插入夹具会话 %q 失败: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取夹具会话 %q 自增 id 失败: %v", name, err)
	}
	return id
}

// isoTime 由基准时刻加偏移秒生成 ISO8601 UTC 文本（失联构造用，避免手算错秒）。
func isoTime(base string, offsetSec int) string {
	b, err := time.Parse(time.RFC3339, base)
	if err != nil {
		panic("isoTime 基准时刻非法: " + base)
	}
	return b.Add(time.Duration(offsetSec) * time.Second).UTC().Format(time.RFC3339)
}

// ---- UpsertSession ----

// TestUpsertInsert AC12.3 首调=隐式注册：同四元组首次 upsert 插入新行，
// 返回「新建」标记 Created=true（供 B1-4 中间件写 audit session.auto_register——T1），
// 返回行为库中真实行（§7.1：last_seen_at=created_at=now）。
func TestUpsertInsert(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	cid := seedColumn(t, s, pid, "05", "栏目05", ColumnStatusActive, "2026-01-01T00:00:00Z")

	got, err := s.UpsertSession(pid, cid, "executor-A", "executor")
	if err != nil {
		t.Fatalf("UpsertSession 首调失败: %v", err)
	}
	if !got.Created {
		t.Error("首调 Created = false，期望 true（新建标记，供中间件写 auto_register）")
	}
	if got.RoleChanged {
		t.Error("首调 RoleChanged = true，期望 false（新建不属 role 变更）")
	}
	if got.OldRole != "" {
		t.Errorf("首调 OldRole = %q，期望空（无旧值）", got.OldRole)
	}
	row := got.Session
	if row.ID <= 0 {
		t.Errorf("id = %d，期望 > 0", row.ID)
	}
	if row.ProjectID != pid || row.ColumnID != cid {
		t.Errorf("project_id/column_id = %d/%d，期望夹具 %d/%d", row.ProjectID, row.ColumnID, pid, cid)
	}
	if row.Name != "executor-A" || row.Role != "executor" {
		t.Errorf("name/role = %q/%q，期望 executor-A/executor", row.Name, row.Role)
	}
	if row.LastSeenAt != t0 || row.CreatedAt != t0 {
		t.Errorf("last_seen_at/created_at = %q/%q，期望注入时钟 %q（§7.1 首插两值同 now）", row.LastSeenAt, row.CreatedAt, t0)
	}

	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("统计 sessions 失败: %v", err)
	}
	if n != 1 {
		t.Errorf("sessions 行数 = %d，期望 1", n)
	}
}

// TestUpsertInsertRejectsEmptyFields 数据面兜底：name/role 空串或纯空白拒绝
// （哨兵 ErrSessionInvalid；role 非空格式校验归中间件，store 层同构兜底——
// projects.go 空 code 防御惯例），且不落数据面。
func TestUpsertInsertRejectsEmptyFields(t *testing.T) {
	s := openTemp(t)
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	cid := seedColumn(t, s, pid, "05", "栏目05", ColumnStatusActive, "2026-01-01T00:00:00Z")

	for _, tc := range []struct{ name, role string }{
		{"", "executor"},
		{"   ", "executor"},
		{"executor-A", ""},
		{"executor-A", " \t"},
	} {
		if _, err := s.UpsertSession(pid, cid, tc.name, tc.role); !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("name=%q role=%q 错误 = %v，期望哨兵 ErrSessionInvalid", tc.name, tc.role, err)
		}
	}

	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("统计 sessions 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("空字段拒绝后 sessions 行数 = %d，期望 0（不应落库）", n)
	}
}

// TestUpsertRefresh AC12.3 幂等同实体：同四元组重调仅刷 last_seen_at（§7.1
// DO UPDATE），不新增行；created_at/id 保持首插值。
func TestUpsertRefresh(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	cid := seedColumn(t, s, pid, "05", "栏目05", ColumnStatusActive, "2026-01-01T00:00:00Z")

	first, err := s.UpsertSession(pid, cid, "executor-A", "executor")
	if err != nil {
		t.Fatalf("首调失败: %v", err)
	}

	// 注入时钟前进（AC12.1：last_seen_at 随服务端时钟刷新）。
	const t1 = "2026-01-01T08:05:00Z"
	injectFixedClock(t, t1)
	second, err := s.UpsertSession(pid, cid, "executor-A", "executor")
	if err != nil {
		t.Fatalf("重调失败: %v", err)
	}
	if second.Created {
		t.Error("重调 Created = true，期望 false（同实体幂等，不重复隐式注册）")
	}
	if second.RoleChanged {
		t.Error("重调 RoleChanged = true，期望 false（role 未变不算变更）")
	}
	row := second.Session
	if row.ID != first.Session.ID {
		t.Errorf("重调 id = %d，期望保持首插 %d", row.ID, first.Session.ID)
	}
	if row.LastSeenAt != t1 {
		t.Errorf("last_seen_at = %q，期望刷新为注入时钟 %q（AC12.1）", row.LastSeenAt, t1)
	}
	if row.CreatedAt != t0 {
		t.Errorf("created_at = %q，期望保持首插 %q", row.CreatedAt, t0)
	}

	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("统计 sessions 失败: %v", err)
	}
	if n != 1 {
		t.Errorf("重调后 sessions 行数 = %d，期望 1（不新增行）", n)
	}
}

// TestUpsertRoleChange D5：同名会话换 role 再调用——role 静默覆盖（§7.1
// DO UPDATE SET role=excluded.role）+ 返回「role 变更」标记与旧 role
// （供 B1-4 中间件写 role-change 审计，detail 记 before/after）。
func TestUpsertRoleChange(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	cid := seedColumn(t, s, pid, "05", "栏目05", ColumnStatusActive, "2026-01-01T00:00:00Z")

	if _, err := s.UpsertSession(pid, cid, "executor-A", "executor"); err != nil {
		t.Fatalf("首调失败: %v", err)
	}

	// 同名换 role。
	const t1 = "2026-01-01T09:00:00Z"
	injectFixedClock(t, t1)
	got, err := s.UpsertSession(pid, cid, "executor-A", "foreman")
	if err != nil {
		t.Fatalf("换 role 重调失败: %v", err)
	}
	if got.Created {
		t.Error("换 role Created = true，期望 false（同实体，非新建）")
	}
	if !got.RoleChanged {
		t.Error("换 role RoleChanged = false，期望 true（D5 变更标记）")
	}
	if got.OldRole != "executor" {
		t.Errorf("换 role OldRole = %q，期望 executor（D5 detail before 值）", got.OldRole)
	}
	if got.Session.Role != "foreman" {
		t.Errorf("换 role 后行内 role = %q，期望静默覆盖为 foreman", got.Session.Role)
	}
	if got.Session.LastSeenAt != t1 {
		t.Errorf("换 role last_seen_at = %q，期望刷新为 %q", got.Session.LastSeenAt, t1)
	}

	// 同 role 稳态重调：不再报变更（避免中间件重复审计）。
	const t2 = "2026-01-01T09:05:00Z"
	injectFixedClock(t, t2)
	steady, err := s.UpsertSession(pid, cid, "executor-A", "foreman")
	if err != nil {
		t.Fatalf("稳态重调失败: %v", err)
	}
	if steady.RoleChanged {
		t.Error("同 role 重调 RoleChanged = true，期望 false（不重复报变更）")
	}
	if steady.OldRole != "" {
		t.Errorf("同 role 重调 OldRole = %q，期望空", steady.OldRole)
	}

	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("统计 sessions 失败: %v", err)
	}
	if n != 1 {
		t.Errorf("sessions 行数 = %d，期望 1（换 role 不新增行）", n)
	}
}

// TestGetProjectColumnForSession 会话中间件存在性前置查询（§7.1 步骤 1 数据面）：
// 一条 LEFT JOIN 同时定位项目与栏目（spec §七「1 查 project/column 可一条 JOIN
// 取两行」往返账）；不筛 status（archived 行原样返回供调用方裁量——b1-spec §二
// 中间件只挡不存在）；未命中哨兵两形态（项目级/栏目级）+ 跨项目同 code 栏目不串。
func TestGetProjectColumnForSession(t *testing.T) {
	s := openTemp(t)
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	cid := seedColumn(t, s, pid, "05", "栏目05", ColumnStatusActive, "2026-01-01T00:00:00Z")
	seedColumn(t, s, pid, "99", "栏目99", ColumnStatusArchived, "2026-01-01T00:00:01Z")
	pidArch := seedProject(t, s, "p-arch", "归档项目", ProjectStatusArchived, 900, "2026-01-01T00:00:02Z")
	cidArch := seedColumn(t, s, pidArch, "05", "归档项目栏目05", ColumnStatusActive, "2026-01-01T00:00:03Z")
	pidB := seedProject(t, s, "p-b", "项目B", ProjectStatusActive, 900, "2026-01-01T00:00:04Z")
	seedColumn(t, s, pidB, "05", "项目B栏目05", ColumnStatusActive, "2026-01-01T00:00:05Z")

	// 全命中：id 与 status 原样装配。
	ref, err := s.GetProjectColumnForSession("p-a", "05")
	if err != nil {
		t.Fatalf("全命中查询失败: %v", err)
	}
	if ref.ProjectID != pid || ref.ColumnID != cid {
		t.Errorf("全命中 id = (%d,%d)，期望 (%d,%d)", ref.ProjectID, ref.ColumnID, pid, cid)
	}
	if ref.ProjectStatus != ProjectStatusActive || ref.ColumnStatus != ColumnStatusActive {
		t.Errorf("全命中 status = (%q,%q)，期望 (active,active)", ref.ProjectStatus, ref.ColumnStatus)
	}

	// archived 项目：原样返回不挡（b1-spec §二 archived 放行口径的数据面前提）。
	ref, err = s.GetProjectColumnForSession("p-arch", "05")
	if err != nil {
		t.Fatalf("archived 项目查询失败: %v", err)
	}
	if ref.ProjectID != pidArch || ref.ColumnID != cidArch {
		t.Errorf("archived 项目 id = (%d,%d)，期望 (%d,%d)", ref.ProjectID, ref.ColumnID, pidArch, cidArch)
	}
	if ref.ProjectStatus != ProjectStatusArchived {
		t.Errorf("archived 项目 status = %q，期望原样返回 archived", ref.ProjectStatus)
	}

	// archived 栏目（active 项目下）：同上原样返回。
	ref, err = s.GetProjectColumnForSession("p-a", "99")
	if err != nil {
		t.Fatalf("archived 栏目查询失败: %v", err)
	}
	if ref.ColumnStatus != ColumnStatusArchived {
		t.Errorf("archived 栏目 status = %q，期望原样返回 archived", ref.ColumnStatus)
	}

	// 项目不存在 → ErrProjectNotFound。
	if _, err := s.GetProjectColumnForSession("nope", "05"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("项目不存在错误 = %v，期望哨兵 ErrProjectNotFound", err)
	}

	// 项目存在但栏目不存在 → ErrColumnNotFound。
	if _, err := s.GetProjectColumnForSession("p-a", "77"); !errors.Is(err, ErrColumnNotFound) {
		t.Errorf("栏目不存在错误 = %v，期望哨兵 ErrColumnNotFound", err)
	}

	// 跨项目同 code 栏目（05 属 p-a/p-arch/p-b 各一）：JOIN 限定 project_id，
	// p-a 配 p-b 的同号栏目不存在歧义命中——但 p-a 自有 05，须命中 p-a 自己的行。
	ref, err = s.GetProjectColumnForSession("p-a", "05")
	if err != nil {
		t.Fatalf("跨项目同 code 查询失败: %v", err)
	}
	if ref.ColumnID != cid {
		t.Errorf("跨项目同 code 命中 column_id = %d，期望项目内自有行 %d（UNIQUE(project_id,code) 隔离）", ref.ColumnID, cid)
	}
}

// TestUpsertSessionConcurrentSmoke 并发 upsert smoke（B1-3 质量审查建议②）：
// N goroutine 并发同四元组 → 全部无错、sessions 恰单行、Created 恰一次
// （隐式注册审计不重复）、RoleChanged 恒零（同 role）。机制归因同 TestConcurrentWrites：
// database/sql 单连接池排队串行化，事务内「探测+upsert」两语句原子。
func TestUpsertSessionConcurrentSmoke(t *testing.T) {
	s := openTemp(t)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	cid := seedColumn(t, s, pid, "05", "栏目05", ColumnStatusActive, "2026-01-01T00:00:00Z")

	const n = 16
	var (
		wg      sync.WaitGroup
		errs    sync.Map
		created atomic.Int64
		changed atomic.Int64
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			up, err := s.UpsertSession(pid, cid, "executor-A", "executor")
			if err != nil {
				errs.Store(err.Error(), true)
				return
			}
			if up.Created {
				created.Add(1)
			}
			if up.RoleChanged {
				changed.Add(1)
			}
		}()
	}
	wg.Wait()

	var errCount int
	errs.Range(func(_, _ any) bool { errCount++; return true })
	if errCount != 0 {
		t.Fatalf("并发 upsert 存在失败（%d/%d）", errCount, n)
	}
	if got := created.Load(); got != 1 {
		t.Errorf("Created 计数 = %d，期望恰 1（隐式注册恰一次，审计不重复）", got)
	}
	if got := changed.Load(); got != 0 {
		t.Errorf("RoleChanged 计数 = %d，期望 0（同 role 恒无变更）", got)
	}
	var rowCount int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&rowCount); err != nil {
		t.Fatalf("统计 sessions 失败: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("sessions 行数 = %d，期望 1（并发同四元组单行）", rowCount)
	}
}

// ---- GetSession / ListSessions（失联计算列，§7.2 读时计算不落库） ----

// TestGetSession 按 id 单查：JOIN 装配项目/栏目 code（#27 响应字段面）；
// Alive 失联计算列（now-last_seen_at > 阈值=失联，严格大于——边界 900s 整不判失联）；
// 未命中返回 ErrSessionNotFound。
func TestGetSession(t *testing.T) {
	s := openTemp(t)
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	cid := seedColumn(t, s, pid, "05", "栏目05", ColumnStatusActive, "2026-01-01T00:00:00Z")
	const seen = "2026-01-01T08:00:00Z"
	sid := seedSession(t, s, pid, cid, "executor-A", "executor", seen, seen)

	const now = "2026-01-01T08:15:01Z" // seen+901s

	// 未超阈值（+30s）→ Alive=true。
	got, err := s.GetSession(sid, isoTime(seen, 30), 900)
	if err != nil {
		t.Fatalf("GetSession 失败: %v", err)
	}
	if !got.Alive {
		t.Errorf("+30s Alive = false，期望 true（30 <= 900 未失联）")
	}
	if got.ID != sid || got.ProjectID != pid || got.ColumnID != cid {
		t.Errorf("id/project_id/column_id = %d/%d/%d，期望 %d/%d/%d", got.ID, got.ProjectID, got.ColumnID, sid, pid, cid)
	}
	if got.ProjectCode != "p-a" || got.ColumnCode != "05" {
		t.Errorf("project/column = %q/%q，期望 JOIN 装配 p-a/05（#27 字段面）", got.ProjectCode, got.ColumnCode)
	}
	if got.LastSeenAt != seen || got.Role != "executor" {
		t.Errorf("last_seen_at/role = %q/%q，期望 %q/executor", got.LastSeenAt, got.Role, seen)
	}

	// 阈值边界：恰 +900s 不判失联（§7.2 判定=严格大于）。
	got, err = s.GetSession(sid, isoTime(seen, 900), 900)
	if err != nil {
		t.Fatalf("GetSession(边界 900s) 失败: %v", err)
	}
	if !got.Alive {
		t.Error("+900s 整 Alive = false，期望 true（> 阈值才失联，§7.2 严格大于）")
	}

	// 超阈值（+901s）→ Alive=false（读时计算，行未改动）。
	got, err = s.GetSession(sid, isoTime(seen, 901), 900)
	if err != nil {
		t.Fatalf("GetSession(901s) 失败: %v", err)
	}
	if got.Alive {
		t.Error("+901s Alive = true，期望 false（901 > 900 失联）")
	}

	// 时钟回拨（now < last_seen，负差值）：按 §7.2 判定式差值恒不超阈值
	// → Alive=true，不误标失联（乱序/回拨容忍语义钉住）。
	got, err = s.GetSession(sid, isoTime(seen, -30), 900)
	if err != nil {
		t.Fatalf("GetSession(回拨 -30s) 失败: %v", err)
	}
	if !got.Alive {
		t.Error("回拨 -30s Alive = false，期望 true（负差值恒不超阈值，不误判失联）")
	}

	// 未命中 → ErrSessionNotFound。
	if _, err := s.GetSession(sid+999, now, 900); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("未命中错误 = %v，期望哨兵 ErrSessionNotFound", err)
	}
}

// TestUpsertSessionForeignKey FK 兜底路径：project_id/column_id 不存在时
// foreign_keys=ON 拒绝插入，错误经「store: upsert 会话 …」上下文包装返回
// （非 ErrSessionInvalid 参数校验拦截——两类错误 HTTP 语义不同：400 vs 数据面故障）；
// 事务回滚不落数据面。§7.1 流程中存在性校验主责在 B1-4 中间件（步骤 1），
// 本测试钉住 store 层兜底行为与错误可辨识性。
func TestUpsertSessionForeignKey(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	// projectID 不存在。
	_, err := s.UpsertSession(9999, 1, "executor-A", "executor")
	if err == nil {
		t.Fatal("projectID 不存在应 FK 拒绝，实际成功")
	}
	if errors.Is(err, ErrSessionInvalid) {
		t.Errorf("FK 错误误判为参数校验哨兵 ErrSessionInvalid: %v", err)
	}
	if !strings.Contains(err.Error(), "upsert 会话") {
		t.Errorf("FK 错误缺「upsert 会话」上下文包装: %v", err)
	}

	// projectID 存在但 columnID 不存在（跨项目 id 亦拒）。
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	if _, err := s.UpsertSession(pid, 9999, "executor-A", "executor"); err == nil {
		t.Fatal("columnID 不存在应 FK 拒绝，实际成功")
	}

	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("统计 sessions 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("FK 拒绝后 sessions 行数 = %d，期望 0（事务回滚不落数据面）", n)
	}
}

// TestListSessions 列表：按项目过滤（projectCode 空=全部）；失联计算列含
// 项目级阈值覆盖（COALESCE(项目级, 服务默认)——§3.6/§7.2）；archived 项目不列
// （§3.6 WHERE p.status='active' 原样）；空结果非 nil 空切片（JSON null 防线）。
func TestListSessions(t *testing.T) {
	s := openTemp(t)
	// 全库无会话：非 nil 空切片。
	empty, err := s.ListSessions("", "2026-01-01T08:00:00Z", 900)
	if err != nil {
		t.Fatalf("空库 ListSessions 失败: %v", err)
	}
	if empty == nil {
		t.Error("空库 ListSessions 返回 nil，期望非 nil 空切片（JSON null 防线）")
	}

	const seen = "2026-01-01T08:00:00Z"
	const now = "2026-01-01T08:15:01Z" // seen+901s

	// 项目 A：默认阈值 900——A1 失联（901s）、A2 在线（101s）、A3 边界在线
	// （now-last_seen 恰 900s 整：last_seen=seen+1s，now=seen+901s）。
	pidA := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	cidA := seedColumn(t, s, pidA, "05", "栏目05", ColumnStatusActive, "2026-01-01T00:00:00Z")
	lostA := seedSession(t, s, pidA, cidA, "executor-A", "executor", seen, seen)
	seedSession(t, s, pidA, cidA, "controller-A", "controller", isoTime(seen, 800), seen)
	seedSession(t, s, pidA, cidA, "watcher-A", "executor", isoTime(seen, 1), seen)

	// 项目 B：项目级覆盖 60s——B1 心跳 901s 前失联（若误用默认 900 会误判在线，
	// 本断言即 COALESCE 项目级覆盖生效的证据）。
	pidB := seedProject(t, s, "p-b", "项目B", ProjectStatusActive, 60, "2026-01-01T00:01:00Z")
	cidB := seedColumn(t, s, pidB, "06", "栏目06", ColumnStatusActive, "2026-01-01T00:01:00Z")
	seedSession(t, s, pidB, cidB, "executor-B", "executor", seen, seen)

	// 项目 C：archived——不列（§3.6 WHERE p.status='active'）。
	pidC := seedProject(t, s, "p-c", "项目C", ProjectStatusArchived, 900, "2026-01-01T00:02:00Z")
	cidC := seedColumn(t, s, pidC, "07", "栏目07", ColumnStatusActive, "2026-01-01T00:02:00Z")
	seedSession(t, s, pidC, cidC, "executor-C", "executor", seen, seen)

	// 全部：恰 4 行（A 3 行 + B 1 行；C 的归档项目会话不列）、按 created_at,id 稳定升序。
	all, err := s.ListSessions("", now, 900)
	if err != nil {
		t.Fatalf("ListSessions(全部) 失败: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("全部列表长度 = %d，期望 4（archived 项目不列）", len(all))
	}
	wantOrder := []string{"executor-A", "controller-A", "watcher-A", "executor-B"}
	for i, want := range wantOrder {
		if all[i].Name != want {
			t.Errorf("全部列表[%d].name = %q，期望 %q（created_at,id 稳定升序）", i, all[i].Name, want)
		}
	}
	if all[0].ID != lostA {
		t.Errorf("全部列表[0].id = %d，期望夹具会话 %d", all[0].ID, lostA)
	}
	if all[0].ProjectCode != "p-a" || all[0].ColumnCode != "05" {
		t.Errorf("列表[0] project/column = %q/%q，期望 JOIN 装配 p-a/05", all[0].ProjectCode, all[0].ColumnCode)
	}

	// 失联计算列：A1 失联、A2 在线、A3 边界在线、B1 项目级覆盖（60s）失联。
	wantAlive := map[string]bool{"executor-A": false, "controller-A": true, "watcher-A": true, "executor-B": false}
	for _, row := range all {
		if row.Alive != wantAlive[row.Name] {
			t.Errorf("会话 %s（project=%s）Alive = %v，期望 %v", row.Name, row.ProjectCode, row.Alive, wantAlive[row.Name])
		}
	}

	// 按项目过滤。
	gotA, err := s.ListSessions("p-a", now, 900)
	if err != nil {
		t.Fatalf("ListSessions(p-a) 失败: %v", err)
	}
	if len(gotA) != 3 {
		t.Fatalf("p-a 列表长度 = %d，期望 3", len(gotA))
	}
	gotB, err := s.ListSessions("p-b", now, 900)
	if err != nil {
		t.Fatalf("ListSessions(p-b) 失败: %v", err)
	}
	if len(gotB) != 1 || gotB[0].Name != "executor-B" {
		t.Fatalf("p-b 列表 = %+v，期望仅 executor-B", gotB)
	}

	// 项目不存在 → ErrProjectNotFound（对齐 ListColumns 惯例）。
	if _, err := s.ListSessions("nope", now, 900); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("项目不存在列表错误 = %v，期望哨兵 ErrProjectNotFound", err)
	}
}
