package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ErrValueInvalid 资源规范串 value 非法哨兵错误。
// handler 层据 errors.Is 映射 400 value_invalid（§2.4）。
var ErrValueInvalid = errors.New("store: 资源规范串 value 非法")

// 资源类型取值（与 schema.sql resources.rtype CHECK 约束同源）。
const (
	rtypePort         = "port"
	rtypeAccountRange = "account_range"
	rtypeDataRange    = "data_range"
)

// ParseResourceValue 解析资源登记 value 规范串（B4-1 先行切片，服务端 #18 端点前置）：
//   - rtype=port：value 为纯非负整数（如 "8080"），返回 prefix=""、start=end=端口值；
//     带前缀或段形态一律 ErrValueInvalid（不校验 1~65535 上限，YAGNI）。
//   - rtype=account_range/data_range：value 为 "prefix:start-end"，前缀可省（"acct:1000-1999"
//     或 "1000-1999"）；无前缀时返回 prefix=""；段值须为非负整数（0 合法）且 start<=end。
//   - prefix 原样保留不校验格式（如 "acct"、"a b"）。
//
// 任何非法形态（空串/缺段/非数字/负数/start>end/未知 rtype）均返回 ErrValueInvalid，
// 其余错误一律不透出，调用方只需判定此哨兵。
func ParseResourceValue(rtype, value string) (prefix string, start, end int64, err error) {
	switch rtype {
	case rtypePort:
		// port 拒绝 ':'/'-' 等一切非数字形态：ParseUint 对符号、空格、空串均报错。
		u, perr := parseNonNegativeInt(value)
		if perr != nil {
			return "", 0, 0, ErrValueInvalid
		}
		return "", u, u, nil
	case rtypeAccountRange, rtypeDataRange:
		prefix := ""
		rest := value
		if p, r, ok := strings.Cut(value, ":"); ok {
			prefix, rest = p, r
		}
		startStr, endStr, ok := strings.Cut(rest, "-")
		if !ok {
			return "", 0, 0, ErrValueInvalid
		}
		s, serr := parseNonNegativeInt(startStr)
		e, eerr := parseNonNegativeInt(endStr)
		if serr != nil || eerr != nil || s > e {
			return "", 0, 0, ErrValueInvalid
		}
		return prefix, s, e, nil
	default:
		// schema CHECK 限定三值；未知 rtype 防御性归入同一哨兵。
		return "", 0, 0, ErrValueInvalid
	}
}

// parseNonNegativeInt 解析十进制非负整数字符串：拒绝空串/正负号/空格/非数字；
// 超出 int64 上限视为非法（返回值类型为 int64，防回绕）。
func parseNonNegativeInt(s string) (int64, error) {
	u, err := strconv.ParseUint(s, 10, 64)
	if err != nil || u > math.MaxInt64 {
		return 0, ErrValueInvalid
	}
	return int64(u), nil
}

// ===== B4-1 续段：resources CRUD + 全局冲突判定（D3）=====

// ErrResourceNotFound 资源行不存在哨兵错误（ReleaseResource 对未知 id 返回）。
// handler 层据 errors.Is 映射 404 resource_not_found（§2.4）。
var ErrResourceNotFound = errors.New("store: 资源不存在")

// ErrResourceConflict 资源冲突哨兵错误（判定根）。
// RegisterResource 冲突时实际返回 *ResourceConflictError（Unwrap 到此哨兵），
// handler 层 errors.Is 判 409 resource_conflict、errors.As 取冲突对象组 message。
var ErrResourceConflict = errors.New("store: 资源与在用资源冲突")

// ResourceConflict 冲突对象（供 handler 组 409 message「与 proj-a/05 在用的 resource#3 冲突」型；
// code 列由 store join 出，端点批免二次查询）。
type ResourceConflict struct {
	ID          int64 // 冲突行 id
	ProjectID   int64
	ColumnID    int64
	Rtype       string
	Value       string
	ProjectCode string // 冲突行所属项目 code（如 proj-a）
	ColumnCode  string // 冲突行所属栏目 code（如 05）
}

// String 实现 fmt.Stringer（调试可读形态；正式 409 message 组装归 handler 层）。
func (c *ResourceConflict) String() string {
	return c.ProjectCode + "/" + c.ColumnCode + " 在用的 resource#" + strconv.FormatInt(c.ID, 10) + " (" + c.Rtype + "=" + c.Value + ")"
}

// ResourceConflictError 资源冲突错误：携带冲突对象，Unwrap 到 ErrResourceConflict 哨兵。
type ResourceConflictError struct {
	Conflict ResourceConflict
}

// Error 实现 error。
func (e *ResourceConflictError) Error() string {
	return ErrResourceConflict.Error() + "（" + e.Conflict.String() + "）"
}

// Unwrap 固定到 ErrResourceConflict 哨兵（handler errors.Is 判定面）。
func (e *ResourceConflictError) Unwrap() error { return ErrResourceConflict }

// Resource 资源行（ListResources 返回视图；不含 range_prefix/start/end 解析冗余列——
// 冗余列为冲突判定内部索引面，展示面经 value 原始串即可）。
type Resource struct {
	ID         int64
	ProjectID  int64
	ColumnID   int64
	Rtype      string
	Value      string
	Note       string
	Status     string // in_use / released
	CreatedBy  int64
	CreatedAt  string
	ReleasedAt any // nil=未释放；string=释放时刻
	// B4-2：所属项目/栏目 code（ListResources join 出，#20 条目 project/column 组装面，
	// 免 handler 二次查询）。
	ProjectCode string
	ColumnCode  string
}

// conflictWherePort/conflictWhereRange §3.6 冲突判定 SQL（WHERE 谓词逐字对拍；
// SELECT 附加 join projects.code/columns.code 供 409 message 组装，仅取 1 行）。
const (
	conflictSelect = `
SELECT r.id, r.project_id, r.column_id, r.rtype, r.value, p.code, c.code
FROM resources r
JOIN projects p ON p.id = r.project_id
JOIN columns c ON c.id = r.column_id
WHERE `
	conflictPortWhere  = `r.status = 'in_use' AND r.rtype = 'port' AND r.range_start = ?`
	conflictRangeWhere = `r.status = 'in_use' AND r.rtype = ? AND r.range_prefix = ?
  AND r.range_start <= ? AND r.range_end >= ?`
)

// RegisterResource 登记共享资源（§2.2 #18 数据面）：
// value 先过 ParseResourceValue（非法→ErrValueInvalid，不落任何行）；随后单事务：
//  1. 冲突判定 SELECT（§3.6 SQL 原样，仅查 status='in_use'）：port=同 rtype+range_start
//     等值；account_range/data_range=同 rtype+同 range_prefix+闭区间重叠
//     （range_start <= :new_end AND range_end >= :new_start，端点相等=重叠）。
//     判定不含 project 过滤——D3 全局口径：跨项目同值一律拒；亦不含 archived 过滤
//     ——B4-T3：归档域在用资源仍拒（归档≠释放）。
//  2. 命中→回滚并返回 *ResourceConflictError（errors.Is(ErrResourceConflict) 判定）。
//  3. 未命中→INSERT resources（冗余列=解析结果，status='in_use'，created_at=now 注入时钟）
//     + INSERT audit_log（action=resource.register，detail=登记快照 JSON），两写同事务。
//
// 并发正确性归因：Store 连接池 MaxOpenConns(1)（store.go）令事务整体串行，
// SELECT 与 INSERT 之间无其他事务穿插窗口；idx_resources_conflict 部分索引支撑判定走索引。
//
// 返回新资源行 id；冲突返回 (0, *ResourceConflictError)；value 非法返回 (0, ErrValueInvalid)。
func (s *Store) RegisterResource(projectID, columnID int64, rtype, value, note string, createdBy int64, now string) (int64, error) {
	prefix, start, end, err := ParseResourceValue(rtype, value)
	if err != nil {
		return 0, err
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: 开启资源登记事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 冲突判定（§3.6：port 等值 / range 同 rtype+同前缀+闭区间重叠；rtype 决定分支，
	// port 的 range_prefix 解析恒为 ''）。
	var (
		conflict ResourceConflict
		qerr     error
	)
	if rtype == rtypePort {
		qerr = tx.QueryRow(conflictSelect+conflictPortWhere, start).
			Scan(&conflict.ID, &conflict.ProjectID, &conflict.ColumnID, &conflict.Rtype, &conflict.Value, &conflict.ProjectCode, &conflict.ColumnCode)
	} else {
		qerr = tx.QueryRow(conflictSelect+conflictRangeWhere, rtype, prefix, end, start).
			Scan(&conflict.ID, &conflict.ProjectID, &conflict.ColumnID, &conflict.Rtype, &conflict.Value, &conflict.ProjectCode, &conflict.ColumnCode)
	}
	if qerr == nil { // 命中在用资源：回滚（仅读无写，Rollback 即收尾）返回冲突对象
		return 0, &ResourceConflictError{Conflict: conflict}
	}
	if !errors.Is(qerr, sql.ErrNoRows) {
		return 0, fmt.Errorf("store: 资源冲突判定失败: %w", qerr)
	}

	// INSERT resources（冗余列=解析结果）。
	res, err := tx.Exec(
		`INSERT INTO resources (project_id, column_id, rtype, value, range_prefix, range_start, range_end, note, status, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'in_use', ?, ?)`,
		projectID, columnID, rtype, value, prefix, start, end, note, createdBy, now,
	)
	if err != nil {
		return 0, fmt.Errorf("store: 插入资源行失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: 取资源行 id 失败: %w", err)
	}

	// INSERT audit_log（action=resource.register，detail=登记快照）。
	detail, err := json.Marshal(map[string]any{
		"project_id": projectID, "column_id": columnID,
		"rtype": rtype, "value": value, "note": note, "created_by": createdBy,
	})
	if err != nil {
		return 0, fmt.Errorf("store: 组装登记快照失败: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO audit_log (project_id, column_id, session_id, action, detail, created_at)
		 VALUES (?, ?, ?, 'resource.register', ?, ?)`,
		projectID, columnID, createdBy, string(detail), now,
	); err != nil {
		return 0, fmt.Errorf("store: 写登记审计失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: 提交资源登记事务失败: %w", err)
	}
	return id, nil
}

// ReleaseResource 释放（注销）共享资源（§2.2 #19 数据面）：单事务 UPDATE status='released'
// +released_at=now + INSERT audit_log（action=resource.release，detail=释放前后快照，
// session_id=createdBy 操作会话身份）。签名微调注：createdBy 相比 plan 的 (id, now) 形态
// 追加操作者身份入参——审计问责面（b4-spec：操作者身份入审计）与 RegisterResource 对齐。
//
// 幂等口径（自定，B4-2 handler 据此映射）：
//   - id 不存在 → ErrResourceNotFound（映射 404）；
//   - 已 released 的重复 release → 幂等成功（返回 nil）：不覆盖 released_at（保持首次
//     释放时刻）、不重复落审计行——重复调用零副作用；
//   - 在用行 → 正常释放并落一条审计。
//
// 时间一律取注入时钟 now（服务端时间），不读墙钟。
func (s *Store) ReleaseResource(id, createdBy int64, now string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("store: 开启资源释放事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 先读行快照：区分不存在 / 已释放 / 在用三态。
	var (
		projectID, columnID int64
		rtype, value        string
		status              string
	)
	err = tx.QueryRow(
		`SELECT project_id, column_id, rtype, value, status FROM resources WHERE id = ?`, id,
	).Scan(&projectID, &columnID, &rtype, &value, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrResourceNotFound
	}
	if err != nil {
		return fmt.Errorf("store: 查询资源行失败: %w", err)
	}
	if status == "released" { // 幂等：重复释放零副作用（不覆盖 released_at、不重复审计）
		return nil
	}

	if _, err := tx.Exec(
		`UPDATE resources SET status = 'released', released_at = ? WHERE id = ? AND status = 'in_use'`, now, id,
	); err != nil {
		return fmt.Errorf("store: 释放资源失败: %w", err)
	}

	before, err := json.Marshal(map[string]any{
		"id": id, "project_id": projectID, "column_id": columnID,
		"rtype": rtype, "value": value, "status": status,
	})
	if err != nil {
		return fmt.Errorf("store: 组装释放快照失败: %w", err)
	}
	after, err := json.Marshal(map[string]any{"status": "released", "released_at": now})
	if err != nil {
		return fmt.Errorf("store: 组装释放快照失败: %w", err)
	}
	detail := `{"before":` + string(before) + `,"after":` + string(after) + `}`
	if _, err := tx.Exec(
		`INSERT INTO audit_log (project_id, column_id, session_id, action, detail, created_at)
		 VALUES (?, ?, ?, 'resource.release', ?, ?)`,
		projectID, columnID, createdBy, detail, now,
	); err != nil {
		return fmt.Errorf("store: 写释放审计失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交资源释放事务失败: %w", err)
	}
	return nil
}

// ListResources 资源清单（§2.2 #20 数据面）：单条 SELECT，三过滤一条 WHERE——
// projectID=0 不过滤（AUTOINCREMENT id 从 1 起，0 安全做哨兵）；rtype="" 不过滤；
// includeReleased=false 默认排除已释放（AC4.1）。
// join projects/columns 取双侧 code（B4-2：#20 条目 project/column 组装面）。
func (s *Store) ListResources(projectID int64, rtype string, includeReleased bool) ([]Resource, error) {
	var (
		sb   strings.Builder
		args []any
	)
	sb.WriteString(`SELECT r.id, r.project_id, r.column_id, r.rtype, r.value, r.note, r.status, r.created_by, r.created_at, r.released_at, p.code, c.code
FROM resources r
JOIN projects p ON p.id = r.project_id
JOIN columns c ON c.id = r.column_id
WHERE 1=1`)
	if projectID != 0 {
		sb.WriteString(` AND r.project_id = ?`)
		args = append(args, projectID)
	}
	if rtype != "" {
		sb.WriteString(` AND r.rtype = ?`)
		args = append(args, rtype)
	}
	if !includeReleased {
		sb.WriteString(` AND r.status = 'in_use'`)
	}
	sb.WriteString(` ORDER BY r.id`)

	rows, err := s.DB.Query(sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询资源清单失败: %w", err)
	}
	defer rows.Close()

	var out []Resource
	for rows.Next() {
		var r Resource
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.ColumnID, &r.Rtype, &r.Value, &r.Note, &r.Status, &r.CreatedBy, &r.CreatedAt, &r.ReleasedAt, &r.ProjectCode, &r.ColumnCode); err != nil {
			return nil, fmt.Errorf("store: 扫描资源行失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
