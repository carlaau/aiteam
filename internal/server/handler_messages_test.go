package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 消息域测试身份夹具（与登记域 authProj/authCol 同域：发送方挂在 p-a/05，
// 身份四头经心跳中间件 upsert——发送方会话由中间件隐式注册，无需预置）。
const (
	msgSendBody = `{"target":{"kind":"direct","column":"05","role":"executor"},"level":"block","body":"请确认方案"}`
)

// seedColumnOnly 向已有项目补插栏目夹具（返回栏目 id 供 column_id 归属断言；
// 照 middleware_test 直插 SQL 惯例，不依赖 CRUD）。
func seedColumnOnly(t *testing.T, st *store.Store, projCode, colCode, colStatus string) int64 {
	t.Helper()
	var projectID int64
	if err := st.DB.QueryRow(`SELECT id FROM projects WHERE code = ?`, projCode).Scan(&projectID); err != nil {
		t.Fatalf("查询项目夹具 %q 失败: %v", projCode, err)
	}
	const ts = "2026-01-01T00:00:00Z"
	res, err := st.DB.Exec(
		`INSERT INTO columns (project_id, code, name, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, colCode, "栏目"+colCode, colStatus, ts, ts,
	)
	if err != nil {
		t.Fatalf("插入栏目夹具 %q 失败: %v", colCode, err)
	}
	id, _ := res.LastInsertId()
	return id
}

// queryMessages 拉 projectCode 项目全部消息（走 store 导出数据面 QueryHistory，
// 空串=全库；单条 SELECT+LIMIT 50，本文件用例量均低于上限）。
func queryMessages(t *testing.T, st *store.Store, projectCode string) []store.Message {
	t.Helper()
	msgs, err := st.QueryHistory(store.HistoryFilter{ProjectCode: projectCode})
	if err != nil {
		t.Fatalf("查询消息失败: %v", err)
	}
	return msgs
}

// findMessageBySeq 按 seq 取消息行（落库逐字段断言用；未命中 Fatal）。
func findMessageBySeq(t *testing.T, st *store.Store, seq int64) store.Message {
	t.Helper()
	for _, m := range queryMessages(t, st, "") {
		if m.Seq == seq {
			return m
		}
	}
	t.Fatalf("seq=%d 消息未落库", seq)
	return store.Message{}
}

// seqFromData 从 201 响应 data 取 seq（安全断言：缺键/非数值走 Fatal 带字段
// 缺失信息，失败形态不劣化为 panic——B2-3 顺手加固）。
func seqFromData(t *testing.T, data map[string]any) int64 {
	t.Helper()
	f, ok := data["seq"].(float64)
	if !ok {
		t.Fatalf("data.seq 缺失或非数值: %v（201 响应须含服务端取号 seq）", data["seq"])
	}
	return int64(f)
}

// ---- #9 POST /api/v1/messages：direct 形态 ----

// TestSendDirect direct 形态：201 响应恰 {seq,created_at,level,kind} 四字段
// （§2.2 #9）；落库断言 kind 归属规则（§3.2 表 5 注：column_id=目标栏目、
// target_role=目标角色、target_session_id=0）+发送方会话与 label（AC5.1）。
func TestSendDirect(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	col06 := seedColumnOnly(t, st, "p-a", "06", store.ColumnStatusActive)
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	// 同栏目定向（05→05/executor）。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", msgSendBody,
		authProj, authCol, authSess, authRole)
	data := wantData(t, rr, http.StatusCreated)
	seq := seqFromData(t, data)
	if seq <= 0 {
		t.Errorf("data.seq = %d，期望 > 0（FR5 服务端取号）", seq)
	}
	if got := data["created_at"]; got != t0 {
		t.Errorf("data.created_at = %v，期望服务端注入时钟 %q（AC5.4）", got, t0)
	}
	if got := data["level"]; got != "block" {
		t.Errorf("data.level = %v，期望回显 block", got)
	}
	if got := data["kind"]; got != "direct" {
		t.Errorf("data.kind = %v，期望 direct", got)
	}
	if len(data) != 4 {
		t.Errorf("data 字段面 = %v，期望恰 {seq,created_at,level,kind}（§2.2 #9）", data)
	}

	// 落库逐字段断言（§3.2 表 5 注 direct 归属规则 + 发送方身份）。
	m := findMessageBySeq(t, st, seq)
	if m.Kind != store.MessageKindDirect {
		t.Errorf("落库 kind = %q，期望 direct", m.Kind)
	}
	if m.ColumnID != columnID {
		t.Errorf("落库 column_id = %d，期望目标栏目 05 的 id %d（direct=目标栏目）", m.ColumnID, columnID)
	}
	if m.TargetRole != "executor" {
		t.Errorf("落库 target_role = %q，期望 executor", m.TargetRole)
	}
	if m.TargetSessionID != 0 {
		t.Errorf("落库 target_session_id = %d，期望 0（direct 无目标会话）", m.TargetSessionID)
	}
	if m.ProjectID != projectID {
		t.Errorf("落库 project_id = %d，期望 %d", m.ProjectID, projectID)
	}
	sender := querySingleSession(t, st)
	if m.SenderSessionID != sender.ID {
		t.Errorf("落库 sender_session_id = %d，期望中间件 upsert 会话 id %d", m.SenderSessionID, sender.ID)
	}
	if m.SenderLabel != "controller-A@05" {
		t.Errorf("落库 sender_label = %q，期望 controller-A@05（§3.2 表 5 name@column 格式）", m.SenderLabel)
	}
	if m.Level != store.MessageLevelBlock {
		t.Errorf("落库 level = %q，期望 block（AC7.1 level 入库）", m.Level)
	}
	if m.Body != "请确认方案" {
		t.Errorf("落库 body = %q，期望原文入库", m.Body)
	}
	if m.CreatedAt != t0 {
		t.Errorf("落库 created_at = %q，期望注入时钟 %q（AC5.4）", m.CreatedAt, t0)
	}

	// 跨栏目定向（05 发送方 → 06 目标栏目）：column_id=目标栏目 06。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","column":"06","role":"executor_reviewer"},"level":"normal","body":"跨栏目"}`,
		authProj, authCol, authSess, authRole)
	data = wantData(t, rr, http.StatusCreated)
	m = findMessageBySeq(t, st, seqFromData(t, data))
	if m.ColumnID != col06 {
		t.Errorf("跨栏目落库 column_id = %d，期望目标栏目 06 的 id %d", m.ColumnID, col06)
	}
	if m.TargetRole != "executor_reviewer" {
		t.Errorf("跨栏目落库 target_role = %q，期望 reviewer", m.TargetRole)
	}
	if got := countTable(t, st, "messages"); got != 2 {
		t.Errorf("messages 行数 = %d，期望 2", got)
	}
}

// TestSendBus bus 形态：无目标参数，column_id=发送方所在栏目（审计锚点，
// §3.2 表 5 注），target_role/target_session_id 缺省。
func TestSendBus(t *testing.T) {
	h, st := newTestEnv(t)
	_, columnID := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"bus"},"level":"important","body":"总线广播"}`,
		authProj, authCol, authSess, authRole)
	data := wantData(t, rr, http.StatusCreated)
	if got := data["kind"]; got != "bus" {
		t.Errorf("data.kind = %v，期望 bus", got)
	}
	m := findMessageBySeq(t, st, seqFromData(t, data))
	if m.Kind != store.MessageKindBus {
		t.Errorf("落库 kind = %q，期望 bus", m.Kind)
	}
	if m.ColumnID != columnID {
		t.Errorf("落库 column_id = %d，期望发送方栏目 05 的 id %d（bus=发送方栏目锚点）", m.ColumnID, columnID)
	}
	if m.TargetRole != "" {
		t.Errorf("落库 target_role = %q，期望空串（bus 无目标角色）", m.TargetRole)
	}
	if m.TargetSessionID != 0 {
		t.Errorf("落库 target_session_id = %d，期望 0", m.TargetSessionID)
	}
}

// TestSendChat chat 形态：column_id=目标会话所属栏目、target_session_id=目标
// 会话 id（§3.2 表 5 注）；目标会话未注册 → 404 target_session_not_found。
func TestSendChat(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, _ := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	col06 := seedColumnOnly(t, st, "p-a", "06", store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	// 目标会话 executor-B 预注册到 06 栏目（sessions 行——chat 目标必须已注册）。
	up, err := st.UpsertSession(projectID, col06, "executor-B", "executor")
	if err != nil {
		t.Fatalf("预注册目标会话失败: %v", err)
	}

	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"chat","session":"executor-B"},"level":"normal","body":"看板回复"}`,
		authProj, authCol, authSess, authRole)
	data := wantData(t, rr, http.StatusCreated)
	if got := data["kind"]; got != "chat" {
		t.Errorf("data.kind = %v，期望 chat", got)
	}
	m := findMessageBySeq(t, st, seqFromData(t, data))
	if m.ColumnID != col06 {
		t.Errorf("落库 column_id = %d，期望目标会话栏目 06 的 id %d（chat=目标会话栏目）", m.ColumnID, col06)
	}
	if m.TargetSessionID != up.Session.ID {
		t.Errorf("落库 target_session_id = %d，期望目标会话 id %d", m.TargetSessionID, up.Session.ID)
	}
	if m.TargetRole != "" {
		t.Errorf("落库 target_role = %q，期望空串（chat 无目标角色）", m.TargetRole)
	}
	// from_project 空串 SQL 直查（b5-AC3 ③ 审计补位：列不在冻结读面
	// messageSelectColumns 内，findMessageBySeq 不带回，只能直查）。面 ③ 唯一
	// 缺口在此：handler FromProject 赋值仅 direct/chat 两分支的 crossProject
	// 守卫内（handler_messages_send.go ⑤ 前）——direct 分支守卫已有 SP1 退化态
	// 锚（TestSendCrossProjectDegenerate ①），chat 分支守卫拆解（赋值移出守卫）
	// 此前可逃逸全部测试；bus 分支无 FromProject 代码、store 层零值落行已由
	// TestMigrationsV3FromProject 锚。本断言给项目内 chat 行零落值留失败面（AC9.4）。
	if fp, _, _ := queryCrossProjectRow(t, st, seqFromData(t, data)); fp != "" {
		t.Errorf("项目内 chat 落行 from_project = %q，期望空串（FromProject 零值=项目内投递语义）", fp)
	}

	// 目标会话未注册 → 404 target_session_not_found，错误信息指明会话名；不落库。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"chat","session":"ghost"},"level":"normal","body":"x"}`,
		authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeTargetSessionNotFound)
	if msg := errMsg(t, rr); !strings.Contains(msg, "ghost") {
		t.Errorf("错误信息 %q 不含目标会话名 ghost（AC2.2 指明）", msg)
	}
	if got := countTable(t, st, "messages"); got != 1 {
		t.Errorf("target_session_not_found 拒绝后 messages 行数 = %d，期望仍 1（拒绝不落库）", got)
	}
}

// TestSendValidation 请求面校验（§2.2 #9/§2.4）：direct 目标 role 空白 → 400
// role_invalid；body 空/纯空白 → 400 body_empty；正文 256KB 边界（恰上限 201、
// 超 1 字节 413 body_too_large）；level 非法/target.kind 非法 → 400 param_invalid。
// 全部无效请求不落库。
func TestSendValidation(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	heads := []string{authProj, authCol, authSess, authRole}

	// direct 目标 role 空白/缺失 → 400 role_invalid。
	for _, body := range []string{
		`{"target":{"kind":"direct","column":"05","role":"   "},"level":"normal","body":"x"}`,
		`{"target":{"kind":"direct","column":"05"},"level":"normal","body":"x"}`,
	} {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", body, heads...)
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeRoleInvalid)
	}

	// body 空/纯空白 → 400 body_empty。
	for _, body := range []string{
		`{"target":{"kind":"direct","column":"05","role":"executor"},"level":"normal","body":""}`,
		`{"target":{"kind":"direct","column":"05","role":"executor"},"level":"normal","body":"  "}`,
		`{"target":{"kind":"bus"},"level":"normal"}`,
	} {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", body, heads...)
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeBodyEmpty)
	}

	// 256KB 边界：恰 256<<10 字节 → 201；超 1 字节 → 413 body_too_large。
	okBody := fmt.Sprintf(`{"target":{"kind":"bus"},"level":"normal","body":"%s"}`,
		strings.Repeat("a", maxMessageBodyBytes))
	if rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", okBody, heads...); rr.Code != http.StatusCreated {
		t.Errorf("恰 256KB 正文 status = %d，期望 201（≤256KB 合法；body: %s）", rr.Code, rr.Body.String())
	}
	bigBody := fmt.Sprintf(`{"target":{"kind":"bus"},"level":"normal","body":"%s"}`,
		strings.Repeat("a", maxMessageBodyBytes+1))
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", bigBody, heads...)
	wantErrBody(t, rr, http.StatusRequestEntityTooLarge, types.CodeBodyTooLarge)

	// level 非法 → 400 param_invalid（§2.4 400 组；handler 前置白名单）。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"bus"},"level":"urgent","body":"x"}`, heads...)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)

	// target.kind 非法/缺失 → 400 param_invalid（三形态解析失败=参数问题）。
	for _, body := range []string{
		`{"target":{"kind":"broadcast"},"level":"normal","body":"x"}`,
		`{"level":"normal","body":"x"}`,
		`{"target":{},"level":"normal","body":"x"}`,
	} {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", body, heads...)
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	}

	// 全部无效请求不落库（恰 256KB 那条合法落库 = 1）。
	if got := countTable(t, st, "messages"); got != 1 {
		t.Errorf("校验用例后 messages 行数 = %d，期望 1（仅恰 256KB 合法请求落库）", got)
	}
}

// TestSendArchivedRejected archived 域拒绝（AC1.3/AC2.3 服务端判据，B2-T2 落点
// =send 动作端点判 409，中间件只挡不存在）：发送方项目 archived → 409
// project_archived（项目优先于栏目）；发送方栏目 archived → 409 column_archived；
// direct 目标栏目 archived → 409 column_archived；未登记目标栏目 → 404
// column_not_found（AC2.2，存在性优先于 archived 语义）。拒绝不落库。
func TestSendArchivedRejected(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive) // 正常域
	seedProjectColumn(t, st, "p-arch", store.ProjectStatusArchived, "01", store.ColumnStatusActive)
	seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "03", store.ColumnStatusActive)
	seedColumnOnly(t, st, "p-b", "02", store.ColumnStatusArchived)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	busBody := `{"target":{"kind":"bus"},"level":"normal","body":"x"}`

	// 发送方项目 archived → 409 project_archived（direct 场景同锚：项目判定
	// 先于目标域查询——目标栏目 01 active 也不放行）。
	for _, body := range []string{busBody,
		`{"target":{"kind":"direct","column":"01","role":"executor"},"level":"normal","body":"x"}`} {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", body, "p-arch", "01", "s-arch", "executor")
		wantErrBody(t, rr, http.StatusConflict, types.CodeProjectArchived)
		if msg := errMsg(t, rr); !strings.Contains(msg, "p-arch") {
			t.Errorf("错误信息 %q 不含项目 code p-arch", msg)
		}
	}

	// 发送方栏目 archived（项目 active）→ 409 column_archived。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", busBody, "p-b", "02", "s-02", "executor")
	wantErrBody(t, rr, http.StatusConflict, types.CodeColumnArchived)

	// direct 目标栏目 archived（发送方 p-b/03 active）→ 409 column_archived。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","column":"02","role":"executor"},"level":"normal","body":"x"}`,
		"p-b", "03", "s-03", "executor")
	wantErrBody(t, rr, http.StatusConflict, types.CodeColumnArchived)
	if msg := errMsg(t, rr); !strings.Contains(msg, "02") {
		t.Errorf("错误信息 %q 不含目标栏目 code 02", msg)
	}

	// direct 目标栏目未登记 → 404 column_not_found（AC2.2），信息指明栏目。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","column":"99","role":"executor"},"level":"normal","body":"x"}`,
		authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeColumnNotFound)
	if msg := errMsg(t, rr); !strings.Contains(msg, "99") {
		t.Errorf("错误信息 %q 不含目标栏目 code 99", msg)
	}

	// 全部拒绝不落库。
	if got := countTable(t, st, "messages"); got != 0 {
		t.Errorf("archived 拒绝后 messages 行数 = %d，期望 0（拒绝不落库）", got)
	}
}

// TestSendChatTargetColumnArchived chat 形态目标栏目 archived（B2-3 修复，AC2.3
// 与 direct 对齐：send 对 archived 栏目→409 column_archived）：目标会话已注册但
// 其所属栏目 archived → 409 column_archived、不落库——缺失校验时消息落进
// archived 栏目且 poll 可拉，与 direct 形态行为不对称。
func TestSendChatTargetColumnArchived(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, _ := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	colArch := seedColumnOnly(t, st, "p-a", "06", store.ColumnStatusArchived)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	// 目标会话预注册到 archived 栏目 06（心跳中间件 upsert 不判栏目状态——
	// b1-spec「中间件只挡不存在」，archived 栏目中的会话行真实存在）。
	if _, err := st.UpsertSession(projectID, colArch, "executor-B", "executor"); err != nil {
		t.Fatalf("预注册目标会话失败: %v", err)
	}

	// 发送方在 active 栏目 05 向 archived 栏目 06 中的会话发 chat → 409。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"chat","session":"executor-B"},"level":"normal","body":"x"}`,
		authProj, authCol, authSess, authRole)
	wantErrBody(t, rr, http.StatusConflict, types.CodeColumnArchived)
	if msg := errMsg(t, rr); !strings.Contains(msg, "06") {
		t.Errorf("错误信息 %q 不含目标栏目 code 06", msg)
	}
	if got := countTable(t, st, "messages"); got != 0 {
		t.Errorf("目标栏目 archived 拒绝后 messages 行数 = %d，期望 0（拒绝不落库）", got)
	}
}

// TestSendServerTime created_at=服务端注入时钟（AC5.4）：请求体伪造 created_at/
// seq 字段被忽略（请求结构无此字段，§2.2 #9 字段面），时钟推进后第二条随注入走。
func TestSendServerTime(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	const (
		t0 = "2026-01-01T08:00:00Z"
		t1 = "2026-01-01T09:00:00Z"
	)
	injectFixedClock(t, t0)

	// 请求体塞伪 created_at/seq——服务端忽略，以注入时钟/自增取号为准。
	forged := `{"target":{"kind":"bus"},"level":"normal","body":"x","created_at":"1999-01-01T00:00:00Z","seq":999}`
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", forged,
		authProj, authCol, authSess, authRole)
	data := wantData(t, rr, http.StatusCreated)
	if got := data["created_at"]; got != t0 {
		t.Errorf("data.created_at = %v，期望注入时钟 %q（与请求体无关，AC5.4）", got, t0)
	}
	if got, ok := data["seq"].(float64); !ok || got == 999 {
		t.Errorf("data.seq = %v，期望服务端自增取号（伪造 seq 被忽略）", data["seq"])
	}

	injectFixedClock(t, t1)
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", forged,
		authProj, authCol, authSess, authRole)
	data = wantData(t, rr, http.StatusCreated)
	if got := data["created_at"]; got != t1 {
		t.Errorf("第二条 data.created_at = %v，期望推进后的注入时钟 %q", got, t1)
	}

	// 落库两行 created_at 与响应一致（服务端时钟权威；QueryHistory 默认 desc
	// =最新在前，msgs[0] 为第二条）。
	msgs := queryMessages(t, st, authProj)
	if len(msgs) != 2 || msgs[0].CreatedAt != t1 || msgs[1].CreatedAt != t0 {
		t.Errorf("落库 created_at = (%q,%q)，期望 desc 序 (%q,%q)", msgs[0].CreatedAt, msgs[1].CreatedAt, t1, t0)
	}
}

// ---- 跨项目投递（b5-W2，FR9/AC9.1~AC9.3）----

// queryCrossProjectRow 按 seq 直查 messages 跨项目审计三列（from_project/
// project_id/column_id）——from_project 不在冻结读面 messageSelectColumns 内
// （spec §1.1-W1③：本批零读面消费方），store.Message 不带回，只能 SQL 直查
// （AC9.3 表断言专用）。
func queryCrossProjectRow(t *testing.T, st *store.Store, seq int64) (fromProject string, projectID, columnID int64) {
	t.Helper()
	err := st.DB.QueryRow(
		`SELECT from_project, project_id, column_id FROM messages WHERE seq = ?`, seq,
	).Scan(&fromProject, &projectID, &columnID)
	if err != nil {
		t.Fatalf("直查 seq=%d 跨项目三列失败: %v", seq, err)
	}
	return fromProject, projectID, columnID
}

// TestSendCrossProjectDirect AC9.1/AC9.3：项目 A（p-a/05）会话 target.project=p-b
// 跨项目 direct → 201；B 侧目标格（p-b/07/executor）poll 可见该条；SQL 直查三列
// 断言并入本用例——from_project=A code、project_id/column_id=B 域 id（AC9.3）。
// 失败态（三层 404/agent_conflict/archived/token/退化形态）归任务 3，此处只成功态。
func TestSendCrossProjectDirect(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	projB, colB07 := seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "07", store.ColumnStatusActive)
	// B 侧目标会话预注册（poll query session 须已注册；目标格单活=agent_conflict 放行）。
	if _, err := st.UpsertSession(projB, colB07, "executor-B", "executor"); err != nil {
		t.Fatalf("预注册 B 侧目标会话失败: %v", err)
	}
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	bHeads := []string{"p-b", "07", "executor-B", "executor"}
	bPoll := pollQuery("07", "executor", "executor-B")

	// 先 poll 建位点（空库 MAX=0——D7 不回看：位点须建在消息落库前才可见）；
	// 断言空箱（质量审建议：夹具破坏时位点会建到消息 seq 上，先钉死空箱前提
	// 免失败面引偏排查方向）。
	if msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, bPoll, "", bHeads...), http.StatusOK)); len(msgs) != 0 {
		t.Fatalf("建位点首轮 poll 条数 = %d，期望 0（空箱前提破——B 侧位点已建到消息之后？）", len(msgs))
	}

	// A 域身份 POST #9，target 含 project 键指向 B 域。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","project":"p-b","column":"07","role":"executor"},"level":"block","body":"跨项目指令"}`,
		heads4()...)
	data := wantData(t, rr, http.StatusCreated)
	seq := seqFromData(t, data)
	if got := data["kind"]; got != "direct" {
		t.Errorf("data.kind = %v，期望 direct 回显", got)
	}

	// B 侧目标会话 poll：恰 1 条且 seq 命中（AC9.1 投递可见）。
	msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, bPoll, "", bHeads...), http.StatusOK))
	if len(msgs) != 1 {
		t.Fatalf("B 侧 poll 条数 = %d，期望 1（跨项目 direct 命中 B 域目标格）: %v", len(msgs), msgs)
	}
	if got := msgs[0].(map[string]any)["seq"]; got != float64(seq) {
		t.Errorf("B 侧 poll seq = %v，期望 %d", got, seq)
	}

	// SQL 直查三列（AC9.3）：from_project=发送方项目 code、project_id/column_id=B 域 id。
	fromProj, pid, cid := queryCrossProjectRow(t, st, seq)
	if fromProj != authProj {
		t.Errorf("落行 from_project = %q，期望发送方项目 %q", fromProj, authProj)
	}
	if pid != projB || cid != colB07 {
		t.Errorf("落行 (project_id,column_id) = (%d,%d)，期望 B 域 (%d,%d)", pid, cid, projB, colB07)
	}
	// direct 归属规则照常（目标角色入行、无目标会话）。
	m := findMessageBySeq(t, st, seq)
	if m.TargetRole != "executor" || m.TargetSessionID != 0 {
		t.Errorf("落行 (target_role,target_session_id) = (%q,%d)，期望 (executor,0)", m.TargetRole, m.TargetSessionID)
	}
}

// TestSendCrossProjectChat AC9.1 chat 形态同款（target.session 按目标项目域解析）：
// A 域身份 target.project=p-b+session=executor-B → 201；B 侧 executor-B poll 对话
// 分支命中；三列断言=from_project=p-a、project_id/column_id=B 域 id（chat=目标
// 会话所属栏目）。
func TestSendCrossProjectChat(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	projB, colB07 := seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "07", store.ColumnStatusActive)
	up, err := st.UpsertSession(projB, colB07, "executor-B", "executor")
	if err != nil {
		t.Fatalf("预注册 B 侧目标会话失败: %v", err)
	}
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	bHeads := []string{"p-b", "07", "executor-B", "executor"}
	bPoll := pollQuery("07", "executor", "executor-B")

	// 先 poll 建位点（D7 不回看）+断言空箱（质量审建议，同 direct 用例口径）。
	if msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, bPoll, "", bHeads...), http.StatusOK)); len(msgs) != 0 {
		t.Fatalf("建位点首轮 poll 条数 = %d，期望 0（空箱前提破——B 侧位点已建到消息之后？）", len(msgs))
	}

	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"chat","project":"p-b","session":"executor-B"},"level":"normal","body":"跨项目点对点"}`,
		heads4()...)
	data := wantData(t, rr, http.StatusCreated)
	seq := seqFromData(t, data)
	if got := data["kind"]; got != "chat" {
		t.Errorf("data.kind = %v，期望 chat 回显", got)
	}

	// B 侧目标会话 poll（对话分支=target_session_id 命中）：恰 1 条且 seq 命中。
	msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, bPoll, "", bHeads...), http.StatusOK))
	if len(msgs) != 1 {
		t.Fatalf("B 侧 poll 条数 = %d，期望 1（跨项目 chat 命中 B 域目标会话）: %v", len(msgs), msgs)
	}
	if got := msgs[0].(map[string]any)["seq"]; got != float64(seq) {
		t.Errorf("B 侧 poll seq = %v，期望 %d", got, seq)
	}

	// SQL 直查三列（AC9.3）+chat 归属规则（target_session_id=目标会话 id）。
	fromProj, pid, cid := queryCrossProjectRow(t, st, seq)
	if fromProj != authProj {
		t.Errorf("落行 from_project = %q，期望发送方项目 %q", fromProj, authProj)
	}
	if pid != projB || cid != colB07 {
		t.Errorf("落行 (project_id,column_id) = (%d,%d)，期望 B 域 (%d,%d)", pid, cid, projB, colB07)
	}
	m := findMessageBySeq(t, st, seq)
	if m.TargetSessionID != up.Session.ID {
		t.Errorf("落行 target_session_id = %d，期望 B 域目标会话 id %d", m.TargetSessionID, up.Session.ID)
	}
}

// TestSendCrossProjectNotFound AC9.2 三层 404 分流（b5-W2 任务 3）：目标项目
// 不存在 → 404 project_not_found；目标项目存在无该栏目 → 404 column_not_found
// （信息含目标项目 code+栏目 code）；目标项目存在无该会话（chat）→ 404
// target_session_not_found（跨项目文案「目标项目 X 无会话 Y」含目标 code）。
// 三层拒绝均不落库。
func TestSendCrossProjectNotFound(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "07", store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	// 第一层：目标项目不存在（direct）→ 404 project_not_found，信息指明项目。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","project":"p-x","column":"07","role":"executor"},"level":"normal","body":"x"}`,
		heads4()...)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeProjectNotFound)
	if msg := errMsg(t, rr); !strings.Contains(msg, "p-x") {
		t.Errorf("错误信息 %q 不含目标项目 code p-x（AC2.2 指明）", msg)
	}

	// 第二层：目标项目存在无该栏目（direct）→ 404 column_not_found，信息分层
	// 格式同时含目标项目 code 与栏目 code（跨项目歧义指明域）。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","project":"p-b","column":"99","role":"executor"},"level":"normal","body":"x"}`,
		heads4()...)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeColumnNotFound)
	if msg := errMsg(t, rr); !strings.Contains(msg, "p-b") || !strings.Contains(msg, "99") {
		t.Errorf("错误信息 %q 不含目标项目 code p-b 或栏目 code 99", msg)
	}

	// 第三层：目标项目存在无该会话（chat）→ 404 target_session_not_found，跨项目
	// 文案补目标项目前缀（spec W2⑤：同 code 会话跨项目歧义时指明域）。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"chat","project":"p-b","session":"ghost"},"level":"normal","body":"x"}`,
		heads4()...)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeTargetSessionNotFound)
	if msg := errMsg(t, rr); !strings.Contains(msg, "p-b") || !strings.Contains(msg, "ghost") {
		t.Errorf("错误信息 %q 不含目标项目 code p-b 或会话名 ghost", msg)
	}

	// 三层拒绝均不落库。
	if got := countTable(t, st, "messages"); got != 0 {
		t.Errorf("三层 404 拒绝后 messages 行数 = %d，期望 0（拒绝不落库）", got)
	}
}

// doTokenAuthedReq 带身份四头+可选 Bearer token 的请求（token 域用例专用——
// 族内 doAuthedReq 不支持 Authorization 头；token 传空串=不带）。
func doTokenAuthedReq(t *testing.T, h http.Handler, method, target, body, token string, heads ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	keys := []string{types.HeaderAiteamProject, types.HeaderAiteamColumn, types.HeaderAiteamSession, types.HeaderAiteamRole}
	for i, k := range keys {
		if i < len(heads) && heads[i] != "" {
			req.Header.Set(k, heads[i])
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestSendCrossProjectTokenDenied AC9.5 token 域校验（tokenAuth 中间件在 mux
// 外层先于路由/心跳）：开态下跨项目 send 无 token → 401 auth_required（带齐
// 身份四头仍拒——token 判定先于会话链）；带对 token → 正常投递 201 且落 B 域
// +from_project（跨项目身份链完整走通，与关态行为一致）。
func TestSendCrossProjectTokenDenied(t *testing.T) {
	h, st := newTokenOnServer(t)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	projB, colB07 := seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "07", store.ColumnStatusActive)
	// B 侧目标格预注册单活会话（direct 目标格代表性形态；单活放行）。时钟注入
	// 先于 upsert（last_seen=判定时钟，差值 0——单边/双边活性谓词下都稳）。
	if _, err := st.UpsertSession(projB, colB07, "executor-B", "executor"); err != nil {
		t.Fatalf("预注册 B 侧目标会话失败: %v", err)
	}
	body := `{"target":{"kind":"direct","project":"p-b","column":"07","role":"executor"},"level":"normal","body":"跨项目指令"}`

	// 无 token → 401 auth_required（tokenAuth 先于心跳——四头带齐仍被 token 拒）。
	rr := doTokenAuthedReq(t, h, http.MethodPost, "/api/v1/messages", body, "", heads4()...)
	wantErrBody(t, rr, http.StatusUnauthorized, types.CodeAuthRequired)

	// 带对 token → 201 正常投递，三列断言=B 域+from_project=A code（AC9.3 复核）。
	rr = doTokenAuthedReq(t, h, http.MethodPost, "/api/v1/messages", body, testToken, heads4()...)
	data := wantData(t, rr, http.StatusCreated)
	fromProj, pid, cid := queryCrossProjectRow(t, st, seqFromData(t, data))
	if fromProj != authProj {
		t.Errorf("落行 from_project = %q，期望发送方项目 %q", fromProj, authProj)
	}
	if pid != projB || cid != colB07 {
		t.Errorf("落行 (project_id,column_id) = (%d,%d)，期望 B 域 (%d,%d)", pid, cid, projB, colB07)
	}
	// 401 拒绝不落库、对 token 恰 1 行。
	if got := countTable(t, st, "messages"); got != 1 {
		t.Errorf("token 用例后 messages 行数 = %d，期望 1（401 不落库、对 token 投递 1 行）", got)
	}
}

// TestSendCrossProjectDegenerate AC7 退化形态（b5-W2 任务 3）：① SP1——
// target.project=发送方自身 → 按项目内路径 201 且 from_project 落空串；② 跨项目
// direct 打目标域双活格 → 409 agent_conflict（信息带目标项目前缀——任务 2 质量
// 审留账：项目内文案不带项目域，跨项目须指明目标域）；③ 目标项目/栏目
// archived → 409 照旧（direct 与 chat 两解析路径各锚）。拒绝均不落库。
func TestSendCrossProjectDegenerate(t *testing.T) {
	h, st := newTestEnv(t)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	projA, colA := seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	projB, colB07 := seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "07", store.ColumnStatusActive)
	// B 侧双活格夹具（07/executor 两活会话 → agent_conflict 拦截面）。时钟注入
	// 先于 upsert（last_seen=判定时钟，差值 0——单边/双边活性谓词下都稳，质量审
	// 修复：原形态夹具先于时钟落 last_seen，隐性依赖 ListAliveSessionNames 单边
	// 谓词，改双边窗口会难定位地假红）。
	for _, name := range []string{"executor-B1", "executor-B2"} {
		if _, err := st.UpsertSession(projB, colB07, name, "executor"); err != nil {
			t.Fatalf("预注册 %s 失败: %v", name, err)
		}
	}
	// archived 目标域夹具：项目级 p-arch（栏目 01 active）、栏目级 p-b/08 archived。
	seedProjectColumn(t, st, "p-arch", store.ProjectStatusArchived, "01", store.ColumnStatusActive)
	seedColumnOnly(t, st, "p-b", "08", store.ColumnStatusArchived)

	// ① SP1 退化态：target.project=发送方自身（p-a）→ 项目内路径 201，from_project
	// 不落（SQL 直查空串）、落行全 A 域。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","project":"p-a","column":"05","role":"executor"},"level":"normal","body":"退化=项目内"}`,
		heads4()...)
	data := wantData(t, rr, http.StatusCreated)
	fromProj, pid, cid := queryCrossProjectRow(t, st, seqFromData(t, data))
	if fromProj != "" {
		t.Errorf("退化态落行 from_project = %q，期望空串（SP1：from_project 不落）", fromProj)
	}
	if pid != projA || cid != colA {
		t.Errorf("退化态落行 (project_id,column_id) = (%d,%d)，期望 A 域 (%d,%d)", pid, cid, projA, colA)
	}

	// ② 跨项目 direct 打 B 域双活格 → 409 agent_conflict：信息带目标项目前缀
	// 「目标项目 p-b 栏目 07/executor 格子…」+两活会话名+教学语。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","project":"p-b","column":"07","role":"executor"},"level":"normal","body":"x"}`,
		heads4()...)
	wantErrBody(t, rr, http.StatusConflict, types.CodeAgentConflict)
	msg := errMsg(t, rr)
	for _, want := range []string{"目标项目 p-b", "07/executor", "executor-B1", "executor-B2", "请个体化命名"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息 %q 不含 %q（跨项目 agent_conflict 指明目标域+列活会话名+教学语）", msg, want)
		}
	}

	// ③ 目标项目 archived（direct 解析路径）→ 409 project_archived 照旧。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","project":"p-arch","column":"01","role":"executor"},"level":"normal","body":"x"}`,
		heads4()...)
	wantErrBody(t, rr, http.StatusConflict, types.CodeProjectArchived)

	// ④ 目标栏目 archived（direct 解析路径）→ 409 column_archived 照旧。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","project":"p-b","column":"08","role":"executor"},"level":"normal","body":"x"}`,
		heads4()...)
	wantErrBody(t, rr, http.StatusConflict, types.CodeColumnArchived)

	// ⑤ chat 形态打 archived 目标项目（GetProjectByCode 解析路径）→ 409
	// project_archived（项目层先于会话解析——会话名随意不触 404）。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"chat","project":"p-arch","session":"anyone"},"level":"normal","body":"x"}`,
		heads4()...)
	wantErrBody(t, rr, http.StatusConflict, types.CodeProjectArchived)

	// ②~⑤ 拒绝均不落库（仅 ① 落 1 行）。
	if got := countTable(t, st, "messages"); got != 1 {
		t.Errorf("退化态用例后 messages 行数 = %d，期望 1（仅 SP1 退化投递落库，拒绝不落库）", got)
	}
}

// TestSendBusWithProjectRejected bus+project 同给 → 400 param_invalid（§2.4，
// b5-W2 拦点）：bus=发送方所在栏目广播语义，无目标项目域可锚——跨项目定向须
// direct/chat。指向他项目与指向自身两形态均拒（键存在即非法，与域无关）；不落库。
func TestSendBusWithProjectRejected(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	for _, proj := range []string{"p-b", authProj} {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
			fmt.Sprintf(`{"target":{"kind":"bus","project":%q},"level":"normal","body":"x"}`, proj),
			heads4()...)
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
		if msg := errMsg(t, rr); !strings.Contains(msg, "bus") {
			t.Errorf("project=%q 错误信息 %q 不含 bus（教学语指明形态约束）", proj, msg)
		}
	}
	if got := countTable(t, st, "messages"); got != 0 {
		t.Errorf("bus 拒绝后 messages 行数 = %d，期望 0（拒绝不落库）", got)
	}
}

// ---- #10 GET /api/v1/mailbox + #11 POST /api/v1/acks（B2-4）----

// queryAckPosition 查 (column, consumer) 位点行值（GET 零推进/ack 幂等零副作用的
// 查库断言面，position 与 updated_at 双值——幂等口径见 store 层 upsert CASE 注）。
// 行不存在 Fatal（调用方须保证先经 poll 惰性初始化或 ack 落行）。
func queryAckPosition(t *testing.T, st *store.Store, columnID int64, consumer string) (position int64, updatedAt string) {
	t.Helper()
	err := st.DB.QueryRow(
		`SELECT position, updated_at FROM ack_positions WHERE column_id = ? AND consumer = ?`,
		columnID, consumer,
	).Scan(&position, &updatedAt)
	if err != nil {
		t.Fatalf("查询位点 column=%d consumer=%s 失败: %v", columnID, consumer, err)
	}
	return position, updatedAt
}

// seedDirectMessage 直插一条 direct 消息夹具（TestPollLimit 501 条批量构造用：
// limit 用例关注面=handler limit 解析+LIMIT 追加，消息来源不经 send 端点 501 轮
// HTTP；seq=AUTOINCREMENT 取号，返回值供断言。sender_session_id=0=系统语义）。
func seedDirectMessage(t *testing.T, st *store.Store, projectID, columnID int64, role string) int64 {
	t.Helper()
	res, err := st.DB.Exec(
		`INSERT INTO messages (project_id, column_id, kind, target_role, target_session_id,
		       sender_session_id, sender_label, level, body, created_at)
		 VALUES (?, ?, 'direct', ?, 0, 0, 'system', 'normal', 'x', '2026-01-01T08:00:00Z')`,
		projectID, columnID, role,
	)
	if err != nil {
		t.Fatalf("直插消息夹具失败: %v", err)
	}
	seq, _ := res.LastInsertId()
	return seq
}

// pollQuery 构造 poll/ack 的消费视角 query（column/role/session 三参，§2.2 #10/#11
// 请求面同构；session 传空=省略该参数）。
func pollQuery(col, role, sess string) string {
	q := "/api/v1/mailbox?column=" + col + "&role=" + role
	if sess != "" {
		q += "&session=" + sess
	}
	return q
}

// assertMailboxData 断言 poll 响应 data 字段面（恰 {mailbox_position,dialog_position,
// messages} 三键，§2.2 #10）并返回消息数组。
func assertMailboxData(t *testing.T, data map[string]any) []any {
	t.Helper()
	if len(data) != 3 {
		t.Fatalf("data 字段面 = %v，期望恰 {mailbox_position,dialog_position,messages}（§2.2 #10）", data)
	}
	msgs, ok := data["messages"].([]any)
	if !ok {
		t.Fatalf("data.messages 缺失或非数组（null 防线破）: %v", data["messages"])
	}
	return msgs
}

// TestPollRoundtrip AC8.1/AC8.4：send×3→poll 返回 3 条 seq 升序+五要素齐（§2.2 #10
// 响应字段逐字段）→再 poll 仍同 3 条且查库位点前后不变（GET 零推进）。
func TestPollRoundtrip(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	// 消费方会话 executor-B 预注册（poll query session 须已注册，404 口径见 TestPollSessionParam）。
	if _, err := st.UpsertSession(projectID, columnID, "executor-B", "executor"); err != nil {
		t.Fatalf("预注册消费方会话失败: %v", err)
	}
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	heads := []string{authProj, authCol, authSess, authRole}
	pollURL := pollQuery("05", "executor", "executor-B")

	// 首轮 poll：空结果（messages=[] 非 null）+信箱位点惰性初始化=当时 MAX(seq)=0。
	data := wantData(t, doAuthedReq(t, h, http.MethodGet, pollURL, "", heads...), http.StatusOK)
	if got := data["mailbox_position"]; got != float64(0) {
		t.Errorf("空库 data.mailbox_position = %v，期望 0（惰性初始化=当时 MAX(seq)）", got)
	}
	if got := data["dialog_position"]; got != float64(0) {
		t.Errorf("data.dialog_position = %v，期望 0（对话位点行缺失按 0，B2-T3）", got)
	}
	if msgs := assertMailboxData(t, data); len(msgs) != 0 {
		t.Errorf("空库 messages = %v，期望空数组", msgs)
	}

	// send×3（direct→05/executor，发送方 controller-A@05）。
	bodies := []struct{ level, body string }{
		{"normal", "第一条"}, {"important", "第二条"}, {"block", "第三条"},
	}
	var seqs []int64
	for _, b := range bodies {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
			fmt.Sprintf(`{"target":{"kind":"direct","column":"05","role":"executor"},"level":%q,"body":%q}`, b.level, b.body),
			heads...)
		seqs = append(seqs, seqFromData(t, wantData(t, rr, http.StatusCreated)))
	}

	// 二轮 poll：3 条 seq 升序+五要素齐（§2.2 #10 响应字段名逐字段）。
	data = wantData(t, doAuthedReq(t, h, http.MethodGet, pollURL, "", heads...), http.StatusOK)
	if got := data["mailbox_position"]; got != float64(0) {
		t.Errorf("poll 后 data.mailbox_position = %v，期望 0（GET 零位点推进，AC8.1）", got)
	}
	msgs := assertMailboxData(t, data)
	if len(msgs) != 3 {
		t.Fatalf("messages 条数 = %d，期望 3", len(msgs))
	}
	for i, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("messages[%d] 非对象: %v", i, raw)
		}
		if len(m) != 6 {
			t.Errorf("messages[%d] 字段面 = %v，期望恰 {seq,kind,level,body,sender,created_at}（§2.2 #10）", i, m)
		}
		if got := m["seq"]; got != float64(seqs[i]) {
			t.Errorf("messages[%d].seq = %v，期望 %d（seq 升序，AC8.1）", i, got, seqs[i])
		}
		if got := m["kind"]; got != "direct" {
			t.Errorf("messages[%d].kind = %v，期望 direct", i, got)
		}
		if got := m["level"]; got != bodies[i].level {
			t.Errorf("messages[%d].level = %v，期望 %q", i, got, bodies[i].level)
		}
		if got := m["body"]; got != bodies[i].body {
			t.Errorf("messages[%d].body = %v，期望 %q", i, got, bodies[i].body)
		}
		if got := m["created_at"]; got != t0 {
			t.Errorf("messages[%d].created_at = %v，期望注入时钟 %q（AC5.4）", i, got, t0)
		}
		// sender 五要素之身份（AC8.4）：嵌套 {session,role,column} 恰三键。
		sender, ok := m["sender"].(map[string]any)
		if !ok {
			t.Fatalf("messages[%d].sender 缺失或非对象: %v", i, m["sender"])
		}
		if len(sender) != 3 {
			t.Errorf("messages[%d].sender 字段面 = %v，期望恰 {session,role,column}", i, sender)
		}
		if got := sender["session"]; got != authSess {
			t.Errorf("sender.session = %v，期望发送方会话 %q", got, authSess)
		}
		if got := sender["role"]; got != authRole {
			t.Errorf("sender.role = %v，期望发送方角色 %q", got, authRole)
		}
		if got := sender["column"]; got != authCol {
			t.Errorf("sender.column = %v，期望发送方栏目 %q", got, authCol)
		}
	}

	// 再 poll：同 3 条+查库位点前后不变（GET 零推进断言走真库 ack_positions 行）。
	posBefore, updBefore := queryAckPosition(t, st, columnID, "executor")
	data2 := wantData(t, doAuthedReq(t, h, http.MethodGet, pollURL, "", heads...), http.StatusOK)
	msgs2 := assertMailboxData(t, data2)
	if len(msgs2) != 3 {
		t.Fatalf("重复 poll messages 条数 = %d，期望 3（幂等重拉，AC8.1）", len(msgs2))
	}
	for i, raw := range msgs2 {
		m := raw.(map[string]any)
		if got := m["seq"]; got != float64(seqs[i]) {
			t.Errorf("重复 poll messages[%d].seq = %v，期望 %d（同批）", i, got, seqs[i])
		}
	}
	posAfter, updAfter := queryAckPosition(t, st, columnID, "executor")
	if posAfter != posBefore || updAfter != updBefore {
		t.Errorf("重复 poll 后位点 (%d,%q)，期望不变 (%d,%q)——GET 零推进（AC8.1）",
			posAfter, updAfter, posBefore, updBefore)
	}
	if posAfter != 0 {
		t.Errorf("位点值 = %d，期望 0（惰性初始化值未被 poll/ack 触碰）", posAfter)
	}
}

// TestPollAfterAck AC8.2/AC9.1：ack 后 poll 不再返回已确认消息；ack 显式 seq 双维度
// 同推（§2.2 #11 单 seq 字段语义——信箱与对话位点均推到该 seq）。
func TestPollAfterAck(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	if _, err := st.UpsertSession(projectID, columnID, "executor-B", "executor"); err != nil {
		t.Fatalf("预注册消费方会话失败: %v", err)
	}
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	heads := []string{authProj, authCol, authSess, authRole}
	pollURL := pollQuery("05", "executor", "executor-B")

	// 先 poll（位点惰性初始化=0，D7 不回看前提）→send×3→poll 拉到 3 条。
	doAuthedReq(t, h, http.MethodGet, pollURL, "", heads...)
	var seqs []int64
	for i := range 3 {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
			fmt.Sprintf(`{"target":{"kind":"direct","column":"05","role":"executor"},"level":"normal","body":"msg-%d"}`, i),
			heads...)
		seqs = append(seqs, seqFromData(t, wantData(t, rr, http.StatusCreated)))
	}
	msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, pollURL, "", heads...), http.StatusOK))
	if len(msgs) != 3 {
		t.Fatalf("ack 前 poll 条数 = %d，期望 3", len(msgs))
	}

	// ack 显式 seq=第 2 条：响应恰 {mailbox_position,dialog_position} 双位点=该 seq。
	ackURL := fmt.Sprintf("/api/v1/acks?column=05&role=executor&session=executor-B&seq=%d", seqs[1])
	data := wantData(t, doAuthedReq(t, h, http.MethodPost, ackURL, "", heads...), http.StatusOK)
	if len(data) != 2 {
		t.Fatalf("ack data 字段面 = %v，期望恰 {mailbox_position,dialog_position}（§2.2 #11）", data)
	}
	if got := data["mailbox_position"]; got != float64(seqs[1]) {
		t.Errorf("ack data.mailbox_position = %v，期望 %d", got, seqs[1])
	}
	if got := data["dialog_position"]; got != float64(seqs[1]) {
		t.Errorf("ack data.dialog_position = %v，期望 %d（单 seq 字段双维度同推）", got, seqs[1])
	}

	// ack 后 poll 只剩第 3 条（AC8.2/AC9.1：位点之后的未确认消息）。
	msgs = assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, pollURL, "", heads...), http.StatusOK))
	if len(msgs) != 1 {
		t.Fatalf("ack 后 poll 条数 = %d，期望 1", len(msgs))
	}
	if got := msgs[0].(map[string]any)["seq"]; got != float64(seqs[2]) {
		t.Errorf("ack 后剩余 seq = %v，期望 %d（位点之前两条已确认）", got, seqs[2])
	}
	if pos, _ := queryAckPosition(t, st, columnID, "executor"); pos != seqs[1] {
		t.Errorf("查库信箱位点 = %d，期望 %d", pos, seqs[1])
	}
}

// TestPollRoleIsolation AC8.3：位点按 column+consumer（角色）独立——executor ack
// 后 reviewer poll 照常拉到全部发给自己的消息，位点值互不波及。
func TestPollRoleIsolation(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	if _, err := st.UpsertSession(projectID, columnID, "executor-B", "executor"); err != nil {
		t.Fatalf("预注册 executor-B 失败: %v", err)
	}
	if _, err := st.UpsertSession(projectID, columnID, "reviewer-C", "executor_reviewer"); err != nil {
		t.Fatalf("预注册 reviewer-C 失败: %v", err)
	}
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	heads := []string{authProj, authCol, authSess, authRole}

	// 两角色先各 poll 一次（位点惰性初始化=0——D7 语义下先建位点再收消息）。
	doAuthedReq(t, h, http.MethodGet, pollQuery("05", "executor", "executor-B"), "", heads...)
	doAuthedReq(t, h, http.MethodGet, pollQuery("05", "executor_reviewer", "reviewer-C"), "", heads...)

	// 各发 2 条 direct（全局 seq 交错但各信箱按 target_role 各取各的）。
	var execSeqs, revSeqs []int64
	for i := range 2 {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
			fmt.Sprintf(`{"target":{"kind":"direct","column":"05","role":"executor"},"level":"normal","body":"e-%d"}`, i),
			heads...)
		execSeqs = append(execSeqs, seqFromData(t, wantData(t, rr, http.StatusCreated)))
		rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
			fmt.Sprintf(`{"target":{"kind":"direct","column":"05","role":"executor_reviewer"},"level":"normal","body":"r-%d"}`, i),
			heads...)
		revSeqs = append(revSeqs, seqFromData(t, wantData(t, rr, http.StatusCreated)))
	}

	// executor poll 拉 2 条→ack 第 1 条→只剩第 2 条。
	msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet,
		pollQuery("05", "executor", "executor-B"), "", heads...), http.StatusOK))
	if len(msgs) != 2 {
		t.Fatalf("executor poll 条数 = %d，期望 2", len(msgs))
	}
	ackURL := fmt.Sprintf("/api/v1/acks?column=05&role=executor&session=executor-B&seq=%d", execSeqs[0])
	wantData(t, doAuthedReq(t, h, http.MethodPost, ackURL, "", heads...), http.StatusOK)
	msgs = assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet,
		pollQuery("05", "executor", "executor-B"), "", heads...), http.StatusOK))
	if len(msgs) != 1 {
		t.Fatalf("executor ack 后 poll 条数 = %d，期望 1", len(msgs))
	}

	// reviewer poll 照常拉到发给自己的全部 2 条（AC8.3：A 的 ack 不影响 B 位点）。
	msgs = assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet,
		pollQuery("05", "executor_reviewer", "reviewer-C"), "", heads...), http.StatusOK))
	if len(msgs) != 2 {
		t.Fatalf("reviewer poll 条数 = %d，期望 2（executor 的 ack 不波及 reviewer 位点）", len(msgs))
	}
	for i, raw := range msgs {
		if got := raw.(map[string]any)["seq"]; got != float64(revSeqs[i]) {
			t.Errorf("reviewer messages[%d].seq = %v，期望 %d", i, got, revSeqs[i])
		}
	}
	// 查库双行位点互不波及。
	if pos, _ := queryAckPosition(t, st, columnID, "executor"); pos != execSeqs[0] {
		t.Errorf("executor 位点 = %d，期望 %d", pos, execSeqs[0])
	}
	if pos, _ := queryAckPosition(t, st, columnID, "executor_reviewer"); pos != 0 {
		t.Errorf("reviewer 位点 = %d，期望 0（reviewer 从未 ack）", pos)
	}
}

// TestPollLimit D8 limit 面：缺省 500（501 条只返回前 500 条且 seq 升序）/显式 0=
// 全量/负值与非数字→400 param_invalid；未登记 query 栏目→404 column_not_found
// （AC2.2，信息指明栏目——存在性优先于 archived，handler 域校验路径）。
func TestPollLimit(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	if _, err := st.UpsertSession(projectID, columnID, "executor-B", "executor"); err != nil {
		t.Fatalf("预注册消费方会话失败: %v", err)
	}
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	heads := []string{authProj, authCol, authSess, authRole}

	// 先 poll（位点惰性初始化=0）→直插 501 条。
	doAuthedReq(t, h, http.MethodGet, pollQuery("05", "executor", "executor-B"), "", heads...)
	for range 501 {
		seedDirectMessage(t, st, projectID, columnID, "executor")
	}

	// 缺省（无 limit 参数）→恰 500 条、seq 升序（D8 默认 500 条分页）。
	msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet,
		pollQuery("05", "executor", "executor-B"), "", heads...), http.StatusOK))
	if len(msgs) != 500 {
		t.Fatalf("缺省 poll 条数 = %d，期望 500（D8）", len(msgs))
	}
	for i := 1; i < len(msgs); i++ {
		prev := msgs[i-1].(map[string]any)["seq"].(float64)
		if got := msgs[i].(map[string]any)["seq"].(float64); got <= prev {
			t.Fatalf("seq 升序破：messages[%d]=%v ≤ messages[%d]=%v（AC8.1）", i, got, i-1, prev)
		}
	}

	// 显式 limit=0 → 全量 501 条（D8 显式 0 要全量）。
	msgs = assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet,
		pollQuery("05", "executor", "executor-B")+"&limit=0", "", heads...), http.StatusOK))
	if len(msgs) != 501 {
		t.Fatalf("limit=0 poll 条数 = %d，期望 501（全量）", len(msgs))
	}

	// 负值/非数字 → 400 param_invalid。
	for _, q := range []string{"&limit=-1", "&limit=abc"} {
		rr := doAuthedReq(t, h, http.MethodGet, pollQuery("05", "executor", "executor-B")+q, "", heads...)
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	}

	// 未登记 query 栏目 → 404 column_not_found（信息指明栏目 code）。
	rr := doAuthedReq(t, h, http.MethodGet, pollQuery("99", "executor", "executor-B"), "", heads...)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeColumnNotFound)
	if msg := errMsg(t, rr); !strings.Contains(msg, "99") {
		t.Errorf("错误信息 %q 不含栏目 code 99（AC2.2 指明）", msg)
	}

	// query session 已传但未注册 → 404 session_not_found（显式报错优于静默空
	// 对话——CLI 身份拼写错误不可被吞；信息指明会话名）。
	rr = doAuthedReq(t, h, http.MethodGet, pollQuery("05", "executor", "ghost"), "", heads...)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeSessionNotFound)
	if msg := errMsg(t, rr); !strings.Contains(msg, "ghost") {
		t.Errorf("错误信息 %q 不含会话名 ghost", msg)
	}

	// ack 的 session 缺失 → 400 param_invalid（AdvancePositions 需真实会话 id）。
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/acks?column=05&role=executor", "", heads...)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)

	// ack 的 seq 非法（非数字/负值）→ 400 param_invalid。
	for _, bad := range []string{"&seq=abc", "&seq=-1"} {
		rr = doAuthedReq(t, h, http.MethodPost,
			"/api/v1/acks?column=05&role=executor&session=executor-B"+bad, "", heads...)
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	}
}

// TestAckOmitSeq ack 省略 seq=推进到当前各自最大可见（§2.2 #11：一键处理完）——
// 信箱（direct+bus 口径）与对话（发给我的 chat/receipt、排除自发）两维度分别断言。
func TestAckOmitSeq(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	up, err := st.UpsertSession(projectID, columnID, "executor-B", "executor")
	if err != nil {
		t.Fatalf("预注册消费方会话失败: %v", err)
	}
	executorBID := up.Session.ID
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	heads := []string{authProj, authCol, authSess, authRole}
	pollURL := pollQuery("05", "executor", "executor-B")

	doAuthedReq(t, h, http.MethodGet, pollURL, "", heads...) // 位点惰性初始化=0

	// 信箱维度：direct→executor×2；对话维度：chat→executor-B×2（controller-A 发）。
	var directSeqs, chatSeqs []int64
	for i := range 2 {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
			fmt.Sprintf(`{"target":{"kind":"direct","column":"05","role":"executor"},"level":"normal","body":"d-%d"}`, i),
			heads...)
		directSeqs = append(directSeqs, seqFromData(t, wantData(t, rr, http.StatusCreated)))
		rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
			fmt.Sprintf(`{"target":{"kind":"chat","session":"executor-B"},"level":"normal","body":"c-%d"}`, i),
			heads...)
		chatSeqs = append(chatSeqs, seqFromData(t, wantData(t, rr, http.StatusCreated)))
	}

	// poll 拉到信箱 2 条+对话 2 条（谓词三分支并集）。
	msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, pollURL, "", heads...), http.StatusOK))
	if len(msgs) != 4 {
		t.Fatalf("poll 条数 = %d，期望 4（direct 2+chat 2）", len(msgs))
	}

	// 省略 seq ack：双位点=各自最大可见（信箱=direct 第 2 条；对话=chat 第 2 条）。
	data := wantData(t, doAuthedReq(t, h, http.MethodPost,
		"/api/v1/acks?column=05&role=executor&session=executor-B", "", heads...), http.StatusOK)
	if got := data["mailbox_position"]; got != float64(directSeqs[1]) {
		t.Errorf("mailbox_position = %v，期望信箱最大可见 %d", got, directSeqs[1])
	}
	if got := data["dialog_position"]; got != float64(chatSeqs[1]) {
		t.Errorf("dialog_position = %v，期望对话最大可见 %d", got, chatSeqs[1])
	}
	// 查库两维度行（信箱 consumer=角色名；对话 consumer=chat:session:<id>）。
	if pos, _ := queryAckPosition(t, st, columnID, "executor"); pos != directSeqs[1] {
		t.Errorf("信箱位点行 = %d，期望 %d", pos, directSeqs[1])
	}
	if pos, _ := queryAckPosition(t, st, columnID, store.DialogConsumer(executorBID)); pos != chatSeqs[1] {
		t.Errorf("对话位点行 = %d，期望 %d", pos, chatSeqs[1])
	}

	// 排除自发（§2.2 #24）：executor-B 自己发出的 chat 不进自己的对话口径——
	// 省略 ack 重放后对话位点不动。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"chat","session":"controller-A"},"level":"normal","body":"自发回复"}`,
		authProj, authCol, "executor-B", "executor")
	seqFromData(t, wantData(t, rr, http.StatusCreated))
	data = wantData(t, doAuthedReq(t, h, http.MethodPost,
		"/api/v1/acks?column=05&role=executor&session=executor-B", "", heads...), http.StatusOK)
	if got := data["dialog_position"]; got != float64(chatSeqs[1]) {
		t.Errorf("重放后 dialog_position = %v，期望仍 %d（自发消息不回流、不推进对话位点）", got, chatSeqs[1])
	}

	// 全推过后 poll 空。
	msgs = assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, pollURL, "", heads...), http.StatusOK))
	if len(msgs) != 0 {
		t.Errorf("省略 ack 后 poll 条数 = %d，期望 0（一键处理完）", len(msgs))
	}
}

// TestAckIdempotent AC9.2：重复 ack 同位点响应逐字段一致、查库 position 与
// updated_at 均不变（position=max 幂等、回退尝试无效果、省略 seq 重放同幂等）。
func TestAckIdempotent(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	up, err := st.UpsertSession(projectID, columnID, "executor-B", "executor")
	if err != nil {
		t.Fatalf("预注册消费方会话失败: %v", err)
	}
	dialogConsumer := store.DialogConsumer(up.Session.ID)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	heads := []string{authProj, authCol, authSess, authRole}
	pollURL := pollQuery("05", "executor", "executor-B")

	doAuthedReq(t, h, http.MethodGet, pollURL, "", heads...) // 位点惰性初始化=0
	var seqs []int64
	for i := range 2 {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
			fmt.Sprintf(`{"target":{"kind":"direct","column":"05","role":"executor"},"level":"normal","body":"m-%d"}`, i),
			heads...)
		seqs = append(seqs, seqFromData(t, wantData(t, rr, http.StatusCreated)))
	}

	ackURL := fmt.Sprintf("/api/v1/acks?column=05&role=executor&session=executor-B&seq=%d", seqs[1])
	first := wantData(t, doAuthedReq(t, h, http.MethodPost, ackURL, "", heads...), http.StatusOK)
	mailboxPos, mailboxUpd := queryAckPosition(t, st, columnID, "executor")
	dialogPos, dialogUpd := queryAckPosition(t, st, columnID, dialogConsumer)

	// 重复 ack 同位点：响应逐字段一致+库 position/updated_at 双不变（幂等零副作用）。
	replay := wantData(t, doAuthedReq(t, h, http.MethodPost, ackURL, "", heads...), http.StatusOK)
	if len(replay) != len(first) {
		t.Fatalf("重复 ack data 字段数 = %d，期望 %d", len(replay), len(first))
	}
	for k, v := range first {
		if replay[k] != v {
			t.Errorf("重复 ack data[%s] = %v，期望 %v（响应一致，AC9.2）", k, replay[k], v)
		}
	}
	if pos, upd := queryAckPosition(t, st, columnID, "executor"); pos != mailboxPos || upd != mailboxUpd {
		t.Errorf("重复 ack 后信箱位点 (%d,%q)，期望不变 (%d,%q)", pos, upd, mailboxPos, mailboxUpd)
	}
	if pos, upd := queryAckPosition(t, st, columnID, dialogConsumer); pos != dialogPos || upd != dialogUpd {
		t.Errorf("重复 ack 后对话位点 (%d,%q)，期望不变 (%d,%q)", pos, upd, dialogPos, dialogUpd)
	}

	// 回退尝试（更小 seq）无效果：position 取 max 不回退，响应仍同首轮。
	retreat := wantData(t, doAuthedReq(t, h, http.MethodPost,
		fmt.Sprintf("/api/v1/acks?column=05&role=executor&session=executor-B&seq=%d", seqs[0]), "", heads...), http.StatusOK)
	for k, v := range first {
		if retreat[k] != v {
			t.Errorf("回退 ack data[%s] = %v，期望 %v（position 取 max 不回退）", k, retreat[k], v)
		}
	}
	if pos, upd := queryAckPosition(t, st, columnID, "executor"); pos != mailboxPos || upd != mailboxUpd {
		t.Errorf("回退 ack 后信箱位点 (%d,%q)，期望不变 (%d,%q)", pos, upd, mailboxPos, mailboxUpd)
	}

	// 省略 seq 重放：目标=当前最大可见（不含位点条件）=已推值，max 幂等零副作用。
	omit := wantData(t, doAuthedReq(t, h, http.MethodPost,
		"/api/v1/acks?column=05&role=executor&session=executor-B", "", heads...), http.StatusOK)
	for k, v := range first {
		if omit[k] != v {
			t.Errorf("省略 ack data[%s] = %v，期望 %v（最大可见=已推位点，幂等）", k, omit[k], v)
		}
	}
	if pos, upd := queryAckPosition(t, st, columnID, "executor"); pos != mailboxPos || upd != mailboxUpd {
		t.Errorf("省略 ack 后信箱位点 (%d,%q)，期望不变 (%d,%q)", pos, upd, mailboxPos, mailboxUpd)
	}
}

// TestPollAckArchivedRejected archived 域拒绝（AC1.3/AC2.3 跨批复验，B2-T2
// 落点=动作端点判 409——poll 与 ack 同为动作端点，经 resolveMailboxDomain 共用
// rejectArchivedDomain 口径）：archived 项目 poll/ack→409 project_archived
// （项目优先于栏目）；active 项目 archived 栏目→409 column_archived。拒绝零
// 副作用（ack_positions 不落行）。
func TestPollAckArchivedRejected(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-arch", store.ProjectStatusArchived, "01", store.ColumnStatusActive)
	seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "03", store.ColumnStatusActive)
	seedColumnOnly(t, st, "p-b", "02", store.ColumnStatusArchived)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	// archived 项目（发送方四头域 p-arch）poll/ack → 409 project_archived。
	for _, tc := range []struct{ method, url string }{
		{http.MethodGet, pollQuery("01", "executor", "s-arch")},
		{http.MethodPost, "/api/v1/acks?column=01&role=executor&session=s-arch"},
	} {
		rr := doAuthedReq(t, h, tc.method, tc.url, "", "p-arch", "01", "s-arch", "executor")
		wantErrBody(t, rr, http.StatusConflict, types.CodeProjectArchived)
		if msg := errMsg(t, rr); !strings.Contains(msg, "p-arch") {
			t.Errorf("错误信息 %q 不含项目 code p-arch", msg)
		}
	}

	// active 项目 archived 栏目 poll/ack → 409 column_archived。
	for _, tc := range []struct{ method, url string }{
		{http.MethodGet, pollQuery("02", "executor", "s-02")},
		{http.MethodPost, "/api/v1/acks?column=02&role=executor&session=s-02"},
	} {
		rr := doAuthedReq(t, h, tc.method, tc.url, "", "p-b", "02", "s-02", "executor")
		wantErrBody(t, rr, http.StatusConflict, types.CodeColumnArchived)
	}

	// 拒绝零副作用：ack_positions 全程零行（poll 惰性初始化/ack upsert 均未触达）。
	if got := countTable(t, st, "ack_positions"); got != 0 {
		t.Errorf("archived 拒绝后 ack_positions 行数 = %d，期望 0（拒绝零副作用）", got)
	}
}

// ---- #12 POST /api/v1/messages/{seq}/receipt + #13 GET /api/v1/messages（B2-5）----

// querySessionByName 按会话名查 sessions 行（回告 target/column 归属断言面；
// 测试域内会话名唯一——多项目夹具时调用方保证不撞名）。
func querySessionByName(t *testing.T, st *store.Store, name string) store.Session {
	t.Helper()
	var se store.Session
	err := st.DB.QueryRow(
		`SELECT id, project_id, column_id, name, role, last_seen_at, created_at
		 FROM sessions WHERE name = ?`,
		name,
	).Scan(&se.ID, &se.ProjectID, &se.ColumnID, &se.Name, &se.Role, &se.LastSeenAt, &se.CreatedAt)
	if err != nil {
		t.Fatalf("查询会话 %q 失败: %v", name, err)
	}
	return se
}

// findReceiptNotify 查恰一条 kind=receipt 回告消息（§2.3 副作用链落库断言面；
// 条数≠1 Fatal——幂等不重复发回告的断言靠此约束）。
func findReceiptNotify(t *testing.T, st *store.Store) store.Message {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE kind = ?`, store.MessageKindReceipt,
	).Scan(&n); err != nil {
		t.Fatalf("统计回告消息失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("kind=receipt 回告消息条数 = %d，期望 1", n)
	}
	var m store.Message
	err := st.DB.QueryRow(
		`SELECT seq, project_id, column_id, kind, target_role, target_session_id,
		       sender_session_id, sender_label, level, body, created_at
		 FROM messages WHERE kind = ?`,
		store.MessageKindReceipt,
	).Scan(&m.Seq, &m.ProjectID, &m.ColumnID, &m.Kind, &m.TargetRole,
		&m.TargetSessionID, &m.SenderSessionID, &m.SenderLabel,
		&m.Level, &m.Body, &m.CreatedAt)
	if err != nil {
		t.Fatalf("查询回告消息失败: %v", err)
	}
	return m
}

// receiptURL 构造 #12 回执路径（seq 为路径参数，§2.2 #12 原文）。
func receiptURL(seq int64) string {
	return fmt.Sprintf("/api/v1/messages/%d/receipt", seq)
}

// seedRawMessage 直插任意 kind/level/sender 消息夹具（#13 过滤参数组合与
// archived 历史保留用例的构造面：send 端点只能种 direct/bus/chat 三形态且
// level 白名单，直插覆盖 receipt 形态与 sender_session_id=0 系统语义）。
func seedRawMessage(t *testing.T, st *store.Store, projectID, columnID int64, kind, role string, targetSessionID, senderSessionID int64, level, body string) int64 {
	t.Helper()
	res, err := st.DB.Exec(
		`INSERT INTO messages (project_id, column_id, kind, target_role, target_session_id,
		       sender_session_id, sender_label, level, body, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '2026-01-01T08:00:00Z')`,
		projectID, columnID, kind, role, targetSessionID, senderSessionID,
		"seed", level, body,
	)
	if err != nil {
		t.Fatalf("直插消息夹具失败: %v", err)
	}
	seq, _ := res.LastInsertId()
	return seq
}

// TestReceiptHappy block 消息回执 happy path（§2.2 #12 + §2.3 副作用链）：
// 响应恰 {seq,receipt_at,receipt_by} 三键；同事务副作用查库断言——message_receipts
// 行+kind=receipt 回告消息（target=原发送方会话、column_id=原发送方会话栏目、
// level=normal、body 照 §2.3 格式、sender_label=system）都在；原发送方 poll 对话
// 分支拉到回告。发送方 05 发 direct 到 06（跨栏目）：回告 column_id=原发送方会话
// 栏目 05 而非原消息栏目 06，验证 §3.2 表 5 注 receipt 归属规则。
func TestReceiptHappy(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	seedColumnOnly(t, st, "p-a", "06", store.ColumnStatusActive)
	// 回执方 executor-B 注册到 05（其身份四头经中间件也可隐式注册，预注册使
	// 会话 id 断言可直查）。
	up, err := st.UpsertSession(projectID, col05, "executor-B", "executor")
	if err != nil {
		t.Fatalf("预注册回执方会话失败: %v", err)
	}
	const (
		t0 = "2026-01-01T08:00:00Z" // send 时刻
		t1 = "2026-01-01T09:00:00Z" // receipt 时刻（注入时钟推进）
	)
	injectFixedClock(t, t0)

	// 发送方 controller-A@05 发 block direct 到 06/reviewer。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","column":"06","role":"executor_reviewer"},"level":"block","body":"阻断请求"}`,
		authProj, authCol, authSess, authRole)
	seq := seqFromData(t, wantData(t, rr, http.StatusCreated))

	injectFixedClock(t, t1)
	// 回执方 executor-B 四头回执。
	rr = doAuthedReq(t, h, http.MethodPost, receiptURL(seq), "", authProj, authCol, "executor-B", "executor")
	data := wantData(t, rr, http.StatusOK)
	if len(data) != 3 {
		t.Fatalf("data 字段面 = %v，期望恰 {seq,receipt_at,receipt_by}（§2.2 #12）", data)
	}
	if got := data["seq"]; got != float64(seq) {
		t.Errorf("data.seq = %v，期望 %d", got, seq)
	}
	if got := data["receipt_at"]; got != t1 {
		t.Errorf("data.receipt_at = %v，期望服务端注入时钟 %q（AC7.3）", got, t1)
	}
	if got := data["receipt_by"]; got != "executor-B" {
		t.Errorf("data.receipt_by = %v，期望回执方会话名 executor-B（AC7.3）", got)
	}

	// 同事务副作用查库断言：message_receipts 恰 1 行（回执方会话 id+服务端时间）。
	if got := countTable(t, st, "message_receipts"); got != 1 {
		t.Fatalf("message_receipts 行数 = %d，期望 1", got)
	}
	var (
		recSeq int64
		recBy  int64
		recAt  string
	)
	if err := st.DB.QueryRow(
		`SELECT message_seq, receipt_session_id, created_at FROM message_receipts`,
	).Scan(&recSeq, &recBy, &recAt); err != nil {
		t.Fatalf("查询回执行失败: %v", err)
	}
	if recSeq != seq {
		t.Errorf("回执 message_seq = %d，期望 %d", recSeq, seq)
	}
	if recBy != up.Session.ID {
		t.Errorf("回执 receipt_session_id = %d，期望回执方会话 id %d（AC7.3）", recBy, up.Session.ID)
	}
	if recAt != t1 {
		t.Errorf("回执 created_at = %q，期望注入时钟 %q", recAt, t1)
	}

	// 回告消息逐字段断言（§2.3 副作用链+§3.2 表 5 注 receipt 归属规则）。
	sender := querySessionByName(t, st, authSess) // 原发送方会话（中间件注册 controller-A@05）
	notify := findReceiptNotify(t, st)
	if notify.TargetSessionID != sender.ID {
		t.Errorf("回告 target_session_id = %d，期望原发送方会话 id %d（§2.3）", notify.TargetSessionID, sender.ID)
	}
	if notify.ColumnID != col05 {
		t.Errorf("回告 column_id = %d，期望原发送方会话栏目 05 的 id %d（§3.2 表 5 注；原消息栏目为 06，验证非原消息栏目）",
			notify.ColumnID, col05)
	}
	if notify.ProjectID != projectID {
		t.Errorf("回告 project_id = %d，期望 %d", notify.ProjectID, projectID)
	}
	if notify.Level != store.MessageLevelNormal {
		t.Errorf("回告 level = %q，期望 normal（§2.3）", notify.Level)
	}
	if notify.TargetRole != "" {
		t.Errorf("回告 target_role = %q，期望空串（receipt 无目标角色）", notify.TargetRole)
	}
	if notify.SenderSessionID != 0 {
		t.Errorf("回告 sender_session_id = %d，期望 0（系统自动生成，§3.2 表 5 弱关联语义）", notify.SenderSessionID)
	}
	if notify.SenderLabel != "system" {
		t.Errorf("回告 sender_label = %q，期望 system（b2-spec §二 #12 行）", notify.SenderLabel)
	}
	wantBody := fmt.Sprintf("receipt: seq=%d by executor-B at %s", seq, t1)
	if notify.Body != wantBody {
		t.Errorf("回告 body = %q，期望 %q（§2.3 格式原文）", notify.Body, wantBody)
	}
	if notify.CreatedAt != t1 {
		t.Errorf("回告 created_at = %q，期望注入时钟 %q", notify.CreatedAt, t1)
	}

	// 原发送方 poll 对话分支拉到回告（§2.3：原发送方下次 poll 即拉到）。
	pollURL := pollQuery(authCol, authRole, authSess)
	msgs := assertMailboxData(t, wantData(t, doAuthedReq(t, h, http.MethodGet, pollURL, "", heads4()...), http.StatusOK))
	if len(msgs) != 1 {
		t.Fatalf("原发送方 poll 条数 = %d，期望 1（对话分支命中回告）", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if got := m["kind"]; got != store.MessageKindReceipt {
		t.Errorf("poll 消息 kind = %v，期望 receipt", got)
	}
	if got := m["seq"]; got != float64(notify.Seq) {
		t.Errorf("poll 消息 seq = %v，期望回告 seq %d", got, notify.Seq)
	}
	if got := m["level"]; got != store.MessageLevelNormal {
		t.Errorf("poll 消息 level = %v，期望 normal", got)
	}
}

// heads4 默认发送方四头（测试内多场景共用，省逐处展开）。
func heads4() []string {
	return []string{authProj, authCol, authSess, authRole}
}

// TestReceiptNotBlock 非 block 级不可回执（AC7.4）：normal/important → 409
// not_block_level，错误信息指明 seq 与实际 level；拒绝零落库（receipts 与回告
// 消息均不产生）。
func TestReceiptNotBlock(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	for _, tc := range []struct{ level string }{{"normal"}, {"important"}} {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
			fmt.Sprintf(`{"target":{"kind":"bus"},"level":%q,"body":"x"}`, tc.level), heads4()...)
		seq := seqFromData(t, wantData(t, rr, http.StatusCreated))
		rr = doAuthedReq(t, h, http.MethodPost, receiptURL(seq), "", heads4()...)
		wantErrBody(t, rr, http.StatusConflict, types.CodeNotBlockLevel)
		if msg := errMsg(t, rr); !strings.Contains(msg, tc.level) || !strings.Contains(msg, fmt.Sprint(seq)) {
			t.Errorf("错误信息 %q 未指明 level=%q 与 seq=%d（AC2.2 指明语义）", msg, tc.level, seq)
		}
	}
	// 零落库：receipts 零行、回告消息零条（send 两条 direct/bus 消息除外）。
	if got := countTable(t, st, "message_receipts"); got != 0 {
		t.Errorf("not_block_level 拒绝后 message_receipts 行数 = %d，期望 0（拒绝零副作用）", got)
	}
	if got := countTable(t, st, "messages"); got != 2 {
		t.Errorf("messages 行数 = %d，期望 2（仅两条 send 消息，无回告）", got)
	}
}

// TestReceiptNotFound 消息 seq 不存在 → 404 message_not_found（§2.4 404 族，
// 信息指明 seq）；路径参数非数字/非正整数 → 400 param_invalid（路由参数解析面）。
func TestReceiptNotFound(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	rr := doAuthedReq(t, h, http.MethodPost, receiptURL(999), "", heads4()...)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeMessageNotFound)
	if msg := errMsg(t, rr); !strings.Contains(msg, "999") {
		t.Errorf("错误信息 %q 不含 seq 999（信息指明）", msg)
	}
	if got := countTable(t, st, "message_receipts"); got != 0 {
		t.Errorf("404 后 message_receipts 行数 = %d，期望 0", got)
	}

	// 非数字/负数/零 seq → 400 param_invalid。
	for _, bad := range []string{"abc", "-1", "0"} {
		rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages/"+bad+"/receipt", "", heads4()...)
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	}
}

// TestReceiptSystemBlockMessage 系统直插 block 行不可回执（handler_messages.go
// ⑤ 防御分支）：sender_session_id=0 的 block 消息本体存在，但回告无处投递
// （系统直插行不可回执）→ 404 message_not_found；拒绝零落库（receipts 零行）。
func TestReceiptSystemBlockMessage(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	// send 端点落库保证 sender_session_id 为真实会话 id，0=系统语义行只能直插
	// 构造（种法参考 TestHistoryFilters 的系统行：seedRawMessage 直插夹具）。
	seq := seedRawMessage(t, st, projectID, col05, store.MessageKindDirect, "", 0, 0,
		store.MessageLevelBlock, "系统阻断")

	rr := doAuthedReq(t, h, http.MethodPost, receiptURL(seq), "", heads4()...)
	wantErrBody(t, rr, http.StatusNotFound, types.CodeMessageNotFound)
	if msg := errMsg(t, rr); !strings.Contains(msg, fmt.Sprint(seq)) || !strings.Contains(msg, "无原发送方会话") {
		t.Errorf("错误信息 %q 未指明 seq=%d 与系统消息不可回执语义（AC2.2 指明）", msg, seq)
	}
	if got := countTable(t, st, "message_receipts"); got != 0 {
		t.Errorf("404 后 message_receipts 行数 = %d，期望 0（防御拒绝零落库）", got)
	}
}

// TestReceiptIdempotent 重复回执幂等（AC9.2 already_recepted 200 返回已有记录）：
// 重执响应逐字段=首执（已有记录视角，非重执请求视角）；回告消息不重复（幂等
// 零副作用——首次执行才落回告，重执仅回读）；回执方身份 T4 不校验，重执换人
// 时 receipt_by 仍=原回执方（已有记录）。
func TestReceiptIdempotent(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	if _, err := st.UpsertSession(projectID, col05, "executor-B", "executor"); err != nil {
		t.Fatalf("预注册回执方会话失败: %v", err)
	}
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","column":"05","role":"executor"},"level":"block","body":"阻断"}`,
		heads4()...)
	seq := seqFromData(t, wantData(t, rr, http.StatusCreated))

	injectFixedClock(t, "2026-01-01T09:00:00Z")
	first := wantData(t, doAuthedReq(t, h, http.MethodPost, receiptURL(seq), "", authProj, authCol, "executor-B", "executor"), http.StatusOK)
	if got := first["receipt_by"]; got != "executor-B" {
		t.Fatalf("首执 receipt_by = %v，期望 executor-B", got)
	}

	// 重执（同回执方，时钟已推进）：200+响应与首执逐字段一致（返回已有记录）。
	replay := wantData(t, doAuthedReq(t, h, http.MethodPost, receiptURL(seq), "", authProj, authCol, "executor-B", "executor"), http.StatusOK)
	if len(replay) != len(first) {
		t.Fatalf("重执 data 字段数 = %d，期望 %d", len(replay), len(first))
	}
	for k, v := range first {
		if replay[k] != v {
			t.Errorf("重执 data[%s] = %v，期望 %v（已有记录视角，AC9.2）", k, replay[k], v)
		}
	}

	// 换人重执（T4 回执方不校验）：200 仍返回已有记录，receipt_by=原回执方。
	injectFixedClock(t, "2026-01-01T10:00:00Z")
	byOther := wantData(t, doAuthedReq(t, h, http.MethodPost, receiptURL(seq), "", authProj, authCol, "reviewer-C", "executor_reviewer"), http.StatusOK)
	if got := byOther["receipt_by"]; got != "executor-B" {
		t.Errorf("换人重执 receipt_by = %v，期望原回执方 executor-B（已有记录）", got)
	}
	if got := byOther["receipt_at"]; got != first["receipt_at"] {
		t.Errorf("换人重执 receipt_at = %v，期望首执时间 %v（已有记录）", got, first["receipt_at"])
	}

	// 幂等零副作用：receipts 恰 1 行、回告消息恰 1 条（不重复发回告）。
	if got := countTable(t, st, "message_receipts"); got != 1 {
		t.Errorf("重执后 message_receipts 行数 = %d，期望 1（一消息一回执）", got)
	}
	findReceiptNotify(t, st) // 条数≠1 即 Fatal
}

// TestHistoryReceiptField #13 receipt 字段状态流转（AC7.2/AC7.3）+archived 历史
// 保留（AC1.3，B2-T2「历史查询不拒」例外）：回执前 receipt=null、回执后
// receipt={by,at}；响应消息恰 {seq,kind,level,body,sender,receipt,created_at}
// 七键；archived 项目/栏目历史查询 200 照常返回。
func TestHistoryReceiptField(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	if _, err := st.UpsertSession(projectID, col05, "executor-B", "executor"); err != nil {
		t.Fatalf("预注册回执方会话失败: %v", err)
	}
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","column":"05","role":"executor"},"level":"block","body":"待回执"}`,
		heads4()...)
	seq := seqFromData(t, wantData(t, rr, http.StatusCreated))
	histURL := "/api/v1/messages?project=" + authProj

	// 回执前：receipt=null（AC7.2 待回执）。
	data := wantData(t, doAuthedReq(t, h, http.MethodGet, histURL, "", heads4()...), http.StatusOK)
	msgs, ok := data["messages"].([]any)
	if !ok {
		t.Fatalf("data.messages 缺失或非数组: %v", data["messages"])
	}
	if len(msgs) != 1 {
		t.Fatalf("回执前 messages 条数 = %d，期望 1", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if len(m) != 7 {
		t.Errorf("消息字段面 = %v，期望恰 {seq,kind,level,body,sender,receipt,created_at}（§2.2 #13）", m)
	}
	if got, exists := m["receipt"]; got != nil || !exists {
		t.Errorf("回执前 receipt = %v（exists=%v），期望 JSON null（AC7.2）", got, exists)
	}
	if got := m["seq"]; got != float64(seq) {
		t.Errorf("messages[0].seq = %v，期望 %d", got, seq)
	}

	// 回执后：receipt={by,at}（AC7.3 已回执）。回执副作用同时落一条 kind=receipt
	// 回告消息（§2.3），历史全量返回 2 条（desc 最新在前=回告第 1 条）——按 seq
	// 定位原 block 消息断言 receipt 状态流转。
	injectFixedClock(t, "2026-01-01T09:00:00Z")
	wantData(t, doAuthedReq(t, h, http.MethodPost, receiptURL(seq), "", authProj, authCol, "executor-B", "executor"), http.StatusOK)
	data = wantData(t, doAuthedReq(t, h, http.MethodGet, histURL, "", heads4()...), http.StatusOK)
	msgs = data["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("回执后 messages 条数 = %d，期望 2（原消息+回告消息均入历史）", len(msgs))
	}
	m = nil
	for _, raw := range msgs {
		if cand := raw.(map[string]any); cand["seq"] == float64(seq) {
			m = cand
			break
		}
	}
	if m == nil {
		t.Fatalf("回执后 messages 未含原消息 seq=%d: %v", seq, msgs)
	}
	rec, ok := m["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("回执后 receipt = %v，期望对象 {by,at}（AC7.3）", m["receipt"])
	}
	if len(rec) != 2 {
		t.Errorf("receipt 字段面 = %v，期望恰 {by,at}（§2.2 #13）", rec)
	}
	if got := rec["by"]; got != "executor-B" {
		t.Errorf("receipt.by = %v，期望回执方会话名 executor-B", got)
	}
	if got := rec["at"]; got != "2026-01-01T09:00:00Z" {
		t.Errorf("receipt.at = %v，期望回执时刻（注入时钟）", got)
	}
	// sender 五要素之身份面（与 #10 同构）：{session,role,column}。
	sender := m["sender"].(map[string]any)
	if got := sender["session"]; got != authSess {
		t.Errorf("sender.session = %v，期望 %q", got, authSess)
	}

	// archived 历史 reserved（AC1.3）：直插两条消息的项目/栏目分别归档，历史
	// 查询 200 照常返回（#13 是 B2-T2「历史查询不拒」例外端点）。
	pid2, cid2 := seedProjectColumn(t, st, "p-arch", store.ProjectStatusActive, "01", store.ColumnStatusActive)
	seedRawMessage(t, st, pid2, cid2, store.MessageKindDirect, "executor", 0, 0, store.MessageLevelBlock, "arch 项目消息")
	pid3, _ := seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "03", store.ColumnStatusActive)
	col02 := seedColumnOnly(t, st, "p-b", "02", store.ColumnStatusActive)
	seedRawMessage(t, st, pid3, col02, store.MessageKindDirect, "executor", 0, 0, store.MessageLevelNormal, "arch 栏目消息")
	// 经端点归档：p-arch 整项目、p-b 的 02 栏目。
	wantData(t, doAuthedReq(t, h, http.MethodDelete, "/api/v1/projects/p-arch", "", heads4()...), http.StatusOK)
	wantData(t, doAuthedReq(t, h, http.MethodDelete, "/api/v1/projects/p-b/columns/02", "", heads4()...), http.StatusOK)

	// archived 项目历史查询（四头域 p-arch——中间件只挡不存在，放行）→ 200。
	data = wantData(t, doAuthedReq(t, h, http.MethodGet, "/api/v1/messages?project=p-arch", "",
		"p-arch", "01", "s-arch", "executor"), http.StatusOK)
	msgs = data["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("archived 项目历史条数 = %d，期望 1（AC1.3 历史保留）", len(msgs))
	}
	// archived 栏目历史查询 → 200。
	data = wantData(t, doAuthedReq(t, h, http.MethodGet, "/api/v1/messages?project=p-b&column=02", "",
		"p-b", "02", "s-02", "executor"), http.StatusOK)
	msgs = data["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("archived 栏目历史条数 = %d，期望 1（AC2.3 历史可查）", len(msgs))
	}
	if got := msgs[0].(map[string]any)["body"]; got != "arch 栏目消息" {
		t.Errorf("archived 栏目历史 body = %v，期望原样返回", got)
	}
}

// TestRollbackAtomicity 同事务原子性（§2.3 副作用链：回执行+回告消息要么都在
// 要么都不在）：TEMP 触发器注入回告 INSERT 失败（挂 messages 表、WHEN 过滤
// kind=receipt），回执请求 → 500；receipts 行也不在（整体回滚）；DROP 触发器
// 后重执成功（事务完整性恢复）。
func TestRollbackAtomicity(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","column":"05","role":"executor"},"level":"block","body":"阻断"}`,
		heads4()...)
	seq := seqFromData(t, wantData(t, rr, http.StatusCreated))

	// 注入：TEMP 触发器（可挂永久表）对 kind=receipt 的 INSERT RAISE ABORT。
	if _, err := st.DB.Exec(
		`CREATE TEMP TRIGGER b2_5_inject_receipt_fail
		 BEFORE INSERT ON messages
		 WHEN NEW.kind = 'receipt'
		 BEGIN SELECT RAISE(ABORT, 'injected for atomicity test'); END`,
	); err != nil {
		t.Fatalf("创建注入触发器失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.DB.Exec(`DROP TRIGGER IF EXISTS b2_5_inject_receipt_fail`)
	})

	// 回执 → 回告 INSERT 撞注入 → 组合方法整体回滚 → 500 internal_error。
	rr = doAuthedReq(t, h, http.MethodPost, receiptURL(seq), "", heads4()...)
	wantErrBody(t, rr, http.StatusInternalServerError, types.CodeInternalError)
	if got := countTable(t, st, "message_receipts"); got != 0 {
		t.Errorf("注入失败后 message_receipts 行数 = %d，期望 0（同事务回滚——回执行不落）", got)
	}
	if got := countTable(t, st, "messages"); got != 1 {
		t.Errorf("注入失败后 messages 行数 = %d，期望 1（仅原消息，回告未落）", got)
	}

	// 摘除注入后重执成功（触发器 TEMP 生命周期随连接，防御性显式 DROP）。
	if _, err := st.DB.Exec(`DROP TRIGGER IF EXISTS b2_5_inject_receipt_fail`); err != nil {
		t.Fatalf("摘除注入触发器失败: %v", err)
	}
	rr = doAuthedReq(t, h, http.MethodPost, receiptURL(seq), "", heads4()...)
	wantData(t, rr, http.StatusOK)
	if got := countTable(t, st, "message_receipts"); got != 1 {
		t.Errorf("恢复后 message_receipts 行数 = %d，期望 1", got)
	}
	findReceiptNotify(t, st)
}

// TestHistoryFilters #13 过滤参数组合抽查（§2.2 #13 query 全量）：kind/level/
// sent_by/since_seq/before_seq/order 逐参数与组合断言+默认 desc/50+limit 截取；
// 非法 order/负 limit/非数字 seq 过滤 → 400 param_invalid。
func TestHistoryFilters(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, col05 := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	col06 := seedColumnOnly(t, st, "p-a", "06", store.ColumnStatusActive)
	if _, err := st.UpsertSession(projectID, col05, "reviewer-C", "executor_reviewer"); err != nil {
		t.Fatalf("预注册 reviewer-C 失败: %v", err)
	}
	injectFixedClock(t, "2026-01-01T08:00:00Z")

	// 种子：controller-A 发 4 条（direct/block、direct/normal、bus/important、
	// chat→reviewer-C/normal）+reviewer-C 发 1 条（direct→executor/normal）
	// +直插 1 条系统语义（sender_session_id=0，kind=receipt）。
	var seqDirectBlock, seqBus int64
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","column":"05","role":"executor"},"level":"block","body":"b1"}`, heads4()...)
	seqDirectBlock = seqFromData(t, wantData(t, rr, http.StatusCreated))
	for _, body := range []string{
		`{"target":{"kind":"direct","column":"05","role":"executor"},"level":"normal","body":"n1"}`,
		`{"target":{"kind":"bus"},"level":"important","body":"bus1"}`,
		`{"target":{"kind":"chat","session":"reviewer-C"},"level":"normal","body":"chat1"}`,
	} {
		rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", body, heads4()...)
		wantData(t, rr, http.StatusCreated)
	}
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","column":"05","role":"executor"},"level":"normal","body":"r1"}`,
		authProj, authCol, "reviewer-C", "executor_reviewer")
	wantData(t, rr, http.StatusCreated)
	// bus 序号记录（第 3 条）。
	busMsgs := queryMessages(t, st, authProj)
	for _, m := range busMsgs {
		if m.Kind == store.MessageKindBus {
			seqBus = m.Seq
		}
	}
	seedRawMessage(t, st, projectID, col06, store.MessageKindReceipt, "", 0, 0, store.MessageLevelNormal, "系统回告")
	// p-b 一条（project 过滤反例）。
	pidB, cidB := seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "01", store.ColumnStatusActive)
	seedRawMessage(t, st, pidB, cidB, store.MessageKindDirect, "executor", 0, 0, store.MessageLevelNormal, "他项目")

	// count 断言 helper：按过滤参数查 #13，返回 seq 列表。
	querySeqs := func(t *testing.T, qs string) []int64 {
		t.Helper()
		data := wantData(t, doAuthedReq(t, h, http.MethodGet, "/api/v1/messages"+qs, "", heads4()...), http.StatusOK)
		raw, ok := data["messages"].([]any)
		if !ok {
			t.Fatalf("messages 非数组: %v", data["messages"])
		}
		seqs := make([]int64, 0, len(raw))
		for _, e := range raw {
			seqs = append(seqs, int64(e.(map[string]any)["seq"].(float64)))
		}
		return seqs
	}
	assertSeqs := func(t *testing.T, got []int64, want ...int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("seq 列表 = %v，期望 %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("seq 列表 = %v，期望 %v", got, want)
			}
		}
	}

	// 默认（无参数）：全库 7 条 desc（最新在前）。
	all := querySeqs(t, "")
	if len(all) != 7 {
		t.Fatalf("无过滤条数 = %d，期望 7", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i] >= all[i-1] {
			t.Fatalf("默认排序非严格降序: %v（order 默认 desc）", all)
		}
	}

	// kind/level 精确过滤。
	assertSeqs(t, querySeqs(t, "?kind=direct&level=block"), seqDirectBlock)
	assertSeqs(t, querySeqs(t, "?kind=bus"), seqBus)
	// kind=receipt 直插行命中（sender=0 系统语义）。
	if got := len(querySeqs(t, "?kind=receipt")); got != 1 {
		t.Errorf("kind=receipt 条数 = %d，期望 1", got)
	}

	// column 过滤：chat 消息归属目标会话所属栏目（reviewer-C 在 05），06 仅
	// 直插 receipt 回告 1 条——验证 column 按消息归属栏目精确过滤。
	col06Seqs := querySeqs(t, "?column=06")
	if len(col06Seqs) != 1 {
		t.Errorf("column=06 条数 = %d，期望 1（chat 归属目标会话栏目 05）", len(col06Seqs))
	}

	// sent_by：controller-A 恰 4 条；reviewer-C 恰 1 条；sender_session_id=0
	// 的系统行不命中任何会话名。
	if got := len(querySeqs(t, "?sent_by=controller-A")); got != 4 {
		t.Errorf("sent_by=controller-A 条数 = %d，期望 4", got)
	}
	rcSeqs := querySeqs(t, "?sent_by=reviewer-C")
	if len(rcSeqs) != 1 {
		t.Fatalf("sent_by=reviewer-C 条数 = %d，期望 1", len(rcSeqs))
	}

	// since_seq/before_seq 区间（含起点/不含终点）。
	mid := seqDirectBlock + 1 // 自增 seq 连续：block=1、normal=2、bus=3、chat=4、r1=5、回告=6、p-b=7
	assertSeqs(t, querySeqs(t, fmt.Sprintf("?since_seq=%d&before_seq=%d&order=asc", mid, mid+2)), mid, mid+1)

	// order=asc 升序。
	asc := querySeqs(t, "?order=asc")
	if len(asc) != 7 || asc[0] >= asc[1] {
		t.Fatalf("order=asc 排序异常: %v", asc)
	}

	// limit 截取：limit=1 → 恰 1 条（desc=最新）；limit 超过 MaxHistoryLimit
	// 被 store clamp 到 500——小库全量返回（无过滤同 7 条）。
	one := querySeqs(t, "?limit=1")
	if len(one) != 1 || one[0] != all[0] {
		t.Errorf("limit=1 = %v，期望最新一条 %v", one, all[0])
	}
	if got := querySeqs(t, "?limit=999"); len(got) != 7 {
		t.Errorf("limit=999 条数 = %d，期望 7（clamp 500 后全量）", len(got))
	}

	// project 过滤（反例隔离）。
	if got := len(querySeqs(t, "?project=p-b")); got != 1 {
		t.Errorf("project=p-b 条数 = %d，期望 1", got)
	}

	// 非法参数 → 400 param_invalid：order 白名单外、limit 负值/非数字、
	// since_seq/before_seq 非数字。
	for _, qs := range []string{
		"?order=sideways", "?limit=-1", "?limit=abc", "?since_seq=x", "?before_seq=-3",
	} {
		rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/messages"+qs, "", heads4()...)
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	}
}

// TestConcurrentSend20 AC5.2 handler 层：20 协程同栏目混合角色并发 send →
// 全 201、seq 两两不同、messages 恰 20 行（store 单连接池串行化 + busy_timeout）。
// goroutine 内不触 *testing.T（非并发安全），结果经 channel 汇总主协程断言。
// 目标角色用无主的 executor_duty（2026-10-03 增补令 agent_conflict 拦截后，
// direct 目标格子 ≥2 活会话即 409——发送方 worker 已占 executor 等格子，
// 定向到无主格子使并发压测聚焦 seq 取号面，不受 role 规约闸拦截）。
func TestConcurrentSend20(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)

	const n = 20
	roles := []string{"executor", "executor_reviewer", "executor_tester"} // role 不枚举硬校验（§3.2 表 3 注）
	type result struct {
		code int
		seq  int64
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"target":{"kind":"direct","column":"05","role":"executor_duty"},"level":"normal","body":"并发-%d"}`, i)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/messages", strings.NewReader(body))
			// 发送方身份：会话名唯一（worker-N，模拟 20 个独立进程）、role 轮换（混合角色）。
			req.Header.Set(types.HeaderAiteamProject, authProj)
			req.Header.Set(types.HeaderAiteamColumn, authCol)
			req.Header.Set(types.HeaderAiteamSession, fmt.Sprintf("worker-%d", i))
			req.Header.Set(types.HeaderAiteamRole, roles[i%len(roles)])
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			var resp types.DataResponse
			_ = json.Unmarshal(rr.Body.Bytes(), &resp)
			data, _ := resp.Data.(map[string]any)
			var seq int64
			if f, ok := data["seq"].(float64); ok {
				seq = int64(f)
			}
			results <- result{code: rr.Code, seq: seq}
		}()
	}
	wg.Wait()
	close(results)

	// 全 201 + seq 两两不同。
	var seqs []int64
	for r := range results {
		if r.code != http.StatusCreated {
			t.Errorf("并发 send status = %d，期望全 201", r.code)
		}
		if r.seq <= 0 {
			t.Errorf("并发 send seq = %d，期望 > 0", r.seq)
		}
		seqs = append(seqs, r.seq)
	}
	slices.Sort(seqs)
	for i := 1; i < len(seqs); i++ {
		if seqs[i] == seqs[i-1] {
			t.Fatalf("seq 重复: %d（AC5.2 全局唯一）", seqs[i])
		}
	}
	if len(seqs) != n {
		t.Fatalf("成功数 = %d，期望 %d", len(seqs), n)
	}

	// 入库数 = 20（AC5.2 入库面）。
	if got := countTable(t, st, "messages"); got != n {
		t.Errorf("messages 行数 = %d，期望 %d", got, n)
	}
	if got := countTable(t, st, "sessions"); got != n {
		t.Errorf("sessions 行数 = %d，期望 %d（20 独立发送方会话经中间件 upsert）", got, n)
	}
}

// ---- #9 direct 形态 agent_conflict 拦截（2026-10-03 增补令·role 规约闸）----
//
// 拦截面=direct 定向分支（域校验后、InsertMessage 前）：目标 (column,role) 格子
// 存在 ≥2 个活会话（活=last_seen 在失联阈值内，与心跳判定同口径）→ 409 拒投，
// 错误信息列活会话名+教学语；单会话/零会话格子零影响；bus/chat 不拦。
// 以下五用例对应增补令测试清单（双活拒投/单活放行/失联者不计活/controller 格
// 同款/bus·chat 不拦）。

// TestSendDirectAgentConflictDoubleAlive 双活拒投：目标 (05,executor) 格子两个
// 活会话 → 409 agent_conflict；错误信息含两活会话名+教学语「请个体化命名
// executor_<标识> 或改用 --bus 广播」（技术设计 §2.2 #9 口径）；拒投不落库。
func TestSendDirectAgentConflictDoubleAlive(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	for _, name := range []string{"executor-A", "executor-B"} {
		if _, err := st.UpsertSession(projectID, columnID, name, "executor"); err != nil {
			t.Fatalf("预注册 %s 失败: %v", name, err)
		}
	}

	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", msgSendBody, heads4()...)
	wantErrBody(t, rr, http.StatusConflict, types.CodeAgentConflict)
	msg := errMsg(t, rr)
	// 「目标 05/executor 格子存在」=项目内文案前缀逐字节锚（b5-W2 收口质量审加固：
	// agent_conflict 文案已重构为 cell 拼装，本断言给项目内格式留失败面——拼错即红，
	// AC9.4 由「代码看起来没变」升级为有测试锚定）。
	for _, want := range []string{"目标 05/executor 格子存在", "executor-A", "executor-B", "请个体化命名", "--bus"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息 %q 不含 %q（增补令：列活会话名+教学语）", msg, want)
		}
	}
	if got := countTable(t, st, "messages"); got != 0 {
		t.Errorf("拒投后 messages 行数 = %d，期望 0（409 拒投不落库）", got)
	}
}

// TestSendDirectAgentConflictSingleAlive 单活放行（增补令「单会话格子零影响」）：
// 目标格子恰一活会话 → 201 正常落库且归属规则不受拦截影响。
func TestSendDirectAgentConflictSingleAlive(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	if _, err := st.UpsertSession(projectID, columnID, "executor-A", "executor"); err != nil {
		t.Fatalf("预注册 executor-A 失败: %v", err)
	}

	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", msgSendBody, heads4()...)
	data := wantData(t, rr, http.StatusCreated)
	if got := data["kind"]; got != "direct" {
		t.Errorf("data.kind = %v，期望 direct（单活格子放行）", got)
	}
	m := findMessageBySeq(t, st, seqFromData(t, data))
	if m.ColumnID != columnID || m.TargetRole != "executor" {
		t.Errorf("落库归属 = (column_id=%d,target_role=%q)，期望目标格子 (%d,executor)", m.ColumnID, m.TargetRole, columnID)
	}
	if m.ProjectID != projectID {
		t.Errorf("落库 project_id = %d，期望 %d", m.ProjectID, projectID)
	}
}

// TestSendDirectAgentConflictStaleNotAlive 失联者不计活（增补令「活=last_seen
// 在失联阈值内，与心跳判定同口径」）：格子两会话其一 last_seen 距今 1200s >
// 项目阈值 900s（seedProjectColumn 夹具值）= 失联 → 活会话仅 1 → 201 放行
// （失联格子复用合法——换班场景）。
func TestSendDirectAgentConflictStaleNotAlive(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	const (
		tStale = "2026-01-01T09:40:00Z" // 距 t0 恰 1200s > 900s 阈值 → 失联
		t0     = "2026-01-01T10:00:00Z"
	)
	injectFixedClock(t, tStale)
	if _, err := st.UpsertSession(projectID, columnID, "executor-stale", "executor"); err != nil {
		t.Fatalf("预注册 executor-stale 失败: %v", err)
	}
	injectFixedClock(t, t0)
	if _, err := st.UpsertSession(projectID, columnID, "executor-fresh", "executor"); err != nil {
		t.Fatalf("预注册 executor-fresh 失败: %v", err)
	}

	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", msgSendBody, heads4()...)
	data := wantData(t, rr, http.StatusCreated)
	m := findMessageBySeq(t, st, seqFromData(t, data))
	if m.ColumnID != columnID {
		t.Errorf("失联者不计活：消息应落库（column_id=%d），实际被拦", columnID)
	}
}

// TestSendDirectAgentConflictControllerCell controller 格同款生效（增补令
// 「controller 格子同款生效=一栏一控机械兜底」），同时固化「含自己在格子」读法：
// 增补令字面「目标 (column,role) 存在 ≥2 个活会话」未排除发送方自己——direct
// 发给自己所在格子时，发送方会话（中间件本请求已 upsert、last_seen=now 恒活）
// 计入格子会话数。格内仅自己（单活）→ 201 放行；注册第二个 controller 后格内
// 2 活（含自己）→ 409 且错误信息列含自己在内的两名会话。
func TestSendDirectAgentConflictControllerCell(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	selfCell := `{"target":{"kind":"direct","column":"05","role":"controller"},"level":"normal","body":"自格定向"}`

	// 格内仅发送方自己（中间件 upsert 的 controller-A）= 单活 → 201。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", selfCell, heads4()...)
	wantData(t, rr, http.StatusCreated)

	// 第二个 controller 注册（t0 活）→ 格内 2 活 → 409，信息含自己与对方。
	if _, err := st.UpsertSession(projectID, columnID, "controller-B", "controller"); err != nil {
		t.Fatalf("预注册 controller-B 失败: %v", err)
	}
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", selfCell, heads4()...)
	wantErrBody(t, rr, http.StatusConflict, types.CodeAgentConflict)
	msg := errMsg(t, rr)
	for _, want := range []string{"controller-A", "controller-B"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息 %q 不含 %q（活会话名清单含发送方自己）", msg, want)
		}
	}
}

// TestSendAgentConflictBusChatNotBlocked bus/chat 不拦（增补令拦截面=direct
// 定向分支）：同双活 (05,executor) 格子下 bus 照发 201（无目标角色，发送方栏目
// 锚点）、chat 定点到具体会话照发 201——多人共格的乱源只在 role 定向投递。
func TestSendAgentConflictBusChatNotBlocked(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	for _, name := range []string{"executor-A", "executor-B"} {
		if _, err := st.UpsertSession(projectID, columnID, name, "executor"); err != nil {
			t.Fatalf("预注册 %s 失败: %v", name, err)
		}
	}

	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"bus"},"level":"normal","body":"总线"}`, heads4()...)
	if got := wantData(t, rr, http.StatusCreated)["kind"]; got != "bus" {
		t.Errorf("bus 形态被拦或 kind = %v，期望 201 放行", got)
	}
	rr = doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"chat","session":"executor-A"},"level":"normal","body":"定点"}`, heads4()...)
	if got := wantData(t, rr, http.StatusCreated)["kind"]; got != "chat" {
		t.Errorf("chat 形态被拦或 kind = %v，期望 201 放行", got)
	}
	if got := countTable(t, st, "messages"); got != 2 {
		t.Errorf("messages 行数 = %d，期望 2（bus+chat 均落库）", got)
	}
}

// ---- b7-W2 #10 cross_grid_hint 装配（b7-spec §一 W2 行/§二.2，AC2/AC3/AC5/R4）----

// seedMessageRow 直插任意 kind 消息夹具（seedDirectMessage 的泛化版，b7 hint
// 用例构造 chat 等非 direct 面；sender/target session 0=弱关联无 FK）。
func seedMessageRow(t *testing.T, st *store.Store, projectID, columnID int64, kind, targetRole string, targetSessionID int64) int64 {
	t.Helper()
	res, err := st.DB.Exec(
		`INSERT INTO messages (project_id, column_id, kind, target_role, target_session_id,
		       sender_session_id, sender_label, level, body, created_at)
		 VALUES (?, ?, ?, ?, ?, 0, 'system', 'normal', 'x', '2026-01-01T08:00:00Z')`,
		projectID, columnID, kind, targetRole, targetSessionID,
	)
	if err != nil {
		t.Fatalf("直插 %s 消息夹具失败: %v", kind, err)
	}
	seq, _ := res.LastInsertId()
	return seq
}

// assertCrossGridHint 解出响应 data 的 cross_grid_hint 数组（缺键/非数组即
// Fatal——装配条件面的存在性断言入口）。
func assertCrossGridHint(t *testing.T, data map[string]any) []any {
	t.Helper()
	hint, ok := data["cross_grid_hint"].([]any)
	if !ok {
		t.Fatalf("data.cross_grid_hint 缺失或非数组（b7-W2 additive 装配面）: %v", data)
	}
	return hint
}

// TestPollCrossGridHint b7 防呆装配条件面（触发/零噪音/位点感知/R4 直接口径）：
//   - 面 C（AC2 空态）：空域 poll 三角色 → 恰三键零 hint；
//   - 面 A（触发主面）：executor_A 格 3 条未消费 direct，错格 role=executor
//     poll → data 恰四键，hint 恰一元素 {role:executor_A,pending:3}，元素恰
//     {role,pending} 两键（AC5 最小面——零正文/零 level/零时间戳）；
//   - 面 B（AC2 零噪音·本格有 direct）：executor_A 格 own direct 可见时不出
//     hint，即使他格（executor）有积压；
//   - 面 D（位点感知两步对照）：先消费自己格 → 可见集归零 hint 仍在；再消费
//     executor_A → hint 消失（消失归因于他格位点消费）；
//   - 面 R4（R4 直接口径）：可见集=chat 在而 direct 空 → 仍出 hint（触发条件
//     是 direct 面 0 而非消息总数 0——实战签名漏防面）。
func TestPollCrossGridHint(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	// executor_B 会话预注册（面 R4 消费视角 session 须已注册）。
	if _, err := st.UpsertSession(projectID, columnID, "exec-B-sess", "executor_B"); err != nil {
		t.Fatalf("预注册 executor_B 会话失败: %v", err)
	}
	pollAs := func(col, role, sess string) map[string]any {
		t.Helper()
		return wantData(t, doAuthedReq(t, h, http.MethodGet, pollQuery(col, role, sess), "",
			authProj, authCol, authSess, authRole), http.StatusOK)
	}

	// 面 C：空域三角色 poll——零积压零 hint（恰三键，additive 空态=键缺省）。
	for _, tc := range []struct{ col, role, sess string }{
		{authCol, "executor", ""}, {authCol, "executor_A", ""}, {authCol, "executor_B", "exec-B-sess"},
	} {
		data := pollAs(tc.col, tc.role, tc.sess)
		if msgs := assertMailboxData(t, data); len(msgs) != 0 {
			t.Errorf("面 C 空域 messages = %d 条，期望 0", len(msgs))
		}
	}

	// 夹具：executor_A 格 3 条未消费 direct（面 A 触发面积压）。
	var lastA int64
	for i := 0; i < 3; i++ {
		lastA = seedDirectMessage(t, st, projectID, columnID, "executor_A")
	}

	// 面 A：错格 poll → hint 恰一元素且最小面。
	data := pollAs(authCol, "executor", "")
	if len(data) != 4 {
		t.Fatalf("面 A data 键数 = %d，期望恰四键（三键+cross_grid_hint）: %v", len(data), data)
	}
	hint := assertCrossGridHint(t, data)
	if len(hint) != 1 {
		t.Fatalf("面 A hint 元素数 = %d，期望 1（仅 executor_A 格积压）: %v", len(hint), hint)
	}
	elem, ok := hint[0].(map[string]any)
	if !ok {
		t.Fatalf("面 A hint[0] 非对象: %v", hint[0])
	}
	if len(elem) != 2 {
		t.Errorf("面 A hint 元素键数 = %d，期望恰 {role,pending} 两键（AC5 最小面）: %v", len(elem), elem)
	}
	if got := elem["role"]; got != "executor_A" {
		t.Errorf("面 A hint[0].role = %v，期望 executor_A", got)
	}
	if got := elem["pending"]; got != float64(3) {
		t.Errorf("面 A hint[0].pending = %v，期望 3", got)
	}

	// 面 B：本格（executor_A）own direct 可见 → 零 hint，即使他格（executor）
	// 有 2 条积压；对照 poll executor（own direct 可见）同样零 hint。
	_ = seedDirectMessage(t, st, projectID, columnID, "executor")
	lastE := seedDirectMessage(t, st, projectID, columnID, "executor")
	data = pollAs(authCol, "executor_A", "")
	msgs := assertMailboxData(t, data)
	if len(msgs) != 3 {
		t.Errorf("面 B executor_A messages = %d 条，期望 3（own direct 可见）", len(msgs))
	}
	if _, present := data["cross_grid_hint"]; present {
		t.Errorf("面 B 本格有 direct 仍出 hint: %v（AC2 零噪音失守）", data["cross_grid_hint"])
	}
	data = pollAs(authCol, "executor", "")
	msgs = assertMailboxData(t, data)
	if len(msgs) != 2 {
		t.Errorf("面 B executor messages = %d 条，期望 2（own direct 可见）", len(msgs))
	}
	if _, present := data["cross_grid_hint"]; present {
		t.Errorf("面 B executor own direct 可见仍出 hint: %v（AC2 零噪音失守）", data["cross_grid_hint"])
	}

	// 面 D（位点感知两步对照）：①先消费自己格（executor 位点推到 own direct
	// 顶）→ 可见集归零，hint 仍在（executor_A 3 条积压未消费）；②再消费
	// executor_A → hint 消失——消失归因于他格位点消费，非本格位点变动。
	bumpPosition(t, st, columnID, "executor", lastE)
	data = pollAs(authCol, "executor", "")
	// D① data 恰四键（可见集空+hint 并存）。
	if len(data) != 4 {
		t.Fatalf("面 D① data 键数 = %d，期望恰四键: %v", len(data), data)
	}
	if rawMsgs, ok := data["messages"].([]any); !ok || len(rawMsgs) != 0 {
		t.Errorf("面 D① messages = %v，期望空（own direct 已消费）", data["messages"])
	}
	hint = assertCrossGridHint(t, data)
	if len(hint) != 1 {
		t.Fatalf("面 D① hint 元素数 = %d，期望 1（executor_A 未消费）: %v", len(hint), hint)
	}
	if elem = hint[0].(map[string]any); elem["role"] != "executor_A" || elem["pending"] != float64(3) {
		t.Errorf("面 D① hint[0] = %v，期望 {executor_A 3}", elem)
	}
	bumpPosition(t, st, columnID, "executor_A", lastA)
	data = pollAs(authCol, "executor", "")
	if msgs := assertMailboxData(t, data); len(msgs) != 0 {
		t.Errorf("面 D② messages = %d 条，期望 0", len(msgs))
	}
	if _, present := data["cross_grid_hint"]; present {
		t.Errorf("面 D② executor_A 消费后仍出 hint: %v（位点感知失守）", data["cross_grid_hint"])
	}

	// 面 R4：executor_B 可见集=chat 1 条（direct 空）→ 仍出 hint（executor 格
	// 2 条积压）——触发按 direct 面 0 非消息总数 0。（面 D 消费掉旧积压后，
	// 为 executor 格另种 2 条新 direct 作 R4 积压面：seq 在其位点 5 之后。）
	_ = seedDirectMessage(t, st, projectID, columnID, "executor")
	_ = seedDirectMessage(t, st, projectID, columnID, "executor")
	seedMessageRow(t, st, projectID, columnID, "chat", "", querySessionID(t, st, projectID, "exec-B-sess"))
	data = pollAs(authCol, "executor_B", "exec-B-sess")
	// R4 面 data 恰四键（messages+hint 并存——chat 可见与 hint 不互斥）。
	rawMsgs, ok := data["messages"].([]any)
	if !ok {
		t.Fatalf("面 R4 data.messages 缺失或非数组: %v", data)
	}
	msgs = rawMsgs
	if len(msgs) != 1 {
		t.Fatalf("面 R4 executor_B messages = %d 条，期望 1（chat 可见）", len(msgs))
	}
	if m := msgs[0].(map[string]any); m["kind"] != "chat" {
		t.Errorf("面 R4 messages[0].kind = %v，期望 chat", m["kind"])
	}
	hint = assertCrossGridHint(t, data)
	if len(hint) != 1 {
		t.Fatalf("面 R4 hint 元素数 = %d，期望 1: %v", len(hint), hint)
	}
	elem = hint[0].(map[string]any)
	if got := elem["role"]; got != "executor" {
		t.Errorf("面 R4 hint[0].role = %v，期望 executor（2 条积压格）", got)
	}
	if got := elem["pending"]; got != float64(2) {
		t.Errorf("面 R4 hint[0].pending = %v，期望 2", got)
	}
}

// querySessionID 按会话名查 id（chat 夹具 target_session_id 定位用）。
func querySessionID(t *testing.T, st *store.Store, projectID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := st.DB.QueryRow(`SELECT id FROM sessions WHERE project_id = ? AND name = ?`,
		projectID, name).Scan(&id); err != nil {
		t.Fatalf("查询会话 %s 失败: %v", name, err)
	}
	return id
}

// TestPollCrossGridHintTruncation 服务端截断聚合面（spec §二.1 末句：列表上限
// 5 格，超出聚合「等 N 格」提示语）：7 格积压（计数 3/2/1×5）→ hint 恰 6 元素=
// Top5（计数降序）+聚合哨兵元素 {role:"等 2 格", pending:2}（剩余两格条数求和；
// 并列 1 条的 5 格间序不定，集合式断言）。
func TestPollCrossGridHintTruncation(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	for i := 0; i < 3; i++ {
		seedDirectMessage(t, st, projectID, columnID, "executor_A")
	}
	for i := 0; i < 2; i++ {
		seedDirectMessage(t, st, projectID, columnID, "executor_B")
	}
	tail := map[string]bool{"executor_C": false, "executor_D": false, "executor_E": false, "executor_F": false, "executor_G": false}
	for role := range tail {
		seedDirectMessage(t, st, projectID, columnID, role)
	}

	data := wantData(t, doAuthedReq(t, h, http.MethodGet, pollQuery(authCol, "executor", ""), "",
		authProj, authCol, authSess, authRole), http.StatusOK)
	if len(data) != 4 {
		t.Fatalf("data 键数 = %d，期望恰四键: %v", len(data), data)
	}
	hint := assertCrossGridHint(t, data)
	if len(hint) != 6 {
		t.Fatalf("hint 元素数 = %d，期望 6（Top5+聚合哨兵）: %v", len(hint), hint)
	}
	head := [][2]any{{"executor_A", float64(3)}, {"executor_B", float64(2)}}
	for i, want := range head {
		elem := hint[i].(map[string]any)
		if elem["role"] != want[0] || elem["pending"] != want[1] {
			t.Errorf("hint[%d] = %v，期望 {role:%v, pending:%v}", i, elem, want[0], want[1])
		}
		if len(elem) != 2 {
			t.Errorf("hint[%d] 键数 = %d，期望恰两键（AC5）: %v", i, len(elem), elem)
		}
	}
	seen := map[string]bool{}
	for i := 2; i < 5; i++ {
		elem := hint[i].(map[string]any)
		role, _ := elem["role"].(string)
		if _, ok := tail[role]; !ok || seen[role] || elem["pending"] != float64(1) {
			t.Errorf("hint[%d] = %v，期望 tail 五格中未重复的一员 pending=1", i, elem)
		}
		seen[role] = true
	}
	if len(seen) != 3 {
		t.Fatalf("Top5 尾部去重格数 = %d，期望 3", len(seen))
	}
	agg := hint[5].(map[string]any)
	if got := agg["role"]; got != "等 2 格" {
		t.Errorf("聚合哨兵 role = %q，期望 \"等 2 格\"（7-5=2 格）", got)
	}
	if got := agg["pending"]; got != float64(2) {
		t.Errorf("聚合哨兵 pending = %v，期望 2（剩余两格各 1 条求和）", got)
	}
}

// TestBuildCrossGridHintPure 装配纯函数面（b7-W2，含 AC3 降级）：err 非 nil 降级
// nil（提示失败不反噬主响应——本函数是 degrade 路径的可测面：集成层无法在不破坏
// 主查询的前提下构造 CrossGridDirectPending 单点失败）；空切片降级 nil（omitempty
// 键缺省）；≤cap 保序全量映射；>cap Top5+聚合哨兵（剩余条数求和）。
func TestBuildCrossGridHintPure(t *testing.T) {
	if got := buildCrossGridHint(nil, fmt.Errorf("boom")); got != nil {
		t.Errorf("err 降级 = %v，期望 nil（提示不反噬主响应）", got)
	}
	if got := buildCrossGridHint([]store.CrossGridPending{}, nil); got != nil {
		t.Errorf("空切片 = %v，期望 nil（零噪音键缺省）", got)
	}

	got := buildCrossGridHint([]store.CrossGridPending{{Role: "a", Pending: 1}, {Role: "b", Pending: 2}}, nil)
	want := []crossGridHintData{{Role: "a", Pending: 1}, {Role: "b", Pending: 2}}
	if len(got) != len(want) {
		t.Fatalf("≤cap 映射长度 = %d，期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("映射[%d] = %+v，期望 %+v（保 SQL 计数降序）", i, got[i], want[i])
		}
	}

	seven := []store.CrossGridPending{
		{Role: "r1", Pending: 7}, {Role: "r2", Pending: 6}, {Role: "r3", Pending: 5},
		{Role: "r4", Pending: 4}, {Role: "r5", Pending: 3}, {Role: "r6", Pending: 2}, {Role: "r7", Pending: 1},
	}
	got = buildCrossGridHint(seven, nil)
	if len(got) != crossGridHintCap+1 {
		t.Fatalf(">cap 映射长度 = %d，期望 %d（Top5+聚合哨兵）", len(got), crossGridHintCap+1)
	}
	for i, w := range []struct {
		role string
		pend int
	}{{"r1", 7}, {"r2", 6}, {"r3", 5}, {"r4", 4}, {"r5", 3}} {
		if got[i].Role != w.role || got[i].Pending != w.pend {
			t.Errorf("Top5[%d] = %+v，期望 {role:%s pending:%d}", i, got[i], w.role, w.pend)
		}
	}
	if agg := got[crossGridHintCap]; agg.Role != "等 2 格" || agg.Pending != 3 {
		t.Errorf("聚合哨兵 = %+v，期望 {role:等 2 格 pending:3}（r6+r7 条数求和）", agg)
	}
}
