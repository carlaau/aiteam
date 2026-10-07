package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// newTestServer httptest + 临时目录真文件库装配完整骨架（WAL 语义需真文件，禁 :memory:）。
// B1-4 起委托 newTestEnv（需要 store 断言/夹具的测试直接用后者）。
func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	h, _ := newTestEnv(t)
	return h
}

// doReq 执行一轮请求，返回 recorder 供状态码/头断言（body 可为 nil=空请求体）。
func doReq(t *testing.T, h http.Handler, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// decodeMap 解析响应 JSON 为通用映射（data/error 二层取值用）。
func decodeMap(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应体非合法 JSON: %v\nbody: %s", err, rr.Body.String())
	}
	return m
}

// wantErrBody 断言错误响应的 code 与 Content-Type（§2.1/B0-T1 统一 JSON 化口径）。
func wantErrBody(t *testing.T, rr *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rr.Code != status {
		t.Errorf("status = %d, want %d（body: %s）", rr.Code, status, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != types.ContentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", got, types.ContentTypeJSON)
	}
	m := decodeMap(t, rr)
	errObj, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 error 对象: %s", rr.Body.String())
	}
	if got := errObj["code"]; got != code {
		t.Errorf("error.code = %v, want %q", got, code)
	}
}

// TestPing §2.2 #29：GET /api/v1/ping → data:{version,now}；无鉴权（不带头也 200）；now 为注入的 ISO8601 UTC。
func TestPing(t *testing.T) {
	h := newTestServer(t)

	// 注入固定时间（types.NowUTC 为包级注入点），精确断言 ISO8601 UTC 文本
	origNow := types.NowUTC
	types.NowUTC = func() string { return "2026-10-02T13:04:05Z" }
	defer func() { types.NowUTC = origNow }()

	// 不带任何请求头（含无 Authorization）——ping 无鉴权，须 200
	rr := doReq(t, h, http.MethodGet, "/api/v1/ping", nil)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != types.ContentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", got, types.ContentTypeJSON)
	}
	m := decodeMap(t, rr)
	data, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 data 对象: %s", rr.Body.String())
	}
	version, _ := data["version"].(string)
	if version != Version {
		t.Errorf("data.version = %q, want %q", version, Version)
	}
	if got := data["now"]; got != "2026-10-02T13:04:05Z" {
		t.Errorf("data.now = %v, want 注入值 %q", got, "2026-10-02T13:04:05Z")
	}
}

// TestVersion §2.2 #30：GET /api/v1/version → data:{version,commit,goos}（commit 允许占位空串）。
func TestVersion(t *testing.T) {
	h := newTestServer(t)

	rr := doReq(t, h, http.MethodGet, "/api/v1/version", nil)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
	}
	m := decodeMap(t, rr)
	data, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 data 对象: %s", rr.Body.String())
	}
	if got, _ := data["version"].(string); got != Version {
		t.Errorf("data.version = %q, want %q", got, Version)
	}
	if got, ok := data["commit"].(string); !ok || got != "" {
		t.Errorf("data.commit = %v (exists=%v), want 占位空串", data["commit"], ok)
	}
	if got, _ := data["goos"].(string); got != runtime.GOOS {
		t.Errorf("data.goos = %q, want %q", got, runtime.GOOS)
	}
}

// TestNotFoundJSON 未知路径 → 404 + error.code=not_found（B0-T1 已裁：标准库纯文本 404 统一 JSON 化）。
// B1-4 心跳中间件挂入后，非豁免 /api/v1 路径要求身份四头（§7.1）——补登记夹具+
// 四头前置，404 兜底断言语义不变。
func TestNotFoundJSON(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	rr := doHeartbeatReq(t, h, http.MethodGet, "/api/v1/no-such-endpoint", "p-a", "05", "executor-A", "executor")

	wantErrBody(t, rr, http.StatusNotFound, "not_found")
}

// TestMethodNotAllowed GET 打 GET-only 端点的 POST 面 → 405 + error.code=method_not_allowed
// （B0-T1 已裁：标准库纯文本 405 统一 JSON 化；本批尚无 POST 端点，以 ping 的反方法验证）。
func TestMethodNotAllowed(t *testing.T) {
	h := newTestServer(t)

	rr := doReq(t, h, http.MethodPost, "/api/v1/ping", nil)

	wantErrBody(t, rr, http.StatusMethodNotAllowed, "method_not_allowed")
	// 探针接管须保留内置 handler 算出的 Allow 头，供客户端自纠
	if allow := rr.Header().Get("Allow"); allow == "" {
		t.Errorf("405 响应缺 Allow 头，want 内置 handler 计算的方法列表（如 GET, HEAD）")
	}
}

// TestHeadFallback 锚定 ServeMux「GET pattern 隐式匹配 HEAD」优先级语义：
// HEAD /api/v1/ping 必须命中业务 handler（200），不得落入 405 兜底。
func TestHeadFallback(t *testing.T) {
	h := newTestServer(t)

	rr := doReq(t, h, http.MethodHead, "/api/v1/ping", nil)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
	}
}

// TestTrailingSlashNotFound 尾斜杠路径（/api/v1/ping/）未注册 → 404 JSON，
// 不得匹配 subtree 兜底以外的任何业务 pattern，也不得 405。
// B1-4 起尾斜杠路径不在豁免名单（精确匹配）——补夹具+四头前置，断言语义不变。
func TestTrailingSlashNotFound(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	rr := doHeartbeatReq(t, h, http.MethodGet, "/api/v1/ping/", "p-a", "05", "executor-A", "executor")

	wantErrBody(t, rr, http.StatusNotFound, "not_found")
}

// TestRedirectPassthrough 路径规范化 redirect（如 //api/v1/ping → 307）必须原样透传，
// 不得被 404/405 JSON 化接管误杀（接管机制的对称面回归锚）。
// B1-4 审查裁定后双斜杠路径不满足 /api/v1/ 前缀，心跳中间件直接放行至 mux，
// 无头即达 307（B0 原行为，前置已回退）。
func TestRedirectPassthrough(t *testing.T) {
	h := newTestServer(t)

	rr := doReq(t, h, http.MethodGet, "//api/v1/ping", nil)

	if rr.Code != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want 307（body: %s）", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/api/v1/ping" {
		t.Errorf("Location = %q, want /api/v1/ping", loc)
	}
}

// TestBodyLimit 超过 1MB 上限的请求体 → 413 + error.code=body_too_large。
// bodyLimit 为最外层中间件：413 必须先于 405（POST 超限同时命中两个条件，顺序即此测的锚点）。
func TestBodyLimit(t *testing.T) {
	h := newTestServer(t)

	big := bytes.Repeat([]byte("a"), maxBodyBytes+1)
	rr := doReq(t, h, http.MethodPost, "/api/v1/ping", big)

	wantErrBody(t, rr, http.StatusRequestEntityTooLarge, "body_too_large")
}

// TestBadJSONShape 错误响应统一 JSON 形状已被 wantErrBody 全量覆盖；
// 本测试独立锚定 Content-Type 契约（§2.1：一律 application/json; charset=utf-8）。
// B1-4 心跳中间件挂入后补夹具+四头前置（非豁免路径要求身份），断言语义不变。
func TestBadJSONShape(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	rr := doHeartbeatReq(t, h, http.MethodGet, "/api/v1/definitely-not-registered", "p-a", "05", "executor-A", "executor")

	if got := rr.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("错误响应 Content-Type = %q, want %q", got, "application/json; charset=utf-8")
	}
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

// doAuthedReq 发带身份四头+请求体的请求（B1-5 登记域端点测试入口：请求经心跳
// 中间件，四头按 heads 变参 proj/col/sess/role 依序传入，传空串=省略该头；
// body 为 JSON 文本，空串=空请求体）。返回 recorder 供状态码/字段断言。
func doAuthedReq(t *testing.T, h http.Handler, method, target, body string, heads ...string) *httptest.ResponseRecorder {
	t.Helper()
	hv := func(i int) string {
		if i < len(heads) {
			return heads[i]
		}
		return ""
	}
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	set := func(k, v string) {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	set(types.HeaderAiteamProject, hv(0))
	set(types.HeaderAiteamColumn, hv(1))
	set(types.HeaderAiteamSession, hv(2))
	set(types.HeaderAiteamRole, hv(3))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// wantData 断言 2xx 响应并返回 data 对象（登记域成功面统一入口）。
func wantData(t *testing.T, rr *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status = %d, want %d（body: %s）", rr.Code, status, rr.Body.String())
	}
	m := decodeMap(t, rr)
	data, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 data 对象: %s", rr.Body.String())
	}
	return data
}
