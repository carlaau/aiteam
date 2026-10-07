package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"aiteam/internal/types"
)

// InitColumnPositions 栏目登记时位点初始化（§2.2 #5 副作用、§3.6「栏目登记时
// 位点初始化」原样 SQL）：为 controller/executor 两常用角色各预置一行 ack_positions，
// position=当前全局 MAX(seq)——D7 不回看（总线消息不回灌新栏目，新栏目位点从
// 登记时刻起算）；空库无消息时 COALESCE 兜底 0。
//
//   - 单条 INSERT...SELECT 批量预置两角色（§3.7 往返账：禁止逐角色两轮 INSERT 循环）；
//   - 必须在调用方事务内执行（tx）：与栏目 INSERT 同事务原子，登记+预置要么都在
//     要么都不在（CreateColumn）；
//   - 其他角色首次 poll 时惰性初始化（§3.6 注：首次出现取当时 max(seq)，读侧规则
//     归位点读写批，本文件仅登记时预置）。
func InitColumnPositions(tx *sql.Tx, projectID, columnID int64, now string) error {
	_, err := tx.Exec(
		`INSERT INTO ack_positions (project_id, column_id, consumer, position, updated_at)
		 SELECT ?, ?, s.consumer, COALESCE((SELECT MAX(seq) FROM messages), 0), ?
		 FROM (SELECT 'controller' AS consumer UNION ALL SELECT 'executor') AS s`,
		projectID, columnID, now,
	)
	if err != nil {
		return fmt.Errorf("store: 预置栏目 %d 登记位点失败: %w", columnID, err)
	}
	return nil
}

// ---- 位点读写（B2-2 追加：§2.2 #10/#11 数据面，决策 A7 双 consumer 同表）----

// ErrInvalidSeq ack 显式 seq 参数非法（AdvancePositions 负值拒绝；0=省略语义合法，
// 正数=显式位点合法）。产于写路径，B2-4 handler 映射 400 时须同步 writeStoreError
// 映射与 write_store_error_test.go 哨兵清单（机械闸注释约定，同 ErrInvalidLimit）。
var ErrInvalidSeq = errors.New("seq 参数非法")

// DialogConsumer 构造对话位点 consumer 键（§3.2 表 7 注/A7：
// consumer='chat:session:<id>' → 会话对话位点，AC15.4 会话隔离所需；
// 信箱位点 consumer=角色名原样入参，无需构造）。
func DialogConsumer(sessionID int64) string {
	return fmt.Sprintf("chat:session:%d", sessionID)
}

// Positions 双维度消费位点（GetPositions/AdvancePositions 返回面，#10/#11 响应
// data:{mailbox_position, dialog_position} 的数据源）。
type Positions struct {
	Mailbox int64 // 信箱位点（consumer=角色名，栏目+角色记账——PRD 冻结术语）
	Dialog  int64 // 对话位点（consumer=chat:session:<id>，会话粒度隔离）
}

// ackPositionsUpsertSQL 位点单行 upsert（AdvancePositions/lazyInitMailbox 共用
// INSERT 形态的冲突分支）：冲突时 position 取 max（AC9.2 幂等——位点单调不回退，
// 回退尝试无效果），且仅在实际推进时刷 updated_at（幂等重放零副作用，口径同
// projectArchiveSQL 的 CASE 先例）。RETURNING 返回落行/更新后的最终 position。
const ackPositionsUpsertSQL = `INSERT INTO ack_positions (project_id, column_id, consumer, position, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(column_id, consumer) DO UPDATE SET
  position = max(position, excluded.position),
  updated_at = CASE WHEN excluded.position > ack_positions.position
                    THEN excluded.updated_at ELSE ack_positions.updated_at END
RETURNING position`

// upsertPosition 在 q（连接池或事务）上执行位点单行 upsert，返回推进后的 position。
func upsertPosition(q queryRower, projectID, columnID int64, consumer string, position int64, now string) (int64, error) {
	var got int64
	err := q.QueryRow(ackPositionsUpsertSQL, projectID, columnID, consumer, position, now).Scan(&got)
	if err != nil {
		return 0, fmt.Errorf("store: 推进位点 column=%d consumer=%s 失败: %w", columnID, consumer, err)
	}
	return got, nil
}

// GetPositions 读双维度位点（§2.2 #10 poll 的位点前置，B2-4 poll handler 消费；
// 本函数只读算位点值，不做消息过滤——过滤归 PollVisible 消费这些值）。
//
// 一条 WHERE consumer IN (角色, chat:session:<id>) 查询取两行（§3.7 往返账：
// 位点读取单条，不逐维度两轮查）。两维度行缺失行为刻意不同（差异化已裁口径）：
//   - 信箱行不存在 → 惰性 upsert 落行=当前全表 MAX(seq)（§3.6 注「其他角色首次
//     poll 时惰性初始化（首次出现时取当前 max(seq)，实现于 store 层 upsert——同
//     规则）」；D7 不回看：首见前的广播不回灌，写！见 lazyInitMailbox）；
//   - 对话行不存在 → 按 0 处理、不落行（待裁点 B2-T3 推荐口径：对话=点对点恢复
//     现场，poll 拉全部发给我的 chat/receipt；读路径保持零写）。
//
// 两维度位点行的 column 归属=调用方传入的消费视角栏目（B2-4 handler 恒传
// query 栏目 ref.ColumnID，正常形态即会话所属栏目）：对话谓词（§3.6 第三分支
// 与 maxVisibleDialog）只按 target_session_id+seq 过滤、不筛栏目，归属仅是
// (column_id, consumer) 键的组织维度、不影响过滤正确性；恒传同一栏目保证键
// 稳定不分裂。勿改为传目标会话所属栏目——跨栏目对话时单条 IN 查不全两行，
// 破坏位点单条读取的往返账（B2-4 审查裁决，B2-2 旧注释口径作废）。
func (s *Store) GetPositions(projectID, columnID int64, role string, sessionID int64) (Positions, error) {
	dialogConsumer := DialogConsumer(sessionID)
	rows, err := s.DB.Query(
		`SELECT consumer, position FROM ack_positions
		 WHERE column_id = ? AND consumer IN (?, ?)`,
		columnID, role, dialogConsumer,
	)
	if err != nil {
		return Positions{}, fmt.Errorf("store: 查询位点 column=%d 失败: %w", columnID, err)
	}
	defer rows.Close()
	var got Positions
	mailboxFound := false
	for rows.Next() {
		var consumer string
		var pos int64
		if err := rows.Scan(&consumer, &pos); err != nil {
			return Positions{}, fmt.Errorf("store: 扫描位点行失败: %w", err)
		}
		// role 与 dialogConsumer 格式恒不相交（后者恒带 chat:session: 前缀），
		// 两 case 互斥；对话分支先匹配，病态重名输入下优先按对话键解释。
		switch consumer {
		case dialogConsumer:
			got.Dialog = pos
		case role:
			got.Mailbox = pos
			mailboxFound = true
		}
	}
	if err := rows.Err(); err != nil {
		return Positions{}, fmt.Errorf("store: 遍历位点行失败: %w", err)
	}
	if !mailboxFound {
		pos, err := s.lazyInitMailbox(projectID, columnID, role)
		if err != nil {
			return Positions{}, err
		}
		got.Mailbox = pos
	}
	// 对话行缺失：got.Dialog 零值即 0（B2-T3），不落行。
	return got, nil
}

// lazyInitMailbox 信箱位点行缺失时的惰性初始化（§3.6 注原样规则，plan 名
// LazyInitMailbox——仅 GetPositions 消费故非导出）：position=全表
// COALESCE(MAX(seq),0)（D7 不回看），与 InitColumnPositions 登记预置同一取值口径。
// ON CONFLICT DO NOTHING：与并发首读竞态时以先落行为准（本库单连接池写串行化下
// 不可达，防御保留），随后无条件回读真实行值（DO NOTHING 未插入时 RETURNING
// 无行，不能依赖其返回）。
func (s *Store) lazyInitMailbox(projectID, columnID int64, consumer string) (int64, error) {
	if _, err := s.DB.Exec(
		`INSERT INTO ack_positions (project_id, column_id, consumer, position, updated_at)
		 VALUES (?, ?, ?, COALESCE((SELECT MAX(seq) FROM messages), 0), ?)
		 ON CONFLICT(column_id, consumer) DO NOTHING`,
		projectID, columnID, consumer, types.NowUTC(),
	); err != nil {
		return 0, fmt.Errorf("store: 惰性初始化信箱位点 column=%d consumer=%s 失败: %w", columnID, consumer, err)
	}
	var pos int64
	if err := s.DB.QueryRow(
		`SELECT position FROM ack_positions WHERE column_id = ? AND consumer = ?`,
		columnID, consumer,
	).Scan(&pos); err != nil {
		return 0, fmt.Errorf("store: 回读惰性初始化位点 column=%d consumer=%s 失败: %w", columnID, consumer, err)
	}
	return pos, nil
}

// AdvancePositions 推进双维度位点（§2.2 #11 ack 数据面，B2-4 ack handler 消费）：
//   - seq > 0：显式推进——信箱与对话两维度均推到 seq（§2.2 #11 请求单 seq 字段）；
//   - seq == 0：省略——推进到当前该消费上下文「各自」最大可见 seq（一键处理完；
//     可见性口径=poll 谓词三分支去掉位点条件的 MAX，见 maxVisibleMailbox/
//     maxVisibleDialog——与 PollVisible 的一致性由 TestAdvanceOmittedMatchesPollPredicate
//     机械对拍守门，非注释约定）；
//   - seq < 0：ErrInvalidSeq。
//
// 口径注记：省略 ack=「ack 时点」的最大可见——poll 与 ack 之间新到的消息会被
// 隐式确认（一键全部已读语义），消费方需按需显式带 seq 精确推进。省略时目标 0
// 也落行——行=「该消费上下文 ack 过」的凭据（0 是合法位点值，非「没 ack 过」；
// 与 GetPositions 读路径对话行缺失按 0 的语义互不混淆）。
//
// 单事务双 upsert（§3.7 写路径「ack=2 UPDATE（信箱+对话位点）」，行缺失由 upsert
// 兜底落行）；position 取 max（AC9.2 幂等：重复推进零副作用——值与 updated_at 均不
// 变，回退尝试无效果）；返回推进后双位点（#11 响应 data:{mailbox_position,
// dialog_position} 直用）。
func (s *Store) AdvancePositions(projectID, columnID int64, role string, sessionID, seq int64) (Positions, error) {
	if seq < 0 {
		return Positions{}, fmt.Errorf("%w: %d（负值非法；0=省略，正数=显式位点）", ErrInvalidSeq, seq)
	}
	mailboxTarget, dialogTarget := seq, seq
	if seq == 0 {
		var err error
		if mailboxTarget, err = s.maxVisibleMailbox(projectID, columnID, role); err != nil {
			return Positions{}, err
		}
		if dialogTarget, err = s.maxVisibleDialog(sessionID); err != nil {
			return Positions{}, err
		}
	}
	now := types.NowUTC()
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return Positions{}, fmt.Errorf("store: 开启推进位点事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 ErrTxDone，忽略无害

	mailbox, err := upsertPosition(tx, projectID, columnID, role, mailboxTarget, now)
	if err != nil {
		return Positions{}, err
	}
	dialog, err := upsertPosition(tx, projectID, columnID, DialogConsumer(sessionID), dialogTarget, now)
	if err != nil {
		return Positions{}, err
	}
	if err := tx.Commit(); err != nil {
		return Positions{}, fmt.Errorf("store: 提交推进位点事务失败: %w", err)
	}
	return Positions{Mailbox: mailbox, Dialog: dialog}, nil
}

// maxVisibleMailbox 信箱消费上下文当前最大可见 seq（§3.6 poll 谓词 direct+bus 两
// 分支去掉位点条件的聚合；「信箱级口径只含 direct+bus，chat 归对话流」同 §3.7
// 待消费计数口径；bus 分支 controller 闸经绑定参数传入，无字符串拼接）。
func (s *Store) maxVisibleMailbox(projectID, columnID int64, role string) (int64, error) {
	var maxSeq int64
	err := s.DB.QueryRow(
		`SELECT COALESCE(MAX(seq), 0) FROM messages
		 WHERE (kind = 'direct' AND column_id = ? AND target_role = ?)
		    OR (kind = 'bus' AND project_id = ? AND ? = 'controller')`,
		columnID, role, projectID, role,
	).Scan(&maxSeq)
	if err != nil {
		return 0, fmt.Errorf("store: 计算信箱最大可见 seq 失败: %w", err)
	}
	return maxSeq, nil
}

// maxVisibleDialog 对话消费上下文当前最大可见 seq（§3.6 poll 谓词第三分支去掉
// 位点条件：发给我的 chat/receipt，排除自发——自发不回流 §2.2 #24 语义）。
func (s *Store) maxVisibleDialog(sessionID int64) (int64, error) {
	var maxSeq int64
	err := s.DB.QueryRow(
		`SELECT COALESCE(MAX(seq), 0) FROM messages
		 WHERE kind IN ('chat','receipt') AND target_session_id = ? AND sender_session_id != ?`,
		sessionID, sessionID,
	).Scan(&maxSeq)
	if err != nil {
		return 0, fmt.Errorf("store: 计算对话最大可见 seq 失败: %w", err)
	}
	return maxSeq, nil
}
