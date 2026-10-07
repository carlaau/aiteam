package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"aiteam/internal/types"
)

// 消息 kind 枚举（DDL CHECK (kind IN ('direct','bus','chat','receipt'))，§3.2 表 5）。
const (
	MessageKindDirect  = "direct"  // 定向：column_id=目标栏目，target_role=目标角色
	MessageKindBus     = "bus"     // 总线：column_id=发送方所在栏目（审计锚点），项目级可见
	MessageKindChat    = "chat"    // 看板对话：column_id=目标会话所属栏目，target_session_id=目标会话
	MessageKindReceipt = "receipt" // 回执回告：column_id=原发送方会话栏目，target_session_id=原发送方会话
)

// 消息 level 枚举（DDL CHECK (level IN ('normal','important','block'))）。
const (
	MessageLevelNormal    = "normal"
	MessageLevelImportant = "important"
	MessageLevelBlock     = "block"
)

// poll/history/dialog 的 limit 约定常量：
//   - DefaultPollLimit：poll 缺省上限（D8「poll 默认 500 条分页」）——store 的
//     PollVisible 不私设默认（0=全量是其谓词语义），此常量供 handler/CLI 缺省注入；
//   - DefaultHistoryLimit / MaxHistoryLimit：history 端点 #13「limit(默认 50,最大 500)」；
//   - DefaultDialogLimit：看板对话流 #25「limit(默认 100)」（§2.2 #25）。
const (
	DefaultPollLimit    = 500
	DefaultHistoryLimit = 50
	MaxHistoryLimit     = 500
	DefaultDialogLimit  = 100
)

// messages 域哨兵错误：
//   - ErrMessageInvalid 产于写路径（InsertMessage 参数校验兜底，handler 主责）——
//     B2-3 handler 映射 400 时须同步 writeStoreError 映射与
//     write_store_error_test.go 哨兵清单（机械闸注释约定）；
//   - ErrMessageNotFound 产于读路径（GetMessageBySeq，B2-5 #12 校验链首步）——
//     写路径语义（回执前提存在性），handler 经 writeStoreError 映射 404
//     message_not_found，新增哨兵须同步映射与哨兵清单（同上机械闸）；
//   - ErrInvalidLimit 产于 PollVisible（limit<0，D8「负值报错」）——可识别错误供
//     handler 映射 400；
//   - ErrInvalidOrder 产于 QueryHistory（order 白名单外——值拼入 ORDER BY 文本，
//     必须白名单校验防注入）。
var (
	ErrMessageInvalid  = errors.New("消息参数无效")
	ErrMessageNotFound = errors.New("消息不存在")
	ErrInvalidLimit    = errors.New("limit 参数非法")
	ErrInvalidOrder    = errors.New("order 参数非法")
)

// Message messages 表一行（§3.2 表 5 字段面；五要素=seq/level/body/sender 身份
// [sender_label+sender_session_id]/created_at——AC8.4 全齐，另含定位字段）。
type Message struct {
	Seq             int64 // 全局唯一序号（AUTOINCREMENT 取号，FR5 服务端生成）
	ProjectID       int64
	ColumnID        int64  // 归属栏目（kind 归属规则见 §3.2 表 5 注）
	Kind            string // direct | bus | chat | receipt
	TargetRole      string // direct：目标角色；其余 ''
	TargetSessionID int64  // chat/receipt：目标会话；其余 0（弱关联）
	SenderSessionID int64  // 发送方会话（0=系统/看板，弱关联）
	SenderLabel     string // 冗余显示：'controller-A@05' / 'board-user:张三' / 'system'
	Level           string // normal | important | block
	Body            string
	CreatedAt       string // 服务端时间（AC5.4，types.NowUTC 注入）
}

// MessageInput InsertMessage 入参：column_id 与 target 字段由调用方按 kind 归属
// 规则算好传入（§3.2 表 5 注四 kind 规则——推导逻辑归 send handler，store 纯落库，
// 不反推 kind 语义）。seq/created_at 服务端生成，不入参。
type MessageInput struct {
	ProjectID       int64
	ColumnID        int64
	Kind            string
	TargetRole      string
	TargetSessionID int64
	SenderSessionID int64
	SenderLabel     string
	Level           string
	Body            string
	// FromProject 跨项目审计面（b5-W1/FR9，v3 列）：跨项目投递=发送方项目 code，
	// ''=项目内投递（零值即既有语义——InsertReceiptWithNotify/InsertBoardMessageWithAudit
	// 两复用点不设此字段，天然落 ''）。本批零读面消费方（spec §1.1-W1③ 冻结：
	// messageSelectColumns/scanMessage 不随动）。
	FromProject string
}

// messageColumns 裸列清单（INSERT...RETURNING 用，与 scanMessage 的 Scan 顺序
// 一一对应，序=schema.sql messages 建表列序——B0 冻结 DDL，迁移 append-only，
// 列序稳定）。SELECT 读路径禁用 m.*（迁移加列时展开序漂移会炸整个消息读路径），
// 一律用 messageSelectColumns 前缀版。
const messageColumns = `seq, project_id, column_id, kind, target_role, target_session_id,
       sender_session_id, sender_label, level, body, created_at`

// messageSelectColumns SELECT 前缀版列清单（PollVisible/QueryHistory 读路径用）：
// 显式列冻结读面——后续迁移追加列时本清单不漂移，scanMessage Scan 顺序恒定
// （新列按需追加本清单+Scan 位，机械可审）。列序与 messageColumns 一致。
const messageSelectColumns = `m.seq, m.project_id, m.column_id, m.kind, m.target_role,
       m.target_session_id, m.sender_session_id, m.sender_label, m.level, m.body, m.created_at`

// scanMessage 从一行解出 Message（scan 兼容 *sql.Row 与 *sql.Rows 的 Scan 签名）。
func scanMessage(scan func(dest ...any) error) (Message, error) {
	var m Message
	err := scan(&m.Seq, &m.ProjectID, &m.ColumnID, &m.Kind, &m.TargetRole,
		&m.TargetSessionID, &m.SenderSessionID, &m.SenderLabel,
		&m.Level, &m.Body, &m.CreatedAt)
	if err != nil {
		return Message{}, err
	}
	return m, nil
}

// insertMessage 在 q（连接池或事务）上执行消息 INSERT...RETURNING（InsertMessage
// 与 InsertReceiptWithNotify 单源共享，勿复制 SQL——q 抽象同 insertProject 惯例）。
// 参数校验为数据面兜底（handler 主责）：kind/level 出枚举或 body 空/纯空白返回
// ErrMessageInvalid。created_at=types.NowUTC 注入时钟（AC5.4）。
func insertMessage(q queryRower, in MessageInput) (Message, error) {
	if !slices.Contains([]string{MessageKindDirect, MessageKindBus, MessageKindChat, MessageKindReceipt}, in.Kind) {
		return Message{}, fmt.Errorf("%w: kind %q（合法值 direct/bus/chat/receipt）", ErrMessageInvalid, in.Kind)
	}
	if !slices.Contains([]string{MessageLevelNormal, MessageLevelImportant, MessageLevelBlock}, in.Level) {
		return Message{}, fmt.Errorf("%w: level %q（合法值 normal/important/block）", ErrMessageInvalid, in.Level)
	}
	if strings.TrimSpace(in.Body) == "" {
		return Message{}, fmt.Errorf("%w: body 为空", ErrMessageInvalid)
	}
	now := types.NowUTC()
	// from_project 三处同步点之一（列清单/VALUES 占位/绑定参数，b5-W1）：显式列名
	// INSERT 不依赖表列序，放 created_at 前仅随读面列序习惯；RETURNING 列清单
	// （messageColumns）本批不扩（spec §1.1-W1③ 冻结）。
	m, err := scanMessage(q.QueryRow(
		`INSERT INTO messages (project_id, column_id, kind, target_role, target_session_id,
		       sender_session_id, sender_label, level, body, from_project, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 RETURNING `+messageColumns,
		in.ProjectID, in.ColumnID, in.Kind, in.TargetRole, in.TargetSessionID,
		in.SenderSessionID, in.SenderLabel, in.Level, in.Body, in.FromProject, now,
	).Scan)
	if err != nil {
		return Message{}, fmt.Errorf("store: 插入消息 kind=%s 失败: %w", in.Kind, err)
	}
	return m, nil
}

// InsertMessage 插入一条消息（§2.2 #9/#12/#24 的数据面）：单条 INSERT，seq 交
// AUTOINCREMENT 取号（FR5 全局唯一、不复用），经 RETURNING 返回库中真实行
// （含服务端生成的 seq/created_at）。append-only（§3.3 触发器禁改删）——状态
// 流转一律写独立表，绝不 UPDATE 本表。事务内复用走 insertMessage 底座。
func (s *Store) InsertMessage(in MessageInput) (Message, error) {
	return insertMessage(s.DB, in)
}

// MessageDomain messages 行+归属域状态（GetMessageBySeq 专用读面）：#12 回执
// 校验链一次查询带回消息本体与项目/栏目状态——存在性 404、not_block_level 409、
// archived 409（B2-T2 动作端点判）三步全靠此行，免三次读往返。
type MessageDomain struct {
	Message
	ProjectStatus string // 归属项目状态（active/archived）
	ColumnStatus  string // 归属栏目状态（active/archived）
}

// GetMessageBySeq 按 seq 精确取消息行+域状态（B2-5 #12 校验链数据面）：JOIN
// projects/columns 装配状态列（INNER JOIN 不丢行——messages 两列均 NOT NULL
// FK）。不筛 status（存在性优先于 archived 判定，404/409 分工见 MessageDomain
// 注）。未命中返回 ErrMessageNotFound。
func (s *Store) GetMessageBySeq(seq int64) (MessageDomain, error) {
	var md MessageDomain
	err := s.DB.QueryRow(
		`SELECT `+messageSelectColumns+`, p.status, c.status
		 FROM messages m
		 JOIN projects p ON p.id = m.project_id
		 JOIN columns c ON c.id = m.column_id
		 WHERE m.seq = ?`,
		seq,
	).Scan(&md.Seq, &md.ProjectID, &md.ColumnID, &md.Kind, &md.TargetRole,
		&md.TargetSessionID, &md.SenderSessionID, &md.SenderLabel,
		&md.Level, &md.Body, &md.CreatedAt, &md.ProjectStatus, &md.ColumnStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageDomain{}, fmt.Errorf("%w: seq=%d", ErrMessageNotFound, seq)
	}
	if err != nil {
		return MessageDomain{}, fmt.Errorf("store: 查询消息 seq=%d 失败: %w", seq, err)
	}
	return md, nil
}

// pollVisibleSQL poll 可见性谓词（§3.6 逐字对拍，命名占位符 :x → ? 绑定参数）：
//
//	参数：pos_m=信箱位点, pos_d=对话位点, ?1 栏目, ?2 角色, ?3 项目, ?4/5 会话 id,
//	     ?6=角色（bus 分支 :r = 'controller' 判断——按设计注「:bus_role 判断在 Go 侧」
//	     参数化保留在谓词内，role 经绑定参数传入，无字符串拼接）。
//
// 三分支语义：
//   - direct：栏目+角色命中且 seq > 信箱位点；
//   - bus：同项目且 seq > 信箱位点且角色为 controller（AC6.2 跨栏目可见、D7 不回看
//     由位点值保证——位点=调用方传入，B2-2 GetPositions/惰性初始化提供）；
//   - chat/receipt：目标会话命中且排除自发（自己的回复不回流，§2.2 #24 语义）
//     且 seq > 对话位点（AC15.4 会话隔离）。
//
// LIMIT 由 PollVisible 按入参追加（D8：0=全量不追加，>0 追加 LIMIT ?）。
//
// 已知代价（审查登记，量级上来再议）：三分支 OR 合一随数据量线性扫（bus 分支
// 有 idx_messages_bus 部分索引支撑，direct/chat 各有前缀索引）；量大后可演进为
// 三分支 UNION ALL 归并各走索引——当前单机 SQLite+消息量级（万级）下 OR 形态
// 足够，且保持 §3.6 谓词逐字对拍。
const pollVisibleSQL = `SELECT ` + messageSelectColumns + ` FROM messages m
WHERE (m.kind = 'direct' AND m.column_id = ? AND m.target_role = ? AND m.seq > ?)
   OR (m.kind = 'bus'    AND m.project_id = ? AND m.seq > ? AND ? = 'controller')
   OR (m.kind IN ('chat','receipt') AND m.target_session_id = ?
       AND m.sender_session_id != ? AND m.seq > ?)
ORDER BY m.seq ASC`

// PollVisible poll 可见性查询（§2.2 #10 数据面，§3.6 谓词单条 SQL 三分支原样）：
// 拉取不推进位点（幂等重拉 AC8.1——位点读写归 B2-2，本函数只按传入位点值过滤）。
//
// 参数（谓词三分支参数化，语义见 pollVisibleSQL）：
//   - projectID：项目 id（bus 分支 :p）；
//   - columnID：栏目 id（direct 分支 :c——消费方所在信箱栏目）；
//   - role：消费方角色（direct 命中键 + bus 的 controller 闸，同一参数两处绑定）；
//   - sessionID：消费方会话 id（chat/receipt 分支 :s）；
//   - mailboxPos：信箱位点 pos_m（direct+bus 共用，seq 严格大于）；
//   - dialogPos：对话位点 pos_d（chat/receipt 用，seq 严格大于）；
//   - limit：>0=LIMIT n；0=全量（D8 显式 0 要全量）；<0=ErrInvalidLimit。
//
// 结果按 seq 升序（AC8.1）；空命中返回非 nil 空切片（JSON null 防线）。
func (s *Store) PollVisible(projectID, columnID int64, role string, sessionID, mailboxPos, dialogPos int64, limit int) ([]Message, error) {
	if limit < 0 {
		return nil, fmt.Errorf("%w: %d（负值非法；0=全量，正数=条数）", ErrInvalidLimit, limit)
	}
	q := pollVisibleSQL
	args := []any{columnID, role, mailboxPos, projectID, mailboxPos, role, sessionID, sessionID, dialogPos}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: poll 可见性查询失败: %w", err)
	}
	defer rows.Close()
	out := make([]Message, 0)
	for rows.Next() {
		m, err := scanMessage(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描 poll 消息行失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 poll 消息失败: %w", err)
	}
	return out, nil
}

// ---- 跨格未消费 direct 计数（b7-W1 消费侧自诊断数据面，b7-spec §二.1）----

// CrossGridPending 跨格未消费 direct 计数行（CrossGridDirectPending 返回面）：
// 他格 role 名 + 该格位点之后的未消费 direct 条数——服务端据此装配 #10 cross_grid_hint
// （`[]{role,pending}` 最小面，零正文/零 level/零时间戳泄漏，b7 AC5）。
// 本类型不带 json tag（store 层类型惯例）——wire 键名 role/pending 归 #10 handler
// 响应结构定义，勿直透序列化（会成 Role/Pending）。
type CrossGridPending struct {
	Role    string // 他格角色（messages.target_role）
	Pending int    // 该格未消费 direct 条数（COUNT(*)）
}

// crossGridDirectPendingSQL 同栏目他格未消费 direct 计数（b7-spec §二.1 冻结 SQL
// 逐字）：位点感知口径=consumer 为目标 role 的 ack 位点（与 pollHitsSQL direct 分支
// 同口径），位点行缺失 COALESCE 0=全量计数（与探测面「缺行全量可见」一致）；GROUP BY
// 只输出计数 ≥1 的 role（清零格自然不成行）；ORDER BY COUNT(*) DESC 降序（并列序
// 不定，服务端截断 Top N 语义不受影响）；column_id 全局唯一自增主键，天然圈定项目域。
// idx_messages_direct(column_id,target_role,seq) 支撑；纯 SELECT 零写面（探测零写
// 纪律同 PollHits）。归属裁量注：落 messages.go 非 sentinels.go——消费方是 #10 信箱
// poll handler（cross_grid_hint 装配），与 PollVisible 同端点域；sentinels.go 组织
// 围绕哨兵行与 #14/#15/#16 端点，本查询无哨兵语义。
const crossGridDirectPendingSQL = `
SELECT m.target_role, COUNT(*)
FROM messages m
WHERE m.kind = 'direct' AND m.column_id = ?
  AND m.target_role != ?
  AND m.seq > COALESCE((SELECT position FROM ack_positions
                        WHERE column_id = m.column_id AND consumer = m.target_role), 0)
GROUP BY m.target_role
ORDER BY COUNT(*) DESC`

// CrossGridDirectPending 同栏目他格未消费 direct 计数（b7-W1）：columnID=被查栏目，
// excludeRole=调用方自己的 role（自己格不计入——提示面只报他格，b7 AC2 零噪音面）。
// 空态返回非 nil 空切片（JSON [] 防线，同 PollVisible/PollHits 惯例）。
func (s *Store) CrossGridDirectPending(columnID int64, excludeRole string) ([]CrossGridPending, error) {
	rows, err := s.DB.Query(crossGridDirectPendingSQL, columnID, excludeRole)
	if err != nil {
		return nil, fmt.Errorf("store: 跨格未消费 direct 计数查询失败: %w", err)
	}
	defer rows.Close()
	out := []CrossGridPending{} // 空态=json []（禁 null，同 PollHits 惯例）
	for rows.Next() {
		var p CrossGridPending
		if err := rows.Scan(&p.Role, &p.Pending); err != nil {
			return nil, fmt.Errorf("store: 扫描跨格计数行失败: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历跨格计数行失败: %w", err)
	}
	return out, nil
}

// HistoryFilter #13 历史查询过滤参数（§2.2 #13 query 面全量；零值字段=不过滤）。
type HistoryFilter struct {
	ProjectCode string // 项目 code 过滤；空=不过滤；未命中=空结果（#13 无 404 语义）
	ColumnCode  string // 栏目 code 过滤；空=不过滤；与 ProjectCode 同给时按项目内定位
	Kind        string // kind 过滤；空=不过滤；非法值=空结果非报错（SQL 等值匹配自然落空）
	Level       string // level 过滤；空=不过滤；非法值=空结果非报错（同上）
	SentBy      string // 发送方会话名过滤；空=不过滤；未命中=空结果；
	//   同名会话跨项目/栏目全命中（宽口径——sessions.name 仅 (project,column) 内
	//   唯一，#13 未限定 sent_by 与 project 的联动窄化语义）
	SinceSeq  int64  // 0=不过滤；>0 时 seq >= SinceSeq（含起点）
	BeforeSeq int64  // 0=不过滤；>0 时 seq < BeforeSeq（不含）——与 SinceSeq 互补成区间
	Limit     int    // <=0=DefaultHistoryLimit(50)；>MaxHistoryLimit(500) 截到 500
	Order     string // ""或"desc"=seq 降序（最新在前）；"asc"=升序；其他=ErrInvalidOrder
}

// QueryHistory 通用历史/审计查询（§2.2 #13 数据面，AC1.3/AC2.3 历史可查）：单条
// SELECT + LIMIT。archived 域不拒（projects/columns 不筛 status）；code/sent_by
// 未命中经子查询自然落空（空结果非 404——#13 特有错误仅通用组）。默认 seq 降序
// 取最新 50 条；seq 单调递增=时间序（§3.1 第 3 条），排序键用 seq 比.created_at
// 文本序更稳（同秒多消息有全序）。
func (s *Store) QueryHistory(f HistoryFilter) ([]Message, error) {
	// order 白名单：值拼入 SQL 文本（LIMIT 前无参数位），必须枚举校验防注入。
	order := "DESC"
	switch f.Order {
	case "", "desc":
	case "asc":
		order = "ASC"
	default:
		return nil, fmt.Errorf("%w: %q（合法值 desc/asc）", ErrInvalidOrder, f.Order)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	if limit > MaxHistoryLimit {
		limit = MaxHistoryLimit
	}

	// WHERE 片段白名单拼接（固定文本+? 占位，无外部输入进 SQL 结构）。
	var conds []string
	var args []any
	if f.Kind != "" {
		conds = append(conds, `m.kind = ?`)
		args = append(args, f.Kind)
	}
	if f.Level != "" {
		conds = append(conds, `m.level = ?`)
		args = append(args, f.Level)
	}
	if f.SinceSeq > 0 {
		conds = append(conds, `m.seq >= ?`)
		args = append(args, f.SinceSeq)
	}
	if f.BeforeSeq > 0 {
		conds = append(conds, `m.seq < ?`)
		args = append(args, f.BeforeSeq)
	}
	if f.ProjectCode != "" {
		conds = append(conds, `m.project_id = (SELECT id FROM projects WHERE code = ?)`)
		args = append(args, f.ProjectCode)
	}
	if f.ColumnCode != "" {
		if f.ProjectCode != "" {
			// 项目内定位（栏目 code 项目内唯一）：未命中（项目或栏目不存在）子查询
			// 返回 NULL，自然落空结果。
			conds = append(conds, `m.column_id = (SELECT c.id FROM columns c
			 WHERE c.code = ? AND c.project_id = (SELECT id FROM projects WHERE code = ?))`)
			args = append(args, f.ColumnCode, f.ProjectCode)
		} else {
			// 未给项目：全局同名栏目全命中（宽口径，#13 未限定此组合语义）。
			conds = append(conds, `m.column_id IN (SELECT id FROM columns WHERE code = ?)`)
			args = append(args, f.ColumnCode)
		}
	}
	if f.SentBy != "" {
		// 宽口径：同名会话跨项目/栏目全命中（sessions.name 仅 (project,column)
		// 内唯一，#13 未限定 sent_by 与 project 联动窄化）。
		conds = append(conds, `m.sender_session_id IN (SELECT id FROM sessions WHERE name = ?)`)
		args = append(args, f.SentBy)
	}

	q := `SELECT ` + messageSelectColumns + ` FROM messages m`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	q += ` ORDER BY m.seq ` + order + ` LIMIT ?`
	args = append(args, limit)

	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 历史查询失败: %w", err)
	}
	defer rows.Close()
	out := make([]Message, 0)
	for rows.Next() {
		m, err := scanMessage(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描历史消息行失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历历史消息失败: %w", err)
	}
	return out, nil
}

// QueryDialog 看板对话流查询（§2.2 #25 数据面）：target_session_id 维度追加谓词
// （idx_messages_chat 部分索引支撑）。全量流视图——kind='chat' AND target_session_id=?
// 两端消息都展示（board 发送 sender_session_id=0 与 agent 回复 sender≠0，from_board
// 判据归 handler 装配；与 poll 谓词「自发不回流」不冲突：那是拉取通道语义，本查询
// 是全流视图，b5-spec §2.2 #25 明文）。seq 升序（对话时间线正序，§3.1 第 3 条
// seq 单调递增=时间序）。
//
// limit：<=0=DefaultDialogLimit(100)（防御调用方漏传，QueryHistory 同惯例）；
// 显式上限不做 store 层钳制（§2.2 #25 未定义最大值，JS 恒传 100）。
// 空命中返回非 nil 空切片（JSON null 防线）。
//
// Deprecated: deprecated_for_board——看板 #25 数据面由 QuerySessionTimeline 承接
// （b3 §9 清退，本批任务 2 落地），本体保留供既有调用面与测试。
func (s *Store) QueryDialog(sessionID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = DefaultDialogLimit
	}
	rows, err := s.DB.Query(
		`SELECT `+messageSelectColumns+` FROM messages m
		 WHERE m.target_session_id = ? AND m.kind = ?
		 ORDER BY m.seq ASC LIMIT ?`,
		sessionID, MessageKindChat, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: 对话流查询失败: %w", err)
	}
	defer rows.Close()
	out := make([]Message, 0)
	for rows.Next() {
		m, err := scanMessage(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描对话流行失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历对话流失败: %w", err)
	}
	return out, nil
}

// QuerySessionTimeline 会话时间线合流查询（b3-W1，看板对话合流/未读角标数据面）：
// chat（目标会话命中——board 发送与目标会话自发回复双向都进流，QueryDialog 全流
// 语义同款）+ direct（目标（栏目,角色）命中——会话所在信箱的定向消息合流进同一
// 时间线）两路 OR 合一，按 seq DESC（最新在前，§3.1 seq 单调递增=时间序）。
//
// Q2 冻结 SQL（tech-design §3.6）逐字采用——WHERE/ORDER/LIMIT 谓词不改写；SELECT
// 列面按本文件读路径冻结惯例用 messageSelectColumns 前缀版（复用 scanMessage
// 装配、列序恒定——设计稿 6 列摘要形态装不满 Message 行，属列面适配非谓词改写）。
//
// limit：<=0=DefaultDialogLimit(100)（QueryDialog 同惯例）；空命中返回非 nil
// 空切片（JSON null 防线）。
//
// 已知代价（pollVisibleSQL 同款登记，量级上来再议）：两路 OR 合一随数据量线性扫
// （chat 分支有 idx_messages_chat 部分索引、direct 分支有 idx_messages_direct
// 前缀索引支撑）；量大后可演进为两分支 UNION ALL 归并各走索引——当前单机
// SQLite+万级消息量下 OR 形态足够。
func (s *Store) QuerySessionTimeline(sessionID, columnID int64, role string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = DefaultDialogLimit
	}
	rows, err := s.DB.Query(
		`SELECT `+messageSelectColumns+` FROM messages m
WHERE (m.kind = 'chat' AND m.target_session_id = ?)
   OR (m.kind = 'direct' AND m.column_id = ? AND m.target_role = ?)
ORDER BY m.seq DESC LIMIT ?`,
		sessionID, columnID, role, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: 会话时间线查询失败: %w", err)
	}
	defer rows.Close()
	out := make([]Message, 0)
	for rows.Next() {
		m, err := scanMessage(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描会话时间线行失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历会话时间线失败: %w", err)
	}
	return out, nil
}

// InsertBoardMessageWithAudit 看板消息落库+审计同事务（§2.2 #24「同事务 INSERT
// audit_log(board.message)」，AC1.4 留痕与消息原子——审计写失败整体回滚，防
// 「消息已投递而发送动作无痕」的留痕缺口）。消息行经 insertMessage 单源底座
// （kind/level/body 校验同构），审计行经 insertAuditRow 单源（action 空拒）。
// entry 三弱关联由调用方装配（board 语义：ProjectID/ColumnID=目标会话域、
// SessionID=0 系统动作——看板无会话身份）；detailFn 在事务内消息 INSERT 之后
// 调用（insertMessage 的 RETURNING 已带 seq，零额外往返），供调用方把落库 seq
// 注入审计 detail（总控 #14③：审计行与 messages 行精确关联）——seq 服务端
// AUTOINCREMENT 取号，调用方在发送前无从得知，只能在此回调中取。
// created_at 均服务端注入。事务形态同 CreateProjectWithAudit 先例（q 抽象复用
// insertMessage/insertAuditRow，勿复制 SQL）。
func (s *Store) InsertBoardMessageWithAudit(in MessageInput, entry AuditEntry, detailFn func(seq int64) string) (Message, error) {
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return Message{}, fmt.Errorf("store: 开启看板消息事务失败: %w", err)
	}
	// Commit 后 Rollback 返回 ErrTxDone，忽略无害——错误路径靠它回滚。
	defer func() { _ = tx.Rollback() }()
	m, err := insertMessage(tx, in)
	if err != nil {
		return Message{}, err
	}
	entry.Detail = detailFn(m.Seq)
	if _, err := insertAuditRow(tx, entry); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, fmt.Errorf("store: 提交看板消息事务失败: %w", err)
	}
	return m, nil
}
