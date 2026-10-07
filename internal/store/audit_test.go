package store

import (
	"errors"
	"strconv"
	"testing"
)

// seedAudit 直插一行 audit_log 夹具，返回自增 id（不依赖 InsertAudit）。
func seedAudit(t *testing.T, s *Store, projectID, columnID, sessionID int64, action, detail, createdAt string) int64 {
	t.Helper()
	res, err := s.DB.Exec(
		`INSERT INTO audit_log (project_id, column_id, session_id, action, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, columnID, sessionID, action, detail, createdAt,
	)
	if err != nil {
		t.Fatalf("插入夹具审计行 %q 失败: %v", action, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取夹具审计行自增 id 失败: %v", err)
	}
	return id
}

// ---- InsertAudit ----

// TestInsertAudit AC1.4 审计写入：全字段经 RETURNING 返回库中真实行（id/服务端
// 时间注入）；三弱关联 id 全 0=系统动作直插（DDL 三列 NOT NULL DEFAULT 0、无 FK）；
// detail 为 JSON 文本原样落库（调用方 marshal，store 不校验 JSON 形状）。
func TestInsertAudit(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	// 全字段：返回库中行。
	got, err := s.InsertAudit(AuditEntry{
		ProjectID: 7, ColumnID: 3, SessionID: 42,
		Action: AuditSessionAutoRegister,
		Detail: `{"after":{"name":"executor-A","role":"executor"}}`,
	})
	if err != nil {
		t.Fatalf("InsertAudit 失败: %v", err)
	}
	if got.ID <= 0 {
		t.Errorf("id = %d，期望 > 0", got.ID)
	}
	if got.ProjectID != 7 || got.ColumnID != 3 || got.SessionID != 42 {
		t.Errorf("三弱关联 = %d/%d/%d，期望 7/3/42", got.ProjectID, got.ColumnID, got.SessionID)
	}
	if got.Action != AuditSessionAutoRegister {
		t.Errorf("action = %q，期望 %q", got.Action, AuditSessionAutoRegister)
	}
	if got.Detail != `{"after":{"name":"executor-A","role":"executor"}}` {
		t.Errorf("detail = %q，期望原样落库", got.Detail)
	}
	if got.CreatedAt != t0 {
		t.Errorf("created_at = %q，期望服务端注入时钟 %q", got.CreatedAt, t0)
	}

	// 三弱关联全 0=系统动作：直插成功（弱关联不设 FK，0 值合法）。
	sys, err := s.InsertAudit(AuditEntry{Action: AuditRoleChange, Detail: `{"before":"executor","after":"foreman"}`})
	if err != nil {
		t.Fatalf("系统动作（三弱关联 0）InsertAudit 失败: %v", err)
	}
	if sys.ProjectID != 0 || sys.ColumnID != 0 || sys.SessionID != 0 {
		t.Errorf("系统动作三弱关联 = %d/%d/%d，期望 0/0/0", sys.ProjectID, sys.ColumnID, sys.SessionID)
	}

	// detail 空串：原样落库（store 忠实写入，不替调用方兜 JSON 形状）。
	if _, err := s.InsertAudit(AuditEntry{Action: AuditRoleChange}); err != nil {
		t.Fatalf("空 detail InsertAudit 失败: %v", err)
	}
	var detail string
	if err := s.DB.QueryRow(`SELECT detail FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatalf("读回 detail 失败: %v", err)
	}
	if detail != "" {
		t.Errorf("空 detail 落库 = %q，期望原样空串", detail)
	}
}

// TestInsertAuditRejectsEmptyAction 空 action 防御：空串与纯空白都拒
// （哨兵 ErrAuditInvalid——动作必须可归因，AC1.4 留痕语义），且不落数据面。
func TestInsertAuditRejectsEmptyAction(t *testing.T) {
	s := openTemp(t)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	for _, action := range []string{"", "   ", "\t\n"} {
		if _, err := s.InsertAudit(AuditEntry{Action: action}); !errors.Is(err, ErrAuditInvalid) {
			t.Errorf("空 action %q 错误 = %v，期望哨兵 ErrAuditInvalid", action, err)
		}
	}

	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatalf("统计 audit_log 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("空 action 拒绝后 audit_log 行数 = %d，期望 0（不应落库）", n)
	}
}

// ---- QueryAudit ----

// TestQueryAudit 过滤与 limit 边界：projectID 0=不过滤、action 空=不过滤、
// 按 id 倒序（新事件在前，idx_audit_project 支撑）；limit 默认 100/上限 1000
// clamp 在 store 层（#28 端点契约：handler 传默认 100，store 兜底 ≤0 并 clamp 上限）。
func TestQueryAudit(t *testing.T) {
	s := openTemp(t)
	const created = "2026-01-01T08:00:00Z"
	pidA := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, created)
	pidB := seedProject(t, s, "p-b", "项目B", ProjectStatusActive, 900, created)

	// 夹具：项目 A 两条 auto_register（id 1、2）+一条 role-change（id 3）；
	// 项目 B 一条 auto_register（id 4）；系统动作一条（id 5，三弱关联 0）。
	seedAudit(t, s, pidA, 1, 11, AuditSessionAutoRegister, `{}`, created)
	seedAudit(t, s, pidA, 1, 12, AuditSessionAutoRegister, `{}`, created)
	seedAudit(t, s, pidA, 1, 11, AuditRoleChange, `{"before":"executor","after":"foreman"}`, created)
	seedAudit(t, s, pidB, 2, 21, AuditSessionAutoRegister, `{}`, created)
	seedAudit(t, s, 0, 0, 0, AuditRoleChange, `{}`, created)

	// 不过滤：全部 5 条，id 倒序（新在前）。
	all, err := s.QueryAudit(0, "", 100)
	if err != nil {
		t.Fatalf("QueryAudit(不过滤) 失败: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("不过滤结果长度 = %d，期望 5", len(all))
	}
	if all[0].ID != 5 || all[4].ID != 1 {
		t.Errorf("不过滤首尾 id = %d/%d，期望 5/1（id 倒序）", all[0].ID, all[4].ID)
	}

	// project 过滤。
	gotA, err := s.QueryAudit(pidA, "", 100)
	if err != nil {
		t.Fatalf("QueryAudit(project) 失败: %v", err)
	}
	if len(gotA) != 3 {
		t.Fatalf("project 过滤长度 = %d，期望 3", len(gotA))
	}
	for _, e := range gotA {
		if e.ProjectID != pidA {
			t.Errorf("project 过滤混入 project_id=%d（期望 %d）", e.ProjectID, pidA)
		}
	}

	// action 过滤（含系统行——projectID=0 过滤只按条件，不过滤时系统行也命中）。
	gotRole, err := s.QueryAudit(0, AuditRoleChange, 100)
	if err != nil {
		t.Fatalf("QueryAudit(action) 失败: %v", err)
	}
	if len(gotRole) != 2 {
		t.Fatalf("action 过滤长度 = %d，期望 2", len(gotRole))
	}
	for _, e := range gotRole {
		if e.Action != AuditRoleChange {
			t.Errorf("action 过滤混入 %q", e.Action)
		}
	}

	// 组合过滤。
	gotBoth, err := s.QueryAudit(pidA, AuditRoleChange, 100)
	if err != nil {
		t.Fatalf("QueryAudit(组合) 失败: %v", err)
	}
	if len(gotBoth) != 1 || gotBoth[0].Action != AuditRoleChange || gotBoth[0].ProjectID != pidA {
		t.Fatalf("组合过滤 = %+v，期望恰一条项目 A 的 role-change", gotBoth)
	}

	// 无命中：非 nil 空切片。
	none, err := s.QueryAudit(0, "no.such_action", 100)
	if err != nil {
		t.Fatalf("QueryAudit(无命中) 失败: %v", err)
	}
	if none == nil {
		t.Error("无命中返回 nil，期望非 nil 空切片（JSON null 防线）")
	}
	if len(none) != 0 {
		t.Errorf("无命中长度 = %d，期望 0", len(none))
	}

	// limit 边界：批量插 1001 条（测试夹具一次性循环直插豁免往返纪律，非生产路径）。
	for i := range 1001 {
		if _, err := s.InsertAudit(AuditEntry{
			ProjectID: pidB, Action: AuditSessionAutoRegister,
			Detail: `{"i":` + strconv.Itoa(i) + `}`,
		}); err != nil {
			t.Fatalf("批量夹具第 %d 条失败: %v", i+1, err)
		}
	}

	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{"恰等于上限 1000", 1000, 1000},
		{"超上限 clamp", 5000, 1000},
		{"默认 100", 100, 100},
		{"≤0 兜底默认 100", 0, 100},
		{"负数兜底默认 100", -3, 100},
	}
	for _, tc := range cases {
		got, err := s.QueryAudit(0, "", tc.limit)
		if err != nil {
			t.Fatalf("QueryAudit(%s) 失败: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("limit=%d（%s）结果长度 = %d，期望 %d", tc.limit, tc.name, len(got), tc.want)
		}
	}
	// 上限截断后仍 id 倒序（最新在前）。
	top, err := s.QueryAudit(0, "", 1000)
	if err != nil {
		t.Fatalf("QueryAudit(1000) 失败: %v", err)
	}
	if top[0].ID != 1006 { // 5 条夹具 + 1001 条批量 = 1006 行
		t.Errorf("limit=1000 首行 id = %d，期望最新 1006", top[0].ID)
	}
}
