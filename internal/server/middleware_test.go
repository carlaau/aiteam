package server

import (
	"bytes"

	"encoding/json"

	"net/http"

	"net/http/httptest"

	"net/url"

	"path/filepath"

	"strings"

	"testing"

	"aiteam/internal/config"

	"aiteam/internal/store"

	"aiteam/internal/types"
)

// newTestEnv httptest + 临时目录真文件库装配完整骨架（WAL 语义需真文件，禁 :memory:），

// 同时返回 store 供夹具直插与数据面断言（B1-4 心跳中间件测试起需要）。

func newTestEnv(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开临时 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewServer(st, config.Default(), Version), st
}

// seedProjectColumn 直插项目+栏目夹具（§7.1 存在性校验的前置数据；照 store 包
// 测试惯例直插 SQL 不依赖 CRUD），返回两自增 id 供审计弱关联断言。colCode 传
// 空串=只建项目（栏目缺失用例）。

func seedProjectColumn(t *testing.T, st *store.Store, projCode, projStatus, colCode, colStatus string) (projectID, columnID int64) {
	t.Helper()
	const ts = "2026-01-01T00:00:00Z"
	res, err := st.DB.Exec(
		`INSERT INTO projects (code, name, status, heartbeat_timeout_sec, created_at, updated_at)
		 VALUES (?, ?, ?, 900, ?, ?)`,
		projCode, "项目"+projCode, projStatus, ts, ts,
	)
	if err != nil {
		t.Fatalf("插入项目夹具 %q 失败: %v", projCode, err)
	}
	projectID, _ = res.LastInsertId()
	if colCode == "" {
		return projectID, 0
	}
	res, err = st.DB.Exec(
		`INSERT INTO columns (project_id, code, name, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, colCode, "栏目"+colCode, colStatus, ts, ts,
	)
	if err != nil {
		t.Fatalf("插入栏目夹具 %q 失败: %v", colCode, err)
	}
	columnID, _ = res.LastInsertId()
	return projectID, columnID
}

// doHeartbeatReq 发起带身份四头的请求；proj/col/sess/role 任一传空串=省略该头
// （缺头用例构造面）。target 传探针路径 /api/v1/no-such-endpoint 时，200 类断言
// 不可用、404 not_found 即「穿过中间件到达 mux」的放行证据。

func doHeartbeatReq(t *testing.T, h http.Handler, method, target, proj, col, sess, role string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(nil))
	set := func(k, v string) {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	set(types.HeaderAiteamProject, proj)
	set(types.HeaderAiteamColumn, col)
	set(types.HeaderAiteamSession, sess)
	set(types.HeaderAiteamRole, role)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// errMsg 取错误响应的 message 文本（指明缺哪个头/哪个实体的断言面）。

func errMsg(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	m := decodeMap(t, rr)
	errObj, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 error 对象: %s", rr.Body.String())
	}
	msg, _ := errObj["message"].(string)
	return msg
}

// doProbe 探针请求（放行场景统一入口，B1-4 审查 Minor 2 收敛样板）：向未注册
// 探针路径带身份四头发请求，内置「已放行到 mux」断言——放行必得路径兜底
// 404 not_found，被心跳中间件 400/存在性 404 拦截即 Fatal。heads 按序填
// proj/col/sess/role（变参缺省=不带该头），返回 recorder 供后续数据面断言。

func doProbe(t *testing.T, h http.Handler, method, target string, heads ...string) *httptest.ResponseRecorder {
	t.Helper()
	hv := func(i int) string {
		if i < len(heads) {
			return heads[i]
		}
		return ""
	}
	rr := doHeartbeatReq(t, h, method, target, hv(0), hv(1), hv(2), hv(3))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("探针未放行: status = %d（body: %s）", rr.Code, rr.Body.String())
	}
	m := decodeMap(t, rr)
	errObj, _ := m["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "not_found" {
		t.Fatalf("探针响应非路径兜底 404 not_found（body: %s）", rr.Body.String())
	}
	return rr
}

// countTable 统计表行数（数据面断言）。

func countTable(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("统计 %s 失败: %v", table, err)
	}
	return n
}

// querySingleSession 恰一行时返回该行，否则 Fatal（心跳面单会话场景断言用）。

func querySingleSession(t *testing.T, st *store.Store) store.Session {
	t.Helper()
	var se store.Session
	err := st.DB.QueryRow(
		`SELECT id, project_id, column_id, name, role, last_seen_at, created_at FROM sessions`,
	).Scan(&se.ID, &se.ProjectID, &se.ColumnID, &se.Name, &se.Role, &se.LastSeenAt, &se.CreatedAt)
	if err != nil {
		t.Fatalf("查询 sessions 行失败: %v", err)
	}
	return se
}

// queryAuditByAction 按 action 过滤拉审计行（走 store 导出数据面 QueryAudit）。

func queryAuditByAction(t *testing.T, st *store.Store, action string) []store.AuditEntry {
	t.Helper()
	entries, err := st.QueryAudit(0, action, 100)
	if err != nil {
		t.Fatalf("查询审计 %q 失败: %v", action, err)
	}
	return entries
}

// decodeAuditDetail 解析审计 detail JSON 快照为映射（键断言面）。

func decodeAuditDetail(t *testing.T, e store.AuditEntry) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(e.Detail), &m); err != nil {
		t.Fatalf("审计 detail 非合法 JSON: %v\nraw: %s", err, e.Detail)
	}
	return m
}

// ---- ① 四头必填校验（b1-spec §二 流程①：缺→400，错误信息指明缺哪个） ----

// TestMissingHeaders 缺任一四头 → 400 missing_header，错误信息逐个指明缺失头名；
// 缺头校验先于存在性校验（无需登记夹具即可构造）。

func TestMissingHeaders(t *testing.T) {
	h, st := newTestEnv(t)
	for _, tc := range []struct {
		name                  string
		proj, col, sess, role string
		wantMissing           []string
	}{
		{"缺项目头", "", "05", "executor-A", "executor", []string{types.HeaderAiteamProject}},
		{"缺栏目头", "p-a", "", "executor-A", "executor", []string{types.HeaderAiteamColumn}},
		{"缺会话头", "p-a", "05", "", "executor", []string{types.HeaderAiteamSession}},
		{"缺角色头", "p-a", "05", "executor-A", "", []string{types.HeaderAiteamRole}},
		{"全缺", "", "", "", "", []string{
			types.HeaderAiteamProject, types.HeaderAiteamColumn,
			types.HeaderAiteamSession, types.HeaderAiteamRole,
		}},
	} {
		rr := doHeartbeatReq(t, h, http.MethodGet, "/api/v1/no-such-endpoint", tc.proj, tc.col, tc.sess, tc.role)
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeMissingHeader)
		msg := errMsg(t, rr)
		for _, want := range tc.wantMissing {
			if !strings.Contains(msg, want) {
				t.Errorf("[%s] 错误信息 %q 不含缺失头名 %q（须指明缺哪个）", tc.name, msg, want)
			}
		}
	}
	if n := countTable(t, st, "sessions"); n != 0 {
		t.Errorf("缺头拒绝后 sessions 行数 = %d，期望 0（校验失败不落库）", n)
	}
}

// ---- ② 存在性校验（AC2.2：未登记 → 404，错误信息指明；短路不 upsert） ----

// TestProjectNotFound 未登记项目 → 404 project_not_found + 错误信息含项目 code；
// 404 短路，不写 sessions。

func TestProjectNotFound(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	rr := doHeartbeatReq(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "nope", "05", "executor-A", "executor")

	wantErrBody(t, rr, http.StatusNotFound, types.CodeProjectNotFound)
	if msg := errMsg(t, rr); !strings.Contains(msg, "nope") {
		t.Errorf("错误信息 %q 不含项目 code %q（AC2.2 指明语义）", msg, "nope")
	}
	if n := countTable(t, st, "sessions"); n != 0 {
		t.Errorf("404 短路后 sessions 行数 = %d，期望 0（未登记拒绝不 upsert）", n)
	}
}

// TestColumnNotFound 栏目未登记两形态 → 404 column_not_found + 错误信息含栏目
// code：①栏目 code 全库不存在；②栏目 code 在其他项目存在（UNIQUE 按项目隔离，
// JOIN 限定 project_id 不得跨项目误命中）；404 短路不写 sessions。

func TestColumnNotFound(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "06", store.ColumnStatusActive)

	for _, tc := range []struct{ name, proj, col string }{
		{"栏目全库不存在", "p-a", "99"},
		{"跨项目同号栏目不串", "p-a", "06"}, // 06 属 p-b，配 p-a 头须判栏目不存在
	} {
		rr := doHeartbeatReq(t, h, http.MethodGet, "/api/v1/no-such-endpoint", tc.proj, tc.col, "executor-A", "executor")
		wantErrBody(t, rr, http.StatusNotFound, types.CodeColumnNotFound)
		if msg := errMsg(t, rr); !strings.Contains(msg, tc.col) {
			t.Errorf("[%s] 错误信息 %q 不含栏目 code %q（AC2.2 指明语义）", tc.name, msg, tc.col)
		}
	}
	if n := countTable(t, st, "sessions"); n != 0 {
		t.Errorf("404 短路后 sessions 行数 = %d，期望 0", n)
	}
}

// ---- ③ archived 放行（b1-spec §二：本批只挡不存在→404；409 拒绝归 B2） ----

// TestArchivedPassThrough archived 项目/栏目四头放行（探针 /api/v1/no-such-endpoint
// 返回 404 not_found = 请求穿过心跳中间件到达 mux，而非 400/存在性 404 拦截），
// 且会话 upsert 照常落行（b1-spec §二「archived 域本批放行」含数据面）。

func TestArchivedPassThrough(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-arch", store.ProjectStatusArchived, "c1", store.ColumnStatusActive)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "c2", store.ColumnStatusArchived)

	// 场景面：archived 项目 + active 栏目；active 项目 + archived 栏目。
	doProbe(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "p-arch", "c1", "executor-A", "executor")
	doProbe(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "p-a", "c2", "executor-B", "executor")

	if n := countTable(t, st, "sessions"); n != 2 {
		t.Fatalf("archived 放行后 sessions 行数 = %d，期望 2（upsert 照常）", n)
	}
}

// ---- ④ 心跳刷新（AC12.1）+ ⑤ 首次自动注册审计（T1/AC12.3） ----

// TestHeartbeatRefresh 同四元组两次请求 → sessions 恰单行，last_seen_at 随注入
// 时钟前进（AC12.1），created_at 保持首插值（§7.1 DO UPDATE 只刷 last_seen_at
// 与 role——role 未变此处不覆盖断言归 TestUpsertRoleChange 数据面）。

func TestHeartbeatRefresh(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	const t0 = "2026-01-01T08:00:00Z"
	const t1 = "2026-01-01T08:05:00Z"
	injectFixedClock(t, t0)
	doProbe(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "p-a", "05", "executor-A", "executor")
	first := querySingleSession(t, st)
	if first.LastSeenAt != t0 {
		t.Errorf("首插 last_seen_at = %q，期望注入时钟 %q（AC12.1）", first.LastSeenAt, t0)
	}

	injectFixedClock(t, t1)
	doProbe(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "p-a", "05", "executor-A", "executor")
	second := querySingleSession(t, st)
	if second.LastSeenAt != t1 {
		t.Errorf("重调 last_seen_at = %q，期望随注入时钟前进至 %q（AC12.1）", second.LastSeenAt, t1)
	}
	if second.CreatedAt != t0 {
		t.Errorf("created_at = %q，期望保持首插 %q", second.CreatedAt, t0)
	}
	if second.ID != first.ID {
		t.Errorf("重调 id = %d，期望保持首插 %d（同实体不新增行，AC12.3）", second.ID, first.ID)
	}
	if n := countTable(t, st, "sessions"); n != 1 {
		t.Errorf("sessions 行数 = %d，期望 1", n)
	}
}

// TestAutoRegisterAudit 首次请求 → audit 落 session.auto_register（T1）：detail
// 为会话快照 JSON（project/column/session/role 四键，b1-spec §二 建议口径）、
// 三弱关联 id 对齐会话行、created_at=服务端注入时钟；重复请求幂等不再落。

func TestAutoRegisterAudit(t *testing.T) {
	h, st := newTestEnv(t)
	pid, cid := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	doProbe(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "p-a", "05", "executor-A", "executor")

	sess := querySingleSession(t, st)
	entries := queryAuditByAction(t, st, store.AuditSessionAutoRegister)
	if len(entries) != 1 {
		t.Fatalf("auto_register 审计行数 = %d，期望 1", len(entries))
	}
	e := entries[0]
	if e.ProjectID != pid || e.ColumnID != cid || e.SessionID != sess.ID {
		t.Errorf("审计弱关联 = (%d,%d,%d)，期望对齐夹具/会话行 (%d,%d,%d)",
			e.ProjectID, e.ColumnID, e.SessionID, pid, cid, sess.ID)
	}
	if e.CreatedAt != t0 {
		t.Errorf("审计 created_at = %q，期望服务端注入时钟 %q（AC1.4）", e.CreatedAt, t0)
	}
	detail := decodeAuditDetail(t, e)
	for k, want := range map[string]any{
		"project": "p-a", "column": "05", "session": "executor-A", "role": "executor",
	} {
		if got := detail[k]; got != want {
			t.Errorf("auto_register detail[%q] = %v，期望 %q（b1-spec §二 快照口径）", k, got, want)
		}
	}

	// 重复请求（时钟前进）→ 幂等，不再落 auto_register。
	injectFixedClock(t, "2026-01-01T08:05:00Z")
	doProbe(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "p-a", "05", "executor-A", "executor")
	if got := len(queryAuditByAction(t, st, store.AuditSessionAutoRegister)); got != 1 {
		t.Errorf("重复请求后 auto_register 审计行数 = %d，期望仍 1（隐式注册幂等不重复留痕）", got)
	}
}

// ---- ⑥ role 变更审计（D5：静默覆盖+强制审计） ----

// TestRoleChangeAudit 同名会话换 role 再调用 → audit 落 role-change，detail 含
// before/after（D5）；sessions.role 静默覆盖；同 role 稳态重调不重复落。

func TestRoleChangeAudit(t *testing.T) {
	h, st := newTestEnv(t)
	pid, cid := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	injectFixedClock(t, "2026-01-01T08:00:00Z")
	doProbe(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "p-a", "05", "executor-A", "executor")

	// 换 role 重调（B2 合规化：换班场景裸 executor → executor_A，规约内合法
	// 变体；原 foreman 已被 role 前缀轻校验 400 拒收，见 ⑨）。
	injectFixedClock(t, "2026-01-01T09:00:00Z")
	doProbe(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "p-a", "05", "executor-A", "executor_A")
	if sess := querySingleSession(t, st); sess.Role != "executor_A" {
		t.Errorf("换 role 后 sessions.role = %q，期望静默覆盖为 executor_A（D5）", sess.Role)
	}
	entries := queryAuditByAction(t, st, store.AuditRoleChange)
	if len(entries) != 1 {
		t.Fatalf("role-change 审计行数 = %d，期望 1", len(entries))
	}
	e := entries[0]
	if e.ProjectID != pid || e.ColumnID != cid {
		t.Errorf("role-change 弱关联 project/column = (%d,%d)，期望 (%d,%d)", e.ProjectID, e.ColumnID, pid, cid)
	}
	detail := decodeAuditDetail(t, e)
	if got := detail["before"]; got != "executor" {
		t.Errorf("role-change detail.before = %v，期望 executor（D5）", got)
	}
	if got := detail["after"]; got != "executor_A" {
		t.Errorf("role-change detail.after = %v，期望 executor_A（D5）", got)
	}

	// 同 role 稳态重调 → 不重复落 role-change。
	injectFixedClock(t, "2026-01-01T09:05:00Z")
	doProbe(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "p-a", "05", "executor-A", "executor_A")
	if got := len(queryAuditByAction(t, st, store.AuditRoleChange)); got != 1 {
		t.Errorf("稳态重调后 role-change 审计行数 = %d，期望仍 1（同 role 不重复审计）", got)
	}
	// 换 role 不重落 auto_register（隐式注册只在首调）。
	if got := len(queryAuditByAction(t, st, store.AuditSessionAutoRegister)); got != 1 {
		t.Errorf("auto_register 审计行数 = %d，期望仍 1", got)
	}
}

// ---- ⑦ 豁免名单（§7.1「ping 除外」+ b1-spec §二「ping/version 豁免」） ----

// TestPingExempt ping/version 无四头 200（健康检查无会话语义）；豁免=完全跳过：
// 带四头打 ping 也不 upsert、不审计（会话数据面零写入）。

func TestPingExempt(t *testing.T) {
	h, st := newTestEnv(t)

	// 无四头 → 200（豁免名单两端点遍历）。
	for _, target := range []string{"/api/v1/ping", "/api/v1/version"} {
		rr := doHeartbeatReq(t, h, http.MethodGet, target, "", "", "", "")
		if rr.Code != http.StatusOK {
			t.Errorf("无四头 %s status = %d，期望 200（豁免名单）", target, rr.Code)
		}
	}

	// 带四头同样不落会话数据面（豁免=跳过整段心跳链，非仅免校验）。
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	if rr := doHeartbeatReq(t, h, http.MethodGet, "/api/v1/ping", "p-a", "05", "executor-A", "executor"); rr.Code != http.StatusOK {
		t.Fatalf("带四头 ping status = %d，期望 200（body: %s）", rr.Code, rr.Body.String())
	}
	if n := countTable(t, st, "sessions"); n != 0 {
		t.Errorf("豁免端点带四头后 sessions 行数 = %d，期望 0（豁免=完全跳过）", n)
	}
	if n := countTable(t, st, "audit_log"); n != 0 {
		t.Errorf("豁免端点带四头后 audit_log 行数 = %d，期望 0", n)
	}

	// 路径行为锁定（B1-4 审查 Minor 3）：豁免按 URL.Path 精确匹配。
	// ① 大小写变体不豁免：/api/v1/PING 走四头校验 → 无头 400；
	rr := doHeartbeatReq(t, h, http.MethodGet, "/api/v1/PING", "", "", "", "")
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeMissingHeader)

	// ② 百分号编码经 URL 解码同 Path：/api/v1/%70ing 解码后 = /api/v1/ping，
	//    命中豁免 200（若未来翻转成 EscapedPath 判定本断言即红）。
	rr = doHeartbeatReq(t, h, http.MethodGet, "/api/v1/%70ing", "", "", "", "")
	if rr.Code != http.StatusOK {
		t.Errorf("/api/v1/%%70ing 解码命中豁免 status = %d，期望 200（body: %s）", rr.Code, rr.Body.String())
	}
}

// ---- ⑧ 作用域（B1-4 审查 Important 1 总控裁定选项①：仅 /api/v1/ 前缀生效） ----

// TestNonAPIPathPassthrough 非 /api/v1/ 前缀路径直接放行：无头请求不 400。
// rebase 演进（B5-2 挂载静态页后）："/" 落 staticHandler 返回 200 index.html
// （正确行为——原 404 兜底断言是 B5-2 挂载前的前提），放行语义不变（未 400 即
// 未被心跳拦截）；/foo 与 /api/v1（无尾斜杠，不满足前缀）仍落 404 路径兜底，
// B0 行为不回归。

func TestNonAPIPathPassthrough(t *testing.T) {
	h, _ := newTestEnv(t)

	// "/"：放行抵达 staticHandler → 200 HTML 本体（B5-2 静态页挂载后行为）
	rr := doHeartbeatReq(t, h, http.MethodGet, "/", "", "", "", "")
	if rr.Code != http.StatusOK {
		t.Errorf(`GET / status = %d, want 200（B5-2 静态页本体；body: %s）`, rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("GET / Content-Type = %q, want text/html; charset=utf-8", ct)
	}
	// "/foo"、"/api/v1"：放行抵达 mux 路径兜底 → 404 not_found（B0 行为不回归）
	for _, target := range []string{"/foo", "/api/v1"} {
		rr := doHeartbeatReq(t, h, http.MethodGet, target, "", "", "", "")
		wantErrBody(t, rr, http.StatusNotFound, "not_found")
	}
}

// ---- ⑨ role 前缀轻校验（B2 批裁定：controller / executor / executor_<标识>） ----

// TestRolePrefixValidation role 取值规约（一格一人，防乱起 role 名导致定向投递
// 混乱）：合法形态恰三种——controller 单值 / executor 裸值（换班场景）/ executor_
// 前缀多执行者（executor_A、executor_web-1 等）；规约外值 → 400 invalid_role，
// 错误信息含实际 role 值与合法形态说明；校验在四头解析后立即短路（先于存在性
// 校验，无需登记夹具即可构造），拒绝不 upsert 不审计（sessions 零写入）。
// 边界裁量：executor_（前缀后空标识）放行——前缀校验是轻校验、不限制标识字符，
// 空标识语义上等价裸 executor 变体，不额外拒绝。类广播（面向全员）不走 role
// 目标（用 --bus），与本规约正交。
func TestRolePrefixValidation(t *testing.T) {
	h, st := newTestEnv(t)
	// 夹具仅供正例/边界放行后穿过存在性校验（探针证据链完整）；反例在 role
	// 校验处短路（先于存在性校验），有无夹具不影响 400 断言。
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	// 反例：规约外 role → 400 invalid_role（错误码 + 错误信息含实际值与形态说明）。
	for _, role := range []string{"worker", "admin"} {
		rr := doHeartbeatReq(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "p-a", "05", "executor-A", role)
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeInvalidRole)
		msg := errMsg(t, rr)
		if !strings.Contains(msg, role) {
			t.Errorf("role %q 错误信息 %q 不含实际 role 值（指明哪个非法）", role, msg)
		}
		for _, want := range []string{"controller", "executor_<标识>"} {
			if !strings.Contains(msg, want) {
				t.Errorf("role %q 错误信息 %q 不含合法形态说明 %q", role, msg, want)
			}
		}
	}
	if n := countTable(t, st, "sessions"); n != 0 {
		t.Errorf("invalid_role 拒绝后 sessions 行数 = %d，期望 0（校验失败不落库）", n)
	}

	// 正例：三种合法形态放行（探针 404 not_found = 穿过中间件到达 mux）。
	for _, role := range []string{"controller", "executor_A", "executor"} {
		doProbe(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "p-a", "05", "executor-A", role)
	}

	// 边界：executor_（前缀后空标识）放行——轻校验裁量，见函数头注释。
	doProbe(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "p-a", "05", "executor-A", "executor_")
}

// ---- ⑩ 系统动作豁免：POST /api/v1/projects 自举引导（B2 首任务前置修，
//        总控裁定 4168d76：首条登记天然无已登记身份域可挂四头，端点层豁免
//        四头强制） ----

// TestProjectRegisterExemptMissingHeaders 全新空库无四头 POST /api/v1/projects
// → 201（登记死锁解除）：豁免缺头强制、请求不经心跳链——sessions 零写入、
// context 无注入，handler 审计 session_id=0（系统动作留痕不丢，audit_log 弱
// 关联 0 直插合法）。带四头 POST 照常走心跳链的 B1 语义由既有
// TestCreateProjectEndpoint 回归保障（豁免只作用于缺头强制环节）。
func TestProjectRegisterExemptMissingHeaders(t *testing.T) {
	h, st := newTestEnv(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	// 死锁解除：无四头（doAuthedReq heads 全省略=不设头）登记 → 201 + 落库。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/projects",
		`{"code":"p-a","name":"项目A"}`)
	data := wantData(t, rr, http.StatusCreated)
	if got := data["code"]; got != "p-a" {
		t.Errorf("data.code = %v，期望 p-a（豁免放行抵达 handler）", got)
	}
	newID := int64(data["id"].(float64))

	// 自举审计链闭环：project.register 恰 1 行，session_id=0（豁免无 context
	// 注入=系统动作），project_id 弱关联新项目行。
	entries := queryAuditByAction(t, st, store.AuditProjectRegister)
	if len(entries) != 1 {
		t.Fatalf("project.register 审计行数 = %d，期望 1", len(entries))
	}
	if e := entries[0]; e.SessionID != 0 || e.ProjectID != newID {
		t.Errorf("审计弱关联 = (session=%d, project=%d)，期望 (session=0 系统动作, project=%d)",
			e.SessionID, e.ProjectID, newID)
	}
	// 豁免=缺头直接放行不经心跳链：sessions 零写入（无头无会话可挂）。
	if n := countTable(t, st, "sessions"); n != 0 {
		t.Errorf("豁免登记后 sessions 行数 = %d，期望 0（缺头豁免不 upsert）", n)
	}
}

// TestProjectRegisterExemptNotLeaky 豁免不外溢（裁定约束：任何匿名写不得借
// 豁免绕过心跳）：同路径其余方法（GET/PATCH/DELETE）缺头仍 400 missing_header，
// 精确匹配外的深层路径（POST /api/v1/projects/{code}）与其余端点（栏目登记）
// 缺头照常 400；ping/version 既有豁免回归不破。
func TestProjectRegisterExemptNotLeaky(t *testing.T) {
	h, _ := newTestEnv(t)

	// 同路径其他方法 + 非精确路径缺头 → 400 missing_header。
	for _, tc := range []struct {
		name, method, target string
	}{
		{"GET 列表不豁免", http.MethodGet, "/api/v1/projects"},
		{"PATCH 更新不豁免", http.MethodPatch, "/api/v1/projects/p-x"},
		{"DELETE 归档不豁免", http.MethodDelete, "/api/v1/projects/p-x"},
		{"POST 深层路径不精确命中", http.MethodPost, "/api/v1/projects/p-x"},
	} {
		rr := doHeartbeatReq(t, h, tc.method, tc.target, "", "", "", "")
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeMissingHeader)
	}

	// 栏目登记已对称豁免（#9 小批派工：空库自举死结第二层）——外溢面改为：
	// 同 pattern 其余方法（GET/PATCH/DELETE）与更深一层路径缺头照常 400。
	for _, tc := range []struct {
		name, method, target string
	}{
		{"GET 栏目列表不豁免", http.MethodGet, "/api/v1/projects/p-x/columns"},
		{"PATCH 栏目更新不豁免", http.MethodPatch, "/api/v1/projects/p-x/columns/01"},
		{"DELETE 栏目归档不豁免", http.MethodDelete, "/api/v1/projects/p-x/columns/01"},
		{"POST 更深一层不豁免", http.MethodPost, "/api/v1/projects/p-x/columns/01/messages"},
	} {
		rr := doHeartbeatReq(t, h, tc.method, tc.target, "", "", "", "")
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeMissingHeader)
	}

	// 既有豁免回归：ping/version 缺头仍 200。
	for _, target := range []string{"/api/v1/ping", "/api/v1/version"} {
		if rr := doHeartbeatReq(t, h, http.MethodGet, target, "", "", "", ""); rr.Code != http.StatusOK {
			t.Errorf("缺头 %s status = %d，期望 200（既有豁免回归不破）", target, rr.Code)
		}
	}
}

// TestColumnRegisterExemptBootstrap 空库自举死结第二层（#9 小批派工，S6 前必
// 修）：首项目豁免（e3d91c9）只解第一层——首栏目 POST /api/v1/projects/{code}/
// columns 的身份四头在心跳 ② 存在性短路（栏目行未登记 column_not_found）下同
// 样建不了，空库 CLI column register 同死。对称 projects 论证：登记场景无身份
// 可挂放行+不 upsert。豁免只作用于缺头环节（带四头照常完整心跳链）。
func TestColumnRegisterExemptBootstrap(t *testing.T) {
	h, st := newTestEnv(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	// 第一层（既有豁免回归）：空库建项目。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/projects",
		`{"code":"p-a","name":"项目A"}`)
	wantData(t, rr, http.StatusCreated)

	// 第二层（本批）：无四头建首栏目 → 201 + 落库。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/projects/p-a/columns",
		`{"code":"05","name":"栏目05"}`)
	data := wantData(t, rr, http.StatusCreated)
	if got := data["code"]; got != "05" {
		t.Errorf("data.code = %v，期望 05（豁免放行抵达 handler）", got)
	}

	// 豁免=缺头不经心跳链：sessions 零写入（登记动作全程零会话可挂）。
	if n := countTable(t, st, "sessions"); n != 0 {
		t.Errorf("豁免登记后 sessions 行数 = %d，期望 0（缺头豁免不 upsert）", n)
	}
	// 自举后带四头正常收发链路可用（心跳 ② 存在性短路解除）。
	rr = doHeartbeatReq(t, h, http.MethodGet, "/api/v1/ping", "p-a", "05", "controller-A", "controller")
	if rr.Code != http.StatusOK {
		t.Errorf("自举后带四头 ping status = %d，期望 200（存在性短路解除）", rr.Code)
	}
}

// testToken 开态测试用令牌：刻意含 : + @ = 等 URL 保留字符，同一份 token 同时

// 覆盖 Bearer 头（原样传输无转义）与 ?token=（须经 URL 编码）两形态边界。

const testToken = "s3cr:et+p@ss=word"

// dataPath 看板数据口代表路径（B1 注册的消息端点；B6 阶段未注册，落 404 兜底）。
// 鉴权判据全走它：无/错 token → 401（先于 404，AC14.6「数据口拒」服务端判据）；
// 对 token → 404（过鉴权抵达 mux 兜底，证明放行而非豁免）。

const dataPath = "/api/v1/messages"

// newAuthTestServer 与 newTestServer 同构（httptest + 临时目录真库，WAL 语义需真
// 文件禁 :memory:），但允许经 mutate 变形配置；同时返回 store——
// TestTokenBeforeHeartbeat 需直接查 sessions 计数锚定链序语义。

func newAuthTestServer(t *testing.T, mutate func(*config.ServerConfig)) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开临时 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	if mutate != nil {
		mutate(cfg)
	}
	return NewServer(st, cfg, Version), st
}

// newTokenOnServer 开态样板：token_enabled=true + testToken（AC17.3）。

func newTokenOnServer(t *testing.T) (http.Handler, *store.Store) {
	return newAuthTestServer(t, func(c *config.ServerConfig) {
		c.Auth.TokenEnabled = true
		c.Auth.Token = testToken
	})
}

// countSessions 查 sessions 表行数（token 判定先于会话心跳的链序判据直接查库，
// 不依赖心跳端点存在——B1 心跳 B6 阶段尚未挂载）。

func countSessions(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("查询 sessions 计数失败: %v", err)
	}
	return n
}

// TestTokenDisabled AC17.2：鉴权关（默认配置，拷贝即跑）→ 无凭证全端点通。
// ping/version 直连 200；未注册数据口落 404 兜底（若误挂拦截应得 401）。

func TestTokenDisabled(t *testing.T) {
	h := newTestServer(t) // config.Default(): TokenEnabled=false

	for _, path := range []string{"/api/v1/ping", "/api/v1/version"} {
		rr := doReq(t, h, http.MethodGet, path, nil)
		if rr.Code != http.StatusOK {
			t.Errorf("关态无凭证 GET %s status = %d, want 200（body: %s）", path, rr.Code, rr.Body.String())
		}
	}
	// 关态无凭证抵达心跳层（缺四头 400 missing_header）而非 401：证明关态中间件
	// 直通未拦截（rebase B1 演进：数据口已注册，判据由 404 兜底改为心跳四头校验）
	wantErrBody(t, doReq(t, h, http.MethodGet, dataPath, nil), http.StatusBadRequest, types.CodeMissingHeader)
}

// TestTokenEnforced AC17.3 服务端：开态下无/错凭证 401 auth_required，对凭证放行；
// Bearer 头与 ?token= 双形态等价；双形态同现时 Bearer 优先（?token= 仅兜底，
// 头形态存在即不回退——「头优先、query 兜底」的取值序语义）。
// 「放行」判据 = 过 tokenAuth 抵达心跳层得 400 missing_header（缺身份四头被心跳
// 拦截；rebase B1 演进：数据口已注册，原 404 mux 兜底判据随之失效）而非 401（被拦截）。

func TestTokenEnforced(t *testing.T) {
	h, _ := newTokenOnServer(t)

	tests := []struct {
		name  string
		auth  string // Authorization 头值（""=不带）
		query string // ?token= 原文（""=不带；非空时经 url.QueryEscape 编码）
		want  int    // 期望状态码
		code  string // 期望 error.code（401/404 响应均为统一 JSON 错误形状）
	}{
		{name: "无任何凭证", want: http.StatusUnauthorized, code: "auth_required"},
		{name: "Bearer头错token", auth: "Bearer wrong-token", want: http.StatusUnauthorized, code: "auth_required"},
		{name: "query错token", query: "wrong-token", want: http.StatusUnauthorized, code: "auth_required"},
		{name: "Bearer头对token放行", auth: "Bearer " + testToken, want: http.StatusBadRequest, code: types.CodeMissingHeader},
		{name: "query对token放行_双形态等价", query: testToken, want: http.StatusBadRequest, code: types.CodeMissingHeader},
		{name: "Bearer小写scheme放行", auth: "bearer " + testToken, want: http.StatusBadRequest, code: types.CodeMissingHeader},
		{name: "Bearer优先于query_头错即拒不回退", auth: "Bearer wrong-token", query: testToken, want: http.StatusUnauthorized, code: "auth_required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := dataPath
			if tt.query != "" {
				target += "?token=" + url.QueryEscape(tt.query)
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			wantErrBody(t, rr, tt.want, tt.code)
		})
	}

	// 豁免口在开态不受影响：对 token GET /api/v1/version → 200（plan「对 token 200」锚点：
	// 开启鉴权后存量健康检查客户端仍可用）
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("开态对 token GET /api/v1/version status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
	}
}

// TestTokenExemptions 开态豁免面（B6-T1 裁定口径，path 精确匹配）：
//   - /api/v1/ping、/api/v1/version：健康检查无 token 仍 200（探活客户端无 token 配置）；
//   - /（GET/HEAD 页面本体）：B5 起页面已注册，无 token 得 200 HTML；豁免语义本体
//     不变（401→放行，放行后命中静态 handler）；
//   - 数据口不豁免：/api/v1/messages 无 token → 401 优先于 404（AC14.6 服务端判据）；
//   - 精确匹配非前缀/非后缀：/api/v1/ping/（尾斜杠）与 /api/v1/pi（前缀截断）均不在
//     豁免面，无 token → 401。

func TestTokenExemptions(t *testing.T) {
	h, _ := newTokenOnServer(t)

	// 健康检查两豁免口
	for _, path := range []string{"/api/v1/ping", "/api/v1/version"} {
		rr := doReq(t, h, http.MethodGet, path, nil)
		if rr.Code != http.StatusOK {
			t.Errorf("开态无 token GET %s status = %d, want 200（豁免失效，body: %s）", path, rr.Code, rr.Body.String())
		}
	}
	// GET / 页面本体豁免：B5-T1 已注册静态页，无 token 得 200 HTML（#23「页面
	// 本身无敏感数据」——B6 阶段未注册时此断言锚定 404 兜底，页面注册后演进为 200）
	rr := doReq(t, h, http.MethodGet, "/", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("开态无 token GET / status = %d, want 200（页面本体豁免可载）", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("开态无 token GET / Content-Type = %q, want text/html; charset=utf-8", got)
	}
	// HEAD / 同豁免（ServeMux GET pattern 隐式含 HEAD），B5 后命中静态 handler 返 200
	if rr := doReq(t, h, http.MethodHead, "/", nil); rr.Code != http.StatusOK {
		t.Errorf("开态无 token HEAD / status = %d, want 200（页面本体豁免可载）", rr.Code)
	}
	// 数据口不豁免：无 token 401 优先于 404
	wantErrBody(t, doReq(t, h, http.MethodGet, dataPath, nil), http.StatusUnauthorized, "auth_required")
	// 精确匹配：近邻路径不在豁免面
	for _, path := range []string{"/api/v1/ping/", "/api/v1/pi", "/api/v1/version/x"} {
		wantErrBody(t, doReq(t, h, http.MethodGet, path, nil), http.StatusUnauthorized, "auth_required")
	}
}

// TestTokenBeforeHeartbeat 链序语义锚（§2.1 组装固化：bodyLimit → tokenAuth → [B1 心跳] → mux）：
//   - token 开时无凭证请求（数据口 401 与豁免口 200 均试）不得产生 sessions 新行——
//     鉴权判定先于会话心跳，未过鉴权不刷心跳（B6 挂点约定：心跳插在 tokenAuth 内层）；
//   - 无凭证数据口请求得 401 而非 404：未过鉴权不触达 mux（中间件层即拒）；
//   - 对凭证 + 身份四头（X-Aiteam-Project/Column/Session/Role，§技术设计）放行抵达
//     mux（ping 200）；当前 B1 心跳未挂载，sessions 仍须零行（不得误写），B1 挂载后
//     该面由 B1 测试接管。

func TestTokenBeforeHeartbeat(t *testing.T) {
	h, st := newTokenOnServer(t)

	// 无凭证两形态请求（拒 + 豁免通）
	wantErrBody(t, doReq(t, h, http.MethodGet, dataPath, nil), http.StatusUnauthorized, "auth_required")
	rr := doReq(t, h, http.MethodGet, "/api/v1/ping", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("豁免口 GET ping status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
	}
	if n := countSessions(t, st); n != 0 {
		t.Errorf("无凭证请求后 sessions 行数 = %d, want 0（鉴权先于心跳，未过鉴权不得写库）", n)
	}

	// 对凭证 + 身份四头：放行抵达 mux
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("X-Aiteam-Project", "proj-a")
	req.Header.Set("X-Aiteam-Column", "05")
	req.Header.Set("X-Aiteam-Session", "controller")
	req.Header.Set("X-Aiteam-Role", "controller")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("对 token+四头 GET ping status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
	}
	if n := countSessions(t, st); n != 0 {
		t.Errorf("对 token 请求后 sessions 行数 = %d, want 0（B1 心跳未挂载，不得误写）", n)
	}
}
