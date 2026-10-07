package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 登记域测试默认身份四头：操作者挂在已登记夹具域（p-a/05）——登记端点的
// 目标实体（新项目/新栏目）尚未存在，身份不能指向目标本身（心跳中间件存在性
// 校验会 404 短路，§7.1）。
const (
	authProj = "p-a"
	authCol  = "05"
	authSess = "controller-A"
	authRole = "controller"
)

// seedDomain 装配登记域测试前置：身份夹具项目+栏目（active），返回 env。
func seedDomain(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	return h, st
}

// ---- #1 POST /api/v1/projects ----

// TestCreateProjectEndpoint 端点 #1：成功 201 且 data 逐字段对拍 §2.2（六字段，
// 无 updated_at）；写动作落 project.register 审计含会话身份+服务端时间（AC1.4）；
// 重复 409 project_exists、缺 code 400 param_invalid、坏 JSON 400 bad_json。
func TestCreateProjectEndpoint(t *testing.T) {
	h, st := seedDomain(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	// 成功：字段齐（heartbeat_timeout_sec 显式覆盖）。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/projects",
		`{"code":"p-b","name":"项目B","heartbeat_timeout_sec":600}`,
		authProj, authCol, authSess, authRole)
	data := wantData(t, rr, http.StatusCreated)
	if got := data["id"]; got == nil || got.(float64) <= 0 {
		t.Errorf("data.id = %v，期望 > 0", got)
	}
	newID := int64(data["id"].(float64))
	for k, want := range map[string]any{
		"code":                  "p-b",
		"name":                  "项目B",
		"status":                "active",
		"heartbeat_timeout_sec": float64(600),
		"created_at":            t0,
	} {
		if got := data[k]; got != want {
			t.Errorf("data.%s = %v，期望 %v（§2.2 #1 逐字段）", k, got, want)
		}
	}
	if _, ok := data["updated_at"]; ok {
		t.Error("data 不应含 updated_at（§2.2 #1 字段面外）")
	}

	// AC1.4：project.register 审计恰 1 行，含会话身份（session_id 弱关联+detail
	// 快照）与服务端时间（created_at=注入时钟）。
	sess := querySingleSession(t, st)
	entries := queryAuditByAction(t, st, store.AuditProjectRegister)
	if len(entries) != 1 {
		t.Fatalf("project.register 审计行数 = %d，期望 1", len(entries))
	}
	e := entries[0]
	if e.SessionID != sess.ID {
		t.Errorf("审计 session_id = %d，期望操作会话 id %d（AC1.4 会话身份）", e.SessionID, sess.ID)
	}
	if e.ProjectID != newID {
		t.Errorf("审计 project_id = %d，期望新项目 id %d", e.ProjectID, newID)
	}
	if e.CreatedAt != t0 {
		t.Errorf("审计 created_at = %q，期望服务端注入时钟 %q（AC1.4）", e.CreatedAt, t0)
	}
	detail := decodeAuditDetail(t, e)
	if _, has := detail["before"]; has {
		t.Error("register 审计 detail 不应含 before 键（无前态）")
	}
	after, ok := detail["after"].(map[string]any)
	if !ok || after["code"] != "p-b" {
		t.Errorf("register detail.after = %v，期望含 code=p-b 的快照", detail["after"])
	}

	// 未提供 heartbeat_timeout_sec → 落 DDL DEFAULT 900 的库中真值。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/projects",
		`{"code":"p-c","name":"项目C"}`, authProj, authCol, authSess, authRole)
	data = wantData(t, rr, http.StatusCreated)
	if got := data["heartbeat_timeout_sec"]; got != float64(900) {
		t.Errorf("缺省 data.heartbeat_timeout_sec = %v，期望 DDL DEFAULT 900", got)
	}

	// 重复登记（AC1.2）→ 409 project_exists，错误信息指明冲突 code；不重复留痕。
	registers := len(queryAuditByAction(t, st, store.AuditProjectRegister)) // 已有 p-b/p-c 两行成功留痕
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/projects",
		`{"code":"p-b","name":"重复"}`, authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusConflict, types.CodeProjectExists)
	if msg := errMsg(t, rr); !strings.Contains(msg, "p-b") {
		t.Errorf("错误信息 %q 不含冲突 code %q", msg, "p-b")
	}
	if got := len(queryAuditByAction(t, st, store.AuditProjectRegister)); got != registers {
		t.Errorf("冲突拒绝后 project.register 审计行数 = %d，期望仍 %d（拒绝动作不留痕）", got, registers)
	}

	// 缺 code → 400 param_invalid（§2.4 400 组），不落库不留痕。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/projects",
		`{"name":"无code"}`, authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	if got := countTable(t, st, "projects"); got != 3 {
		t.Errorf("缺 code 拒绝后 projects 行数 = %d，期望 3（p-a 夹具+p-b+p-c，无效请求不落库）", got)
	}
	// 坏 JSON → 400 bad_json。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/projects",
		`{"code":`, authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeBadJSON)

	// 尾随数据（拼接体）→ 400 bad_json，防静默只取首个文档；两文档均不得落库。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/projects",
		`{"code":"p-tail"}{"code":"p-evil"}`, authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeBadJSON)
	if _, err := st.GetProjectByCode("p-tail"); !strings.Contains(err.Error(), "不存在") {
		t.Errorf("尾随数据拒绝后 p-tail 不应落库，查询错误 = %v", err)
	}
	if _, err := st.GetProjectByCode("p-evil"); !strings.Contains(err.Error(), "不存在") {
		t.Errorf("尾随文档 p-evil 不应落库，查询错误 = %v", err)
	}
}

// ---- #2 PATCH /api/v1/projects/{code} ----

// TestUpdateProjectEndpoint 端点 #2：改 name/heartbeat_timeout_sec 逐字段生效；
// 空更新 400、404 project_not_found；project.update 审计 detail 记 before+after。
func TestUpdateProjectEndpoint(t *testing.T) {
	h, st := seedDomain(t)
	const (
		t0 = "2026-01-01T08:00:00Z"
		t1 = "2026-01-01T09:00:00Z"
	)
	injectFixedClock(t, t0)
	if _, err := st.CreateProject(store.Project{Code: "p-b", Name: "项目B", HeartbeatTimeoutSec: 900}); err != nil {
		t.Fatalf("夹具 p-b 失败: %v", err)
	}

	// 改 name（时钟前进：服务端时间随注入走）。
	injectFixedClock(t, t1)
	rr := doAuthedReq(t, h, http.MethodPatch, "/api/v1/projects/p-b",
		`{"name":"新名"}`, authProj, authCol, authSess, authRole)
	data := wantData(t, rr, http.StatusOK)
	if got := data["name"]; got != "新名" {
		t.Errorf("data.name = %v，期望 新名", got)
	}
	if got := data["code"]; got != "p-b" {
		t.Errorf("data.code = %v，期望 p-b（§2.2 #2 响应同 #1 字段面）", got)
	}
	if got := data["heartbeat_timeout_sec"]; got != float64(900) {
		t.Errorf("data.heartbeat_timeout_sec = %v，期望保持 900（未提供不更新）", got)
	}

	// project.update 审计：before/after 快照+会话身份+服务端时间。
	entries := queryAuditByAction(t, st, store.AuditProjectUpdate)
	if len(entries) != 1 {
		t.Fatalf("project.update 审计行数 = %d，期望 1", len(entries))
	}
	e := entries[0]
	if e.SessionID != querySingleSession(t, st).ID {
		t.Error("update 审计 session_id 与操作会话不符（AC1.4）")
	}
	if e.CreatedAt != t1 {
		t.Errorf("update 审计 created_at = %q，期望注入时钟 %q", e.CreatedAt, t1)
	}
	detail := decodeAuditDetail(t, e)
	before, _ := detail["before"].(map[string]any)
	after, _ := detail["after"].(map[string]any)
	if before == nil || before["name"] != "项目B" {
		t.Errorf("update detail.before = %v，期望含 name=项目B", detail["before"])
	}
	if after == nil || after["name"] != "新名" {
		t.Errorf("update detail.after = %v，期望含 name=新名", detail["after"])
	}

	// 改 heartbeat_timeout_sec。
	rr = doAuthedReq(t, h, http.MethodPatch, "/api/v1/projects/p-b",
		`{"heartbeat_timeout_sec":300}`, authProj, authCol, authSess, authRole)
	data = wantData(t, rr, http.StatusOK)
	if got := data["heartbeat_timeout_sec"]; got != float64(300) {
		t.Errorf("timeout 更新后 data.heartbeat_timeout_sec = %v，期望 300", got)
	}

	// 404：目标不存在 → project_not_found，错误信息指明；不留痕。
	rr = doAuthedReq(t, h, http.MethodPatch, "/api/v1/projects/nope",
		`{"name":"x"}`, authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeProjectNotFound)
	if msg := errMsg(t, rr); !strings.Contains(msg, "nope") {
		t.Errorf("错误信息 %q 不含目标 code nope", msg)
	}

	// 负数 timeout → 400 param_invalid 显式拒绝（0=未提供语义保留），不留痕不落库。
	updates0 := len(queryAuditByAction(t, st, store.AuditProjectUpdate))
	rr = doAuthedReq(t, h, http.MethodPatch, "/api/v1/projects/p-b",
		`{"heartbeat_timeout_sec":-5}`, authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	if got, err := st.GetProjectByCode("p-b"); err != nil || got.HeartbeatTimeoutSec != 300 {
		t.Fatalf("负数拒绝后 timeout = %v（err=%v），期望保持 300（拒绝不落库）", got, err)
	}
	if got := len(queryAuditByAction(t, st, store.AuditProjectUpdate)); got != updates0 {
		t.Errorf("负数拒绝后 project.update 审计行数 = %d，期望仍 %d", got, updates0)
	}

	// 空更新 → 400 param_invalid（§2.2 #2「至少一项」），不留痕。
	updates := len(queryAuditByAction(t, st, store.AuditProjectUpdate)) // 已有改 name/timeout 两行
	rr = doAuthedReq(t, h, http.MethodPatch, "/api/v1/projects/p-b",
		`{}`, authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	if got := len(queryAuditByAction(t, st, store.AuditProjectUpdate)); got != updates {
		t.Errorf("空更新后 project.update 审计行数 = %d，期望仍 %d（空更新不落库不留痕）", got, updates)
	}
}

// ---- #3 DELETE /api/v1/projects/{code} ----

// TestArchiveProjectEndpoint 端点 #3：软删 status→archived（AC1.3），响应恰
// {status:"archived"}；project.archive 审计留痕；列表默认过滤、含归档可查；
// 404 project_not_found；重复归档幂等仍 200。
func TestArchiveProjectEndpoint(t *testing.T) {
	h, st := seedDomain(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	if _, err := st.CreateProject(store.Project{Code: "p-b", Name: "项目B"}); err != nil {
		t.Fatalf("夹具 p-b 失败: %v", err)
	}

	rr := doAuthedReq(t, h, http.MethodDelete, "/api/v1/projects/p-b",
		"", authProj, authCol, authSess, authRole)
	data := wantData(t, rr, http.StatusOK)
	if got := data["status"]; got != "archived" {
		t.Errorf("data.status = %v，期望 archived", got)
	}
	if len(data) != 1 {
		t.Errorf("data 字段面 = %v，期望恰 {status}（§2.2 #3）", data)
	}

	// 数据面：默认列表仅剩夹具 p-a（active），p-b 已归档不可见。
	active, err := st.ListProjects(false)
	if err != nil || len(active) != 1 || active[0].Code != authProj {
		t.Fatalf("归档后默认列表 = %v（err=%v），期望仅夹具 p-a", active, err)
	}
	all, err := st.ListProjects(true)
	if err != nil || len(all) != 2 {
		t.Fatalf("含归档列表长度 = %d（err=%v），期望 2", len(all), err)
	}

	// AC1.4：project.archive 审计，detail 记 status 流转 before/after。
	entries := queryAuditByAction(t, st, store.AuditProjectArchive)
	if len(entries) != 1 {
		t.Fatalf("project.archive 审计行数 = %d，期望 1", len(entries))
	}
	e := entries[0]
	if e.SessionID != querySingleSession(t, st).ID || e.CreatedAt != t0 {
		t.Errorf("archive 审计身份/时间 = (%d,%q)，期望操作会话+注入时钟 %q", e.SessionID, e.CreatedAt, t0)
	}
	detail := decodeAuditDetail(t, e)
	before, _ := detail["before"].(map[string]any)
	after, _ := detail["after"].(map[string]any)
	if before == nil || before["status"] != "active" || after == nil || after["status"] != "archived" {
		t.Errorf("archive detail = %v，期望 before.status=active/after.status=archived", detail)
	}

	// 幂等重放：再归档仍 200（store 幂等语义），重放动作照常留痕（before 已是 archived）。
	rr = doAuthedReq(t, h, http.MethodDelete, "/api/v1/projects/p-b",
		"", authProj, authCol, authSess, authRole)
	wantData(t, rr, http.StatusOK)
	entries = queryAuditByAction(t, st, store.AuditProjectArchive)
	if len(entries) != 2 {
		t.Fatalf("重放后 project.archive 审计行数 = %d，期望 2（动作发生即留痕）", len(entries))
	}
	detail = decodeAuditDetail(t, entries[0])
	if b, _ := detail["before"].(map[string]any); b == nil || b["status"] != "archived" {
		t.Errorf("重放审计 detail.before = %v，期望 status=archived（真实前态）", detail["before"])
	}

	// 404：目标不存在 → project_not_found，不留痕。
	rr = doAuthedReq(t, h, http.MethodDelete, "/api/v1/projects/nope",
		"", authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeProjectNotFound)
	if got := len(queryAuditByAction(t, st, store.AuditProjectArchive)); got != 2 {
		t.Errorf("404 后 project.archive 审计行数 = %d，期望仍 2（拒绝动作不留痕）", got)
	}
}

// ---- #4 GET /api/v1/projects ----

// TestListProjectsEndpoint 端点 #4：默认仅 active，include_archived=true 全含
// （created_at,id 稳定序）；行字段 code/name/status/columns_count 对拍；非布尔
// include_archived 400 param_invalid。
func TestListProjectsEndpoint(t *testing.T) {
	h, st := seedDomain(t)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	// 夹具：p-a 两栏目（01 active、02 archived）、p-b 一栏目且项目归档。
	for _, c := range []struct {
		code, status string
	}{
		{"01", store.ColumnStatusActive},
		{"02", store.ColumnStatusArchived},
	} {
		if _, _, err := st.CreateColumnWithAudit(authProj, store.Column{Code: c.code, Name: "栏目" + c.code},
			store.AuditEntry{Action: store.AuditColumnRegister, Detail: "{}"}); err != nil {
			t.Fatalf("夹具栏目 %s 失败: %v", c.code, err)
		}
	}
	if _, err := st.CreateProject(store.Project{Code: "p-b", Name: "项目B"}); err != nil {
		t.Fatalf("夹具 p-b 失败: %v", err)
	}
	if _, _, err := st.CreateColumnWithAudit("p-b", store.Column{Code: "01", Name: "B01"},
		store.AuditEntry{Action: store.AuditColumnRegister, Detail: "{}"}); err != nil {
		t.Fatal("夹具 p-b 栏目失败")
	}
	if err := st.ArchiveProject("p-b"); err != nil {
		t.Fatalf("归档夹具 p-b 失败: %v", err)
	}

	// 默认：仅 active 的 p-a，columns_count=2（archived 栏目计入计数口径）。
	rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/projects",
		"", authProj, authCol, authSess, authRole)
	data := wantData(t, rr, http.StatusOK)
	list, ok := data["projects"].([]any)
	if !ok {
		t.Fatalf("data.projects 缺失或非数组: %s", rr.Body.String())
	}
	if len(list) != 1 {
		t.Fatalf("默认列表长度 = %d，期望 1（archived 项目过滤）", len(list))
	}
	row := list[0].(map[string]any)
	if row["code"] != authProj || row["status"] != "active" {
		t.Errorf("首行 code/status = %v/%v，期望 p-a/active", row["code"], row["status"])
	}
	if row["columns_count"] != float64(3) {
		t.Errorf("columns_count = %v，期望 3（夹具 05+登记 01/02，archived 计入）", row["columns_count"])
	}
	if _, ok := row["name"]; !ok {
		t.Error("列表行缺 name 字段（§2.2 #4）")
	}

	// include_archived=true：全含，created_at,id 稳定序（p-a 先登记在前）。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/projects?include_archived=true",
		"", authProj, authCol, authSess, authRole)
	data = wantData(t, rr, http.StatusOK)
	list = data["projects"].([]any)
	if len(list) != 2 {
		t.Fatalf("含归档列表长度 = %d，期望 2", len(list))
	}
	if list[1].(map[string]any)["status"] != "archived" {
		t.Errorf("第二行 status = %v，期望 archived", list[1].(map[string]any)["status"])
	}

	// include_archived=false 显式等价默认。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/projects?include_archived=false",
		"", authProj, authCol, authSess, authRole)
	data = wantData(t, rr, http.StatusOK)
	if got := len(data["projects"].([]any)); got != 1 {
		t.Errorf("显式 false 列表长度 = %d，期望 1", got)
	}

	// 非布尔值 → 400 param_invalid。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/projects?include_archived=yeah",
		"", authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
}

// ---- 路由面：方法不匹配 405 兜底覆盖登记域 pattern ----

// TestProjectRoutesMethodNotAllowed 同 pattern 未注册方法 → 405 method_not_allowed
// （s.handle 统一兜底对登记域 pattern 生效的抽样锚：/api/v1/projects 双方法、
// /api/v1/projects/{code} 仅 PATCH/DELETE）。
func TestProjectRoutesMethodNotAllowed(t *testing.T) {
	h, _ := seedDomain(t)

	for _, tc := range []struct{ method, target string }{
		{http.MethodPut, "/api/v1/projects"},
		{http.MethodGet, "/api/v1/projects/p-b"}, // pattern 仅注册 PATCH/DELETE
	} {
		rr := doAuthedReq(t, h, tc.method, tc.target, "", authProj, authCol, authSess, authRole)
		wantErrBody(t, rr, http.StatusMethodNotAllowed, types.CodeMethodNotAllowed)
	}
}

// TestBodyLimitReadTimeFallback 413 读时兜底分支：ContentLength=-1（chunked 等
// 无预检长度场景）绕过 bodyLimit 中间件的 ContentLength 预检，MaxBytesReader
// 读时撞上限 → decodeJSONBody 的 MaxBytesError 分支回 413 body_too_large。
func TestBodyLimitReadTimeFallback(t *testing.T) {
	h, _ := seedDomain(t)

	// body 须为合法 JSON 起始（字符串值内超限）——若整体非法 JSON，Decode 在
	// 撞上限前即报 SyntaxError，测不到 MaxBytesError 分支。
	big := []byte(`{"code":"p-x","name":"` + strings.Repeat("a", maxBodyBytes) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(big))
	req.ContentLength = -1 // 模拟 chunked：预检不可用，读时兜底
	for k, v := range map[string]string{
		types.HeaderAiteamProject: authProj,
		types.HeaderAiteamColumn:  authCol,
		types.HeaderAiteamSession: authSess,
		types.HeaderAiteamRole:    authRole,
	} {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	wantErrBody(t, rr, http.StatusRequestEntityTooLarge, "body_too_large")
}
