package server

import (
	"net/http"
	"strings"
	"testing"

	"aiteam/internal/store"
)

// 多栏同名会话歧义测试（M-42，v0.4.2-b2-T1 红笔）：病灶=store GetSessionByName
// 按（项目, name）查询无栏位约束、ORDER BY s.id LIMIT 1 最早行静默胜出，send
// chat 目标解析与 poll/ack 消费视角对话位点定位两个调用点共用此歧义。裁定修复
// 方向（本文件只锚行为不落实现，绿笔落码）：
//   - send chat 偏好锚=发送方 --column（身份四头 column）；poll/ack 偏好锚=
//     query column 参数（resolveMailboxDomain 的 ref.ColumnID）；
//   - 偏好栏命中 → 命中即用；偏好栏无命中且同名多行 → 409 显式报错，错误码
//     session_ambiguous（新码未落 types，本文件按任务书禁引未存在符号，以字符
//     串字面量锚定），message 附候选清单（名称@栏目 逐行列出，风格对齐
//     agent_conflict 现款）；偏好栏无命中但全项目单行 → 回落命中该单行
//     （单行场景响应与文案逐字节不变）；
//   - 跨项目 chat 变体（target.project 指向他项目）：发送方栏不在目标项目域 →
//     无偏好锚，同名多行直接 409。
//
// 五锚红绿分布（T1 验收线）：锚 1~4 修复前 FAIL（断言红非编译红）、修复后 PASS；
// 锚 5 负面锚修复前后均 PASS（防「栏位优先」过收敛把栏外单行误伤为 4xx）。
//
// 文件裁量留痕：独立新文件不并入 handler_messages_test.go——①M-42 歧义专题
// 测试聚簇（T1 红笔/后续绿笔对拍/E2E 同文件演进，diff 面不混入既有测试噪声）；
// ②既有文件已 2594 行，b2-spec §五拆分预案同方向；③helper 全复用包内既有件
// （newTestEnv/seedProjectColumn/seedColumnOnly/doAuthedReq/wantData/wantErrBody/
// errMsg/pollQuery/assertMailboxData/seqFromData/findMessageBySeq/countTable/
// heads4），零重复定义。
//
// 时钟纪律：send chat/poll 位点定位均不判会话活性（GetSessionByName 无时钟
// 依赖），注入固定时钟仅为照惯例稳定中间件心跳 upsert 的 last_seen（fixture
// 注册先于注入判定窗口，单边/双边活性谓词下都稳，TestSendCrossProjectDegenerate
// 口径）；注册后即可发送，无需时钟推进。

// ambiguousFixture 一对同名会话注册 helper（M-42 五锚共用）：projID 项目内
// colFirst（先注册、id 更小=病灶「最早行静默胜出」的受害行）与 colSecond 两栏
// 各注册 name 会话，返回两行会话 id 供落库 target_session_id 归属断言；注册序
// 前提（first 行 id < second 行 id）失败即 Fatal（夹具破坏时不引偏排查方向）。
func ambiguousFixture(t *testing.T, st *store.Store, projID, colFirst, colSecond int64, name string) (firstID, secondID int64) {
	t.Helper()
	for i, cid := range []int64{colFirst, colSecond} {
		up, err := st.UpsertSession(projID, cid, name, "executor")
		if err != nil {
			t.Fatalf("预注册同名会话 %q 到栏 %d 失败: %v", name, cid, err)
		}
		if i == 0 {
			firstID = up.Session.ID
		} else {
			secondID = up.Session.ID
		}
	}
	if firstID >= secondID {
		t.Fatalf("夹具前提破：先注册行 id=%d 不小于后注册行 id=%d（同名歧义病灶依赖注册序，顺序错则红绿分布失真）", firstID, secondID)
	}
	return firstID, secondID
}

// mustColumnID 按项目 code+栏目 code 查栏目 id（ambiguousFixture 后注册栏的
// id 取回面——seedColumnOnly 返回值与注册序耦合，直查解耦）。
func mustColumnID(t *testing.T, st *store.Store, projCode, colCode string) int64 {
	t.Helper()
	var id int64
	err := st.DB.QueryRow(
		`SELECT c.id FROM columns c JOIN projects p ON p.id = c.project_id
		 WHERE p.code = ? AND c.code = ?`, projCode, colCode,
	).Scan(&id)
	if err != nil {
		t.Fatalf("查询栏目夹具 %s/%s 失败: %v", projCode, colCode, err)
	}
	return id
}

// TestSendChatAmbiguousSenderColumnWins 锚 1（栏位优先落真身）：栏 A（05，先
// 注册 id 更小）、栏 B（06）各注册同名会话 executor-B；发送方身份声明在栏 B
// 发 chat → 修复后偏好锚（发送方 --column=06）命中栏 B 行，消息 target_session_id
// 落栏 B 行：栏 A 视角 poll（query column=05&session=同名）拉不到、栏 B 视角能
// 拉到。修复前 GetSessionByName 最早行（A 栏行）静默胜出 → 消息落 A 栏行、A 视
// 角能拉到 → 本锚 FAIL=红。
func TestSendChatAmbiguousSenderColumnWins(t *testing.T) {
	h, st := newTestEnv(t)
	projID, colAID := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	colBID := seedColumnOnly(t, st, "p-a", "06", store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	firstID, secondID := ambiguousFixture(t, st, projID, colAID, colBID, "executor-B")

	// 消费视角位点前置（D7 不回看：位点须建在消息落库前）+空箱前提钉死；
	// poll 四头身份名避开目标会话名（防 upsert 混入第三行同名实体扰夹具）。
	pollA := pollQuery("05", "executor", "executor-B")
	pollB := pollQuery("06", "executor", "executor-B")
	headsA := []string{"p-a", "05", "watcher-A", "executor"}
	headsB := []string{"p-a", "06", "watcher-B", "executor"}
	if msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, pollA, "", headsA...), http.StatusOK)); len(msgs) != 0 {
		t.Fatalf("栏 A 视角建位点首轮 poll 条数 = %d，期望 0（空箱前提破）", len(msgs))
	}
	if msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, pollB, "", headsB...), http.StatusOK)); len(msgs) != 0 {
		t.Fatalf("栏 B 视角建位点首轮 poll 条数 = %d，期望 0（空箱前提破）", len(msgs))
	}

	// 发送方身份声明在栏 B（--column=06 偏好锚）：chat 到同名会话，201（偏好
	// 命中不改成功面）。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"chat","session":"executor-B"},"level":"normal","body":"栏位优先点名"}`,
		"p-a", "06", "controller-A", "controller")
	seq := seqFromData(t, wantData(t, rr, http.StatusCreated))

	// 落库强断言：target_session_id/column_id 必须落在发送方栏 B 行（修复前落
	// 最早行 A 行 id → 失败=红）。
	m := findMessageBySeq(t, st, seq)
	if m.TargetSessionID != secondID {
		t.Errorf("落库 target_session_id = %d，期望发送方栏 B 行会话 id %d（栏位优先：--column 偏好锚命中，修复前静默落最早行 id=%d）",
			m.TargetSessionID, secondID, firstID)
	}
	if m.ColumnID != colBID {
		t.Errorf("落库 column_id = %d，期望栏 B（06）id %d（chat=目标会话所属栏目）", m.ColumnID, colBID)
	}

	// 栏 A 视角 poll 拉不到（对话分支 target_session_id=栏 B 行 id 不命中；修复
	// 前消息落 A 栏行、A 视角能拉到 → 失败=红，本断言即病灶主证）。
	if msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, pollA, "", headsA...), http.StatusOK)); len(msgs) != 0 {
		t.Errorf("栏 A 视角 poll 条数 = %d，期望 0（消息应落发送方栏 B 行，A 栏对话分支不命中；修复前最早行胜出则误中）: %v", len(msgs), msgs)
	}
	// 栏 B 视角 poll 拉到且 seq 命中。
	msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, pollB, "", headsB...), http.StatusOK))
	if len(msgs) != 1 {
		t.Fatalf("栏 B 视角 poll 条数 = %d，期望 1（发送方栏偏好命中）: %v", len(msgs), msgs)
	}
	if got := msgs[0].(map[string]any)["seq"]; got != float64(seq) {
		t.Errorf("栏 B 视角 poll seq = %v，期望 %d", got, seq)
	}
}

// TestSendChatAmbiguousNoSenderColumnHit 锚 2（无偏好命中 → 409 候选清单）：
// 栏 05、06 各注册同名会话 executor-B；发送方在栏 07（无同名行）发 chat →
// 409 session_ambiguous，message 含候选清单两片段 executor-B@05、executor-B@06
// （名称@栏目 形态）。修复前 200 静默落最早行 → FAIL=红。
func TestSendChatAmbiguousNoSenderColumnHit(t *testing.T) {
	h, st := newTestEnv(t)
	projID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	seedColumnOnly(t, st, "p-a", "06", store.ColumnStatusActive)
	seedColumnOnly(t, st, "p-a", "07", store.ColumnStatusActive) // 偏好锚栏（无同名行）
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	ambiguousFixture(t, st, projID, col05, mustColumnID(t, st, "p-a", "06"), "executor-B")

	// 发送方 --column=07（偏好锚无命中）且全项目同名多行 → 409 显式歧义。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"chat","session":"executor-B"},"level":"normal","body":"歧义点名"}`,
		"p-a", "07", "controller-C", "controller")
	wantErrBody(t, rr, http.StatusConflict, "session_ambiguous")
	msg := errMsg(t, rr)
	for _, frag := range []string{"executor-B@05", "executor-B@06"} {
		if !strings.Contains(msg, frag) {
			t.Errorf("错误信息 %q 不含候选清单片段 %q（名称@栏目 形态）", msg, frag)
		}
	}
	// 拒绝不落库。
	if got := countTable(t, st, "messages"); got != 0 {
		t.Errorf("session_ambiguous 拒绝后 messages 行数 = %d，期望 0（拒绝不落库）", got)
	}
}

// TestSendChatAmbiguousCrossProject 锚 3（跨项目变体同构）：发送方在 p-a/05，
// target.project=p-b，p-b 内栏 07（先注册）、08 各注册同名会话 executor-B →
// 发送方栏不在目标项目域、无偏好锚 → 同名多行直接 409 session_ambiguous +
// 候选清单（executor-B@07、executor-B@08）。修复前 200 静默落最早行 → FAIL=红。
func TestSendChatAmbiguousCrossProject(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	projB, col07 := seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "07", store.ColumnStatusActive)
	seedColumnOnly(t, st, "p-b", "08", store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	ambiguousFixture(t, st, projB, col07, mustColumnID(t, st, "p-b", "08"), "executor-B")

	// 跨项目 chat（无偏好锚形态）：同名多行 → 409 显式歧义+候选清单。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"chat","project":"p-b","session":"executor-B"},"level":"normal","body":"跨项目点名"}`,
		heads4()...)
	wantErrBody(t, rr, http.StatusConflict, "session_ambiguous")
	msg := errMsg(t, rr)
	for _, frag := range []string{"executor-B@07", "executor-B@08"} {
		if !strings.Contains(msg, frag) {
			t.Errorf("错误信息 %q 不含候选清单片段 %q（名称@栏目 形态）", msg, frag)
		}
	}
	// 拒绝不落库。
	if got := countTable(t, st, "messages"); got != 0 {
		t.Errorf("跨项目 session_ambiguous 拒绝后 messages 行数 = %d，期望 0（拒绝不落库）", got)
	}
}

// TestPollSessionAmbiguousNoColumnHit 锚 4（消费视角同口径显式报错）：栏 05、06
// 各注册同名会话 executor-B；poll query column=07（存在但无同名行）&session=同名
// → 409 session_ambiguous（消费视角与 send 同口径显式报错，不再静默取最早行）。
// 修复前 200 空箱静默取最早行 → FAIL=红。
func TestPollSessionAmbiguousNoColumnHit(t *testing.T) {
	h, st := newTestEnv(t)
	projID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	seedColumnOnly(t, st, "p-a", "06", store.ColumnStatusActive)
	seedColumnOnly(t, st, "p-a", "07", store.ColumnStatusActive) // 消费视角栏目（无同名行）
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	ambiguousFixture(t, st, projID, col05, mustColumnID(t, st, "p-a", "06"), "executor-B")

	// 消费视角 poll：query column=07 偏好锚无命中且全项目同名多行 → 409。
	rr := doAuthedReq(t, h, http.MethodGet, pollQuery("07", "executor", "executor-B"), "", heads4()...)
	wantErrBody(t, rr, http.StatusConflict, "session_ambiguous")
	msg := errMsg(t, rr)
	for _, frag := range []string{"executor-B@05", "executor-B@06"} {
		if !strings.Contains(msg, frag) {
			t.Errorf("错误信息 %q 不含候选清单片段 %q（名称@栏目 形态）", msg, frag)
		}
	}
}

// TestSendChatSingleRowNegativeAnchor 锚 5（单行负面锚，修复前后均须 PASS）：
// 单行会话 executor-B 注册在栏 05、栏 06 无同名行。①发送方在栏 06（偏好锚无
// 同名命中）发 chat → 全项目单行回落命中，201 正常（单行场景响应与文案逐字节
// 不变）；②消费视角 poll query column=06（唯一行不在消费栏）→ 回落命中单行，
// 200 拉到消息——防「栏位优先」过收敛把栏外单行误伤为 4xx。
func TestSendChatSingleRowNegativeAnchor(t *testing.T) {
	h, st := newTestEnv(t)
	projID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	seedColumnOnly(t, st, "p-a", "06", store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	upB, err := st.UpsertSession(projID, col05, "executor-B", "executor")
	if err != nil {
		t.Fatalf("预注册单行目标会话失败: %v", err)
	}

	// ② 前置：消费视角位点（query column=06，唯一行不在消费栏——修复前后均应
	// 200 空箱；先建位点钉死 D7 不回看前提）。四头身份名避开目标会话名。
	pollURL := pollQuery("06", "executor", "executor-B")
	heads := []string{"p-a", "06", "watcher-A", "executor"}
	if msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, pollURL, "", heads...), http.StatusOK)); len(msgs) != 0 {
		t.Fatalf("建位点首轮 poll 条数 = %d，期望 0（空箱前提破）", len(msgs))
	}

	// ① 发送方在栏 06（偏好锚无同名命中）但全项目单行 → 回落命中，201 正常。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"chat","session":"executor-B"},"level":"normal","body":"单行回落点名"}`,
		"p-a", "06", "controller-A", "controller")
	seq := seqFromData(t, wantData(t, rr, http.StatusCreated))
	m := findMessageBySeq(t, st, seq)
	if m.TargetSessionID != upB.Session.ID {
		t.Errorf("落库 target_session_id = %d，期望单行会话 id %d（偏好栏无命中回落单行）", m.TargetSessionID, upB.Session.ID)
	}

	// ② 消费视角 poll query column=06（唯一行不在消费栏 06）→ 回落命中单行，
	// 200 拉到该消息。
	msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, pollURL, "", heads...), http.StatusOK))
	if len(msgs) != 1 {
		t.Fatalf("消费视角 poll 条数 = %d，期望 1（单行回落命中）: %v", len(msgs), msgs)
	}
	if got := msgs[0].(map[string]any)["seq"]; got != float64(seq) {
		t.Errorf("消费视角 poll seq = %v，期望 %d", got, seq)
	}
}

// TestSessionAmbiguousMessageWording 绿笔裁量补充（b2-T1 绿笔落码，五锚之外的
// 新增测试函数不属五锚断言面）：409 session_ambiguous 三条文案全句逐字节锚——
// 红笔锚 2/3/4 只断言候选清单片段（executor-B@05 形态），本函数锁定文案全句
// （风格先例=TestSendDirectAgentConflictClearingHint 逐字节锚定），防后续措辞
// 微调无锚漂移。三场景=锚 2（项目内 chat）/锚 3（跨项目 chat）/锚 4（消费视角
// poll）的最小复刻，夹具同构。
func TestSessionAmbiguousMessageWording(t *testing.T) {
	t.Run("项目内 chat", func(t *testing.T) {
		h, st := newTestEnv(t)
		projID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
		seedColumnOnly(t, st, "p-a", "06", store.ColumnStatusActive)
		seedColumnOnly(t, st, "p-a", "07", store.ColumnStatusActive) // 发送方栏（无同名行）
		injectFixedClock(t, "2026-01-01T08:00:00Z")
		ambiguousFixture(t, st, projID, col05, mustColumnID(t, st, "p-a", "06"), "executor-B")

		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
			`{"target":{"kind":"chat","session":"executor-B"},"level":"normal","body":"文案锚"}`,
			"p-a", "07", "controller-C", "controller")
		wantErrBody(t, rr, http.StatusConflict, "session_ambiguous")
		want := "目标会话 executor-B 在项目内存在 2 个同名会话（executor-B@05、executor-B@06），" +
			"且发送方栏目 07 内无同名行——请先治理同名会话行（或改用 direct --to-role 定向）"
		if got := errMsg(t, rr); got != want {
			t.Errorf("项目内歧义文案全句漂移:\n got: %s\nwant: %s", got, want)
		}
	})

	t.Run("跨项目 chat", func(t *testing.T) {
		h, st := newTestEnv(t)
		seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
		projB, col07 := seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "07", store.ColumnStatusActive)
		seedColumnOnly(t, st, "p-b", "08", store.ColumnStatusActive)
		injectFixedClock(t, "2026-01-01T08:00:00Z")
		ambiguousFixture(t, st, projB, col07, mustColumnID(t, st, "p-b", "08"), "executor-B")

		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
			`{"target":{"kind":"chat","project":"p-b","session":"executor-B"},"level":"normal","body":"文案锚"}`,
			heads4()...)
		wantErrBody(t, rr, http.StatusConflict, "session_ambiguous")
		want := "目标项目 p-b 内会话 executor-B 存在 2 个同名会话（executor-B@07、executor-B@08）" +
			"——请先治理同名会话行（或改用 direct 定向）"
		if got := errMsg(t, rr); got != want {
			t.Errorf("跨项目歧义文案全句漂移:\n got: %s\nwant: %s", got, want)
		}
	})

	t.Run("消费视角 poll", func(t *testing.T) {
		h, st := newTestEnv(t)
		projID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
		seedColumnOnly(t, st, "p-a", "06", store.ColumnStatusActive)
		seedColumnOnly(t, st, "p-a", "07", store.ColumnStatusActive) // 消费视角栏目（无同名行）
		injectFixedClock(t, "2026-01-01T08:00:00Z")
		ambiguousFixture(t, st, projID, col05, mustColumnID(t, st, "p-a", "06"), "executor-B")

		rr := doAuthedReq(t, h, http.MethodGet, pollQuery("07", "executor", "executor-B"), "", heads4()...)
		wantErrBody(t, rr, http.StatusConflict, "session_ambiguous")
		want := "消费视角会话 executor-B 存在 2 个同名会话（executor-B@05、executor-B@06），" +
			"且栏目 07 内无同名行——请指定确切栏目或治理同名会话行"
		if got := errMsg(t, rr); got != want {
			t.Errorf("消费视角歧义文案全句漂移:\n got: %s\nwant: %s", got, want)
		}
	})
}
