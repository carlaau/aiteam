package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// ---- 测试夹具 ----

// seedColumn 直插一行 columns 夹具，返回自增 id（不依赖 CRUD 方法）。
func seedColumn(t *testing.T, s *Store, projectID int64, code, name, status, createdAt string) int64 {
	t.Helper()
	res, err := s.DB.Exec(
		`INSERT INTO columns (project_id, code, name, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, code, name, status, createdAt, createdAt,
	)
	if err != nil {
		t.Fatalf("插入夹具栏目 %q 失败: %v", code, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取夹具栏目 %q 自增 id 失败: %v", code, err)
	}
	return id
}

// seedMessage 直插一行 messages 夹具，返回 seq（append-only 表最小字段集：
// project_id/column_id/kind/level/body/created_at，其余列走 DDL 默认值；
// column_id 外键指向 columns(id)，调用方须保证栏目行已存在）。
func seedMessage(t *testing.T, s *Store, projectID, columnID int64, kind, level, body, createdAt string) int64 {
	t.Helper()
	res, err := s.DB.Exec(
		`INSERT INTO messages (project_id, column_id, kind, level, body, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, columnID, kind, level, body, createdAt,
	)
	if err != nil {
		t.Fatalf("插入夹具消息失败: %v", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取夹具消息 seq 失败: %v", err)
	}
	return seq
}

// ---- CreateColumn ----

// TestCreateColumn AC2.1：成功经 RETURNING 返回库中真实行；同项目重复 code 唯一
// 冲突返回哨兵 ErrColumnExists（handler 映射 column_exists 409）；跨项目同 code
// 合法（UNIQUE(project_id, code) 按项目隔离）；项目未命中返回 ErrProjectNotFound
// （handler 映射 project_not_found 404）。
func TestCreateColumn(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	pidA := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")

	// 成功：返回值是落库后 RETURNING 的真实行。
	got, err := s.CreateColumn("p-a", Column{Code: "05", Name: "栏目05"})
	if err != nil {
		t.Fatalf("CreateColumn(p-a, 05) 失败: %v", err)
	}
	if got.ID <= 0 {
		t.Errorf("id = %d，期望 > 0", got.ID)
	}
	if got.ProjectID != pidA {
		t.Errorf("project_id = %d，期望夹具项目 id %d", got.ProjectID, pidA)
	}
	if got.Code != "05" || got.Name != "栏目05" {
		t.Errorf("code/name = %q/%q，期望 05/栏目05", got.Code, got.Name)
	}
	if got.Status != ColumnStatusActive {
		t.Errorf("status = %q，期望默认 %q", got.Status, ColumnStatusActive)
	}
	if got.CreatedAt != t0 || got.UpdatedAt != t0 {
		t.Errorf("created_at/updated_at = %q/%q，期望注入时钟 %q", got.CreatedAt, got.UpdatedAt, t0)
	}

	// 同项目重复 code（AC2.1 重名拒数据面）→ ErrColumnExists。
	got2, err := s.CreateColumn("p-a", Column{Code: "05", Name: "重复"})
	if !errors.Is(err, ErrColumnExists) {
		t.Errorf("同项目重复 code 错误 = %v，期望哨兵 ErrColumnExists", err)
	}
	if got2.ID != 0 {
		t.Errorf("冲突时返回 id = %d，期望零值", got2.ID)
	}

	// 跨项目同 code 合法：唯一约束按 (project_id, code) 隔离。
	pidB := seedProject(t, s, "p-b", "项目B", ProjectStatusActive, 900, "2026-01-01T00:01:00Z")
	got3, err := s.CreateColumn("p-b", Column{Code: "05", Name: "B 的 05"})
	if err != nil {
		t.Fatalf("跨项目同 code 应合法，实际失败: %v", err)
	}
	if got3.ProjectID != pidB {
		t.Errorf("跨项目同 code 的 project_id = %d，期望 %d", got3.ProjectID, pidB)
	}

	// 项目不存在 → ErrProjectNotFound（事务回滚，不落任何行）。
	got4, err := s.CreateColumn("nope", Column{Code: "99", Name: "孤儿"})
	if !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("项目不存在错误 = %v，期望哨兵 ErrProjectNotFound", err)
	}
	if got4.ID != 0 {
		t.Errorf("项目不存在时返回 id = %d，期望零值", got4.ID)
	}

	// 落库行数 = 2（冲突/失败插入未落数据面）。
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM columns`).Scan(&n); err != nil {
		t.Fatalf("统计 columns 失败: %v", err)
	}
	if n != 2 {
		t.Errorf("columns 行数 = %d，期望 2", n)
	}
}

// TestCreateColumnRejectsEmptyCode 空 code 防御：空串与纯空白都拒（哨兵
// ErrColumnInvalid），且不落数据面。
func TestCreateColumnRejectsEmptyCode(t *testing.T) {
	s := openTemp(t)
	seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")

	for _, code := range []string{"", "   ", "\t\n"} {
		if _, err := s.CreateColumn("p-a", Column{Code: code}); !errors.Is(err, ErrColumnInvalid) {
			t.Errorf("空 code %q 错误 = %v，期望哨兵 ErrColumnInvalid", code, err)
		}
	}

	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM columns`).Scan(&n); err != nil {
		t.Fatalf("统计 columns 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("空 code 拒绝后 columns 行数 = %d，期望 0（不应落库）", n)
	}
}

// ---- TestColumnRegisterPositions（AC2.1 副作用 + D7 不回看） ----

// TestColumnRegisterPositions 登记栏目同事务预置 controller/executor 两角色信箱
// 位点，position=登记时刻全局 MAX(seq)——D7 不回看（总线消息不回灌新栏目）；
// 空库无消息时 COALESCE(MAX(seq), 0) 兜底 0（§3.6 原样 SQL）。
func TestColumnRegisterPositions(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")

	// messages.column_id 外键指向 columns(id)：夹具旧栏目须先行，3 行消息落在
	// 旧栏目——模拟「项目已有其他栏目在收发」场景，新栏目位点从当时 MAX(seq)=3 起算。
	oldID := seedColumn(t, s, pid, "01", "栏目01", ColumnStatusActive, "2026-01-01T00:00:00Z")
	for i := 1; i <= 3; i++ {
		seedMessage(t, s, pid, oldID, "bus", "normal", fmt.Sprintf("msg-%d", i), "2026-01-01T00:00:00Z")
	}

	got, err := s.CreateColumn("p-a", Column{Code: "02", Name: "栏目02"})
	if err != nil {
		t.Fatalf("CreateColumn(02) 失败: %v", err)
	}

	// 全表恰好 2 行（controller/executor 各一，无多余）。
	var total int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ack_positions`).Scan(&total); err != nil {
		t.Fatalf("统计 ack_positions 失败: %v", err)
	}
	if total != 2 {
		t.Fatalf("ack_positions 行数 = %d，期望 2（controller/executor 各一行）", total)
	}

	// 逐行断言：project_id=夹具项目（防 InitColumnPositions 参数序写反成 columnID
	// 而测试抓不到）、position=3（登记时刻全局 MAX(seq)）、updated_at=注入时钟。
	want := map[string]int64{"controller": 3, "executor": 3}
	rows, err := s.DB.Query(
		`SELECT project_id, consumer, position, updated_at FROM ack_positions WHERE column_id = ? ORDER BY consumer`,
		got.ID,
	)
	if err != nil {
		t.Fatalf("查询位点失败: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var consumer, updatedAt string
		var rowProjectID, position int64
		if err := rows.Scan(&rowProjectID, &consumer, &position, &updatedAt); err != nil {
			t.Fatalf("扫描位点行失败: %v", err)
		}
		if rowProjectID != pid {
			t.Errorf("位点 %s project_id = %d，期望夹具项目 id %d（参数序防回归）", consumer, rowProjectID, pid)
		}
		if want[consumer] != position {
			t.Errorf("位点 %s position = %d，期望 %d（登记时刻全局 MAX(seq)，D7 不回看）", consumer, position, want[consumer])
		}
		if updatedAt != t0 {
			t.Errorf("位点 %s updated_at = %q，期望注入时钟 %q", consumer, updatedAt, t0)
		}
		seen[consumer] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历位点失败: %v", err)
	}
	for consumer := range want {
		if !seen[consumer] {
			t.Errorf("位点缺 consumer=%s", consumer)
		}
	}

	// 场景二：空库无任何消息 → COALESCE(MAX(seq), 0) 兜底 position=0。
	s2 := openTemp(t)
	seedProject(t, s2, "p-b", "项目B", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	got2, err := s2.CreateColumn("p-b", Column{Code: "01", Name: "首个栏目"})
	if err != nil {
		t.Fatalf("空库 CreateColumn 失败: %v", err)
	}
	rows2, err := s2.DB.Query(
		`SELECT consumer, position FROM ack_positions WHERE column_id = ? ORDER BY consumer`,
		got2.ID,
	)
	if err != nil {
		t.Fatalf("查询空库位点失败: %v", err)
	}
	defer rows2.Close()
	wantConsumers := map[string]bool{"controller": true, "executor": true}
	seenConsumers := map[string]bool{}
	for rows2.Next() {
		var consumer string
		var position int64
		if err := rows2.Scan(&consumer, &position); err != nil {
			t.Fatalf("扫描空库位点行失败: %v", err)
		}
		if position != 0 {
			t.Errorf("空库位点 %s position = %d，期望 0（COALESCE 兜底）", consumer, position)
		}
		if !wantConsumers[consumer] {
			t.Errorf("空库位点混入意外 consumer=%q（期望恰为 controller/executor）", consumer)
		}
		seenConsumers[consumer] = true
	}
	if err := rows2.Err(); err != nil {
		t.Fatalf("遍历空库位点失败: %v", err)
	}
	if len(seenConsumers) != 2 {
		t.Errorf("空库位点 consumer 集合 = %v，期望恰为 controller/executor 各一", seenConsumers)
	}
	for consumer := range wantConsumers {
		if !seenConsumers[consumer] {
			t.Errorf("空库位点缺 consumer=%s", consumer)
		}
	}
}

// ---- UpdateColumn ----

// TestUpdateColumn name 更新 + updated_at 刷新、created_at 不变；name 未提供
// 即空更新拒绝（ErrNoFields，columns 可变字段仅 name）；栏目不存在或项目不存在
// 均归并 ErrColumnNotFound（端点 #6 特有错误仅 column_not_found）。
func TestUpdateColumn(t *testing.T) {
	s := openTemp(t)
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	const created = "2026-01-01T00:00:00Z"
	seedColumn(t, s, pid, "05", "旧名", ColumnStatusActive, created)
	const t1 = "2026-01-02T08:00:00Z"
	injectFixedClock(t, t1)

	// 更新成功：返回库中真实行。
	got, err := s.UpdateColumn("p-a", "05", strPtr("新名"))
	if err != nil {
		t.Fatalf("UpdateColumn 失败: %v", err)
	}
	if got.Name != "新名" || got.Code != "05" || got.ProjectID != pid {
		t.Errorf("name/code/project_id = %q/%q/%d，期望 新名/05/%d", got.Name, got.Code, got.ProjectID, pid)
	}
	if got.CreatedAt != created || got.UpdatedAt != t1 {
		t.Errorf("created_at/updated_at = %q/%q，期望 %q/%q（updated_at 刷新）", got.CreatedAt, got.UpdatedAt, created, t1)
	}

	// 空串合法：指针提供即更新。
	got, err = s.UpdateColumn("p-a", "05", strPtr(""))
	if err != nil {
		t.Fatalf("UpdateColumn(空串名) 失败: %v", err)
	}
	if got.Name != "" {
		t.Errorf("name = %q，期望空串合法（指针区分「未提供」）", got.Name)
	}

	// 空更新拒绝：name 未提供 → ErrNoFields，行不变。
	snapshot := func(t *testing.T) Column {
		t.Helper()
		cols, err := s.ListColumns("p-a", true)
		if err != nil {
			t.Fatalf("读取栏目快照失败: %v", err)
		}
		if len(cols) != 1 {
			t.Fatalf("栏目快照行数 = %d，期望 1", len(cols))
		}
		return cols[0]
	}
	before := snapshot(t)
	if _, err := s.UpdateColumn("p-a", "05", nil); !errors.Is(err, ErrNoFields) {
		t.Errorf("空更新错误 = %v，期望哨兵 ErrNoFields", err)
	}
	if after := snapshot(t); after != before {
		t.Errorf("空更新不应改动行：before=%+v after=%+v", before, after)
	}

	// 栏目不存在 → ErrColumnNotFound。
	if _, err := s.UpdateColumn("p-a", "nope", strPtr("x")); !errors.Is(err, ErrColumnNotFound) {
		t.Errorf("更新不存在栏目错误 = %v，期望哨兵 ErrColumnNotFound", err)
	}

	// 项目不存在：定位子查询落空，同归 ErrColumnNotFound。
	if _, err := s.UpdateColumn("nope", "05", strPtr("x")); !errors.Is(err, ErrColumnNotFound) {
		t.Errorf("项目不存在更新错误 = %v，期望哨兵 ErrColumnNotFound", err)
	}
}

// ---- ArchiveColumn ----

// TestArchiveColumn AC2.3 同 AC1.3 型：archive 后 status=archived 且 updated_at
// 刷新；重复 archive 幂等成功且不刷 updated_at；栏目/项目不存在归并
// ErrColumnNotFound。
func TestArchiveColumn(t *testing.T) {
	s := openTemp(t)
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	seedColumn(t, s, pid, "05", "栏目05", ColumnStatusActive, "2026-01-01T00:00:00Z")
	seedColumn(t, s, pid, "arc0", "已归档", ColumnStatusArchived, "2026-01-01T00:01:00Z")

	// 首次 archive：status 流转 + updated_at 刷新。
	const t1 = "2026-01-02T08:00:00Z"
	injectFixedClock(t, t1)
	if err := s.ArchiveColumn("p-a", "05"); err != nil {
		t.Fatalf("ArchiveColumn(05) 失败: %v", err)
	}
	cols, err := s.ListColumns("p-a", true)
	if err != nil {
		t.Fatalf("archive 后读取列表失败: %v", err)
	}
	var got *Column
	for i := range cols {
		if cols[i].Code == "05" {
			got = &cols[i]
		}
	}
	if got == nil {
		t.Fatal("archive 后列表缺 05")
	}
	if got.Status != ColumnStatusArchived {
		t.Errorf("status = %q，期望 %q", got.Status, ColumnStatusArchived)
	}
	if got.CreatedAt != "2026-01-01T00:00:00Z" || got.UpdatedAt != t1 {
		t.Errorf("created_at/updated_at = %q/%q，期望 created 不变 + updated 刷新为 %q", got.CreatedAt, got.UpdatedAt, t1)
	}

	// 重复 archive：幂等成功，且 updated_at 不刷新（换新时钟证明无副作用）。
	const t2 = "2026-01-03T08:00:00Z"
	injectFixedClock(t, t2)
	if err := s.ArchiveColumn("p-a", "05"); err != nil {
		t.Fatalf("重复 ArchiveColumn 应幂等成功，实际: %v", err)
	}
	cols, err = s.ListColumns("p-a", true)
	if err != nil {
		t.Fatalf("重复 archive 后读取列表失败: %v", err)
	}
	for _, c := range cols {
		if c.Code == "05" && c.UpdatedAt != t1 {
			t.Errorf("重复 archive 不应刷 updated_at：= %q，期望保持 %q", c.UpdatedAt, t1)
		}
	}

	// 对本就 archived 的夹具行 archive：同样幂等成功。
	if err := s.ArchiveColumn("p-a", "arc0"); err != nil {
		t.Fatalf("对 archived 行 ArchiveColumn 应幂等成功，实际: %v", err)
	}

	// 栏目不存在 → ErrColumnNotFound。
	if err := s.ArchiveColumn("p-a", "nope"); !errors.Is(err, ErrColumnNotFound) {
		t.Errorf("archive 不存在栏目错误 = %v，期望哨兵 ErrColumnNotFound", err)
	}

	// 项目不存在 → 归并 ErrColumnNotFound。
	if err := s.ArchiveColumn("nope", "05"); !errors.Is(err, ErrColumnNotFound) {
		t.Errorf("archive 不存在项目的栏目错误 = %v，期望哨兵 ErrColumnNotFound", err)
	}
}

// ---- ListColumns ----

// TestListColumns 默认只含 active、include_archived 全含；按 created_at,id 稳定
// 升序；项目间隔离；项目不存在返回 ErrProjectNotFound；空项目返回非 nil 空切片
// （JSON 序列化 null 防线）。
func TestListColumns(t *testing.T) {
	s := openTemp(t)
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	seedColumn(t, s, pid, "late", "晚建", ColumnStatusActive, "2026-01-03T00:00:00Z")
	seedColumn(t, s, pid, "early", "早建", ColumnStatusActive, "2026-01-01T00:00:00Z")
	seedColumn(t, s, pid, "arc", "归档", ColumnStatusArchived, "2026-01-02T00:00:00Z")
	pidB := seedProject(t, s, "p-b", "项目B", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	seedColumn(t, s, pidB, "b05", "B 栏目", ColumnStatusActive, "2026-01-01T00:00:00Z")

	// 默认：仅 active、按 created_at 升序、只含本项目。
	got, err := s.ListColumns("p-a", false)
	if err != nil {
		t.Fatalf("ListColumns(p-a, false) 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("默认列表长度 = %d，期望 2（不含 archived）", len(got))
	}
	if got[0].Code != "early" || got[1].Code != "late" {
		t.Errorf("默认列表顺序 = [%s, %s]，期望 [early, late]（created_at 升序）", got[0].Code, got[1].Code)
	}
	for _, c := range got {
		if c.Status != ColumnStatusActive {
			t.Errorf("默认列表混入 %q（status=%s）", c.Code, c.Status)
		}
		if c.ProjectID != pid {
			t.Errorf("默认列表混入其他项目行 %q（project_id=%d）", c.Code, c.ProjectID)
		}
	}

	// include_archived：全含 3 行。
	gotAll, err := s.ListColumns("p-a", true)
	if err != nil {
		t.Fatalf("ListColumns(p-a, true) 失败: %v", err)
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
	if gotAll[1].Status != ColumnStatusArchived {
		t.Errorf("arc.status = %q，期望 archived（include_archived 应含归档行）", gotAll[1].Status)
	}

	// 项目隔离：p-b 只有 b05。
	gotB, err := s.ListColumns("p-b", true)
	if err != nil {
		t.Fatalf("ListColumns(p-b, true) 失败: %v", err)
	}
	if len(gotB) != 1 || gotB[0].Code != "b05" {
		t.Errorf("p-b 列表 = %v，期望仅 [b05]", gotB)
	}

	// 项目不存在 → ErrProjectNotFound。
	if _, err := s.ListColumns("nope", false); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("项目不存在列表错误 = %v，期望哨兵 ErrProjectNotFound", err)
	}

	// 空项目（存在无栏目）→ 非 nil 空切片而非报错。
	seedProject(t, s, "p-c", "空项目", ProjectStatusActive, 900, "2026-01-01T00:02:00Z")
	gotC, err := s.ListColumns("p-c", false)
	if err != nil {
		t.Fatalf("空项目 ListColumns 失败: %v", err)
	}
	if gotC == nil {
		t.Error("空项目 ListColumns 返回 nil，期望非 nil 空切片（JSON null 防线）")
	}
	if len(gotC) != 0 {
		t.Errorf("空项目列表长度 = %d，期望 0", len(gotC))
	}
}

// ---- CreateColumnWithAudit / GetColumnByCode / ColumnCountsByProject（B1-5） ----

// TestCreateColumnWithAudit 栏目登记+位点预置+审计同事务（spec §七「column
// register = 单事务 3 语句」型）：三面同库、审计弱关联 project_id/column_id
// 自动回填返回栏目行、created_at=注入时钟。
func TestCreateColumnWithAudit(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")

	const detail = `{"after":{"code":"02"}}`
	col, e, err := s.CreateColumnWithAudit(
		"p-a",
		Column{Code: "02", Name: "栏目02"},
		AuditEntry{Action: AuditColumnRegister, Detail: detail, SessionID: 7},
	)
	if err != nil {
		t.Fatalf("CreateColumnWithAudit 失败: %v", err)
	}
	if col.ID <= 0 || col.ProjectID != pid {
		t.Fatalf("返回栏目行异常: %+v", col)
	}
	if e.ID <= 0 {
		t.Errorf("返回审计行 id = %d，期望 > 0（RETURNING 真值）", e.ID)
	}
	if e.ProjectID != pid || e.ColumnID != col.ID {
		t.Errorf("审计弱关联 = (%d,%d)，期望回填 (%d,%d)", e.ProjectID, e.ColumnID, pid, col.ID)
	}
	if e.SessionID != 7 {
		t.Errorf("审计 session_id = %d，期望保留调用方传入 7", e.SessionID)
	}
	if e.Action != AuditColumnRegister || e.Detail != detail {
		t.Errorf("审计 action/detail = %q/%q，期望原样落库", e.Action, e.Detail)
	}
	if e.CreatedAt != t0 {
		t.Errorf("审计 created_at = %q，期望注入时钟 %q（AC1.4）", e.CreatedAt, t0)
	}

	// 副作用可见：位点预置照常落 2 行（与纯 CreateColumn 同一事务体）。
	if n := countRows(t, s, "ack_positions"); n != 2 {
		t.Errorf("ack_positions 行数 = %d，期望 2（controller/executor）", n)
	}
}

// TestCreateColumnWithAuditAtomic 同事务原子性：审计参数无效（action 空）时
// 栏目行+位点预置+审计整体回滚（登记+预置+审计要么都在要么都不在）。
func TestCreateColumnWithAuditAtomic(t *testing.T) {
	s := openTemp(t)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")

	if _, _, err := s.CreateColumnWithAudit(
		"p-a", Column{Code: "02", Name: "栏目02"},
		AuditEntry{Action: " "},
	); !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("审计 action 空 → 错误 = %v，期望 ErrAuditInvalid", err)
	}
	if n := countRows(t, s, "columns"); n != 0 {
		t.Errorf("回滚后 columns 行数 = %d，期望 0", n)
	}
	if n := countRows(t, s, "ack_positions"); n != 0 {
		t.Errorf("回滚后 ack_positions 行数 = %d，期望 0（位点预置同事务回滚）", n)
	}
	if n := countRows(t, s, "audit_log"); n != 0 {
		t.Errorf("回滚后 audit_log 行数 = %d，期望 0", n)
	}

	// 项目不存在 → ErrProjectNotFound（端点 #5 project_not_found 404），无任何落库。
	if _, _, err := s.CreateColumnWithAudit(
		"nope", Column{Code: "02"},
		AuditEntry{Action: AuditColumnRegister, Detail: "{}"},
	); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("项目不存在错误 = %v，期望 ErrProjectNotFound", err)
	}
	if n := countRows(t, s, "columns"); n != 0 {
		t.Errorf("项目未命中后 columns 行数 = %d，期望 0", n)
	}
}

// TestGetColumnByCode 按（项目 code, 栏目 code）精确定位：命中返回整行；
// 未命中/跨项目同号栏目不串 → ErrColumnNotFound（端点 #6 特有错误仅
// column_not_found 的归并口径）。
func TestGetColumnByCode(t *testing.T) {
	s := openTemp(t)
	pidA := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	pidB := seedProject(t, s, "p-b", "项目B", ProjectStatusActive, 900, "2026-01-01T00:01:00Z")
	seedColumn(t, s, pidA, "05", "A05", ColumnStatusActive, "2026-01-01T00:00:00Z")
	seedColumn(t, s, pidB, "05", "B05", ColumnStatusArchived, "2026-01-01T00:01:00Z")

	// 命中（archived 照常返回：PATCH/DELETE 以 code 定位、不筛 status）。
	got, err := s.GetColumnByCode("p-a", "05")
	if err != nil {
		t.Fatalf("GetColumnByCode(p-a, 05) 失败: %v", err)
	}
	if got.Name != "A05" || got.ProjectID != pidA {
		t.Errorf("命中行 = %+v，期望 A05/project=%d", got, pidA)
	}

	// 跨项目同号不串：p-a 域查不到 p-b 的 05。
	if _, err := s.GetColumnByCode("p-b", "05"); err != nil {
		t.Errorf("p-b 域 05 应命中（archived 不筛），错误 = %v", err)
	}
	if _, err := s.GetColumnByCode("p-a", "99"); !errors.Is(err, ErrColumnNotFound) {
		t.Errorf("未命中错误 = %v，期望 ErrColumnNotFound", err)
	}
	if _, err := s.GetColumnByCode("nope", "05"); !errors.Is(err, ErrColumnNotFound) {
		t.Errorf("项目不存在错误 = %v，期望归并 ErrColumnNotFound", err)
	}
}

// TestColumnCountsByProject 各项目栏目计数（端点 #4 columns_count）：一条
// GROUP BY 批量出全表（§3.7 禁逐项目循环查库），archived 栏目计入（计数=
// 栏目总量，与列表 status 过滤解耦）；无栏目项目不在映射；空库返回非 nil 空 map。
func TestColumnCountsByProject(t *testing.T) {
	s := openTemp(t)
	pidA := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	pidB := seedProject(t, s, "p-b", "项目B", ProjectStatusActive, 900, "2026-01-01T00:01:00Z")
	seedProject(t, s, "p-c", "空项目", ProjectStatusActive, 900, "2026-01-01T00:02:00Z")
	seedColumn(t, s, pidA, "01", "A01", ColumnStatusActive, "2026-01-01T00:00:00Z")
	seedColumn(t, s, pidA, "02", "A02", ColumnStatusArchived, "2026-01-01T00:00:00Z")
	seedColumn(t, s, pidB, "01", "B01", ColumnStatusActive, "2026-01-01T00:01:00Z")

	got, err := s.ColumnCountsByProject()
	if err != nil {
		t.Fatalf("ColumnCountsByProject 失败: %v", err)
	}
	if got == nil {
		t.Fatal("返回 nil，期望非 nil map")
	}
	if got[pidA] != 2 {
		t.Errorf("p-a 计数 = %d，期望 2（archived 计入）", got[pidA])
	}
	if got[pidB] != 1 {
		t.Errorf("p-b 计数 = %d，期望 1", got[pidB])
	}
	if _, ok := got[0]; ok {
		t.Error("空栏目项目不应出现在映射")
	}

	empty, err := Open(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("Open 空库失败: %v", err)
	}
	defer func() { _ = empty.Close() }()
	gotEmpty, err := empty.ColumnCountsByProject()
	if err != nil {
		t.Fatalf("空库 ColumnCountsByProject 失败: %v", err)
	}
	if gotEmpty == nil || len(gotEmpty) != 0 {
		t.Errorf("空库返回 = %v，期望非 nil 空 map", gotEmpty)
	}
}

// ---- UpdateColumnWithAudit / ArchiveColumnWithAudit（B1-5 审查修复，同 projects 型） ----

// TestUpdateColumnWithAudit 栏目更新+审计同事务：detailFn 拿事务内真实
// before/after、弱关联回填、栏目/项目不存在归并 404、action 空回滚 name 不变。
func TestUpdateColumnWithAudit(t *testing.T) {
	s := openTemp(t)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	seedColumn(t, s, pid, "05", "旧栏目", ColumnStatusActive, "2026-01-01T00:00:00Z")

	detailFn := func(before, after Column) string {
		return `{"b":"` + before.Name + `","a":"` + after.Name + `"}`
	}
	got, e, err := s.UpdateColumnWithAudit("p-a", "05", strPtr("新栏目"),
		AuditEntry{Action: AuditColumnUpdate, SessionID: 6}, detailFn)
	if err != nil {
		t.Fatalf("UpdateColumnWithAudit 失败: %v", err)
	}
	if got.Name != "新栏目" || got.ProjectID != pid {
		t.Errorf("更新后行 = %+v，期望 name=新栏目/project=%d", got, pid)
	}
	if e.ProjectID != pid || e.ColumnID != got.ID {
		t.Errorf("审计弱关联 = (%d,%d)，期望 (%d,%d)", e.ProjectID, e.ColumnID, pid, got.ID)
	}
	if e.Detail != `{"b":"旧栏目","a":"新栏目"}` {
		t.Errorf("审计 detail = %q，期望事务内真值拼串", e.Detail)
	}

	// 空更新 → ErrNoFields。
	if _, _, err := s.UpdateColumnWithAudit("p-a", "05", nil,
		AuditEntry{Action: AuditColumnUpdate}, detailFn); !errors.Is(err, ErrNoFields) {
		t.Errorf("空更新错误 = %v，期望 ErrNoFields", err)
	}
	// 栏目不存在/项目不存在 → 归并 ErrColumnNotFound，无审计。
	for _, tc := range [][2]string{{"p-a", "99"}, {"nope", "05"}} {
		if _, _, err := s.UpdateColumnWithAudit(tc[0], tc[1], strPtr("x"),
			AuditEntry{Action: AuditColumnUpdate}, detailFn); !errors.Is(err, ErrColumnNotFound) {
			t.Errorf("目标 %s/%s 错误 = %v，期望 ErrColumnNotFound", tc[0], tc[1], err)
		}
	}
	if n := countRows(t, s, "audit_log"); n != 1 {
		t.Errorf("audit_log 行数 = %d，期望 1（无效动作不留痕）", n)
	}

	// 原子性：action 空 → 回滚 name 不变。
	if _, _, err := s.UpdateColumnWithAudit("p-a", "05", strPtr("又改名"),
		AuditEntry{Action: " "}, detailFn); !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("action 空 → 错误 = %v，期望 ErrAuditInvalid", err)
	}
	if c, err := s.GetColumnByCode("p-a", "05"); err != nil || c.Name != "新栏目" {
		t.Errorf("回滚后 name = %v（err=%v），期望保持 新栏目", c, err)
	}
	if n := countRows(t, s, "audit_log"); n != 1 {
		t.Errorf("回滚后 audit_log 行数 = %d，期望仍 1", n)
	}
}

// TestArchiveColumnWithAudit 栏目归档+审计同事务：真实前态入快照、位点不受
// 归档影响（保留）、404 无审计、action 空回滚 status 不变。
func TestArchiveColumnWithAudit(t *testing.T) {
	s := openTemp(t)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	// 经 WithAudit 登记产生位点（归档后断言位点保留）。
	if _, _, err := s.CreateColumnWithAudit("p-a", Column{Code: "05", Name: "栏目05"},
		AuditEntry{Action: AuditColumnRegister, Detail: "{}"}); err != nil {
		t.Fatalf("登记夹具失败: %v", err)
	}

	detailFn := func(before Column) string {
		return `{"was":"` + before.Status + `"}`
	}
	e, err := s.ArchiveColumnWithAudit("p-a", "05",
		AuditEntry{Action: AuditColumnArchive, SessionID: 2}, detailFn)
	if err != nil {
		t.Fatalf("ArchiveColumnWithAudit 失败: %v", err)
	}
	if e.Detail != `{"was":"active"}` || e.ColumnID <= 0 {
		t.Errorf("审计 = %+v，期望 detail 记真实前态 active+弱关联回填", e)
	}
	if c, err := s.GetColumnByCode("p-a", "05"); err != nil || c.Status != ColumnStatusArchived {
		t.Fatalf("归档后 status = %v（err=%v），期望 archived", c, err)
	}
	if n := countRows(t, s, "ack_positions"); n != 2 {
		t.Errorf("归档后位点行数 = %d，期望 2（位点保留，AC2.3）", n)
	}

	// 目标不存在 → ErrColumnNotFound，无审计。
	if _, err := s.ArchiveColumnWithAudit("p-a", "99",
		AuditEntry{Action: AuditColumnArchive}, detailFn); !errors.Is(err, ErrColumnNotFound) {
		t.Errorf("目标不存在错误 = %v，期望 ErrColumnNotFound", err)
	}
	// 原子性：action 空 → 回滚 status 不变（先恢复一行 active）。
	seedColumn(t, s, pid, "06", "栏目06", ColumnStatusActive, "2026-01-01T00:01:00Z")
	if _, err := s.ArchiveColumnWithAudit("p-a", "06",
		AuditEntry{Action: " "}, detailFn); !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("action 空 → 错误 = %v，期望 ErrAuditInvalid", err)
	}
	if c, err := s.GetColumnByCode("p-a", "06"); err != nil || c.Status != ColumnStatusActive {
		t.Errorf("回滚后 06 status = %v（err=%v），期望保持 active", c, err)
	}
	if n := countRows(t, s, "audit_log"); n != 2 {
		t.Errorf("audit_log 行数 = %d，期望 2（register+archive 各一，回滚不计）", n)
	}
}
