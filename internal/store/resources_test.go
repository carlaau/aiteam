package store

import (
	"encoding/json"
	"errors"
	"math"
	"slices"
	"testing"
)

// TestParseResourceValue 表驱动覆盖资源规范串 value 解析（B4-1 先行切片）：
// port=纯整数；account_range/data_range=`prefix:start-end`（前缀可省）。
// 边界裁定（B4-T1）：段值非负整数且 start<=end；0 合法；port 不校验 1~65535 上限；
// 超 int64 上限防回绕（parseNonNegativeInt 实现行为，此处固化）。
func TestParseResourceValue(t *testing.T) {
	tests := []struct {
		name        string
		rtype       string
		value       string
		wantPrefix  string
		wantStart   int64
		wantEnd     int64
		wantInvalid bool // 期望返回 ErrValueInvalid
	}{
		// ---- port：纯整数 ----
		{name: "port_纯整数", rtype: "port", value: "8080", wantPrefix: "", wantStart: 8080, wantEnd: 8080},
		{name: "port_零合法", rtype: "port", value: "0", wantPrefix: "", wantStart: 0, wantEnd: 0},
		{name: "port_负数非法", rtype: "port", value: "-1", wantInvalid: true},
		{name: "port_非数字非法", rtype: "port", value: "abc", wantInvalid: true},
		{name: "port_段形态非法", rtype: "port", value: "1000-1999", wantInvalid: true},
		{name: "port_带前缀非法", rtype: "port", value: "acct:1000-1999", wantInvalid: true},
		{name: "port_超int64上限非法", rtype: "port", value: "9223372036854775808", wantInvalid: true}, // 2^63

		// ---- range：prefix:start-end，前缀可省 ----
		{name: "range_带前缀", rtype: "account_range", value: "acct:1000-1999", wantPrefix: "acct", wantStart: 1000, wantEnd: 1999},
		{name: "range_省略前缀", rtype: "account_range", value: "1000-1999", wantPrefix: "", wantStart: 1000, wantEnd: 1999},
		{name: "data_range_带前缀", rtype: "data_range", value: "acct:1000-1999", wantPrefix: "acct", wantStart: 1000, wantEnd: 1999},
		{name: "range_零段合法", rtype: "account_range", value: "0-0", wantPrefix: "", wantStart: 0, wantEnd: 0},
		{name: "range_start等于end合法", rtype: "account_range", value: "1000-1000", wantPrefix: "", wantStart: 1000, wantEnd: 1000},
		{name: "range_end为MaxInt64合法", rtype: "account_range", value: "0-9223372036854775807", wantPrefix: "", wantStart: 0, wantEnd: math.MaxInt64},
		{name: "range_前缀空格原样保留", rtype: "account_range", value: "a b:1-2", wantPrefix: "a b", wantStart: 1, wantEnd: 2},

		// ---- range：非法形态 ----
		{name: "range_start大于end非法", rtype: "account_range", value: "1999-1000", wantInvalid: true},
		{name: "range_负数段非法", rtype: "account_range", value: "100--1", wantInvalid: true},
		{name: "range_缺end非法", rtype: "account_range", value: "1000-", wantInvalid: true},
		{name: "range_缺start非法", rtype: "account_range", value: "-1999", wantInvalid: true},
		{name: "range_空串非法", rtype: "account_range", value: "", wantInvalid: true},
		{name: "range_缺段非法", rtype: "account_range", value: "1000", wantInvalid: true},
		{name: "range_非数字非法", rtype: "account_range", value: "abc", wantInvalid: true},
		{name: "range_带前缀start大于end非法", rtype: "data_range", value: "x:5-3", wantInvalid: true},

		// ---- 未知 rtype：固定走哨兵错误 ----
		{name: "未知rtype非法", rtype: "bucket", value: "8080", wantInvalid: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix, start, end, err := ParseResourceValue(tt.rtype, tt.value)
			if tt.wantInvalid {
				if !errors.Is(err, ErrValueInvalid) {
					t.Fatalf("ParseResourceValue(%q, %q) err = %v，期望 ErrValueInvalid", tt.rtype, tt.value, err)
				}
				// 非法路径三返回值固定零值（handler 据此短路，无脏残留）。
				if prefix != "" || start != 0 || end != 0 {
					t.Errorf("ParseResourceValue(%q, %q) 非法路径返回 (%q, %d, %d)，期望零值 (%q, %d, %d)",
						tt.rtype, tt.value, prefix, start, end, "", 0, 0)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseResourceValue(%q, %q) 意外报错: %v", tt.rtype, tt.value, err)
			}
			if prefix != tt.wantPrefix || start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("ParseResourceValue(%q, %q) = (%q, %d, %d)，期望 (%q, %d, %d)",
					tt.rtype, tt.value, prefix, start, end, tt.wantPrefix, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

// ===== B4-1 续段：resources CRUD + 冲突判定（真库 + 夹具，循 store_test.go 直插模式）=====

// resFixtureNow 资源域测试注入时钟（≠夹具 created_at，验证落库时间为注入值而非墙钟）。
const resFixtureNow = "2026-01-02T03:04:05Z"

// resFixtures 资源域夹具实体 id 集：三项目（含一归档）/四栏目/三会话。
type resFixtures struct {
	projA     int64 // proj-a（active）
	projArch  int64 // proj-arch（archived，B4-T3 archived 域冲突用例）
	projB     int64 // proj-b（active，D3 跨项目用例）
	colA05    int64
	colA06    int64
	colArch05 int64
	colB05    int64
	sessA     int64
	sessArch  int64
	sessB     int64
}

// seedResFixtures 直插资源域夹具行（projects/columns/sessions——resources 三 FK 依赖，
// 绕开上层 API 直插保证 store 层测试零依赖）。
func seedResFixtures(t *testing.T, s *Store) resFixtures {
	t.Helper()
	insert := func(query string, args ...any) int64 {
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
	const ts = "2026-01-01T00:00:00Z"
	fx := resFixtures{}
	fx.projA = insert(`INSERT INTO projects (code, name, status, created_at, updated_at) VALUES ('proj-a', '项目A', 'active', ?, ?)`, ts, ts)
	fx.projArch = insert(`INSERT INTO projects (code, name, status, created_at, updated_at) VALUES ('proj-arch', '归档项目', 'archived', ?, ?)`, ts, ts)
	fx.projB = insert(`INSERT INTO projects (code, name, status, created_at, updated_at) VALUES ('proj-b', '项目B', 'active', ?, ?)`, ts, ts)
	fx.colA05 = insert(`INSERT INTO columns (project_id, code, name, created_at, updated_at) VALUES (?, '05', 'A05', ?, ?)`, fx.projA, ts, ts)
	fx.colA06 = insert(`INSERT INTO columns (project_id, code, name, created_at, updated_at) VALUES (?, '06', 'A06', ?, ?)`, fx.projA, ts, ts)
	fx.colArch05 = insert(`INSERT INTO columns (project_id, code, name, created_at, updated_at) VALUES (?, '05', 'Arch05', ?, ?)`, fx.projArch, ts, ts)
	fx.colB05 = insert(`INSERT INTO columns (project_id, code, name, created_at, updated_at) VALUES (?, '05', 'B05', ?, ?)`, fx.projB, ts, ts)
	fx.sessA = insert(`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at) VALUES (?, ?, 'controller-a', 'controller', ?, ?)`, fx.projA, fx.colA05, ts, ts)
	fx.sessArch = insert(`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at) VALUES (?, ?, 'controller-arch', 'controller', ?, ?)`, fx.projArch, fx.colArch05, ts, ts)
	fx.sessB = insert(`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at) VALUES (?, ?, 'controller-b', 'controller', ?, ?)`, fx.projB, fx.colB05, ts, ts)
	return fx
}

// mustRegister 登记成功路径便捷封装（注入固定时钟，失败即 Fatal）。
func mustRegister(t *testing.T, s *Store, projectID, columnID int64, rtype, value, note string, createdBy int64) int64 {
	t.Helper()
	id, err := s.RegisterResource(projectID, columnID, rtype, value, note, createdBy, resFixtureNow)
	if err != nil {
		t.Fatalf("RegisterResource(%s=%s) 意外失败: %v", rtype, value, err)
	}
	if id <= 0 {
		t.Fatalf("RegisterResource(%s=%s) 返回非法 id=%d", rtype, value, id)
	}
	return id
}

// mustRelease 释放成功路径便捷封装（注入固定时钟；参数序与 ReleaseResource 的
// (id, createdBy) 对齐——两组 int64 换位编译器不报，封装序必须与实现一致防误传）。
func mustRelease(t *testing.T, s *Store, id, createdBy int64) {
	t.Helper()
	if err := s.ReleaseResource(id, createdBy, resFixtureNow); err != nil {
		t.Fatalf("ReleaseResource(id=%d) 意外失败: %v", id, err)
	}
}

// resourceRowValues 库内资源行值断言视图（含冗余列——Register 持久化断言用）。
type resourceRowValues struct {
	RangePrefix string
	RangeStart  int64
	RangeEnd    int64
	Status      string
	CreatedBy   int64
	CreatedAt   string
	ReleasedAt  any // nil=未释放
}

// queryResourceByValue 按 value 查库内资源行（每 value 至多一行是测试场景约定）。
func queryResourceByValue(t *testing.T, s *Store, value string) resourceRowValues {
	t.Helper()
	var got resourceRowValues
	err := s.DB.QueryRow(
		`SELECT range_prefix, range_start, range_end, status, created_by, created_at, released_at
		 FROM resources WHERE value = ?`, value,
	).Scan(&got.RangePrefix, &got.RangeStart, &got.RangeEnd, &got.Status, &got.CreatedBy, &got.CreatedAt, &got.ReleasedAt)
	if err != nil {
		t.Fatalf("查询资源行 value=%s 失败: %v", value, err)
	}
	return got
}

// listValues 提取 ListResources 结果的 value 集合。
func listValues(rows []Resource) []string {
	vals := make([]string, 0, len(rows))
	for _, r := range rows {
		vals = append(vals, r.Value)
	}
	return vals
}

// assertValuesEqual 无序比较两组 value（长度+逐个包含）。
func assertValuesEqual(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s 行数 = %d（%v），期望 %d（%v）", what, len(got), got, len(want), want)
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Fatalf("%s 缺少 %q（实际 %v）", what, w, got)
		}
	}
}

// TestRegisterAndConflict 登记冲突判定矩阵（AC4.2 / D3 / B4-T3）：
// 冲突谓词=§3.6 SQL 原样（port 同 rtype 等值；range 同 rtype+同前缀+闭区间重叠端点相等=重叠），
// 判定不含 project 过滤（D3 全局拒）、不含 archived 过滤（B4-T3 archived 域 in_use 仍拒）。
// 每子测试独立子库（openTemp+seed），避免矩阵行互相污染。
func TestRegisterAndConflict(t *testing.T) {
	tests := []struct {
		name         string
		setup        func(t *testing.T, s *Store, fx resFixtures) // 预置在用/已释放资源
		probeProj    func(resFixtures) int64                      // 探针登记目标（夹具 id 每子测试重建，经选择器引用）
		probeCol     func(resFixtures) int64
		probeRtype   string
		probeValue   string
		wantConflict bool
		conflictVal  string // 冲突对象=在用冲突行的 value（=setup 登记值；非探针值）
	}{
		{
			name: "同port冲突",
			setup: func(t *testing.T, s *Store, fx resFixtures) {
				mustRegister(t, s, fx.projA, fx.colA05, rtypePort, "8080", "", fx.sessA)
			},
			probeProj:  func(fx resFixtures) int64 { return fx.projA },
			probeCol:   func(fx resFixtures) int64 { return fx.colA05 },
			probeRtype: rtypePort, probeValue: "8080", wantConflict: true, conflictVal: "8080",
		},
		{
			name: "不同type同数值不冲突",
			setup: func(t *testing.T, s *Store, fx resFixtures) {
				mustRegister(t, s, fx.projA, fx.colA05, rtypePort, "8080", "", fx.sessA)
			},
			probeProj:  func(fx resFixtures) int64 { return fx.projA },
			probeCol:   func(fx resFixtures) int64 { return fx.colA05 },
			probeRtype: rtypeAccountRange, probeValue: "8080-8080", wantConflict: false,
		},
		{
			name: "同前缀区间重叠_端点相等即冲突",
			setup: func(t *testing.T, s *Store, fx resFixtures) {
				mustRegister(t, s, fx.projA, fx.colA05, rtypeAccountRange, "acct:1000-1999", "", fx.sessA)
			},
			probeProj:  func(fx resFixtures) int64 { return fx.projA },
			probeCol:   func(fx resFixtures) int64 { return fx.colA05 },
			probeRtype: rtypeAccountRange, probeValue: "acct:1999-2500", wantConflict: true, conflictVal: "acct:1000-1999",
		},
		{
			name: "同前缀区间重叠_包含即冲突",
			setup: func(t *testing.T, s *Store, fx resFixtures) {
				mustRegister(t, s, fx.projA, fx.colA05, rtypeDataRange, "data:1000-1999", "", fx.sessA)
			},
			probeProj:  func(fx resFixtures) int64 { return fx.projA },
			probeCol:   func(fx resFixtures) int64 { return fx.colA05 },
			probeRtype: rtypeDataRange, probeValue: "data:1200-1300", wantConflict: true, conflictVal: "data:1000-1999",
		},
		{
			name: "相邻不重叠",
			setup: func(t *testing.T, s *Store, fx resFixtures) {
				mustRegister(t, s, fx.projA, fx.colA05, rtypeAccountRange, "acct:1000-1999", "", fx.sessA)
			},
			probeProj:  func(fx resFixtures) int64 { return fx.projA },
			probeCol:   func(fx resFixtures) int64 { return fx.colA05 },
			probeRtype: rtypeAccountRange, probeValue: "acct:2000-2999", wantConflict: false,
		},
		{
			name: "不同前缀不重叠",
			setup: func(t *testing.T, s *Store, fx resFixtures) {
				mustRegister(t, s, fx.projA, fx.colA05, rtypeAccountRange, "acct:1000-1999", "", fx.sessA)
			},
			probeProj:  func(fx resFixtures) int64 { return fx.projA },
			probeCol:   func(fx resFixtures) int64 { return fx.colA05 },
			probeRtype: rtypeAccountRange, probeValue: "bank:1500-1600", wantConflict: false,
		},
		{
			name: "released行不冲突_同值可再登记",
			setup: func(t *testing.T, s *Store, fx resFixtures) {
				id := mustRegister(t, s, fx.projA, fx.colA05, rtypeAccountRange, "acct:1000-1999", "", fx.sessA)
				mustRelease(t, s, id, fx.sessA)
			},
			probeProj:  func(fx resFixtures) int64 { return fx.projA },
			probeCol:   func(fx resFixtures) int64 { return fx.colA05 },
			probeRtype: rtypeAccountRange, probeValue: "acct:1500-1500", wantConflict: false,
		},
		{
			name: "跨项目同值冲突_D3全局拒",
			setup: func(t *testing.T, s *Store, fx resFixtures) {
				mustRegister(t, s, fx.projA, fx.colA05, rtypePort, "8080", "", fx.sessA)
			},
			probeProj:  func(fx resFixtures) int64 { return fx.projB },
			probeCol:   func(fx resFixtures) int64 { return fx.colB05 },
			probeRtype: rtypePort, probeValue: "8080", wantConflict: true, conflictVal: "8080",
		},
		{
			name: "同项目不同栏目冲突",
			setup: func(t *testing.T, s *Store, fx resFixtures) {
				mustRegister(t, s, fx.projA, fx.colA05, rtypePort, "8080", "", fx.sessA)
			},
			probeProj:  func(fx resFixtures) int64 { return fx.projA },
			probeCol:   func(fx resFixtures) int64 { return fx.colA06 },
			probeRtype: rtypePort, probeValue: "8080", wantConflict: true, conflictVal: "8080",
		},
		{
			name: "archived域in_use资源仍冲突_B4-T3",
			setup: func(t *testing.T, s *Store, fx resFixtures) {
				mustRegister(t, s, fx.projArch, fx.colArch05, rtypePort, "9090", "", fx.sessArch)
			},
			probeProj:  func(fx resFixtures) int64 { return fx.projA },
			probeCol:   func(fx resFixtures) int64 { return fx.colA05 },
			probeRtype: rtypePort, probeValue: "9090", wantConflict: true, conflictVal: "9090",
		},
		{
			name:       "无前置资源_登记成功",
			setup:      func(t *testing.T, s *Store, fx resFixtures) {},
			probeProj:  func(fx resFixtures) int64 { return fx.projA },
			probeCol:   func(fx resFixtures) int64 { return fx.colA05 },
			probeRtype: rtypeAccountRange, probeValue: "acct:1000-1999", wantConflict: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTemp(t)
			fx := seedResFixtures(t, s)
			tt.setup(t, s, fx)

			id, err := s.RegisterResource(tt.probeProj(fx), tt.probeCol(fx), tt.probeRtype, tt.probeValue, "探针", fx.sessA, resFixtureNow)
			if !tt.wantConflict {
				if err != nil {
					t.Fatalf("RegisterResource(%s=%s) 应成功，实际报错: %v", tt.probeRtype, tt.probeValue, err)
				}
				if id <= 0 {
					t.Fatalf("成功登记应返回正 id，实际 %d", id)
				}
				return
			}
			if !errors.Is(err, ErrResourceConflict) {
				t.Fatalf("RegisterResource(%s=%s) 应返回 ErrResourceConflict，实际 err=%v id=%d", tt.probeRtype, tt.probeValue, err, id)
			}
			// 冲突对象字段齐（供 handler 组 409 message「与 proj-a/05 在用的 resource#3 冲突」型）。
			var ce *ResourceConflictError
			if !errors.As(err, &ce) {
				t.Fatalf("冲突错误应可 errors.As 取 *ResourceConflictError，实际 %T", err)
			}
			if ce.Conflict.ID <= 0 {
				t.Errorf("冲突对象 ID = %d，期望冲突行正 id", ce.Conflict.ID)
			}
			if ce.Conflict.Value != tt.conflictVal {
				t.Errorf("冲突对象 Value = %q，期望在用冲突行值 %q", ce.Conflict.Value, tt.conflictVal)
			}
			if ce.Conflict.ProjectCode == "" || ce.Conflict.ColumnCode == "" {
				t.Errorf("冲突对象 ProjectCode/ColumnCode = %q/%q，期望非空（供 message 组装）", ce.Conflict.ProjectCode, ce.Conflict.ColumnCode)
			}
			// 冲突路径不落行：库内 resources 行数=setup 登记数（此处均 ≤1）。
			var n int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM resources`).Scan(&n); err != nil {
				t.Fatalf("统计 resources 失败: %v", err)
			}
			if n > 1 {
				t.Errorf("冲突路径不应落行，resources 行数 = %d", n)
			}
		})
	}
}

// TestRegisterInvalidValue 非法 value 直接拒绝（ParseResourceValue 哨兵透传），不落任何行。
func TestRegisterInvalidValue(t *testing.T) {
	s := openTemp(t)
	fx := seedResFixtures(t, s)

	id, err := s.RegisterResource(fx.projA, fx.colA05, rtypePort, "abc", "", fx.sessA, resFixtureNow)
	if !errors.Is(err, ErrValueInvalid) {
		t.Fatalf("非法 value 应返回 ErrValueInvalid，实际 err=%v id=%d", err, id)
	}
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM resources`).Scan(&n); err != nil {
		t.Fatalf("统计 resources 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("非法 value 不应落行，resources 行数 = %d", n)
	}
	var audits int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&audits); err != nil {
		t.Fatalf("统计 audit_log 失败: %v", err)
	}
	if audits != 0 {
		t.Errorf("非法 value 不应落审计，audit_log 行数 = %d", audits)
	}
}

// TestRegisterPersistsSnapshot 成功登记持久化快照：解析冗余列=解析结果、status=in_use、
// created_by/created_at=注入值（非墙钟）。
func TestRegisterPersistsSnapshot(t *testing.T) {
	s := openTemp(t)
	fx := seedResFixtures(t, s)

	mustRegister(t, s, fx.projA, fx.colA05, rtypeAccountRange, "acct:1000-1999", "网关段", fx.sessA)

	got := queryResourceByValue(t, s, "acct:1000-1999")
	if got.RangePrefix != "acct" || got.RangeStart != 1000 || got.RangeEnd != 1999 {
		t.Errorf("冗余列 = (%q, %d, %d)，期望解析结果 (acct, 1000, 1999)", got.RangePrefix, got.RangeStart, got.RangeEnd)
	}
	if got.Status != "in_use" {
		t.Errorf("status = %q，期望 in_use", got.Status)
	}
	if got.CreatedBy != fx.sessA {
		t.Errorf("created_by = %d，期望会话 id %d", got.CreatedBy, fx.sessA)
	}
	if got.CreatedAt != resFixtureNow {
		t.Errorf("created_at = %q，期望注入时钟 %q", got.CreatedAt, resFixtureNow)
	}
	if got.ReleasedAt != nil {
		t.Errorf("released_at = %v，期望 nil（未释放）", got.ReleasedAt)
	}
}

// TestRelease 释放语义（AC4.3 数据面）：
// status→released + released_at=注入时钟；幂等口径=重复 release 成功无副作用
// （released_at 保持首次值、audit 不重复落行）；id 不存在→ErrResourceNotFound。
func TestRelease(t *testing.T) {
	s := openTemp(t)
	fx := seedResFixtures(t, s)
	id := mustRegister(t, s, fx.projA, fx.colA05, rtypePort, "7777", "", fx.sessA)

	mustRelease(t, s, id, fx.sessA)

	got := queryResourceByValue(t, s, "7777")
	if got.Status != "released" {
		t.Errorf("status = %q，期望 released", got.Status)
	}
	if got.ReleasedAt != resFixtureNow {
		t.Errorf("released_at = %v，期望注入时钟 %q", got.ReleasedAt, resFixtureNow)
	}

	// 重复 release：幂等成功，released_at 不被二次覆盖（若被覆盖会变成第二次调用时刻——
	// 此处两次注入时钟相同，改用不同时钟值验证「首次值保留」）。
	if err := s.ReleaseResource(id, fx.sessA, "2099-12-31T23:59:59Z"); err != nil {
		t.Fatalf("重复 release 应幂等成功，实际报错: %v", err)
	}
	got = queryResourceByValue(t, s, "7777")
	if got.ReleasedAt != resFixtureNow {
		t.Errorf("重复 release 后 released_at = %v，期望保留首次值 %q（幂等不覆盖）", got.ReleasedAt, resFixtureNow)
	}

	// 不存在 id。
	if err := s.ReleaseResource(999, fx.sessA, resFixtureNow); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("release 不存在 id 应返回 ErrResourceNotFound，实际 %v", err)
	}

	// 释放后同值可再登记（AC4.3）。
	mustRegister(t, s, fx.projA, fx.colA06, rtypePort, "7777", "", fx.sessA)
}

// TestListResources 三过滤一条 WHERE（AC4.1 数据面）：project/rtype/include_released 组合，
// 默认排除 released。
func TestListResources(t *testing.T) {
	s := openTemp(t)
	fx := seedResFixtures(t, s)

	id := mustRegister(t, s, fx.projA, fx.colA05, rtypePort, "8080", "", fx.sessA) // 之后释放
	mustRegister(t, s, fx.projA, fx.colA05, rtypeAccountRange, "acct:1000-1999", "", fx.sessA)
	mustRegister(t, s, fx.projB, fx.colB05, rtypePort, "9090", "", fx.sessB)
	mustRelease(t, s, id, fx.sessA)

	// 默认：排除 released → acct + proj-b 9090。
	got, err := s.ListResources(0, "", false)
	if err != nil {
		t.Fatalf("ListResources(默认) 失败: %v", err)
	}
	assertValuesEqual(t, "默认列表", listValues(got), "acct:1000-1999", "9090")
	for _, r := range got {
		if r.Status != "in_use" {
			t.Errorf("默认列表不应含 released 行，value=%s status=%s", r.Value, r.Status)
		}
	}

	// include_released=true：全量 3 行。
	got, err = s.ListResources(0, "", true)
	if err != nil {
		t.Fatalf("ListResources(include_released) 失败: %v", err)
	}
	assertValuesEqual(t, "全量列表", listValues(got), "8080", "acct:1000-1999", "9090")

	// project 过滤：proj-a 含 released 共 2 行。
	got, err = s.ListResources(fx.projA, "", true)
	if err != nil {
		t.Fatalf("ListResources(project) 失败: %v", err)
	}
	assertValuesEqual(t, "proj-a 全量", listValues(got), "8080", "acct:1000-1999")

	// project 过滤+默认排除：仅 acct。
	got, err = s.ListResources(fx.projA, "", false)
	if err != nil {
		t.Fatalf("ListResources(project,默认) 失败: %v", err)
	}
	assertValuesEqual(t, "proj-a 默认", listValues(got), "acct:1000-1999")

	// type 过滤：全项目 port 含 released 2 行。
	got, err = s.ListResources(0, rtypePort, true)
	if err != nil {
		t.Fatalf("ListResources(type) 失败: %v", err)
	}
	assertValuesEqual(t, "port 全量", listValues(got), "8080", "9090")

	// type 过滤+排除 released：仅 proj-b 9090。
	got, err = s.ListResources(0, rtypePort, false)
	if err != nil {
		t.Fatalf("ListResources(type,默认) 失败: %v", err)
	}
	assertValuesEqual(t, "port 默认", listValues(got), "9090")

	// project+type 双过滤。
	got, err = s.ListResources(fx.projA, rtypeAccountRange, false)
	if err != nil {
		t.Fatalf("ListResources(project+type) 失败: %v", err)
	}
	assertValuesEqual(t, "proj-a account_range", listValues(got), "acct:1000-1999")
}

// TestResolveSession B4-2 规格审查改进 2①：三元组防歧义固化——sessions 的
// UNIQUE(project_id, column_id, name) 允许同名会话跨栏目多行（总控多栏目=多实体行），
// ResolveSession 必须限定 (project, column, name) 三元组精确命中、各归各 id 不串号；
// 查无列组合返回 ErrSessionNotFound。
func TestResolveSession(t *testing.T) {
	s := openTemp(t)
	fx := seedResFixtures(t, s)

	// 同 project（proj-a）同 name（fx.sessA 在 colA05）在 colA06 再插一行同名会话。
	res, err := s.DB.Exec(
		`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at)
		 VALUES (?, ?, 'controller-a', 'controller', ?, ?)`, fx.projA, fx.colA06, resFixtureNow, resFixtureNow)
	if err != nil {
		t.Fatalf("插入同名跨栏目会话失败: %v", err)
	}
	sessA06, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取会话 id 失败: %v", err)
	}

	// 同 name 两栏目各归各 id，不串号。
	id05, err := s.ResolveSession("proj-a", "05", "controller-a")
	if err != nil {
		t.Fatalf("ResolveSession(proj-a,05,controller-a) 意外失败: %v", err)
	}
	if id05 != fx.sessA {
		t.Errorf("ResolveSession(proj-a,05,controller-a) = %d, want fx.sessA=%d", id05, fx.sessA)
	}
	id06, err := s.ResolveSession("proj-a", "06", "controller-a")
	if err != nil {
		t.Fatalf("ResolveSession(proj-a,06,controller-a) 意外失败: %v", err)
	}
	if id06 != sessA06 {
		t.Errorf("ResolveSession(proj-a,06,controller-a) = %d, want colA06 行 id=%d（同名不串号）", id06, sessA06)
	}

	// 查无列组合（栏目不存在/会话不在该栏目域/项目不存在）→ ErrSessionNotFound。
	for _, tt := range []struct {
		name                 string
		projectCode, colCode string
	}{
		{name: "栏目不存在", projectCode: "proj-a", colCode: "99"},
		{name: "会话不在该栏目域", projectCode: "proj-b", colCode: "05"},
		{name: "项目不存在", projectCode: "no-such", colCode: "05"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.ResolveSession(tt.projectCode, tt.colCode, "controller-a"); !errors.Is(err, ErrSessionNotFound) {
				t.Fatalf("ResolveSession(%s,%s,controller-a) err = %v, want ErrSessionNotFound", tt.projectCode, tt.colCode, err)
			}
		})
	}
}

// auditRowValues 审计行值断言视图。
type auditRowValues struct {
	ProjectID int64
	ColumnID  int64
	SessionID int64
	Action    string
	Detail    string
	CreatedAt string
}

// auditLogRows 全量审计行（ORDER BY id=写入序）。
func auditLogRows(t *testing.T, s *Store) []auditRowValues {
	t.Helper()
	rows, err := s.DB.Query(`SELECT project_id, column_id, session_id, action, detail, created_at FROM audit_log ORDER BY id`)
	if err != nil {
		t.Fatalf("查询 audit_log 失败: %v", err)
	}
	defer rows.Close()
	var out []auditRowValues
	for rows.Next() {
		var r auditRowValues
		if err := rows.Scan(&r.ProjectID, &r.ColumnID, &r.SessionID, &r.Action, &r.Detail, &r.CreatedAt); err != nil {
			t.Fatalf("扫描 audit_log 失败: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 audit_log 失败: %v", err)
	}
	return out
}

// mustAuditDetail unmarshal 审计 detail JSON 为 map（失败即 Fatal）。
func mustAuditDetail(t *testing.T, r auditRowValues) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Detail), &m); err != nil {
		t.Fatalf("detail 非合法 JSON: %v\ndetail=%s", err, r.Detail)
	}
	return m
}

// TestRegisterReleaseAudit register/release 各落一行审计（action/detail/身份/注入时钟齐）。
func TestRegisterReleaseAudit(t *testing.T) {
	s := openTemp(t)
	fx := seedResFixtures(t, s)

	id := mustRegister(t, s, fx.projA, fx.colA05, rtypePort, "8080", "网关", fx.sessA)
	mustRelease(t, s, id, fx.sessA)

	rows := auditLogRows(t, s)
	if len(rows) != 2 {
		t.Fatalf("audit_log 行数 = %d，期望 2（register+release 各一）", len(rows))
	}

	reg := rows[0]
	if reg.Action != "resource.register" {
		t.Errorf("第一行 action = %q，期望 resource.register", reg.Action)
	}
	if reg.ProjectID != fx.projA || reg.ColumnID != fx.colA05 {
		t.Errorf("register 审计域 = (%d, %d)，期望 (%d, %d)", reg.ProjectID, reg.ColumnID, fx.projA, fx.colA05)
	}
	if reg.SessionID != fx.sessA {
		t.Errorf("register 审计身份 = %d，期望会话 id %d", reg.SessionID, fx.sessA)
	}
	if reg.CreatedAt != resFixtureNow {
		t.Errorf("register 审计 created_at = %q，期望注入时钟 %q", reg.CreatedAt, resFixtureNow)
	}
	regDetail := mustAuditDetail(t, reg)
	if regDetail["rtype"] != "port" || regDetail["value"] != "8080" || regDetail["note"] != "网关" {
		t.Errorf("register detail 快照缺字段: %s", reg.Detail)
	}

	rel := rows[1]
	if rel.Action != "resource.release" {
		t.Errorf("第二行 action = %q，期望 resource.release", rel.Action)
	}
	if rel.SessionID != fx.sessA {
		t.Errorf("release 审计身份 = %d，期望会话 id %d", rel.SessionID, fx.sessA)
	}
	if rel.CreatedAt != resFixtureNow {
		t.Errorf("release 审计 created_at = %q，期望注入时钟 %q", rel.CreatedAt, resFixtureNow)
	}
	relDetail := mustAuditDetail(t, rel)
	if before, ok := relDetail["before"].(map[string]any); !ok || before["id"] != float64(id) || before["value"] != "8080" {
		t.Errorf("release detail before 快照不符: %s", rel.Detail)
	}
	if after, ok := relDetail["after"].(map[string]any); !ok || after["status"] != "released" {
		t.Errorf("release detail after 快照不符: %s", rel.Detail)
	}

	// 幂等口径联动：重复 release 不重复落审计行。
	if err := s.ReleaseResource(id, fx.sessA, resFixtureNow); err != nil {
		t.Fatalf("重复 release 应幂等成功: %v", err)
	}
	if rows = auditLogRows(t, s); len(rows) != 2 {
		t.Errorf("重复 release 不应新增审计行，audit_log 行数 = %d", len(rows))
	}
}

// TestReleaseNotFound 空库释放：ErrResourceNotFound 哨兵（风格对齐 ErrValueInvalid 判定面）。
func TestReleaseNotFound(t *testing.T) {
	s := openTemp(t)
	if err := s.ReleaseResource(1, 0, resFixtureNow); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("空库 release 应返回 ErrResourceNotFound，实际 %v", err)
	}
}
