// handler_resources.go —— B4-2 资源域三端点（技术设计 §2.2 #18/#19/#20）。
//
// 身份四头 X-Aiteam-Project/Column/Session/Role：handler r.Header.Get 直读=终态形态
// （总控信箱#10 裁定：B1 心跳中间件=旁路刷 last_seen_at，正交不改读头）。
// 写口（#18/#19）定位会话用前头三元组（X-Aiteam-Role 仅审计展示语义，不参与定位）；
// 读口（#20）不强制四头。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 身份四头（技术设计 §2.1 冻结头名；X-Aiteam-Role 本域不参与定位，不定义常量）。
const (
	hdrProject = "X-Aiteam-Project"
	hdrColumn  = "X-Aiteam-Column"
	hdrSession = "X-Aiteam-Session"
)

// 资源域错误码（§2.4：端点特有码随业务域定义，就近本文件；通用码复用 types 常量）。
const (
	codeBadJSON          = "bad_json"
	codeAuthRequired     = "auth_required"
	codeColumnNotFound   = "column_not_found"
	codeValueInvalid     = "value_invalid"
	codeResourceConflict = "resource_conflict"
	codeResourceNotFound = "resource_not_found"
	codeInternalError    = "internal_error"
)

// registerResourceReq #18 请求体（§2.2 #18）。
type registerResourceReq struct {
	Column string `json:"column"` // 栏目 code（四头 project 域内，如 "05"）
	Type   string `json:"type"`   // port | account_range | data_range
	Value  string `json:"value"`  // 规范串：'8080' / 'acct:1000-1999'
	Note   string `json:"note"`   // 用途说明（可空）
}

// resourceData 资源条目响应视图（#18 登记回显与 #20 清单条目同构；project/column
// 出 code 不出 id——人读对齐，内部 id 不外泄）。
type resourceData struct {
	ID         int64  `json:"id"`
	Project    string `json:"project"`
	Column     string `json:"column"`
	Type       string `json:"type"`
	Value      string `json:"value"`
	Note       string `json:"note"`
	Status     string `json:"status"`                // in_use / released
	CreatedAt  string `json:"created_at"`            // ISO8601 UTC 秒级
	ReleasedAt string `json:"released_at,omitempty"` // 未释放不出现
}

// releaseData #19 响应 data（§2.2 #19）。
type releaseData struct {
	Status     string `json:"status"`
	ReleasedAt string `json:"released_at"`
}

// resourceListData #20 响应 data（空清单 marshal 为 [] 而非 null，由 handler 保证非 nil）。
type resourceListData struct {
	Resources []resourceData `json:"resources"`
}

// handleRegisterResource POST /api/v1/resources（§2.2 #18）：登记共享资源。
// 错误序：bad_json 400 → auth_required 401（身份门槛先于域解析）→ column_not_found 404
// → value_invalid 400 → resource_conflict 409（message 指明冲突对象：
// 「与 {项目code}/{栏目code} 在用的 resource#{id} 冲突」）。
// created_by 取 X-Aiteam-Session 头解析的会话 id——resources.created_by 是指向
// sessions(id) 的硬 FK，严禁裸收请求体 id。
func (s *Server) handleRegisterResource(w http.ResponseWriter, r *http.Request) {
	var req registerResourceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, codeBadJSON, "请求体不是合法 JSON")
		return
	}
	sessID, ok := s.resolveSession(w, r)
	if !ok {
		return // 401 已写出
	}
	projCode := r.Header.Get(hdrProject)
	ref, err := s.st.ResolveColumn(projCode, req.Column)
	if errors.Is(err, store.ErrColumnNotFound) {
		types.WriteError(w, http.StatusNotFound, codeColumnNotFound,
			fmt.Sprintf("栏目 %s 在项目 %s 下不存在", req.Column, projCode))
		return
	}
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "栏目解析失败")
		return
	}

	// 服务端时钟：types.NowUTC 为包级注入点（测试替换即生效，不读墙钟）。
	now := types.NowUTC()
	id, err := s.st.RegisterResource(ref.ProjectID, ref.ColumnID, req.Type, req.Value, req.Note, sessID, now)
	var conflict *store.ResourceConflictError
	switch {
	case errors.As(err, &conflict):
		types.WriteError(w, http.StatusConflict, codeResourceConflict,
			fmt.Sprintf("与 %s/%s 在用的 resource#%d 冲突",
				conflict.Conflict.ProjectCode, conflict.Conflict.ColumnCode, conflict.Conflict.ID))
	case errors.Is(err, store.ErrValueInvalid):
		types.WriteError(w, http.StatusBadRequest, codeValueInvalid,
			"资源 value 规范串非法（port=纯整数；range=前缀:起始-结束）")
	case err != nil:
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "资源登记失败")
	default:
		types.WriteData(w, http.StatusCreated, resourceData{
			ID: id, Project: projCode, Column: req.Column, Type: req.Type,
			Value: req.Value, Note: req.Note, Status: "in_use", CreatedAt: now,
		})
	}
}

// handleReleaseResource DELETE /api/v1/resources/{id}（§2.2 #19）：注销（释放）共享资源。
// 同样需四头会话身份（审计 resource.release 落操作会话）；释放响应 released_at=本次
// 注入时钟。路径 id 非数字/非正数按「查无此资源」映射 404（口径：解析不了=不存在）。
func (s *Server) handleReleaseResource(w http.ResponseWriter, r *http.Request) {
	sessID, ok := s.resolveSession(w, r)
	if !ok {
		return // 401 已写出
	}
	id, perr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if perr != nil || id <= 0 {
		types.WriteError(w, http.StatusNotFound, codeResourceNotFound, "资源不存在")
		return
	}

	now := types.NowUTC()
	switch err := s.st.ReleaseResource(id, sessID, now); {
	case errors.Is(err, store.ErrResourceNotFound):
		types.WriteError(w, http.StatusNotFound, codeResourceNotFound, "资源不存在")
	case err != nil:
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "资源释放失败")
	default:
		types.WriteData(w, http.StatusOK, releaseData{Status: "released", ReleasedAt: now})
	}
}

// handleListResources GET /api/v1/resources（§2.2 #20）：资源清单读口。
// 读口宽容：不强制身份四头（存在不报错）；project query 按 code 过滤，项目不存在=空集
// （裁定固定口径：过滤条件选不出任何行，非 4xx）；include_released 默认 false 排除
// 已释放（AC4.1），非法布尔值宽容为 false；空清单固定 200+空数组（非 204/非 null）。
func (s *Server) handleListResources(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rtype := q.Get("type")
	projectCode := q.Get("project")
	includeReleased, _ := strconv.ParseBool(q.Get("include_released"))

	var projectID int64
	if projectCode != "" {
		pid, err := s.st.ResolveProjectID(projectCode)
		if errors.Is(err, store.ErrProjectNotFound) {
			types.WriteData(w, http.StatusOK, resourceListData{Resources: []resourceData{}})
			return
		}
		if err != nil {
			types.WriteError(w, http.StatusInternalServerError, codeInternalError, "项目解析失败")
			return
		}
		projectID = pid
	}

	rows, err := s.st.ListResources(projectID, rtype, includeReleased)
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "资源清单查询失败")
		return
	}
	items := make([]resourceData, 0, len(rows)) // 非 nil 保证：空清单序列化为 []
	for _, row := range rows {
		item := resourceData{
			ID: row.ID, Project: row.ProjectCode, Column: row.ColumnCode,
			Type: row.Rtype, Value: row.Value, Note: row.Note,
			Status: row.Status, CreatedAt: row.CreatedAt,
		}
		if rs, ok := row.ReleasedAt.(string); ok {
			item.ReleasedAt = rs
		}
		items = append(items, item)
	}
	types.WriteData(w, http.StatusOK, resourceListData{Resources: items})
}

// resolveSession 会话身份解析（#18/#19 写口前置）：身份四头前三元组
// （X-Aiteam-Project/Column/Session）查 sessions.id——对齐 sessions 表
// UNIQUE(project_id, column_id, name) 与 B1 心跳 upsert 键（B1 在位时必命中；
// 同名会话可跨栏目多行，仅按 name 查会歧义，故限定三头域精确命中）。
// 任一头缺失或域内无此会话 → 写 401 auth_required 并返回 false（响应已写出）。
func (s *Server) resolveSession(w http.ResponseWriter, r *http.Request) (int64, bool) {
	projCode, colCode, sessName := r.Header.Get(hdrProject), r.Header.Get(hdrColumn), r.Header.Get(hdrSession)
	if projCode == "" || colCode == "" || sessName == "" {
		types.WriteError(w, http.StatusUnauthorized, codeAuthRequired,
			"缺少会话身份头（X-Aiteam-Project/X-Aiteam-Column/X-Aiteam-Session）")
		return 0, false
	}
	sessID, err := s.st.ResolveSession(projCode, colCode, sessName)
	if errors.Is(err, store.ErrSessionNotFound) {
		types.WriteError(w, http.StatusUnauthorized, codeAuthRequired, "会话不存在")
		return 0, false
	}
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "会话解析失败")
		return 0, false
	}
	return sessID, true
}
