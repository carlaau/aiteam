package server

import (
	"net/http"
	"testing"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 本文件为端点 #27 GET /api/v1/sessions 与 #28 GET /api/v1/audit 的 handler 测试
// （§2.2 逐字段）：httptest+真库、请求经心跳中间件带四头（doAuthedReq）、注入
// 时钟构造失联场景（AC12.1 观测口 / AC1.4+AC2.4 机械核验口）。

// seedSessionRow 直插会话行（照 store 包测试惯例直插 SQL，不依赖中间件 upsert），
// 返回自增 id。lastSeenAt 相对注入 now 的差值供 alive 失联场景构造。
func seedSessionRow(t *testing.T, st *store.Store, projectID, columnID int64, name, role, lastSeenAt, createdAt string) int64 {
	t.Helper()
	res, err := st.DB.Exec(
		`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, columnID, name, role, lastSeenAt, createdAt,
	)
	if err != nil {
		t.Fatalf("插入会话夹具 %q 失败: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取会话夹具 %q 自增 id 失败: %v", name, err)
	}
	return id
}

// seedAuditRow 直插审计行（三弱关联 id 可指定任意值含悬空——弱关联无 FK），
// 返回自增 id。
func seedAuditRow(t *testing.T, st *store.Store, projectID, columnID, sessionID int64, action, detail, createdAt string) int64 {
	t.Helper()
	res, err := st.DB.Exec(
		`INSERT INTO audit_log (project_id, column_id, session_id, action, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, columnID, sessionID, action, detail, createdAt,
	)
	if err != nil {
		t.Fatalf("插入审计夹具 %q 失败: %v", action, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取审计夹具 %q 自增 id 失败: %v", action, err)
	}
	return id
}

// seedBulkAudit 单条 WITH RECURSIVE 直插 n 行同型审计行（limit 边界场景夹具，
// 一条语句免逐行往返；夹具直插非生产代码，不受往返账约束）。
func seedBulkAudit(t *testing.T, st *store.Store, n int) {
	t.Helper()
	_, err := st.DB.Exec(
		`INSERT INTO audit_log (project_id, column_id, session_id, action, detail, created_at)
		 WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM seq WHERE x < ?)
		 SELECT 0, 0, 0, 'project.register', '{}', '2026-01-01T00:00:00Z' FROM seq`, n,
	)
	if err != nil {
		t.Fatalf("批量插入审计夹具 %d 行失败: %v", n, err)
	}
}

// decodeRows 响应 data 内数组字段 → 行对象切片（nil 或非数组即 Fatal——空结果
// 须为非 nil 空数组，JSON null 视为响应面缺陷）。
func decodeRows(t *testing.T, v any, what string) []map[string]any {
	t.Helper()
	rows, ok := v.([]any)
	if !ok {
		t.Fatalf("%s 非数组（null/缺失即缺陷）: %v", what, v)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("%s 行非对象: %v", what, r)
		}
		out = append(out, m)
	}
	return out
}

// wantKeys 断言行对象字段面齐（缺键=响应面与 §2.2 表对拍失败）。
func wantKeys(t *testing.T, row map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := row[k]; !ok {
			t.Errorf("行缺字段 %q（§2.2 字段面对拍）: %v", k, row)
		}
	}
}

// seedSentinel 直插哨兵行（last_ping_at 可控——alive/dead 混合夹具；绕过 #14 注册
// 隔离被测面，store 包 fixtureSentinel 同款 INSERT...SELECT 带出 project_id）。
func seedSentinel(t *testing.T, st *store.Store, sessionID, columnID int64, role, pingAt string) int64 {
	t.Helper()
	res, err := st.DB.Exec(
		`INSERT INTO sentinels (session_id, project_id, column_id, role, started_at, last_ping_at)
		 SELECT ?, project_id, ?, ?, ?, ? FROM sessions WHERE id = ?`,
		sessionID, columnID, role, pingAt, pingAt, sessionID,
	)
	if err != nil {
		t.Fatalf("插入哨兵 %s 夹具失败: %v", role, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取哨兵自增 id 失败: %v", err)
	}
	return id
}

// ---- #27 GET /api/v1/sessions ----

// TestSessionList 心跳面数据口（§2.2 #27）：带四头调用走中间件（自身隐式注册
// 入列表=AC12.1 端到端观测闭环）；字段面 8 键齐、sentinels 恒空数组；alive 失联
// 计算正确（注入时钟：默认阈值 900s 与项目级覆盖 60s 双分支）；archived 项目
// 会话不列（§7.2）；按 created_at,id 稳定升序。
func TestSessionList(t *testing.T) {
	h, st := newTestEnv(t)
	pidA, cidA := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	pidB, cidB := seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "06", store.ColumnStatusActive)
	pidArch, cidArch := seedProjectColumn(t, st, "p-arch", store.ProjectStatusArchived, "c1", store.ColumnStatusActive)

	// p-b 项目级阈值覆盖为 60s（失联计算 COALESCE(项目级, 服务默认) 分支）。
	if _, err := st.DB.Exec(`UPDATE projects SET heartbeat_timeout_sec = 60 WHERE id = ?`, pidB); err != nil {
		t.Fatalf("覆盖 p-b 项目级阈值失败: %v", err)
	}
	// 注入 now=08:00:00；夹具会话 created_at 递增锚定列表稳定序。
	const now = "2026-01-01T08:00:00Z"
	seedSessionRow(t, st, pidA, cidA, "worker-W1", "worker", "2026-01-01T07:50:00Z", "2026-01-01T07:00:00Z")      // 差 600s < 900 → 在线
	seedSessionRow(t, st, pidB, cidB, "worker-W2", "worker", "2026-01-01T07:50:00Z", "2026-01-01T07:05:00Z")      // 差 600s > 60 → 失联（项目级）
	seedSessionRow(t, st, pidA, cidA, "executor-E1", "executor", "2026-01-01T07:00:00Z", "2026-01-01T07:10:00Z")  // 差 3600s > 900 → 失联（默认）
	seedSessionRow(t, st, pidArch, cidArch, "worker-X", "worker", "2026-01-01T07:59:00Z", "2026-01-01T07:15:00Z") // archived 项目不列

	injectFixedClock(t, now)
	// 带四头调用（走心跳中间件）：probe-S 首调隐式注册，last_seen_at=注入 now。
	rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/sessions", "", "p-a", "05", "probe-S", "executor")

	data := wantData(t, rr, http.StatusOK)
	rows := decodeRows(t, data["sessions"], "data.sessions")
	if len(rows) != 4 {
		t.Fatalf("sessions 行数 = %d，期望 4（两 active 项目 3 夹具+probe 自身；archived 项目不列）", len(rows))
	}
	// 稳定升序（created_at,id）：07:00 / 07:05 / 07:10 / now。
	wantOrder := []string{"worker-W1", "worker-W2", "executor-E1", "probe-S"}
	for i, want := range wantOrder {
		if got := rows[i]["name"]; got != want {
			t.Errorf("sessions[%d].name = %v，期望 %q（created_at,id 升序）", i, got, want)
		}
	}

	// 逐行字段面对拍：8 键齐 + sentinels 空数组（本测试夹具无哨兵行；填充语义
	// 归 TestSessionListSentinels）+ alive 三分支 + probe 行心跳闭环。
	wantAlive := map[string]bool{"worker-W1": true, "worker-W2": false, "executor-E1": false, "probe-S": true}
	for _, row := range rows {
		name, _ := row["name"].(string)
		wantKeys(t, row, "id", "project", "column", "name", "role", "last_seen_at", "alive", "sentinels")
		if sen, ok := row["sentinels"].([]any); !ok || len(sen) != 0 {
			t.Errorf("会话 %q sentinels = %v, want 空数组（本夹具无哨兵行，禁 null）", name, row["sentinels"])
		}
		if got, ok := row["alive"].(bool); !ok || got != wantAlive[name] {
			t.Errorf("会话 %q alive = %v, want %v（注入 now=%s 失联计算）", name, row["alive"], wantAlive[name], now)
		}
	}
	if got := rows[3]["project"]; got != "p-a" {
		t.Errorf("probe 行 project = %v, want %q（JOIN 装配 code）", got, "p-a")
	}
	if got := rows[3]["column"]; got != "05" {
		t.Errorf("probe 行 column = %v, want %q（JOIN 装配 code）", got, "05")
	}
	if got := rows[3]["last_seen_at"]; got != now {
		t.Errorf("probe 行 last_seen_at = %v, want 注入时钟 %q（AC12.1 带四头请求刷心跳，#27 可观测）", got, now)
	}
	if id, ok := rows[3]["id"].(float64); !ok || id <= 0 {
		t.Errorf("probe 行 id = %v, want 正整数自增 id", rows[3]["id"])
	}
}

// TestSessionListSentinels #27 sentinels 数组填充（b3-spec #27：每会话哨兵
// id/role/alive，B1 恒空数组半成品闭合；b3-plan B3-5 扩展测试）：alive/dead 混合
// 夹具按哨兵 id 序全列；无哨兵会话=空数组非 null；活性阈值=服务配置
// sentinel_timeout_sec（config.Default 15s，注入时钟推算两档）。
func TestSessionListSentinels(t *testing.T) {
	h, st := newTestEnv(t)
	pid, cid := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	s1 := seedSessionRow(t, st, pid, cid, "worker-A", "executor", "2026-01-01T07:59:00Z", "2026-01-01T07:00:00Z")
	s2 := seedSessionRow(t, st, pid, cid, "worker-B", "worker", "2026-01-01T07:59:00Z", "2026-01-01T07:01:00Z")

	alive := seedSentinel(t, st, s1, cid, "executor", "2026-01-01T07:59:50Z")  // 差 10s ≤15 → alive
	dead := seedSentinel(t, st, s1, cid, "controller", "2026-01-01T07:00:00Z") // 差 3600s >15 → dead
	seedSentinel(t, st, s2, cid, "executor", "2026-01-01T07:00:00Z")           // worker-B 挂一只 dead

	// 时钟注入点 injectFixedClock（B3-8 收敛完成：withClock 旧名已删，本文件历史
	// 用例调用点机械替换，签名与注入语义零变化——两 helper 实现逐行同型）。
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	// 请求者=probe-S（中间件隐式注册第三会话，无哨兵——空数组断言面）。
	rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/sessions", "", "p-a", "05", "probe-S", "executor")
	rows := decodeRows(t, wantData(t, rr, http.StatusOK)["sessions"], "data.sessions")
	if len(rows) != 3 {
		t.Fatalf("sessions 行数 = %d, want 3（worker-A/worker-B/probe 自身）", len(rows))
	}

	// worker-A：双哨兵混合全列，按 id 序（alive 先插 id 小在前），id/role/alive 三键面。
	var rowA map[string]any
	for _, row := range rows {
		if row["name"] == "worker-A" {
			rowA = row
		}
	}
	if rowA == nil {
		t.Fatalf("缺 worker-A 行: %v", rows)
	}
	sen := decodeRows(t, rowA["sentinels"], "worker-A.sentinels")
	if len(sen) != 2 {
		t.Fatalf("worker-A sentinels = %d 行, want 2（alive+dead 混合全列）: %v", len(sen), sen)
	}
	wantKeys(t, sen[0], "id", "role", "alive")
	if sen[0]["id"] != float64(alive) || sen[0]["role"] != "executor" || sen[0]["alive"] != true {
		t.Errorf("sentinels[0] = %v, want {id:%d, executor, alive:true}（id 序+活性判定）", sen[0], alive)
	}
	if sen[1]["id"] != float64(dead) || sen[1]["role"] != "controller" || sen[1]["alive"] != false {
		t.Errorf("sentinels[1] = %v, want {id:%d, controller, alive:false}", sen[1], dead)
	}

	// worker-B：恰一只（dead）。
	var rowB map[string]any
	for _, row := range rows {
		if row["name"] == "worker-B" {
			rowB = row
		}
	}
	if rowB == nil {
		t.Fatalf("缺 worker-B 行: %v", rows)
	}
	senB := decodeRows(t, rowB["sentinels"], "worker-B.sentinels")
	if len(senB) != 1 || senB[0]["alive"] != false {
		t.Errorf("worker-B sentinels = %v, want 恰 1 只 dead", senB)
	}

	// probe 自身（中间件隐式注册）：无哨兵=空数组非 null。
	var rowP map[string]any
	for _, row := range rows {
		if row["name"] == "probe-S" {
			rowP = row
		}
	}
	if rowP == nil {
		t.Fatalf("缺 probe-S 行: %v", rows)
	}
	if senP, ok := rowP["sentinels"].([]any); !ok || len(senP) != 0 {
		t.Errorf("probe-S sentinels = %v, want 空数组（无哨兵非 null）", rowP["sentinels"])
	}
}

// TestSessionListProjectFilter #27 可选 project 过滤（项目 code）：命中只列该项目
// 会话；archived 项目命中→空数组非 null（其会话被 §7.2 active 过滤排除）；未登记
// 项目→404 project_not_found（store 哨兵映射）。
func TestSessionListProjectFilter(t *testing.T) {
	h, st := newTestEnv(t)
	pidA, cidA := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	pidB, cidB := seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "06", store.ColumnStatusActive)
	pidArch, cidArch := seedProjectColumn(t, st, "p-arch", store.ProjectStatusArchived, "c1", store.ColumnStatusActive)
	seedSessionRow(t, st, pidA, cidA, "worker-A", "worker", "2026-01-01T07:59:00Z", "2026-01-01T07:00:00Z")
	seedSessionRow(t, st, pidB, cidB, "worker-B", "worker", "2026-01-01T07:59:00Z", "2026-01-01T07:01:00Z")
	seedSessionRow(t, st, pidArch, cidArch, "worker-arch", "worker", "2026-01-01T07:59:00Z", "2026-01-01T07:02:00Z")

	injectFixedClock(t, "2026-01-01T08:00:00Z")
	// 过滤命中：同四元组稳态调用（首调已注册，此后不重复落审计）。
	rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/sessions?project=p-a", "", "p-a", "05", "worker-A", "executor")
	rows := decodeRows(t, wantData(t, rr, http.StatusOK)["sessions"], "data.sessions")
	if len(rows) != 1 || rows[0]["name"] != "worker-A" {
		t.Fatalf("project=p-a 过滤行数/首行 = (%d, %v)，期望恰 worker-A", len(rows), rows)
	}

	// archived 项目命中→空数组非 null（§7.2 归档项目会话不列，过滤本身合法）。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/sessions?project=p-arch", "", "p-a", "05", "worker-A", "executor")
	rows = decodeRows(t, wantData(t, rr, http.StatusOK)["sessions"], "data.sessions")
	if len(rows) != 0 {
		t.Errorf("project=p-arch 行数 = %d，期望 0（archived 项目会话不列）", len(rows))
	}

	// 未登记项目→404 project_not_found。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/sessions?project=nope", "", "p-a", "05", "worker-A", "executor")
	wantErrBody(t, rr, http.StatusNotFound, types.CodeProjectNotFound)
}

// TestSessionListRequiresHeaders #27 四头豁免面演进（B5-T1 裁定：#27 列入看板
// 六端点可选豁免——无四头直接放行不 upsert，看板②区数据口路径）：原「无四头
// 400 missing_header」断言是 B1 时代 #27 不在豁免名单的前提，被本批裁定推翻。
// 演进断言：无四头 → 200 空列表（豁免直达 handler），且 sessions 零新增
// （缺头放行不 upsert——与 TestHeaderExempt ① 同判据，此处锚定 #27 单点）。
func TestSessionListRequiresHeaders(t *testing.T) {
	h, st := newTestEnv(t)

	rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/sessions", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("无四头 GET /api/v1/sessions status = %d, want 200（B5-T1 可选豁免；body: %s）",
			rr.Code, rr.Body.String())
	}
	rows := decodeRows(t, wantData(t, rr, http.StatusOK)["sessions"], "data.sessions")
	if len(rows) != 0 {
		t.Errorf("无四头空库 sessions 行数 = %d, want 0", len(rows))
	}
	if n := countTable(t, st, "sessions"); n != 0 {
		t.Errorf("无四头请求后 sessions 表行数 = %d, want 0（缺头放行不 upsert）", n)
	}
}

// ---- #28 GET /api/v1/audit ----

// TestAuditList 操作留痕查询口（§2.2 #28，AC1.4/AC2.4 机械核验）：带四头调用走
// 中间件；无过滤=id 倒序全量（含自身首调 auto_register 落痕——核验口自证 AC1.4
// 「含操作会话身份+服务端时间」）；字段面 7 键齐（值=audit_log 三弱关联 id，
// §3.2 表 10 数据面）；project/action 单独与组合过滤；无匹配=空数组非 null。
func TestAuditList(t *testing.T) {
	h, st := newTestEnv(t)
	pidA, cidA := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	pidB, _ := seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "06", store.ColumnStatusActive)

	// 审计夹具 4 行（created_at 递增；r2/r4 会话弱关联 7/9 为非 0 直插值）。
	r1 := seedAuditRow(t, st, pidA, cidA, 0, "project.register", `{"after":{"code":"p-a"}}`, "2026-01-01T07:00:00Z")
	r2 := seedAuditRow(t, st, pidA, cidA, 7, "column.register", `{"after":{"code":"05"}}`, "2026-01-01T07:01:00Z")
	r3 := seedAuditRow(t, st, pidB, 0, 7, "project.register", "{}", "2026-01-01T07:02:00Z")
	r4 := seedAuditRow(t, st, pidA, cidA, 9, "session.auto_register", "{}", "2026-01-01T07:03:00Z")

	injectFixedClock(t, "2026-01-01T08:00:00Z")
	// 预热+核验闭环：首次带四头调用触发 auditor 自身隐式注册（落第 5 行
	// auto_register）；同四元组后续查询为稳态心跳，不再新增审计行。
	const query = "/api/v1/audit"
	doAuthedReq(t, h, http.MethodGet, query, "", "p-a", "05", "auditor", "executor")
	sess := querySingleSession(t, st)

	// 无过滤：5 行 id 倒序，最新=auditor 自身 auto_register（AC1.4 核验口可查）。
	rr := doAuthedReq(t, h, http.MethodGet, query, "", "p-a", "05", "auditor", "executor")
	rows := decodeRows(t, wantData(t, rr, http.StatusOK)["entries"], "data.entries")
	if len(rows) != 5 {
		t.Fatalf("entries 行数 = %d，期望 5（4 夹具+auditor 自身 auto_register）", len(rows))
	}
	// 首行=auditor 自身 auto_register（预热时落库，审计行 id 恒大于 4 条夹具行）；
	// 其余按夹具 id 倒序。
	topID, _ := rows[0]["id"].(float64)
	if topID <= float64(r4) {
		t.Errorf("entries[0].id = %v, want 大于最新夹具行 %d（auditor auto_register 最新在前）", rows[0]["id"], r4)
	}
	wantOrder := []float64{float64(r4), float64(r3), float64(r2), float64(r1)}
	for i, want := range wantOrder {
		if got, _ := rows[i+1]["id"].(float64); got != want {
			t.Errorf("entries[%d].id = %v, want %v（id 倒序，最新在前）", i+1, rows[i+1]["id"], want)
		}
	}
	top := rows[0]
	wantKeys(t, top, "id", "action", "session", "project", "column", "detail", "created_at")
	if top["action"] != "session.auto_register" {
		t.Errorf("entries[0].action = %v, want session.auto_register（自身注册留痕可查）", top["action"])
	}
	if top["session"] != float64(sess.ID) || top["project"] != float64(pidA) || top["column"] != float64(cidA) {
		t.Errorf("entries[0] 弱关联 = (%v, %v, %v), want (%d, %d, %d)（操作会话身份+实体对齐）",
			top["session"], top["project"], top["column"], sess.ID, pidA, cidA)
	}
	if top["created_at"] != "2026-01-01T08:00:00Z" {
		t.Errorf("entries[0].created_at = %v, want 注入时钟（AC1.4 服务端时间）", top["created_at"])
	}

	// project 过滤（项目 code）：p-a 夹具 3 行+auditor 自身 1 行。
	rr = doAuthedReq(t, h, http.MethodGet, query+"?project=p-a", "", "p-a", "05", "auditor", "executor")
	rows = decodeRows(t, wantData(t, rr, http.StatusOK)["entries"], "data.entries")
	if len(rows) != 4 {
		t.Errorf("project=p-a 行数 = %d, want 4", len(rows))
	}

	// action 过滤（精确匹配）。
	rr = doAuthedReq(t, h, http.MethodGet, query+"?action=project.register", "", "p-a", "05", "auditor", "executor")
	rows = decodeRows(t, wantData(t, rr, http.StatusOK)["entries"], "data.entries")
	if len(rows) != 2 {
		t.Errorf("action=project.register 行数 = %d, want 2", len(rows))
	}

	// 组合过滤。
	rr = doAuthedReq(t, h, http.MethodGet, query+"?project=p-b&action=project.register", "", "p-a", "05", "auditor", "executor")
	rows = decodeRows(t, wantData(t, rr, http.StatusOK)["entries"], "data.entries")
	if len(rows) != 1 || rows[0]["id"] != float64(r3) {
		t.Errorf("project=p-b&action=project.register = (%d 行, 首行 id %v), want (1, %d)",
			len(rows), rows[0]["id"], r3)
	}

	// 无匹配 → 空数组非 null。
	rr = doAuthedReq(t, h, http.MethodGet, query+"?action=no-such.action", "", "p-a", "05", "auditor", "executor")
	rows = decodeRows(t, wantData(t, rr, http.StatusOK)["entries"], "data.entries")
	if len(rows) != 0 {
		t.Errorf("无匹配 action 行数 = %d, want 0（空数组）", len(rows))
	}
}

// TestAuditListLimit #28 limit 参数：缺省=100（store AuditLimitDefault）、显式
// 超界 clamp 到 1000（AuditLimitMax 数据面收敛）。
func TestAuditListLimit(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	seedBulkAudit(t, st, 1001) // 夹具 1001 行

	injectFixedClock(t, "2026-01-01T08:00:00Z")
	// 预热（+1 行 auto_register，总 1002）后同四元组稳态查询。
	doAuthedReq(t, h, http.MethodGet, "/api/v1/audit", "", "p-a", "05", "auditor", "executor")

	// 缺省 limit → 100（默认值经 handler 传参保证，§2.2 #28「默认 100」）。
	rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/audit", "", "p-a", "05", "auditor", "executor")
	rows := decodeRows(t, wantData(t, rr, http.StatusOK)["entries"], "data.entries")
	if len(rows) != 100 {
		t.Errorf("缺省 limit 行数 = %d, want 100", len(rows))
	}

	// 显式超界 → clamp 1000（「最大 1000」为收敛语义非报错，store AuditLimitMax）；
	// 1000=恰好边界（clamp 条件为严格大于，不收缩仍返 1000）。
	for _, raw := range []string{"2000", "1001", "1000"} {
		rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/audit?limit="+raw, "", "p-a", "05", "auditor", "executor")
		rows = decodeRows(t, wantData(t, rr, http.StatusOK)["entries"], "data.entries")
		if len(rows) != 1000 {
			t.Errorf("limit=%s 行数 = %d, want 1000（clamp 到 AuditLimitMax）", raw, len(rows))
		}
	}

	// 显式小值照常生效。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/audit?limit=1", "", "p-a", "05", "auditor", "executor")
	rows = decodeRows(t, wantData(t, rr, http.StatusOK)["entries"], "data.entries")
	if len(rows) != 1 {
		t.Errorf("limit=1 行数 = %d, want 1", len(rows))
	}
}

// TestAuditListInvalidLimit #28 limit 非法值 → 400 param_invalid：非数字与显式
// ≤0 均拒绝（防「看似成功实未生效」，对齐 #2 负阈值拒绝先例）。
func TestAuditListInvalidLimit(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	for _, raw := range []string{"abc", "0", "-3"} {
		rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/audit?limit="+raw, "",
			"p-a", "05", "auditor", "executor")
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	}
}

// TestAuditListProjectNotFound #28 过滤项目未登记 → 404 project_not_found
// （对齐 #27 过滤语义，错误信息指明实体）。
func TestAuditListProjectNotFound(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/audit?project=nope", "",
		"p-a", "05", "auditor", "executor")

	wantErrBody(t, rr, http.StatusNotFound, types.CodeProjectNotFound)
}

// TestAuditListRequiresHeaders #28 无四头 → 心跳中间件 400 missing_header
// （端点挂 /api/v1/ 前缀下的中间件走查）。
func TestAuditListRequiresHeaders(t *testing.T) {
	h, _ := newTestEnv(t)

	rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/audit", "")

	wantErrBody(t, rr, http.StatusBadRequest, types.CodeMissingHeader)
}
