package server

import (
	"net/http"
	"strings"
	"testing"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// ---- #5 POST /api/v1/projects/{code}/columns ----

// TestCreateColumnEndpoint 端点 #5：成功 201 且 data 逐字段对拍 §2.2（六字段，
// 无 updated_at）；副作用=为该栏目预置 controller/executor 位点（AC2.1，副作用
// 可见）；写动作落 column.register 审计（AC2.4）；column_exists 409、
// project_not_found 404、缺 code 400。
func TestCreateColumnEndpoint(t *testing.T) {
	h, st := seedDomain(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	projB, err := st.CreateProject(store.Project{Code: "p-b", Name: "项目B"})
	if err != nil {
		t.Fatalf("夹具 p-b 失败: %v", err)
	}
	pidB := projB.ID

	// 成功：目标项目 p-b 下登记栏目 06（身份挂 p-a/05——目标栏目尚未存在）。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/projects/p-b/columns",
		`{"code":"06","name":"栏目六"}`, authProj, authCol, authSess, authRole)
	data := wantData(t, rr, http.StatusCreated)
	newID := int64(data["id"].(float64))
	for k, want := range map[string]any{
		"project_id": float64(pidB),
		"code":       "06",
		"name":       "栏目六",
		"status":     "active",
		"created_at": t0,
	} {
		if got := data[k]; got != want {
			t.Errorf("data.%s = %v，期望 %v（§2.2 #5 逐字段）", k, got, want)
		}
	}
	if data["id"] == nil || newID <= 0 {
		t.Errorf("data.id = %v，期望 > 0", data["id"])
	}
	if _, ok := data["updated_at"]; ok {
		t.Error("data 不应含 updated_at（§2.2 #5 字段面外）")
	}

	// AC2.4：column.register 审计含会话身份+服务端时间，弱关联指向新栏目。
	sess := querySingleSession(t, st)
	entries := queryAuditByAction(t, st, store.AuditColumnRegister)
	if len(entries) != 1 {
		t.Fatalf("column.register 审计行数 = %d，期望 1", len(entries))
	}
	e := entries[0]
	if e.SessionID != sess.ID || e.ProjectID != pidB || e.ColumnID != newID {
		t.Errorf("审计弱关联 = (sess=%d,proj=%d,col=%d)，期望 (%d,%d,%d)",
			e.SessionID, e.ProjectID, e.ColumnID, sess.ID, pidB, newID)
	}
	if e.CreatedAt != t0 {
		t.Errorf("审计 created_at = %q，期望注入时钟 %q（AC2.4）", e.CreatedAt, t0)
	}

	// 副作用可见（AC2.1）：预置位点恰 2 行（controller/executor）。
	var consumers []string
	rows, err := st.DB.Query(`SELECT consumer FROM ack_positions WHERE column_id = ? ORDER BY consumer`, newID)
	if err != nil {
		t.Fatalf("查询预置位点失败: %v", err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("扫描 consumer 失败: %v", err)
		}
		consumers = append(consumers, c)
	}
	_ = rows.Close()
	if len(consumers) != 2 || consumers[0] != "controller" || consumers[1] != "executor" {
		t.Errorf("预置位点 consumers = %v，期望 [controller executor]", consumers)
	}

	// column_exists（AC2.1 重名拒数据面）→ 409，错误信息指明冲突栏目。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/projects/p-b/columns",
		`{"code":"06","name":"重复"}`, authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusConflict, types.CodeColumnExists)
	if msg := errMsg(t, rr); !strings.Contains(msg, "06") {
		t.Errorf("错误信息 %q 不含冲突栏目 code 06", msg)
	}

	// project_not_found → 404（§2.2 #5 特有错误）。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/projects/nope/columns",
		`{"code":"06"}`, authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeProjectNotFound)

	// 缺 code → 400 param_invalid，不落库不留痕。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/projects/p-b/columns",
		`{"name":"无code"}`, authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	if got := countTable(t, st, "columns"); got != 2 {
		t.Errorf("无效请求后 columns 行数 = %d，期望 2（夹具 05+p-b 的 06）", got)
	}
}

// ---- #6 PATCH /api/v1/projects/{code}/columns/{col} ----

// TestUpdateColumnEndpoint 端点 #6：改 name 生效（响应同 #5 字段面）；
// column.update 审计 before/after；栏目不存在与项目不存在归并 404
// column_not_found（#6 特有错误仅此一个）；空更新 400。
func TestUpdateColumnEndpoint(t *testing.T) {
	h, st := seedDomain(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	// 改 name（身份=目标域 p-a/05：目标已存在，身份可即目标）。
	rr := doAuthedReq(t, h, http.MethodPatch, "/api/v1/projects/p-a/columns/05",
		`{"name":"新栏目"}`, authProj, authCol, authSess, authRole)
	data := wantData(t, rr, http.StatusOK)
	if data["name"] != "新栏目" || data["code"] != authCol {
		t.Errorf("data.name/code = %v/%v，期望 新栏目/05", data["name"], data["code"])
	}
	if data["project_id"] == nil || data["status"] != "active" {
		t.Errorf("data.project_id/status = %v/%v，期望非零/active（§2.2 #6 同 #5 字段面）", data["project_id"], data["status"])
	}

	// column.update 审计：before/after 记 name 流转。
	entries := queryAuditByAction(t, st, store.AuditColumnUpdate)
	if len(entries) != 1 {
		t.Fatalf("column.update 审计行数 = %d，期望 1", len(entries))
	}
	e := entries[0]
	if e.SessionID != querySingleSession(t, st).ID || e.CreatedAt != t0 {
		t.Errorf("update 审计身份/时间 = (%d,%q)，期望操作会话+注入时钟", e.SessionID, e.CreatedAt)
	}
	detail := decodeAuditDetail(t, e)
	before, _ := detail["before"].(map[string]any)
	after, _ := detail["after"].(map[string]any)
	if before == nil || before["name"] != "栏目05" {
		t.Errorf("update detail.before = %v，期望含 name=栏目05", detail["before"])
	}
	if after == nil || after["name"] != "新栏目" {
		t.Errorf("update detail.after = %v，期望含 name=新栏目", detail["after"])
	}

	// 404 双形态归并：栏目不存在 / 项目不存在。
	for _, tc := range []struct{ name, target, wantEntity string }{
		{"栏目不存在", "/api/v1/projects/p-a/columns/99", "99"},
		{"项目不存在", "/api/v1/projects/nope/columns/05", "nope"},
	} {
		rr = doAuthedReq(t, h, http.MethodPatch, tc.target, `{"name":"x"}`,
			authProj, authCol, authSess, authRole)
		wantErrBody(t, rr, http.StatusNotFound, types.CodeColumnNotFound)
		if msg := errMsg(t, rr); !strings.Contains(msg, tc.wantEntity) {
			t.Errorf("[%s] 错误信息 %q 未指明实体 %q", tc.name, msg, tc.wantEntity)
		}
	}

	// 空更新 → 400 param_invalid（columns 可变字段仅 name），不留痕。
	rr = doAuthedReq(t, h, http.MethodPatch, "/api/v1/projects/p-a/columns/05",
		`{}`, authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	if got := len(queryAuditByAction(t, st, store.AuditColumnUpdate)); got != 1 {
		t.Errorf("空更新后 column.update 审计行数 = %d，期望仍 1", got)
	}
}

// ---- #7 DELETE /api/v1/projects/{code}/columns/{col} ----

// TestArchiveColumnEndpoint 端点 #7：软删 status→archived（AC2.3），响应恰
// {status:"archived"}；位点保留可查（AC2.3「历史与位点可查」）；column.archive
// 审计留痕；404 column_not_found。
func TestArchiveColumnEndpoint(t *testing.T) {
	h, st := seedDomain(t)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	// 前置：登记栏目 06（经 CreateColumnWithAudit，位点预置 2 行），归档后验证位点保留。
	if _, _, err := st.CreateColumnWithAudit(authProj, store.Column{Code: "06", Name: "栏目06"},
		store.AuditEntry{Action: store.AuditColumnRegister, Detail: "{}"}); err != nil {
		t.Fatalf("夹具 06 失败: %v", err)
	}

	rr := doAuthedReq(t, h, http.MethodDelete, "/api/v1/projects/p-a/columns/06",
		"", authProj, authCol, authSess, authRole)
	data := wantData(t, rr, http.StatusOK)
	if got := data["status"]; got != "archived" || len(data) != 1 {
		t.Errorf("data = %v，期望恰 {status:\"archived\"}（§2.2 #7）", data)
	}

	// 数据面：默认列表不见；位点保留（AC2.3）。
	active, err := st.ListColumns(authProj, false)
	if err != nil || len(active) != 1 || active[0].Code != authCol {
		t.Fatalf("归档后默认栏目列表 = %v（err=%v），期望仅夹具 05", active, err)
	}
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM ack_positions WHERE column_id = ?`,
		queryColumnID(t, st, authProj, "06")).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("归档后位点行数 = %d，期望 2（位点保留可查，AC2.3）", n)
	}

	// AC2.4：column.archive 审计。
	entries := queryAuditByAction(t, st, store.AuditColumnArchive)
	if len(entries) != 1 {
		t.Fatalf("column.archive 审计行数 = %d，期望 1", len(entries))
	}
	e := entries[0]
	if e.SessionID != querySingleSession(t, st).ID || e.CreatedAt != t0 {
		t.Errorf("archive 审计身份/时间 = (%d,%q)，期望操作会话+注入时钟", e.SessionID, e.CreatedAt)
	}
	detail := decodeAuditDetail(t, e)
	before, _ := detail["before"].(map[string]any)
	after, _ := detail["after"].(map[string]any)
	if before == nil || before["status"] != "active" || after == nil || after["status"] != "archived" {
		t.Errorf("archive detail = %v，期望 status 流转 before/after", detail)
	}

	// 404：栏目不存在 → column_not_found，不留痕。
	rr = doAuthedReq(t, h, http.MethodDelete, "/api/v1/projects/p-a/columns/99",
		"", authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeColumnNotFound)
	if got := len(queryAuditByAction(t, st, store.AuditColumnArchive)); got != 1 {
		t.Errorf("404 后 column.archive 审计行数 = %d，期望仍 1", got)
	}
}

// ---- #8 GET /api/v1/projects/{code}/columns ----

// TestListColumnsEndpoint 端点 #8：默认仅 active、include_archived=true 全含；
// 项目不存在 → 404 project_not_found。
func TestListColumnsEndpoint(t *testing.T) {
	h, st := seedDomain(t)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	// 夹具：05 active（seedDomain 已建）+ 06 archived。
	if _, _, err := st.CreateColumnWithAudit(authProj, store.Column{Code: "06", Name: "栏目06"},
		store.AuditEntry{Action: store.AuditColumnRegister, Detail: "{}"}); err != nil {
		t.Fatalf("夹具 06 失败: %v", err)
	}
	if err := st.ArchiveColumn(authProj, "06"); err != nil {
		t.Fatalf("归档夹具 06 失败: %v", err)
	}

	// 默认：仅 active 的 05。
	rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/projects/p-a/columns",
		"", authProj, authCol, authSess, authRole)
	data := wantData(t, rr, http.StatusOK)
	list, ok := data["columns"].([]any)
	if !ok {
		t.Fatalf("data.columns 缺失或非数组: %s", rr.Body.String())
	}
	if len(list) != 1 {
		t.Fatalf("默认列表长度 = %d，期望 1（archived 栏目过滤）", len(list))
	}
	row := list[0].(map[string]any)
	if row["code"] != authCol || row["status"] != "active" {
		t.Errorf("首行 code/status = %v/%v，期望 05/active", row["code"], row["status"])
	}
	if row["project_id"] == nil {
		t.Error("列表行缺 project_id（§2.2 #8 元素同 #5 字段面）")
	}

	// include_archived=true：全含。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/projects/p-a/columns?include_archived=true",
		"", authProj, authCol, authSess, authRole)
	data = wantData(t, rr, http.StatusOK)
	if got := len(data["columns"].([]any)); got != 2 {
		t.Errorf("含归档列表长度 = %d，期望 2", got)
	}

	// 项目不存在 → 404 project_not_found。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/projects/nope/columns",
		"", authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeProjectNotFound)
}

// queryColumnID 按（项目 code, 栏目 code）查栏目自增 id（位点断言用）。
func queryColumnID(t *testing.T, st *store.Store, projectCode, colCode string) int64 {
	t.Helper()
	var id int64
	err := st.DB.QueryRow(
		`SELECT c.id FROM columns c JOIN projects p ON p.id = c.project_id
		 WHERE p.code = ? AND c.code = ?`, projectCode, colCode,
	).Scan(&id)
	if err != nil {
		t.Fatalf("查询栏目 %s/%s id 失败: %v", projectCode, colCode, err)
	}
	return id
}
