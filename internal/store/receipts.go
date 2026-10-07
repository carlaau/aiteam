package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"aiteam/internal/types"
)

// ErrReceiptNotFound 按 message_seq 定位的回执行不存在（GetReceiptBySeq 读路径；
// InsertReceipt 的 UNIQUE 冲突不走它——幂等回读已有记录返回 already=true，AC9.2）。
var ErrReceiptNotFound = errors.New("回执不存在")

// Receipt message_receipts 表一行（§3.2 表 6 字段面；AC7.3 回执=回执方会话身份+
// 服务端时间）：B2-5 receipt handler 用它组装 #12 响应 data:{seq, receipt_at,
// receipt_by} 与回告消息 body（receipt_by 的会话名由 handler 按会话 id 另取）。
type Receipt struct {
	MessageSeq int64  // 原消息 seq（UNIQUE，一消息一回执）
	ReceiptBy  int64  // 回执方会话 id（receipt_session_id，AC7.3）
	ReceiptAt  string // 服务端时间（created_at，types.NowUTC 注入，AC5.4）
}

// receiptColumns SELECT/RETURNING 列清单（与 scanReceipt 的 Scan 顺序一一对应）。
const receiptColumns = `message_seq, receipt_session_id, created_at`

// scanReceipt 从一行解出 Receipt（scan 兼容 *sql.Row 与 *sql.Rows 的 Scan 签名）。
func scanReceipt(scan func(dest ...any) error) (Receipt, error) {
	var r Receipt
	if err := scan(&r.MessageSeq, &r.ReceiptBy, &r.ReceiptAt); err != nil {
		return Receipt{}, err
	}
	return r, nil
}

// getReceiptBySeq 在 q（连接池或事务）上按 message_seq 查回执行，未命中
// ErrReceiptNotFound（errors.Is 可判）。
func getReceiptBySeq(q queryRower, messageSeq int64) (Receipt, error) {
	r, err := scanReceipt(q.QueryRow(
		`SELECT `+receiptColumns+` FROM message_receipts WHERE message_seq = ?`,
		messageSeq,
	).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, fmt.Errorf("%w: seq=%d", ErrReceiptNotFound, messageSeq)
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("store: 查询回执 seq=%d 失败: %w", messageSeq, err)
	}
	return r, nil
}

// insertReceipt 在 q（连接池或事务）上插入回执（§2.2 #12 数据面）：单条
// INSERT...RETURNING，created_at=types.NowUTC 注入时钟（AC5.4）。
// message_seq UNIQUE 冲突 → 幂等回读已有记录返回 (已有, true, nil)（AC9.2 回执面
// 「重复回执幂等返回已有记录」；B2-5 handler 层映射 already_recepted 200），不报
// 错、不落第二行（一消息一回执）。
//
// FK 违反（message_seq 指向的消息不存在 / receipt_session_id 指向的会话不存在）
// 不在幂等路径——唯一冲突检测只认 SQLITE_CONSTRAINT_UNIQUE，FK 错误原样冒泡；
// B2-5 handler 须先行校验消息存在（404 message_not_found）/会话身份（中间件保证）。
//
// 单源共享底座（q 抽象同 insertProject 惯例）：B2-5「同事务双 INSERT（本函数+
// receipt 回告消息）」的事务变体直接以 tx 调用，勿复制 SQL。
func insertReceipt(q queryRower, messageSeq, sessionID int64, now string) (Receipt, bool, error) {
	r, err := scanReceipt(q.QueryRow(
		`INSERT INTO message_receipts (message_seq, receipt_session_id, created_at)
		 VALUES (?, ?, ?)
		 RETURNING `+receiptColumns,
		messageSeq, sessionID, now,
	).Scan)
	if err == nil {
		return r, false, nil
	}
	if isUniqueViolation(err) {
		got, gerr := getReceiptBySeq(q, messageSeq)
		if gerr != nil {
			return Receipt{}, false, fmt.Errorf("store: 回执 seq=%d 冲突但回读已有记录失败: %w", messageSeq, gerr)
		}
		return got, true, nil
	}
	return Receipt{}, false, fmt.Errorf("store: 插入回执 seq=%d 失败: %w", messageSeq, err)
}

// InsertReceipt 记录一条阻断回执（§2.2 #12 数据面，AC7.3）：见 insertReceipt。
// 返回值 already=true 表示该消息此前已有回执（返回的是已有记录而非本次入参视角）。
// 注：receipt 回告消息的写入与位点推进归 handler 层（§2.3 副作用链/T4），
// 本函数只落 receipts 行——B2-5 经事务底座组装原子性。
func (s *Store) InsertReceipt(messageSeq, sessionID int64) (Receipt, bool, error) {
	return insertReceipt(s.DB, messageSeq, sessionID, types.NowUTC())
}

// GetReceiptBySeq 按 message_seq 查回执（AC9.3 可查面；#13 receipt 状态装配
// 数据源）。未命中返回 ErrReceiptNotFound。
func (s *Store) GetReceiptBySeq(messageSeq int64) (Receipt, error) {
	return getReceiptBySeq(s.DB, messageSeq)
}

// GetReceiptsBySeqs 按 seq 批量查回执（B2-5 #13 receipt 字段装配数据面）：单条
// SELECT...IN（去重占位），禁逐 seq 循环查回执（b2-spec §七「receipt 装配 ≤1
// 条」）。入参空/去重后空返回空 map（非 nil）；未命中 seq 不出现在 map——#13
// 以 map 缺席判 receipt=null（AC7.2 未回执态），不视为错误。
func (s *Store) GetReceiptsBySeqs(seqs []int64) (map[int64]Receipt, error) {
	uniq := seqs[:0:0] // 复制去重（底层数组独立，勿改传入切片）
	seen := make(map[int64]struct{}, len(seqs))
	for _, sseq := range seqs {
		if _, dup := seen[sseq]; !dup {
			seen[sseq] = struct{}{}
			uniq = append(uniq, sseq)
		}
	}
	out := make(map[int64]Receipt, len(uniq))
	if len(uniq) == 0 {
		return out, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?, ", len(uniq)), ", ")
	args := make([]any, len(uniq))
	for i, sseq := range uniq {
		args[i] = sseq
	}
	rows, err := s.DB.Query(
		`SELECT `+receiptColumns+` FROM message_receipts
		 WHERE message_seq IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: 批量查询回执失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanReceipt(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描回执行失败: %w", err)
		}
		out[r.MessageSeq] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历回执行失败: %w", err)
	}
	return out, nil
}

// ReceiptNotifyInput 回告消息语义面（§2.3 副作用链/T4，InsertReceiptWithNotify
// 入参）：字段由 handler 按被回执消息算好传入（store 纯落库不反推语义——同
// MessageInput 惯例）；body 由组合方法按 §2.3 格式原文组装（时间戳=回执时刻，
// 与回执行 created_at 同一注入时钟取值），不入参。
type ReceiptNotifyInput struct {
	ProjectID       int64  // 回告消息归属项目（=被回执消息所属项目）
	TargetSessionID int64  // 原发送方会话 id（§2.3 target_session_id）
	ColumnID        int64  // 原发送方会话所属栏目 id（§3.2 表 5 注 receipt 归属）
	ReceiptByName   string // 回执方会话名（body「by <会话名>」段，AC7.3 身份面）
}

// InsertReceiptWithNotify 记录阻断回执+自动回告消息，单事务双 INSERT（§2.2 #12
// 副作用链原文：「校验通过 → INSERT message_receipts → 同事务 INSERT 一条
// kind="receipt" 消息」；b2-spec §七回执行「单事务 2 INSERT」）——回执行与回告
// 要么都提交要么都不在，杜绝「回执已记而原发送方永远收不到回告」的半状态。
//
// 幂等与副作用的关系（AC9.2 already_recepted）：insertReceipt 底座 UNIQUE 冲突
// 幂等回读已有记录（already=true）时**直接返回零副作用**——不重复发回告。首次
// 执行才落回告；重执返回已有记录（回告消息不重复，一消息一回执一回告）。
//
// 回告消息字段（§2.3/§3.2 表 5 注逐项）：kind=receipt、level=normal、
// target_session_id=原发送方会话、column_id=原发送方会话栏目、target_role=空、
// sender_session_id=0（系统生成，弱关联语义）、sender_label=system、
// body=`receipt: seq=N by <会话名> at <时间>`（时间=回执时刻）。
func (s *Store) InsertReceiptWithNotify(messageSeq, receiptSessionID int64, in ReceiptNotifyInput) (Receipt, bool, error) {
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return Receipt{}, false, fmt.Errorf("store: 开启回执事务失败: %w", err)
	}
	// Commit 后 Rollback 返回 ErrTxDone，忽略无害——错误路径靠它回滚。
	defer func() { _ = tx.Rollback() }()

	now := types.NowUTC() // 回执时刻：回执行 created_at 与回告 body at 同值（AC5.4 注入时钟）
	r, already, err := insertReceipt(tx, messageSeq, receiptSessionID, now)
	if err != nil {
		return Receipt{}, false, err
	}
	if already {
		// 幂等命中：零副作用直接返回（未写任何行，defer Rollback 收敛空事务）。
		return r, true, nil
	}
	notify := MessageInput{
		ProjectID:       in.ProjectID,
		ColumnID:        in.ColumnID,
		Kind:            MessageKindReceipt,
		TargetSessionID: in.TargetSessionID,
		SenderSessionID: 0,
		SenderLabel:     "system",
		Level:           MessageLevelNormal,
		Body: fmt.Sprintf("receipt: seq=%d by %s at %s",
			messageSeq, in.ReceiptByName, now),
	}
	if _, err := insertMessage(tx, notify); err != nil {
		// 回告失败（含测试注入）→ 事务整体回滚，回执行一并消失（原子性）。
		return Receipt{}, false, fmt.Errorf("store: 插入回告消息 seq=%d 失败: %w", messageSeq, err)
	}
	if err := tx.Commit(); err != nil {
		return Receipt{}, false, fmt.Errorf("store: 提交回执事务失败: %w", err)
	}
	return r, false, nil
}

// UnreceiptedMessage 发送方 block 未回执清单行（§3.6 NOT EXISTS SQL select 面：
// seq/发送方冗余显示/目标角色/栏目/创建时间——AC11.3 清单展示所需五列）。
type UnreceiptedMessage struct {
	Seq         int64  // 原消息 seq
	SenderLabel string // 发送方冗余显示（'controller-A@05' 等）
	TargetRole  string // 目标角色（direct 消息；空=非 direct）
	ColumnID    int64  // 归属栏目
	CreatedAt   string // 服务端时间
}

// ListUnreceipted 发送方 block 未回执清单（§3.6 SQL 逐字对拍，AC7.2/AC11.3 动态
// 聚合；status 内嵌清单 B3 复用同函数）：「我」发出的 level=block 且尚无任何回执
// 的消息（NOT EXISTS 单条子查询，无 join 展开），按 seq 降序（最新在前）。
// 参数=发送方会话 id（SQL :me 单参数——清单按发送方会话身份过滤，别人的消息不进
// 我的清单）。空清单返回非 nil 空切片（JSON null 防线，B1-5/B1-8 消费惯例）。
func (s *Store) ListUnreceipted(senderSessionID int64) ([]UnreceiptedMessage, error) {
	rows, err := s.DB.Query(
		`SELECT m.seq, m.sender_label, m.target_role, m.column_id, m.created_at
		 FROM messages m
		 WHERE m.level = 'block' AND m.sender_session_id = ?
		   AND NOT EXISTS (SELECT 1 FROM message_receipts r WHERE r.message_seq = m.seq)
		 ORDER BY m.seq DESC`,
		senderSessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: 查询未回执清单失败: %w", err)
	}
	defer rows.Close()
	out := make([]UnreceiptedMessage, 0)
	for rows.Next() {
		var m UnreceiptedMessage
		if err := rows.Scan(&m.Seq, &m.SenderLabel, &m.TargetRole, &m.ColumnID, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 扫描未回执清单行失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历未回执清单失败: %w", err)
	}
	return out, nil
}
