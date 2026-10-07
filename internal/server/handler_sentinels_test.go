package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aiteam/internal/config"
	"aiteam/internal/store"
	"aiteam/internal/types"
)

// fID int64 转 URL 路径十进制串（测试辅助）。
func fID(v int64) string {
	return strconv.FormatInt(v, 10)
}

// ===== B3-4 哨兵三端点测试（规格：技术设计 §2.2 #14/#15/#16、§7.1 T3 心跳链、D2 裁定、
// B3-T4 机械闸；b3-plan B3-4）=====
//
// 时钟纪律：types.NowUTC 包级注入点替换为固定值（TestPing 同款形态），
// 注册（t0）与 poll（t1=t0+5s）两档注入做「前后对比」断言，零真实墙钟。
//
// 夹具：直插 projects/columns/sessions 行（feat/b3 无 B1 中间件，会话无隐式注册路径，
// 必须真库直插造三元组），消息/位点同法直插——隔离被测面只经 HTTP API。

// 哨兵域测试固定时刻（RFC3339 UTC 秒级；t1=t0+5s 恰为 D2 默认轮询间隔一个周期，
// t2=t0+16s 越过哨兵失活阈值 15s 进入 dead 侧——复活/错配用例基准）。
const (
	sentinelT0 = "2026-10-02T13:00:00Z"
	sentinelT1 = "2026-10-02T13:00:05Z"
	sentinelT2 = "2026-10-02T13:00:16Z"
)

// newSentinelTestServer httptest + 临时真文件库，同时暴露 store 句柄供夹具直插与
// 库内状态断言（server_test.go 的 newTestServer 不返回 store，哨兵测试需要落库面）。
func newSentinelTestServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开临时 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewServer(st, config.Default(), Version), st
}

// injectFixedClock 替换 types.NowUTC 包级注入点为固定值（可叠加调用，cleanup 逆序还原）。
func injectFixedClock(t *testing.T, at string) {
	t.Helper()
	orig := types.NowUTC
	types.NowUTC = func() string { return at }
	t.Cleanup(func() { types.NowUTC = orig })
}

// doReqWithHeaders 带自定义请求头的一轮请求（哨兵身份四头直读面）。
func doReqWithHeaders(t *testing.T, h http.Handler, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// sentinelHeads 构造身份四头（总控 #10 终态口径：X-Aiteam-* 直读，测试用字面量锚定头名）。
func sentinelHeads(project, column, session, role string) map[string]string {
	return map[string]string{
		"X-Aiteam-Project": project,
		"X-Aiteam-Column":  column,
		"X-Aiteam-Session": session,
		"X-Aiteam-Role":    role,
	}
}

// executorHeads 常态身份头：proj-a/05 栏目 watch-a 会话（executor 角色）。
func executorHeads() map[string]string {
	return sentinelHeads("proj-a", "05", "watch-a", "executor")
}

// dataOf 解出响应 data 对象（缺 data 或非对象即 Fatal）。
func dataOf(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	m := decodeMap(t, rr)
	data, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 data 对象: %s", rr.Body.String())
	}
	return data
}

// fixtureSentinelDomain 直插观测域夹具链（proj-a 项目 / "05" 栏目 / watch-a 会话），
// 会话 last_seen_at=sentinelT0（T3 心跳续期断言基准）；返回三元 id。
func fixtureSentinelDomain(t *testing.T, st *store.Store) (projectID, columnID, sessionID int64) {
	t.Helper()
	mustLastInsertID := func(what, query string, args ...any) int64 {
		t.Helper()
		res, err := st.DB.Exec(query, args...)
		if err != nil {
			t.Fatalf("插入 %s 夹具失败: %v", what, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("读取 %s 自增 id 失败: %v", what, err)
		}
		return id
	}
	projectID = mustLastInsertID("projects",
		`INSERT INTO projects (code, name, created_at, updated_at) VALUES ('proj-a', '项目A', ?, ?)`,
		sentinelT0, sentinelT0)
	columnID = mustLastInsertID("columns",
		`INSERT INTO columns (project_id, code, name, created_at, updated_at) VALUES (?, '05', '栏目05', ?, ?)`,
		projectID, sentinelT0, sentinelT0)
	sessionID = mustLastInsertID("sessions",
		`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at) VALUES (?, ?, 'watch-a', 'executor', ?, ?)`,
		projectID, columnID, sentinelT0, sentinelT0)
	return projectID, columnID, sessionID
}

// insertMessage 直插一条消息（append-only 表唯一入口），返回全局 seq。
// sender=0 表示系统/看板发送（弱关联口径）。
func insertMessage(t *testing.T, st *store.Store, columnID int64, kind, targetRole string,
	targetSessionID, senderSessionID int64, level string) int64 {
	t.Helper()
	res, err := st.DB.Exec(
		`INSERT INTO messages (project_id, column_id, kind, target_role, target_session_id,
		       sender_session_id, sender_label, level, body, created_at)
		 SELECT project_id, ?, ?, ?, ?, ?, 'fixture', ?, 'hello', ? FROM columns WHERE id = ?`,
		columnID, kind, targetRole, targetSessionID, senderSessionID, level, sentinelT0, columnID)
	if err != nil {
		t.Fatalf("插入 %s 消息夹具失败: %v", kind, err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取消息 seq 失败: %v", err)
	}
	return seq
}

// insertPosition 直插一行消费位点（position 初值 0，锁定「行存在」以便 AC10.4 断言行值不被 poll 改动）。
func insertPosition(t *testing.T, st *store.Store, columnID int64, consumer string, position int64) {
	t.Helper()
	_, err := st.DB.Exec(
		`INSERT INTO ack_positions (project_id, column_id, consumer, position, updated_at)
		 SELECT project_id, ?, ?, ?, ? FROM columns WHERE id = ?`,
		columnID, consumer, position, sentinelT0, columnID)
	if err != nil {
		t.Fatalf("插入位点夹具失败: %v", err)
	}
}

// bumpPosition 推进既有位点到指定值（「已 ack 不再命中」用例的 ack 动作等效）。
func bumpPosition(t *testing.T, st *store.Store, columnID int64, consumer string, position int64) {
	t.Helper()
	res, err := st.DB.Exec(
		`UPDATE ack_positions SET position = ?, updated_at = ? WHERE column_id = ? AND consumer = ?`,
		position, sentinelT1, columnID, consumer)
	if err != nil {
		t.Fatalf("推进位点失败: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("推进位点影响行数 = %d，期望 1（consumer=%s）", n, consumer)
	}
}

// queryOneText 查询单值文本列（时间列断言用）。
func queryOneText(t *testing.T, st *store.Store, query string, args ...any) string {
	t.Helper()
	var v string
	if err := st.DB.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("查询单值失败: %v\nSQL: %s", err, query)
	}
	return v
}

// countRows 统计表行数（落行/audit 零写入断言用）。
func countRows(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("统计 %s 失败: %v", table, err)
	}
	return n
}

// registerSentinelViaAPI 经 #14 端点注册哨兵（缺省 interval），返回 sentinel_id。
func registerSentinelViaAPI(t *testing.T, h http.Handler, headers map[string]string) int64 {
	t.Helper()
	rr := doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels", nil, headers)
	if rr.Code != http.StatusCreated {
		t.Fatalf("注册哨兵 status = %d，期望 201（body: %s）", rr.Code, rr.Body.String())
	}
	data := dataOf(t, rr)
	id, ok := data["sentinel_id"].(float64)
	if !ok || id <= 0 {
		t.Fatalf("data.sentinel_id = %v，期望正数", data["sentinel_id"])
	}
	return int64(id)
}

// pollSentinelViaAPI 调 #15 端点，返回 recorder（query 与身份头同 session——watch CLI
// 正常路径形态；身份四头固定 executorHeads 域，跨会话错配用例走 pollAsSession）。
func pollSentinelViaAPI(t *testing.T, h http.Handler, id int64, role string) *httptest.ResponseRecorder {
	return pollAsSession(t, h, id, role, "watch-a")
}

// pollAsSession 以指定 query session 调 #15（query column 固定 "05"）。
func pollAsSession(t *testing.T, h http.Handler, id int64, role, session string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/sentinels/" + fID(id) + "/poll?column=05&role=" + role + "&session=" + session
	return doReqWithHeaders(t, h, http.MethodPost, target, nil, executorHeads())
}

// wantHits 断言 data.hits 与期望 (seq,level) 序列完全一致（升序），且元素恰含
// seq/level/kind/target_role 四键（b7-W1 additive 扩列；无 body 键——零载荷纪律不变）。
func wantHits(t *testing.T, rr *httptest.ResponseRecorder, want [][2]any) {
	t.Helper()
	data := dataOf(t, rr)
	raw, ok := data["hits"].([]any)
	if !ok {
		t.Fatalf("data.hits 非数组: %s", rr.Body.String())
	}
	if len(raw) != len(want) {
		t.Fatalf("hits 长度 = %d，期望 %d（body: %s）", len(raw), len(want), rr.Body.String())
	}
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("hits[%d] 非对象: %v", i, item)
		}
		// b7-W1 扩列锚更新：Hit 增 kind/target_role 两键（additive），恰四键——
		// 零载荷纪律不变（无 body）；新键值断言归 b7-W2 handler 层测试面。
		if len(m) != 4 {
			t.Errorf("hits[%d] 键数 = %d，期望恰 seq/level/kind/target_role 四键（实际: %v）", i, len(m), m)
		}
		seq, _ := m["seq"].(float64)
		if int64(seq) != want[i][0].(int64) {
			t.Errorf("hits[%d].seq = %v，期望 %v", i, seq, want[i][0])
		}
		if lv, _ := m["level"].(string); lv != want[i][1].(string) {
			t.Errorf("hits[%d].level = %v，期望 %v", i, m["level"], want[i][1])
		}
	}
}

// wantDataEmpty 断言 data 存在且为空对象（#16 data:{} 契约）。
func wantDataEmpty(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	data := dataOf(t, rr)
	if len(data) != 0 {
		t.Errorf("data = %v，期望空对象 {}", data)
	}
}

// TestSentinelRegister §2.2 #14：201 {sentinel_id,interval_sec}；B3-T4 机械闸=哨兵失活阈值口径
// （3×interval ≤ sentinel_timeout_sec=15，恰等 5s 放行、6s 拒——`>` 严格大于才拒）；
// 注册幂等（信箱#12）：同 (column,role,session) 已有活哨兵 → 200 既有 id 不新建，
// 死哨兵 → 201 复活（upsert 刷新时间列——B3-T2）；显式非正值→400；
// 未登记栏目→404 column_not_found；Session 头缺失/Role 头缺失→400 missing_header、
// ghost 会话→201（B1 心跳中间件终态口径——缺头拦/隐式注册，见子用例注释）。
func TestSentinelRegister(t *testing.T) {
	h, st := newSentinelTestServer(t)
	_, _, _ = fixtureSentinelDomain(t, st)
	injectFixedClock(t, sentinelT0)

	// 1) 显式 interval_sec=5：3×5=15 ≤ 15 恰等边界放行（B3-T4 对哨兵失活阈值）→ 201，
	//    interval_sec 回显生效值 5。
	rr := doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels",
		[]byte(`{"interval_sec":5}`), executorHeads())
	if rr.Code != http.StatusCreated {
		t.Fatalf("恰等注册 status = %d，期望 201（body: %s）", rr.Code, rr.Body.String())
	}
	data := dataOf(t, rr)
	firstID, ok := data["sentinel_id"].(float64)
	if !ok || firstID <= 0 {
		t.Fatalf("data.sentinel_id = %v，期望正数", data["sentinel_id"])
	}
	if got := data["interval_sec"]; got != float64(5) {
		t.Errorf("data.interval_sec = %v，期望回显 5", got)
	}
	if n := countRows(t, st, "sentinels"); n != 1 {
		t.Errorf("注册后 sentinels 行数 = %d，期望 1", n)
	}

	// 2) 注册幂等（信箱#12）：活哨兵在（恰等边界 alive），同 (column,role,session) 再注册
	//    （缺省 interval）→ 200 返回既有 sentinel_id 不新建 + 行数不增。
	rr = doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels", nil, executorHeads())
	if rr.Code != http.StatusOK {
		t.Fatalf("幂等注册 status = %d，期望 200（body: %s）", rr.Code, rr.Body.String())
	}
	data = dataOf(t, rr)
	if got, ok := data["sentinel_id"].(float64); !ok || int64(got) != int64(firstID) {
		t.Errorf("幂等注册 sentinel_id = %v，期望既有 id %d", data["sentinel_id"], int64(firstID))
	}
	if got := data["interval_sec"]; got != float64(5) {
		t.Errorf("幂等注册 data.interval_sec = %v，期望回显本次生效值 5", got)
	}
	if n := countRows(t, st, "sentinels"); n != 1 {
		t.Errorf("幂等注册后 sentinels 行数 = %d，期望仍 1", n)
	}

	// 3) B3-T4 机械闸：3×6=18 > 哨兵失活阈值 15 → 400 param_invalid
	//    （60s 才 ping 一次在 15s 阈值下恒 dead——规格要堵的真缺口）。
	rr = doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels",
		[]byte(`{"interval_sec":6}`), executorHeads())
	wantErrBody(t, rr, http.StatusBadRequest, "param_invalid")

	// 4) 显式 interval_sec=0：非法（0 无法表达轮询节奏）→ 400 param_invalid。
	rr = doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels",
		[]byte(`{"interval_sec":0}`), executorHeads())
	wantErrBody(t, rr, http.StatusBadRequest, "param_invalid")

	// 5) 未登记栏目（Column 头在 project 域不存在）→ 404 column_not_found。
	badCol := sentinelHeads("proj-a", "99", "watch-a", "executor")
	rr = doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels", nil, badCol)
	wantErrBody(t, rr, http.StatusNotFound, "column_not_found")

	// 6) Session 头缺失 → 400 missing_header（B1 合并对齐：缺头拦截归心跳
	//    中间件 §7.1 流程①；auth_required 401 是 token 场景，与此无关）。
	noSession := sentinelHeads("proj-a", "05", "", "executor")
	rr = doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels", nil, noSession)
	wantErrBody(t, rr, http.StatusBadRequest, "missing_header")

	// 7) ghost 会话 → 201（B1 合并对齐：心跳中间件 upsert 首调=隐式注册
	//    AC12.1/AC12.3，四头齐+实体在即当场建会话放行；handler 层查无会话
	//    的 401 分支降级为防御性，中间件保证三元组已建）。
	ghost := sentinelHeads("proj-a", "05", "ghost", "executor")
	rr = doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels", nil, ghost)
	if rr.Code != http.StatusCreated {
		t.Fatalf("ghost 会话注册 status = %d, want 201（AC12.1 隐式注册；body: %s）", rr.Code, rr.Body.String())
	}
	// 隐式注册真实落行：sessions 表出现 watch-a 同坐标的 ghost 行。
	var ghostCnt int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE name = 'ghost'`).Scan(&ghostCnt); err != nil {
		t.Fatalf("查询 ghost 会话失败: %v", err)
	}
	if ghostCnt != 1 {
		t.Errorf("ghost 会话行数 = %d, want 1（中间件隐式注册落行）", ghostCnt)
	}

	// 8) Role 头缺失（watch 目标信箱角色不完整）→ 400 missing_header
	//    （B1 合并对齐：Role 同属身份四头，缺失由心跳中间件 §7.1 流程①先拦；
	//    handler 层的 param_invalid 分支降级为防御性——中间件保证四头非空）。
	noRole := sentinelHeads("proj-a", "05", "watch-a", "")
	rr = doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels", nil, noRole)
	wantErrBody(t, rr, http.StatusBadRequest, "missing_header")

	// 9) 死哨兵复活：时钟推进 16s（哨兵失活 15s 阈值外）后同坐标再注册 →
	//    无活哨兵命中 → 201（upsert 复活同 id——B3-T2 kill 重启语义）+ 行数仍 1
	//    （按 watch-a 坐标计——ghost 子用例的隐式注册已给全表多落一行）。
	injectFixedClock(t, sentinelT2)
	rr = doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels", nil, executorHeads())
	if rr.Code != http.StatusCreated {
		t.Fatalf("死哨兵复活注册 status = %d，期望 201（body: %s）", rr.Code, rr.Body.String())
	}
	if got, ok := dataOf(t, rr)["sentinel_id"].(float64); !ok || int64(got) != int64(firstID) {
		t.Errorf("复活注册 sentinel_id = %v，期望复用同 id %d（upsert）", got, int64(firstID))
	}
	var watchACnt int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sentinels se JOIN sessions s ON s.id = se.session_id WHERE s.name = 'watch-a'`).Scan(&watchACnt); err != nil {
		t.Fatalf("统计 watch-a 哨兵失败: %v", err)
	}
	if watchACnt != 1 {
		t.Errorf("复活注册后 watch-a 坐标哨兵行数 = %d，期望仍 1", watchACnt)
	}
}

// TestPollPingAndHeartbeat §2.2 #15 T3 三合一（双续期断言）：poll 后 sentinels.last_ping_at
// 前进（哨兵 ping）且 sessions.last_seen_at 同步前进（会话心跳续期——「哨兵活着=会话活着」，
// §7.3 双向隔离的正向面）；now 回显注入时钟；不存在 id → 404 sentinel_not_found。
func TestPollPingAndHeartbeat(t *testing.T) {
	h, st := newSentinelTestServer(t)
	_, _, sessionID := fixtureSentinelDomain(t, st)
	injectFixedClock(t, sentinelT0)
	id := registerSentinelViaAPI(t, h, executorHeads())

	// 前置基准：两时间列均为 t0。
	if got := queryOneText(t, st, `SELECT last_ping_at FROM sentinels WHERE id = ?`, id); got != sentinelT0 {
		t.Fatalf("前置 last_ping_at = %s，期望 %s", got, sentinelT0)
	}
	if got := queryOneText(t, st, `SELECT last_seen_at FROM sessions WHERE id = ?`, sessionID); got != sentinelT0 {
		t.Fatalf("前置 last_seen_at = %s，期望 %s", got, sentinelT0)
	}

	// poll（时钟推进到 t1）。
	injectFixedClock(t, sentinelT1)
	rr := pollSentinelViaAPI(t, h, id, "executor")
	if rr.Code != http.StatusOK {
		t.Fatalf("poll status = %d，期望 200（body: %s）", rr.Code, rr.Body.String())
	}
	data := dataOf(t, rr)
	if got := data["now"]; got != sentinelT1 {
		t.Errorf("data.now = %v，期望注入时钟 %s", got, sentinelT1)
	}

	// T3 双续期断言：哨兵 ping 与会话心跳同步前进到 t1。
	if got := queryOneText(t, st, `SELECT last_ping_at FROM sentinels WHERE id = ?`, id); got != sentinelT1 {
		t.Errorf("poll 后 last_ping_at = %s，期望前进到 %s（哨兵 ping）", got, sentinelT1)
	}
	if got := queryOneText(t, st, `SELECT last_seen_at FROM sessions WHERE id = ?`, sessionID); got != sentinelT1 {
		t.Errorf("poll 后 last_seen_at = %s，期望同步前进到 %s（T3 会话心跳续期）", got, sentinelT1)
	}

	// 不存在 id → 404 sentinel_not_found。
	rr = pollSentinelViaAPI(t, h, 999, "executor")
	wantErrBody(t, rr, http.StatusNotFound, "sentinel_not_found")
}

// TestPollWrongOwner 哨兵-会话绑定（审查 I1）：持 A 会话身份 poll B 会话的哨兵 id →
// 404 sentinel_not_found，且 B 的哨兵 ping 与 A 的会话心跳双双不动（T3 双续期不得跨会话
// 错配——ping 刷 B、心跳续 A 的破洞堵实证据）。
func TestPollWrongOwner(t *testing.T) {
	h, st := newSentinelTestServer(t)
	_, columnID, sessionA := fixtureSentinelDomain(t, st)
	// 第二会话 watcher-b（同项目同栏目，watch 独立身份）。
	var sessionB int64
	if err := st.DB.QueryRow(
		`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at)
		 VALUES ((SELECT project_id FROM columns WHERE id = ?), ?, 'watch-b', 'executor', ?, ?)
		 RETURNING id`,
		columnID, columnID, sentinelT0, sentinelT0).Scan(&sessionB); err != nil {
		t.Fatalf("插入第二会话夹具失败: %v", err)
	}

	injectFixedClock(t, sentinelT0)
	idB := registerSentinelViaAPI(t, h, sentinelHeads("proj-a", "05", "watch-b", "executor"))

	// A 会话身份 poll B 的哨兵 → 404（归属绑定校验，事务回滚零写入）。
	injectFixedClock(t, sentinelT1)
	rr := pollAsSession(t, h, idB, "executor", "watch-a")
	wantErrBody(t, rr, http.StatusNotFound, "sentinel_not_found")

	// 零写断言（B1 合并对齐）：handler 层归属校验拒绝零写——B 哨兵 last_ping_at
	// 与 B 会话 last_seen_at 均停留 t0；A 会话（请求者）last_seen_at=t1 是心跳
	// 中间件的正当续期（§7.1 在 handler 之前刷请求者心跳，AC12.1），与本测试
	// 验证的 handler 层零写正交。
	if got := queryOneText(t, st, `SELECT last_ping_at FROM sentinels WHERE id = ?`, idB); got != sentinelT0 {
		t.Errorf("错配 poll 后 B 哨兵 last_ping_at = %s，期望不动 %s", got, sentinelT0)
	}
	if got := queryOneText(t, st, `SELECT last_seen_at FROM sessions WHERE id = ?`, sessionA); got != sentinelT1 {
		t.Errorf("错配 poll 后 A 会话 last_seen_at = %s，期望中间件续期至 %s", got, sentinelT1)
	}
	// B 会话自身心跳也不应被 A 的请求续期。
	if got := queryOneText(t, st, `SELECT last_seen_at FROM sessions WHERE id = ?`, sessionB); got != sentinelT0 {
		t.Errorf("错配 poll 后 B 会话 last_seen_at = %s，期望不动 %s", got, sentinelT0)
	}
}

// TestPollHits §3.6 谓词三分支（direct/bus 仅 controller/chat 对话流）：
// 位点之后新消息 → hits=[{seq,level}] 升序（无 body 键）；已 ack 消息不再命中；
// 自己的回复不回流；非 controller 不命中 bus；AC10.4 位点前后相等（探测纯读不推进）。
func TestPollHits(t *testing.T) {
	h, st := newSentinelTestServer(t)
	_, columnID, sessionID := fixtureSentinelDomain(t, st)
	injectFixedClock(t, sentinelT0)
	id := registerSentinelViaAPI(t, h, executorHeads())

	// 位点三行（信箱 executor/controller + 对话流），初值 0 锁定行存在性。
	insertPosition(t, st, columnID, "executor", 0)
	insertPosition(t, st, columnID, "controller", 0)
	insertPosition(t, st, columnID, "chat:session:"+fID(sessionID), 0)

	// 消息面（空表自增 seq 依插入序 = 1..7）：
	seq1 := insertMessage(t, st, columnID, "direct", "executor", 0, 0, "important")    // 命中
	seq2 := insertMessage(t, st, columnID, "bus", "", 0, 0, "normal")                  // 仅 controller
	seq3 := insertMessage(t, st, columnID, "chat", "", sessionID, 0, "normal")         // 命中（看板/系统侧）
	seq4 := insertMessage(t, st, columnID, "chat", "", sessionID, sessionID, "normal") // 自己回复不回流
	seq5 := insertMessage(t, st, columnID, "direct", "executor", 0, 0, "block")        // 命中
	seq6 := insertMessage(t, st, columnID, "receipt", "", sessionID, 0, "normal")      // 命中（回告进对话流）
	seq7 := insertMessage(t, st, columnID, "direct", "tester", 0, 0, "normal")         // 其他角色不命中
	// seq4（自己的回复）与 seq7（其他角色）构造后不进任何 want 断言——命中面排除项。
	_ = seq4
	_ = seq7

	// executor 视角第一轮（位点全 0）：direct(seq1,5) + chat/receipt(seq3,6) 升序；
	// bus(seq2) 与他人角色(seq7) 不命中；自己的回复(seq4) 不回流。
	injectFixedClock(t, sentinelT1)
	rr := pollSentinelViaAPI(t, h, id, "executor")
	if rr.Code != http.StatusOK {
		t.Fatalf("poll status = %d，期望 200（body: %s）", rr.Code, rr.Body.String())
	}
	wantHits(t, rr, [][2]any{
		{seq1, "important"}, {seq3, "normal"}, {seq5, "block"}, {seq6, "normal"},
	})

	// AC10.4：hits 探测纯读——三个位点值前后相等。
	for _, c := range []struct {
		consumer string
		want     int64
	}{
		{"executor", 0}, {"controller", 0}, {"chat:session:" + fID(sessionID), 0},
	} {
		if got := queryOneText(t, st,
			`SELECT position FROM ack_positions WHERE column_id = ? AND consumer = ?`,
			columnID, c.consumer); got != fID(c.want) {
			t.Errorf("位点 %s poll 后 = %s，期望不变 = %d（AC10.4 不推进）", c.consumer, got, c.want)
		}
	}

	// controller 视角：bus(seq2) 追加命中（bus 仅 controller——§3.6 谓词 :r='controller'
	// 分支）；chat/receipt 分支与 role 无关（对话流总会话可见），seq3/seq6 照常命中。
	rr = pollSentinelViaAPI(t, h, id, "controller")
	wantHits(t, rr, [][2]any{{seq2, "normal"}, {seq3, "normal"}, {seq6, "normal"}})

	// 已 ack 消息不再命中：executor 位点推到 1、对话位点推到 3 后，
	// seq1/seq3 退出命中面，仅剩 seq5/seq6。
	bumpPosition(t, st, columnID, "executor", 1)
	bumpPosition(t, st, columnID, "chat:session:"+fID(sessionID), 3)
	rr = pollSentinelViaAPI(t, h, id, "executor")
	wantHits(t, rr, [][2]any{{seq5, "block"}, {seq6, "normal"}})
}

// TestPollEmptyAndNotFound：无新消息 hits=[]（空数组非 null）+now 回显；
// {id} 非数字 → 404（sentinel_not_found 口径——语义上即无此哨兵）。
func TestPollEmptyAndNotFound(t *testing.T) {
	h, st := newSentinelTestServer(t)
	_, columnID, _ := fixtureSentinelDomain(t, st)
	injectFixedClock(t, sentinelT0)
	id := registerSentinelViaAPI(t, h, executorHeads())

	// 无新消息（位点推到当前最大 seq 之上）→ hits=[] + now=t1。
	insertPosition(t, st, columnID, "executor", 999)
	injectFixedClock(t, sentinelT1)
	rr := pollSentinelViaAPI(t, h, id, "executor")
	if rr.Code != http.StatusOK {
		t.Fatalf("空轮 poll status = %d，期望 200（body: %s）", rr.Code, rr.Body.String())
	}
	data := dataOf(t, rr)
	raw, ok := data["hits"].([]any)
	if !ok {
		t.Fatalf("data.hits 非数组: %s", rr.Body.String())
	}
	if len(raw) != 0 {
		t.Errorf("hits = %v，期望空数组 []", raw)
	}
	if got := data["now"]; got != sentinelT1 {
		t.Errorf("data.now = %v，期望 %s", got, sentinelT1)
	}

	// {id} 非数字 → 404 sentinel_not_found（不 500 不 panic）。
	rr = doReqWithHeaders(t, h, http.MethodPost,
		"/api/v1/sentinels/abc/poll?column=05&role=executor&session=watch-a", nil, executorHeads())
	wantErrBody(t, rr, http.StatusNotFound, "sentinel_not_found")

	// 缺参校验（审查 I2）：query 任一参数缺失 → 400 param_invalid（空串落谓词会让
	// direct/bus 永不命中被 200 掩盖；也收口空串进三元组 401 消息的插值混乱）。
	for _, target := range []string{
		"/api/v1/sentinels/" + fID(id) + "/poll?role=executor&session=watch-a", // 缺 column
		"/api/v1/sentinels/" + fID(id) + "/poll?column=05&session=watch-a",     // 缺 role
		"/api/v1/sentinels/" + fID(id) + "/poll?column=05&role=executor",       // 缺 session
	} {
		rr = doReqWithHeaders(t, h, http.MethodPost, target, nil, executorHeads())
		wantErrBody(t, rr, http.StatusBadRequest, "param_invalid")
	}
}

// TestDelete §2.2 #16：成功 data:{}；重复删除 → 404 幂等语义（store 固化口径）；
// {id} 非数字 → 404；哨兵注册/poll/注销全程 audit_log 零写入（契约未列哨兵审计面）。
func TestDelete(t *testing.T) {
	h, st := newSentinelTestServer(t)
	_, _, _ = fixtureSentinelDomain(t, st)
	injectFixedClock(t, sentinelT0)
	id := registerSentinelViaAPI(t, h, executorHeads())

	// 成功注销：200 + data:{}。
	rr := doReqWithHeaders(t, h, http.MethodDelete, "/api/v1/sentinels/"+fID(id), nil, executorHeads())
	if rr.Code != http.StatusOK {
		t.Fatalf("注销 status = %d，期望 200（body: %s）", rr.Code, rr.Body.String())
	}
	wantDataEmpty(t, rr)
	if n := countRows(t, st, "sentinels"); n != 0 {
		t.Errorf("注销后 sentinels 行数 = %d，期望 0", n)
	}

	// 重复注销 → 404 幂等语义（效果已达成，非错误分支——b3-spec #16 口径）。
	rr = doReqWithHeaders(t, h, http.MethodDelete, "/api/v1/sentinels/"+fID(id), nil, executorHeads())
	wantErrBody(t, rr, http.StatusNotFound, "sentinel_not_found")

	// {id} 非数字 → 404。
	rr = doReqWithHeaders(t, h, http.MethodDelete, "/api/v1/sentinels/xyz", nil, executorHeads())
	wantErrBody(t, rr, http.StatusNotFound, "sentinel_not_found")

	// 哨兵全生命周期（注册+注销+重复注销）audit_log 零写入——契约未列哨兵审计面。
	if n := countRows(t, st, "audit_log"); n != 0 {
		t.Errorf("audit_log 行数 = %d，期望 0（哨兵操作不入审计）", n)
	}
}

// TestPollHitsKindTargetRoleWire b7-W2 #15 additive 新键值锚（AC3）：hits 元素
// 恰四键且 kind/target_role 随三分支带出结构值（direct=目标角色原值；
// bus/chat/receipt 空串照实回显非省略——零载荷纪律不变，无 body）。#15 handler
// 零改动（[]store.Hit 直透），本测试锚即 W2 交付面。
func TestPollHitsKindTargetRoleWire(t *testing.T) {
	h, st := newSentinelTestServer(t)
	_, columnID, sessionID := fixtureSentinelDomain(t, st)
	injectFixedClock(t, sentinelT0)
	id := registerSentinelViaAPI(t, h, executorHeads())
	insertPosition(t, st, columnID, "executor", 0)
	insertPosition(t, st, columnID, "controller", 0)
	insertPosition(t, st, columnID, "chat:session:"+fID(sessionID), 0)

	seqDirect := insertMessage(t, st, columnID, "direct", "executor_A", 0, 0, "important")
	seqChat := insertMessage(t, st, columnID, "chat", "", sessionID, 0, "normal")
	seqReceipt := insertMessage(t, st, columnID, "receipt", "", sessionID, 0, "normal")
	seqBus := insertMessage(t, st, columnID, "bus", "", 0, 0, "normal")

	// executor_A 视角（哨兵归属绑 session=watch-a，role 仅为命中谓词参数——
	// pollAsSession 传目标角色）：direct+chat+receipt 三分支命中，
	// kind/target_role 逐元素核对。
	injectFixedClock(t, sentinelT1)
	rr := pollSentinelViaAPI(t, h, id, "executor_A")
	if rr.Code != http.StatusOK {
		t.Fatalf("poll status = %d，期望 200（body: %s）", rr.Code, rr.Body.String())
	}
	hits, ok := dataOf(t, rr)["hits"].([]any)
	if !ok {
		t.Fatalf("data.hits 非数组: %s", rr.Body.String())
	}
	if len(hits) != 3 {
		t.Fatalf("hits 长度 = %d，期望 3: %s", len(hits), rr.Body.String())
	}
	want := []struct {
		seq        float64
		kind       string
		targetRole string
	}{
		{float64(seqDirect), "direct", "executor_A"},
		{float64(seqChat), "chat", ""},
		{float64(seqReceipt), "receipt", ""},
	}
	for i, w := range want {
		m, ok := hits[i].(map[string]any)
		if !ok {
			t.Fatalf("hits[%d] 非对象: %v", i, hits[i])
		}
		if len(m) != 4 {
			t.Errorf("hits[%d] 键数 = %d，期望恰四键: %v", i, len(m), m)
		}
		if seq, _ := m["seq"].(float64); seq != w.seq {
			t.Errorf("hits[%d].seq = %v，期望 %v", i, m["seq"], w.seq)
		}
		if got := m["kind"]; got != w.kind {
			t.Errorf("hits[%d].kind = %v，期望 %s", i, got, w.kind)
		}
		if got := m["target_role"]; got != w.targetRole {
			t.Errorf("hits[%d].target_role = %v，期望 %q", i, got, w.targetRole)
		}
	}

	// controller 视角：bus 命中 target_role=''（空串照实回显）。
	rr = pollSentinelViaAPI(t, h, id, "controller")
	hits, ok = dataOf(t, rr)["hits"].([]any)
	if !ok {
		t.Fatalf("controller 视角 data.hits 非数组: %s", rr.Body.String())
	}
	if len(hits) != 3 {
		t.Fatalf("controller 视角 hits 长度 = %d，期望 3（bus+chat+receipt）", len(hits))
	}
	m := hits[2].(map[string]any) // bus seq 最大（direct/chat/receipt=seq1..3），升序居末
	if seq, _ := m["seq"].(float64); seq != float64(seqBus) {
		t.Errorf("controller 视角 hits[2].seq = %v，期望 %d（bus 升序居末）", m["seq"], seqBus)
	}
	if got := m["kind"]; got != "bus" {
		t.Errorf("controller 视角 hits[2].kind = %v，期望 bus", got)
	}
	if got := m["target_role"]; got != "" {
		t.Errorf("controller 视角 hits[2].target_role = %v，期望空串", got)
	}
}

// ===== b8-W2/W3 handler 测试：#14 force 接管+持有者诊断 / #15 命中留痕 =====

// TestSentinelRegisterHolderDiagnostics b8-W2①（AC4 服务面）：幂等命中（200）响应
// 增 holder 诊断 {session,last_ping_at,age_sec}——session=持有者会话名、last_ping_at=
// 既有行 ping 时刻、age_sec=now−last_ping_at（固定钟 t0 注册/t1 再注册恰 5s）。
// sentinel_id/interval_sec 既有键不动（AC6 回归面）。
func TestSentinelRegisterHolderDiagnostics(t *testing.T) {
	h, st := newSentinelTestServer(t)
	_, _, _ = fixtureSentinelDomain(t, st)
	injectFixedClock(t, sentinelT0)
	firstID := registerSentinelViaAPI(t, h, executorHeads()) // 201 新建

	injectFixedClock(t, sentinelT1) // +5s，仍在 15s 失活阈值内 → 幂等命中
	rr := doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels", nil, executorHeads())
	if rr.Code != http.StatusOK {
		t.Fatalf("幂等注册 status = %d，期望 200（body: %s）", rr.Code, rr.Body.String())
	}
	data := dataOf(t, rr)
	if got, ok := data["sentinel_id"].(float64); !ok || int64(got) != firstID {
		t.Errorf("幂等注册 sentinel_id = %v，期望既有 id %d", data["sentinel_id"], firstID)
	}
	holder, ok := data["holder"].(map[string]any)
	if !ok {
		t.Fatalf("200 响应缺 holder 对象: %s", rr.Body.String())
	}
	if len(holder) != 3 {
		t.Errorf("holder 键数 = %d，期望恰 session/last_ping_at/age_sec 三键（实际: %v）", len(holder), holder)
	}
	if got, _ := holder["session"].(string); got != "watch-a" {
		t.Errorf("holder.session = %q，期望 watch-a（持有者会话名）", got)
	}
	if got, _ := holder["last_ping_at"].(string); got != sentinelT0 {
		t.Errorf("holder.last_ping_at = %q，期望 %s", got, sentinelT0)
	}
	if got, _ := holder["age_sec"].(float64); int64(got) != 5 {
		t.Errorf("holder.age_sec = %v，期望 5（t1−t0）", holder["age_sec"])
	}
}

// TestSentinelRegisterForceTakeover b8-W2②（AC5 服务面）：活哨兵在且 force=true →
// 同事务旧删新插 201（新 id，行数仍 1）；旧 id 下次 poll 404 sentinel_not_found；
// 死行 force → 既有注册路径不变（upsert 复活同 id，force no-op）；无行 force →
// 普通注册 201。
func TestSentinelRegisterForceTakeover(t *testing.T) {
	h, st := newSentinelTestServer(t)
	_, _, _ = fixtureSentinelDomain(t, st)
	injectFixedClock(t, sentinelT0)
	oldID := registerSentinelViaAPI(t, h, executorHeads()) // 201

	// 活哨兵在（t1=+5s）+ force → 201 接管，新 id。
	injectFixedClock(t, sentinelT1)
	forceBody := []byte(`{"force":true}`)
	rr := doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels", forceBody, executorHeads())
	if rr.Code != http.StatusCreated {
		t.Fatalf("force 接管 status = %d，期望 201（body: %s）", rr.Code, rr.Body.String())
	}
	newID := int64(dataOf(t, rr)["sentinel_id"].(float64))
	if newID <= 0 || newID == oldID {
		t.Fatalf("force 接管 sentinel_id = %d，期望新 id（≠旧 id %d）", newID, oldID)
	}
	if n := countRows(t, st, "sentinels"); n != 1 {
		t.Fatalf("接管后 sentinels 行数 = %d，期望 1", n)
	}

	// 旧哨兵 id 下次 poll → 404 sentinel_not_found（旧孤儿自然退出锚）。
	rr = pollSentinelViaAPI(t, h, oldID, "executor")
	wantErrBody(t, rr, http.StatusNotFound, "sentinel_not_found")

	// 新 id poll 正常（接管后值守无缝）。
	rr = pollSentinelViaAPI(t, h, newID, "executor")
	if rr.Code != http.StatusOK {
		t.Fatalf("新 id poll status = %d，期望 200（body: %s）", rr.Code, rr.Body.String())
	}

	// 死行 force → 既有注册路径（upsert 复活同 id）：时钟推到 t1+16s（>15s 阈值），
	// FindAliveSentinel 未命中 → force no-op 走 RegisterSentinel。
	injectFixedClock(t, "2026-10-02T13:00:21Z")
	rr = doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels", forceBody, executorHeads())
	if rr.Code != http.StatusCreated {
		t.Fatalf("死行 force 注册 status = %d，期望 201（body: %s）", rr.Code, rr.Body.String())
	}
	if got := int64(dataOf(t, rr)["sentinel_id"].(float64)); got != newID {
		t.Errorf("死行 force sentinel_id = %d，期望复活同 id %d（既有注册路径不变）", got, newID)
	}

	// 无行 force → 普通注册 201：先注销再 force 注册。
	rr = doReqWithHeaders(t, h, http.MethodDelete, "/api/v1/sentinels/"+fID(newID), nil, executorHeads())
	if rr.Code != http.StatusOK {
		t.Fatalf("注销 status = %d，期望 200", rr.Code)
	}
	rr = doReqWithHeaders(t, h, http.MethodPost, "/api/v1/sentinels", forceBody, executorHeads())
	if rr.Code != http.StatusCreated {
		t.Fatalf("无行 force 注册 status = %d，期望 201（body: %s）", rr.Code, rr.Body.String())
	}
	if n := countRows(t, st, "sentinels"); n != 1 {
		t.Errorf("无行 force 注册后行数 = %d，期望 1", n)
	}
}

// TestPollMarksSentinelLastHit b8-W3/AC7 服务面：哨兵 poll 非空 hits → 该会话行
// sentinel_last_hit_at=now（命中即记会话行，不记哨兵行）；未命中会话保持空串；
// #17 响应 SessionEntry.sentinel_last_hit_at 两态透出；留痕 UPDATE 失败（注入故障=
// 删列，poll 主链三查均不触该列）→ #15 仍 200 且 hits 完整（降级不阻塞）。
func TestPollMarksSentinelLastHit(t *testing.T) {
	h, st := newSentinelTestServer(t)
	_, columnID, _ := fixtureSentinelDomain(t, st)
	// 第二会话 watch-b（executor_b 角色——合法三形态；direct 消息定向 executor
	// 精确匹配不命中它→poll 恒空轮=未命中对照面）。
	if _, err := st.DB.Exec(
		`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at)
		 SELECT project_id, ?, 'watch-b', 'executor_b', ?, ? FROM columns WHERE id = ?`,
		columnID, sentinelT0, sentinelT0, columnID); err != nil {
		t.Fatalf("插入 watch-b 夹具失败: %v", err)
	}
	seq := insertMessage(t, st, columnID, "direct", "executor", 0, 0, "normal")

	injectFixedClock(t, sentinelT0)
	idA := registerSentinelViaAPI(t, h, executorHeads()) // watch-a/executor
	bHeads := sentinelHeads("proj-a", "05", "watch-b", "executor_b")
	idB := registerSentinelViaAPI(t, h, bHeads)

	// watch-a 命中 poll → 留痕=now。
	rr := pollSentinelViaAPI(t, h, idA, "executor")
	if rr.Code != http.StatusOK {
		t.Fatalf("watch-a poll status = %d，期望 200", rr.Code)
	}
	if got := queryOneText(t, st, `SELECT sentinel_last_hit_at FROM sessions WHERE name = 'watch-a'`); got != sentinelT0 {
		t.Errorf("watch-a sentinel_last_hit_at = %q，期望 %s", got, sentinelT0)
	}

	// watch-b 空轮 poll → 留痕保持 ''。
	rr = pollAsSession(t, h, idB, "executor_b", "watch-b")
	if rr.Code != http.StatusOK {
		t.Fatalf("watch-b poll status = %d，期望 200", rr.Code)
	}
	if got := queryOneText(t, st, `SELECT sentinel_last_hit_at FROM sessions WHERE name = 'watch-b'`); got != "" {
		t.Errorf("watch-b sentinel_last_hit_at = %q，期望空串（空轮不记）", got)
	}

	// #17 线面：SessionEntry.sentinel_last_hit_at 两态透出（命中 seq 与留痕值同验）。
	rr = doReq(t, h, http.MethodGet, "/api/v1/status?mode=column&project=proj-a&column=05", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("#17 status = %d（body: %s）", rr.Code, rr.Body.String())
	}
	raw, _ := json.Marshal(dataOf(t, rr))
	if !strings.Contains(string(raw), `"sentinel_last_hit_at":"`+sentinelT0+`"`) {
		t.Errorf("#17 响应缺 watch-a 命中留痕值: %s", raw)
	}
	ovData := dataOf(t, rr)
	projects, _ := ovData["projects"].([]any)
	found := map[string]string{}
	for _, pitem := range projects {
		pm, _ := pitem.(map[string]any)
		sessions, _ := pm["sessions"].([]any)
		for _, sitem := range sessions {
			sm, _ := sitem.(map[string]any)
			name, _ := sm["name"].(string)
			found[name], _ = sm["sentinel_last_hit_at"].(string)
		}
	}
	if found["watch-a"] != sentinelT0 {
		t.Errorf("#17 watch-a sentinel_last_hit_at = %q，期望 %s（seq=%d 命中留痕）", found["watch-a"], sentinelT0, seq)
	}
	if found["watch-b"] != "" {
		t.Errorf("#17 watch-b sentinel_last_hit_at = %q，期望空串键恒在", found["watch-b"])
	}

	// 注入故障降级：删列后 poll 主链（Lookup/ping/PollHits 均不触该列）不受留痕
	// UPDATE 失败影响——#15 仍 200 且 hits 完整。
	if _, err := st.DB.Exec(`ALTER TABLE sessions DROP COLUMN sentinel_last_hit_at`); err != nil {
		t.Fatalf("注入故障（删列）失败: %v", err)
	}
	rr = pollSentinelViaAPI(t, h, idA, "executor")
	if rr.Code != http.StatusOK {
		t.Fatalf("降级 poll status = %d，期望 200（留痕失败不阻塞主响应；body: %s）", rr.Code, rr.Body.String())
	}
	wantHits(t, rr, [][2]any{{seq, "normal"}})
}
