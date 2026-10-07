package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 本文件为进度上报域端点 #31 POST /api/v1/progress 与 #32 GET /api/v1/progress
// 的 handler 测试（§2.2 #31/#32 逐字段、b8-plan B8-2 用例面）：四头校验与上报即
// 心跳由 sessionHeartbeat 中间件达成（doAuthedReq 走完整链断言端到端行为）、
// limit clamp 与倒序为 store 数据面（夹具直插+端到端对拍）。

// seedProgressRow 直插进度上报夹具（照 handlers_misc_test 直插惯例，不依赖
// #31 端点），返回自增 id。testStatus 传空串=落 NULL（DDL CHECK 允许 NULL，
// 读回空串——store nullableText 同语义）。
func seedProgressRow(t *testing.T, st *store.Store, sessionID int64,
	batch, task, commitHash, branch, testStatus, summary, createdAt string) int64 {
	t.Helper()
	var ts any
	if testStatus != "" {
		ts = testStatus
	}
	res, err := st.DB.Exec(
		`INSERT INTO progress_reports (session_id, batch, task, commit_hash, branch, test_status, summary, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, batch, task, commitHash, branch, ts, summary, createdAt,
	)
	if err != nil {
		t.Fatalf("插入进度夹具失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取进度夹具自增 id 失败: %v", err)
	}
	return id
}

// seedBulkProgress 单条 WITH RECURSIVE 直插 n 行进度夹具（limit clamp 场景，
// 一条语句免逐行往返——夹具直插非生产代码不受往返账约束）。created_at 按序号
// 递增（'2026-01-01T00:MM:SSZ'，分秒两位补零文本序=时间序），免同秒 tie 歧义。
func seedBulkProgress(t *testing.T, st *store.Store, sessionID int64, n int) {
	t.Helper()
	_, err := st.DB.Exec(
		`INSERT INTO progress_reports (session_id, batch, task, commit_hash, branch, test_status, summary, created_at)
		 WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM seq WHERE x < ?)
		 SELECT ?, 'B8', 'B8-1', '', '', NULL, '',
		        '2026-01-01T00:' || printf('%02d', x/60) || ':' || printf('%02d', x%60) || 'Z'
		 FROM seq`, n, sessionID,
	)
	if err != nil {
		t.Fatalf("批量插入进度夹具 %d 行失败: %v", n, err)
	}
}

// lastSeenAtOf 查库取会话 last_seen_at（TestPostIsHeartbeat 数据面对拍口）。
func lastSeenAtOf(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	var got string
	if err := st.DB.QueryRow(`SELECT last_seen_at FROM sessions WHERE name = ?`, name).Scan(&got); err != nil {
		t.Fatalf("查会话 %q last_seen_at 失败: %v", name, err)
	}
	return got
}

// ---- #31 POST /api/v1/progress ----

// TestProgressPost #31 正向面（b8-plan B8-2 用例一）：201+响应恰 {id,created_at}
// 两键且 created_at=服务端注入时钟；六业务字段落库对拍（#32 端到端回读+session
// 会话名装配）；空 body {}（hook 自动层形态：batch/task 空）合法且可空字段读回
// 空串。
func TestProgressPost(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	// ① 201+字段面：六业务字段全给，created_at=注入时钟（AC5.4 服务端时钟）。
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	body := `{"batch":"B8","task":"B8-2","commit_hash":"abc123","branch":"feat/b8","test_status":"pass","summary":"两端点落地"}`
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/progress", body, "p-a", "05", "cli-B8", "executor")
	data := wantData(t, rr, http.StatusCreated)
	if len(data) != 2 {
		t.Errorf("data 键数 = %d，期望恰 2（§2.2 #31 恰 {id,created_at}）: %v", len(data), data)
	}
	if data["created_at"] != "2026-01-01T08:00:00Z" {
		t.Errorf("created_at = %v, want 注入时钟（AC5.4 不用客户端时间）", data["created_at"])
	}
	if id, ok := data["id"].(float64); !ok || id <= 0 {
		t.Errorf("id = %v, want 正整数自增 id", data["id"])
	}

	// ② 落库对拍：六业务字段+会话归属经 #32 端到端回读（session=JOIN 装配名）。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/progress", "", "p-a", "05", "cli-B8", "executor")
	rows := decodeRows(t, wantData(t, rr, http.StatusOK)["items"], "data.items")
	if len(rows) != 1 {
		t.Fatalf("回读行数 = %d, want 1", len(rows))
	}
	wantKeys(t, rows[0], "id", "session", "batch", "task", "commit_hash", "branch", "test_status", "summary", "created_at")
	for k, want := range map[string]any{
		"session": "cli-B8", "batch": "B8", "task": "B8-2",
		"commit_hash": "abc123", "branch": "feat/b8", "test_status": "pass",
		"summary": "两端点落地", "created_at": "2026-01-01T08:00:00Z",
	} {
		if got := rows[0][k]; got != want {
			t.Errorf("回读 %s = %v, want %q", k, got, want)
		}
	}

	// ③ 空 body {}：hook 自动层形态（batch/task 空），201 且可空字段读回空串。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/progress", `{}`, "p-a", "05", "hook-auto", "executor")
	wantData(t, rr, http.StatusCreated)
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/progress?session=hook-auto", "", "p-a", "05", "hook-auto", "executor")
	rows = decodeRows(t, wantData(t, rr, http.StatusOK)["items"], "data.items")
	if len(rows) != 1 {
		t.Fatalf("hook 行回读行数 = %d, want 1", len(rows))
	}
	for _, k := range []string{"batch", "task", "commit_hash", "branch", "test_status", "summary"} {
		if got, _ := rows[0][k].(string); got != "" {
			t.Errorf("hook 行 %s = %q, want 空串（空 body 未提供落 NULL 读回空串）", k, got)
		}
	}
}

// TestProgressPostRejects #31 拒绝面（用例一负向段）：四头缺失 400 missing_header
// （与既有写端点同链同口径，仓内无 401 概念）；未登记项目中间件存在性短路 404；
// summary 恰 512B 边界过/超 1B 拒（按 UTF-8 字节计，多字节字符同口径）；
// test_status 非法值 400 param_invalid（DDL CHECK 之上 handler 预校验出明确 4xx，
// 免落 store 层透成 500）；非法 JSON 400 bad_json。
func TestProgressPostRejects(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	// 四头缺失：缺 project 与缺 role 各验一发（400 missing_header，中间件口径）。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/progress", `{}`, "", "05", "s", "executor")
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeMissingHeader)
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/progress", `{}`, "p-a", "05", "s", "")
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeMissingHeader)

	// 未登记项目：中间件存在性短路 404 project_not_found（AC2.2）。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/progress", `{}`, "no-such", "05", "s", "executor")
	wantErrBody(t, rr, http.StatusNotFound, types.CodeProjectNotFound)

	// summary 恰 512B 边界过 / 超 1B 拒（按 UTF-8 字节计）。
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	atLimit := fmt.Sprintf(`{"summary":"%s"}`, strings.Repeat("a", 512))
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/progress", atLimit, "p-a", "05", "s-limit", "executor")
	wantData(t, rr, http.StatusCreated)
	overLimit := fmt.Sprintf(`{"summary":"%s"}`, strings.Repeat("a", 513))
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/progress", overLimit, "p-a", "05", "s-limit", "executor")
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	// 多字节同理：256 个中文 = 768B 超限（字符数 256 未超、字节数超——按字节计锚定）。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/progress",
		fmt.Sprintf(`{"summary":"%s"}`, strings.Repeat("进", 256)), "p-a", "05", "s-limit", "executor")
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)

	// test_status 非法值：400 param_invalid（合法枚举仅 pass/fail/unknown/空缺省）。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/progress", `{"test_status":"ok"}`, "p-a", "05", "s-bad", "executor")
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)

	// 非法 JSON：400 bad_json（decodeJSONBody 统一口径）。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/progress", `not-json`, "p-a", "05", "s-bad", "executor")
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeBadJSON)
}

// TestPostIsHeartbeat 上报即心跳（§2.2 #31 中间件挂链设计点，b8-plan B8-2 用例二）：
// POST 经心跳中间件 upsert 会话刷 last_seen_at——夹具回拨 last_seen_at 模拟久未
// 上报，再 POST 断言 last_seen_at 前进到服务端时钟（直接查库对拍，AC12.1）。
func TestPostIsHeartbeat(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	// 首报隐式注册：last_seen_at=T1。
	injectFixedClock(t, "2026-01-01T10:00:00Z")
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/progress", `{}`, "p-a", "05", "worker-W", "executor")
	wantData(t, rr, http.StatusCreated)
	if got := lastSeenAtOf(t, st, "worker-W"); got != "2026-01-01T10:00:00Z" {
		t.Fatalf("注册心跳 last_seen_at = %q, want T1（首报即注册+心跳）", got)
	}

	// 夹具回拨模拟 4 小时未上报，再报：last_seen_at 前进到 T2（上报即心跳）。
	if _, err := st.DB.Exec(`UPDATE sessions SET last_seen_at = '2026-01-01T06:00:00Z' WHERE name = 'worker-W'`); err != nil {
		t.Fatalf("回拨夹具失败: %v", err)
	}
	injectFixedClock(t, "2026-01-01T12:00:00Z")
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/progress", `{"batch":"B8"}`, "p-a", "05", "worker-W", "executor")
	wantData(t, rr, http.StatusCreated)
	if got := lastSeenAtOf(t, st, "worker-W"); got != "2026-01-01T12:00:00Z" {
		t.Errorf("上报后 last_seen_at = %q, want 前进到 T2（上报即心跳，AC12.1）", got)
	}

	// 会话归属：两报同落一 sessions 行，progress.session_id 均指向它（FK 链正确）。
	var n int64
	if err := st.DB.QueryRow(`SELECT COUNT(DISTINCT session_id) FROM progress_reports`).Scan(&n); err != nil {
		t.Fatalf("查进度会话数失败: %v", err)
	}
	if n != 1 {
		t.Errorf("progress 落在 %d 个会话上, want 1（同四元组不重复注册）", n)
	}
}

// ---- #32 GET /api/v1/progress ----

// TestProgressGet #32 查询面（b8-plan B8-2 用例三）：无过滤全量倒序（created_at
// DESC,id DESC——同秒夹具由 id 决序）；project 过滤（未登记 404 project_not_found，
// #28 GetProjectByCode 换装先例）；session 过滤与组合过滤；limit 非法 400
// param_invalid、超 1000 clamp 到 1000；items 字段面恰 §2.2 #32 九键（无 stale——
// stale 仅 LatestBySession 消费面，AC23.3）；GET 同走心跳链（四头缺失 400）。
func TestProgressGet(t *testing.T) {
	h, st := newTestEnv(t)
	pidA, cidA := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	pidB, cidB := seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "06", store.ColumnStatusActive)
	sidA1 := seedSessionRow(t, st, pidA, cidA, "worker-A1", "worker", "2026-01-01T07:59:00Z", "2026-01-01T07:00:00Z")
	sidA2 := seedSessionRow(t, st, pidA, cidA, "worker-A2", "worker", "2026-01-01T07:59:00Z", "2026-01-01T07:01:00Z")
	sidB1 := seedSessionRow(t, st, pidB, cidB, "worker-B1", "worker", "2026-01-01T07:59:00Z", "2026-01-01T07:02:00Z")

	// 倒序夹具：p-a 两会话共 4 条（00:03:00 同秒两行由 id 决序）+ p-b 一条。
	seedProgressRow(t, st, sidA1, "B8", "B8-1", "", "", "", "第一条", "2026-01-01T00:01:00Z")
	seedProgressRow(t, st, sidA1, "B8", "B8-2", "", "", "fail", "同秒先插", "2026-01-01T00:03:00Z")
	seedProgressRow(t, st, sidA2, "B8", "B8-3", "", "", "", "同秒后插", "2026-01-01T00:03:00Z")
	seedProgressRow(t, st, sidA2, "B8", "B8-4", "", "", "pass", "", "2026-01-01T00:04:00Z")
	seedProgressRow(t, st, sidB1, "B8", "B8-5", "", "", "", "", "2026-01-01T00:05:00Z")

	injectFixedClock(t, "2026-01-01T08:00:00Z")

	// ① 无过滤全量倒序（跨项目全局倒序；同秒 00:03 后插 id 大在前）：
	//    B8-5(00:05) → B8-4(00:04) → B8-3(00:03,id 大) → B8-2(00:03,id 小) → B8-1(00:01)。
	rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/progress", "", "p-a", "05", "worker-A1", "executor")
	rows := decodeRows(t, wantData(t, rr, http.StatusOK)["items"], "data.items")
	if len(rows) != 5 {
		t.Fatalf("无过滤行数 = %d, want 5", len(rows))
	}
	type wantRow struct{ task, session, summary, testStatus string }
	wantSeq := []wantRow{
		{"B8-5", "worker-B1", "", ""},
		{"B8-4", "worker-A2", "", "pass"},
		{"B8-3", "worker-A2", "同秒后插", ""},
		{"B8-2", "worker-A1", "同秒先插", "fail"},
		{"B8-1", "worker-A1", "第一条", ""},
	}
	for i, w := range wantSeq {
		if len(rows[i]) != 9 {
			t.Errorf("items[%d] 键数 = %d, want 恰 9（§2.2 #32 字段面对拍）: %v", i, len(rows[i]), rows[i])
		}
		wantKeys(t, rows[i], "id", "session", "batch", "task", "commit_hash", "branch", "test_status", "summary", "created_at")
		for k, want := range map[string]string{
			"task": w.task, "session": w.session, "summary": w.summary, "test_status": w.testStatus,
		} {
			if got := rows[i][k]; got != want {
				t.Errorf("items[%d].%s = %v, want %q（created_at DESC,id DESC 倒序对拍）", i, k, got, want)
			}
		}
	}

	// ② project 过滤：只回该项目会话的进度（4 条）。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/progress?project=p-a", "", "p-a", "05", "worker-A1", "executor")
	rows = decodeRows(t, wantData(t, rr, http.StatusOK)["items"], "data.items")
	if len(rows) != 4 {
		t.Fatalf("project=p-a 行数 = %d, want 4", len(rows))
	}
	for i, r := range rows {
		if got := r["session"]; got == "worker-B1" {
			t.Errorf("items[%d] project=p-a 混入他项目会话 worker-B1", i)
		}
	}

	// ③ project 未登记：404 project_not_found（#28 换装先例）。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/progress?project=no-such", "", "p-a", "05", "worker-A1", "executor")
	wantErrBody(t, rr, http.StatusNotFound, types.CodeProjectNotFound)

	// ④ session 过滤：恰该会话行且倒序。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/progress?session=worker-A1", "", "p-a", "05", "worker-A1", "executor")
	rows = decodeRows(t, wantData(t, rr, http.StatusOK)["items"], "data.items")
	if len(rows) != 2 {
		t.Fatalf("session=worker-A1 行数 = %d, want 2", len(rows))
	}
	if got := rows[0]["task"]; got != "B8-2" {
		t.Errorf("session 过滤首行 = %v, want B8-2（会话内仍倒序）", got)
	}

	// ⑤ project+session 组合过滤：交集为空回空数组非 null。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/progress?project=p-b&session=worker-A1", "", "p-b", "06", "worker-B1", "executor")
	rows = decodeRows(t, wantData(t, rr, http.StatusOK)["items"], "data.items")
	if len(rows) != 0 {
		t.Errorf("p-b×worker-A1 组合行数 = %d, want 0（空结果非 nil 空数组）", len(rows))
	}

	// ⑥ limit 非法：非数字/≤0 → 400 param_invalid（#28 同型口径）。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/progress?limit=abc", "", "p-a", "05", "worker-A1", "executor")
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/progress?limit=0", "", "p-a", "05", "worker-A1", "executor")
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)

	// ⑦ limit clamp：另建会话批插 1001 行——不带 limit 回 100（§2.2 #32「默认
	//    100」=ProgressLimitDefault）；limit=1000 恰好回 1000；limit=2000 clamp
	//    回 1000（「最大 1000」为钳制非报错，ProgressLimitMax 数据面收敛）。
	sidC := seedSessionRow(t, st, pidA, cidA, "worker-C", "worker", "2026-01-01T07:59:00Z", "2026-01-01T07:03:00Z")
	seedBulkProgress(t, st, sidC, 1001)
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/progress?session=worker-C", "", "p-a", "05", "worker-C", "executor")
	rows = decodeRows(t, wantData(t, rr, http.StatusOK)["items"], "data.items")
	if len(rows) != 100 {
		t.Errorf("不带 limit 行数 = %d, want 100（ProgressLimitDefault）", len(rows))
	}
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/progress?session=worker-C&limit=1000", "", "p-a", "05", "worker-C", "executor")
	rows = decodeRows(t, wantData(t, rr, http.StatusOK)["items"], "data.items")
	if len(rows) != 1000 {
		t.Errorf("limit=1000 行数 = %d, want 1000（max 边界直取）", len(rows))
	}
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/progress?session=worker-C&limit=2000", "", "p-a", "05", "worker-C", "executor")
	rows = decodeRows(t, wantData(t, rr, http.StatusOK)["items"], "data.items")
	if len(rows) != 1000 {
		t.Errorf("limit=2000 行数 = %d, want 1000（ProgressLimitMax clamp）", len(rows))
	}

	// ⑧ GET 同走心跳链：四头缺失 400 missing_header（§7.1 全部 /api/v1 端点）。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/progress", "")
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeMissingHeader)
}
