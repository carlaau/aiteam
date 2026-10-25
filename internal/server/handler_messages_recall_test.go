package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// b5-T2 recall 端点测试（b5-spec R3/AC1，M-36）：POST /api/v1/messages/{seq}/recall
// 校验链五闸（存在性 404→发送方 409→kind 白名单 409→block 拒 409→未消费判定 409，
// 序从简到繁早退）+心跳链（非豁免端点：四头必经心跳中间件 upsert+sessionIDFromCtx
// 归因）+审计 action 枚举（message.recall 每次照记）。

// recallURL 构造 recall 端点路径（seq 路径参数，b5-spec R3 原文）。
func recallURL(seq int64) string {
	return fmt.Sprintf("/api/v1/messages/%d/recall", seq)
}

// seedAckPosition 直插/覆盖位点行（upsert 形态，测试夹具面）：已消费判定的目标格
// 位点构造器——direct=consumer 角色、chat=DialogConsumer(目标会话)。探测语义
// （缺行=0=全量未消费）由「不种行」表达，本夹具只造已推进面。
func seedAckPosition(t *testing.T, st *store.Store, projectID, columnID int64, consumer string, position int64) {
	t.Helper()
	if _, err := st.DB.Exec(
		`INSERT INTO ack_positions (project_id, column_id, consumer, position, updated_at)
		 VALUES (?, ?, ?, ?, '2026-01-01T08:00:00Z')
		 ON CONFLICT(column_id, consumer) DO UPDATE SET position = excluded.position`,
		projectID, columnID, consumer, position,
	); err != nil {
		t.Fatalf("种位点行 column=%d consumer=%s 失败: %v", columnID, consumer, err)
	}
}

// recallTombstone 直查墓碑行 created_at（幂等不刷时间的权威证据源，T1 同款）。
func recallTombstone(t *testing.T, st *store.Store, seq int64) (createdAt string, found bool) {
	t.Helper()
	err := st.DB.QueryRow(
		`SELECT created_at FROM recalled_messages WHERE message_seq = ?`, seq,
	).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("查墓碑行 seq=%d 失败: %v", seq, err)
	}
	return createdAt, true
}

// recallTombstoneReason 直查墓碑行 reason（请求体 reason 透传落库断言面）。
func recallTombstoneReason(t *testing.T, st *store.Store, seq int64) string {
	t.Helper()
	var reason string
	if err := st.DB.QueryRow(
		`SELECT reason FROM recalled_messages WHERE message_seq = ?`, seq,
	).Scan(&reason); err != nil {
		t.Fatalf("查墓碑 reason seq=%d 失败: %v", seq, err)
	}
	return reason
}

// sendDirect 种一条 direct 消息（经 send 端点：发送方=controller-A@05、目标
// 05/<role>，返回 seq）——recall 测试的默认构造面。
func sendDirect(t *testing.T, h http.Handler, role, level, body string) int64 {
	t.Helper()
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		fmt.Sprintf(`{"target":{"kind":"direct","column":%q,"role":%q},"level":%q,"body":%q}`,
			authCol, role, level, body),
		heads4()...)
	return seqFromData(t, wantData(t, rr, http.StatusCreated))
}

// TestRecallDirectHappy happy path（b5-spec R3）：direct 消息未消费（目标格无
// 位点行）→ 200 data 恰 {seq,status:"recalled"} 两键；墓碑行落库（created_at=注入
// 时钟、reason 请求体透传）；审计 action=message.recall 恰一行且 SessionID=调用方
// 会话（sessionIDFromCtx 心跳链归因）、域锚=消息归属域（store 回填）。
func TestRecallDirectHappy(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	seq := sendDirect(t, h, "executor", store.MessageLevelNormal, "误发指令")
	rr := doAuthedReq(t, h, http.MethodPost, recallURL(seq), `{"reason":"发错指令"}`, heads4()...)
	data := wantData(t, rr, http.StatusOK)
	if len(data) != 2 {
		t.Errorf("data 字段面 = %v，期望恰 {seq,status}（b5-spec R3）", data)
	}
	if got := data["seq"]; got != float64(seq) {
		t.Errorf("data.seq = %v，期望 %d", got, seq)
	}
	if got := data["status"]; got != "recalled" {
		t.Errorf("data.status = %v，期望 recalled", got)
	}

	if at, found := recallTombstone(t, st, seq); !found || at != t0 {
		t.Errorf("墓碑行 = (created_at=%q found=%v)，期望 (%q, true)（服务端注入时钟）", at, found, t0)
	}
	if got := recallTombstoneReason(t, st, seq); got != "发错指令" {
		t.Errorf("墓碑 reason = %q，期望请求体透传「发错指令」", got)
	}

	// 审计面：action 枚举 message.recall 恰一行；归因=心跳链注入的调用方会话 id
	// （四头齐全走完整链的证据——非 0 系统动作）；域锚=消息归属域（store 回填）。
	sender := querySessionByName(t, st, authSess)
	if sender.ID <= 0 {
		t.Fatalf("调用方会话未 upsert（心跳链未走完整）")
	}
	logs := queryAuditByAction(t, st, store.AuditMessageRecall)
	if len(logs) != 1 {
		t.Fatalf("audit(message.recall) = %d 行，期望 1", len(logs))
	}
	if logs[0].SessionID != sender.ID {
		t.Errorf("审计 SessionID = %d，期望调用方会话 %d（sessionIDFromCtx 归因）", logs[0].SessionID, sender.ID)
	}
	if logs[0].ProjectID != projectID || logs[0].ColumnID != col05 {
		t.Errorf("审计域锚 = (project=%d column=%d)，期望 (%d, %d)（store 从消息归属域回填）",
			logs[0].ProjectID, logs[0].ColumnID, projectID, col05)
	}
}

// TestRecallChatHappy chat 形态撤回（b5-spec R1.3 chat 维度）：目标会话对话位点
// 行缺失=0=未消费可撤；位点=seq-1（严格小于）亦可撤——两形态锁定探测口径。
func TestRecallChatHappy(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	up, err := st.UpsertSession(projectID, col05, "executor-B", "executor")
	if err != nil {
		t.Fatalf("预注册目标会话失败: %v", err)
	}

	sendChat := func() int64 {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
			fmt.Sprintf(`{"target":{"kind":"chat","session":"executor-B"},"level":"normal","body":"误发对话"}`),
			heads4()...)
		return seqFromData(t, wantData(t, rr, http.StatusCreated))
	}

	// ①目标会话从未 poll/ack（对话位点行缺失）→ 未消费可撤。
	seq1 := sendChat()
	wantData(t, doAuthedReq(t, h, http.MethodPost, recallURL(seq1), "", heads4()...), http.StatusOK)

	// ②位点=seq-1（严格小于 seq）→ 仍未消费可撤（位点边界锁定）。
	seq2 := sendChat()
	seedAckPosition(t, st, projectID, col05, store.DialogConsumer(up.Session.ID), seq2-1)
	wantData(t, doAuthedReq(t, h, http.MethodPost, recallURL(seq2), "", heads4()...), http.StatusOK)
}

// TestRecallNotSender AC1①：非发送方撤回 → 409 not_sender（信息指明 seq+仅发送方
// 语义）；拒绝零墓碑。
func TestRecallNotSender(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	seq := sendDirect(t, h, "executor", store.MessageLevelNormal, "x")
	rr := doAuthedReq(t, h, http.MethodPost, recallURL(seq), "", authProj, authCol, "executor-B", "executor")
	wantErrBody(t, rr, http.StatusConflict, types.CodeNotSender)
	if msg := errMsg(t, rr); !strings.Contains(msg, fmt.Sprint(seq)) || !strings.Contains(msg, "仅发送方") {
		t.Errorf("错误信息 %q 未指明 seq=%d 与仅发送方语义", msg, seq)
	}
	if got := countTable(t, st, "recalled_messages"); got != 0 {
		t.Errorf("not_sender 拒绝后墓碑行数 = %d，期望 0（拒绝零落库）", got)
	}
}

// TestRecallKindRejected AC1②：bus/receipt 两 kind 拒 409 kind_not_recallable——
// 文案区分 bus（多格消费进度不一）/receipt（系统件）；拒绝零墓碑。
func TestRecallKindRejected(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	// bus：经 send 端点种（发送方=controller-A，发送方闸先行通过才落 kind 闸）。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"bus"},"level":"normal","body":"误广播"}`, heads4()...)
	busSeq := seqFromData(t, wantData(t, rr, http.StatusCreated))
	rr = doAuthedReq(t, h, http.MethodPost, recallURL(busSeq), "", heads4()...)
	wantErrBody(t, rr, http.StatusConflict, types.CodeKindNotRecallable)
	if msg := errMsg(t, rr); !strings.Contains(msg, "bus") || !strings.Contains(msg, "消费进度不一") {
		t.Errorf("bus 拒绝文案 %q 未区分 bus 语义（多格消费进度不一）", msg)
	}

	// receipt：send 端点不可达（系统件），直插构造（sender=controller-A 会话 id）。
	sender := querySessionByName(t, st, authSess)
	rtSeq := seedRawMessage(t, st, projectID, col05, store.MessageKindReceipt, "", 0,
		sender.ID, store.MessageLevelNormal, "回执回告")
	rr = doAuthedReq(t, h, http.MethodPost, recallURL(rtSeq), "", heads4()...)
	wantErrBody(t, rr, http.StatusConflict, types.CodeKindNotRecallable)
	if msg := errMsg(t, rr); !strings.Contains(msg, "receipt") {
		t.Errorf("receipt 拒绝文案 %q 未指明 receipt（系统件）", msg)
	}

	if got := countTable(t, st, "recalled_messages"); got != 0 {
		t.Errorf("kind 拒绝后墓碑行数 = %d，期望 0", got)
	}
}

// TestRecallBlockRejected AC1③：block 拒 409 block_not_recallable——即使未消费
// （无位点行）也拒（承诺语义文案说明）；拒绝零墓碑。
func TestRecallBlockRejected(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	seq := sendDirect(t, h, "executor", store.MessageLevelBlock, "阻断请求")
	rr := doAuthedReq(t, h, http.MethodPost, recallURL(seq), "", heads4()...)
	wantErrBody(t, rr, http.StatusConflict, types.CodeBlockNotRecallable)
	if msg := errMsg(t, rr); !strings.Contains(msg, "block") || !strings.Contains(msg, "未消费") {
		t.Errorf("block 拒绝文案 %q 未说明 block 承诺语义（即使未消费也拒）", msg)
	}
	if got := countTable(t, st, "recalled_messages"); got != 0 {
		t.Errorf("block 拒绝后墓碑行数 = %d，期望 0", got)
	}
}

// TestRecallAlreadyConsumed AC1④：目标格位点 ≥ seq（已消费）→ 409
// already_consumed，文案含 spec R1.3 原文「已消费不可撤，可追发更正」；direct
// （目标 role 格信箱位点）与 chat（目标会话对话位点）两维度各锚；位点恰 seq-1
// 的边界仍可撤（严格小于）。
func TestRecallAlreadyConsumed(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	up, err := st.UpsertSession(projectID, col05, "executor-B", "executor")
	if err != nil {
		t.Fatalf("预注册目标会话失败: %v", err)
	}

	// direct：目标格信箱位点=seq → 409 already_consumed 含更正指引原文。
	dSeq := sendDirect(t, h, "executor", store.MessageLevelNormal, "误发")
	seedAckPosition(t, st, projectID, col05, "executor", dSeq)
	rr := doAuthedReq(t, h, http.MethodPost, recallURL(dSeq), "", heads4()...)
	wantErrBody(t, rr, http.StatusConflict, types.CodeAlreadyConsumed)
	if msg := errMsg(t, rr); !strings.Contains(msg, "已消费不可撤，可追发更正") {
		t.Errorf("已消费文案 %q 缺 spec R1.3 原文「已消费不可撤，可追发更正」", msg)
	}

	// chat：目标会话对话位点=seq → 409 already_consumed。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"chat","session":"executor-B"},"level":"normal","body":"误发对话"}`, heads4()...)
	cSeq := seqFromData(t, wantData(t, rr, http.StatusCreated))
	seedAckPosition(t, st, projectID, col05, store.DialogConsumer(up.Session.ID), cSeq)
	rr = doAuthedReq(t, h, http.MethodPost, recallURL(cSeq), "", heads4()...)
	wantErrBody(t, rr, http.StatusConflict, types.CodeAlreadyConsumed)

	// 边界：位点恰 seq-1（严格小于=未消费）→ 200（严格不等式锁定）。
	eSeq := sendDirect(t, h, "executor", store.MessageLevelNormal, "边界")
	seedAckPosition(t, st, projectID, col05, "executor", eSeq-1)
	wantData(t, doAuthedReq(t, h, http.MethodPost, recallURL(eSeq), "", heads4()...), http.StatusOK)

	// 前两拒零墓碑（仅边界撤成功 1 行）。
	if got := countTable(t, st, "recalled_messages"); got != 1 {
		t.Errorf("已消费拒绝+边界成功后墓碑行数 = %d，期望 1（两拒绝零落库）", got)
	}
}

// TestRecallNotFound AC1⑤：不存在 seq → 404 message_not_found（信息指明 seq）；
// 路径参数非数字/非正整数 → 400 param_invalid（#12 receipt 同款路由参数面）。
func TestRecallNotFound(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	rr := doAuthedReq(t, h, http.MethodPost, recallURL(999), "", heads4()...)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeMessageNotFound)
	if msg := errMsg(t, rr); !strings.Contains(msg, "999") {
		t.Errorf("错误信息 %q 不含 seq 999（信息指明）", msg)
	}
	if got := countTable(t, st, "recalled_messages"); got != 0 {
		t.Errorf("404 后墓碑行数 = %d，期望 0", got)
	}

	for _, bad := range []string{"abc", "-1", "0"} {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages/"+bad+"/recall", "", heads4()...)
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	}
}

// TestRecallIdempotent AC1⑥：重复撤回幂等成功——两次均 200 {seq,status:"recalled"}
// 同形；墓碑 created_at 不刷（首撤时间保留）；审计每次照记（action=message.recall
// 两行——幂等成功≠零留痕，T1 口径在 handler 面复锚）。
func TestRecallIdempotent(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	seq := sendDirect(t, h, "executor", store.MessageLevelNormal, "待撤")
	data := wantData(t, doAuthedReq(t, h, http.MethodPost, recallURL(seq), "", heads4()...), http.StatusOK)
	if got := data["status"]; got != "recalled" {
		t.Errorf("首撤 data.status = %v，期望 recalled", got)
	}

	const t1 = "2026-01-01T09:00:00Z"
	injectFixedClock(t, t1) // 时钟推进：幂等不刷 created_at 的失败面
	data = wantData(t, doAuthedReq(t, h, http.MethodPost, recallURL(seq), "", heads4()...), http.StatusOK)
	if got := data["seq"]; got != float64(seq) {
		t.Errorf("重撤 data.seq = %v，期望 %d（同形响应）", got, seq)
	}
	if got := data["status"]; got != "recalled" {
		t.Errorf("重撤 data.status = %v，期望 recalled（幂等成功非报错）", got)
	}
	if at, found := recallTombstone(t, st, seq); !found || at != t0 {
		t.Errorf("重撤后墓碑 created_at = (%q, found=%v)，期望 (%q, true)（幂等不刷时间）", at, found, t0)
	}
	if logs := queryAuditByAction(t, st, store.AuditMessageRecall); len(logs) != 2 {
		t.Errorf("重撤后 audit(message.recall) = %d 行，期望 2（每次照记）", len(logs))
	}
}

// TestRecallHeartbeatChain 心跳链断言：recall 为非豁免端点——缺任一身份头 → 400
// missing_header（中间件先于 handler，非豁免证据）；四头齐全 → 完整链（会话
// upsert 在库+审计归因该会话 id 非 0）。
func TestRecallHeartbeatChain(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	// 缺四头：请求未达 handler 即 400 missing_header（走完整心跳链的证据）。
	rr := doReq(t, h, http.MethodPost, recallURL(1), nil)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeMissingHeader)

	// 四头齐全：中间件 upsert 调用方会话+注入 session id——撤回审计归因该行。
	seq := sendDirect(t, h, "executor", store.MessageLevelNormal, "x")
	wantData(t, doAuthedReq(t, h, http.MethodPost, recallURL(seq), "", heads4()...), http.StatusOK)
	sender := querySessionByName(t, st, authSess)
	logs := queryAuditByAction(t, st, store.AuditMessageRecall)
	if len(logs) != 1 || logs[0].SessionID != sender.ID || sender.ID == 0 {
		t.Errorf("审计归因 = (行数 %d, session=%d)，期望 1 行且=心跳链会话 %d（非 0 系统动作）",
			len(logs), logs[0].SessionID, sender.ID)
	}
}
