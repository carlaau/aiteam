package store

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

// TestInWindow 表驱动覆盖时间窗判定（技术设计 §6.2 对拍表 + 端点边界）：
// HH:MM 等宽字符串字典序即时间序；start<=end 同日闭区间；start>end 跨午夜
// （start==end 语义=伪码字面单点命中，见 InWindow doc 注）。
func TestInWindow(t *testing.T) {
	tests := []struct {
		name  string
		now   string
		start string
		end   string
		want  bool
	}{
		// ---- §6.2 对拍表行 ----
		{name: "跨午夜_窗内白天", now: "15:00", start: "13:00", end: "01:00", want: true},
		{name: "跨午夜_窗外", now: "10:00", start: "15:00", end: "05:00", want: false},
		{name: "同日_窗外", now: "07:00", start: "09:00", end: "18:00", want: false},

		// ---- 跨午夜端点边界 ----
		{name: "跨午夜_start端点", now: "23:00", start: "23:00", end: "09:00", want: true},
		{name: "跨午夜_end端点", now: "09:00", start: "23:00", end: "09:00", want: true},
		{name: "跨午夜_后半夜零点", now: "00:00", start: "23:00", end: "09:00", want: true},

		// ---- 同日区间补强 + 端点边界 ----
		{name: "同日_窗内", now: "12:00", start: "09:00", end: "18:00", want: true},
		{name: "同日_start端点", now: "09:00", start: "09:00", end: "18:00", want: true},
		{name: "同日_end端点", now: "18:00", start: "09:00", end: "18:00", want: true},

		// ---- start==end 单点命中（伪码字面行为，语义见 InWindow doc 注）----
		{name: "首尾相等_命中该时刻", now: "09:00", start: "09:00", end: "09:00", want: true},
		{name: "首尾相等_其他时刻不命中", now: "10:00", start: "09:00", end: "09:00", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InWindow(tt.now, tt.start, tt.end); got != tt.want {
				t.Errorf("InWindow(%q, %q, %q) = %v，期望 %v", tt.now, tt.start, tt.end, got, tt.want)
			}
		})
	}
}

// ===== B4-4 续段：stage_windows 覆盖式写（真库 + 夹具，循 store_test.go 直插模式）=====

// winFixtureNow 时间窗域测试注入时钟（≠夹具 created_at，验证落库时间为注入值而非墙钟）。
const winFixtureNow = "2026-03-04T05:06:07Z"

// winFixtures 时间窗域夹具实体 id 集：一项目/一栏目/一会话。
type winFixtures struct {
	projID int64
	colID  int64
	sessID int64
}

// seedWinFixtures 直插时间窗域夹具行（projects/columns/sessions——FK 依赖，绕开上层 API）。
func seedWinFixtures(t *testing.T, s *Store) winFixtures {
	t.Helper()
	const ts = "2026-01-01T00:00:00Z"
	fx := winFixtures{}
	seed := func(query string, args ...any) int64 {
		t.Helper()
		res, err := s.DB.Exec(query, args...)
		if err != nil {
			t.Fatalf("插入夹具失败: %v\nSQL: %s", err, query)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("取夹具 LastInsertId 失败: %v", err)
		}
		return id
	}
	fx.projID = seed(`INSERT INTO projects (code, name, created_at, updated_at) VALUES ('proj-w', '窗项目', ?, ?)`, ts, ts)
	fx.colID = seed(`INSERT INTO columns (project_id, code, name, created_at, updated_at) VALUES (?, '05', 'W05', ?, ?)`, fx.projID, ts, ts)
	fx.sessID = seed(`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at) VALUES (?, ?, 'controller-w', 'controller', ?, ?)`, fx.projID, fx.colID, ts, ts)
	return fx
}

// mustSetWindows 提交成功路径便捷封装（注入固定时钟，失败即 Fatal）。
func mustSetWindows(t *testing.T, s *Store, projectID, columnID int64, windows []WindowInput, createdBy int64) {
	t.Helper()
	if err := s.SetWindows(projectID, columnID, windows, createdBy, winFixtureNow); err != nil {
		t.Fatalf("SetWindows 意外失败: %v", err)
	}
}

// winRowValues 库内时间窗行值断言视图。
type winRowValues struct {
	ColumnID  int64
	Stage     string
	StartTime string
	EndTime   string
	Enabled   int
	CreatedAt string
	UpdatedAt string
}

// queryWinRows 查指定 (project, column) 域全部时间窗行（ORDER BY stage=断言稳定序）。
func queryWinRows(t *testing.T, s *Store, projectID, columnID int64) []winRowValues {
	t.Helper()
	rows, err := s.DB.Query(
		`SELECT column_id, stage, start_time, end_time, enabled, created_at, updated_at
		 FROM stage_windows WHERE project_id = ? AND column_id = ? ORDER BY stage`, projectID, columnID)
	if err != nil {
		t.Fatalf("查询 stage_windows 失败: %v", err)
	}
	defer rows.Close()
	var out []winRowValues
	for rows.Next() {
		var r winRowValues
		if err := rows.Scan(&r.ColumnID, &r.Stage, &r.StartTime, &r.EndTime, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			t.Fatalf("扫描 stage_windows 失败: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 stage_windows 失败: %v", err)
	}
	return out
}

// winStages 提取行集的 stage 集合。
func winStages(rows []winRowValues) []string {
	stages := make([]string, 0, len(rows))
	for _, r := range rows {
		stages = append(stages, r.Stage)
	}
	return stages
}

// TestSetWindowsReplace 覆盖式写（§2.2 #21）：一次提交该域全部窗配置，缺席=删除；
// 幂等重放（同集再提交）行集一致；落库 created_at/updated_at=注入时钟。
func TestSetWindowsReplace(t *testing.T) {
	s := openTemp(t)
	fx := seedWinFixtures(t, s)

	mustSetWindows(t, s, fx.projID, fx.colID, []WindowInput{
		{Stage: "S1", From: "08:00", To: "09:00", Enabled: 1},
		{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1}, // start>end 跨午夜合法
		{Stage: "S5", From: "13:00", To: "14:00", Enabled: 1},
	}, fx.sessID)
	if rows := queryWinRows(t, s, fx.projID, fx.colID); len(rows) != 3 {
		t.Fatalf("首次提交后行数 = %d，期望 3", len(rows))
	}

	// 覆盖式：提交 2 窗（S4 留任改值、S6 新增）→ S1/S5 缺席已删。
	mustSetWindows(t, s, fx.projID, fx.colID, []WindowInput{
		{Stage: "S4", From: "20:00", To: "07:00", Enabled: 1},
		{Stage: "S6", From: "09:00", To: "18:00", Enabled: 1},
	}, fx.sessID)

	rows := queryWinRows(t, s, fx.projID, fx.colID)
	if !slices.Equal(winStages(rows), []string{"S4", "S6"}) {
		t.Fatalf("覆盖后 stage 集 = %v，期望 [S4 S6]（S1/S5 缺席即删）", winStages(rows))
	}
	for _, r := range rows {
		if r.CreatedAt != winFixtureNow || r.UpdatedAt != winFixtureNow {
			t.Errorf("stage=%s 落库时间 = (%q, %q)，期望注入时钟 %q", r.Stage, r.CreatedAt, r.UpdatedAt, winFixtureNow)
		}
	}
	if rows[0].StartTime != "20:00" || rows[0].EndTime != "07:00" {
		t.Errorf("覆盖后 S4 值 = %s-%s，期望 20:00-07:00（改值生效）", rows[0].StartTime, rows[0].EndTime)
	}

	// 幂等重放：同集再提交→行集一致（stage 集与值均不变，无残留无重复）。
	mustSetWindows(t, s, fx.projID, fx.colID, []WindowInput{
		{Stage: "S4", From: "20:00", To: "07:00", Enabled: 1},
		{Stage: "S6", From: "09:00", To: "18:00", Enabled: 1},
	}, fx.sessID)
	rows = queryWinRows(t, s, fx.projID, fx.colID)
	if !slices.Equal(winStages(rows), []string{"S4", "S6"}) {
		t.Fatalf("幂等重放后 stage 集 = %v，期望 [S4 S6]", winStages(rows))
	}
}

// TestSetWindowsEmptyClears 空集提交=清空该域全部窗（§2.2 #21 合法输入——PUT 空数组
// 即全删；SetWindows doc 声称行为的固化用例）：该域行数=0 且照常落 window.set 审计；
// 兄弟域（项目级）不受牵连。
func TestSetWindowsEmptyClears(t *testing.T) {
	s := openTemp(t)
	fx := seedWinFixtures(t, s)

	mustSetWindows(t, s, fx.projID, fx.colID, []WindowInput{
		{Stage: "S1", From: "08:00", To: "09:00", Enabled: 1},
		{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1},
	}, fx.sessID)
	mustSetWindows(t, s, fx.projID, 0, []WindowInput{{Stage: "S5", From: "13:00", To: "14:00", Enabled: 1}}, fx.sessID)

	// 空集提交：清空栏目级域。
	mustSetWindows(t, s, fx.projID, fx.colID, nil, fx.sessID)

	if rows := queryWinRows(t, s, fx.projID, fx.colID); len(rows) != 0 {
		t.Errorf("空集提交后栏目级行数 = %d，期望 0（域已清空）", len(rows))
	}
	// 项目级兄弟域不受牵连（DELETE 域隔离）。
	if rows := queryWinRows(t, s, fx.projID, 0); len(rows) != 1 || rows[0].Stage != "S5" {
		t.Errorf("空集提交不应影响项目级域，项目级行 = %+v", rows)
	}
	// 审计照常落行：2 次设置 + 1 次空集清空 = 3 条 window.set。
	if audits := auditLogRows(t, s); len(audits) != 3 || audits[2].Action != "window.set" {
		t.Errorf("空集提交应照常落 window.set 审计，audit_log = %d 行（期望 3）", len(audits))
	}
}

// TestProjectScope 项目级（column_id=0）与栏目级并存互不覆盖：UNIQUE(project,column,stage)
// 各域独立，覆盖式 DELETE 只清各自域。
func TestProjectScope(t *testing.T) {
	s := openTemp(t)
	fx := seedWinFixtures(t, s)

	mustSetWindows(t, s, fx.projID, 0, []WindowInput{{Stage: "S4", From: "22:00", To: "06:00", Enabled: 1}}, fx.sessID)
	mustSetWindows(t, s, fx.projID, fx.colID, []WindowInput{{Stage: "S4", From: "09:00", To: "18:00", Enabled: 1}}, fx.sessID)

	projRows := queryWinRows(t, s, fx.projID, 0)
	if len(projRows) != 1 || projRows[0].Stage != "S4" || projRows[0].StartTime != "22:00" {
		t.Fatalf("项目级行不符: %+v，期望 S4=22:00-06:00（column_id=0）", projRows)
	}
	colRows := queryWinRows(t, s, fx.projID, fx.colID)
	if len(colRows) != 1 || colRows[0].Stage != "S4" || colRows[0].StartTime != "09:00" {
		t.Fatalf("栏目级行不符: %+v，期望 S4=09:00-18:00", colRows)
	}

	// 再覆盖项目级（换成 S5）→栏目级 S4 不受影响（DELETE 域隔离）。
	mustSetWindows(t, s, fx.projID, 0, []WindowInput{{Stage: "S5", From: "10:00", To: "11:00", Enabled: 1}}, fx.sessID)
	projRows = queryWinRows(t, s, fx.projID, 0)
	if !slices.Equal(winStages(projRows), []string{"S5"}) {
		t.Errorf("项目级覆盖后 stage 集 = %v，期望 [S5]", winStages(projRows))
	}
	colRows = queryWinRows(t, s, fx.projID, fx.colID)
	if !slices.Equal(winStages(colRows), []string{"S4"}) {
		t.Errorf("项目级覆盖不应影响栏目级，栏目级 stage 集 = %v", winStages(colRows))
	}
}

// TestDuplicateStage 同一提交内重复 stage→预校验拒绝（ErrDuplicateStage），
// 且拒绝发生在 DELETE 之前——库内原数据未破坏（事务原子性）。
func TestDuplicateStage(t *testing.T) {
	s := openTemp(t)
	fx := seedWinFixtures(t, s)

	mustSetWindows(t, s, fx.projID, fx.colID, []WindowInput{{Stage: "S1", From: "08:00", To: "09:00", Enabled: 1}}, fx.sessID)

	err := s.SetWindows(fx.projID, fx.colID, []WindowInput{
		{Stage: "S4", From: "09:00", To: "10:00", Enabled: 1},
		{Stage: "S4", From: "11:00", To: "12:00", Enabled: 1},
	}, fx.sessID, winFixtureNow)
	if !errors.Is(err, ErrDuplicateStage) {
		t.Fatalf("重复 stage 应返回 ErrDuplicateStage，实际 %v", err)
	}

	rows := queryWinRows(t, s, fx.projID, fx.colID)
	if !slices.Equal(winStages(rows), []string{"S1"}) {
		t.Errorf("被拒提交不应破坏原数据，stage 集 = %v，期望 [S1]", winStages(rows))
	}
	// setup 的首次成功提交已落 1 条 window.set 审计；被拒提交不应再新增。
	if audits := auditLogRows(t, s); len(audits) != 1 || audits[0].Action != "window.set" {
		t.Errorf("被拒提交不应落审计行，audit_log = %d 行（期望保持 setup 的 1 条 window.set）", len(audits))
	}
}

// TestEnabledFlag enabled=0（停用）行正常写入并保留（存储面；停用语义归读侧聚合）。
func TestEnabledFlag(t *testing.T) {
	s := openTemp(t)
	fx := seedWinFixtures(t, s)

	mustSetWindows(t, s, fx.projID, fx.colID, []WindowInput{
		{Stage: "S2", From: "01:00", To: "02:00", Enabled: 0},
		{Stage: "S3", From: "03:00", To: "04:00", Enabled: 1},
	}, fx.sessID)

	rows := queryWinRows(t, s, fx.projID, fx.colID)
	if !slices.Equal(winStages(rows), []string{"S2", "S3"}) {
		t.Fatalf("stage 集 = %v，期望 [S2 S3]（enabled=0 行照常写入保留）", winStages(rows))
	}
	if rows[0].Enabled != 0 {
		t.Errorf("S2 enabled = %d，期望 0", rows[0].Enabled)
	}
	if rows[1].Enabled != 1 {
		t.Errorf("S3 enabled = %d，期望 1", rows[1].Enabled)
	}
}

// ===== B4-5 续段：#22 「当前允许阶段」聚合读侧 + NowInTz（测试先行）=====

// TestListWindowStatus §6.3 聚合（B4-5 #22 数据面）：未配置=allowed+null（AC3.4）/
// 窗内 allowed / 窗外 waiting（AC3.2）/ 栏目级覆盖项目级 / enabled=0 视同未配置 /
// 枚举外阶段并集防漏报（per-域并集不跨域串台）/ project 过滤与不存在项目空集 /
// 输出排序稳定（project code→column code→stage）。as_of 一律显式入参（时钟全注入）。
func TestListWindowStatus(t *testing.T) {
	// entryAt 取 (project, column, stage) 定位的条目（断言便捷视图；不存在即 Fatal）。
	entryAt := func(t *testing.T, entries []WindowStatusEntry, project, column, stage string) WindowStatusEntry {
		t.Helper()
		for _, e := range entries {
			if e.ProjectCode == project && e.ColumnCode == column && e.Stage == stage {
				return e
			}
		}
		t.Fatalf("entries 缺 (%s/%s/%s) 条目: %+v", project, column, stage, entries)
		return WindowStatusEntry{}
	}

	t.Run("未配置域_枚举全阶段allowed_windowNull_AC3.4", func(t *testing.T) {
		s := openTemp(t)
		seedWinFixtures(t, s) // proj-w/05 无任何窗配置

		entries, err := s.ListWindowStatus("12:00", "")
		if err != nil {
			t.Fatalf("ListWindowStatus 意外失败: %v", err)
		}
		if len(entries) != 8 { // S0..S7 全集
			t.Fatalf("entries 数 = %d，期望 8（S0..S7 枚举全集）", len(entries))
		}
		for _, e := range entries {
			if e.Status != "allowed" || e.Window != nil {
				t.Errorf("未配置阶段 %s 应 allowed+window=nil，实际 %+v", e.Stage, e)
			}
			if e.ProjectCode != "proj-w" || e.ColumnCode != "05" {
				t.Errorf("条目域不符: %+v", e)
			}
		}
	})

	t.Run("窗内allowed_窗外waiting_均带window_AC3.1_3.2", func(t *testing.T) {
		s := openTemp(t)
		fx := seedWinFixtures(t, s)
		mustSetWindows(t, s, fx.projID, fx.colID, []WindowInput{
			{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1}, // 跨午夜
		}, fx.sessID)

		// 窗内：23:30 ∈ [23:00,09:00]（跨午夜后半段）→ allowed + window 快照
		entries, err := s.ListWindowStatus("23:30", "")
		if err != nil {
			t.Fatalf("ListWindowStatus 意外失败: %v", err)
		}
		e := entryAt(t, entries, "proj-w", "05", "S4")
		if e.Status != "allowed" || e.Window == nil || e.Window.Start != "23:00" || e.Window.End != "09:00" {
			t.Errorf("窗内应 allowed+window{23:00,09:00}，实际 %+v", e)
		}

		// 窗外：12:00 ∉ 窗 → waiting（非报错，window 快照照带，AC3.2）
		entries, err = s.ListWindowStatus("12:00", "")
		if err != nil {
			t.Fatalf("ListWindowStatus 意外失败: %v", err)
		}
		e = entryAt(t, entries, "proj-w", "05", "S4")
		if e.Status != "waiting" || e.Window == nil || e.Window.Start != "23:00" || e.Window.End != "09:00" {
			t.Errorf("窗外应 waiting+window 快照，实际 %+v", e)
		}
	})

	t.Run("栏目级覆盖项目级", func(t *testing.T) {
		s := openTemp(t)
		fx := seedWinFixtures(t, s)
		mustSetWindows(t, s, fx.projID, 0, []WindowInput{{Stage: "S4", From: "09:00", To: "18:00", Enabled: 1}}, fx.sessID)
		mustSetWindows(t, s, fx.projID, fx.colID, []WindowInput{{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1}}, fx.sessID)

		// as_of=12:00：项目级窗(09:00-18:00)内，栏目级窗(23:00-09:00)外——
		// 期望 waiting 证明栏目级覆盖生效（若误用项目级会判 allowed）。
		entries, err := s.ListWindowStatus("12:00", "")
		if err != nil {
			t.Fatalf("ListWindowStatus 意外失败: %v", err)
		}
		e := entryAt(t, entries, "proj-w", "05", "S4")
		if e.Status != "waiting" || e.Window == nil || e.Window.Start != "23:00" {
			t.Errorf("栏目级应覆盖项目级（12:00 按栏目级窗判 waiting），实际 %+v", e)
		}
	})

	t.Run("enabled0行视同未配置_allowed_null", func(t *testing.T) {
		s := openTemp(t)
		fx := seedWinFixtures(t, s)
		mustSetWindows(t, s, fx.projID, fx.colID, []WindowInput{
			{Stage: "S5", From: "01:00", To: "02:00", Enabled: 0}, // 停用但 as_of 在窗内
		}, fx.sessID)

		entries, err := s.ListWindowStatus("01:30", "")
		if err != nil {
			t.Fatalf("ListWindowStatus 意外失败: %v", err)
		}
		e := entryAt(t, entries, "proj-w", "05", "S5")
		if e.Status != "allowed" || e.Window != nil {
			t.Errorf("enabled=0 应视同未配置 allowed+nil，实际 %+v", e)
		}
	})

	t.Run("枚举外阶段并集防漏报_且不跨域串台", func(t *testing.T) {
		s := openTemp(t)
		fx := seedWinFixtures(t, s)
		// 同项目第二个栏目（无 S9 配置域）。
		if _, err := s.DB.Exec(
			`INSERT INTO columns (project_id, code, name, created_at, updated_at) VALUES (?, '06', 'W06', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, fx.projID); err != nil {
			t.Fatalf("插入第二栏目失败: %v", err)
		}
		mustSetWindows(t, s, fx.projID, fx.colID, []WindowInput{
			{Stage: "S9", From: "08:00", To: "09:00", Enabled: 1}, // 枚举外（S0..S7 之外）
		}, fx.sessID)

		entries, err := s.ListWindowStatus("08:30", "")
		if err != nil {
			t.Fatalf("ListWindowStatus 意外失败: %v", err)
		}
		// 防漏报：配置了 S9 的域 05 出现 S9 且参与判定（08:30 窗内 → allowed）。
		e := entryAt(t, entries, "proj-w", "05", "S9")
		if e.Status != "allowed" || e.Window == nil {
			t.Errorf("枚举外配置阶段应入集并参与判定，实际 %+v", e)
		}
		// 不串台：未配置 S9 的域 06 阶段集恒为 S0..S7（per-域并集口径）。
		for _, x := range entries {
			if x.ColumnCode == "06" && x.Stage == "S9" {
				t.Errorf("S9 不应串台到未配置域: %+v", x)
			}
		}
		if got := len(entries); got != 17 { // 05: S0..S7+S9 共 9 条；06: S0..S7 共 8 条
			t.Errorf("entries 数 = %d，期望 17（05 含 S9 共 9 条 + 06 枚举 8 条）", got)
		}
	})

	t.Run("project过滤_省略全部_不存在项目空集", func(t *testing.T) {
		s := openTemp(t)
		seedWinFixtures(t, s)
		if _, err := s.DB.Exec(
			`INSERT INTO projects (code, name, created_at, updated_at) VALUES ('proj-x', 'X', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
			t.Fatalf("插入第二项目失败: %v", err)
		}
		if _, err := s.DB.Exec(
			`INSERT INTO columns (project_id, code, name, created_at, updated_at)
			 VALUES ((SELECT id FROM projects WHERE code='proj-x'), '05', 'X05', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
			t.Fatalf("插入 proj-x 栏目失败: %v", err)
		}

		all, err := s.ListWindowStatus("12:00", "")
		if err != nil {
			t.Fatalf("ListWindowStatus 意外失败: %v", err)
		}
		seen := map[string]bool{}
		for _, e := range all {
			seen[e.ProjectCode] = true
		}
		if !seen["proj-w"] || !seen["proj-x"] {
			t.Errorf("省略 project 应出全部项目，实际项目集 %v", seen)
		}

		only, err := s.ListWindowStatus("12:00", "proj-w")
		if err != nil {
			t.Fatalf("ListWindowStatus(project=proj-w) 意外失败: %v", err)
		}
		for _, e := range only {
			if e.ProjectCode != "proj-w" {
				t.Errorf("project=proj-w 过滤串入其他项目: %+v", e)
			}
		}

		none, err := s.ListWindowStatus("12:00", "no-such")
		if err != nil {
			t.Fatalf("ListWindowStatus(no-such) 意外失败: %v", err)
		}
		if len(none) != 0 {
			t.Errorf("不存在项目应空集（读口宽容，对齐 #20 口径），实际 %d 条", len(none))
		}
	})

	t.Run("输出排序稳定_项目_栏目_阶段", func(t *testing.T) {
		s := openTemp(t)
		seedWinFixtures(t, s)
		if _, err := s.DB.Exec(
			`INSERT INTO projects (code, name, created_at, updated_at) VALUES ('proj-x', 'X', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
			t.Fatalf("插入第二项目失败: %v", err)
		}
		if _, err := s.DB.Exec(
			`INSERT INTO columns (project_id, code, name, created_at, updated_at)
			 VALUES ((SELECT id FROM projects WHERE code='proj-x'), '05', 'X05', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
			t.Fatalf("插入 proj-x 栏目失败: %v", err)
		}

		entries, err := s.ListWindowStatus("12:00", "")
		if err != nil {
			t.Fatalf("ListWindowStatus 意外失败: %v", err)
		}
		type key struct{ p, c, st string }
		prev := key{"", "", ""}
		for i, e := range entries {
			cur := key{e.ProjectCode, e.ColumnCode, e.Stage}
			if i > 0 && !(prev.p < cur.p || (prev.p == cur.p && (prev.c < cur.c || (prev.c == cur.c && prev.st < cur.st)))) {
				t.Fatalf("entries 序紊乱: #%d %+v 在 %+v 之后", i, cur, prev)
			}
			prev = cur
		}
	})
}

// ===== B4-6b 续段：#21 修订行 GET 读侧（窗配置原始行，管理面全量）=====

// TestListWindows ListWindows 原始行读侧（B4-6b #21 修订行数据面）：含 enabled=0
// 停用行原样返回（管理面全量，不做 #22 判定态过滤）/两域隔离（项目级 columnID=0
// 与栏目级互不可见）/空域非 nil 空切片/stage 升序稳定排序（ORDER BY stage 全序
// 确定——域内 stage UNIQUE）。
func TestListWindows(t *testing.T) {
	s := openTemp(t)
	fx := seedWinFixtures(t, s)

	mustSetWindows(t, s, fx.projID, fx.colID, []WindowInput{
		{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1}, // 跨午夜
		{Stage: "S1", From: "08:00", To: "09:00", Enabled: 0}, // 停用行
		{Stage: "S3", From: "09:00", To: "18:00", Enabled: 1},
	}, fx.sessID)
	mustSetWindows(t, s, fx.projID, 0, []WindowInput{
		{Stage: "S2", From: "10:00", To: "12:00", Enabled: 1},
	}, fx.sessID)

	// 栏目级：stage 升序 [S1 S3 S4]，停用行原样返回（enabled=0 不过滤不改写）。
	colWins, err := s.ListWindows(fx.projID, fx.colID)
	if err != nil {
		t.Fatalf("ListWindows(栏目级) 意外失败: %v", err)
	}
	if colWins == nil {
		t.Fatalf("ListWindows(栏目级) 返回 nil，期望非 nil 切片")
	}
	gotStages := make([]string, 0, len(colWins))
	for _, w := range colWins {
		gotStages = append(gotStages, w.Stage)
	}
	if !slices.Equal(gotStages, []string{"S1", "S3", "S4"}) {
		t.Fatalf("栏目级 stage 序 = %v，期望 [S1 S3 S4]（升序稳定；项目级 S2 不串入）", gotStages)
	}
	if colWins[0].Enabled != 0 {
		t.Errorf("S1 enabled = %d，期望 0（停用行原样返回）", colWins[0].Enabled)
	}
	if colWins[0].From != "08:00" || colWins[0].To != "09:00" {
		t.Errorf("S1 窗值 = %s-%s，期望 08:00-09:00", colWins[0].From, colWins[0].To)
	}
	if colWins[2].From != "23:00" || colWins[2].To != "09:00" {
		t.Errorf("S4 窗值 = %s-%s，期望 23:00-09:00（跨午夜原样）", colWins[2].From, colWins[2].To)
	}

	// 项目级（columnID=0）：只见 S2（两域隔离反向——栏目级行不串入）。
	projWins, err := s.ListWindows(fx.projID, 0)
	if err != nil {
		t.Fatalf("ListWindows(项目级) 意外失败: %v", err)
	}
	gotStages = gotStages[:0]
	for _, w := range projWins {
		gotStages = append(gotStages, w.Stage)
	}
	if !slices.Equal(gotStages, []string{"S2"}) {
		t.Errorf("项目级 stage 集 = %v，期望 [S2]（栏目级行不串入）", gotStages)
	}

	// 空域：非 nil 空切片（序列化为 [] 非 null 的存储面保证）。
	empty, err := s.ListWindows(fx.projID, 99999) // 不存在的栏目域，读口纯 SELECT 无 FK 面
	if err != nil {
		t.Fatalf("ListWindows(空域) 意外失败: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("空域应返回非 nil 空切片，实际 len=%d nil=%v", len(empty), empty == nil)
	}
}

// TestNowInTz NowInTz 时区参数化取钟（§9.4 D4）：Local 与固定 IANA 名输出 "HH:MM"
// 等宽形态；非法时区返回 error（handler 决策兜底，本函数不静默回退）。
// Asia/Shanghai 依赖 time/tzdata 兜底（Windows 无系统 tzdata 亦可解析）。
func TestNowInTz(t *testing.T) {
	isHHMM := func(s string) bool {
		if len(s) != 5 || s[2] != ':' {
			return false
		}
		for _, i := range []int{0, 1, 3, 4} {
			if s[i] < '0' || s[i] > '9' {
				return false
			}
		}
		return true
	}

	for _, tz := range []string{"Local", "UTC", "Asia/Shanghai"} {
		hhmm, err := NowInTz(tz)
		if err != nil {
			t.Fatalf("NowInTz(%q) 意外失败: %v", tz, err)
		}
		if !isHHMM(hhmm) {
			t.Errorf("NowInTz(%q) = %q，不符合 HH:MM 两位等宽形态", tz, hhmm)
		}
	}

	// UTC 固定 IANA：与标准库直算对齐（容忍秒边界跨分钟翻转 ±1 分钟）。
	hhmm, err := NowInTz("UTC")
	if err != nil {
		t.Fatalf("NowInTz(UTC) 失败: %v", err)
	}
	want := time.Now().UTC().Format("15:04")
	toMin := func(s string) int { // "HH:MM" → 分钟数（形态已由上方断言保证）
		return int(s[0]-'0')*600 + int(s[1]-'0')*60 + int(s[3]-'0')*10 + int(s[4]-'0')
	}
	if d := toMin(hhmm) - toMin(want); d > 1 || d < -1 {
		t.Errorf("NowInTz(UTC) = %q，与标准库直算 %q 偏差超 1 分钟", hhmm, want)
	}

	// 非法时区：返回 error 不静默兜底。
	if hhmm, err := NowInTz("No/SuchZone"); err == nil {
		t.Errorf("NowInTz(No/SuchZone) 应返回 error，实际 (%q, nil)", hhmm)
	}
}

// TestSetWindowsAudit window.set 审计落行：action/操作会话身份/注入时钟/detail=提交快照 JSON 齐。
func TestSetWindowsAudit(t *testing.T) {
	s := openTemp(t)
	fx := seedWinFixtures(t, s)

	mustSetWindows(t, s, fx.projID, fx.colID, []WindowInput{
		{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1},
	}, fx.sessID)

	rows := auditLogRows(t, s)
	if len(rows) != 1 {
		t.Fatalf("audit_log 行数 = %d，期望 1（window.set）", len(rows))
	}
	r := rows[0]
	if r.Action != "window.set" {
		t.Errorf("action = %q，期望 window.set", r.Action)
	}
	if r.ProjectID != fx.projID || r.ColumnID != fx.colID {
		t.Errorf("审计域 = (%d, %d)，期望 (%d, %d)", r.ProjectID, r.ColumnID, fx.projID, fx.colID)
	}
	if r.SessionID != fx.sessID {
		t.Errorf("审计身份 = %d，期望会话 id %d", r.SessionID, fx.sessID)
	}
	if r.CreatedAt != winFixtureNow {
		t.Errorf("created_at = %q，期望注入时钟 %q", r.CreatedAt, winFixtureNow)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(r.Detail), &m); err != nil {
		t.Fatalf("detail 非合法 JSON: %v\ndetail=%s", err, r.Detail)
	}
	wins, ok := m["windows"].([]any)
	if !ok || len(wins) != 1 {
		t.Fatalf("detail.windows 应含 1 窗快照: %s", r.Detail)
	}
	w0, _ := wins[0].(map[string]any)
	if w0["stage"] != "S4" || w0["from"] != "23:00" || w0["to"] != "09:00" || w0["enabled"] != float64(1) {
		t.Errorf("window.set detail 快照不符: %s", r.Detail)
	}
}
