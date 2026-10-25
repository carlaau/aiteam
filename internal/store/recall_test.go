package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"aiteam/internal/types"
)

// ---- b5-T1 撤回域测试夹具（M-36：未消费窗口消息撤回，store 数据面）----

// seedRecallFixture 撤回域共享夹具：项目 p-a / 栏目 01 / 会话 controller@01 与
// executor-B@01。消息按各测试自种（seq 断言需精确控制插入序），此处只铺域骨架。
func seedRecallFixture(t *testing.T, s *Store) (pid, c1, ctrl, exe int64) {
	t.Helper()
	const t0 = "2026-10-25T08:00:00Z"
	pid = seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, t0)
	c1 = seedColumn(t, s, pid, "01", "栏目01", ColumnStatusActive, t0)
	ctrl = seedSession(t, s, pid, c1, "controller", "controller", t0, t0)
	exe = seedSession(t, s, pid, c1, "executor-B", "executor", t0, t0)
	return pid, c1, ctrl, exe
}

// tombstoneRow 直查 recalled_messages 单行（断言用：幂等重放不刷 created_at 的
// 权威证据源——不依赖被测方法的返回面自证）。
func tombstoneRow(t *testing.T, s *Store, seq int64) (rm RecalledMessage, found bool) {
	t.Helper()
	err := s.DB.QueryRow(
		`SELECT message_seq, recalled_by_session, reason, created_at
		 FROM recalled_messages WHERE message_seq = ?`, seq,
	).Scan(&rm.MessageSeq, &rm.RecalledBySession, &rm.Reason, &rm.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RecalledMessage{}, false
	}
	if err != nil {
		t.Fatalf("读墓碑行 seq=%d 失败: %v", seq, err)
	}
	return rm, true
}

// recallDetailFn 测试共用 detailFn：把权威墓碑行编成 detail 快照（回填断言面）。
func recallDetailFn(rm RecalledMessage) string {
	return fmt.Sprintf(`{"seq":%d,"reason":%q,"recalled_by":%d}`, rm.MessageSeq, rm.Reason, rm.RecalledBySession)
}

// TestRecallMessageWithAudit 撤回单事务落库面（b5-spec R2.2/AC3①）：
//   - 墓碑行+审计行同事务落库（action=message.recall），detailFn 拿事务内权威
//     墓碑行（含服务端注入的 created_at——调用方发送前无从得知，回调先例同
//     InsertBoardMessageWithAudit 的 seq 注入）；
//   - 审计弱关联域锚（ProjectID/ColumnID）由 store 从消息归属域自动回填
//     （UpdateProjectWithAudit 回填 entry.ProjectID 先例），SessionID=调用方归因；
//   - 目标 seq 不存在 → ErrMessageNotFound（404 哨兵，B2-5 #12 校验链同源）且
//     零审计落库。
func TestRecallMessageWithAudit(t *testing.T) {
	const t0 = "2026-10-25T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid, c1, ctrl, _ := seedRecallFixture(t, s)
	r1 := seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelImportant, Body: "误发指令",
	}, t0)

	rm, e, err := s.RecallMessageWithAudit(r1, ctrl, "发错栏目",
		AuditEntry{Action: AuditMessageRecall, SessionID: ctrl}, recallDetailFn)
	if err != nil {
		t.Fatalf("RecallMessageWithAudit 失败: %v", err)
	}
	wantRM := RecalledMessage{MessageSeq: r1, RecalledBySession: ctrl, Reason: "发错栏目", CreatedAt: t0}
	if rm != wantRM {
		t.Errorf("返回墓碑行 = %+v，期望 %+v（created_at=服务端注入 t0）", rm, wantRM)
	}
	if e.Action != AuditMessageRecall || e.SessionID != ctrl {
		t.Errorf("审计返回 = (action=%q session=%d)，期望 (%q, %d)", e.Action, e.SessionID, AuditMessageRecall, ctrl)
	}
	if e.ProjectID != pid || e.ColumnID != c1 {
		t.Errorf("审计域锚 = (project=%d column=%d)，期望 (%d, %d)——store 从消息归属域自动回填", e.ProjectID, e.ColumnID, pid, c1)
	}
	if want := fmt.Sprintf(`{"seq":%d,"reason":"发错栏目","recalled_by":%d}`, r1, ctrl); e.Detail != want {
		t.Errorf("审计 detail = %q，期望 %q（detailFn 拿权威墓碑行）", e.Detail, want)
	}

	// 落库面：墓碑行真值直查 + 审计行经 QueryAudit 可见。
	if got, found := tombstoneRow(t, s, r1); !found || got != wantRM {
		t.Errorf("墓碑行 = (%+v, found=%v)，期望 (%+v, true)", got, found, wantRM)
	}
	logs, err := s.QueryAudit(pid, AuditMessageRecall, 10)
	if err != nil {
		t.Fatalf("QueryAudit 失败: %v", err)
	}
	if len(logs) != 1 || logs[0].Detail != e.Detail {
		t.Errorf("audit_log(message.recall) = %d 行 %v，期望 1 行且 detail 对上", len(logs), logs)
	}

	// 不存在 seq → ErrMessageNotFound，零审计新增。
	if _, _, err := s.RecallMessageWithAudit(9999, ctrl, "x",
		AuditEntry{Action: AuditMessageRecall}, recallDetailFn); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("不存在 seq 错误 = %v，期望 errors.Is ErrMessageNotFound", err)
	}
	if n := countRows(t, s, "audit_log"); n != 1 {
		t.Errorf("404 后 audit_log 行数 = %d，期望仍 1（存在性先于动作，无审计）", n)
	}
}

// TestRecallIdempotentReplayKeepsFirstTombstone 墓碑幂等（b5-spec R2.1/AC1⑥）：
// 重复撤回幂等成功——已墓碑再撤 OR IGNORE 跳过，created_at/recalled_by/reason
// 均保留首撤要素（对齐 archive 家族 CASE 口径：幂等不刷时间）；审计每次照记
// （动作发生即留痕，ArchiveProjectWithAudit 重放留痕同口径）。
func TestRecallIdempotentReplayKeepsFirstTombstone(t *testing.T) {
	const t0 = "2026-10-25T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid, c1, ctrl, exe := seedRecallFixture(t, s)
	r1 := seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "待撤",
	}, t0)

	if _, _, err := s.RecallMessageWithAudit(r1, ctrl, "首发撤回",
		AuditEntry{Action: AuditMessageRecall, SessionID: ctrl}, recallDetailFn); err != nil {
		t.Fatalf("首撤失败: %v", err)
	}

	// 换时钟+换撤回人+换理由重放（store 层不做发送方校验——409 not_sender 归
	// handler 校验链；此处验证 OR IGNORE 的纯幂等面）。
	const t1 = "2026-10-25T09:00:00Z"
	types.NowUTC = func() string { return t1 } // injectFixedClock 的 cleanup 仍会还原
	rm2, e2, err := s.RecallMessageWithAudit(r1, exe, "重放换理由",
		AuditEntry{Action: AuditMessageRecall, SessionID: exe}, recallDetailFn)
	if err != nil {
		t.Fatalf("幂等重放失败: %v", err)
	}
	wantRM := RecalledMessage{MessageSeq: r1, RecalledBySession: ctrl, Reason: "首发撤回", CreatedAt: t0}
	if rm2 != wantRM {
		t.Errorf("重放返回墓碑行 = %+v，期望 %+v（首撤要素全保留，created_at 不刷）", rm2, wantRM)
	}
	if got, found := tombstoneRow(t, s, r1); !found || got != wantRM {
		t.Errorf("重放后墓碑行 = (%+v, found=%v)，期望 (%+v, true)——OR IGNORE 不改首撤行", got, found, wantRM)
	}
	// 审计每次照记：两行 message.recall，重放行 detail 记的是权威（首撤）要素。
	if n := countRows(t, s, "audit_log"); n != 2 {
		t.Errorf("重放后 audit_log 行数 = %d，期望 2（幂等成功≠零留痕，每次照记）", n)
	}
	if e2.ID <= 0 || e2.SessionID != exe {
		t.Errorf("重放审计 = (id=%d session=%d)，期望 id>0 且归因重放会话 %d", e2.ID, e2.SessionID, exe)
	}
}

// TestRecallFiltersPollAllKinds 消费面过滤+投递零回归（b5-spec R2.3/AC2①③）：
// 撤回后收件方 poll 零命中且内容永不可见（direct/chat 两必撤形态 + bus 分支
// 统一过滤的裁量④验证——store 层不设 kind 闸，handler 校验链归 T2）；未撤回的
// direct/chat/bus/receipt 四 kind 投递照常（零回归面）。
func TestRecallFiltersPollAllKinds(t *testing.T) {
	const t0 = "2026-10-25T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid, c1, ctrl, exe := seedRecallFixture(t, s)

	// 七连种（seq 按插入序 1~7）：撤 1/3/5（direct/chat/bus 各一），留 2/4/6/7。
	dRec := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelImportant, Body: "误发指令"}, t0)
	dKeep := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "正常指令"}, t0)
	cRec := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1,
		Kind: MessageKindChat, TargetSessionID: exe,
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "误发对话"}, t0)
	cKeep := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1,
		Kind: MessageKindChat, TargetSessionID: exe,
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "正常对话"}, t0)
	bRec := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1,
		Kind: MessageKindBus, SenderSessionID: exe, SenderLabel: "executor-B@01",
		Level: MessageLevelNormal, Body: "误广播"}, t0)
	bKeep := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1,
		Kind: MessageKindBus, SenderSessionID: exe, SenderLabel: "executor-B@01",
		Level: MessageLevelNormal, Body: "正常广播"}, t0)
	rtKeep := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1,
		Kind: MessageKindReceipt, TargetSessionID: exe,
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "回执回告"}, t0)

	for _, seq := range []int64{dRec, cRec, bRec} {
		if _, _, err := s.RecallMessageWithAudit(seq, ctrl, "测试撤回",
			AuditEntry{Action: AuditMessageRecall, SessionID: ctrl}, recallDetailFn); err != nil {
			t.Fatalf("撤回 seq=%d 失败: %v", seq, err)
		}
	}

	// executor 消费上下文：direct（撤 1 滤/留 2 到）+ chat/receipt（撤 3 滤/留 4、7 到）。
	got, err := s.PollVisible(pid, c1, "executor", exe, 0, 0, 0)
	if err != nil {
		t.Fatalf("executor PollVisible 失败: %v", err)
	}
	if want := []int64{dKeep, cKeep, rtKeep}; !slices.Equal(seqs(got), want) {
		t.Errorf("executor poll = %v，期望 %v（撤回的 direct/chat 零命中，未撤四 kind 照常）", seqs(got), want)
	}

	// controller 消费上下文：bus 可见集（撤 5 滤/留 6 到）——bus 分支过滤（裁量④）。
	got, err = s.PollVisible(pid, c1, "controller", ctrl, 0, 0, 0)
	if err != nil {
		t.Fatalf("controller PollVisible 失败: %v", err)
	}
	if want := []int64{bKeep}; !slices.Equal(seqs(got), want) {
		t.Errorf("controller poll = %v，期望 %v（撤回的 bus 被统一过滤）", seqs(got), want)
	}

	// 内容永不可见：三具墓碑的 body 不出现在任何 poll 输出（不只 seq 缺席）。
	recalledBodies := map[string]bool{"误发指令": true, "误发对话": true, "误广播": true}
	for _, m := range got {
		if recalledBodies[m.Body] {
			t.Errorf("撤回消息 body %q 泄漏进 controller poll（seq=%d）", m.Body, m.Seq)
		}
	}
}

// TestRecallPositionAdvanceCrossesTombstone 位点跨越墓碑无感（b5-spec AC2②）：
// 收件方位点已越过墓碑 seq 后 ack 正常——墓碑只影响 poll 可见性，不影响位点
// 推进语义（位点只进不退 + 高水位合法口径同 TestAdvancePositionsHighWatermark）。
func TestRecallPositionAdvanceCrossesTombstone(t *testing.T) {
	const t0 = "2026-10-25T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid, c1, ctrl, exe := seedRecallFixture(t, s)
	r1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "撤后跨越",
	}, t0)
	if _, _, err := s.RecallMessageWithAudit(r1, ctrl, "跨越测试",
		AuditEntry{Action: AuditMessageRecall, SessionID: ctrl}, recallDetailFn); err != nil {
		t.Fatalf("撤回失败: %v", err)
	}

	// 撤回后收件方 poll 零命中（过滤先证）。
	if got, err := s.PollVisible(pid, c1, "executor", exe, 0, 0, 0); err != nil {
		t.Fatalf("撤回后 PollVisible 失败: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("撤回后 executor poll = %v，期望零命中", seqs(got))
	}

	// 位点显式推到墓碑 seq 之后（高水位）——推进正常，位点机制对墓碑无感。
	got, err := s.AdvancePositions(pid, c1, "executor", exe, r1+10)
	if err != nil {
		t.Fatalf("跨越墓碑 AdvancePositions(seq=%d) 失败: %v", r1+10, err)
	}
	if got.Mailbox != r1+10 || got.Dialog != r1+10 {
		t.Errorf("跨越推进 = {Mailbox:%d Dialog:%d}，期望 {%d, %d}", got.Mailbox, got.Dialog, r1+10, r1+10)
	}

	// 越过墓碑后再 poll 零命中、回退尝试无效果（位点只进不退，与墓碑互不干扰）。
	if msgs, err := s.PollVisible(pid, c1, "executor", exe, got.Mailbox, got.Dialog, 0); err != nil || len(msgs) != 0 {
		t.Errorf("越过墓碑后 poll = (msgs=%v err=%v)，期望零命中无错", seqs(msgs), err)
	}
	got, err = s.AdvancePositions(pid, c1, "executor", exe, r1)
	if err != nil {
		t.Fatalf("回退 AdvancePositions(seq=%d) 失败: %v", r1, err)
	}
	if got.Mailbox != r1+10 {
		t.Errorf("回退后 Mailbox = %d，期望 %d（位点只进不退，墓碑不改变推进语义）", got.Mailbox, r1+10)
	}
}

// TestRecallOmittedAckSwallowsRecalledSeq 省略 ack 与墓碑的口径分歧登记（b5-T1
// 开工令裁量：maxVisibleMailbox 省略 ack 侧**不加**过滤——位点机制文件冻结面）。
// 两面锁定（结构镜像 TestAdvanceOmittedSwallowsSelfBusOnly）：
//
//	①分歧存在性：撤回后 poll 零命中，但省略 ack 信箱位点=墓碑 seq——ack 侧
//	  maxVisibleMailbox 不过滤墓碑，位点越位跨过 poll 可见最大（0）；
//	②无害性：越位后新到的可见消息（direct）零丢失到场——seq 单调递增，新消息
//	  恒 > 位点，越位只吞墓碑 seq 不吞任何（现在或将来）poll 可见消息
//	  （self-bus 吞没同款论证，messages.go 口径分歧登记段）。
func TestRecallOmittedAckSwallowsRecalledSeq(t *testing.T) {
	const t0 = "2026-10-25T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid, c1, ctrl, exe := seedRecallFixture(t, s)
	r1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "唯一消息（撤）",
	}, t0)
	if _, _, err := s.RecallMessageWithAudit(r1, ctrl, "分歧登记",
		AuditEntry{Action: AuditMessageRecall, SessionID: ctrl}, recallDetailFn); err != nil {
		t.Fatalf("撤回失败: %v", err)
	}

	// ①poll 侧过滤：零命中；省略 ack 侧不过滤：Mailbox=r1——分歧实拍锁定。
	if got, err := s.PollVisible(pid, c1, "executor", exe, 0, 0, 0); err != nil || len(got) != 0 {
		t.Fatalf("撤回后 poll = (%v, err=%v)，期望零命中", seqs(got), err)
	}
	pos, err := s.AdvancePositions(pid, c1, "executor", exe, 0)
	if err != nil {
		t.Fatalf("省略 AdvancePositions 失败: %v", err)
	}
	if pos.Mailbox != r1 {
		t.Errorf("省略 ack 信箱位点 = %d，期望 %d（ack 侧 maxVisibleMailbox 不过滤墓碑——越位跨过 poll 可见最大 0）", pos.Mailbox, r1)
	}

	// ②无害性：新到可见 direct（seq > 位点）零丢失到场。
	d2 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "越位后新消息",
	}, t0)
	got, err := s.PollVisible(pid, c1, "executor", exe, pos.Mailbox, pos.Dialog, 0)
	if err != nil {
		t.Fatalf("越位后 PollVisible 失败: %v", err)
	}
	if want := []int64{d2}; !slices.Equal(seqs(got), want) {
		t.Errorf("越位后 poll = %v，期望 %v（越位只吞墓碑 seq，后续可见消息零丢失）", seqs(got), want)
	}
}

// TestRecallMessageWithAuditAtomic 单事务原子性（b5-spec R2.2/AC3①）：审计参数
// 无效（action 空）时整体回滚——不留半程墓碑（「消息已撤而留痕缺失」与反向的
// 「墓碑已落而动作无痕」都违反 AC1.4 原子前提；手段同 TestArchiveProjectWithAudit）。
func TestRecallMessageWithAuditAtomic(t *testing.T) {
	const t0 = "2026-10-25T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid, c1, ctrl, _ := seedRecallFixture(t, s)
	r1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1,
		Kind: MessageKindChat, TargetSessionID: 0,
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "原子性探针",
	}, t0)

	if _, _, err := s.RecallMessageWithAudit(r1, ctrl, "x",
		AuditEntry{Action: " "}, recallDetailFn); !errors.Is(err, ErrAuditInvalid) {
		t.Fatalf("action 空 → 错误 = %v，期望 errors.Is ErrAuditInvalid", err)
	}
	if _, found := tombstoneRow(t, s, r1); found {
		t.Error("审计失败后墓碑行残留——期望整体回滚零墓碑（不留半程）")
	}
	if n := countRows(t, s, "audit_log"); n != 0 {
		t.Errorf("回滚后 audit_log 行数 = %d，期望 0", n)
	}
}

// TestRecallMigrationUpgradesStockV3 旧库升级路径（b5-spec R2.1 迁移兼容）：
// 存量 v3 停版库（v1 基线+v2 扩列+v3 扩列，user_version=3，含既有消息行）重开——
// v4 迁移自动补 recalled_messages 表、版本追至迁移数组末位、再次重开幂等；
// 升级库上撤回落库可用（FK 指向既有消息行成立）。手段镜像 TestMigrationsV3FromProject
// 的存量库造法（裸连接手跑迁移停版，TestMigrationsV3FromProject 先例）。
func TestRecallMigrationUpgradesStockV3(t *testing.T) {
	v3Path := filepath.Join(t.TempDir(), "stock-v3.db")
	db, err := sql.Open("sqlite", "file:"+v3Path)
	if err != nil {
		t.Fatalf("裸开存量 v3 库失败: %v", err)
	}
	for _, m := range migrations[:3] { // v1 基线 + v2 + v3，止步于 3
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("手跑迁移 v%d 失败: %v", m.version, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=3`); err != nil {
		t.Fatalf("停版 user_version=3 失败: %v", err)
	}
	// 裸连接未开 foreign_keys，直插 messages 免父行夹具（先例同 TestMigrationsV3FromProject）。
	if _, err := db.Exec(`INSERT INTO messages (project_id, column_id, kind, level, body, created_at)
		VALUES (1, 1, 'direct', 'normal', 'v3 时代存量行', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("插入 v3 时代存量行失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭存量 v3 库失败: %v", err)
	}

	// 重开：v4 自动补表，版本追至迁移数组末位；升级库上撤回可用（FK 命中存量行 seq=1）。
	s, err := Open(v3Path)
	if err != nil {
		t.Fatalf("存量 v3 库重开失败: %v", err)
	}
	if v := userVersion(t, s.DB); v != len(migrations) {
		t.Errorf("存量 v3 库重开后 user_version = %d，期望 = 迁移数组长度 %d（v4 补表）", v, len(migrations))
	}
	const t0 = "2026-10-25T08:00:00Z"
	injectFixedClock(t, t0)
	rm, _, err := s.RecallMessageWithAudit(1, 7, "升级库撤回",
		AuditEntry{Action: AuditMessageRecall, SessionID: 7}, recallDetailFn)
	if err != nil {
		t.Fatalf("升级库上撤回失败: %v", err)
	}
	if rm.MessageSeq != 1 || rm.CreatedAt != t0 {
		t.Errorf("升级库墓碑行 = %+v，期望 seq=1 且 created_at=%s", rm, t0)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("升级库 Close 失败: %v", err)
	}

	// 二次重开幂等：版本不动、表仍在（墓碑行可回读）。
	s2, err := Open(v3Path)
	if err != nil {
		t.Fatalf("二次重开失败: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if v := userVersion(t, s2.DB); v != len(migrations) {
		t.Errorf("二次重开后 user_version = %d，期望 %d（幂等）", v, len(migrations))
	}
	if _, found := tombstoneRow(t, s2, 1); !found {
		t.Error("二次重开后墓碑行丢失（升级数据未持久化或表被重建）")
	}
}

// ---- b5-T4 看板墓碑读面（b5-spec R5/AC3②③）：QueryHistory/QuerySessionTimeline
// 墓碑标记+body 剥离。红线：墓碑消息 body 永不出现在看板读面——剥离落 SQL 层
// （CASE 置空，非 handler/前端自律），标记与剥离同源单点。 ----

// TestQueryHistoryTombstoneMarker #13 历史读面（#26 bus-stream 同源查询）墓碑标记：
// 已撤回 direct 行 Recalled=true 且 Body 已剥空；未撤回行零漂移（Recalled 零值+
// Body 原文）。墓碑不可达面（bus v1 不可撤）下 #26 分支恒空标记，本测以 direct
// 锁标记机制本体。
func TestQueryHistoryTombstoneMarker(t *testing.T) {
	const t0 = "2026-10-25T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid, c1, ctrl, _ := seedRecallFixture(t, s)
	recalledSeq := seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "已撤回正文",
	}, t0)
	keptSeq := seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "保留正文",
	}, t0)
	if _, _, err := s.RecallMessageWithAudit(recalledSeq, ctrl, "", AuditEntry{Action: AuditMessageRecall, SessionID: ctrl}, recallDetailFn); err != nil {
		t.Fatalf("撤回夹具失败: %v", err)
	}

	msgs, err := s.QueryHistory(HistoryFilter{Kind: MessageKindDirect, Order: "asc"})
	if err != nil {
		t.Fatalf("QueryHistory 失败: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("历史行数 = %d，期望 2（墓碑行仍在历史观测面，只标记不删行）", len(msgs))
	}
	bySeq := make(map[int64]Message, len(msgs))
	for _, m := range msgs {
		bySeq[m.Seq] = m
	}
	got := bySeq[recalledSeq]
	if !got.Recalled {
		t.Errorf("墓碑行 seq=%d Recalled = false，期望 true（b5-spec R5 看板墓碑标记）", recalledSeq)
	}
	if got.Body != "" {
		t.Errorf("墓碑行 seq=%d Body = %q，期望空串（SQL 层剥离硬闸——看板只显占位不显 body）", recalledSeq, got.Body)
	}
	kept := bySeq[keptSeq]
	if kept.Recalled {
		t.Errorf("未撤回行 seq=%d Recalled = true，期望 false（零值不受涉）", keptSeq)
	}
	if kept.Body != "保留正文" {
		t.Errorf("未撤回行 seq=%d Body = %q，期望原文（正常消息零漂移）", keptSeq, kept.Body)
	}
}

// TestQuerySessionTimelineTombstoneMarker #25 会话时间线读面墓碑标记：chat 分支
// 墓碑行 Recalled=true 且 Body 剥空（chat 可撤 kind，时间线合流后仍须标记）；
// direct 分支未撤回行零漂移；progress 合流行（合成 kind）无墓碑语义，结构零触碰
// 由既有 TestQuerySessionTimeline 系列锁定，本测不重复。
func TestQuerySessionTimelineTombstoneMarker(t *testing.T) {
	const t0 = "2026-10-25T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid, c1, ctrl, exe := seedRecallFixture(t, s)
	chatRecalledSeq := seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1, Kind: MessageKindChat, TargetSessionID: exe,
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelImportant, Body: "对话撤回稿",
	}, t0)
	directKeptSeq := seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "到格正常消息",
	}, t0)
	if _, _, err := s.RecallMessageWithAudit(chatRecalledSeq, ctrl, "", AuditEntry{Action: AuditMessageRecall, SessionID: ctrl}, recallDetailFn); err != nil {
		t.Fatalf("撤回夹具失败: %v", err)
	}

	msgs, err := s.QuerySessionTimeline(exe, c1, "executor", 0)
	if err != nil {
		t.Fatalf("QuerySessionTimeline 失败: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("时间线行数 = %d，期望 2（chat 墓碑行+direct 正常行）", len(msgs))
	}
	bySeq := make(map[int64]Message, len(msgs))
	for _, m := range msgs {
		bySeq[m.Seq] = m
	}
	got := bySeq[chatRecalledSeq]
	if got.Kind != MessageKindChat {
		t.Fatalf("墓碑行 kind = %q，期望 chat（夹具错位）", got.Kind)
	}
	if !got.Recalled {
		t.Errorf("chat 墓碑行 seq=%d Recalled = false，期望 true（b5-spec R5 看板墓碑标记）", chatRecalledSeq)
	}
	if got.Body != "" {
		t.Errorf("chat 墓碑行 seq=%d Body = %q，期望空串（SQL 层剥离硬闸）", chatRecalledSeq, got.Body)
	}
	kept := bySeq[directKeptSeq]
	if kept.Recalled {
		t.Errorf("direct 未撤回行 seq=%d Recalled = true，期望 false（零值不受涉）", directKeptSeq)
	}
	if kept.Body != "到格正常消息" {
		t.Errorf("direct 未撤回行 seq=%d Body = %q，期望原文（UNION 两侧正常行零漂移）", directKeptSeq, kept.Body)
	}
}
