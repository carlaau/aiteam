package server

import (
	"net/http"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 本文件为登记域（FR2 栏目登记）端点 #5~#8（§2.2 逐字段）。错误映射/
// 请求体解析/审计 detail 序列化复用 handler_projects.go 登记域共享 helper。

// columnData 端点 #5/#6 响应 data 与 column.register/update 审计 detail 快照
// 共用结构（§2.2 #5 字段面逐字段：updated_at 不出字段面）。
type columnData struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	Code      string `json:"code"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// columnUpdateDetail column.update 审计 detail（before/after 口径同 project.update）。
type columnUpdateDetail struct {
	Before columnData `json:"before"`
	After  columnData `json:"after"`
}

// columnRegisterDetail column.register 审计 detail：无 before，after=请求面快照
// （id 由审计弱关联列 column_id 指向、project_id 同理，与 project.register 同口径）。
type columnRegisterDetail struct {
	After columnRegisterSnapshot `json:"after"`
}

// columnRegisterSnapshot register 动作可预知的实体面。
type columnRegisterSnapshot struct {
	Code   string `json:"code"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// newColumnData store 行 → 响应/快照结构（§2.2 #5 字段面）。
func newColumnData(c store.Column) columnData {
	return columnData{
		ID:        c.ID,
		ProjectID: c.ProjectID,
		Code:      c.Code,
		Name:      c.Name,
		Status:    c.Status,
		CreatedAt: c.CreatedAt,
	}
}

// ---- #5 POST /api/v1/projects/{code}/columns ----

// handleColumnCreate 端点 #5（§2.2 #5 逐字段）：栏目登记+位点预置+审计同事务
// （CreateColumnWithAudit，AC2.1 副作用+AC2.4 留痕原子）；成功 201 Created。
func (s *Server) handleColumnCreate(w http.ResponseWriter, r *http.Request) {
	projectCode := r.PathValue("code")
	var req struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	col, _, err := s.st.CreateColumnWithAudit(
		projectCode,
		store.Column{Code: req.Code, Name: req.Name},
		store.AuditEntry{
			Action:    store.AuditColumnRegister,
			SessionID: sessionIDFromCtx(r.Context()),
			Detail: auditDetailJSON(columnRegisterDetail{After: columnRegisterSnapshot{
				Code:   req.Code,
				Name:   req.Name,
				Status: store.ColumnStatusActive,
			}}),
		},
	)
	if err != nil {
		s.writeStoreError(w, err) // ErrColumnExists→409 / ErrProjectNotFound→404
		return
	}
	types.WriteData(w, http.StatusCreated, newColumnData(col))
}

// ---- #6 PATCH /api/v1/projects/{code}/columns/{col} ----

// handleColumnUpdate 端点 #6（§2.2 #6：可变字段仅 name，响应同 #5 字段面）：
// 更新+审计同事务（UpdateColumnWithAudit——§七 往返账「update/archive = 单事务」，
// 审计失败整体回滚，AC2.4 原子）；栏目/项目不存在归并 404 column_not_found
// （store 归并口径）。空更新由 store ErrNoFields 映射 400 param_invalid。
func (s *Server) handleColumnUpdate(w http.ResponseWriter, r *http.Request) {
	projectCode, colCode := r.PathValue("code"), r.PathValue("col")
	var req struct {
		Name *string `json:"name"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	after, _, err := s.st.UpdateColumnWithAudit(projectCode, colCode, req.Name,
		store.AuditEntry{
			Action:    store.AuditColumnUpdate,
			SessionID: sessionIDFromCtx(r.Context()),
		},
		// detail 快照在事务内 before/after 真实行齐备后生成（store 回调）。
		func(before, after store.Column) string {
			return auditDetailJSON(columnUpdateDetail{
				Before: newColumnData(before),
				After:  newColumnData(after),
			})
		},
	)
	if err != nil {
		s.writeStoreError(w, err) // ErrNoFields→400；不存在→404
		return
	}
	types.WriteData(w, http.StatusOK, newColumnData(after))
}

// ---- #7 DELETE /api/v1/projects/{code}/columns/{col} ----

// handleColumnArchive 端点 #7（§2.2 #7 软删，AC2.3）：响应恰
// {status:"archived"}；归档+审计同事务（ArchiveColumnWithAudit——§七 往返账
// 同 update）；位点/历史保留可查；幂等重放照常留痕且 detail 记真实前态。
func (s *Server) handleColumnArchive(w http.ResponseWriter, r *http.Request) {
	projectCode, colCode := r.PathValue("code"), r.PathValue("col")
	if _, err := s.st.ArchiveColumnWithAudit(projectCode, colCode,
		store.AuditEntry{
			Action:    store.AuditColumnArchive,
			SessionID: sessionIDFromCtx(r.Context()),
		},
		// detail 快照在事务内拿到 before 真实行后生成（store 回调）。
		func(before store.Column) string {
			return auditDetailJSON(statusFlow{
				Before: statusSnapshot{Status: before.Status},
				After:  statusSnapshot{Status: store.ColumnStatusArchived},
			})
		},
	); err != nil {
		s.writeStoreError(w, err)
		return
	}
	types.WriteData(w, http.StatusOK, archiveData{Status: store.ColumnStatusArchived})
}

// ---- #8 GET /api/v1/projects/{code}/columns ----

// columnListData 端点 #8 响应 data（元素同 #5 字段面）。
type columnListData struct {
	Columns []columnData `json:"columns"`
}

// handleColumnList 端点 #8（§2.2 #8）：按项目定位（未命中 404 project_not_found，
// store ListColumns 口径），默认仅 active、include_archived 全含。
func (s *Server) handleColumnList(w http.ResponseWriter, r *http.Request) {
	projectCode := r.PathValue("code")
	includeArchived, ok := parseIncludeArchived(w, r)
	if !ok {
		return
	}
	cols, err := s.st.ListColumns(projectCode, includeArchived)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	// 非 nil 空切片兜底（store 已保证，双保险防 JSON null）。
	rows := make([]columnData, 0, len(cols))
	for _, c := range cols {
		rows = append(rows, newColumnData(c))
	}
	types.WriteData(w, http.StatusOK, columnListData{Columns: rows})
}
