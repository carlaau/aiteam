package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"aiteam/internal/types"
)

// ---- b5-T1 撤回域（M-36：未消费窗口消息撤回，store 数据面）----

// RecalledMessage recalled_messages 表一行（b5-spec R2.1）：一 seq 一墓碑——
// messages 表零触碰（append-only 触发器禁改删），撤回状态流转落独立墓碑表，
// poll 消费面经 pollVisibleSQL NOT EXISTS 过滤（消费方永不见），看板审计面读
// 墓碑行（观测方可追溯）——双口径是本批核心设计决策（b5-spec 裁量③）。
type RecalledMessage struct {
	MessageSeq        int64  // 撤回目标 seq（PRIMARY KEY，一 seq 一墓碑）
	RecalledBySession int64  // 撤回操作会话（归因，弱关联不设 FK）
	Reason            string // 撤回理由（''=未填）
	CreatedAt         string // 首撤时间（幂等重放不刷，types.NowUTC 注入）
}

// recalledMessageInsertSQL 墓碑落行单源语句（RecallMessageWithAudit 事务内执行）：
// OR IGNORE 幂等——message_seq 主键冲突（已墓碑再撤）静默跳过，首撤要素
// （recalled_by_session/reason/created_at）全保留，对齐 archive 家族 CASE 口径
// （幂等不刷时间，b5-spec 裁量②）。OR IGNORE 未插入时无 RETURNING 行，权威行
// 由随后的无条件回读提供（lazyInitMailbox 的 DO NOTHING+回读同款形态）。
const recalledMessageInsertSQL = `INSERT OR IGNORE INTO recalled_messages
  (message_seq, recalled_by_session, reason, created_at)
 VALUES (?, ?, ?, ?)`

// consumerPositionSQL 单 consumer 位点纯读探测（b5-T2 handler 未消费判定数据面，
// b5-spec R1.3）：缺行 COALESCE 0=全量未消费——与探测面「缺行全量可见」同口径
// （pollHitsSQL/crossGridDirectPendingSQL 同款内联 COALESCE 形态）。刻意不走
// GetPositions：其信箱行缺失走 lazyInitMailbox 惰性 upsert 落行=当时 MAX(seq)
// （D7 不回看），撤回探测语境下会把「从未消费（缺行=0 可撤）」翻成「已消费
// （MAX(seq)≥seq 不可撤）」且探测自带写副作用——sentinels.go pollHitsSQL 注释
// 登记的「探测必须零写/缺行语义相反」两条理由在撤回面同构成立（B3-8 收敛
// 裁定先例：保留自含零写实现，不收敛为 GetPositions 链）。
const consumerPositionSQL = `SELECT COALESCE((SELECT position FROM ack_positions
  WHERE column_id = ? AND consumer = ?), 0)`

// ConsumerPosition 读单个 (栏目, consumer) 当前位点值（b5-T2 撤回未消费判定）：
// direct 传 consumer=目标角色名（信箱维度）、chat 传 DialogConsumer(目标会话 id)
// （对话维度）；缺行=0（纯读零写，语义依据见 consumerPositionSQL 注释——
// GetPositions 的惰性初始化写面不可用于探测）。
func (s *Store) ConsumerPosition(columnID int64, consumer string) (int64, error) {
	var pos int64
	if err := s.DB.QueryRow(consumerPositionSQL, columnID, consumer).Scan(&pos); err != nil {
		return 0, fmt.Errorf("store: 探测位点 column=%d consumer=%s 失败: %w", columnID, consumer, err)
	}
	return pos, nil
}

// RecallMessageWithAudit 撤回单事务（b5-spec R2.2/AC3①，M-36）：INSERT 墓碑
// （OR IGNORE 幂等——已墓碑再撤不刷 created_at 且仍记审计）+ INSERT audit 同
// 事务，审计失败整体回滚不留半程墓碑（事务骨架与 detailFn 回调风格镜像
// ArchiveProjectWithAudit）。
//
// 实测账：单事务 4 语句 = SELECT 目标消息（存在性+域锚）+ INSERT OR IGNORE 墓碑
// + 回读权威墓碑行 + INSERT audit（口径同 Archive*WithAudit 的「spec 账面外
// SELECT 为快照源」审查裁定——回读是幂等路径下权威行的唯一来源）。
//
// 分工面：store 纯落库不做撤回资格校验——发送方 409/kind 白名单/block 拒/未消费
// 判定归 handler 校验链（b5-spec R1，T2 落地）；本函数只做存在性兜底（目标 seq
// 不存在返回 ErrMessageNotFound，404 哨兵与 GetMessageBySeq 同源）。
//
//   - seq：撤回目标消息 seq；
//   - recalledBySession / reason：墓碑归因与理由（落墓碑行；幂等重放时首撤要素
//     保留，本次入参只进审计 detail 侧由调用方经 entry/detailFn 自行装配）；
//   - entry：审计骨架——ProjectID/ColumnID 由 store 从消息归属域自动回填
//     （UpdateProjectWithAudit 回填 entry.ProjectID 先例），Detail 由 detailFn
//     在事务内权威墓碑行齐备后生成（含服务端注入的 created_at 与幂等重放下的
//     首撤要素——调用方发送前无从得知，回调先例同 InsertBoardMessageWithAudit
//     的 seq 注入）。
//
// 返回权威墓碑行（幂等重放=首撤行）与落库审计行。
func (s *Store) RecallMessageWithAudit(seq int64, recalledBySession int64, reason string, entry AuditEntry, detailFn func(rm RecalledMessage) string) (RecalledMessage, AuditEntry, error) {
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return RecalledMessage{}, AuditEntry{}, fmt.Errorf("store: 开启撤回事务失败: %w", err)
	}
	// Commit 后 Rollback 返回 ErrTxDone，忽略无害——错误路径靠它回滚。
	defer func() { _ = tx.Rollback() }()

	// 存在性+域锚：目标消息行（ErrMessageNotFound→404；project_id/column_id
	// 供审计域锚回填）。FK REFERENCES messages(seq) 是插入侧兜底，存在性判定
	// 以本 SELECT 为准（错误语义带 seq 可读信息）。
	msg, err := scanMessage(tx.QueryRow(
		`SELECT `+messageSelectColumns+` FROM messages m WHERE m.seq = ?`, seq,
	).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return RecalledMessage{}, AuditEntry{}, fmt.Errorf("%w: seq=%d", ErrMessageNotFound, seq)
	}
	if err != nil {
		return RecalledMessage{}, AuditEntry{}, fmt.Errorf("store: 定位消息 seq=%d 失败: %w", seq, err)
	}

	if _, err := tx.Exec(recalledMessageInsertSQL,
		seq, recalledBySession, reason, types.NowUTC(),
	); err != nil {
		return RecalledMessage{}, AuditEntry{}, fmt.Errorf("store: 落墓碑 seq=%d 失败: %w", seq, err)
	}

	// 回读权威墓碑行：幂等重放（OR IGNORE 跳过）时带回首撤行——created_at/
	// recalled_by/reason 全为首撤要素（detailFn 与返回值同源，防两口径漂移）。
	var rm RecalledMessage
	err = tx.QueryRow(
		`SELECT message_seq, recalled_by_session, reason, created_at
		 FROM recalled_messages WHERE message_seq = ?`, seq,
	).Scan(&rm.MessageSeq, &rm.RecalledBySession, &rm.Reason, &rm.CreatedAt)
	if err != nil {
		return RecalledMessage{}, AuditEntry{}, fmt.Errorf("store: 回读墓碑 seq=%d 失败: %w", seq, err)
	}

	entry.ProjectID = msg.ProjectID
	entry.ColumnID = msg.ColumnID
	entry.Detail = detailFn(rm)
	audit, err := insertAuditRow(tx, entry)
	if err != nil {
		return RecalledMessage{}, AuditEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return RecalledMessage{}, AuditEntry{}, fmt.Errorf("store: 提交撤回事务失败: %w", err)
	}
	return rm, audit, nil
}

// ---- b5-T4 墓碑读面（看板观测面，M-36 双口径之读半：poll 消费面整体过滤、
// 看板观测面标记+剥离并存——墓碑行不删、body 不可见、recalled 可追溯）----

// boardHistorySelectColumns 看板历史读面列清单（QueryHistory 专用，b5-T4）：
// messageSelectColumns 的墓碑感知变体——body 列以 CASE 包裹（墓碑行置空）、
// 尾追加 recalled 布尔列。与 messageSelectColumns 分立的理由：后者被
// PollVisible/GetMessageBySeq/QueryDialog 等 11 列扫描面共享（poll 消费面是
// b5-T1 红线零触碰面，扩列会炸全部既有 Scan 位序）；本清单只归 QueryHistory
// 一个消费方（#13 history/#26 bus-stream 两端点同源，双胞胎纪律不破）。
//
// 剥离落 SQL 层是红线硬闸（b5-spec R5/AC3②「看板只显占位不显 body」服务端
// 剥离，非靠前端不渲染）——墓碑行 body 到不了 Go 层更到不了 wire，标记与剥离
// 同源同表达式（r.message_seq IS NOT NULL），零两口径漂移面。recalled_messages.
// message_seq 是 INTEGER PRIMARY KEY（rowid 别名），LEFT JOIN 等值探走行键零
// 额外索引需求，且一 seq 一墓碑不放大行数（LIMIT/ORDER BY 语义不受扰）。
const boardHistorySelectColumns = `m.seq, m.project_id, m.column_id, m.kind, m.target_role,
       m.target_session_id, m.sender_session_id, m.sender_label, m.level,
       CASE WHEN r.message_seq IS NOT NULL THEN '' ELSE m.body END AS body,
       m.created_at, (r.message_seq IS NOT NULL) AS recalled`

// boardHistoryJoinSQL 看板历史读面 FROM 段（boardHistorySelectColumns 的 r 别名
// 唯一来源，勿在调用方散写 JOIN——列表达式与 JOIN 形态单源绑定）。LEFT JOIN
// 不丢未撤行（INNER JOIN 会把全量未撤消息蒸发光，观测面不可接受）。
const boardHistoryJoinSQL = ` FROM messages m
       LEFT JOIN recalled_messages r ON r.message_seq = m.seq`

// scanBoardMessage 从一行解出 Message+墓碑标记（boardHistorySelectColumns 列序
// 12 列）：scanMessage 的 11 列扫描器位序不兼容（多尾列），独立扫描器机械可审
// （新列按需加位，同 scanMessage 惯例）。SQLite 布尔表达式回 0/1，database/sql
// 经 driver.Bool 默认转换扫入 bool。
func scanBoardMessage(scan func(dest ...any) error) (Message, error) {
	var m Message
	err := scan(&m.Seq, &m.ProjectID, &m.ColumnID, &m.Kind, &m.TargetRole,
		&m.TargetSessionID, &m.SenderSessionID, &m.SenderLabel,
		&m.Level, &m.Body, &m.CreatedAt, &m.Recalled)
	if err != nil {
		return Message{}, err
	}
	return m, nil
}
