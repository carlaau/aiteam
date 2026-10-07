package store

import (
	"errors"
	"fmt"
	"strings"

	"aiteam/internal/types"
)

// audit 域哨兵错误：调用方（handler/B1-5 起）用 errors.Is 映射 HTTP 语义
// （ErrAuditInvalid→bad_request 族）。
var (
	// ErrAuditInvalid 审计参数无效（action 空串/纯空白——动作必须可归因，
	// AC1.4 留痕语义；store 层同构兜底——projects/sessions 空 code/name 防御惯例，
	// 调用方以常量传参正常不应触达）。
	ErrAuditInvalid = errors.New("审计参数无效")
)

// 审计 action 常量（§3.2 表 10 枚举面）。本批（B1-3）仅定义 sessions 域用到的
// 两个；project.register/update/archive、column.register/update/archive 归 B1-5，
// resource.register/release、window.set、board.message 归 B4/B5——随各写入侧
// 同批定义，勿在本文件预置空常量。
const (
	// AuditSessionAutoRegister T1 首次隐式注册留痕（§7.1 中间件首调写）。
	AuditSessionAutoRegister = "session.auto_register"
	// AuditRoleChange D5 同名会话 role 静默覆盖强制审计（detail 记 before/after）。
	// §3.2 枚举清单未列本事件（D5 裁定追加，b1-spec §二），命名对齐 session.* 前缀。
	AuditRoleChange = "session.role_change"
)

// 登记域审计动作（§3.2 表 10 枚举，B1-5 随写入侧定义）：项目/栏目登记、修改、
// 注销（软删）各一；resource.*/window.set/board.message 归 B4/B5 随写入侧追加。
const (
	AuditProjectRegister = "project.register"
	AuditProjectUpdate   = "project.update"
	AuditProjectArchive  = "project.archive"
	AuditColumnRegister  = "column.register"
	AuditColumnUpdate    = "column.update"
	AuditColumnArchive   = "column.archive"
)

// AuditBoardMessage 看板唯一写口 #24 的消息留痕（§3.2 审计枚举，B5-1 随写入侧
// 定义）：看板无会话身份（sender_session_id=0 系统动作），审计是看板发送唯一
// 的操作者归因面。
const AuditBoardMessage = "board.message"

// audit limit 口径（§2.2 #28：limit 默认 100、最大 1000）。
const (
	AuditLimitDefault = 100
	AuditLimitMax     = 1000
)

// AuditEntry audit_log 表一行（§3.2 表 10 字段面）。
type AuditEntry struct {
	ID        int64
	ProjectID int64  // 弱关联，0=系统/跨域动作（不设 FK，直插合法）
	ColumnID  int64  // 弱关联，0=无对应栏目
	SessionID int64  // 弱关联，0=系统动作
	Action    string // 枚举见本文件常量与 §3.2 表 10 注
	Detail    string // JSON 快照文本（调用方 marshal；store 原样落库不校验形状，建议至少 '{}'）
	CreatedAt string // ISO8601 UTC（服务端注入，AC5.4 同口径）
}

// auditColumns SELECT 列清单（与 scanAudit 的 Scan 顺序一一对应）。
const auditColumns = `id, project_id, column_id, session_id, action, detail, created_at`

// scanAudit 从一行解出 AuditEntry（scan 兼容 *sql.Row 与 *sql.Rows 的 Scan 签名）。
func scanAudit(scan func(dest ...any) error) (AuditEntry, error) {
	var e AuditEntry
	err := scan(&e.ID, &e.ProjectID, &e.ColumnID, &e.SessionID, &e.Action, &e.Detail, &e.CreatedAt)
	if err != nil {
		return AuditEntry{}, err
	}
	return e, nil
}

// InsertAudit 审计写入（AC1.4/AC2.4 数据面；登记类动作全留痕）：
//   - action 空串/纯空白返回 ErrAuditInvalid（动作必须可归因）；
//   - 三弱关联 project_id/column_id/session_id 接受 0 值直插（系统动作，DDL
//     NOT NULL DEFAULT 0、无 FK——§3.2 表 10 注）；
//   - created_at 服务端注入 types.NowUTC（多机时钟免疫，AC5.4 同口径）；
//   - 经 RETURNING 返回库中真实行（含自增 id 与注入时间，写路径模板惯例）；
//   - 与登记动作同事务的审计写入走 insertAuditRow 的事务形态（CreateProjectWithAudit
//     /CreateColumnWithAudit），本函数为池版单条写入。
func (s *Store) InsertAudit(e AuditEntry) (AuditEntry, error) {
	return insertAuditRow(s.DB, e)
}

// auditInsertSQL 审计 INSERT...RETURNING 单源语句（InsertAudit 与事务变体共享，勿复制）。
const auditInsertSQL = `INSERT INTO audit_log (project_id, column_id, session_id, action, detail, created_at)
 VALUES (?, ?, ?, ?, ?, ?)
 RETURNING ` + auditColumns

// insertAuditRow 在 q（连接池或事务）上写入审计行（InsertAudit 与 CreateXxxWithAudit
// 单源共享）：action 空/纯空白拒绝（ErrAuditInvalid）、created_at 服务端注入。
func insertAuditRow(q queryRower, e AuditEntry) (AuditEntry, error) {
	if strings.TrimSpace(e.Action) == "" {
		return AuditEntry{}, ErrAuditInvalid
	}
	got, err := scanAudit(q.QueryRow(auditInsertSQL,
		e.ProjectID, e.ColumnID, e.SessionID, e.Action, e.Detail, types.NowUTC(),
	).Scan)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("store: 写入审计 %q 失败: %w", e.Action, err)
	}
	return got, nil
}

// QueryAudit 审计查询（#28 数据面）：projectID 0=不过滤、action 空=不过滤；
// 按 id 倒序（最新事件在前，idx_audit_project (project_id, id DESC) 支撑）。
//
// limit 口径（§2.2 #28「默认 100,最大 1000」的分层约定）：端点默认值 100 由
// handler 传参保证（B1-6）；store 层对 ≤0 兜底 AuditLimitDefault（防御调用方
// 漏传）并对超界值 clamp 到 AuditLimitMax（上限强制收敛在数据面，任何调用方
// 无法绕过）。
func (s *Store) QueryAudit(projectID int64, action string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = AuditLimitDefault
	}
	if limit > AuditLimitMax {
		limit = AuditLimitMax
	}
	var conds []string
	var args []any
	if projectID != 0 {
		conds = append(conds, "project_id = ?")
		args = append(args, projectID)
	}
	if action != "" {
		conds = append(conds, "action = ?")
		args = append(args, action)
	}
	// 条件白名单片段拼接，值全走 ? 占位（UpdateProject 同款惯例）。
	q := `SELECT ` + auditColumns + ` FROM audit_log`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询审计失败: %w", err)
	}
	defer rows.Close()
	// 非 nil 空切片兜底：nil 切片 JSON 序列化为 null（#28/B1-8 消费防线）。
	out := make([]AuditEntry, 0)
	for rows.Next() {
		e, err := scanAudit(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描审计行失败: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历审计失败: %w", err)
	}
	return out, nil
}
