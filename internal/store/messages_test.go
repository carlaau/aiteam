package store

import (
	"errors"
	"maps"
	"slices"
	"testing"
)

// ---- messages 域测试夹具（直插行，不依赖 handler/CRUD）----
// seedSession/seedColumn/seedProject 复用 sessions_test.go/columns_test.go/store_test.go 既有 helper，勿重声明。

// seedMessageFull 全字段直插 messages 夹具（columns_test.go 的 seedMessage 只覆盖
// 最小字段集；chat/direct/receipt 谓词测试需要 target/sender 四字段，扩展此版。
// 不改 seedMessage——既有消费方签名稳定）。
func seedMessageFull(t *testing.T, s *Store, in MessageInput, createdAt string) int64 {
	t.Helper()
	res, err := s.DB.Exec(
		`INSERT INTO messages (project_id, column_id, kind, target_role, target_session_id,
		       sender_session_id, sender_label, level, body, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ProjectID, in.ColumnID, in.Kind, in.TargetRole, in.TargetSessionID,
		in.SenderSessionID, in.SenderLabel, in.Level, in.Body, createdAt,
	)
	if err != nil {
		t.Fatalf("插入夹具消息 kind=%s 失败: %v", in.Kind, err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取夹具消息 seq 失败: %v", err)
	}
	return seq
}

// mailboxPosition 读 ack_positions 一行位点值（D7 测试用：CreateColumn 预置位点后
// 读出真实值传入 PollVisible——位点读写函数是 B2-2 任务，此处直查 SQL 不提前实现）。
func mailboxPosition(t *testing.T, s *Store, columnID int64, consumer string) int64 {
	t.Helper()
	var pos int64
	if err := s.DB.QueryRow(
		`SELECT position FROM ack_positions WHERE column_id = ? AND consumer = ?`,
		columnID, consumer,
	).Scan(&pos); err != nil {
		t.Fatalf("读位点 column=%d consumer=%s 失败: %v", columnID, consumer, err)
	}
	return pos
}

// seqs 提取消息切片的 seq 列表（断言辅助）。
func seqs(msgs []Message) []int64 {
	out := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Seq)
	}
	return out
}

// ---- TestInsertFourKinds：四 kind 落库 + column_id 归属规则（§3.2 表 5 注）----

// TestInsertFourKinds AC5.x：direct/bus/chat/receipt 四 kind 单 INSERT 落库。
// 逐 kind 断言 column_id 归属（§3.2 表 5 注：direct=目标栏目；bus=发送方所在栏目
// 审计锚点；chat=目标会话所属栏目；receipt=原发送方会话栏目——四 kind 均为真实
// 栏目行，归属由调用方算好经 MessageInput.ColumnID 传入，store 纯落库）；
// target 字段缺省（非目标语义 kind 落 DDL 默认值：空串与 0）；seq 全局严格递增；
// created_at=types.NowUTC 注入时钟（AC5.4 多机时钟免疫机制）。
func TestInsertFourKinds(t *testing.T) {
	const t0 = "2026-10-02T08:00:00Z"
	injectFixedClock(t, t0)
	s := openTemp(t)
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, t0)
	c1 := seedColumn(t, s, pid, "01", "栏目01", ColumnStatusActive, t0)
	c2 := seedColumn(t, s, pid, "02", "栏目02", ColumnStatusActive, t0)
	ctrl := seedSession(t, s, pid, c1, "controller", "controller", t0, t0)
	exe := seedSession(t, s, pid, c2, "executor-B", "executor", t0, t0)

	// direct：column_id=目标栏目 c2、target_role=目标角色、target_session_id 缺省 0。
	direct, err := s.InsertMessage(MessageInput{
		ProjectID: pid, ColumnID: c2,
		Kind: MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelImportant, Body: "定向指令",
	})
	if err != nil {
		t.Fatalf("插入 direct 失败: %v", err)
	}
	if direct.ColumnID != c2 || direct.TargetRole != "executor" || direct.TargetSessionID != 0 {
		t.Errorf("direct 落库 = (column=%d role=%q tsession=%d)，期望 (%d, executor, 0)",
			direct.ColumnID, direct.TargetRole, direct.TargetSessionID, c2)
	}

	// bus：column_id=发送方所在栏目 c1（审计锚点），target_role/target_session_id 缺省。
	bus, err := s.InsertMessage(MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind:            MessageKindBus,
		SenderSessionID: ctrl, SenderLabel: "controller@01",
		Level: MessageLevelNormal, Body: "总线广播",
	})
	if err != nil {
		t.Fatalf("插入 bus 失败: %v", err)
	}
	if bus.ColumnID != c1 || bus.TargetRole != "" || bus.TargetSessionID != 0 {
		t.Errorf("bus 落库 = (column=%d role=%q tsession=%d)，期望 (%d, '', 0)",
			bus.ColumnID, bus.TargetRole, bus.TargetSessionID, c1)
	}

	// chat：column_id=目标会话所属栏目 c2（executor-B 挂 c2）、target_session_id=目标会话。
	chat, err := s.InsertMessage(MessageInput{
		ProjectID: pid, ColumnID: c2,
		Kind: MessageKindChat, TargetSessionID: exe,
		SenderSessionID: 0, SenderLabel: "board-user:张三",
		Level: MessageLevelImportant, Body: "看板对话",
	})
	if err != nil {
		t.Fatalf("插入 chat 失败: %v", err)
	}
	if chat.ColumnID != c2 || chat.TargetSessionID != exe {
		t.Errorf("chat 落库 = (column=%d tsession=%d)，期望 (%d, %d)", chat.ColumnID, chat.TargetSessionID, c2, exe)
	}

	// receipt：column_id=原发送方会话栏目 c1（controller 发的原消息）、target_session_id=原发送方会话。
	receipt, err := s.InsertMessage(MessageInput{
		ProjectID: pid, ColumnID: c1,
		Kind: MessageKindReceipt, TargetSessionID: ctrl,
		SenderSessionID: 0, SenderLabel: "system",
		Level: MessageLevelNormal, Body: "receipt: seq=1 by executor-B at " + t0,
	})
	if err != nil {
		t.Fatalf("插入 receipt 失败: %v", err)
	}
	if receipt.ColumnID != c1 || receipt.TargetSessionID != ctrl {
		t.Errorf("receipt 落库 = (column=%d tsession=%d)，期望 (%d, %d)", receipt.ColumnID, receipt.TargetSessionID, c1, ctrl)
	}

	// seq 全局严格递增（AUTOINCREMENT 取号，FR5；不承诺连续——事务回滚烧号合法，
	// 故只断递增不断连续）；sender/level/body/created_at 逐字段回断。
	if !(bus.Seq > direct.Seq && chat.Seq > bus.Seq && receipt.Seq > chat.Seq) {
		t.Errorf("seq 非严格递增: direct=%d bus=%d chat=%d receipt=%d", direct.Seq, bus.Seq, chat.Seq, receipt.Seq)
	}
	if direct.SenderLabel != "controller@01" || direct.SenderSessionID != ctrl ||
		direct.Level != MessageLevelImportant || direct.Body != "定向指令" || direct.ProjectID != pid {
		t.Errorf("direct 五要素/身份字段回读不符: %+v", direct)
	}
	if chat.SenderLabel != "board-user:张三" || receipt.SenderLabel != "system" {
		t.Errorf("sender_label 回读不符: chat=%q receipt=%q", chat.SenderLabel, receipt.SenderLabel)
	}
	if direct.CreatedAt != t0 || bus.CreatedAt != t0 || chat.CreatedAt != t0 || receipt.CreatedAt != t0 {
		t.Errorf("created_at 应=注入时钟 %s（AC5.4）: %s %s %s %s",
			t0, direct.CreatedAt, bus.CreatedAt, chat.CreatedAt, receipt.CreatedAt)
	}

	// 参数校验兜底（handler 主责，store 同构防御——projects.go 空 code 惯例）：
	// kind/level 出枚举、body 空 → ErrMessageInvalid。
	if _, err := s.InsertMessage(MessageInput{ProjectID: pid, ColumnID: c1, Kind: "bogus", Level: "normal", Body: "x"}); !errors.Is(err, ErrMessageInvalid) {
		t.Errorf("非法 kind 应返回 ErrMessageInvalid，实际: %v", err)
	}
	if _, err := s.InsertMessage(MessageInput{ProjectID: pid, ColumnID: c1, Kind: "bus", Level: "loud", Body: "x"}); !errors.Is(err, ErrMessageInvalid) {
		t.Errorf("非法 level 应返回 ErrMessageInvalid，实际: %v", err)
	}
	if _, err := s.InsertMessage(MessageInput{ProjectID: pid, ColumnID: c1, Kind: "bus", Level: "normal", Body: "  "}); !errors.Is(err, ErrMessageInvalid) {
		t.Errorf("空白 body 应返回 ErrMessageInvalid，实际: %v", err)
	}
}

// ---- TestPollPredicateDirect：direct 分支（栏目+角色+seq>位点，升序）----

// TestPollPredicateDirect AC8.1/AC8.4：direct 消息按（栏目,角色）命中、只拉位点
// 之后（seq > 信箱位点）、按 seq 升序；位点推进后幂等重拉不回看旧消息。
func TestPollPredicateDirect(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-10-02T08:00:00Z"
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, t0)
	c1 := seedColumn(t, s, pid, "01", "栏目01", ColumnStatusActive, t0)
	c2 := seedColumn(t, s, pid, "02", "栏目02", ColumnStatusActive, t0)
	ctrl := seedSession(t, s, pid, c1, "controller", "controller", t0, t0)
	exe := seedSession(t, s, pid, c1, "executor-A", "executor", t0, t0)

	d1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor", SenderSessionID: ctrl, SenderLabel: "controller@01", Level: "normal", Body: "d1"}, t0)
	d2 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "controller", SenderSessionID: exe, SenderLabel: "executor-A@01", Level: "normal", Body: "d2"}, t0)
	d3 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor", SenderSessionID: ctrl, SenderLabel: "controller@01", Level: "important", Body: "d3"}, t0)
	// 其他栏目的 executor direct：角色命中但栏目不符，c1 视角不可见。
	seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c2, Kind: MessageKindDirect, TargetRole: "executor", SenderSessionID: ctrl, SenderLabel: "controller@01", Level: "normal", Body: "d4-other-column"}, t0)

	// executor@c1 位点 0：见 d1/d3（升序），不见 controller 的 d2、不见 c2 的 d4。
	got, err := s.PollVisible(pid, c1, "executor", exe, 0, 0, 0)
	if err != nil {
		t.Fatalf("PollVisible(executor) 失败: %v", err)
	}
	if want := []int64{d1, d3}; !slices.Equal(seqs(got), want) {
		t.Errorf("executor 位点 0 = %v，期望 %v（升序 AC8.1）", seqs(got), want)
	}
	for _, m := range got {
		if m.Kind != MessageKindDirect {
			t.Errorf("direct 视角混入 kind=%s", m.Kind)
		}
	}

	// 位点推进到 d1：只剩 d3（seq > 位点，AC8.4 五要素之位点语义）。
	got, err = s.PollVisible(pid, c1, "executor", exe, d1, 0, 0)
	if err != nil {
		t.Fatalf("PollVisible(位点=%d) 失败: %v", d1, err)
	}
	if want := []int64{d3}; !slices.Equal(seqs(got), want) {
		t.Errorf("executor 位点=%d = %v，期望 %v（不回看）", d1, seqs(got), want)
	}

	// controller@c1 位点 0：只见 d2。
	got, err = s.PollVisible(pid, c1, "controller", ctrl, 0, 0, 0)
	if err != nil {
		t.Fatalf("PollVisible(controller) 失败: %v", err)
	}
	if want := []int64{d2}; !slices.Equal(seqs(got), want) {
		t.Errorf("controller 位点 0 = %v，期望 %v", seqs(got), want)
	}
}

// ---- TestPollPredicateBus：bus 分支（项目级可见性 + controller 角色闸 + D7 不回看）----

// TestPollPredicateBus AC6.2/D7：controller 对同项目全部 bus 可见（含跨栏目——
// bus 分支只按 project_id 过滤）；非 controller 角色恒不可见（角色闸参数化在
// SQL 谓词内）；新登记栏目位点=登记时 MAX(seq)（B1 InitColumnPositions 预置），
// 历史 bus 不回灌（D7 不回看），此后新发的 bus 正常可见。
func TestPollPredicateBus(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-10-02T08:00:00Z"
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, t0)
	cA := seedColumn(t, s, pid, "01", "栏目01", ColumnStatusActive, t0)
	cB := seedColumn(t, s, pid, "02", "栏目02", ColumnStatusActive, t0)
	ctrl := seedSession(t, s, pid, cA, "controller", "controller", t0, t0)
	exe := seedSession(t, s, pid, cA, "executor-A", "executor", t0, t0)

	b1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: cA, Kind: MessageKindBus, SenderSessionID: ctrl, SenderLabel: "controller@01", Level: "normal", Body: "bus-1"}, t0)
	b2 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: cB, Kind: MessageKindBus, SenderSessionID: exe, SenderLabel: "executor-A@02", Level: "important", Body: "bus-2"}, t0)

	// controller（挂栏目 A、位点 0）：跨栏目见项目全部 bus（AC6.2）。
	got, err := s.PollVisible(pid, cA, "controller", ctrl, 0, 0, 0)
	if err != nil {
		t.Fatalf("PollVisible(controller) 失败: %v", err)
	}
	if want := []int64{b1, b2}; !slices.Equal(seqs(got), want) {
		t.Errorf("controller 位点 0 = %v，期望 %v（跨栏目同项目 bus 全可见）", seqs(got), want)
	}

	// executor（位点 0）：bus 分支角色闸关闭，bus 不可见（direct 亦无）——
	// 空、非 nil（JSON null 防线测试化）。
	got, err = s.PollVisible(pid, cA, "executor", exe, 0, 0, 0)
	if err != nil {
		t.Fatalf("PollVisible(executor) 失败: %v", err)
	}
	if got == nil {
		t.Fatal("executor 空命中应返回非 nil 空切片（JSON 序列化 null 防线），实际 nil")
	}
	if len(got) != 0 {
		t.Errorf("executor 不应见 bus，实际 %v", seqs(got))
	}

	// D7 不回看：新登记栏目 C，B1 InitColumnPositions 预置位点=当时 MAX(seq)=b2。
	cC, err := s.CreateColumn("p-a", Column{Code: "03", Name: "栏目03"})
	if err != nil {
		t.Fatalf("登记新栏目 C 失败: %v", err)
	}
	pos := mailboxPosition(t, s, cC.ID, "controller")
	if pos != b2 {
		t.Fatalf("新栏目 controller 位点 = %d，期望 = 当时 MAX(seq) %d（惰性初始化=MAX 语义）", pos, b2)
	}
	got, err = s.PollVisible(pid, cC.ID, "controller", ctrl, pos, 0, 0)
	if err != nil {
		t.Fatalf("PollVisible(新栏目位点) 失败: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("新栏目不应看到历史 bus（D7 不回看），实际 %v", seqs(got))
	}

	// 此后新发 bus：新栏目位点之上，正常可见。
	b3 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: cA, Kind: MessageKindBus, SenderSessionID: ctrl, SenderLabel: "controller@01", Level: "normal", Body: "bus-3"}, t0)
	got, err = s.PollVisible(pid, cC.ID, "controller", ctrl, pos, 0, 0)
	if err != nil {
		t.Fatalf("PollVisible(新 bus 后) 失败: %v", err)
	}
	if want := []int64{b3}; !slices.Equal(seqs(got), want) {
		t.Errorf("新栏目位点之上 = %v，期望 %v", seqs(got), want)
	}
}

// ---- TestPollPredicateChat：chat/receipt 分支（target_session + 排除自发）----

// TestPollPredicateChat AC15.4：chat/receipt 按 target_session_id 命中；自发消息
// 不回流（sender_session_id = 当前会话被排除）；同栏目两会话互不可见；对话位点
// （pos_d）推进后不回看。
func TestPollPredicateChat(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-10-02T08:00:00Z"
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, t0)
	c1 := seedColumn(t, s, pid, "01", "栏目01", ColumnStatusActive, t0)
	sa := seedSession(t, s, pid, c1, "executor-A", "executor", t0, t0)
	sb := seedSession(t, s, pid, c1, "executor-B", "executor", t0, t0)

	m1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindChat, TargetSessionID: sa, SenderSessionID: 0, SenderLabel: "board-user:张三", Level: "important", Body: "board->A"}, t0)
	// 自发：A 发给自己的 chat，A 不可见（自己的回复不回流，§2.2 #24 语义）。
	seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindChat, TargetSessionID: sa, SenderSessionID: sa, SenderLabel: "executor-A@01", Level: "normal", Body: "A->A self"}, t0)
	// 发给 B 的 chat：A 不可见（互不可见 AC15.4）。
	m3 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindChat, TargetSessionID: sb, SenderSessionID: sa, SenderLabel: "executor-A@01", Level: "normal", Body: "A->B"}, t0)
	// receipt 回告：目标=原发送方 A，A 可见。
	m4 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindReceipt, TargetSessionID: sa, SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "receipt: seq=1 by executor-B"}, t0)

	// A 视角（对话位点 0）：m1(chat) + m4(receipt)，排除自发 m2、排除发给 B 的 m3。
	got, err := s.PollVisible(pid, c1, "executor", sa, 0, 0, 0)
	if err != nil {
		t.Fatalf("PollVisible(A) 失败: %v", err)
	}
	if want := []int64{m1, m4}; !slices.Equal(seqs(got), want) {
		t.Errorf("A 对话位点 0 = %v，期望 %v（chat+receipt、排除自发与他两会话）", seqs(got), want)
	}

	// B 视角：只见发给 B 的 m3。
	got, err = s.PollVisible(pid, c1, "executor", sb, 0, 0, 0)
	if err != nil {
		t.Fatalf("PollVisible(B) 失败: %v", err)
	}
	if want := []int64{m3}; !slices.Equal(seqs(got), want) {
		t.Errorf("B 对话位点 0 = %v，期望 %v（会话隔离 AC15.4）", seqs(got), want)
	}

	// A 推进对话位点到 m1：只剩 m4（seq > pos_d）。
	got, err = s.PollVisible(pid, c1, "executor", sa, 0, m1, 0)
	if err != nil {
		t.Fatalf("PollVisible(A, pos_d=%d) 失败: %v", m1, err)
	}
	if want := []int64{m4}; !slices.Equal(seqs(got), want) {
		t.Errorf("A 对话位点=%d = %v，期望 %v（对话位点独立于信箱位点）", m1, seqs(got), want)
	}
}

// ---- TestPollLimit：D8 上限语义（0=全量 / >0=截断 / 负值报错 / 缺省 500 常量）----

// TestPollLimit D8：显式 0=全量；>0 按 LIMIT 截断（升序取前 n）；负值返回
// ErrInvalidLimit（可识别错误供 handler 映射 400）；DefaultPollLimit=500 为
// handler/CLI 的缺省注入值（store 不私设默认——0 的语义就是全量）。
func TestPollLimit(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-10-02T08:00:00Z"
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, t0)
	c1 := seedColumn(t, s, pid, "01", "栏目01", ColumnStatusActive, t0)
	exe := seedSession(t, s, pid, c1, "executor-A", "executor", t0, t0)

	for range 10 {
		seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor", SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "m"}, t0)
	}

	// limit=3：升序取前 3。
	got, err := s.PollVisible(pid, c1, "executor", exe, 0, 0, 3)
	if err != nil {
		t.Fatalf("PollVisible(limit=3) 失败: %v", err)
	}
	if len(got) != 3 || got[0].Seq != 1 || got[2].Seq != 3 {
		t.Errorf("limit=3 = %v，期望升序前 3 条 [1 2 3]", seqs(got))
	}

	// limit=0：显式全量（10 条）。
	got, err = s.PollVisible(pid, c1, "executor", exe, 0, 0, 0)
	if err != nil {
		t.Fatalf("PollVisible(limit=0) 失败: %v", err)
	}
	if len(got) != 10 {
		t.Errorf("limit=0 应全量返回 10 条，实际 %d", len(got))
	}

	// limit<0：ErrInvalidLimit。
	if _, err := s.PollVisible(pid, c1, "executor", exe, 0, 0, -1); !errors.Is(err, ErrInvalidLimit) {
		t.Errorf("limit=-1 应返回 ErrInvalidLimit，实际: %v", err)
	}

	// 缺省 500 锚定（D8：handler/CLI 缺省注入此常量）。
	if DefaultPollLimit != 500 {
		t.Errorf("DefaultPollLimit = %d，期望 500（D8 poll 缺省上限）", DefaultPollLimit)
	}
}

// ---- TestPollVisibleMixedBranchesLimit：三分支混合+双位点非零+limit 跨分支截断 ----

// TestPollVisibleMixedBranchesLimit 混合用例：direct/bus/chat 同库并存、信箱与
// 对话两位点均非零（各自分支先过滤）、limit=2 对三分支合并升序后截断（取 seq
// 最小 2 条）——双位点组合与跨分支截断两缺口一个用例覆盖。
func TestPollVisibleMixedBranchesLimit(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-10-02T08:00:00Z"
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, t0)
	c1 := seedColumn(t, s, pid, "01", "栏目01", ColumnStatusActive, t0)
	ctrl := seedSession(t, s, pid, c1, "controller", "controller", t0, t0)

	// seq 递增：dc1(direct→controller) b1(bus) ch1(chat→ctrl) ch2(chat→ctrl) dc2(direct→controller)
	dc1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "controller", SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "dc1"}, t0)
	b1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindBus, SenderSessionID: ctrl, SenderLabel: "controller@01", Level: "normal", Body: "b1"}, t0)
	ch1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindChat, TargetSessionID: ctrl, SenderSessionID: 0, SenderLabel: "board-user:张三", Level: "important", Body: "ch1"}, t0)
	ch2 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindChat, TargetSessionID: ctrl, SenderSessionID: 0, SenderLabel: "board-user:张三", Level: "normal", Body: "ch2"}, t0)
	dc2 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "controller", SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "dc2"}, t0)

	// 两位点非零：信箱位点=dc1（排除 dc1）、对话位点=ch1（排除 ch1）。
	// 合并可见集 = {b1, ch2, dc2}；limit=2 → seq 最小 2 条（升序跨分支截断）。
	got, err := s.PollVisible(pid, c1, "controller", ctrl, dc1, ch1, 2)
	if err != nil {
		t.Fatalf("PollVisible(混合 limit=2) 失败: %v", err)
	}
	if want := []int64{b1, ch2}; !slices.Equal(seqs(got), want) {
		t.Errorf("混合两位点 limit=2 = %v，期望 %v（合并升序后取 seq 最小 2 条）", seqs(got), want)
	}

	// 同参 limit=0（全量）：合并可见集 3 条升序，双位点排除语义复核。
	got, err = s.PollVisible(pid, c1, "controller", ctrl, dc1, ch1, 0)
	if err != nil {
		t.Fatalf("PollVisible(混合全量) 失败: %v", err)
	}
	if want := []int64{b1, ch2, dc2}; !slices.Equal(seqs(got), want) {
		t.Errorf("混合两位点全量 = %v，期望 %v", seqs(got), want)
	}
}

// ---- TestCrossGridDirectPending：跨格未消费 direct 计数（b7-W1 消费侧自诊断数据面）----

// toPendingMap 跨格计数行转 角色→条数 映射（断言辅助）。
func toPendingMap(rows []CrossGridPending) map[string]int {
	out := make(map[string]int, len(rows))
	for _, p := range rows {
		out[p.Role] = p.Pending
	}
	return out
}

// TestCrossGridDirectPending b7-spec §二.1/§二.5：同栏目他格未消费 direct 按
// target_role 分组计数——位点感知口径=consumer 为目标 role 的 ack 位点（与
// pollHitsSQL direct 分支同口径：位点行缺失 COALESCE 0=全量计数）；excludeRole
// 不出现（自己格排除）；bus/chat/receipt 不计入（只数 direct）；ack 推进后对应格
// 计数下降、全消费后清零（清零=GROUP BY 无行）；栏目隔离（他栏目 direct 不串）；
// 空态=非 nil 空切片；计数降序（数据无并列，序可锁）。
func TestCrossGridDirectPending(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-10-02T08:00:00Z"
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, t0)
	c1 := seedColumn(t, s, pid, "01", "栏目01", ColumnStatusActive, t0)
	c2 := seedColumn(t, s, pid, "02", "栏目02", ColumnStatusActive, t0)
	c3 := seedColumn(t, s, pid, "03", "栏目03", ColumnStatusActive, t0)
	exeB := seedSession(t, s, pid, c1, "executor-B", "executor", t0, t0)

	// 消息面（c1 为被查栏目；seq 依插入序递增）：
	// c1：executor-B ×3、executor-C ×1、executor-A ×2（自己格应排除）+ bus/chat/receipt 各 1（不计数）；
	// c2：executor-B ×1（他栏目 direct，栏目隔离）。
	b1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor-B", SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "b1"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor-A", SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "a1"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor-B", SenderSessionID: 0, SenderLabel: "system", Level: "important", Body: "b2"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindBus, SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "bus"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor-C", SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "c1"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindChat, TargetSessionID: exeB, SenderSessionID: 0, SenderLabel: "board-user:张三", Level: "normal", Body: "chat"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindReceipt, TargetSessionID: exeB, SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "receipt"}, t0)
	b3 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor-B", SenderSessionID: 0, SenderLabel: "system", Level: "block", Body: "b3"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor-A", SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "a2"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c2, Kind: MessageKindDirect, TargetRole: "executor-B", SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "other-column"}, t0)

	// 1) executor-A 视角：他格 executor-B=3、executor-C=1（位点行缺失=COALESCE 0 全量计数）；
	// 自己格 executor-A 不出现；bus/chat/receipt 不计入；降序（首行=最大计数 3）。
	got, err := s.CrossGridDirectPending(c1, "executor-A")
	if err != nil {
		t.Fatalf("CrossGridDirectPending(c1, executor-A) 失败: %v", err)
	}
	if want := map[string]int{"executor-B": 3, "executor-C": 1}; !maps.Equal(toPendingMap(got), want) {
		t.Errorf("executor-A 视角 = %v，期望 %v（他格计数/排除自己格/只数 direct）", toPendingMap(got), want)
	}
	if len(got) == 0 || got[0].Pending != 3 {
		t.Errorf("计数应降序（首行 Pending=3），实际 %v", got)
	}

	// 2) 排除角色切换：excludeRole=executor-B → 结果含 executor-A=2、executor-C=1，无 executor-B。
	got, err = s.CrossGridDirectPending(c1, "executor-B")
	if err != nil {
		t.Fatalf("CrossGridDirectPending(c1, executor-B) 失败: %v", err)
	}
	if want := map[string]int{"executor-A": 2, "executor-C": 1}; !maps.Equal(toPendingMap(got), want) {
		t.Errorf("executor-B 视角 = %v，期望 %v（excludeRole 不出现）", toPendingMap(got), want)
	}

	// 3) 位点感知——executor-B 经 #11 ack 推进到 b1（消费 1 条）：计数 3→2（seq 严格 > 位点）。
	if _, err := s.AdvancePositions(pid, c1, "executor-B", exeB, b1); err != nil {
		t.Fatalf("ack 推进 executor-B 位点到 %d 失败: %v", b1, err)
	}
	got, err = s.CrossGridDirectPending(c1, "executor-A")
	if err != nil {
		t.Fatalf("部分消费后查询失败: %v", err)
	}
	if want := map[string]int{"executor-B": 2, "executor-C": 1}; !maps.Equal(toPendingMap(got), want) {
		t.Errorf("executor-B ack 到 b1 后 = %v，期望 %v（位点感知计数）", toPendingMap(got), want)
	}

	// 4) 全消费归零：executor-B 推进到 b3 → 不再成行（清零=GROUP BY 无行，非 Pending=0 行）。
	if _, err := s.AdvancePositions(pid, c1, "executor-B", exeB, b3); err != nil {
		t.Fatalf("ack 推进 executor-B 位点到 %d 失败: %v", b3, err)
	}
	got, err = s.CrossGridDirectPending(c1, "executor-A")
	if err != nil {
		t.Fatalf("全消费后查询失败: %v", err)
	}
	if want := map[string]int{"executor-C": 1}; !maps.Equal(toPendingMap(got), want) {
		t.Errorf("executor-B 全消费后 = %v，期望 %v（清零格不成行）", toPendingMap(got), want)
	}

	// 5) 栏目隔离：c2 视角只见自己栏目积压（executor-B=1，b1~b3 的位点推进不跨栏目）；
	// c3 无消息 → 空、非 nil（JSON [] 防线）。
	got, err = s.CrossGridDirectPending(c2, "executor-A")
	if err != nil {
		t.Fatalf("CrossGridDirectPending(c2) 失败: %v", err)
	}
	if want := map[string]int{"executor-B": 1}; !maps.Equal(toPendingMap(got), want) {
		t.Errorf("c2 视角 = %v，期望 %v（他栏目 direct 不串）", toPendingMap(got), want)
	}
	got, err = s.CrossGridDirectPending(c3, "executor-A")
	if err != nil {
		t.Fatalf("CrossGridDirectPending(c3) 失败: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("无消息栏目应返回空非 nil 切片，实际 %v", got)
	}
}

// ---- TestHistoryFilter：#13 全参数过滤 + 默认 desc/50 ----

// TestHistoryFilter §2.2 #13：project/column/kind/level/sent_by/since_seq/
// before_seq/limit/order 全参数组合抽查；默认 desc 最新在前、limit 缺省 50
// 上限 500；code/sent_by 未命中返回空列表而非报错（#13 无 404 语义，archived
// 域不拒同理）；since_seq 含起点（>=）、before_seq 不含（<）——互补区间语义。
func TestHistoryFilter(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-10-02T08:00:00Z"
	pid1 := seedProject(t, s, "p-1", "项目1", ProjectStatusActive, 900, t0)
	pid2 := seedProject(t, s, "p-2", "项目2", ProjectStatusActive, 900, t0)
	c1 := seedColumn(t, s, pid1, "01", "栏目01", ColumnStatusActive, t0)
	c2 := seedColumn(t, s, pid1, "02", "栏目02", ColumnStatusActive, t0)
	c3 := seedColumn(t, s, pid2, "01", "P2栏目01", ColumnStatusActive, t0)
	alice := seedSession(t, s, pid1, c1, "alice", "controller", t0, t0)
	bob := seedSession(t, s, pid1, c2, "bob", "executor", t0, t0)

	// 消息面（seq 递增 1~5）：
	// 1: p1.c1 direct executor normal alice
	// 2: p1.c1 bus normal alice
	// 3: p1.c2 chat important bob(target=bob)
	// 4: p1.c2 direct controller block bob
	// 5: p2.c3 bus important alice
	seedMessageFull(t, s, MessageInput{ProjectID: pid1, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor", SenderSessionID: alice, SenderLabel: "alice@01", Level: "normal", Body: "m1"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid1, ColumnID: c1, Kind: MessageKindBus, SenderSessionID: alice, SenderLabel: "alice@01", Level: "normal", Body: "m2"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid1, ColumnID: c2, Kind: MessageKindChat, TargetSessionID: bob, SenderSessionID: bob, SenderLabel: "bob@02", Level: "important", Body: "m3"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid1, ColumnID: c2, Kind: MessageKindDirect, TargetRole: "controller", SenderSessionID: bob, SenderLabel: "bob@02", Level: "block", Body: "m4"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid2, ColumnID: c3, Kind: MessageKindBus, SenderSessionID: alice, SenderLabel: "alice@01", Level: "important", Body: "m5"}, t0)

	t.Run("kind", func(t *testing.T) {
		got, err := s.QueryHistory(HistoryFilter{Kind: MessageKindBus})
		if err != nil {
			t.Fatalf("QueryHistory(kind=bus) 失败: %v", err)
		}
		if want := []int64{5, 2}; !slices.Equal(seqs(got), want) {
			t.Errorf("kind=bus = %v，期望 %v（desc）", seqs(got), want)
		}
	})
	t.Run("level", func(t *testing.T) {
		got, err := s.QueryHistory(HistoryFilter{Level: MessageLevelBlock})
		if err != nil {
			t.Fatalf("QueryHistory(level=block) 失败: %v", err)
		}
		if want := []int64{4}; !slices.Equal(seqs(got), want) {
			t.Errorf("level=block = %v，期望 %v", seqs(got), want)
		}
	})
	t.Run("project_and_column", func(t *testing.T) {
		got, err := s.QueryHistory(HistoryFilter{ProjectCode: "p-1"})
		if err != nil {
			t.Fatalf("QueryHistory(project=p-1) 失败: %v", err)
		}
		if want := []int64{4, 3, 2, 1}; !slices.Equal(seqs(got), want) {
			t.Errorf("project=p-1 = %v，期望 %v", seqs(got), want)
		}
		got, err = s.QueryHistory(HistoryFilter{ProjectCode: "p-1", ColumnCode: "01"})
		if err != nil {
			t.Fatalf("QueryHistory(project+column) 失败: %v", err)
		}
		if want := []int64{2, 1}; !slices.Equal(seqs(got), want) {
			t.Errorf("project=p-1 column=01 = %v，期望 %v", seqs(got), want)
		}
	})
	t.Run("sent_by", func(t *testing.T) {
		got, err := s.QueryHistory(HistoryFilter{SentBy: "alice"})
		if err != nil {
			t.Fatalf("QueryHistory(sent_by=alice) 失败: %v", err)
		}
		if want := []int64{5, 2, 1}; !slices.Equal(seqs(got), want) {
			t.Errorf("sent_by=alice = %v，期望 %v", seqs(got), want)
		}
	})
	t.Run("since_before", func(t *testing.T) {
		got, err := s.QueryHistory(HistoryFilter{SinceSeq: 2, Order: "asc"})
		if err != nil {
			t.Fatalf("QueryHistory(since=2) 失败: %v", err)
		}
		if want := []int64{2, 3, 4, 5}; !slices.Equal(seqs(got), want) {
			t.Errorf("since_seq=2 asc = %v，期望 %v（含起点）", seqs(got), want)
		}
		got, err = s.QueryHistory(HistoryFilter{BeforeSeq: 4})
		if err != nil {
			t.Fatalf("QueryHistory(before=4) 失败: %v", err)
		}
		if want := []int64{3, 2, 1}; !slices.Equal(seqs(got), want) {
			t.Errorf("before_seq=4 = %v，期望 %v（不含端点）", seqs(got), want)
		}
		got, err = s.QueryHistory(HistoryFilter{SinceSeq: 2, BeforeSeq: 4, Order: "asc"})
		if err != nil {
			t.Fatalf("QueryHistory(since=2,before=4) 失败: %v", err)
		}
		if want := []int64{2, 3}; !slices.Equal(seqs(got), want) {
			t.Errorf("since=2 before=4 asc = %v，期望 %v（互补区间 [2,4)）", seqs(got), want)
		}
	})
	t.Run("组合抽查", func(t *testing.T) {
		got, err := s.QueryHistory(HistoryFilter{ProjectCode: "p-1", Kind: MessageKindDirect, Level: MessageLevelNormal})
		if err != nil {
			t.Fatalf("QueryHistory(组合) 失败: %v", err)
		}
		if want := []int64{1}; !slices.Equal(seqs(got), want) {
			t.Errorf("p-1+direct+normal = %v，期望 %v", seqs(got), want)
		}
	})
	t.Run("order_asc", func(t *testing.T) {
		got, err := s.QueryHistory(HistoryFilter{Order: "asc"})
		if err != nil {
			t.Fatalf("QueryHistory(order=asc) 失败: %v", err)
		}
		if want := []int64{1, 2, 3, 4, 5}; !slices.Equal(seqs(got), want) {
			t.Errorf("order=asc = %v，期望 %v", seqs(got), want)
		}
	})
	t.Run("默认desc与limit", func(t *testing.T) {
		// 默认（零值 Order/Limit）：desc 最新在前，全量 5 条。
		got, err := s.QueryHistory(HistoryFilter{})
		if err != nil {
			t.Fatalf("QueryHistory(零值) 失败: %v", err)
		}
		if want := []int64{5, 4, 3, 2, 1}; !slices.Equal(seqs(got), want) {
			t.Errorf("默认序 = %v，期望 %v（desc）", seqs(got), want)
		}
		// 显式 limit=2：最新 2 条。
		got, err = s.QueryHistory(HistoryFilter{Limit: 2})
		if err != nil {
			t.Fatalf("QueryHistory(limit=2) 失败: %v", err)
		}
		if want := []int64{5, 4}; !slices.Equal(seqs(got), want) {
			t.Errorf("limit=2 = %v，期望 %v", seqs(got), want)
		}
		// limit 超上限 500：截断不报错（数据只有 5 条，全返）。
		got, err = s.QueryHistory(HistoryFilter{Limit: MaxHistoryLimit + 1})
		if err != nil {
			t.Fatalf("QueryHistory(limit>500) 失败: %v", err)
		}
		if len(got) != 5 {
			t.Errorf("limit>500 应截断为 500 上限（数据 5 条全返），实际 %d", len(got))
		}
	})
	t.Run("未命中返回空非nil", func(t *testing.T) {
		got, err := s.QueryHistory(HistoryFilter{ProjectCode: "nope"})
		if err != nil {
			t.Fatalf("QueryHistory(不存在项目) 失败: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Errorf("不存在项目应返回空非 nil 切片，实际 %v", got)
		}
		got, err = s.QueryHistory(HistoryFilter{SentBy: "ghost"})
		if err != nil {
			t.Fatalf("QueryHistory(不存在会话名) 失败: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Errorf("不存在 sent_by 应返回空非 nil 切片，实际 %v", got)
		}
	})
	t.Run("order非法报错", func(t *testing.T) {
		if _, err := s.QueryHistory(HistoryFilter{Order: "sideways"}); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("order=sideways 应返回 ErrInvalidOrder，实际: %v", err)
		}
	})
	t.Run("默认limit50", func(t *testing.T) {
		// 独立库种 60 条：零值过滤只返回最新 50 条（§2.2 #13 默认 50）。
		s2 := openTemp(t)
		pid := seedProject(t, s2, "p-l", "项目L", ProjectStatusActive, 900, t0)
		col := seedColumn(t, s2, pid, "01", "栏目01", ColumnStatusActive, t0)
		for range 60 {
			seedMessageFull(t, s2, MessageInput{ProjectID: pid, ColumnID: col, Kind: MessageKindBus, SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "m"}, t0)
		}
		got, err := s2.QueryHistory(HistoryFilter{})
		if err != nil {
			t.Fatalf("QueryHistory(60条零值) 失败: %v", err)
		}
		if len(got) != DefaultHistoryLimit {
			t.Fatalf("默认 limit = %d 条，期望 %d", len(got), DefaultHistoryLimit)
		}
		// desc：首条=最新 seq(60)，末条=seq(11)。
		if got[0].Seq != 60 || got[49].Seq != 11 {
			t.Errorf("默认 50 条应=最新 50 条（seq 60..11），实际首=%d 末=%d", got[0].Seq, got[49].Seq)
		}
		if DefaultHistoryLimit != 50 || MaxHistoryLimit != 500 {
			t.Errorf("history 常量漂移: default=%d max=%d，期望 50/500（§2.2 #13）", DefaultHistoryLimit, MaxHistoryLimit)
		}
	})
}

// ---- TestQuerySessionTimeline：chat 双向+direct 到格合流（seq DESC）/ 会话隔离 / LIMIT 缺省（b3-W1）----

// TestQuerySessionTimeline b3-W1 T7：Q2 合流查询——chat 目标会话命中（board 发送
// 与目标会话自发回复双向都进流，QueryDialog 全流语义同款）+ direct 到格（column+
// role 命中，同格两会话共享）按 seq DESC 合流；会话隔离（S2 流不见 S1 的 chat）；
// 他角色/他栏目 direct 不混入；receipt/bus 不进流；limit<=0 缺省=DefaultDialogLimit(100)。
func TestQuerySessionTimeline(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-10-02T08:00:00Z"
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, t0)
	c1 := seedColumn(t, s, pid, "01", "栏目01", ColumnStatusActive, t0)
	c2 := seedColumn(t, s, pid, "02", "栏目02", ColumnStatusActive, t0)
	s1 := seedSession(t, s, pid, c1, "executor-A", "executor", t0, t0)
	s2 := seedSession(t, s, pid, c1, "executor-B", "executor", t0, t0)
	ctl := seedSession(t, s, pid, c1, "controller", "controller", t0, t0)
	quiet := seedSession(t, s, pid, c2, "observer-Q", "observer", t0, t0)

	// 编排（seq 插入序递增）——s1 时间线期望集（DESC）：in4 > in3 > in2 > in1。
	//  in1: chat→s1 board 发（board→会话方向）
	//  in2: direct→(01,executor)（到格合流——s1/s2 同格共享）
	//  in3: chat→s1 自发 s1（会话→board 回复方向：sender==target 也进流，全流视图）
	//  in4: chat→s1 board 发（最新，DESC 首条）
	// 排除面（漏入 s1 流即 want 对拍红）：
	//  out1: chat→s2 board 发 / out2: chat→s2 sender=s1（会话间私聊——进 s2 流不进 s1 流）
	//  out3: direct→(01,controller)（他角色 direct 不混入）
	//  out4: direct→(02,executor)（他栏目同角色 direct 不混入——栏目过滤锚）
	//  out5: receipt→s1（Q2 谓词无 receipt）/ out6: bus（不进流）
	in1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindChat, TargetSessionID: s1, SenderSessionID: 0, SenderLabel: "board-user:张三", Level: "normal", Body: "board->s1"}, t0)
	in2 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor", SenderSessionID: ctl, SenderLabel: "controller@01", Level: "normal", Body: "to-grid"}, t0)
	in3 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindChat, TargetSessionID: s1, SenderSessionID: s1, SenderLabel: "executor-A@01", Level: "normal", Body: "s1-reply"}, t0)
	out1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindChat, TargetSessionID: s2, SenderSessionID: 0, SenderLabel: "board-user:张三", Level: "normal", Body: "board->s2"}, t0)
	out2 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindChat, TargetSessionID: s2, SenderSessionID: s1, SenderLabel: "executor-A@01", Level: "normal", Body: "s1->s2"}, t0)
	out3 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "controller", SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "to-controller"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c2, Kind: MessageKindDirect, TargetRole: "executor", SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "other-column"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindReceipt, TargetSessionID: s1, SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "receipt"}, t0)
	seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindBus, SenderSessionID: ctl, SenderLabel: "controller@01", Level: "normal", Body: "bus"}, t0)
	in4 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindChat, TargetSessionID: s1, SenderSessionID: 0, SenderLabel: "board-user:张三", Level: "normal", Body: "board->s1-latest"}, t0)

	// s1 时间线（limit 0=缺省 100，数据 4 条全量）：chat 双向+到格 direct 按 seq DESC。
	got, err := s.QuerySessionTimeline(s1, c1, "executor", 0)
	if err != nil {
		t.Fatalf("QuerySessionTimeline(s1) 失败: %v", err)
	}
	if want := []int64{in4, in3, in2, in1}; !slices.Equal(seqs(got), want) {
		t.Errorf("s1 时间线 = %v，期望 %v（chat 双向+direct 到格，DESC）", seqs(got), want)
	}

	// 显式小 limit=2：DESC 截断取最新 2 条。
	got, err = s.QuerySessionTimeline(s1, c1, "executor", 2)
	if err != nil {
		t.Fatalf("QuerySessionTimeline(s1, limit=2) 失败: %v", err)
	}
	if want := []int64{in4, in3}; !slices.Equal(seqs(got), want) {
		t.Errorf("s1 时间线 limit=2 = %v，期望 %v（DESC 截断）", seqs(got), want)
	}

	// 会话隔离：s2 流=发给 s2 的双向 chat（out2>out1）+同格 direct in2——不见 s1 的
	// chat 流（in1/in3/in4 目标是 s1）。
	got, err = s.QuerySessionTimeline(s2, c1, "executor", 0)
	if err != nil {
		t.Fatalf("QuerySessionTimeline(s2) 失败: %v", err)
	}
	if want := []int64{out2, out1, in2}; !slices.Equal(seqs(got), want) {
		t.Errorf("s2 时间线 = %v，期望 %v（会话隔离+同格 direct 共享）", seqs(got), want)
	}

	// 他角色会话：ctl 流只见 out3（自己格的 direct 命中；executor 面 direct 不混入）。
	got, err = s.QuerySessionTimeline(ctl, c1, "controller", 0)
	if err != nil {
		t.Fatalf("QuerySessionTimeline(ctl) 失败: %v", err)
	}
	if want := []int64{out3}; !slices.Equal(seqs(got), want) {
		t.Errorf("ctl 时间线 = %v，期望 %v（他角色 direct 各归各格）", seqs(got), want)
	}

	// 空命中：quiet（02/observer——无 chat 无 direct 到格）——非 nil 空切片
	// （JSON null 防线，QueryDialog/PollVisible 同款）。
	got, err = s.QuerySessionTimeline(quiet, c2, "observer", 0)
	if err != nil {
		t.Fatalf("QuerySessionTimeline(quiet) 失败: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("空命中应返回非 nil 空切片，实际 %v", got)
	}

	// 缺省 100 截断：独立库 120 条 chat → 恰 100 条、首条=最新（DESC 取最新窗）。
	sLim := openTemp(t)
	pLim := seedProject(t, sLim, "p-l", "项目L", ProjectStatusActive, 900, t0)
	cLim := seedColumn(t, sLim, pLim, "01", "栏目01", ColumnStatusActive, t0)
	sessLim := seedSession(t, sLim, pLim, cLim, "executor-L", "executor", t0, t0)
	for range 120 {
		seedMessageFull(t, sLim, MessageInput{ProjectID: pLim, ColumnID: cLim, Kind: MessageKindChat, TargetSessionID: sessLim, SenderSessionID: 0, SenderLabel: "board-user:张三", Level: "normal", Body: "m"}, t0)
	}
	got, err = sLim.QuerySessionTimeline(sessLim, cLim, "executor", 0)
	if err != nil {
		t.Fatalf("QuerySessionTimeline(缺省 limit) 失败: %v", err)
	}
	if len(got) != DefaultDialogLimit {
		t.Fatalf("缺省 limit = %d 条，期望 %d（DefaultDialogLimit）", len(got), DefaultDialogLimit)
	}
	if got[0].Seq != 120 || got[99].Seq != 21 {
		t.Errorf("缺省 100 条应=最新 100 条（seq 120..21），实际首=%d 末=%d", got[0].Seq, got[99].Seq)
	}
}
