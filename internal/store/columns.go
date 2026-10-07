package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"aiteam/internal/types"
)

// 栏目 status 枚举（DDL CHECK (status IN ('active','archived'))）。
const (
	ColumnStatusActive   = "active"
	ColumnStatusArchived = "archived"
)

// columns 域哨兵错误：handler 层用 errors.Is 映射 HTTP 语义
// （ErrColumnInvalid→bad_request 族，ErrColumnExists→column_exists 409，
// ErrColumnNotFound→column_not_found 404；ErrProjectNotFound/ErrNoFields 复用
// projects 域哨兵）。
var (
	// ErrColumnInvalid 栏目参数无效（CreateColumn 的 code 为空/纯空白）。
	ErrColumnInvalid = errors.New("栏目参数无效")
	// ErrColumnExists 栏目 code 在项目内唯一冲突（AC2.1 重名拒数据面；
	// UNIQUE(project_id, code) 按项目隔离，跨项目同 code 合法）。
	ErrColumnExists = errors.New("栏目 code 已存在")
	// ErrColumnNotFound 按（项目, 栏目 code）定位的栏目行不存在。
	ErrColumnNotFound = errors.New("栏目不存在")
)

// Column columns 表一行（§3.2 表 2 字段面）。
type Column struct {
	ID        int64
	ProjectID int64
	Code      string // 栏目标识（如 "05"），项目内唯一
	Name      string
	Status    string // active | archived
	CreatedAt string // ISO8601 UTC（types.NowUTC 注入，服务端生成）
	UpdatedAt string
}

// columnColumns SELECT 列清单（与 scanColumn 的 Scan 顺序一一对应）。
const columnColumns = `id, project_id, code, name, status, created_at, updated_at`

// scanColumn 从一行解出 Column（scan 兼容 *sql.Row 与 *sql.Rows 的 Scan 签名）。
func scanColumn(scan func(dest ...any) error) (Column, error) {
	var c Column
	err := scan(&c.ID, &c.ProjectID, &c.Code, &c.Name, &c.Status, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return Column{}, err
	}
	return c, nil
}

// CreateColumn 新建栏目（§2.2 #5 数据面，AC2.1）：纯登记版（无审计），事务
// 变体见 CreateColumnWithAudit——handler 层登记动作用 WithAudit 版（AC2.4）。
//   - col.Code 为空/纯空白返回 ErrColumnInvalid（handler 层必填校验的数据面兜底）；
//   - 项目 code 未命中返回 ErrProjectNotFound（端点 #5 project_not_found 404）；
//   - status 固定 active、时间戳服务端注入 types.NowUTC；
//   - 副作用=同事务预置 controller/executor 角色信箱位点（§3.6 初始化、D7 不回看）；
//   - 经 RETURNING 返回库中真实行；UNIQUE(project_id, code) 冲突返回 ErrColumnExists
//     （跨项目同 code 合法：唯一约束按项目隔离）。
//
// 事务边界：项目定位 + INSERT columns + InitColumnPositions 单事务（登记+预置
// 要么都在要么都不在）。
func (s *Store) CreateColumn(projectCode string, col Column) (Column, error) {
	got, _, err := s.createColumnTx(projectCode, col, nil)
	return got, err
}

// CreateColumnWithAudit 栏目登记+位点预置+审计落行同事务（§2.2 #5 的 handler
// 写路径，spec §七「column register = 单事务 3 语句：INSERT columns +
// INSERT...SELECT 预置位点 + INSERT audit_log」）：CreateColumn 的带审计事务
// 变体——审计落库失败整体回滚（含位点预置），杜绝「栏目已登记而留痕缺失」
// 违反 AC2.4。entry.ProjectID/ColumnID 自动回填新栏目行弱关联；SessionID 由
// handler 传入（心跳中间件注入的操作会话，0=系统）。
func (s *Store) CreateColumnWithAudit(projectCode string, col Column, entry AuditEntry) (Column, AuditEntry, error) {
	return s.createColumnTx(projectCode, col, &entry)
}

// createColumnTx 登记栏目事务体（CreateColumn/CreateColumnWithAudit 单源共享，
// 勿复制）：entry 非 nil 时同事务追加审计 INSERT（弱关联回填新栏目行）。
func (s *Store) createColumnTx(projectCode string, col Column, entry *AuditEntry) (Column, AuditEntry, error) {
	if strings.TrimSpace(col.Code) == "" {
		return Column{}, AuditEntry{}, ErrColumnInvalid
	}
	now := types.NowUTC()
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return Column{}, AuditEntry{}, fmt.Errorf("store: 开启登记栏目事务失败: %w", err)
	}
	// Commit 后 Rollback 返回 ErrTxDone，忽略无害——错误路径靠它回滚。
	defer func() { _ = tx.Rollback() }()

	// 项目定位走事务内 tx（GetProjectByCode 绑定 s.DB 连接池，出事务）。
	var projectID int64
	err = tx.QueryRow(`SELECT id FROM projects WHERE code = ?`, projectCode).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return Column{}, AuditEntry{}, fmt.Errorf("%w: %s", ErrProjectNotFound, projectCode)
	}
	if err != nil {
		return Column{}, AuditEntry{}, fmt.Errorf("store: 定位项目 %q 失败: %w", projectCode, err)
	}

	row := tx.QueryRow(
		`INSERT INTO columns (project_id, code, name, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 RETURNING `+columnColumns,
		projectID, col.Code, col.Name, ColumnStatusActive, now, now,
	)
	got, err := scanColumn(row.Scan)
	if err != nil {
		if isUniqueViolation(err) {
			return Column{}, AuditEntry{}, fmt.Errorf("%w: 项目 %s 栏目 %s", ErrColumnExists, projectCode, col.Code)
		}
		return Column{}, AuditEntry{}, fmt.Errorf("store: 插入栏目 %q 失败: %w", col.Code, err)
	}

	if err := InitColumnPositions(tx, projectID, got.ID, now); err != nil {
		return Column{}, AuditEntry{}, fmt.Errorf("store: 登记栏目 %q 时: %w", col.Code, err)
	}

	var audit AuditEntry
	if entry != nil {
		entry.ProjectID = projectID
		entry.ColumnID = got.ID
		audit, err = insertAuditRow(tx, *entry)
		if err != nil {
			return Column{}, AuditEntry{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return Column{}, AuditEntry{}, fmt.Errorf("store: 提交登记栏目事务失败: %w", err)
	}
	return got, audit, nil
}

// UpdateColumn 按（项目 code, 栏目 code）更新 name（§2.2 #6 数据面）：纯更新版
// （无审计），事务变体见 UpdateColumnWithAudit——handler 层更新动作用 WithAudit
// 版（AC2.4 原子）。
//   - columns 可变字段仅 name，name *string：nil=未提供即空更新拒绝（ErrNoFields）；
//     空串 "" 是合法值（指针区分「未提供」）；
//   - 定位子查询 project_id = (SELECT id FROM projects WHERE code = ?)：单条语句
//     单往返（§3.7 写路径），项目不存在与栏目不存在同样命中 0 行，归并返回
//     ErrColumnNotFound（端点 #6 特有错误仅 column_not_found）；
//   - 经 RETURNING 返回整行新值，updated_at 刷新为注入时钟。
func (s *Store) UpdateColumn(projectCode, colCode string, name *string) (Column, error) {
	return updateColumn(s.DB, projectCode, colCode, name)
}

// updateColumn 在 q（连接池或事务）上执行栏目 UPDATE...RETURNING（UpdateColumn
// 与 UpdateColumnWithAudit 单源共享，勿复制 SQL）。
func updateColumn(q queryRower, projectCode, colCode string, name *string) (Column, error) {
	if name == nil {
		return Column{}, fmt.Errorf("%w（project=%q column=%q）", ErrNoFields, projectCode, colCode)
	}
	row := q.QueryRow(
		`UPDATE columns SET name = ?, updated_at = ?
		 WHERE code = ? AND project_id = (SELECT id FROM projects WHERE code = ?)
		 RETURNING `+columnColumns,
		*name, types.NowUTC(), colCode, projectCode,
	)
	got, err := scanColumn(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Column{}, fmt.Errorf("%w: 项目 %s 栏目 %s", ErrColumnNotFound, projectCode, colCode)
	}
	if err != nil {
		return Column{}, fmt.Errorf("store: 更新栏目 %q 失败: %w", colCode, err)
	}
	return got, nil
}

// columnArchiveSQL 栏目归档单源语句（ArchiveColumn 与事务变体共用）：CASE 幂等
// 语义同 projectArchiveSQL。
const columnArchiveSQL = `UPDATE columns
 SET status = 'archived',
     updated_at = CASE WHEN status = 'active' THEN ? ELSE updated_at END
 WHERE code = ? AND project_id = (SELECT id FROM projects WHERE code = ?)`

// ArchiveColumn 软删（§2.2 #7 数据面，AC2.3 同 AC1.3 型）：纯归档版（无审计），
// 事务变体见 ArchiveColumnWithAudit——handler 层归档动作用 WithAudit 版。
// 幂等语义「目标不存在才 404」：已 archived 再 archive 幂等成功且不刷 updated_at
// （CASE 表达式保持原值），单条 UPDATE 完成；命中 0 行（项目或栏目不存在均归并）
// 返回 ErrColumnNotFound。archive 后历史与位点保留可查，收发拒绝归 handler/B2。
func (s *Store) ArchiveColumn(projectCode, colCode string) error {
	res, err := s.DB.Exec(columnArchiveSQL, types.NowUTC(), colCode, projectCode)
	if err != nil {
		return fmt.Errorf("store: 归档栏目 %q 失败: %w", colCode, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取归档栏目 %q 影响行数失败: %w", colCode, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: 项目 %s 栏目 %s", ErrColumnNotFound, projectCode, colCode)
	}
	return nil
}

// UpdateColumnWithAudit 栏目更新+审计同事务（§2.2 #6 的 handler 写路径，§七
// 往返账「update/archive = 单事务（UPDATE + audit INSERT）」）：UPDATE 与审计
// INSERT 原子，杜绝「栏目名已变更而留痕缺失」违反 AC2.4。
//
// 实测账：单事务 3 语句 = SELECT before + UPDATE + INSERT audit——before 快照
// 源入事务的口径同 UpdateProjectWithAudit（审查裁定可接受）。
//
//	entry 提供 action/session 骨架（ProjectID/ColumnID 自动回填 after 行）；
//	detailFn 在事务内 before/after 真实行齐备后生成 detail 快照文本。
func (s *Store) UpdateColumnWithAudit(projectCode, colCode string, name *string, entry AuditEntry, detailFn func(before, after Column) string) (Column, AuditEntry, error) {
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return Column{}, AuditEntry{}, fmt.Errorf("store: 开启更新栏目事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// before 快照源：定位口径与 GetColumnByCode 同（项目不存在归并栏目 404）。
	before, err := scanColumn(tx.QueryRow(
		`SELECT `+columnColumns+`
		 FROM columns
		 WHERE code = ? AND project_id = (SELECT id FROM projects WHERE code = ?)`,
		colCode, projectCode,
	).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Column{}, AuditEntry{}, fmt.Errorf("%w: 项目 %s 栏目 %s", ErrColumnNotFound, projectCode, colCode)
	}
	if err != nil {
		return Column{}, AuditEntry{}, fmt.Errorf("store: 定位项目 %q 栏目 %q 失败: %w", projectCode, colCode, err)
	}

	after, err := updateColumn(tx, projectCode, colCode, name)
	if err != nil {
		return Column{}, AuditEntry{}, err
	}

	entry.ProjectID = after.ProjectID
	entry.ColumnID = after.ID
	entry.Detail = detailFn(before, after)
	audit, err := insertAuditRow(tx, entry)
	if err != nil {
		return Column{}, AuditEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return Column{}, AuditEntry{}, fmt.Errorf("store: 提交更新栏目事务失败: %w", err)
	}
	return after, audit, nil
}

// ArchiveColumnWithAudit 栏目归档+审计同事务（§2.2 #7 的 handler 写路径，§七
// 往返账同 update）：SELECT before + UPDATE（CASE 幂等语义保留，位点不受影响）
// + INSERT audit 单事务 3 语句（实测账，口径同 UpdateProjectWithAudit）。
func (s *Store) ArchiveColumnWithAudit(projectCode, colCode string, entry AuditEntry, detailFn func(before Column) string) (AuditEntry, error) {
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("store: 开启归档栏目事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	before, err := scanColumn(tx.QueryRow(
		`SELECT `+columnColumns+`
		 FROM columns
		 WHERE code = ? AND project_id = (SELECT id FROM projects WHERE code = ?)`,
		colCode, projectCode,
	).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return AuditEntry{}, fmt.Errorf("%w: 项目 %s 栏目 %s", ErrColumnNotFound, projectCode, colCode)
	}
	if err != nil {
		return AuditEntry{}, fmt.Errorf("store: 定位项目 %q 栏目 %q 失败: %w", projectCode, colCode, err)
	}

	res, err := tx.Exec(columnArchiveSQL, types.NowUTC(), colCode, projectCode)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("store: 归档栏目 %q 失败: %w", colCode, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// before SELECT 与 UPDATE 间目标被并发删除的竞态兜底。
		return AuditEntry{}, fmt.Errorf("%w: 项目 %s 栏目 %s", ErrColumnNotFound, projectCode, colCode)
	}

	entry.ProjectID = before.ProjectID
	entry.ColumnID = before.ID
	entry.Detail = detailFn(before)
	audit, err := insertAuditRow(tx, entry)
	if err != nil {
		return AuditEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuditEntry{}, fmt.Errorf("store: 提交归档栏目事务失败: %w", err)
	}
	return audit, nil
}

// ListColumns 栏目列表（§2.2 #8 数据面）：按项目 code 定位（未命中复用
// GetProjectByCode 返回 ErrProjectNotFound），默认仅 active，includeArchived=true
// 全含；按 created_at,id 稳定升序（TEXT ISO8601 字典序=时间序，§3.1 第 1 条）；
// 空列表返回非 nil 空切片（JSON null 防线）。
func (s *Store) ListColumns(projectCode string, includeArchived bool) ([]Column, error) {
	p, err := s.GetProjectByCode(projectCode)
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + columnColumns + ` FROM columns WHERE project_id = ?`
	if !includeArchived {
		q += ` AND status = 'active'`
	}
	q += ` ORDER BY created_at, id`
	rows, err := s.DB.Query(q, p.ID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询项目 %q 栏目列表失败: %w", projectCode, err)
	}
	defer rows.Close()
	// 非 nil 空切片兜底：nil 切片 JSON 序列化为 null，会炸前端/CLI 渲染（B1-5/B1-8 消费防线）。
	out := make([]Column, 0)
	for rows.Next() {
		c, err := scanColumn(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描栏目行失败: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历栏目列表失败: %w", err)
	}
	return out, nil
}

// GetColumnByCode 按（项目 code, 栏目 code）精确定位一行：PATCH/DELETE 的
// before 快照数据源（端点 #6/#7 审计 detail），不筛 status（archived 照常返回，
// 与 GetProjectByCode 同口径）。未命中返回 ErrColumnNotFound——项目不存在与
// 栏目不存在归并同一哨兵（端点 #6 特有错误仅 column_not_found，UpdateColumn
// 归并口径一致）。
func (s *Store) GetColumnByCode(projectCode, colCode string) (Column, error) {
	row := s.DB.QueryRow(
		`SELECT `+columnColumns+`
		 FROM columns
		 WHERE code = ? AND project_id = (SELECT id FROM projects WHERE code = ?)`,
		colCode, projectCode,
	)
	c, err := scanColumn(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Column{}, fmt.Errorf("%w: 项目 %s 栏目 %s", ErrColumnNotFound, projectCode, colCode)
	}
	if err != nil {
		return Column{}, fmt.Errorf("store: 查询项目 %q 栏目 %q 失败: %w", projectCode, colCode, err)
	}
	return c, nil
}

// ColumnCountsByProject 各项目栏目计数（端点 #4 columns_count）：一条 GROUP BY
// 批量出全表（§3.7 往返账禁逐项目循环查库——handler 端 ListProjects 一次 +
// 本函数一次共 2 条读，内存 map 装配）。口径：archived 栏目计入（计数=栏目
// 总量，项目的资产面，与列表 status 过滤解耦）；无栏目项目不出现在映射
// （handler 取值零值 0 即语义）。
func (s *Store) ColumnCountsByProject() (map[int64]int, error) {
	rows, err := s.DB.Query(`SELECT project_id, COUNT(*) FROM columns GROUP BY project_id`)
	if err != nil {
		return nil, fmt.Errorf("store: 统计项目栏目数失败: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]int)
	for rows.Next() {
		var projectID int64
		var n int
		if err := rows.Scan(&projectID, &n); err != nil {
			return nil, fmt.Errorf("store: 扫描栏目计数行失败: %w", err)
		}
		out[projectID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历栏目计数失败: %w", err)
	}
	return out, nil
}
