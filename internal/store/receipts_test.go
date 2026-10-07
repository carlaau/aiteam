package store

import (
	"errors"
	"slices"
	"testing"
)

// seedReceiptsFixture 回执域测试共享夹具，返回 (项目, 栏目, ctrl 会话, exe 会话,
// exe2 会话, ctrl 已发的 block 消息 seq)：
//
//	项目 p-a / 栏目 01；会话 controller@01 / executor-B@01 / executor-C@01。
func seedReceiptsFixture(t *testing.T, s *Store) (pid, c1, ctrl, exe, exe2, blockSeq int64) {
	t.Helper()
	const t0 = "2026-10-02T08:00:00Z"
	pid = seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, t0)
	c1 = seedColumn(t, s, pid, "01", "栏目01", ColumnStatusActive, t0)
	ctrl = seedSession(t, s, pid, c1, "controller", "controller", t0, t0)
	exe = seedSession(t, s, pid, c1, "executor-B", "executor", t0, t0)
	exe2 = seedSession(t, s, pid, c1, "executor-C", "executor", t0, t0)

	blockSeq = seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelBlock, Body: "阻断：先回执再继续",
	}, t0)
	return pid, c1, ctrl, exe, exe2, blockSeq
}

// TestInsertReceipt §2.2 #12 数据面（AC7.3 回执=回执方会话身份+服务端时间）：
//   - 成功：INSERT...RETURNING 返回库中真实行（ReceiptAt=注入时钟，AC5.4）；
//   - message_seq UNIQUE 冲突 → 幂等返回已有记录+already=true（AC9.2 回执面；
//     B2-5 handler 层映射 already_recepted 200），不报错、不落第二行。
func TestInsertReceipt(t *testing.T) {
	const t0 = "2026-10-02T13:04:05Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	_, _, _, exe, exe2, blockSeq := seedReceiptsFixture(t, s)
	// 场景一：成功插入。
	got, already, err := s.InsertReceipt(blockSeq, exe)
	if err != nil {
		t.Fatalf("InsertReceipt 失败: %v", err)
	}
	if already {
		t.Error("首次插入 already = true，期望 false")
	}
	if got.MessageSeq != blockSeq || got.ReceiptBy != exe || got.ReceiptAt != t0 {
		t.Errorf("回执行 = {seq:%d by:%d at:%s}，期望 {%d, %d, %s}（AC7.3 身份+服务端时间）",
			got.MessageSeq, got.ReceiptBy, got.ReceiptAt, blockSeq, exe, t0)
	}
	if n := countRows(t, s, "message_receipts"); n != 1 {
		t.Errorf("message_receipts 行数 = %d，期望 1", n)
	}

	// 场景二：同消息换回执方重复回执——UNIQUE 冲突幂等返回「已有」记录
	//（返回的是 exe 的回执，不是 exe2 的），不落第二行。
	got, already, err = s.InsertReceipt(blockSeq, exe2)
	if err != nil {
		t.Fatalf("重复 InsertReceipt 应幂等成功，实际报错: %v", err)
	}
	if !already {
		t.Error("重复插入 already = false，期望 true（AC9.2 幂等返回已有记录）")
	}
	if got.ReceiptBy != exe || got.MessageSeq != blockSeq || got.ReceiptAt != t0 {
		t.Errorf("幂等返回 = {seq:%d by:%d at:%s}，期望已有记录 {%d, %d, %s}",
			got.MessageSeq, got.ReceiptBy, got.ReceiptAt, blockSeq, exe, t0)
	}
	if n := countRows(t, s, "message_receipts"); n != 1 {
		t.Errorf("重复回执后 message_receipts 行数 = %d，期望仍 1（一消息一回执）", n)
	}
}

// TestGetReceipt 按 seq 查回执（AC9.3 可查面；#13 receipt 状态装配数据源）。
func TestGetReceipt(t *testing.T) {
	const t0 = "2026-10-02T13:04:05Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	_, _, _, exe, _, blockSeq := seedReceiptsFixture(t, s)

	// 未命中：ErrReceiptNotFound 哨兵（errors.Is 可判）。
	if _, err := s.GetReceiptBySeq(blockSeq); !errors.Is(err, ErrReceiptNotFound) {
		t.Errorf("未命中 err = %v，期望 errors.Is ErrReceiptNotFound", err)
	}

	// 命中：与 InsertReceipt 返回一致。
	if _, _, err := s.InsertReceipt(blockSeq, exe); err != nil {
		t.Fatalf("InsertReceipt 失败: %v", err)
	}
	got, err := s.GetReceiptBySeq(blockSeq)
	if err != nil {
		t.Fatalf("GetReceiptBySeq 失败: %v", err)
	}
	if got.MessageSeq != blockSeq || got.ReceiptBy != exe || got.ReceiptAt != t0 {
		t.Errorf("回执查询 = {seq:%d by:%d at:%s}，期望 {%d, %d, %s}",
			got.MessageSeq, got.ReceiptBy, got.ReceiptAt, blockSeq, exe, t0)
	}
}

// TestUnreceiptedList §3.6「发送方 block 未回执清单」NOT EXISTS SQL 正反例
// （AC7.2 数据面；status 内嵌清单 B3 复用同函数）：
//   - 只列 level=block（normal/important 不进清单）；
//   - 按发送方会话过滤（别人的消息不进我的清单）；
//   - NOT EXISTS 两态：无回执列出、回执后消失；
//   - seq 降序（最新在前）；全回执后非 nil 空切片（JSON null 防线）。
func TestUnreceiptedList(t *testing.T) {
	const t0 = "2026-10-02T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid, c1, ctrl, exe, _, blockSeq := seedReceiptsFixture(t, s)
	_ = pid

	// 追加夹具：ctrl 再发 block(seq2)、normal(seq3)、important(seq4)；exe 发 block(seq5)。
	//（seq3/seq4 只为验证「只列 block」——出现即 gotSeqs 对比红。）
	seq2 := seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelBlock, Body: "阻断二",
	}, t0)
	_ = seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "普通消息不进清单",
	}, t0)
	_ = seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelImportant, Body: "重要消息也不进清单",
	}, t0)
	seq5 := seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "controller",
		SenderSessionID: exe, SenderLabel: "executor-B@01",
		Level: MessageLevelBlock, Body: "exe 发的阻断",
	}, t0)

	// 正例：ctrl 无回执时清单=[seq2, blockSeq] 降序；exe 清单=[seq5]（按发送方隔离）。
	got, err := s.ListUnreceipted(ctrl)
	if err != nil {
		t.Fatalf("ListUnreceipted(ctrl) 失败: %v", err)
	}
	wantSeqs := []int64{seq2, blockSeq}
	gotSeqs := make([]int64, 0, len(got))
	for _, m := range got {
		gotSeqs = append(gotSeqs, m.Seq)
		if m.SenderLabel != "controller@01" {
			t.Errorf("清单行 sender_label = %q，期望发送方自身 label", m.SenderLabel)
		}
	}
	if !slices.Equal(gotSeqs, wantSeqs) {
		t.Errorf("ctrl 清单 = %v，期望 %v（只含 ctrl 的 block、seq 降序）", gotSeqs, wantSeqs)
	}

	got, err = s.ListUnreceipted(exe)
	if err != nil {
		t.Fatalf("ListUnreceipted(exe) 失败: %v", err)
	}
	if len(got) != 1 || got[0].Seq != seq5 {
		t.Errorf("exe 清单 = %v，期望 [%d]（按发送方过滤）", got, seq5)
	}

	// NOT EXISTS 两态：blockSeq 被 exe 回执后从 ctrl 清单消失，seq2 仍在。
	if _, _, err := s.InsertReceipt(blockSeq, exe); err != nil {
		t.Fatalf("回执 seq=%d 失败: %v", blockSeq, err)
	}
	got, err = s.ListUnreceipted(ctrl)
	if err != nil {
		t.Fatalf("回执后 ListUnreceipted(ctrl) 失败: %v", err)
	}
	if len(got) != 1 || got[0].Seq != seq2 {
		t.Errorf("回执后 ctrl 清单 = %v，期望 [%d]", got, seq2)
	}

	// 全回执：非 nil 空切片（JSON null 防线，B1-5/B1-8 消费惯例）。
	if _, _, err := s.InsertReceipt(seq2, exe); err != nil {
		t.Fatalf("回执 seq=%d 失败: %v", seq2, err)
	}
	got, err = s.ListUnreceipted(ctrl)
	if err != nil {
		t.Fatalf("全回执后 ListUnreceipted(ctrl) 失败: %v", err)
	}
	if got == nil {
		t.Error("空清单返回 nil，期望非 nil 空切片")
	}
	if len(got) != 0 {
		t.Errorf("全回执后 ctrl 清单 = %v，期望空", got)
	}
}
