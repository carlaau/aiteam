// handler_resources_test.go —— B4-2 资源域三端点契约测试（#18 登记/#19 注销/#20 查询）。
// 形态：httptest+临时真库（循 server_test.go）；身份头/错误码一律字面量（契约黑盒，
// 零依赖实现侧符号）；时钟经 types.NowUTC 包级注入点固定（仅串行用例，并发用例禁用）。
package server

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"aiteam/internal/config"
	"aiteam/internal/store"
	"aiteam/internal/types"
)

// apiResNow 资源端点测试注入时钟（≠夹具时间戳，验证落库/响应/审计均为注入值而非墙钟）。
const apiResNow = "2026-04-05T06:07:08Z"

// newResTestServer httptest+临时目录真文件库装配完整骨架，另返回 *store.Store
// 供夹具直插与审计/落库断言直查（WAL 语义需真文件，禁 :memory:）。
func newResTestServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开临时 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewServer(st, config.Default(), Version), st
}

// injectFixedClock 已由 handler_sentinels_test.go 提供带参版本（B3 rebase 收敛：
// 超集签名 injectFixedClock(t, at)，本域调用统一传 apiResNow，语义不变）。

// resAPIFixtures 资源端点夹具实体 id 集：两项目/两栏目/一会话（身份四头对应 proj-a/05/controller-a）。
type resAPIFixtures struct {
	projA  int64
	projB  int64
	colA05 int64
	colB05 int64
	sessA  int64
}

// seedResAPIFixtures 直插资源端点夹具行（projects/columns/sessions——绕开上层 API
// 保证 HTTP 层测试零依赖；循 store/resources_test.go 直插模式）。
func seedResAPIFixtures(t *testing.T, st *store.Store) resAPIFixtures {
	t.Helper()
	insert := func(query string, args ...any) int64 {
		t.Helper()
		res, err := st.DB.Exec(query, args...)
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
	fx := resAPIFixtures{}
	fx.projA = insert(`INSERT INTO projects (code, name, status, created_at, updated_at) VALUES ('proj-a', '项目A', 'active', ?, ?)`, ts, ts)
	fx.projB = insert(`INSERT INTO projects (code, name, status, created_at, updated_at) VALUES ('proj-b', '项目B', 'active', ?, ?)`, ts, ts)
	fx.colA05 = insert(`INSERT INTO columns (project_id, code, name, created_at, updated_at) VALUES (?, '05', 'A05', ?, ?)`, fx.projA, ts, ts)
	fx.colB05 = insert(`INSERT INTO columns (project_id, code, name, created_at, updated_at) VALUES (?, '05', 'B05', ?, ?)`, fx.projB, ts, ts)
	fx.sessA = insert(`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at) VALUES (?, ?, 'controller-a', 'controller', ?, ?)`, fx.projA, fx.colA05, ts, ts)
	return fx
}

// seedRes 经 store 直登一条在用资源（#19/#20 用例数据准备，避免依赖 #18 HTTP 链路）。
func seedRes(t *testing.T, st *store.Store, sessID, projectID, columnID int64, rtype, value string) int64 {
	t.Helper()
	id, err := st.RegisterResource(projectID, columnID, rtype, value, "", sessID, apiResNow)
	if err != nil {
		t.Fatalf("准备资源 %s=%s 失败: %v", rtype, value, err)
	}
	return id
}

// doReqHeaders 执行一轮带自定义头的请求（headers 可为 nil），返回 recorder 供断言。
func doReqHeaders(t *testing.T, h http.Handler, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// resIdentity 标准身份四头（夹具域 proj-a/05/controller-a；Role 头本域不参与定位，
// 带齐验证「四头存在不报错」的宽容面）。
func resIdentity() map[string]string {
	return map[string]string{
		"X-Aiteam-Project": "proj-a",
		"X-Aiteam-Column":  "05",
		"X-Aiteam-Session": "controller-a",
		"X-Aiteam-Role":    "controller",
	}
}

// queryAudit 查最近一条指定 action 的审计行（session_id/created_at/detail）。
func queryAudit(t *testing.T, st *store.Store, action string) (sessionID int64, createdAt, detail string) {
	t.Helper()
	err := st.DB.QueryRow(
		`SELECT session_id, created_at, detail FROM audit_log WHERE action = ? ORDER BY id DESC LIMIT 1`, action,
	).Scan(&sessionID, &createdAt, &detail)
	if err != nil {
		t.Fatalf("查询审计行 action=%s 失败: %v", action, err)
	}
	return sessionID, createdAt, detail
}

// decodeResources 解析 #20 响应 data.resources 为条目映射切片（空数组与非数组在此区分）。
func decodeResources(t *testing.T, rr *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	m := decodeMap(t, rr)
	data, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 data 对象: %s", rr.Body.String())
	}
	items, ok := data["resources"].([]any) // null 反序列化后类型断言失败，借此锚定「空数组非 null」口径
	if !ok {
		t.Fatalf("data.resources 非数组（null 亦不合格）: %s", rr.Body.String())
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		mItem, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("data.resources 条目非对象: %v", it)
		}
		out = append(out, mItem)
	}
	return out
}

// apiResValues 提取 #20 响应条目的 value 集合（无序）。
func apiResValues(t *testing.T, rr *httptest.ResponseRecorder) []string {
	t.Helper()
	items := decodeResources(t, rr)
	vals := make([]string, 0, len(items))
	for _, it := range items {
		vals = append(vals, it["value"].(string))
	}
	return vals
}

// assertResValues 无序断言清单条目 value 集合。
func assertResValues(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	slices.Sort(got)
	wantSorted := slices.Clone(want)
	slices.Sort(wantSorted)
	if !slices.Equal(got, wantSorted) {
		t.Errorf("%s = %v, want %v", what, got, wantSorted)
	}
}

// TestRegister §2.2 #18 POST /api/v1/resources：
// 201 字段齐/409 冲突对象三要素/400 value_invalid/404 column_not_found/401 身份缺失/bad_json。
func TestRegister(t *testing.T) {
	t.Run("登记成功_201字段齐_落库与审计齐", func(t *testing.T) {
		injectFixedClock(t, apiResNow)
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)

		body := []byte(`{"column":"05","type":"port","value":"8080","note":"网关端口"}`)
		rr := doReqHeaders(t, h, http.MethodPost, "/api/v1/resources", body, resIdentity())

		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201（body: %s）", rr.Code, rr.Body.String())
		}
		m := decodeMap(t, rr)
		data, ok := m["data"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺 data 对象: %s", rr.Body.String())
		}
		if got := data["id"].(float64); got <= 0 {
			t.Errorf("data.id = %v, want 正数", got)
		}
		id := int64(data["id"].(float64))
		if got := data["type"]; got != "port" {
			t.Errorf("data.type = %v, want port", got)
		}
		if got := data["value"]; got != "8080" {
			t.Errorf("data.value = %v, want 8080", got)
		}
		if got := data["column"]; got != "05" {
			t.Errorf("data.column = %v, want 05", got)
		}
		if got := data["project"]; got != "proj-a" {
			t.Errorf("data.project = %v, want proj-a", got)
		}
		if got := data["note"]; got != "网关端口" {
			t.Errorf("data.note = %v, want 网关端口", got)
		}
		if got := data["status"]; got != "in_use" {
			t.Errorf("data.status = %v, want in_use", got)
		}
		if got := data["created_at"]; got != apiResNow {
			t.Errorf("data.created_at = %v, want 注入时钟 %q", got, apiResNow)
		}

		// 落库断言：created_by=会话 id（身份来自 X-Aiteam-Session 头，非请求体）、created_at=注入时钟
		var createdBy int64
		var createdAt string
		if err := st.DB.QueryRow(`SELECT created_by, created_at FROM resources WHERE id = ?`, id).
			Scan(&createdBy, &createdAt); err != nil {
			t.Fatalf("查询资源行失败: %v", err)
		}
		if createdBy != fx.sessA {
			t.Errorf("resources.created_by = %d, want 会话 id %d", createdBy, fx.sessA)
		}
		if createdAt != apiResNow {
			t.Errorf("resources.created_at = %q, want 注入时钟 %q", createdAt, apiResNow)
		}

		// 审计断言：resource.register 落行，含会话身份+注入时钟
		auditSess, auditAt, detail := queryAudit(t, st, "resource.register")
		if auditSess != fx.sessA {
			t.Errorf("audit.session_id = %d, want 会话 id %d", auditSess, fx.sessA)
		}
		if auditAt != apiResNow {
			t.Errorf("audit.created_at = %q, want 注入时钟 %q", auditAt, apiResNow)
		}
		if !strings.Contains(detail, "8080") {
			t.Errorf("audit.detail 缺登记 value 快照: %s", detail)
		}
	})

	t.Run("冲突_409_message指明冲突对象", func(t *testing.T) {
		injectFixedClock(t, apiResNow)
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)

		// 先经 store 直登在用资源（绕开 #18 HTTP 链路，聚焦冲突映射面）
		conflictID, err := st.RegisterResource(fx.projA, fx.colA05, "port", "8080", "在用占位", fx.sessA, apiResNow)
		if err != nil {
			t.Fatalf("准备在用资源失败: %v", err)
		}

		body := []byte(`{"column":"05","type":"port","value":"8080","note":"撞车登记"}`)
		rr := doReqHeaders(t, h, http.MethodPost, "/api/v1/resources", body, resIdentity())

		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409（body: %s）", rr.Code, rr.Body.String())
		}
		m := decodeMap(t, rr)
		errObj, ok := m["error"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺 error 对象: %s", rr.Body.String())
		}
		if got := errObj["code"]; got != "resource_conflict" {
			t.Errorf("error.code = %v, want resource_conflict", got)
		}
		// message 三要素：项目 code/栏目 code/冲突资源 id
		wantMsg := fmt.Sprintf("与 proj-a/05 在用的 resource#%d 冲突", conflictID)
		if got := errObj["message"]; got != wantMsg {
			t.Errorf("error.message = %q, want %q", got, wantMsg)
		}
	})

	t.Run("value非法_400", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		body := []byte(`{"column":"05","type":"port","value":"abc"}`)
		rr := doReqHeaders(t, h, http.MethodPost, "/api/v1/resources", body, resIdentity())

		wantErrBody(t, rr, http.StatusBadRequest, "value_invalid")
	})

	t.Run("栏目不存在_404", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		body := []byte(`{"column":"99","type":"port","value":"8080"}`)
		rr := doReqHeaders(t, h, http.MethodPost, "/api/v1/resources", body, resIdentity())

		wantErrBody(t, rr, http.StatusNotFound, "column_not_found")
	})

	t.Run("会话头缺失_400", func(t *testing.T) {
		// B1 合并对齐：缺头拦截归心跳中间件（§7.1 流程①），口径=400 missing_header
		// 逐个指明缺失头名；auth_required 401 是 token 场景错误码，与此无关。
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		headers := resIdentity()
		delete(headers, "X-Aiteam-Session")
		body := []byte(`{"column":"05","type":"port","value":"8080"}`)
		rr := doReqHeaders(t, h, http.MethodPost, "/api/v1/resources", body, headers)

		wantErrBody(t, rr, http.StatusBadRequest, "missing_header")
	})

	t.Run("头齐跨项目新会话_中间件隐式注册_201", func(t *testing.T) {
		// B1 合并对齐（原「头齐但会话组合查无_401」）：心跳中间件 upsert 会话
		// 首调=隐式注册（AC12.1/AC12.3），四头齐+实体存在即放行——handler 的
		// ResolveSession 401 分支降级为防御性（中间件保证三元组已建，不可达）。
		// controller-a 在 proj-a/05，不在 proj-b/05：本例 proj-b/05 请求由中间件
		// 当场建会话，登记成功且 created_by=新建会话 id。
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)

		headers := resIdentity()
		headers["X-Aiteam-Project"] = "proj-b"
		body := []byte(`{"column":"05","type":"port","value":"8080"}`)
		rr := doReqHeaders(t, h, http.MethodPost, "/api/v1/resources", body, headers)

		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201（AC12.1 隐式注册后登记成功；body: %s）", rr.Code, rr.Body.String())
		}
		var createdBy int64
		if err := st.DB.QueryRow(`SELECT created_by FROM resources WHERE value = '8080' AND status = 'in_use'`).Scan(&createdBy); err != nil {
			t.Fatalf("查询登记行失败: %v", err)
		}
		if createdBy == fx.sessA {
			t.Errorf("created_by = %d, want ≠ proj-a 侧会话 %d（隐式注册新建 proj-b 会话）", createdBy, fx.sessA)
		}
	})

	t.Run("坏JSON体_400", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		rr := doReqHeaders(t, h, http.MethodPost, "/api/v1/resources", []byte(`{"column":`), resIdentity())

		wantErrBody(t, rr, http.StatusBadRequest, "bad_json")
	})
}

// TestRelease §2.2 #19 DELETE /api/v1/resources/{id}：
// released+released_at=注入时钟/404/审计落行/释放后同值再登记成功（AC4.3）。
func TestRelease(t *testing.T) {
	t.Run("释放成功_released_at为注入时钟_audit落行", func(t *testing.T) {
		injectFixedClock(t, apiResNow)
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		id := seedRes(t, st, fx.sessA, fx.projA, fx.colA05, "port", "8080")

		rr := doReqHeaders(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/resources/%d", id), nil, resIdentity())

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		m := decodeMap(t, rr)
		data, ok := m["data"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺 data 对象: %s", rr.Body.String())
		}
		if got := data["status"]; got != "released" {
			t.Errorf("data.status = %v, want released", got)
		}
		if got := data["released_at"]; got != apiResNow {
			t.Errorf("data.released_at = %v, want 注入时钟 %q", got, apiResNow)
		}

		// 落库断言：status/released_at 均为注入时钟产物
		var status, releasedAt string
		if err := st.DB.QueryRow(`SELECT status, released_at FROM resources WHERE id = ?`, id).
			Scan(&status, &releasedAt); err != nil {
			t.Fatalf("查询资源行失败: %v", err)
		}
		if status != "released" || releasedAt != apiResNow {
			t.Errorf("库内行 status=%q released_at=%q, want released/%s", status, releasedAt, apiResNow)
		}

		// 审计断言：resource.release 落行，含操作会话身份+注入时钟
		auditSess, auditAt, _ := queryAudit(t, st, "resource.release")
		if auditSess != fx.sessA {
			t.Errorf("audit.session_id = %d, want 会话 id %d", auditSess, fx.sessA)
		}
		if auditAt != apiResNow {
			t.Errorf("audit.created_at = %q, want 注入时钟 %q", auditAt, apiResNow)
		}
	})

	t.Run("资源不存在_404", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		rr := doReqHeaders(t, h, http.MethodDelete, "/api/v1/resources/999", nil, resIdentity())

		wantErrBody(t, rr, http.StatusNotFound, "resource_not_found")
	})

	t.Run("释放后再登记同值成功_AC4.3", func(t *testing.T) {
		injectFixedClock(t, apiResNow)
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		id := seedRes(t, st, fx.sessA, fx.projA, fx.colA05, "port", "8080")
		if err := st.ReleaseResource(id, fx.sessA, apiResNow); err != nil {
			t.Fatalf("准备已释放资源失败: %v", err)
		}

		// 同值再登记走 HTTP 端到端：released 不再参与冲突判定
		body := []byte(`{"column":"05","type":"port","value":"8080"}`)
		rr := doReqHeaders(t, h, http.MethodPost, "/api/v1/resources", body, resIdentity())

		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201（AC4.3 释放后同值可再登记；body: %s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("会话头缺失_400", func(t *testing.T) {
		// B1 合并对齐：缺头拦截归心跳中间件，口径=400 missing_header。
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		id := seedRes(t, st, fx.sessA, fx.projA, fx.colA05, "port", "8080")

		headers := resIdentity()
		delete(headers, "X-Aiteam-Session")
		rr := doReqHeaders(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/resources/%d", id), nil, headers)

		wantErrBody(t, rr, http.StatusBadRequest, "missing_header")
	})

	t.Run("重复release幂等_响应本次时钟_库留首次_审计不重复", func(t *testing.T) {
		// B4-2 规格审查改进 3：幂等口径固化。store 侧重复释放零副作用（不覆盖
		// released_at、不重复落审计）；HTTP 面仍 200 released。
		// 已知差异（按现实现锚定）：响应 released_at=本次调用时钟——handler 不回查库，
		// 响应时间戳仅表意「本次请求被接受」，幂等真相（首次释放时刻）以库内行为准。
		injectFixedClock(t, apiResNow) // 首释时钟=apiResNow
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		id := seedRes(t, st, fx.sessA, fx.projA, fx.colA05, "port", "8080")

		// 首释。
		target := fmt.Sprintf("/api/v1/resources/%d", id)
		rr1 := doReqHeaders(t, h, http.MethodDelete, target, nil, resIdentity())
		if rr1.Code != http.StatusOK {
			t.Fatalf("首释 status = %d, want 200（body: %s）", rr1.Code, rr1.Body.String())
		}

		// 换时钟后二次 DELETE：响应 released_at=本次时钟（apiResLater，≠首释）。
		const apiResLater = "2026-04-05T06:07:09Z"
		types.NowUTC = func() string { return apiResLater }
		rr2 := doReqHeaders(t, h, http.MethodDelete, target, nil, resIdentity())
		if rr2.Code != http.StatusOK {
			t.Fatalf("二次释放 status = %d, want 200 幂等（body: %s）", rr2.Code, rr2.Body.String())
		}
		m := decodeMap(t, rr2)
		data, ok := m["data"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺 data 对象: %s", rr2.Body.String())
		}
		if got := data["status"]; got != "released" {
			t.Errorf("二次释放 data.status = %v, want released", got)
		}
		if got := data["released_at"]; got != apiResLater {
			t.Errorf("二次释放 data.released_at = %v, want 本次时钟 %q", got, apiResLater)
		}

		// 库内真相：released_at 保留首释时刻，审计恰 1 条（零副作用）。
		var releasedAt string
		if err := st.DB.QueryRow(`SELECT released_at FROM resources WHERE id = ?`, id).Scan(&releasedAt); err != nil {
			t.Fatalf("查询资源行失败: %v", err)
		}
		if releasedAt != apiResNow {
			t.Errorf("库内 released_at = %q, want 保留首释时刻 %q", releasedAt, apiResNow)
		}
		var auditCnt int
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'resource.release'`).Scan(&auditCnt); err != nil {
			t.Fatalf("统计释放审计失败: %v", err)
		}
		if auditCnt != 1 {
			t.Errorf("resource.release 审计条数 = %d, want 1（重复释放不重复落审计）", auditCnt)
		}
	})
}

// TestList §2.2 #20 GET /api/v1/resources：三过滤组合/默认排除 released（AC4.1）/
// 空清单口径固定为空数组（非 null）/project 不存在=空集（读口宽容）。
func TestList(t *testing.T) {
	t.Run("project_type_组合三过滤", func(t *testing.T) {
		injectFixedClock(t, apiResNow)
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		seedRes(t, st, fx.sessA, fx.projA, fx.colA05, "port", "8080")
		seedRes(t, st, fx.sessA, fx.projA, fx.colA05, "account_range", "acct:1-9")
		seedRes(t, st, fx.sessA, fx.projB, fx.colB05, "port", "9090")

		// project 过滤（此用例带全四头，验证读口「四头存在不报错」宽容面）
		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/resources?project=proj-a", nil, resIdentity())
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		assertResValues(t, "project=proj-a", apiResValues(t, rr), "8080", "acct:1-9")

		// type 过滤（B1 合并对齐：心跳中间件四头必填，读口不豁免——豁免面归 B5 裁量）
		rr = doReqHeaders(t, h, http.MethodGet, "/api/v1/resources?type=port", nil, resIdentity())
		assertResValues(t, "type=port", apiResValues(t, rr), "8080", "9090")

		// project+type 组合
		rr = doReqHeaders(t, h, http.MethodGet, "/api/v1/resources?project=proj-a&type=port", nil, resIdentity())
		assertResValues(t, "project=proj-a&type=port", apiResValues(t, rr), "8080")
	})

	t.Run("默认排除released_AC4.1_include_released含", func(t *testing.T) {
		injectFixedClock(t, apiResNow)
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		id := seedRes(t, st, fx.sessA, fx.projA, fx.colA05, "port", "8080")
		seedRes(t, st, fx.sessA, fx.projA, fx.colA05, "port", "8081")
		if err := st.ReleaseResource(id, fx.sessA, apiResNow); err != nil {
			t.Fatalf("准备已释放资源失败: %v", err)
		}

		// 默认：排除 released
		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/resources", nil, resIdentity())
		assertResValues(t, "默认清单", apiResValues(t, rr), "8081")

		// include_released=true：含 released，条目带 released_at
		rr = doReqHeaders(t, h, http.MethodGet, "/api/v1/resources?include_released=true", nil, resIdentity())
		assertResValues(t, "含已释放清单", apiResValues(t, rr), "8080", "8081")
		for _, it := range decodeResources(t, rr) {
			if it["value"] == "8080" {
				if got := it["status"]; got != "released" {
					t.Errorf("released 条目 status = %v, want released", got)
				}
				if got := it["released_at"]; got != apiResNow {
					t.Errorf("released 条目 released_at = %v, want %q", got, apiResNow)
				}
			}
		}
	})

	t.Run("空清单_空数组非null", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/resources", nil, resIdentity())

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		// 口径固定：200 + data.resources=[]（decodeResources 对 null 反序列化直接 Fatalf）
		if got := decodeResources(t, rr); len(got) != 0 {
			t.Errorf("空清单条目数 = %d, want 0", len(got))
		}
	})

	t.Run("project不存在_空集", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/resources?project=no-such", nil, resIdentity())

		// 读口宽容裁定：过滤条件「不存在的项目」选不出任何行 → 200 空集（口径固定）
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		if got := decodeResources(t, rr); len(got) != 0 {
			t.Errorf("不存在项目过滤条目数 = %d, want 0", len(got))
		}
	})
}

// TestConcurrentRegister AC4.2 并发面：20 goroutine 并发同值登记 → 恰 1×201 + 19×409。
// 正确性归因：store 单连接池（MaxOpenConns(1)）令登记事务整体串行，冲突判定与 INSERT
// 之间无穿插窗口。不注入时钟（包级替换与并发读有竞态面）；本机无 gcc 不可 -race，
// 用例照写，go test（不带 -race）执行，-race 留环境补跑。
func TestConcurrentRegister(t *testing.T) {
	h, st := newResTestServer(t)
	seedResAPIFixtures(t, st)

	const n = 20
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := []byte(`{"column":"05","type":"port","value":"7777","note":"并发登记"}`)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/resources", bytes.NewReader(body))
			req.Header.Set("X-Aiteam-Project", "proj-a")
			req.Header.Set("X-Aiteam-Column", "05")
			req.Header.Set("X-Aiteam-Session", "controller-a")
			req.Header.Set("X-Aiteam-Role", "controller")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			codes <- rr.Code
		}()
	}
	wg.Wait()
	close(codes)

	created, conflict := 0, 0
	for c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("并发登记出现意外状态码 %d", c)
		}
	}
	if created != 1 || conflict != n-1 {
		t.Errorf("并发登记结果 = %d×201 + %d×409, want 恰 1×201 + %d×409", created, conflict, n-1)
	}

	// 库内恰落一行在用资源+一条登记审计（并发无重复落行）
	var cnt int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM resources WHERE value = '7777'`).Scan(&cnt); err != nil {
		t.Fatalf("统计资源行失败: %v", err)
	}
	if cnt != 1 {
		t.Errorf("value=7777 落行数 = %d, want 1", cnt)
	}
}
