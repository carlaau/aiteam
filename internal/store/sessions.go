package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"aiteam/internal/types"
)

// sessions 域哨兵错误：消费方用 errors.Is 映射 HTTP 语义。ErrSessionInvalid
// 唯一消费点在心跳中间件（理论不可达，中间件自兜 500）；ErrSessionNotFound
// 不经 writeStoreError 通用映射（write_store_error_test.go 清单二刻意不映射，
// 落 default 500）——chat 形态 handler 内 errors.Is 专属口→404
// target_session_not_found。
var (
	// ErrSessionInvalid 会话参数无效（name/role 空串或纯空白；role 不枚举硬校验
	// ——§3.2 表 3 注「登记自由」，仅非空格式校验，且该校验主责在 B1-4 中间件，
	// store 层同构兜底——projects.go 空 code 防御惯例）。
	ErrSessionInvalid = errors.New("会话参数无效")
	// ErrSessionNotFound 按 id 定位的会话行不存在。
	ErrSessionNotFound = errors.New("会话不存在")
)

// Session sessions 表一行 + 失联计算列（§3.2 表 3 + §7.2）。
type Session struct {
	ID          int64
	ProjectID   int64
	ColumnID    int64
	ProjectCode string // JOIN projects 装配（#27 响应 project 字段）；UpsertSession 返回值为空串（不 JOIN）
	ColumnCode  string // JOIN columns 装配（#27 响应 column 字段；GetSessionByName 供 chat archived 判定错误信息）；同上
	// ColumnStatus 目标栏目状态：仅 GetSessionByName 装配（B2-3 chat 形态
	// 目标栏目 archived 判定用，AC2.3），其余路径空串——零值空串表示「未装配」
	// 而非 active，消费方注意区分（同 Alive 的未判定语义）。
	ColumnStatus string
	Name         string // --session 显式名（实体四元组之一）
	Role         string // controller / executor / …（不枚举硬校验，§3.2 表 3 注）
	LastSeenAt   string // 心跳时间戳（服务端时间）
	CreatedAt    string // ISO8601 UTC（首插时刻，upsert 不变）
	// Alive 失联计算列（§7.2 读时计算不落库）：now-last_seen_at ≤ 阈值=在线。
	// 仅 GetSession/ListSessions 路径计算；UpsertSession 返回值不计算，
	// 其零值 false 表示「未判定」而非「失联」——消费方注意区分。
	Alive bool
}

// SessionUpsert upsert 结果：upsert 后库中行 + 事件标记（B1-4 中间件审计分支判据）。
type SessionUpsert struct {
	Session Session // upsert 后库中真实行（ProjectCode/ColumnCode/Alive 不装配，见各字段注）
	// Created 本次为新插入（首调=隐式注册，T1）——中间件据此写 audit
	// session.auto_register；同秒内同四元组的第二次调用不会误报（标记来自
	// upsert 前旧行探测，见 UpsertSession 实现注）。
	Created bool
	// RoleChanged role 相对 upsert 前被覆盖（D5：静默覆盖+强制审计）——中间件
	// 据此写 role-change 审计；同 role 稳态重调不置位（不重复审计）。
	RoleChanged bool
	// OldRole upsert 前的 role（RoleChanged=true 时为旧值，供 detail 记
	// before/after）；Created=true 或未变更时空串。
	OldRole string
}

// sessionColumns SELECT 列清单（GetSession/ListSessions 共用，与 scanSession 的
// Scan 顺序一一对应）。末列为失联计算列（§3.6 失联判定 SQL 原样改造：
// strftime 秒差 > COALESCE(项目级阈值, 服务默认) = 失联，取反即 Alive；
// §7.2 判定为严格大于，此处 <= 即在线）。COALESCE 对 NOT NULL DEFAULT 900 的
// heartbeat_timeout_sec 恒取列值（B1-1 已核实），照 §3.6 原样保留，冗余无害。
// 注意：该常量含 2 个占位符（now、defaultTimeoutSec），调用方 Query 参数须以
// [now, defaultTimeoutSec, ...] 开头——占位符按 SQL 文本出现顺序绑定。
// now 须为 strftime 可解析的时间文本（ISO8601 UTC，types.NowUTC 口径）。
const sessionColumns = `s.id, s.project_id, s.column_id, p.code, c.code,
       s.name, s.role, s.last_seen_at, s.created_at,
       (strftime('%s', ?) - strftime('%s', s.last_seen_at))
         <= COALESCE(p.heartbeat_timeout_sec, ?)`

// sessionFrom 失联计算列的 JOIN 主体（§3.6 原样：JOIN projects 取阈值/code、
// JOIN columns 取 code；WHERE p.status = 'active'——archived 项目会话不列，
// §7.2「归档不再判定展示」）。
const sessionFrom = `FROM sessions s
       JOIN projects p ON p.id = s.project_id
       JOIN columns c ON c.id = s.column_id
       WHERE p.status = 'active'`

// scanSession 从一行解出 Session（alive 0/1 经 int 中转转 bool——SQLite 布尔
// 表达式返回整数，database/sql 不支持 int64 直 Scan 到 bool）。
func scanSession(scan func(dest ...any) error) (Session, error) {
	var se Session
	var aliveInt int
	err := scan(&se.ID, &se.ProjectID, &se.ColumnID, &se.ProjectCode, &se.ColumnCode,
		&se.Name, &se.Role, &se.LastSeenAt, &se.CreatedAt, &aliveInt)
	if err != nil {
		return Session{}, err
	}
	se.Alive = aliveInt == 1
	return se, nil
}

// UpsertSession 会话隐式注册/心跳刷新（§7.1 数据面，T1）：
//
//	INSERT (project,column,name,role,last_seen_at=now,created_at=now)
//	ON CONFLICT(project_id,column_id,name) DO UPDATE
//	  SET last_seen_at=excluded.last_seen_at, role=excluded.role
//
// 同四元组换机重开=同实体幂等（AC12.3）；同名换 role 静默覆盖（D5）。
// 返回事件标记（SessionUpsert.Created/RoleChanged/OldRole）供 B1-4 中间件
// 写审计分支（首次→session.auto_register；变更→role-change，detail 记 before/after）。
//
// 实现偏离说明（§7.1 SQL 原样保留为事务第 ② 条；为什么多一条探测）：
// 实测 modernc/sqlite 的 RETURNING 无法引用修改前值（子查询读到的恒为修改后
// 行——探针验证更新路径 prev=新值），而 D5 role-change 审计必须记 before，
// 故 upsert 前须先探测旧行。事务两语句原子（单连接池 §3.1 第 8 条 + 事务，
// 探测与 upsert 间无竞态）。往返账影响（b1-spec §七「中间件每请求固定 ≤3 语句」）：
// 稳态心跳请求 = 1 查 project/column + 2（本函数）= 3 达标；首次/role 变更
// 事件路径 = +1 audit INSERT = 4（低频事件，非每请求形态）。
func (s *Store) UpsertSession(projectID, columnID int64, name, role string) (SessionUpsert, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(role) == "" {
		return SessionUpsert{}, ErrSessionInvalid
	}
	now := types.NowUTC()
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return SessionUpsert{}, fmt.Errorf("store: 开启会话 upsert 事务失败: %w", err)
	}
	// Commit 后 Rollback 返回 ErrTxDone，忽略无害——错误路径靠它回滚。
	defer func() { _ = tx.Rollback() }()

	// ① 旧行探测：拿 upsert 前 role（D5 before 值）与存在性（首调判定）。
	var oldRole sql.NullString
	err = tx.QueryRow(
		`SELECT role FROM sessions WHERE project_id = ? AND column_id = ? AND name = ?`,
		projectID, columnID, name,
	).Scan(&oldRole)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SessionUpsert{}, fmt.Errorf("store: 探测会话 %q 旧值失败: %w", name, err)
	}

	// ② §7.1 upsert SQL 原样 + RETURNING 返回库中真实行（模板惯例）。
	//    project_id/column_id 不存在时 FK 约束失败（foreign_keys=ON）原样包装——
	//    中间件先行校验存在性（§7.1 步骤 1），此处为数据面兜底。
	row := tx.QueryRow(
		`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, column_id, name) DO UPDATE
		   SET last_seen_at = excluded.last_seen_at, role = excluded.role
		 RETURNING id, project_id, column_id, name, role, last_seen_at, created_at`,
		projectID, columnID, name, role, now, now,
	)
	var got Session
	err = row.Scan(&got.ID, &got.ProjectID, &got.ColumnID, &got.Name, &got.Role, &got.LastSeenAt, &got.CreatedAt)
	if err != nil {
		return SessionUpsert{}, fmt.Errorf("store: upsert 会话 %q 失败: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return SessionUpsert{}, fmt.Errorf("store: 提交会话 upsert 事务失败: %w", err)
	}

	up := SessionUpsert{Session: got}
	switch {
	case !oldRole.Valid:
		up.Created = true // 首调=隐式注册（T1）
	case oldRole.String != role:
		up.RoleChanged = true // D5：role 静默覆盖
		up.OldRole = oldRole.String
	}
	return up, nil
}

// ProjectColumnRef 会话心跳前置查询结果（§7.1 步骤 1 数据面）：两 id 供
// UpsertSession 定位，两 status 原样返回供调用方按批次口径裁量——B1-4 中间件
// 不消费 status（archived 放行，b1-spec §二），B2 若裁中间件挡 archived 直接取用。
type ProjectColumnRef struct {
	ProjectID     int64
	ProjectStatus string // active | archived（不筛，原样返回）
	ColumnID      int64
	ColumnStatus  string // 同上
}

// GetProjectColumnForSession 会话中间件存在性前置查询（§7.1 步骤 1）：按
// （项目 code, 栏目 code）一条 LEFT JOIN 同时定位两行（b1-spec §七往返账
// 「1 查 project/column，可一条 JOIN 取两行」——404 分歧判定在 JOIN 空值上
// 完成，无第二次查询）：
//   - 项目 code 未命中（外层行缺失）→ ErrProjectNotFound（404 project_not_found）；
//   - 项目命中但栏目未命中（JOIN 条件限定 c.project_id = p.id，跨项目同 code
//     栏目不误串）→ ErrColumnNotFound（404 column_not_found）；
//   - 不筛 status：archived 行原样返回（b1-spec §二「中间件只挡不存在→404，
//     archived 409 拒绝归 B2 动作端点」）。
//
// 归档说明：放 sessions.go 追加而非新开文件——本函数是 §7.1 会话心跳链路的
// 第 1 步数据面（UpsertSession 的前置），同域同文件（base.md §4.8 一域一文件）；
// CRUD SQL 集中 store 包纪律不变（store/doc.go）。
func (s *Store) GetProjectColumnForSession(projectCode, columnCode string) (ProjectColumnRef, error) {
	var (
		projectID, columnID         sql.NullInt64
		projectStatus, columnStatus sql.NullString
	)
	err := s.DB.QueryRow(
		`SELECT p.id, p.status, c.id, c.status
		 FROM projects p
		 LEFT JOIN columns c ON c.project_id = p.id AND c.code = ?
		 WHERE p.code = ?`,
		columnCode, projectCode,
	).Scan(&projectID, &projectStatus, &columnID, &columnStatus)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ProjectColumnRef{}, fmt.Errorf("store: 查询会话前置项目/栏目失败: %w", err)
	}
	// ErrNoRows 与 projectID 无效同映射（前者=外层 0 行；后者理论冗余兜底，
	// LEFT JOIN 外层行存在则 id 恒非 NULL）。
	if !projectID.Valid {
		return ProjectColumnRef{}, fmt.Errorf("%w: %s", ErrProjectNotFound, projectCode)
	}
	if !columnID.Valid {
		return ProjectColumnRef{}, fmt.Errorf("%w: 项目 %s 栏目 %s", ErrColumnNotFound, projectCode, columnCode)
	}
	return ProjectColumnRef{
		ProjectID:     projectID.Int64,
		ProjectStatus: projectStatus.String,
		ColumnID:      columnID.Int64,
		ColumnStatus:  columnStatus.String,
	}, nil
}

// SenderRef 消息发送方装配面（#10 poll 响应 sender:{session,role,column} 数据源，
// B2-4）：五要素之 sender 身份三件（AC8.4）。
type SenderRef struct {
	Session string // 发送方会话名（sessions.name）
	Role    string // 发送方角色（sessions.role）
	Column  string // 发送方栏目 code（columns.code）
}

// GetSendersByIDs 按 id 批量装配发送方（poll 响应 sender 三件，B2-4）：一条
// JOIN IN 查询取回本批消息的 distinct 发送方（§3.7 往返账「无逐行循环」——
// 调用方按消息条数内 distinct id 传入，本函数再去重防重复占位）。未命中的 id
// 不入 map（sender_session_id=0=系统/看板发送的弱关联语义，消费方零值兜底），
// 不报错——装配缺失非数据错误。
func (s *Store) GetSendersByIDs(ids []int64) (map[int64]SenderRef, error) {
	out := make(map[int64]SenderRef, len(ids))
	uniq := ids[:0:0] // 复制去重（capacity 0 新底层数组，不改写入参）
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return out, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?, ", len(uniq)), ", ")
	args := make([]any, len(uniq))
	for i, id := range uniq {
		args[i] = id
	}
	rows, err := s.DB.Query(
		`SELECT s.id, s.name, s.role, c.code
		 FROM sessions s
		 JOIN columns c ON c.id = s.column_id
		 WHERE s.id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: 批量装配发送方失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var ref SenderRef
		if err := rows.Scan(&id, &ref.Session, &ref.Role, &ref.Column); err != nil {
			return nil, fmt.Errorf("store: 扫描发送方行失败: %w", err)
		}
		out[id] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历发送方行失败: %w", err)
	}
	return out, nil
}

// GetSessionByName 按（项目 id, 会话名）定位目标会话（§2.2 #9 chat 形态数据面，
// B2-3）：send handler 用其 id/target_session_id 与 column_id（chat 消息归属=
// 目标会话栏目，§3.2 表 5 注），并随同一查询带回目标栏目 code/status 供 handler
// 判 archived→409 column_archived（AC2.3——不带回则 chat 缺目标栏目归档校验，
// 消息会落进 archived 栏目且 poll 可拉；一次 JOIN 带回，免第二次域读往返）。
// sessions 仅 (project_id, column_id, name) 唯一——同名跨栏目多实体行时取最早
// 实体行（ORDER BY id LIMIT 1；规格未定义同名定位口径，此为防御性确定性兜底）。
// 未命中返回 ErrSessionNotFound（handler 映射 404 target_session_not_found
// 专属口，不经 writeStoreError——write_store_error_test.go 清单二口径）。
// 不复用 sessionColumns/sessionFrom：后者筛 p.status='active' 且带失联计算列
// 占位符，而本查询须能定位 archived 栏目中的会话（先 404 存在性、后 409 归档，
// 存在性优先）且保持最轻读面（不 JOIN projects、不算失联列；FK 保证栏目行
// 存在，INNER JOIN 不丢行）。
func (s *Store) GetSessionByName(projectID int64, name string) (Session, error) {
	var se Session
	err := s.DB.QueryRow(
		`SELECT s.id, s.column_id, s.name, c.code, c.status
		 FROM sessions s
		 JOIN columns c ON c.id = s.column_id
		 WHERE s.project_id = ? AND s.name = ?
		 ORDER BY s.id LIMIT 1`,
		projectID, name,
	).Scan(&se.ID, &se.ColumnID, &se.Name, &se.ColumnCode, &se.ColumnStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("%w: 项目 id=%d 会话 %q", ErrSessionNotFound, projectID, name)
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: 按名查询会话 %q 失败: %w", name, err)
	}
	return se, nil
}

// GetSession 按 id 单查（JOIN 装配项目/栏目 code + 失联计算列）。
// now=判定基准时刻（ISO8601 UTC，注入参数——读路径非写入时间戳，服务端聚合
// 场景多处查询共享同一 now 保一致性）；defaultTimeoutSec=服务配置
// session.heartbeat_timeout_sec（config 注入，§7.2）。未命中返回 ErrSessionNotFound。
func (s *Store) GetSession(id int64, now string, defaultTimeoutSec int) (Session, error) {
	row := s.DB.QueryRow(
		`SELECT `+sessionColumns+` `+sessionFrom+` AND s.id = ?`,
		now, defaultTimeoutSec, id,
	)
	se, err := scanSession(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("%w: id=%d", ErrSessionNotFound, id)
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: 查询会话 id=%d 失败: %w", id, err)
	}
	return se, nil
}

// ListSessions 会话列表（#27 数据面）：projectCode 空=全部项目，非空按项目过滤
// （未命中返回 ErrProjectNotFound，对齐 ListColumns 惯例）；仅列 active 项目
// （sessionFrom，§3.6 原样）；按 created_at,id 稳定升序（§3.1 第 1 条）；
// 空结果返回非 nil 空切片（JSON 序列化 null 防线）。
func (s *Store) ListSessions(projectCode, now string, defaultTimeoutSec int) ([]Session, error) {
	q := `SELECT ` + sessionColumns + ` ` + sessionFrom
	args := []any{now, defaultTimeoutSec}
	if projectCode != "" {
		p, err := s.GetProjectByCode(projectCode)
		if err != nil {
			return nil, err
		}
		q += ` AND s.project_id = ?`
		args = append(args, p.ID)
	}
	q += ` ORDER BY s.created_at, s.id`
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询会话列表失败: %w", err)
	}
	defer rows.Close()
	out := make([]Session, 0)
	for rows.Next() {
		se, err := scanSession(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描会话行失败: %w", err)
		}
		out = append(out, se)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历会话列表失败: %w", err)
	}
	return out, nil
}

// ListAliveSessionNames 目标 (project, column, role) 格子的活会话名清单（#9
// direct agent_conflict 拦截数据面，2026-10-03 增补令·role 规约闸）：活=last_seen
// 在失联阈值内，与 GetSession/ListSessions 失联计算列完全同口径（同 strftime
// 秒差算式 + COALESCE(项目级阈值, 服务默认) + p.status='active' 过滤，防口径
// 漂移；阈值 JOIN projects 取列值，sessionColumns 同款）。一条查询同时供
// COUNT（len 返回值）与错误信息列名两用（增补令「store 侧一条 COUNT」——
// 往返账 send 读侧 +1 条）。不 JOIN columns：本查询不取栏目列，轻读面（FK
// 保证 sessions.column_id 有效行存在，省略 INNER JOIN 不丢行，GetSessionByName
// 同论证）。空格子返回非 nil 空切片（ListSessions 惯例，防 nil 语义混淆）；
// ORDER BY s.id 稳定序 = 错误信息列名顺序确定。
func (s *Store) ListAliveSessionNames(projectID, columnID int64, role, now string, defaultTimeoutSec int) ([]string, error) {
	rows, err := s.DB.Query(
		`SELECT s.name
		 FROM sessions s
		 JOIN projects p ON p.id = s.project_id
		 WHERE p.status = 'active'
		   AND s.project_id = ? AND s.column_id = ? AND s.role = ?
		   AND (strftime('%s', ?) - strftime('%s', s.last_seen_at))
		         <= COALESCE(p.heartbeat_timeout_sec, ?)
		 ORDER BY s.id`,
		projectID, columnID, role, now, defaultTimeoutSec,
	)
	if err != nil {
		return nil, fmt.Errorf("store: 查询目标格子活会话失败: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: 扫描活会话名失败: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历活会话名失败: %w", err)
	}
	return out, nil
}
