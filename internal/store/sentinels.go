// sentinels.go —— 哨兵域 store（B3-1）：注册 upsert（B3-T2）/ ping 刷新 / 注销幂等 / 活性单条 SQL 计算列。
// 规格：技术设计 §3.2 表 4（sentinels DDL）、§3.6 哨兵不活跃 SQL、§7.3 哨兵活性；b3-plan B3-1 + B3-T2。
// 纪律：now 全参数注入（零真实墙钟，禁 SQL 内 'now' 字面量）；活性判定单条 SQL 计算列（禁逐行判——§3.7 尾注）。
//
// B3-4 追加（#14/#15/#16 端点消费面）：会话三元组定位（LookupSessionID）/ 栏目存在性
// 探针（LookupColumnID）/ ping+心跳双续期（PingSentinelWithHeartbeat——归属绑定）/ 活哨兵
// 幂等命中查询（FindAliveSentinel）/ poll 命中探测（PollHits——§3.6 谓词自含形态，
// 对齐 status.go B3-3 分层口径）。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// ErrSentinelNotFound 哨兵行不存在哨兵错误（PingSentinel/DeleteSentinel 对未知 id 返回；
// DeleteSentinel 重复注销同口径=幂等 not_found）。
// handler 层据 errors.Is 映射 404 sentinel_not_found（§2.4）。
var ErrSentinelNotFound = errors.New("store: 哨兵不存在")

// Sentinel 哨兵行 + 活性计算列（ListSentinelsWithLiveness 返回视图）。
type Sentinel struct {
	ID         int64
	SessionID  int64  // 所属会话（哨兵挂会话——A9：活性独立信号，不冒充会话心跳）
	ProjectID  int64  // 冗余归属列，自 sessions.project_id 带出
	ColumnID   int64  // 监控的目标信箱栏目
	Role       string // 监控的信箱角色（§3.2 表 4）
	StartedAt  string // 本次值守启动时刻（upsert 复用时刷新——B3-T2）
	LastPingAt string // 每轮 watch 循环刷新（§3.2 表 4）
	Alive      bool   // 活性计算列：now - last_ping_at > timeout → dead；恰等边界=alive（§7.3）
}

// registerSentinelSQL 注册哨兵单语句（1 条往返）：
// INSERT...SELECT 自 sessions 带出 project_id（免前置查询；session 不存在时 SELECT 空集
// → INSERT 0 行 → RETURNING 无行 → sql.ErrNoRows，调用方映射防御错误）；
// ON CONFLICT(session_id,column_id,role) DO UPDATE 刷新 started_at/last_ping_at（B3-T2
// upsert 口径：kill 重启即复活返回同 id、行数不增、无 409 分支——与 sessions upsert 同型）。
// SELECT 必须带 WHERE：SQLite 官方文档要求，规避 INSERT...SELECT...ON CONFLICT 中 ON 关键字
// 的解析歧义（join ON vs upsert ON CONFLICT）。
const registerSentinelSQL = `
INSERT INTO sentinels (session_id, project_id, column_id, role, started_at, last_ping_at)
SELECT ?, s.project_id, ?, ?, ?, ? FROM sessions s WHERE s.id = ?
ON CONFLICT(session_id, column_id, role) DO UPDATE SET
  started_at = excluded.started_at,
  last_ping_at = excluded.last_ping_at
RETURNING id`

// RegisterSentinel 注册哨兵（§2.2 #14 数据面）：sessionID=所属会话，columnID+role=监控的
// 目标信箱；now=服务端注入时钟，首插同时写 started_at 与 last_ping_at。
// 同 (session,column,role) 重复注册：复用既有行返回同 id，started_at/last_ping_at 双双刷新
// 为本次 now（B3-T2 裁定口径），行数不增。
// 签名说明：不含 interval——sentinels 表（B0 冻结 DDL）无 interval 列不落库；interval 相容
// 校验（B3-T4：3×interval ≤ sentinel_timeout 否则 400）属 handler 层职责。
// session 不存在（正常路径由 B1 中间件前置 upsert 保证不发生）→ 防御性错误（包装
// sql.ErrNoRows，调用方 errors.Is 可判别）。
// now 必须为 RFC3339 带时区后缀（Z 或 ±HH:MM）形态；禁无时区本地墙钟串（SQLite 对无时区串
// 一律按 UTC 解释，本地串会静默偏移）。
func (s *Store) RegisterSentinel(sessionID, columnID int64, role string, now string) (int64, error) {
	var id int64
	err := s.DB.QueryRow(registerSentinelSQL,
		sessionID, columnID, role, now, now, sessionID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("store: 会话 %d 不存在，无法注册哨兵: %w", sessionID, sql.ErrNoRows)
	}
	if err != nil {
		return 0, fmt.Errorf("store: 注册哨兵失败: %w", err)
	}
	return id, nil
}

// PingSentinel 刷新哨兵 last_ping_at（§2.2 #15 数据面；每轮 watch 轮询调用，§7.3）。
// 复核钩子（B3-8 清点呈报）：生产消费面暂空（#15 实走 PingSentinelWithHeartbeat，
// 本函数当前仅测试消费）——若后续仍无接线，随下一清退批删（base.md §4.8 死代码
// 纪律；删除时 TestPingSentinel/TestUniqueConstraint 的裸 ping 断言迁至绑定版）。
// 只动 last_ping_at 不动 started_at（启动时刻语义保留）；行不存在 → ErrSentinelNotFound。
func (s *Store) PingSentinel(id int64, now string) error {
	res, err := s.DB.Exec(`UPDATE sentinels SET last_ping_at = ? WHERE id = ?`, now, id)
	if err != nil {
		return fmt.Errorf("store: 哨兵 ping 失败: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: 哨兵 ping 读取影响行数失败: %w", err)
	} else if n == 0 {
		return ErrSentinelNotFound
	}
	return nil
}

// PingSentinelWithHeartbeat 哨兵 ping + 会话心跳双续期（§2.2 #15 T3 三合一的写面——
// 「哨兵活着=会话活着」§7.3 双向隔离的正向面；会话其他 CLI 活动不刷哨兵 ping 为反向面）。
// 同事务原子提交（任一失败整体回滚，两时间列同涨同不涨）。
//
// 归属绑定（审查 I1）：ping 的 WHERE 带 session_id——持 A 会话身份调 B 会话的哨兵 id
// 时 0 行命中 → ErrSentinelNotFound（事务回滚），杜绝「B 被刷 ping + A 被续心跳」的
// T3 语义破洞。与 PingSentinel（裸 id，store 层信任调用方）的 SQL 刻意不共用：
// 两处 WHERE 语义已分化（绑定校验 vs 直达），抽 helper 反而藏住差异。
//
// 错误面：
//   - ErrSentinelNotFound：哨兵行不存在，或存在但不属于该会话（绑定不匹配，同一 404
//     口径——不泄露他者哨兵的存在性）；
//   - 包装 ErrSessionMissing 的防御错误：sessions 行在 ping 与续期之间被外力清理时返回
//     （正常路径 LookupSessionID 先行定位保证不触发；仅数据面被旁路改动时的最后防线，
//     handler 映射同 auth_required 族内部错误——500 口径，不外泄细节）。
//
// B3-8 收敛裁定：保留双入口不合并——#15 的心跳续期与哨兵 ping 必须同事务原子
// （ping 成功心跳才前进，「哨兵活着=会话活着」的 T3 绑定在事务里），B1 中间件
// upsert 路径给不了该绑定；中间件对每请求刷 sessions.last_seen_at 与本函数的
// 会话续期双写同字段幂等（同为 now 覆盖），无冲突面。PingSentinel（裸 id）仅
// 测试/内部面消费，两 WHERE 语义分化见上注，维持独立。
func (s *Store) PingSentinelWithHeartbeat(sentinelID, sessionID int64, now string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("store: 开启双续期事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // 提交后 Rollback 为无害 no-op

	// 哨兵 ping（绑定归属：只续本会话自己的哨兵）。
	res, err := tx.Exec(`UPDATE sentinels SET last_ping_at = ? WHERE id = ? AND session_id = ?`,
		now, sentinelID, sessionID)
	if err != nil {
		return fmt.Errorf("store: 哨兵 ping 失败: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: 哨兵 ping 读取影响行数失败: %w", err)
	} else if n == 0 {
		return ErrSentinelNotFound
	}

	// 会话心跳续期（T3）。
	res, err = tx.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, now, sessionID)
	if err != nil {
		return fmt.Errorf("store: 会话心跳续期失败: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: 会话心跳续期读取影响行数失败: %w", err)
	} else if n == 0 {
		// 防御分支：会话刚 Lookup 过必存在，命中即数据面被外力清理。
		return fmt.Errorf("store: 会话 %d 续期时不存在: %w", sessionID, ErrSessionMissing)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交双续期事务失败: %w", err)
	}
	return nil
}

// DeleteSentinel 注销哨兵（§2.2 #16 数据面；watch 正常退出 / --max-wait 超时先行注销——B3-T3）。
// 幂等口径：行已不存在（重复注销）返回 ErrSentinelNotFound，handler 层映射 404 幂等语义
// （b3-spec #16「幂等 404」——删除效果已达成，非错误分支）。
func (s *Store) DeleteSentinel(id int64) error {
	res, err := s.DB.Exec(`DELETE FROM sentinels WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: 注销哨兵失败: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: 注销哨兵读取影响行数失败: %w", err)
	} else if n == 0 {
		return ErrSentinelNotFound
	}
	return nil
}

// listSentinelsWithLivenessSQL 全量哨兵 + 活性计算列（单条 SQL——§3.7 往返账第 7 条基线，
// 禁逐行判活性）：活性谓词与 §3.6 哨兵 SQL 原样
// （strftime('%s','now') - strftime('%s', last_ping_at) > :sentinel_timeout），唯一偏差=
// 'now' 字面量改绑定参数注入（时钟全注入纪律，与 B1 失联计算同口径）。输出列名 dead，Go 侧翻转存 Alive。
const listSentinelsWithLivenessSQL = `
SELECT id, session_id, project_id, column_id, role, started_at, last_ping_at,
       (strftime('%s', ?) - strftime('%s', last_ping_at)) > ? AS dead
FROM sentinels
ORDER BY id`

// ListSentinelsWithLiveness 返回全量哨兵清单 + 逐行活性标志：now - last_ping_at >
// timeoutSec → dead（Alive=false），否则 alive（恰等边界=alive，§7.3 `>` 口径）。
// 含无自动清理的死行——AC10.3「dead 可见」判据依赖（b3-spec §二：行操作仅 INSERT /
// UPDATE last_ping_at / DELETE，无清理路径）。now 与 timeoutSec 均绑定参数注入。
// now 必须为 RFC3339 带时区后缀（Z 或 ±HH:MM）形态；禁无时区本地墙钟串（SQLite 对无时区串
// 一律按 UTC 解释，本地串会静默偏移）。
func (s *Store) ListSentinelsWithLiveness(now string, timeoutSec int64) ([]Sentinel, error) {
	rows, err := s.DB.Query(listSentinelsWithLivenessSQL, now, timeoutSec)
	if err != nil {
		return nil, fmt.Errorf("store: 哨兵活性查询失败: %w", err)
	}
	defer rows.Close()
	var out []Sentinel
	for rows.Next() {
		var (
			st   Sentinel
			dead bool
		)
		if err := rows.Scan(&st.ID, &st.SessionID, &st.ProjectID, &st.ColumnID, &st.Role,
			&st.StartedAt, &st.LastPingAt, &dead); err != nil {
			return nil, fmt.Errorf("store: 扫描哨兵行失败: %w", err)
		}
		st.Alive = !dead
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历哨兵行失败: %w", err)
	}
	return out, nil
}

// ===== B3-4 追加：#14/#15/#16 端点消费面 =====

// ErrSessionMissing 会话三元组未命中（LookupSessionID 对 (project,column,session) 查无行返回；
// handler 层 errors.Is 映射 401 auth_required）。
//
// B3-8 收敛裁定：保留不并入 B4 ErrSessionNotFound——两哨兵各随其函数（本域
// LookupSessionID vs B4 ResolveSession），合并哨兵须先并函数（见 LookupSessionID
// 注的裁定），强行共用会把 404/401 两侧错误语义绑死在同一个 var 上。
var ErrSessionMissing = errors.New("store: 会话三元组未命中")

// SessionLoc 会话定位结果：sessions.id + 归属 project_id/column_id——poll 探测一次定位
// 拿全三个 id（§3.6 谓词 :p/:c/:s 一查齐备，免二次查库）。
type SessionLoc struct {
	SessionID int64
	ProjectID int64
	ColumnID  int64
}

// lookupSessionSQL 会话三元组定位单条（sessions UNIQUE(project_id,column_id,name) 保证单行；
// JOIN projects/columns 按 code 定位——project/column code 查无同样 0 行，统一
// ErrSessionMissing：三元组任一环缺失=会话不存在，身份语义单一出口）。
const lookupSessionSQL = `
SELECT s.id, s.project_id, s.column_id
FROM sessions s
JOIN projects p ON p.id = s.project_id
JOIN columns c ON c.id = s.column_id
WHERE p.code = ? AND c.code = ? AND s.name = ?`

// LookupSessionID 按 (project, column, session) 三元组定位会话（#15 身份门槛+探测定位
// 合一查询；#14 亦可复用）。查无 → ErrSessionMissing。
//
// B3-8 收敛裁定：保留不收敛为 B4 ResolveSession——ResolveSession 仅返回会话 id，
// 本函数返回 SessionLoc 三 id 一查齐备（#15 探测谓词 :p/:c/:s 所需，§3.7 往返账
// 「一查齐备免二次查库」）；改调 ResolveSession 须再加 ResolveColumn/项目存在性
// 两查（+2 往返）或改 B4 跨批签名。两函数 SQL 同构但返回面不同层，非双胞胎。
func (s *Store) LookupSessionID(projectCode, columnCode, sessionName string) (SessionLoc, error) {
	var loc SessionLoc
	err := s.DB.QueryRow(lookupSessionSQL, projectCode, columnCode, sessionName).
		Scan(&loc.SessionID, &loc.ProjectID, &loc.ColumnID)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionLoc{}, fmt.Errorf("store: 三元组 (%s, %s, %s) 未命中: %w",
			projectCode, columnCode, sessionName, ErrSessionMissing)
	}
	if err != nil {
		return SessionLoc{}, fmt.Errorf("store: 会话定位查询失败: %w", err)
	}
	return loc, nil
}

// lookupColumnSQL #14 栏目存在性探针单条（项目 JOIN 栏目按 code 定位；项目不存在同样
// 0 行，与栏目缺失同映射 column_not_found——「Column 头在 project 域不存在」单一语义）。
//
// 审查 K1/M-L2 勘误后退化：B3-T4 闸值=服务配置 sentinel_timeout_sec（config.Watch.
// SentinelTimeoutSec，无项目级覆盖列——schema 无该覆盖面），不再需要此查询带出项目
// 失联阈值，故删 COALESCE 阈值输出面（旧 LookupColumnTimeout），保留纯存在性探针。
const lookupColumnSQL = `
SELECT c.id
FROM projects p
JOIN columns c ON c.project_id = p.id
WHERE p.code = ? AND c.code = ?`

// LookupColumnID 探测 (project, column) 栏目存在性（#14 前置校验面），返回栏目 id。
// 栏目（或项目）查无 → 包装 sql.ErrNoRows（调用方 errors.Is 判别映射 404 column_not_found）。
func (s *Store) LookupColumnID(projectCode, columnCode string) (int64, error) {
	var columnID int64
	err := s.DB.QueryRow(lookupColumnSQL, projectCode, columnCode).Scan(&columnID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("store: 栏目 (%s, %s) 未命中: %w", projectCode, columnCode, sql.ErrNoRows)
	}
	if err != nil {
		return 0, fmt.Errorf("store: 栏目存在性探针查询失败: %w", err)
	}
	return columnID, nil
}

// findAliveSentinelSQL 活哨兵幂等命中单查（#14 注册幂等判定——信箱#12：活哨兵在=
// 幂等返回既有 id 不新建；b8-W2 质量审查修 1 单查化：JOIN sessions 同语句带出持有者
// 诊断三列）。活性谓词与 ListSentinelsWithLiveness 同款（now - last_ping_at > timeout
// 为 dead，恰等=alive），取 `<=` 即 alive 侧；now 为绑定参数注入（时钟全注入纪律，
// age_sec 计算列与活性谓词同注入值同源算式）。
//
// 竞态不可达说明（质量审查修 1 语义锚）：id 与持有者三列同语句原子读——不存在
// 「FindAlive 命中后、二次取持有者前行被并发 DELETE」的窗口（单条 SELECT 内 SQLite
// 语句级一致性），200 路径故无 ErrNoRows→500 分支；未命中（无行/死行/持有者会话
// JOIN 缺行）统一 alive=false 零值返回。
const findAliveSentinelSQL = `
SELECT sen.id, se.name, sen.last_ping_at,
       (strftime('%s', ?) - strftime('%s', sen.last_ping_at)) AS age_sec
FROM sentinels sen
JOIN sessions se ON se.id = sen.session_id
WHERE sen.session_id = ? AND sen.column_id = ? AND sen.role = ?
  AND (strftime('%s', ?) - strftime('%s', sen.last_ping_at)) <= ?
LIMIT 1`

// SentinelHolder 活哨兵命中行视图（#14 幂等命中 200 响应数据面）：既有哨兵行 id +
// 持有者诊断三列（session 名/last_ping_at/age_sec）。未命中时零值返回（调用方以
// alive=false 判别不消费字段面）。
type SentinelHolder struct {
	ID         int64
	Session    string
	LastPingAt string
	AgeSec     int64
}

// FindAliveSentinel 查同坐标活哨兵（#14 幂等命中面，b8-W2 起带持有者诊断）：命中
// 返回 (行视图, true, nil)——id 供 200 sentinel_id 回显、三列供 holder 诊断；无活哨兵
// （含无行/行已死两态）返回 (零值, false, nil)。now=注入时钟（RFC3339 带时区后缀
// 形态）；timeoutSec=哨兵失活阈值（与 ListSentinelsWithLiveness 同一口径值）。
func (s *Store) FindAliveSentinel(sessionID, columnID int64, role, now string, timeoutSec int64) (SentinelHolder, bool, error) {
	var h SentinelHolder
	err := s.DB.QueryRow(findAliveSentinelSQL,
		now, sessionID, columnID, role, now, timeoutSec,
	).Scan(&h.ID, &h.Session, &h.LastPingAt, &h.AgeSec)
	if errors.Is(err, sql.ErrNoRows) {
		return SentinelHolder{}, false, nil
	}
	if err != nil {
		return SentinelHolder{}, false, fmt.Errorf("store: 活哨兵查询失败: %w", err)
	}
	return h, true, nil
}

// ===== b8-W2/W3 追加：锁接管 / 命中留痕 =====

// takeoverSentinelSQL 接管插入单句（b8-W2）：与 registerSentinelSQL 同构但无 ON
// CONFLICT——同事务内旧行已 DELETE，冲突不可能发生（单连接池写路径串行化）；
// INSERT...SELECT 自 sessions 带出 project_id，session 不存在时空集插 0 行 →
// RETURNING 无行 → sql.ErrNoRows（防御口径与 RegisterSentinel 同）。
const takeoverSentinelSQL = `
INSERT INTO sentinels (session_id, project_id, column_id, role, started_at, last_ping_at)
SELECT ?, s.project_id, ?, ?, ?, ? FROM sessions s WHERE s.id = ?
RETURNING id`

// TakeoverSentinel --force 显式接管（b8-W2 #14 force 分流数据面）：同事务 DELETE
// 同坐标旧行 + INSERT 新行——新 id 由 AUTOINCREMENT 产出不复用旧值，旧哨兵进程
// 下次 poll 0 行命中 → ErrSentinelNotFound → 404 sentinel_not_found 自然退出，
// 一格一哨单例恢复。无旧行坐标（含并发窗口被先行注销）= DELETE 0 行照常插入
// （普通注册语义，force no-op 面与 handler「未命中走既有注册路径」分层一致）。
// session 不存在（正常路径 B1 中间件保证不发生）→ 防御错误不落行。
// now=注入时钟（RFC3339 带时区后缀形态）；started_at/last_ping_at 双写 now。
func (s *Store) TakeoverSentinel(sessionID, columnID int64, role, now string) (int64, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: 开启接管事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // 提交后 Rollback 为无害 no-op

	// 旧行注销（按锁键三元组全行删——活行死行一体，接管的语义是「这个格子归我」）。
	if _, err := tx.Exec(`DELETE FROM sentinels WHERE session_id = ? AND column_id = ? AND role = ?`,
		sessionID, columnID, role); err != nil {
		return 0, fmt.Errorf("store: 接管注销旧行失败: %w", err)
	}

	var id int64
	if err := tx.QueryRow(takeoverSentinelSQL,
		sessionID, columnID, role, now, now, sessionID,
	).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("store: 会话 %d 不存在，无法接管注册哨兵: %w", sessionID, sql.ErrNoRows)
		}
		return 0, fmt.Errorf("store: 接管注册哨兵失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: 提交接管事务失败: %w", err)
	}
	return id, nil
}

// MarkSentinelLastHit 命中留痕（b8-W3 #15 数据面）：哨兵 poll 非空 hits 时回写
// 该会话行 sentinel_last_hit_at（命中即注销哨兵行——留痕落会话行不落哨兵行）。
// handler 尽力面调用：本函数出错仅 slog.Warn 降级，不阻塞 #15 响应（b7
// cross_grid_hint 降级哲学同款）。
// 会话 0 行命中（Lookup 与留痕间被外力清理的防御分支）→ 错误（降级留痕来源）。
// at=注入时钟（RFC3339 带时区后缀形态）。
func (s *Store) MarkSentinelLastHit(sessionID int64, at string) error {
	res, err := s.DB.Exec(`UPDATE sessions SET sentinel_last_hit_at = ? WHERE id = ?`, at, sessionID)
	if err != nil {
		return fmt.Errorf("store: 命中留痕写入失败: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: 命中留痕读取影响行数失败: %w", err)
	} else if n == 0 {
		return fmt.Errorf("store: 会话 %d 留痕时不存在: %w", sessionID, ErrSessionMissing)
	}
	return nil
}

// Hit poll 命中摘要（§2.2 #15 hits 元素：seq/level——无 body，探测面零载荷）。
// b7-W1 扩列：Kind/TargetRole 随行带出（watch 命中标注 + #15 hits 元素 additive
// 新键的 JSON 数据面），谓词零改动纯扩列。
type Hit struct {
	Seq        int64  `json:"seq"`
	Level      string `json:"level"`
	Kind       string `json:"kind"`
	TargetRole string `json:"target_role"`
}

// chatConsumerPrefix 会话对话位点 consumer 前缀（§3.2 表 7：chat:session:<id>）。
const chatConsumerPrefix = "chat:session:"

// pollHitsSQL poll 命中探测单条（§3.6 可见性谓词三分支自含，谓词与位点子查询均按
// 冻结 SQL 原样；位点缺行 COALESCE 0=全量可见——新哨兵首探不漏位点行缺失前的
// 历史，与 B2 GetPositions 惰性初始化「缺行落 MAX(seq) 不回看」语义相反，探测面
// 有意为之：#15 零写，位点行落位只归 poll 命令链）：
//   - direct：目标栏目+角色，位点=信箱角色位点（consumer=:r）
//   - bus：项目域广播，仅 controller 可见（:r='controller' 短路），位点复用 controller 信箱位点
//   - chat/receipt：目标会话对话流，位点=chat:session:<id>，自己的回复不回流（sender≠:s）
//
// 纯 SELECT 零写面——AC10.4 hits 探测不推进位点（推进归 #11 ack 端点）。
//
// b7-W1 扩列注：SELECT 增 m.kind, m.target_role 两列（命中 kind 标注与跨格防呆的
// 数据面），WHERE/三分支谓词与位点子查询逐字不动。
//
// B3-8 收敛裁定：保留自含实现，不收敛为 B2 GetPositions+PollVisible 链——
// ①探测必须零写（AC10.4），GetPositions 位点缺行即惰性 upsert 落行=写面；
// ②位点缺行语义相反（本 SQL 全量可见 vs 惰性初始化不回看），互换即行为回归；
// ③位点子查询内联保持单条往返，拆链变两查。谓词三分支与 §3.6/B2 pollVisibleSQL
// 逐字同源，双路径显式登记非静默。
const pollHitsSQL = `
SELECT m.seq, m.level, m.kind, m.target_role
FROM messages m
WHERE (m.kind = 'direct' AND m.column_id = ? AND m.target_role = ?
        AND m.seq > COALESCE((SELECT position FROM ack_positions
                              WHERE column_id = ? AND consumer = ?), 0))
   OR (m.kind = 'bus' AND m.project_id = ? AND ? = 'controller'
        AND m.seq > COALESCE((SELECT position FROM ack_positions
                              WHERE column_id = ? AND consumer = 'controller'), 0))
   OR (m.kind IN ('chat','receipt') AND m.target_session_id = ?
        AND m.sender_session_id != ?
        AND m.seq > COALESCE((SELECT position FROM ack_positions
                              WHERE column_id = ? AND consumer = ?), 0))
ORDER BY m.seq ASC`

// PollHits 探测位点之后的新消息（§2.2 #15 探测面）：direct 三分支 + bus 仅 controller +
// chat 对话流（§3.6 冻结谓词），seq 升序返回；无命中返回空切片（json []，非 null）。
// 纯读不写位点（AC10.4——位点推进只归 #11 ack）。b7-W1 扩列：返回值带 kind/target_role。
func (s *Store) PollHits(projectID, columnID int64, role string, sessionID int64) ([]Hit, error) {
	rows, err := s.DB.Query(pollHitsSQL,
		columnID, role, columnID, role, // direct 分支（位点子查询复用同参）
		projectID, role, columnID, // bus 分支（role 仅作 controller 开关）
		sessionID, sessionID, columnID, chatConsumerPrefix+strconv.FormatInt(sessionID, 10), // chat 分支
	)
	if err != nil {
		return nil, fmt.Errorf("store: poll 命中探测查询失败: %w", err)
	}
	defer rows.Close()
	hits := []Hit{} // 空命中=json []（契约「hits:[] 或 [...]」，禁 null）
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Seq, &h.Level, &h.Kind, &h.TargetRole); err != nil {
			return nil, fmt.Errorf("store: 扫描 poll 命中行失败: %w", err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 poll 命中行失败: %w", err)
	}
	return hits, nil
}
