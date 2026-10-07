package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 本文件为登记域（FR1 项目登记）端点 #1~#4（§2.2 逐字段）+ 登记域共享 helper
// （错误映射/请求体解析/审计快照结构——handler_columns.go 同型消费）。

// projectData 端点 #1/#2 响应 data 与 project.register/update 审计 detail 快照
// 共用结构（§2.2 #1 字段面逐字段：updated_at 不出字段面）。
type projectData struct {
	ID                  int64  `json:"id"`
	Code                string `json:"code"`
	Name                string `json:"name"`
	Status              string `json:"status"`
	HeartbeatTimeoutSec int    `json:"heartbeat_timeout_sec"`
	CreatedAt           string `json:"created_at"`
}

// projectUpdateDetail project.update 审计 detail（b1-spec §二建议口径 before/after）。
type projectUpdateDetail struct {
	Before projectData `json:"before"`
	After  projectData `json:"after"`
}

// projectRegisterDetail project.register 审计 detail：register 无 before，
// detail=after 快照（b1-spec §二）。快照取请求面字段（code/name/status/阈值）——
// 自增 id 由审计弱关联列 project_id 精确指向、created_at 与审计行同事务同时钟，
// 不在快照内重复。
type projectRegisterDetail struct {
	After projectRegisterSnapshot `json:"after"`
}

// projectRegisterSnapshot register 动作可预知的实体面。阈值用指针口径：未提供
// （≤0）省略键——与落库口径一致（库侧补 DDL DEFAULT 900，快照不谎报 0）。
type projectRegisterSnapshot struct {
	Code                string `json:"code"`
	Name                string `json:"name"`
	Status              string `json:"status"`
	HeartbeatTimeoutSec *int   `json:"heartbeat_timeout_sec,omitempty"`
}

// archiveData 端点 #3/#7 响应 data（§2.2：data:{status:"archived"}，恰一字段）。
type archiveData struct {
	Status string `json:"status"`
}

// statusFlow 归档动作审计 detail：status 流转精简快照（updated_at 归档后无法
// 免查询取得真值，不入快照；幂等重放时 before.status=archived 诚实反映无变化）。
type statusFlow struct {
	Before statusSnapshot `json:"before"`
	After  statusSnapshot `json:"after"`
}

type statusSnapshot struct {
	Status string `json:"status"`
}

// newProjectData store 行 → 响应/快照结构（§2.2 #1 字段面）。
func newProjectData(p store.Project) projectData {
	return projectData{
		ID:                  p.ID,
		Code:                p.Code,
		Name:                p.Name,
		Status:              p.Status,
		HeartbeatTimeoutSec: p.HeartbeatTimeoutSec,
		CreatedAt:           p.CreatedAt,
	}
}

// writeStoreError store 哨兵错误 → §2.4 HTTP 错误码映射（登记域 handler 共用，
// columns/messages 同型消费）：project_exists 409 / column_exists 409 /
// project_not_found 404 / column_not_found 404 / 参数族（ErrProjectInvalid、
// ErrColumnInvalid、ErrNoFields、ErrAuditInvalid、ErrMessageInvalid——后者为
// B2 消息域 InsertMessage 数据面兜底，handler 主责前置校验；ErrInvalidLimit/
// ErrInvalidSeq 产于 B2-4 poll/ack 链路 store 层、ErrInvalidOrder 产于 #13
// QueryHistory（B2-5 消费）——三者均为 handler 前置拦截后的数据面兜底）400
// param_invalid / 兜底 500 internal_error。错误信息透传 store 侧文本（中文、
// 含实体标识——AC2.2「错误信息指明」）；未映射错误 slog 落日志（生产可排查，
// 500 响应不带内部细节）。
func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrProjectExists):
		types.WriteError(w, http.StatusConflict, types.CodeProjectExists, err.Error())
	case errors.Is(err, store.ErrColumnExists):
		types.WriteError(w, http.StatusConflict, types.CodeColumnExists, err.Error())
	case errors.Is(err, store.ErrProjectNotFound):
		types.WriteError(w, http.StatusNotFound, types.CodeProjectNotFound, err.Error())
	case errors.Is(err, store.ErrColumnNotFound):
		types.WriteError(w, http.StatusNotFound, types.CodeColumnNotFound, err.Error())
	case errors.Is(err, store.ErrMessageNotFound):
		// B2-5：#12 回执校验链首步（GetMessageBySeq 未命中）——信息指明 seq。
		types.WriteError(w, http.StatusNotFound, types.CodeMessageNotFound, err.Error())
	case errors.Is(err, store.ErrProjectInvalid),
		errors.Is(err, store.ErrColumnInvalid),
		errors.Is(err, store.ErrNoFields),
		errors.Is(err, store.ErrAuditInvalid),
		errors.Is(err, store.ErrMessageInvalid),
		errors.Is(err, store.ErrInvalidLimit),
		errors.Is(err, store.ErrInvalidSeq),
		errors.Is(err, store.ErrInvalidOrder):
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid, err.Error())
	default:
		slog.Error("store 错误未映射为 HTTP 状态，按 internal_error 兜底", "err", err)
		types.WriteError(w, http.StatusInternalServerError, types.CodeInternalError, "服务端内部错误")
	}
}

// decodeJSONBody 解析 JSON 请求体到 dst（§2.1 application/json）：非法 JSON →
// 400 bad_json；JSON 体后含尾随数据（如 {}{} 拼接体）→ 400 bad_json（防静默
// 只取首个文档）；读体撞 bodyLimit 中间件的 MaxBytesReader 上限 → 413
// body_too_large。返回 false=错误响应已写出，调用方直接 return。
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			types.WriteError(w, http.StatusRequestEntityTooLarge, types.CodeBodyTooLarge,
				"请求体超过 1MB 上限")
			return false
		}
		types.WriteError(w, http.StatusBadRequest, types.CodeBadJSON, "请求体不是合法 JSON")
		return false
	}
	// 尾随数据确认：合法体后只允许剩 EOF（空白）。
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		types.WriteError(w, http.StatusBadRequest, types.CodeBadJSON, "请求体含尾随数据")
		return false
	}
	return true
}

// parseIncludeArchived 解析 GET 列表端的 include_archived 查询参数（#4/#8 共用）：
// 缺省 false（§2.2 #4「默认 false」）；非布尔值 → 400 param_invalid 并返回
// ok=false（调用方须立即 return，响应已写出）。
func parseIncludeArchived(w http.ResponseWriter, r *http.Request) (include, ok bool) {
	raw := r.URL.Query().Get("include_archived")
	if raw == "" {
		return false, true
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
			"include_archived 须为布尔值")
		return false, false
	}
	return b, true
}

// ---- #1 POST /api/v1/projects ----

// handleProjectCreate 端点 #1（§2.2 #1 逐字段）：登记+审计同事务（AC1.4 留痕
// 与登记原子，spec §七「单事务 2 语句」）；成功 201 Created。
func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code                string `json:"code"`
		Name                string `json:"name"`
		HeartbeatTimeoutSec int    `json:"heartbeat_timeout_sec"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// 快照阈值指针化：>0 才记键（未提供=请求未指定，落库 DEFAULT 由库侧负责）。
	var timeoutSnapshot *int
	if req.HeartbeatTimeoutSec > 0 {
		timeout := req.HeartbeatTimeoutSec
		timeoutSnapshot = &timeout
	}
	p, _, err := s.st.CreateProjectWithAudit(
		store.Project{Code: req.Code, Name: req.Name, HeartbeatTimeoutSec: req.HeartbeatTimeoutSec},
		store.AuditEntry{
			Action: store.AuditProjectRegister,
			// SessionID=心跳中间件注入的操作会话（0=未注入，系统动作语义）。
			SessionID: sessionIDFromCtx(r.Context()),
			Detail: auditDetailJSON(projectRegisterDetail{After: projectRegisterSnapshot{
				Code:                req.Code,
				Name:                req.Name,
				Status:              store.ProjectStatusActive,
				HeartbeatTimeoutSec: timeoutSnapshot,
			}}),
		},
	)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	types.WriteData(w, http.StatusCreated, newProjectData(p))
}

// ---- #2 PATCH /api/v1/projects/{code} ----

// handleProjectUpdate 端点 #2（§2.2 #2：name/heartbeat_timeout_sec 至少一项，
// 响应同 #1 字段面）：更新+审计同事务（UpdateProjectWithAudit——§七 往返账
// 「update/archive = 单事务」，审计失败整体回滚不留半程状态，AC1.4 原子）。
// 空更新由 store ErrNoFields 映射 400 param_invalid。
func (s *Server) handleProjectUpdate(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	var req struct {
		Name                *string `json:"name"`
		HeartbeatTimeoutSec *int    `json:"heartbeat_timeout_sec"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	var timeout int
	if req.HeartbeatTimeoutSec != nil {
		switch {
		case *req.HeartbeatTimeoutSec < 0:
			// 负数显式拒绝（参数问题，防静默忽略造成「看似成功实未生效」）；
			// 0 保留「未提供」语义（store 侧跳过，落 DDL 默认口径不覆盖）。
			types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
				"heartbeat_timeout_sec 不可为负数（0=未提供走默认）")
			return
		default:
			timeout = *req.HeartbeatTimeoutSec
		}
	}
	after, _, err := s.st.UpdateProjectWithAudit(code, req.Name, timeout,
		store.AuditEntry{
			Action:    store.AuditProjectUpdate,
			SessionID: sessionIDFromCtx(r.Context()),
		},
		// detail 快照在事务内 before/after 真实行齐备后生成（store 回调）。
		func(before, after store.Project) string {
			return auditDetailJSON(projectUpdateDetail{
				Before: newProjectData(before),
				After:  newProjectData(after),
			})
		},
	)
	if err != nil {
		s.writeStoreError(w, err) // ErrNoFields→400；不存在→404
		return
	}
	types.WriteData(w, http.StatusOK, newProjectData(after))
}

// ---- #3 DELETE /api/v1/projects/{code} ----

// handleProjectArchive 端点 #3（§2.2 #3 软删=archive，AC1.3）：响应恰
// {status:"archived"}；归档+审计同事务（ArchiveProjectWithAudit，§七 往返账
// 「update/archive = 单事务」）；幂等重放照常留痕且 detail 记真实前态。
func (s *Server) handleProjectArchive(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if _, err := s.st.ArchiveProjectWithAudit(code,
		store.AuditEntry{
			Action:    store.AuditProjectArchive,
			SessionID: sessionIDFromCtx(r.Context()),
		},
		// detail 快照在事务内拿到 before 真实行后生成（store 回调）。
		func(before store.Project) string {
			return auditDetailJSON(statusFlow{
				Before: statusSnapshot{Status: before.Status},
				After:  statusSnapshot{Status: store.ProjectStatusArchived},
			})
		},
	); err != nil {
		s.writeStoreError(w, err)
		return
	}
	types.WriteData(w, http.StatusOK, archiveData{Status: store.ProjectStatusArchived})
}

// ---- #4 GET /api/v1/projects ----

// projectListRow 端点 #4 列表行（§2.2 #4：code/name/status/columns_count +
// 超集补充 created_at/heartbeat_timeout_sec，表「...」允许）。
type projectListRow struct {
	Code                string `json:"code"`
	Name                string `json:"name"`
	Status              string `json:"status"`
	ColumnsCount        int    `json:"columns_count"`
	HeartbeatTimeoutSec int    `json:"heartbeat_timeout_sec"`
	CreatedAt           string `json:"created_at"`
}

// projectListData 端点 #4 响应 data。
type projectListData struct {
	Projects []projectListRow `json:"projects"`
}

// handleProjectList 端点 #4（§2.2 #4）：默认仅 active，include_archived 全含；
// columns_count 经一条 GROUP BY 批量装配（共 2 条读，内存组装，§3.7 禁 N+1），
// 口径=栏目总量（archived 计入，与列表 status 过滤解耦）。
func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request) {
	includeArchived, ok := parseIncludeArchived(w, r)
	if !ok {
		return
	}
	projects, err := s.st.ListProjects(includeArchived)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	counts, err := s.st.ColumnCountsByProject()
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	// 非 nil 空切片兜底（store 已保证，双保险防 JSON null）。
	rows := make([]projectListRow, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, projectListRow{
			Code:                p.Code,
			Name:                p.Name,
			Status:              p.Status,
			ColumnsCount:        counts[p.ID],
			HeartbeatTimeoutSec: p.HeartbeatTimeoutSec,
			CreatedAt:           p.CreatedAt,
		})
	}
	types.WriteData(w, http.StatusOK, projectListData{Projects: rows})
}
