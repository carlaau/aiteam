package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"modernc.org/sqlite"

	"aiteam/internal/types"
)

// 项目 status 枚举（DDL CHECK (status IN ('active','archived'))）。
const (
	ProjectStatusActive   = "active"
	ProjectStatusArchived = "archived"
)

// sqliteCodeConstraintUnique SQLite extended result code SQLITE_CONSTRAINT_UNIQUE
// （sqlite3.h：19 | 8<<8；modernc/sqlite 的 Error.Code() 返回 extended code，实测 UNIQUE 冲突恒为该值）。
const sqliteCodeConstraintUnique = 2067

// projects 域哨兵错误：handler 层用 errors.Is 映射 HTTP 语义
// （ErrProjectInvalid→bad_request 族，ErrProjectExists→project_exists 409，
// ErrProjectNotFound→project_not_found 404，ErrNoFields→bad_request 族）。
var (
	// ErrProjectInvalid 项目参数无效（CreateProject 的 code 为空/纯空白）。
	ErrProjectInvalid = errors.New("项目参数无效")
	// ErrProjectExists 项目 code 唯一冲突（AC1.2 重名拒数据面）。
	ErrProjectExists = errors.New("项目 code 已存在")
	// ErrProjectNotFound 按 code 定位的项目行不存在。
	ErrProjectNotFound = errors.New("项目不存在")
	// ErrNoFields 更新调用未提供任何待更新字段（空更新拒绝，§2.2 #2「至少一项」）。
	ErrNoFields = errors.New("未提供任何待更新字段")
)

// Project projects 表一行（§3.2 表 1 字段面）。
//
// HeartbeatTimeoutSec 口径：DDL 列为 INTEGER NOT NULL DEFAULT 900（非可空列），
// 故用值类型+零值语义——0（及负数）表示「未提供项目级覆盖」：
// CreateProject 落 DDL DEFAULT 900，UpdateProject 跳过该字段；
// >0 即为显式覆盖值（T3：项目级失联阈值）。
type Project struct {
	ID                  int64
	Code                string
	Name                string
	Status              string // active | archived
	HeartbeatTimeoutSec int    // 0=未提供覆盖（INSERT 走 DEFAULT 900；UPDATE 跳过）
	CreatedAt           string // ISO8601 UTC（types.NowUTC 注入，服务端生成）
	UpdatedAt           string
}

// projectColumns SELECT 列清单（与 scanProject 的 Scan 顺序一一对应）。
const projectColumns = `id, code, name, status, heartbeat_timeout_sec, created_at, updated_at`

// scanProject 从一行解出 Project（scan 兼容 *sql.Row 与 *sql.Rows 的 Scan 签名）。
func scanProject(scan func(dest ...any) error) (Project, error) {
	var p Project
	err := scan(&p.ID, &p.Code, &p.Name, &p.Status, &p.HeartbeatTimeoutSec, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return Project{}, err
	}
	return p, nil
}

// isUniqueViolation 判定 SQLite UNIQUE 约束冲突。
// 全域共享（columns 域亦消费），勿在各域文件复制。
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqliteCodeConstraintUnique
}

// queryRower QueryRow 能力最小抽象（*sql.DB 与 *sql.Tx 均满足）：登记+审计
// 同事务变体与池版共享同一份 INSERT 语句（SQL 单源，防两处漂移）。
type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// CreateProject 新建项目（§2.2 #1 数据面）：纯登记版（无审计），事务变体见
// CreateProjectWithAudit——handler 层登记动作用 WithAudit 版（AC1.4 留痕与
// 登记同事务原子），本函数保留给无需审计的内部场景。
//   - code 为空/纯空白返回 ErrProjectInvalid（handler 层必填校验的数据面兜底）；
//   - status 固定 active、时间戳服务端注入 types.NowUTC；
//   - HeartbeatTimeoutSec≤0 时省略列落 DDL DEFAULT 900（DEFAULT 语义只在 DDL 一处）；
//   - 经 RETURNING 返回库中真实行（与 UpdateProject 模式统一）；
//   - code 唯一冲突返回 ErrProjectExists。
func (s *Store) CreateProject(p Project) (Project, error) {
	return insertProject(s.DB, p)
}

// insertProject 在 q（连接池或事务）上执行项目 INSERT...RETURNING（CreateProject
// 与 CreateProjectWithAudit 单源共享，勿复制 SQL）。
func insertProject(q queryRower, p Project) (Project, error) {
	if strings.TrimSpace(p.Code) == "" {
		return Project{}, ErrProjectInvalid
	}
	now := types.NowUTC()
	var row *sql.Row
	if p.HeartbeatTimeoutSec > 0 {
		row = q.QueryRow(
			`INSERT INTO projects (code, name, status, heartbeat_timeout_sec, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)
			 RETURNING `+projectColumns,
			p.Code, p.Name, ProjectStatusActive, p.HeartbeatTimeoutSec, now, now,
		)
	} else {
		// 未提供覆盖：省略 heartbeat_timeout_sec 列，走 DDL DEFAULT 900。
		row = q.QueryRow(
			`INSERT INTO projects (code, name, status, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)
			 RETURNING `+projectColumns,
			p.Code, p.Name, ProjectStatusActive, now, now,
		)
	}
	got, err := scanProject(row.Scan)
	if err != nil {
		if isUniqueViolation(err) {
			return Project{}, fmt.Errorf("%w: %s", ErrProjectExists, p.Code)
		}
		return Project{}, fmt.Errorf("store: 插入项目 %q 失败: %w", p.Code, err)
	}
	return got, nil
}

// CreateProjectWithAudit 项目登记+审计落行同事务（§2.2 #1 的 handler 写路径，
// spec §七「project register = 单事务 2 语句：INSERT projects + INSERT audit_log」）：
// CreateProject 的带审计事务变体——审计落库失败整体回滚，杜绝「项目已登记而
// 留痕缺失」违反 AC1.4。entry.ProjectID 自动回填新项目自增 id（登记时调用方
// 无从预知）；SessionID 由 handler 传入（心跳中间件注入的操作会话，0=系统）。
func (s *Store) CreateProjectWithAudit(p Project, entry AuditEntry) (Project, AuditEntry, error) {
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return Project{}, AuditEntry{}, fmt.Errorf("store: 开启登记项目事务失败: %w", err)
	}
	// Commit 后 Rollback 返回 ErrTxDone，忽略无害——错误路径靠它回滚。
	defer func() { _ = tx.Rollback() }()

	got, err := insertProject(tx, p)
	if err != nil {
		return Project{}, AuditEntry{}, err
	}
	entry.ProjectID = got.ID
	audit, err := insertAuditRow(tx, entry)
	if err != nil {
		return Project{}, AuditEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return Project{}, AuditEntry{}, fmt.Errorf("store: 提交登记项目事务失败: %w", err)
	}
	return got, audit, nil
}

// GetProjectByCode 按 code 精确取一行，不筛 status（archived 行照常返回：
// PATCH/DELETE 以 code 定位、历史查询不拒——§2.2 #3）。未命中返回 ErrProjectNotFound。
func (s *Store) GetProjectByCode(code string) (Project, error) {
	row := s.DB.QueryRow(`SELECT `+projectColumns+` FROM projects WHERE code = ?`, code)
	p, err := scanProject(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("%w: %s", ErrProjectNotFound, code)
	}
	if err != nil {
		return Project{}, fmt.Errorf("store: 查询项目 %q 失败: %w", code, err)
	}
	return p, nil
}

// UpdateProject 按 code 更新 name / heartbeat_timeout_sec（§2.2 #2 数据面）：
// 纯更新版（无审计），事务变体见 UpdateProjectWithAudit——handler 层更新动作
// 用 WithAudit 版（AC1.4 留痕与 UPDATE 同事务原子）。
//   - name *string：nil=不更新；提供指针即更新（空串 "" 是合法值，指针区分「未提供」）；
//   - heartbeatTimeoutSec：≤0=不更新；>0=覆盖为该值（DDL NOT NULL 列，无 NULL 口径）；
//
// 两项均未提供返回 ErrNoFields（空更新拒绝）；code 不存在返回 ErrProjectNotFound。
// 成功经 RETURNING 返回整行新值，updated_at 刷新为注入时钟。
func (s *Store) UpdateProject(code string, name *string, heartbeatTimeoutSec int) (Project, error) {
	return updateProject(s.DB, code, name, heartbeatTimeoutSec)
}

// updateProject 在 q（连接池或事务）上执行项目 UPDATE...RETURNING（UpdateProject
// 与 UpdateProjectWithAudit 单源共享，勿复制 SQL/参数拼装）。
func updateProject(q queryRower, code string, name *string, heartbeatTimeoutSec int) (Project, error) {
	var sets []string
	var args []any
	if name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *name)
	}
	if heartbeatTimeoutSec > 0 {
		sets = append(sets, "heartbeat_timeout_sec = ?")
		args = append(args, heartbeatTimeoutSec)
	}
	if len(sets) == 0 {
		return Project{}, fmt.Errorf("%w（code=%q）", ErrNoFields, code)
	}
	args = append(args, types.NowUTC(), code)
	// SET 子句由上方白名单片段拼接，无外部输入进 SQL 文本（值全走 ? 占位）。
	row := q.QueryRow(
		`UPDATE projects SET `+strings.Join(sets, ", ")+`, updated_at = ?
		 WHERE code = ?
		 RETURNING `+projectColumns,
		args...,
	)
	p, err := scanProject(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("%w: %s", ErrProjectNotFound, code)
	}
	if err != nil {
		return Project{}, fmt.Errorf("store: 更新项目 %q 失败: %w", code, err)
	}
	return p, nil
}

// projectArchiveSQL 项目归档单源语句（ArchiveProject 与事务变体共用）：CASE 表达式
// 保证幂等语义——已 archived 再归档不刷 updated_at，0 行命中（目标不存在）由调用方判。
const projectArchiveSQL = `UPDATE projects
 SET status = 'archived',
     updated_at = CASE WHEN status = 'active' THEN ? ELSE updated_at END
 WHERE code = ?`

// ArchiveProject 软删（§2.2 #3 数据面，AC1.3）：纯归档版（无审计），事务变体见
// ArchiveProjectWithAudit——handler 层归档动作用 WithAudit 版（AC1.4 原子）。
// 幂等语义「目标不存在才 404」：已 archived 再 archive 幂等成功且不刷 updated_at
// （CASE 表达式保持原值），单条 UPDATE 完成；code 不存在（0 行匹配）返回 ErrProjectNotFound。
func (s *Store) ArchiveProject(code string) error {
	res, err := s.DB.Exec(projectArchiveSQL, types.NowUTC(), code)
	if err != nil {
		return fmt.Errorf("store: 归档项目 %q 失败: %w", code, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取归档项目 %q 影响行数失败: %w", code, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrProjectNotFound, code)
	}
	return nil
}

// UpdateProjectWithAudit 项目更新+审计同事务（§2.2 #2 的 handler 写路径，§七
// 往返账「update/archive = 单事务（UPDATE + audit INSERT）」）：UPDATE 与审计
// INSERT 原子，杜绝「字段已变更而留痕缺失」违反 AC1.4。
//
// 实测账：单事务 3 语句 = SELECT before + UPDATE + INSERT audit——spec 账面
// 2 语句外的 1 条 SELECT 为 before 快照源（审计 detail 记 before/after 需更新
// 前真值），读语句入事务换取快照与变更的强一致（审查裁定可接受）。
//
//	entry 提供 action/session 骨架（ProjectID 自动回填 after.ID）；
//	detailFn 在事务内 before/after 真实行齐备后生成 detail 快照文本。
func (s *Store) UpdateProjectWithAudit(code string, name *string, heartbeatTimeoutSec int, entry AuditEntry, detailFn func(before, after Project) string) (Project, AuditEntry, error) {
	// 空更新为纯参数问题，事务外短路（省一次事务开销）。
	if name == nil && heartbeatTimeoutSec <= 0 {
		return Project{}, AuditEntry{}, fmt.Errorf("%w（code=%q）", ErrNoFields, code)
	}
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return Project{}, AuditEntry{}, fmt.Errorf("store: 开启更新项目事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// before 快照源：兼做存在性短路（ErrProjectNotFound→404）。
	before, err := scanProject(tx.QueryRow(`SELECT `+projectColumns+` FROM projects WHERE code = ?`, code).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, AuditEntry{}, fmt.Errorf("%w: %s", ErrProjectNotFound, code)
	}
	if err != nil {
		return Project{}, AuditEntry{}, fmt.Errorf("store: 定位项目 %q 失败: %w", code, err)
	}

	after, err := updateProject(tx, code, name, heartbeatTimeoutSec)
	if err != nil {
		return Project{}, AuditEntry{}, err
	}

	entry.ProjectID = after.ID
	entry.Detail = detailFn(before, after)
	audit, err := insertAuditRow(tx, entry)
	if err != nil {
		return Project{}, AuditEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return Project{}, AuditEntry{}, fmt.Errorf("store: 提交更新项目事务失败: %w", err)
	}
	return after, audit, nil
}

// ArchiveProjectWithAudit 项目归档+审计同事务（§2.2 #3 的 handler 写路径，§七
// 往返账同 update）：SELECT before + UPDATE（CASE 幂等语义保留）+ INSERT audit
// 单事务 3 语句（实测账，口径同 UpdateProjectWithAudit）。幂等重放照常留痕且
// detail 记真实前态（重放时 before.Status=archived）。
func (s *Store) ArchiveProjectWithAudit(code string, entry AuditEntry, detailFn func(before Project) string) (AuditEntry, error) {
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("store: 开启归档项目事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	before, err := scanProject(tx.QueryRow(`SELECT `+projectColumns+` FROM projects WHERE code = ?`, code).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return AuditEntry{}, fmt.Errorf("%w: %s", ErrProjectNotFound, code)
	}
	if err != nil {
		return AuditEntry{}, fmt.Errorf("store: 定位项目 %q 失败: %w", code, err)
	}

	res, err := tx.Exec(projectArchiveSQL, types.NowUTC(), code)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("store: 归档项目 %q 失败: %w", code, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// before SELECT 与 UPDATE 间目标被并发删除的竞态兜底。
		return AuditEntry{}, fmt.Errorf("%w: %s", ErrProjectNotFound, code)
	}

	entry.ProjectID = before.ID
	entry.Detail = detailFn(before)
	audit, err := insertAuditRow(tx, entry)
	if err != nil {
		return AuditEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuditEntry{}, fmt.Errorf("store: 提交归档项目事务失败: %w", err)
	}
	return audit, nil
}

// ListProjects 项目列表（§2.2 #4 数据面）：默认仅 active，includeArchived=true 全含；
// 按 created_at,id 稳定升序（TEXT ISO8601 字典序=时间序，§3.1 第 1 条）。
func (s *Store) ListProjects(includeArchived bool) ([]Project, error) {
	q := `SELECT ` + projectColumns + ` FROM projects`
	if !includeArchived {
		q += ` WHERE status = 'active'`
	}
	q += ` ORDER BY created_at, id`
	rows, err := s.DB.Query(q)
	if err != nil {
		return nil, fmt.Errorf("store: 查询项目列表失败: %w", err)
	}
	defer rows.Close()
	// 非 nil 空切片兜底：nil 切片 JSON 序列化为 null，会炸前端/CLI 渲染（B1-5/B1-6 消费防线）。
	out := make([]Project, 0)
	for rows.Next() {
		p, err := scanProject(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描项目行失败: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历项目列表失败: %w", err)
	}
	return out, nil
}
