package store

import (
	"database/sql"
	"errors"
	"testing"

	"aiteam/internal/types"
)

// positionRow 读 ack_positions 单行完整值（断言用：position/updated_at/存在性）。
func positionRow(t *testing.T, s *Store, columnID int64, consumer string) (pos int64, updatedAt string, found bool) {
	t.Helper()
	err := s.DB.QueryRow(
		`SELECT position, updated_at FROM ack_positions WHERE column_id = ? AND consumer = ?`,
		columnID, consumer,
	).Scan(&pos, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false
	}
	if err != nil {
		t.Fatalf("读位点行 column=%d consumer=%s 失败: %v", columnID, consumer, err)
	}
	return pos, updatedAt, true
}

// seedPositionsFixture 位点读写测试共享夹具，返回 (项目, 栏目01, ctrl 会话,
// exe 会话, exe2 会话, 消息 seq 映射)：
//
//	项目 p-a / 栏目 01、02；会话 controller@01 / executor-B@01 / executor-C@01。
//	消息五条（seq 按插入序 1~5 自增，键入映射便于断言）：
//	  seq1 direct→executor（exe 信箱可见）
//	  seq2 bus（仅 controller 信箱可见——bus 分支 controller 闸）
//	  seq3 chat→exe（发送方 ctrl——exe 对话可见）
//	  seq4 chat 自发 by exe（自发不回流，任何消费上下文不可见）
//	  seq5 chat→exe2（发送方 ctrl——exe2 对话可见）
func seedPositionsFixture(t *testing.T, s *Store) (pid, c1, ctrl, exe, exe2 int64, seq map[string]int64) {
	t.Helper()
	const t0 = "2026-10-02T08:00:00Z"
	pid = seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, t0)
	c1 = seedColumn(t, s, pid, "01", "栏目01", ColumnStatusActive, t0)
	_ = seedColumn(t, s, pid, "02", "栏目02", ColumnStatusActive, t0)
	ctrl = seedSession(t, s, pid, c1, "controller", "controller", t0, t0)
	exe = seedSession(t, s, pid, c1, "executor-B", "executor", t0, t0)
	exe2 = seedSession(t, s, pid, c1, "executor-C", "executor", t0, t0)

	put := func(key string, got int64) {
		t.Helper()
		seq[key] = got
	}
	seq = map[string]int64{}
	put("direct_exe", seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelImportant, Body: "定向给 executor",
	}, t0))
	put("bus", seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind:            MessageKindBus,
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "总线广播",
	}, t0))
	put("chat_to_exe", seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindChat, TargetSessionID: exe,
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelImportant, Body: "看板发给 exe",
	}, t0))
	put("chat_self", seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindChat, TargetSessionID: exe,
		SenderSessionID: exe, SenderLabel: "executor-B@01",
		Level: MessageLevelNormal, Body: "exe 自发（不回流）",
	}, t0))
	put("chat_to_exe2", seedMessageFull(t, s, MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindChat, TargetSessionID: exe2,
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelImportant, Body: "看板发给 exe2",
	}, t0))

	if seq["direct_exe"] != 1 || seq["bus"] != 2 || seq["chat_to_exe"] != 3 ||
		seq["chat_self"] != 4 || seq["chat_to_exe2"] != 5 {
		t.Fatalf("夹具 seq 自增与预期不符: %v", seq)
	}
	return pid, c1, ctrl, exe, exe2, seq
}

// TestGetPositions §2.2 #10 数据面前置（B2-4 poll handler 消费）：一条
// WHERE consumer IN (角色, chat:session:<id>) 查询取两行（A7 双维度同表）。
//   - 信箱行不存在 → 惰性 upsert 落行 = 当前全表 MAX(seq)（§3.6 注「首次出现时取
//     当前 max(seq)，实现于 store 层 upsert——同规则」；D7 不回看：首见前的消息不
//     回灌）；
//   - 对话行不存在 → 按 0 处理、不落行（待裁点 B2-T3 推荐口径：信箱=广播不回看，
//     对话=点对点恢复现场，两者差异化）；
//   - 位点锁定：已落行后新消息不使读值漂移（读不推进位点，AC8.1 幂等重拉的另一面）。
func TestGetPositions(t *testing.T) {
	const t0 = "2026-10-02T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid, c1, _, exe, exe2, _ := seedPositionsFixture(t, s)
	dialogExe := DialogConsumer(exe)

	// 场景一：两 consumer 行均不存在——首读。
	// 夹具全表 MAX(seq)=5（seq4 自发也计入——惰性初始化口径=全表 max(seq) 字面，
	// 非「我的可见集 max」）。信箱惰性落行=5；对话按 0 不落行。
	got, err := s.GetPositions(pid, c1, "executor", exe)
	if err != nil {
		t.Fatalf("GetPositions 失败: %v", err)
	}
	if got.Mailbox != 5 || got.Dialog != 0 {
		t.Errorf("首读位点 = {Mailbox:%d Dialog:%d}，期望 {5, 0}（信箱=惰性 MAX(seq)，对话=B2-T3 按 0）",
			got.Mailbox, got.Dialog)
	}
	if pos, _, found := positionRow(t, s, c1, "executor"); !found || pos != 5 {
		t.Errorf("惰性落行 (column=%d, executor) = (%d, found=%v)，期望 (5, true)", c1, pos, found)
	}
	if _, _, found := positionRow(t, s, c1, dialogExe); found {
		t.Error("对话位点行不该被读路径落行（B2-T3：按 0 处理不落行）")
	}

	// 场景二：位点锁定——已落行后新消息不使读值漂移。
	if _, err := s.InsertMessage(MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: 0, SenderLabel: "system",
		Level: MessageLevelNormal, Body: "后续新消息 seq=6",
	}); err != nil {
		t.Fatalf("插入后续消息失败: %v", err)
	}
	got, err = s.GetPositions(pid, c1, "executor", exe)
	if err != nil {
		t.Fatalf("二次 GetPositions 失败: %v", err)
	}
	if got.Mailbox != 5 {
		t.Errorf("已落行再读 Mailbox = %d，期望仍 5（惰性初始化只在首读，读不推进）", got.Mailbox)
	}

	// 场景三：新角色首读——惰性初始化=「当时」MAX(seq)=6（各角色独立锁定）。
	got, err = s.GetPositions(pid, c1, "watcher", exe2)
	if err != nil {
		t.Fatalf("新角色 GetPositions 失败: %v", err)
	}
	if got.Mailbox != 6 || got.Dialog != 0 {
		t.Errorf("新角色首读 = {Mailbox:%d Dialog:%d}，期望 {6, 0}（惰性=当时 MAX）", got.Mailbox, got.Dialog)
	}

	// 场景四：双行已存在时直读行值（信箱行=场景三落行的 watcher=6；对话行由本
	// 场景预置=2——行值由写路径维护，读路径原样返回，见 TestAdvancePositions）。
	if _, err := s.DB.Exec(
		`INSERT INTO ack_positions (project_id, column_id, consumer, position, updated_at)
		 VALUES (?, ?, ?, 2, ?)`, pid, c1, DialogConsumer(exe2), t0,
	); err != nil {
		t.Fatalf("预置对话位点行失败: %v", err)
	}
	got, err = s.GetPositions(pid, c1, "watcher", exe2)
	if err != nil {
		t.Fatalf("GetPositions(watcher, exe2) 失败: %v", err)
	}
	if got.Mailbox != 6 || got.Dialog != 2 {
		t.Errorf("双行直读 = {Mailbox:%d Dialog:%d}，期望 {6, 2}", got.Mailbox, got.Dialog)
	}
}

// TestAdvancePositions §2.2 #11 ack 数据面（B2-4 ack handler 消费）：
//   - 显式 seq：信箱+对话两维度均推进到 seq（单事务双 upsert，§3.7「ack=2 UPDATE」）；
//   - seq 省略（0）：推进到当前该消费上下文各自最大可见 seq（可见性口径=poll 谓词
//     三分支去掉位点条件的 MAX——bus 分支 controller 闸、自发 chat 不回流）；
//   - position 取 max（AC9.2 幂等：重复推进零副作用——值与 updated_at 均不变，
//     回退尝试无效果）；
//   - 信箱与对话互不串扰（AC8.3 数据面：A 消费者 ack 不动 B 消费者的行）。
func TestAdvancePositions(t *testing.T) {
	const t0 = "2026-10-02T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid, c1, ctrl, exe, exe2, _ := seedPositionsFixture(t, s)

	// 场景一：显式 seq=3——两维度同推，返回推进后位点。
	got, err := s.AdvancePositions(pid, c1, "executor", exe, 3)
	if err != nil {
		t.Fatalf("AdvancePositions(seq=3) 失败: %v", err)
	}
	if got.Mailbox != 3 || got.Dialog != 3 {
		t.Errorf("显式 seq=3 返回 = {Mailbox:%d Dialog:%d}，期望 {3, 3}", got.Mailbox, got.Dialog)
	}
	if pos, _, found := positionRow(t, s, c1, "executor"); !found || pos != 3 {
		t.Errorf("信箱行 = (%d, found=%v)，期望 (3, true)", pos, found)
	}
	if pos, _, found := positionRow(t, s, c1, DialogConsumer(exe)); !found || pos != 3 {
		t.Errorf("对话行 = (%d, found=%v)，期望 (3, true)", pos, found)
	}

	// 场景二：幂等重复推进（AC9.2）——换时钟后重放同 seq：值不变且 updated_at 不刷
	//（零副作用口径同 projectArchiveSQL 的 CASE 幂等先例）。
	const t1 = "2026-10-02T09:00:00Z"
	types.NowUTC = func() string { return t1 } // injectFixedClock 的 cleanup 仍会还原
	got, err = s.AdvancePositions(pid, c1, "executor", exe, 3)
	if err != nil {
		t.Fatalf("重复 AdvancePositions(seq=3) 失败: %v", err)
	}
	if got.Mailbox != 3 || got.Dialog != 3 {
		t.Errorf("重复推进返回 = {Mailbox:%d Dialog:%d}，期望 {3, 3}", got.Mailbox, got.Dialog)
	}
	if pos, updatedAt, _ := positionRow(t, s, c1, "executor"); pos != 3 || updatedAt != t0 {
		t.Errorf("重复推进后信箱行 = (pos=%d updated_at=%s)，期望 (3, %s)——updated_at 不该被幂等重放刷新",
			pos, updatedAt, t0)
	}
	// 场景二补：对话行同查——幂等重放对两维度零副作用同口径。
	if pos, updatedAt, _ := positionRow(t, s, c1, DialogConsumer(exe)); pos != 3 || updatedAt != t0 {
		t.Errorf("重复推进后对话行 = (pos=%d updated_at=%s)，期望 (3, %s)——updated_at 不该被幂等重放刷新",
			pos, updatedAt, t0)
	}

	// 场景三：回退尝试取 max——seq=1 < 当前行值 3，位点不回退。
	got, err = s.AdvancePositions(pid, c1, "executor", exe, 1)
	if err != nil {
		t.Fatalf("回退 AdvancePositions(seq=1) 失败: %v", err)
	}
	if got.Mailbox != 3 || got.Dialog != 3 {
		t.Errorf("回退尝试返回 = {Mailbox:%d Dialog:%d}，期望仍 {3, 3}（position 取 max）", got.Mailbox, got.Dialog)
	}

	// 场景四：seq 省略（0）=各自消费上下文当前最大可见（§2.2 #11「各自」字面）。
	// executor：信箱可见={seq1 direct 给 executor}（bus seq2 被 controller 闸掉）→1，
	//           与行值 3 取 max 后仍 3；对话可见={seq3 发给我}（seq4 自发不回流）→3。
	got, err = s.AdvancePositions(pid, c1, "executor", exe, 0)
	if err != nil {
		t.Fatalf("省略 AdvancePositions(executor) 失败: %v", err)
	}
	if got.Mailbox != 3 || got.Dialog != 3 {
		t.Errorf("省略推进(executor) = {Mailbox:%d Dialog:%d}，期望 {3, 3}（目标 1/3 与行值 3/3 取 max）",
			got.Mailbox, got.Dialog)
	}
	// controller：信箱可见={seq2 bus}→2；对话可见=0（无发给我的 chat/receipt）。
	got, err = s.AdvancePositions(pid, c1, "controller", ctrl, 0)
	if err != nil {
		t.Fatalf("省略 AdvancePositions(controller) 失败: %v", err)
	}
	if got.Mailbox != 2 || got.Dialog != 0 {
		t.Errorf("省略推进(controller) = {Mailbox:%d Dialog:%d}，期望 {2, 0}", got.Mailbox, got.Dialog)
	}
	// Minor 1：省略时目标 0 也落行——(c1, chat:ctrl) 行存在且 pos=0
	//（行=「该消费上下文 ack 过」的凭据，0 是合法位点值非「没 ack 过」）。
	if pos, _, found := positionRow(t, s, c1, DialogConsumer(ctrl)); !found || pos != 0 {
		t.Errorf("controller 对话行 = (%d, found=%v)，期望 (0, true)——省略 ack 目标 0 也落行", pos, found)
	}
	// executor-C：与 exe 共享 role=executor 信箱行（A7 位点按栏目+角色记账，
	// 非 per-session）——信箱目标=direct seq1=1，与行值 3 取 max→3；
	// 对话目标={seq5 发给 exe2}→5（对话键才是 per-session）。
	got, err = s.AdvancePositions(pid, c1, "executor", exe2, 0)
	if err != nil {
		t.Fatalf("省略 AdvancePositions(executor-C) 失败: %v", err)
	}
	if got.Mailbox != 3 || got.Dialog != 5 {
		t.Errorf("省略推进(executor-C) = {Mailbox:%d Dialog:%d}，期望 {3, 5}", got.Mailbox, got.Dialog)
	}
	// reviewer 角色：无 direct 给 reviewer、bus 被 controller 闸掉→信箱目标 0
	//（COALESCE 兜底，惰性落行 0）；对话键同 exe2 会话，行值已 5 取 max 仍 5。
	got, err = s.AdvancePositions(pid, c1, "reviewer", exe2, 0)
	if err != nil {
		t.Fatalf("省略 AdvancePositions(reviewer) 失败: %v", err)
	}
	if got.Mailbox != 0 || got.Dialog != 5 {
		t.Errorf("省略推进(reviewer) = {Mailbox:%d Dialog:%d}，期望 {0, 5}", got.Mailbox, got.Dialog)
	}

	// 场景五：AC8.3 互不串扰——exe2 的 ack 不动 exe 与 controller 的行
	//（UNIQUE(column_id, consumer) 按行隔离，角色名/对话键两维度均不受牵连）。
	if pos, updatedAt, found := positionRow(t, s, c1, "executor"); !found || pos != 3 || updatedAt != t0 {
		t.Errorf("exe2 ack 后 exe 信箱行 = (%d, %s, found=%v)，期望 (3, %s, true)", pos, updatedAt, found, t0)
	}
	if pos, _, found := positionRow(t, s, c1, "controller"); !found || pos != 2 {
		t.Errorf("exe2 ack 后 controller 信箱行 = (%d, found=%v)，期望 (2, true)", pos, found)
	}
	if pos, _, found := positionRow(t, s, c1, DialogConsumer(exe)); !found || pos != 3 {
		t.Errorf("exe2 ack 后 exe 对话行 = (%d, found=%v)，期望 (3, true)", pos, found)
	}

	// 场景六：负 seq 拒绝（ErrInvalidSeq 哨兵，B2-4 handler 映射 400）。
	if _, err := s.AdvancePositions(pid, c1, "executor", exe, -1); !errors.Is(err, ErrInvalidSeq) {
		t.Errorf("负 seq err = %v，期望 errors.Is ErrInvalidSeq", err)
	}
}

// TestAdvancePositionsHighWatermark 建议 3 守门：显式 seq 超前全表 MAX(seq) 合法
// （高水位——「我已处理到 999」的位点是消费方自报状态，store 层不做上限校验）；
// 锁定该行为不被将来「加 MAX 上限校验」的改动误伤。
func TestAdvancePositionsHighWatermark(t *testing.T) {
	const t0 = "2026-10-02T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid, c1, _, exe, _, _ := seedPositionsFixture(t, s) // 全表 MAX(seq)=5

	got, err := s.AdvancePositions(pid, c1, "executor", exe, 999)
	if err != nil {
		t.Fatalf("高水位 AdvancePositions(seq=999) 失败: %v", err)
	}
	if got.Mailbox != 999 || got.Dialog != 999 {
		t.Errorf("高水位返回 = {Mailbox:%d Dialog:%d}，期望 {999, 999}（超前 MAX(seq) 合法）",
			got.Mailbox, got.Dialog)
	}
	if pos, _, found := positionRow(t, s, c1, "executor"); !found || pos != 999 {
		t.Errorf("高水位信箱行 = (%d, found=%v)，期望 (999, true)", pos, found)
	}
	if pos, _, found := positionRow(t, s, c1, DialogConsumer(exe)); !found || pos != 999 {
		t.Errorf("高水位对话行 = (%d, found=%v)，期望 (999, true)", pos, found)
	}
}

// ackTargetsFromPoll 从 PollVisible 的返回（§3.6 谓词真源）按 kind 分组算「各自
// 消费上下文最大可见 seq」——Important 1 机械对拍的期望值源：信箱=direct+bus
// 两分支 max，对话=chat/receipt 分支 max。位点传 0,0=谓词全开（limit 0=全量），
// 返回集即该消费上下文可见全集。
func ackTargetsFromPoll(t *testing.T, s *Store, pid, c1 int64, role string, sessionID int64) (mailbox, dialog int64) {
	t.Helper()
	msgs, err := s.PollVisible(pid, c1, role, sessionID, 0, 0, 0)
	if err != nil {
		t.Fatalf("PollVisible(位点 0,0) 失败: %v", err)
	}
	for _, m := range msgs {
		switch m.Kind {
		case MessageKindDirect, MessageKindBus:
			mailbox = max(mailbox, m.Seq)
		case MessageKindChat, MessageKindReceipt:
			dialog = max(dialog, m.Seq)
		}
	}
	return mailbox, dialog
}

// assertAckMatchesPoll Important 1 机械对拍：省略 ack（AdvancePositions(...,0)）
// 双位点 == poll 谓词（PollVisible 位点 0,0 全开）按 kind 分组的最大 seq——把
// 「省略 ack 口径与 poll 谓词一致」从注释约定升级为机械守门：任一侧口径漂移
// （如省略误用全表 MAX(seq)、谓词分支增删未同步 ack 侧）即红。
func assertAckMatchesPoll(t *testing.T, s *Store, pid, c1 int64, role string, sessionID int64) {
	t.Helper()
	wantMailbox, wantDialog := ackTargetsFromPoll(t, s, pid, c1, role, sessionID)
	got, err := s.AdvancePositions(pid, c1, role, sessionID, 0)
	if err != nil {
		t.Fatalf("省略 AdvancePositions(role=%s session=%d) 失败: %v", role, sessionID, err)
	}
	if got.Mailbox != wantMailbox || got.Dialog != wantDialog {
		t.Errorf("省略 ack(role=%s session=%d) = {Mailbox:%d Dialog:%d}，与 poll 谓词对拍期望 {%d, %d} 不符",
			role, sessionID, got.Mailbox, got.Dialog, wantMailbox, wantDialog)
	}
}

// TestAdvanceOmittedMatchesPollPredicate Important 1：四种角色组合下的省略 ack
// 与 poll 谓词机械对拍。夹具天然带鉴别力——全表 MAX(seq)=5 是 chat（seq5），而
// executor 信箱目标=1、controller 信箱目标=2：若省略口径误用全表 MAX(seq)， Mailbox
// 会错成 5/5 立即对拍红。
func TestAdvanceOmittedMatchesPollPredicate(t *testing.T) {
	const t0 = "2026-10-02T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid, c1, ctrl, exe, exe2, _ := seedPositionsFixture(t, s)

	// (role, session) 四组合：executor+exe / controller+ctrl / executor+exe2 /
	// reviewer+exe2——覆盖「有 direct 有 chat」「只 bus 无 chat」「共享信箱行+
	// 独立对话行」「信箱空可见」四象限。
	for _, tc := range []struct {
		role string
		sid  int64
	}{
		{"executor", exe},
		{"controller", ctrl},
		{"executor", exe2},
		{"reviewer", exe2},
	} {
		assertAckMatchesPoll(t, s, pid, c1, tc.role, tc.sid)
	}

	// 补一条 direct 后再对拍一轮：验证「poll 与 ack 之间新到消息」场景下两侧
	// 口径仍一致（都把新消息计入最大可见——建议 1 注明的隐式确认语义，两侧同源）。
	if _, err := s.InsertMessage(MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelImportant, Body: "对拍追加消息 seq=6",
	}); err != nil {
		t.Fatalf("插入对拍追加消息失败: %v", err)
	}
	assertAckMatchesPoll(t, s, pid, c1, "executor", exe)
}
