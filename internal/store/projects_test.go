package store

import (
	"errors"
	"path/filepath"
	"testing"
)

// ---- 测试夹具 ----

// strPtr 取字符串指针（UpdateProject 的 name 参数构造用）。
func strPtr(s string) *string { return &s }

// injectFixedClock/seedProject 共享夹具见 store_test.go（跨文件 helper 进共享文件惯例）。

// ---- CreateProject ----

// TestCreateProject AC1.2：成功经 RETURNING 返回库中真实行（注入时钟时间戳、
// 未提供覆盖时为 DDL DEFAULT 900 落库后的真值）；code 唯一冲突返回哨兵
// ErrProjectExists（handler 映射 project_exists 409）。
func TestCreateProject(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	// 成功：hb 未提供（0）→ 省略列走 DDL DEFAULT，返回值是落库后 RETURNING 的真实行。
	got, err := s.CreateProject(Project{Code: "proj-a", Name: "项目A"})
	if err != nil {
		t.Fatalf("CreateProject(proj-a) 失败: %v", err)
	}
	if got.ID <= 0 {
		t.Errorf("id = %d，期望 > 0", got.ID)
	}
	if got.Code != "proj-a" || got.Name != "项目A" {
		t.Errorf("code/name = %q/%q，期望 proj-a/项目A", got.Code, got.Name)
	}
	if got.Status != ProjectStatusActive {
		t.Errorf("status = %q，期望默认 %q", got.Status, ProjectStatusActive)
	}
	if got.HeartbeatTimeoutSec != 900 {
		t.Errorf("heartbeat_timeout_sec = %d，期望库中真实行值（DDL DEFAULT）900", got.HeartbeatTimeoutSec)
	}
	if got.CreatedAt != t0 || got.UpdatedAt != t0 {
		t.Errorf("created_at/updated_at = %q/%q，期望注入时钟 %q", got.CreatedAt, got.UpdatedAt, t0)
	}

	// 成功：提供项目级覆盖 1800。
	got2, err := s.CreateProject(Project{Code: "proj-b", Name: "项目B", HeartbeatTimeoutSec: 1800})
	if err != nil {
		t.Fatalf("CreateProject(proj-b) 失败: %v", err)
	}
	if got2.HeartbeatTimeoutSec != 1800 {
		t.Errorf("heartbeat_timeout_sec = %d，期望覆盖值 1800", got2.HeartbeatTimeoutSec)
	}
	if got2.ID <= got.ID {
		t.Errorf("自增 id 未递增：got2.ID=%d，got.ID=%d", got2.ID, got.ID)
	}

	// 唯一冲突（AC1.2）：同 code 再建 → ErrProjectExists。
	got3, err := s.CreateProject(Project{Code: "proj-a", Name: "重复"})
	if !errors.Is(err, ErrProjectExists) {
		t.Errorf("重复 code 错误 = %v，期望哨兵 ErrProjectExists", err)
	}
	if got3.ID != 0 {
		t.Errorf("冲突时返回 id = %d，期望零值", got3.ID)
	}

	// 落库行数 = 2（冲突插入未落数据面）。
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n); err != nil {
		t.Fatalf("统计 projects 失败: %v", err)
	}
	if n != 2 {
		t.Errorf("projects 行数 = %d，期望 2", n)
	}
}

// TestCreateProjectRejectsEmptyCode M-4 空 code 防御：空串与纯空白都拒
// （哨兵 ErrProjectInvalid），且不落数据面。
func TestCreateProjectRejectsEmptyCode(t *testing.T) {
	s := openTemp(t)

	for _, code := range []string{"", "   ", "\t\n"} {
		if _, err := s.CreateProject(Project{Code: code}); !errors.Is(err, ErrProjectInvalid) {
			t.Errorf("空 code %q 错误 = %v，期望哨兵 ErrProjectInvalid", code, err)
		}
	}

	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n); err != nil {
		t.Fatalf("统计 projects 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("空 code 拒绝后 projects 行数 = %d，期望 0（不应落库）", n)
	}
}

// ---- GetProjectByCode ----

// TestGetProjectByCode 命中返回全字段；archived 行照常返回（不筛状态，
// PATCH/DELETE 以 code 定位时需要）；未命中返回哨兵 ErrProjectNotFound。
func TestGetProjectByCode(t *testing.T) {
	s := openTemp(t)
	seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	seedProject(t, s, "p-b", "项目B", ProjectStatusActive, 1800, "2026-01-01T00:01:00Z")
	seedProject(t, s, "p-arc", "已归档", ProjectStatusArchived, 900, "2026-01-01T00:02:00Z")

	got, err := s.GetProjectByCode("p-a")
	if err != nil {
		t.Fatalf("GetProjectByCode(p-a) 失败: %v", err)
	}
	want := Project{
		ID: got.ID, Code: "p-a", Name: "项目A", Status: ProjectStatusActive,
		HeartbeatTimeoutSec: 900,
		CreatedAt:           "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	if got != want {
		t.Errorf("GetProjectByCode(p-a) = %+v，期望 %+v", got, want)
	}

	got2, err := s.GetProjectByCode("p-b")
	if err != nil {
		t.Fatalf("GetProjectByCode(p-b) 失败: %v", err)
	}
	if got2.HeartbeatTimeoutSec != 1800 {
		t.Errorf("heartbeat_timeout_sec = %d，期望 1800", got2.HeartbeatTimeoutSec)
	}

	// archived 行照常返回（含历史查询场景）。
	got3, err := s.GetProjectByCode("p-arc")
	if err != nil {
		t.Fatalf("GetProjectByCode(p-arc) 失败: %v", err)
	}
	if got3.Status != ProjectStatusArchived {
		t.Errorf("status = %q，期望 %q（archived 行不应被筛掉）", got3.Status, ProjectStatusArchived)
	}

	// 未命中。
	if _, err := s.GetProjectByCode("nope"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("未命中错误 = %v，期望哨兵 ErrProjectNotFound", err)
	}
}

// ---- UpdateProject ----

// TestUpdateProject name/heartbeat_timeout_sec 更新 + updated_at 刷新、created_at 不变；
// 两项均未提供返回 ErrNoFields（空更新拒绝）；hb 负数视为未提供静默跳过；
// 目标不存在返回 ErrProjectNotFound。
func TestUpdateProject(t *testing.T) {
	s := openTemp(t)
	const created = "2026-01-01T00:00:00Z"
	seedProject(t, s, "p-a", "旧名", ProjectStatusActive, 900, created)
	const t1 = "2026-01-02T08:00:00Z"
	injectFixedClock(t, t1)

	// 仅 name（空串合法：指针提供即更新）。
	got, err := s.UpdateProject("p-a", strPtr("新名"), 0)
	if err != nil {
		t.Fatalf("UpdateProject(name) 失败: %v", err)
	}
	if got.Name != "新名" || got.HeartbeatTimeoutSec != 900 {
		t.Errorf("name/hb = %q/%d，期望 新名/900（hb 不应被 0 触碰）", got.Name, got.HeartbeatTimeoutSec)
	}
	assertUpdateStamp(t, got, created, t1)

	// 仅 heartbeat_timeout_sec。
	got, err = s.UpdateProject("p-a", nil, 1800)
	if err != nil {
		t.Fatalf("UpdateProject(hb) 失败: %v", err)
	}
	if got.Name != "新名" || got.HeartbeatTimeoutSec != 1800 {
		t.Errorf("name/hb = %q/%d，期望 新名/1800", got.Name, got.HeartbeatTimeoutSec)
	}
	assertUpdateStamp(t, got, created, t1)

	// 两项同时。
	got, err = s.UpdateProject("p-a", strPtr(""), 3600)
	if err != nil {
		t.Fatalf("UpdateProject(双字段) 失败: %v", err)
	}
	if got.Name != "" || got.HeartbeatTimeoutSec != 3600 {
		t.Errorf("name/hb = %q/%d，期望 \"\"/3600（空串名合法）", got.Name, got.HeartbeatTimeoutSec)
	}

	// hb 负数静默跳过：不当作覆盖值，其余字段照常更新。
	got, err = s.UpdateProject("p-a", strPtr("负跳过"), -1)
	if err != nil {
		t.Fatalf("UpdateProject(hb=-1) 失败: %v", err)
	}
	if got.Name != "负跳过" || got.HeartbeatTimeoutSec != 3600 {
		t.Errorf("name/hb = %q/%d，期望 负跳过/3600（-1 不应触碰 hb）", got.Name, got.HeartbeatTimeoutSec)
	}

	// 空更新拒绝：两项均未提供 → ErrNoFields，updated_at 不应刷新。
	before, err := s.GetProjectByCode("p-a")
	if err != nil {
		t.Fatalf("更新前读取失败: %v", err)
	}
	if _, err := s.UpdateProject("p-a", nil, 0); !errors.Is(err, ErrNoFields) {
		t.Errorf("空更新错误 = %v，期望哨兵 ErrNoFields", err)
	}
	after, err := s.GetProjectByCode("p-a")
	if err != nil {
		t.Fatalf("空更新后读取失败: %v", err)
	}
	if after != before {
		t.Errorf("空更新不应改动行：before=%+v after=%+v", before, after)
	}

	// 目标不存在。
	if _, err := s.UpdateProject("nope", strPtr("x"), 0); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("更新不存在项目错误 = %v，期望哨兵 ErrProjectNotFound", err)
	}
}

// assertUpdateStamp 断言 updated_at 已刷新为 wantUpdated 且 created_at 保持原值。
func assertUpdateStamp(t *testing.T, got Project, wantCreated, wantUpdated string) {
	t.Helper()
	if got.CreatedAt != wantCreated {
		t.Errorf("created_at = %q，期望保持 %q 不变", got.CreatedAt, wantCreated)
	}
	if got.UpdatedAt != wantUpdated {
		t.Errorf("updated_at = %q，期望刷新为注入时钟 %q", got.UpdatedAt, wantUpdated)
	}
}

// ---- ArchiveProject ----

// TestArchiveProject AC1.3：archive 后 status=archived 且 updated_at 刷新；
// 重复 archive 幂等成功且不刷 updated_at（§2.2 #3：目标不存在才 404）。
func TestArchiveProject(t *testing.T) {
	s := openTemp(t)
	seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	seedProject(t, s, "p-arc", "已归档", ProjectStatusArchived, 900, "2026-01-01T00:01:00Z")

	// 首次 archive：status 流转 + updated_at 刷新。
	const t1 = "2026-01-02T08:00:00Z"
	injectFixedClock(t, t1)
	if err := s.ArchiveProject("p-a"); err != nil {
		t.Fatalf("ArchiveProject(p-a) 失败: %v", err)
	}
	got, err := s.GetProjectByCode("p-a")
	if err != nil {
		t.Fatalf("archive 后读取失败: %v", err)
	}
	if got.Status != ProjectStatusArchived {
		t.Errorf("status = %q，期望 %q", got.Status, ProjectStatusArchived)
	}
	assertUpdateStamp(t, got, "2026-01-01T00:00:00Z", t1)

	// 已 archived 再 archive：幂等成功，且 updated_at 不刷新（换新时钟证明无副作用）。
	const t2 = "2026-01-03T08:00:00Z"
	injectFixedClock(t, t2)
	if err := s.ArchiveProject("p-a"); err != nil {
		t.Fatalf("重复 ArchiveProject 应幂等成功，实际: %v", err)
	}
	got, err = s.GetProjectByCode("p-a")
	if err != nil {
		t.Fatalf("重复 archive 后读取失败: %v", err)
	}
	if got.UpdatedAt != t1 {
		t.Errorf("重复 archive 不应刷 updated_at：= %q，期望保持 %q", got.UpdatedAt, t1)
	}

	// 对本就 archived 的夹具行 archive：同样幂等成功。
	if err := s.ArchiveProject("p-arc"); err != nil {
		t.Fatalf("对 archived 行 ArchiveProject 应幂等成功，实际: %v", err)
	}

	// 目标不存在 → ErrProjectNotFound。
	if err := s.ArchiveProject("nope"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("archive 不存在项目错误 = %v，期望哨兵 ErrProjectNotFound", err)
	}
}

// ---- ListProjects ----

// TestListProjects 默认只含 active、include_archived 全含；按 created_at,id 稳定升序；
// 空库返回非 nil 空切片（JSON 序列化 null 防线）。
func TestListProjects(t *testing.T) {
	s := openTemp(t)
	seedProject(t, s, "late", "晚建", ProjectStatusActive, 900, "2026-01-03T00:00:00Z")
	seedProject(t, s, "early", "早建", ProjectStatusActive, 1800, "2026-01-01T00:00:00Z")
	seedProject(t, s, "arc", "归档", ProjectStatusArchived, 900, "2026-01-02T00:00:00Z")

	got, err := s.ListProjects(false)
	if err != nil {
		t.Fatalf("ListProjects(false) 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("默认列表长度 = %d，期望 2（不含 archived）", len(got))
	}
	if got[0].Code != "early" || got[1].Code != "late" {
		t.Errorf("默认列表顺序 = [%s, %s]，期望 [early, late]（created_at 升序）", got[0].Code, got[1].Code)
	}
	for _, p := range got {
		if p.Status != ProjectStatusActive {
			t.Errorf("默认列表混入 %q（status=%s）", p.Code, p.Status)
		}
	}

	gotAll, err := s.ListProjects(true)
	if err != nil {
		t.Fatalf("ListProjects(true) 失败: %v", err)
	}
	if len(gotAll) != 3 {
		t.Fatalf("含归档列表长度 = %d，期望 3", len(gotAll))
	}
	wantOrder := []string{"early", "arc", "late"}
	for i, code := range wantOrder {
		if gotAll[i].Code != code {
			t.Errorf("含归档列表顺序[%d] = %s，期望 %s", i, gotAll[i].Code, code)
		}
	}
	if gotAll[1].Status != ProjectStatusArchived {
		t.Errorf("arc.status = %q，期望 archived（include_archived 应含归档行）", gotAll[1].Status)
	}

	// 空库返回非 nil 空切片而非报错（nil 切片 JSON 序列化为 null，会炸前端/CLI 渲染）。
	empty, err := Open(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("Open 空库失败: %v", err)
	}
	defer func() { _ = empty.Close() }()
	gotEmpty, err := empty.ListProjects(false)
	if err != nil {
		t.Fatalf("空库 ListProjects 失败: %v", err)
	}
	if gotEmpty == nil {
		t.Error("空库 ListProjects 返回 nil，期望非 nil 空切片（JSON null 防线）")
	}
	if len(gotEmpty) != 0 {
		t.Errorf("空库列表长度 = %d，期望 0", len(gotEmpty))
	}
}

// ---- CreateProjectWithAudit（B1-5：登记+审计同事务，spec §七「单事务 2 语句」） ----

// TestCreateProjectWithAudit 项目登记与审计落行同事务：两行同库、审计弱关联
// project_id 自动回填新项目自增 id（调用方登记时无从预知）、created_at=注入
// 时钟（AC1.4 服务端时间）、action/detail 原样落库。
func TestCreateProjectWithAudit(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	const detail = `{"after":{"code":"proj-x"}}`
	p, e, err := s.CreateProjectWithAudit(
		Project{Code: "proj-x", Name: "项目X", HeartbeatTimeoutSec: 600},
		AuditEntry{Action: AuditProjectRegister, Detail: detail},
	)
	if err != nil {
		t.Fatalf("CreateProjectWithAudit 失败: %v", err)
	}
	if p.ID <= 0 || p.Code != "proj-x" || p.HeartbeatTimeoutSec != 600 {
		t.Fatalf("返回项目行异常: %+v", p)
	}
	if e.ID <= 0 {
		t.Errorf("返回审计行 id = %d，期望 > 0（RETURNING 真值）", e.ID)
	}
	if e.ProjectID != p.ID {
		t.Errorf("审计 project_id = %d，期望回填新项目 id %d", e.ProjectID, p.ID)
	}
	if e.ColumnID != 0 || e.SessionID != 0 {
		t.Errorf("审计 column_id/session_id = %d/%d，期望调用方未填保持 0", e.ColumnID, e.SessionID)
	}
	if e.Action != AuditProjectRegister || e.Detail != detail {
		t.Errorf("审计 action/detail = %q/%q，期望原样落库", e.Action, e.Detail)
	}
	if e.CreatedAt != t0 {
		t.Errorf("审计 created_at = %q，期望注入时钟 %q（AC1.4）", e.CreatedAt, t0)
	}
	// 数据面复核：库里恰 1 项目 1 审计。
	if n := countRows(t, s, "projects"); n != 1 {
		t.Errorf("projects 行数 = %d，期望 1", n)
	}
	if n := countRows(t, s, "audit_log"); n != 1 {
		t.Errorf("audit_log 行数 = %d，期望 1", n)
	}
}

// TestCreateProjectWithAuditAtomic 同事务原子性：审计参数无效（action 空）时
// 整体回滚——项目行不得残留（登记+审计要么都在要么都不在，spec §七）。
func TestCreateProjectWithAuditAtomic(t *testing.T) {
	s := openTemp(t)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	if _, _, err := s.CreateProjectWithAudit(
		Project{Code: "proj-x", Name: "项目X"},
		AuditEntry{Action: "  "},
	); !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("审计 action 空 → 错误 = %v，期望 ErrAuditInvalid", err)
	}
	if _, err := s.GetProjectByCode("proj-x"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("回滚后查 proj-x 错误 = %v，期望 ErrProjectNotFound（项目行已随事务回滚）", err)
	}

	// code 冲突同样不落审计（项目 INSERT 失败即回滚）。
	seedProject(t, s, "dup", "已有", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	if _, _, err := s.CreateProjectWithAudit(
		Project{Code: "dup", Name: "重复"},
		AuditEntry{Action: AuditProjectRegister, Detail: "{}"},
	); !errors.Is(err, ErrProjectExists) {
		t.Fatalf("重复登记错误 = %v，期望 ErrProjectExists", err)
	}
	if n := countRows(t, s, "audit_log"); n != 0 {
		t.Errorf("冲突后 audit_log 行数 = %d，期望 0", n)
	}
}

// ---- UpdateProjectWithAudit / ArchiveProjectWithAudit（B1-5 审查修复：
// update/archive 同事务达 §七 往返账「UPDATE+audit INSERT」，before 快照
// SELECT 入事务，实测 3 语句/动作） ----

// TestUpdateProjectWithAudit 更新+审计同事务：detailFn 在事务内 before/after
// 真实行齐备后生成 detail（测试以拼串证明两行皆事务内真值）、审计弱关联回填
// after.ID、UPDATE 与 INSERT 同事务原子。
func TestUpdateProjectWithAudit(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	seedProject(t, s, "p-a", "旧名", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")

	detailFn := func(before, after Project) string {
		return `{"b":"` + before.Name + `","a":"` + after.Name + `"}`
	}
	got, e, err := s.UpdateProjectWithAudit("p-a", strPtr("新名"), 0,
		AuditEntry{Action: AuditProjectUpdate, SessionID: 5}, detailFn)
	if err != nil {
		t.Fatalf("UpdateProjectWithAudit 失败: %v", err)
	}
	if got.Name != "新名" || got.HeartbeatTimeoutSec != 900 {
		t.Errorf("更新后行 = %+v，期望 name=新名/timeout 保持 900", got)
	}
	if e.ID <= 0 || e.ProjectID != got.ID {
		t.Errorf("审计 id/project_id = %d/%d，期望 >0/回填 %d", e.ID, e.ProjectID, got.ID)
	}
	if e.SessionID != 5 {
		t.Errorf("审计 session_id = %d，期望保留传入 5", e.SessionID)
	}
	if e.Detail != `{"b":"旧名","a":"新名"}` {
		t.Errorf("审计 detail = %q，期望事务内真值拼串（before=旧名/after=新名）", e.Detail)
	}
	if n := countRows(t, s, "audit_log"); n != 1 {
		t.Errorf("audit_log 行数 = %d，期望 1", n)
	}

	// 空更新 → ErrNoFields（事务外参数校验），不落审计。
	if _, _, err := s.UpdateProjectWithAudit("p-a", nil, 0,
		AuditEntry{Action: AuditProjectUpdate}, detailFn); !errors.Is(err, ErrNoFields) {
		t.Errorf("空更新错误 = %v，期望 ErrNoFields", err)
	}
	if n := countRows(t, s, "audit_log"); n != 1 {
		t.Errorf("空更新后 audit_log 行数 = %d，期望仍 1", n)
	}

	// 目标不存在 → ErrProjectNotFound，无审计。
	if _, _, err := s.UpdateProjectWithAudit("nope", strPtr("x"), 0,
		AuditEntry{Action: AuditProjectUpdate}, detailFn); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("目标不存在错误 = %v，期望 ErrProjectNotFound", err)
	}
	if n := countRows(t, s, "audit_log"); n != 1 {
		t.Errorf("404 后 audit_log 行数 = %d，期望仍 1", n)
	}
}

// TestUpdateProjectWithAuditAtomic 同事务原子性：审计参数无效（action 空）时
// UPDATE 整体回滚——字段变更不得生效而留痕缺失（AC1.4 原子前提）。
func TestUpdateProjectWithAuditAtomic(t *testing.T) {
	s := openTemp(t)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	seedProject(t, s, "p-a", "旧名", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")

	if _, _, err := s.UpdateProjectWithAudit("p-a", strPtr("新名"), 0,
		AuditEntry{Action: " "}, func(before, after Project) string { return "{}" }); !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("审计 action 空 → 错误 = %v，期望 ErrAuditInvalid", err)
	}
	p, err := s.GetProjectByCode("p-a")
	if err != nil {
		t.Fatalf("查询项目失败: %v", err)
	}
	if p.Name != "旧名" {
		t.Errorf("回滚后 name = %q，期望保持 %q（UPDATE 已随事务回滚）", p.Name, "旧名")
	}
	if n := countRows(t, s, "audit_log"); n != 0 {
		t.Errorf("audit_log 行数 = %d，期望 0", n)
	}
}

// TestArchiveProjectWithAudit 归档+审计同事务：detailFn 拿事务内真实前态
// （幂等重放时 before.Status=archived 诚实入快照）、CASE WHEN 幂等语义保留、
// 目标不存在 404 无审计、action 空回滚 status 不变。
func TestArchiveProjectWithAudit(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")

	detailFn := func(before Project) string {
		return `{"was":"` + before.Status + `"}`
	}
	e, err := s.ArchiveProjectWithAudit("p-a",
		AuditEntry{Action: AuditProjectArchive, SessionID: 3}, detailFn)
	if err != nil {
		t.Fatalf("ArchiveProjectWithAudit 失败: %v", err)
	}
	if e.ID <= 0 || e.Detail != `{"was":"active"}` {
		t.Errorf("审计 = %+v，期望 id>0 且 detail 记真实前态 active", e)
	}
	if p, err := s.GetProjectByCode("p-a"); err != nil || p.Status != ProjectStatusArchived {
		t.Fatalf("归档后 status = %v（err=%v），期望 archived", p, err)
	}

	// 幂等重放：再归档成功，before.Status=archived 诚实入快照，审计照落。
	e2, err := s.ArchiveProjectWithAudit("p-a",
		AuditEntry{Action: AuditProjectArchive}, detailFn)
	if err != nil {
		t.Fatalf("幂等重放失败: %v", err)
	}
	if e2.Detail != `{"was":"archived"}` {
		t.Errorf("重放审计 detail = %q，期望 {\"was\":\"archived\"}（真实前态）", e2.Detail)
	}
	if n := countRows(t, s, "audit_log"); n != 2 {
		t.Errorf("audit_log 行数 = %d，期望 2（动作发生即留痕）", n)
	}

	// 目标不存在 → ErrProjectNotFound，无审计。
	if _, err := s.ArchiveProjectWithAudit("nope",
		AuditEntry{Action: AuditProjectArchive}, detailFn); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("目标不存在错误 = %v，期望 ErrProjectNotFound", err)
	}
	if n := countRows(t, s, "audit_log"); n != 2 {
		t.Errorf("404 后 audit_log 行数 = %d，期望仍 2", n)
	}

	// 原子性：action 空 → 回滚，status 不变（另一 active 项目验证）。
	seedProject(t, s, "p-b", "项目B", ProjectStatusActive, 900, "2026-01-01T00:01:00Z")
	if _, err := s.ArchiveProjectWithAudit("p-b",
		AuditEntry{Action: " "}, detailFn); !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("action 空 → 错误 = %v，期望 ErrAuditInvalid", err)
	}
	if p, err := s.GetProjectByCode("p-b"); err != nil || p.Status != ProjectStatusActive {
		t.Errorf("回滚后 p-b status = %v（err=%v），期望保持 active", p, err)
	}
}
