package server

// B5-1 看板域三端点测试（b5-plan §B5-1 TDD；b5-spec §四测试策略）：
//   - TestBoardSend     #24 POST /api/v1/board/messages（落库五要素+审计同事务+错误面）
//   - TestBoardDialog   #25 GET /api/v1/board/sessions/{id}/dialog（全流+隔离+AC15.3）
//   - TestBoardDialogTimelineKeys  b3-W2 #25 换 Timeline 后键面（六键+From 三分支+positions）
//   - TestBusStream     #26 GET /api/v1/board/bus-stream（倒序+level+preview+limit）
//   - TestHeaderExempt  B5-T1 四头豁免面（六端点可选/#24 恒豁免/强制面零回归）
//
// #24 请求契约按 b5-spec §1.1「对话目标定位（2026-10-03 总控裁定注记）」：POST 体
// session=目标会话数字 id（禁按 name——跨栏目同名不唯一，#24 恒无四头无项目上下文，
// id 直达零歧义）。全部请求不带身份四头（看板请求恒不带，§2.1 身份四头注）。

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// seedBoardSession 经看板可选豁免端点带四头心跳注册会话（B5-T1「带=隐式注册+刷
// 心跳 CLI 路径」语义本身即测试面），返回会话 id。失败 Fatal。
func seedBoardSession(t *testing.T, h http.Handler, st *store.Store, proj, col, sess, role string) int64 {
	t.Helper()
	rr := doHeartbeatReq(t, h, http.MethodGet, "/api/v1/status", proj, col, sess, role)
	if rr.Code != http.StatusOK {
		t.Fatalf("带四头心跳注册会话 %s@%s/%s 失败: status = %d（body: %s）",
			sess, proj, col, rr.Code, rr.Body.String())
	}
	var id int64
	if err := st.DB.QueryRow(
		`SELECT s.id FROM sessions s
		 JOIN projects p ON p.id = s.project_id
		 JOIN columns c ON c.id = s.column_id
		 WHERE p.code = ? AND c.code = ? AND s.name = ?`,
		proj, col, sess).Scan(&id); err != nil {
		t.Fatalf("查询注册会话 %s 失败: %v", sess, err)
	}
	return id
}

// seedBoardMessage 直插消息行（board 发送/CLI send --to-session 语义同构——
// kind/column_id/target 由调用方按 §3.2 表 5 归属规则算好传入），返回落库行。
func seedBoardMessage(t *testing.T, st *store.Store, in store.MessageInput) store.Message {
	t.Helper()
	m, err := st.InsertMessage(in)
	if err != nil {
		t.Fatalf("插入消息夹具失败: %v", err)
	}
	return m
}

// boardSend 无四头 POST #24（看板恒不带身份四头），返回状态码与响应映射。
func boardSend(t *testing.T, h http.Handler, jsonBody string) (int, map[string]any) {
	t.Helper()
	rr := doReq(t, h, http.MethodPost, "/api/v1/board/messages", []byte(jsonBody))
	return rr.Code, decodeMap(t, rr)
}

// boardGet 无四头 GET 看板读端点，返回状态码与响应映射。
func boardGet(t *testing.T, h http.Handler, target string) (int, map[string]any) {
	t.Helper()
	rr := doReq(t, h, http.MethodGet, target, nil)
	return rr.Code, decodeMap(t, rr)
}

// assertBoardMessages 取 data.messages 数组（缺失/形状错 Fatal）。
func assertBoardMessages(t *testing.T, m map[string]any) []any {
	t.Helper()
	data, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 data 对象: %v", m)
	}
	list, ok := data["messages"].([]any)
	if !ok {
		t.Fatalf("data.messages 非数组: %v", data)
	}
	return list
}

// ---- TestBoardSend：#24 落库五要素+审计同事务+错误面 ----

func TestBoardSend(t *testing.T) {
	h, st := newTestEnv(t)
	pid, cid := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	sessID := seedBoardSession(t, h, st, "p-a", "05", "executor-A", "executor")

	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	// ① 成功（缺 from）：无四头 POST → 201 {seq}；落库五要素逐字段断言。
	code, m := boardSend(t, h, `{"session":`+fmt.Sprint(sessID)+`,"body":"请检查构建日志"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST #24 status = %d, want 201（body: %v）", code, m)
	}
	data, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 data 对象: %v", m)
	}
	seq, _ := data["seq"].(float64)
	if seq <= 0 {
		t.Errorf("data.seq = %v, want 正数（FR5 服务端取号）", data["seq"])
	}
	msgs := queryBoardMessages(t, st)
	if len(msgs) != 1 {
		t.Fatalf("messages 行数 = %d, want 1", len(msgs))
	}
	got := msgs[0]
	if got.Kind != store.MessageKindChat {
		t.Errorf("kind = %q, want chat（§2.2 #24 服务端转 chat）", got.Kind)
	}
	if got.TargetSessionID != sessID {
		t.Errorf("target_session_id = %d, want %d（目标会话 id 直达）", got.TargetSessionID, sessID)
	}
	if got.Level != store.MessageLevelImportant {
		t.Errorf("level = %q, want important（§2.2 #24 恒 important）", got.Level)
	}
	if got.SenderLabel != "board-user" {
		t.Errorf("sender_label = %q, want board-user（缺 from 形态）", got.SenderLabel)
	}
	if got.SenderSessionID != 0 {
		t.Errorf("sender_session_id = %d, want 0（看板独立 sender 语义）", got.SenderSessionID)
	}
	if got.ColumnID != cid || got.ProjectID != pid {
		t.Errorf("column/project = (%d,%d), want 目标会话栏目 (%d,%d)（§3.2 表 5 chat 归属）",
			got.ColumnID, got.ProjectID, cid, pid)
	}
	if got.Body != "请检查构建日志" {
		t.Errorf("body = %q, want 原文", got.Body)
	}
	if got.CreatedAt != t0 {
		t.Errorf("created_at = %q, want 注入时钟 %q（AC5.4）", got.CreatedAt, t0)
	}

	// ② 审计 board.message 同事务：恰 1 行、弱关联对齐、session_id=0（系统动作）、
	//    detail JSON 快照含目标会话名。
	entries := queryAuditByAction(t, st, store.AuditBoardMessage)
	if len(entries) != 1 {
		t.Fatalf("board.message 审计行数 = %d, want 1（同事务落行）", len(entries))
	}
	e := entries[0]
	if e.ProjectID != pid || e.ColumnID != cid || e.SessionID != 0 {
		t.Errorf("审计弱关联 = (%d,%d,%d), want (%d,%d,0 系统动作)", e.ProjectID, e.ColumnID, e.SessionID, pid, cid)
	}
	if e.CreatedAt != t0 {
		t.Errorf("审计 created_at = %q, want %q", e.CreatedAt, t0)
	}
	detail := decodeAuditDetail(t, e)
	if detail["session"] != "executor-A" {
		t.Errorf("detail.session = %v, want executor-A", detail["session"])
	}
	// detail.seq=落库消息序号（总控 #14③：单事务 RETURNING 注入，审计与消息精确关联）
	if detail["seq"] != float64(got.Seq) {
		t.Errorf("detail.seq = %v, want %d（=落库 seq）", detail["seq"], got.Seq)
	}

	// ③ 带 from：sender_label=board-user:张三（§2.2 #24 两形态），detail.from 随行；
	//    from 纯空白按缺省形态（TrimSpace 口径）。
	code, _ = boardSend(t, h, `{"session":`+fmt.Sprint(sessID)+`,"body":"第二条","from":"张三"}`)
	if code != http.StatusCreated {
		t.Fatalf("带 from POST #24 status = %d, want 201", code)
	}
	msgs = queryBoardMessages(t, st)
	if last := msgs[len(msgs)-1]; last.SenderLabel != "board-user:张三" {
		t.Errorf("带 from sender_label = %q, want board-user:张三", last.SenderLabel)
	}
	detail = decodeAuditDetail(t, queryAuditByAction(t, st, store.AuditBoardMessage)[0]) // QueryAudit id DESC：[0]=最新（带 from 第二条）
	if detail["from"] != "张三" {
		t.Errorf("detail.from = %v, want 张三", detail["from"])
	}
	if wantSeq := msgs[len(msgs)-1].Seq; detail["seq"] != float64(wantSeq) { // msgs 按 seq 升序：末条=带 from 第二条
		t.Errorf("detail.seq = %v, want %d（带 from 行同注入落库 seq）", detail["seq"], wantSeq)
	}
	code, _ = boardSend(t, h, `{"session":`+fmt.Sprint(sessID)+`,"body":"第三条","from":"   "}`)
	if code != http.StatusCreated {
		t.Fatalf("from 空白 POST #24 status = %d, want 201", code)
	}
	msgs = queryBoardMessages(t, st)
	if last := msgs[len(msgs)-1]; last.SenderLabel != "board-user" {
		t.Errorf("from 纯空白 sender_label = %q, want board-user（空白按缺省形态）", last.SenderLabel)
	}

	// ④ 错误面（失败路径零落库/零审计——同事务原子性的失败侧证据）。
	before := len(queryBoardMessages(t, st))
	beforeAudit := len(queryAuditByAction(t, st, store.AuditBoardMessage))

	// 404 目标会话不存在（target_session_not_found 专属码，#9 chat 同款语义）
	wantErrBody(t, doReq(t, h, http.MethodPost, "/api/v1/board/messages",
		[]byte(`{"session":99999,"body":"x"}`)), http.StatusNotFound, types.CodeTargetSessionNotFound)
	// 413 body 超 4KB（§2.2 #24 消息级上限，body_too_large 复用既有码）
	wantErrBody(t, doReq(t, h, http.MethodPost, "/api/v1/board/messages",
		[]byte(`{"session":`+fmt.Sprint(sessID)+`,"body":"`+strings.Repeat("a", 4097)+`"}`)),
		http.StatusRequestEntityTooLarge, types.CodeBodyTooLarge)
	// 400 body 空（body_empty）；纯空白同判
	wantErrBody(t, doReq(t, h, http.MethodPost, "/api/v1/board/messages",
		[]byte(`{"session":`+fmt.Sprint(sessID)+`,"body":""}`)), http.StatusBadRequest, types.CodeBodyEmpty)
	wantErrBody(t, doReq(t, h, http.MethodPost, "/api/v1/board/messages",
		[]byte(`{"session":`+fmt.Sprint(sessID)+`,"body":"   "}`)), http.StatusBadRequest, types.CodeBodyEmpty)
	// 400 session 缺失/非正数（param_invalid——目标会话 id 必填）
	wantErrBody(t, doReq(t, h, http.MethodPost, "/api/v1/board/messages",
		[]byte(`{"body":"x"}`)), http.StatusBadRequest, types.CodeParamInvalid)
	wantErrBody(t, doReq(t, h, http.MethodPost, "/api/v1/board/messages",
		[]byte(`{"session":0,"body":"x"}`)), http.StatusBadRequest, types.CodeParamInvalid)
	// 400 非法 JSON（decodeJSONBody 公共面）
	wantErrBody(t, doReq(t, h, http.MethodPost, "/api/v1/board/messages",
		[]byte(`{`)), http.StatusBadRequest, types.CodeBadJSON)

	if after := len(queryBoardMessages(t, st)); after != before {
		t.Errorf("错误面后 messages 行数 = %d, want %d（失败路径零落库）", after, before)
	}
	if after := len(queryAuditByAction(t, st, store.AuditBoardMessage)); after != beforeAudit {
		t.Errorf("错误面后审计行数 = %d, want %d（失败路径零审计）", after, beforeAudit)
	}
}

// ---- TestBoardDialog：#25 全流+AC15.4 隔离+AC15.3 无已读字段+limit ----

func TestBoardDialog(t *testing.T) {
	h, st := newTestEnv(t)
	pid, cid := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	s1 := seedBoardSession(t, h, st, "p-a", "05", "executor-A", "executor")
	s2 := seedBoardSession(t, h, st, "p-a", "05", "executor-B", "executor")

	// 夹具：S1 双向流（board 发 + agent 经 CLI send --to-session S1 语义回复——
	// 两者均落 kind=chat/target_session_id=S1，from_board 判据=sender_session_id 是否 0）。
	boardMsg := seedBoardMessage(t, st, store.MessageInput{
		ProjectID: pid, ColumnID: cid,
		Kind: store.MessageKindChat, TargetSessionID: s1,
		SenderLabel: "board-user", Level: store.MessageLevelImportant, Body: "看板首问",
	})
	agentMsg := seedBoardMessage(t, st, store.MessageInput{
		ProjectID: pid, ColumnID: cid,
		Kind: store.MessageKindChat, TargetSessionID: s1, SenderSessionID: s2,
		SenderLabel: "executor-B@05", Level: store.MessageLevelNormal, Body: "agent 回复",
	})
	// S2 专属消息（board→S2）——AC15.4 隔离断言的判别物。
	seedBoardMessage(t, st, store.MessageInput{
		ProjectID: pid, ColumnID: cid,
		Kind: store.MessageKindChat, TargetSessionID: s2,
		SenderLabel: "board-user", Level: store.MessageLevelImportant, Body: "S2 专属",
	})

	// ① S1 全流：无四头 GET → 200，双向两条 seq 倒序（b3-W2 数据面换 Timeline，
	//    最新在前）、from_board 双向正确。
	code, m := boardGet(t, h, fmt.Sprintf("/api/v1/board/sessions/%d/dialog", s1))
	if code != http.StatusOK {
		t.Fatalf("GET #25 status = %d, want 200（body: %v）", code, m)
	}
	list := assertBoardMessages(t, m)
	if len(list) != 2 {
		t.Fatalf("S1 对话流条数 = %d, want 2（双向全流）", len(list))
	}
	first, _ := list[0].(map[string]any)  // 倒序首位=agent 回复（最新）
	second, _ := list[1].(map[string]any) // 倒序次位=board 首问（较早）
	if first["seq"] != float64(agentMsg.Seq) || second["seq"] != float64(boardMsg.Seq) {
		t.Errorf("seq 序 = [%v, %v], want 倒序 [%d, %d]",
			first["seq"], second["seq"], agentMsg.Seq, boardMsg.Seq)
	}
	if first["from_board"] != false {
		t.Errorf("agent 回复侧 from_board = %v, want false", first["from_board"])
	}
	if second["from_board"] != true {
		t.Errorf("board 发送侧 from_board = %v, want true（sender_session_id=0）", second["from_board"])
	}
	if first["body"] != "agent 回复" || second["body"] != "看板首问" {
		t.Errorf("body 回显 = [%v, %v], want 原文（倒序位）", first["body"], second["body"])
	}
	for i, row := range []map[string]any{first, second} {
		if s, _ := row["created_at"].(string); s == "" {
			t.Errorf("messages[%d].created_at 缺失（AC8.4 时间要素）", i)
		}
	}

	// ② AC15.4 会话隔离：S2 流只见自己 1 条；S1 不含 S2 专属消息（互不可见）。
	_, m2 := boardGet(t, h, fmt.Sprintf("/api/v1/board/sessions/%d/dialog", s2))
	list2 := assertBoardMessages(t, m2)
	if len(list2) != 1 {
		t.Fatalf("S2 对话流条数 = %d, want 1（AC15.4 互不可见）", len(list2))
	}
	row2, _ := list2[0].(map[string]any)
	if row2["body"] != "S2 专属" {
		t.Errorf("S2 流唯一消息 body = %v, want S2 专属（串流即隔离破防）", row2["body"])
	}

	// ③ 404 会话不存在（session_not_found——#25 消费视角码，与 #24 目标码分立）。
	wantErrBody(t, doReq(t, h, http.MethodGet, "/api/v1/board/sessions/9999/dialog", nil),
		http.StatusNotFound, types.CodeSessionNotFound)
	// 路径 id 非数字同判 404（形态错=资源不存在语义）
	wantErrBody(t, doReq(t, h, http.MethodGet, "/api/v1/board/sessions/abc/dialog", nil),
		http.StatusNotFound, types.CodeSessionNotFound)

	// ④ limit 默认 100：101 条取最新 100（seq DESC——Timeline 倒序截断）；?limit=2
	//    显式生效；非法 400。
	s3 := seedBoardSession(t, h, st, "p-a", "05", "executor-C", "executor")
	for i := 0; i < 101; i++ {
		seedBoardMessage(t, st, store.MessageInput{
			ProjectID: pid, ColumnID: cid,
			Kind: store.MessageKindChat, TargetSessionID: s3,
			SenderLabel: "board-user", Level: store.MessageLevelImportant, Body: fmt.Sprintf("m%03d", i),
		})
	}
	_, m3 := boardGet(t, h, fmt.Sprintf("/api/v1/board/sessions/%d/dialog", s3))
	if got := len(assertBoardMessages(t, m3)); got != 100 {
		t.Errorf("默认 limit 条数 = %d, want 100（§2.2 #25 默认）", got)
	}
	_, m4 := boardGet(t, h, fmt.Sprintf("/api/v1/board/sessions/%d/dialog?limit=2", s3))
	if got := len(assertBoardMessages(t, m4)); got != 2 {
		t.Errorf("?limit=2 条数 = %d, want 2", got)
	}
	wantErrBody(t, doReq(t, h, http.MethodGet,
		fmt.Sprintf("/api/v1/board/sessions/%d/dialog?limit=abc", s3), nil),
		http.StatusBadRequest, types.CodeParamInvalid)
	wantErrBody(t, doReq(t, h, http.MethodGet,
		fmt.Sprintf("/api/v1/board/sessions/%d/dialog?limit=-1", s3), nil),
		http.StatusBadRequest, types.CodeParamInvalid)

	// ⑤ AC15.3 结构判据：响应全文本无已读类字段子串；单条键集合恰
	//    {seq,body,from_board,created_at,kind,from}（b3-W2 扩两键后冻结面，多一字段
	//    即契约漂移）；顶层 positions{mailbox,dialog} 恒在且为数值——positions/kind/
	//    from 键名不含 banned 子串，已读语义检查自然通过（本批不引入已读语义）。
	raw := doReq(t, h, http.MethodGet, fmt.Sprintf("/api/v1/board/sessions/%d/dialog", s1), nil).Body.String()
	for _, banned := range []string{"read", "unread", "acked", "receipt"} {
		if strings.Contains(raw, banned) {
			t.Errorf("对话流响应含已读类字段子串 %q（AC15.3：无已读语义）", banned)
		}
	}
	keys := map[string]bool{}
	for k := range first {
		keys[k] = true
	}
	if len(keys) != 6 || !keys["seq"] || !keys["body"] || !keys["from_board"] ||
		!keys["created_at"] || !keys["kind"] || !keys["from"] {
		t.Errorf("单条消息键集合 = %v, want 恰 {seq,body,from_board,created_at,kind,from}", keys)
	}
	if data, ok := m["data"].(map[string]any); !ok {
		t.Fatalf("响应缺 data 对象: %v", m)
	} else if pos, ok := data["positions"].(map[string]any); !ok {
		t.Errorf("顶层 positions 缺失或非对象: %v（b3-W2 恒在键）", data["positions"])
	} else {
		for _, k := range []string{"mailbox", "dialog"} {
			if _, ok := pos[k].(float64); !ok {
				t.Errorf("positions.%s = %v, want 数值", k, pos[k])
			}
		}
	}
}

// ---- TestBoardDialogTimelineKeys：b3-W2 合流扩键（六键面+From 装配三分支+顶层 positions）----

// TestBoardDialogTimelineKeys b3-W2 T3：#25 数据面换 QuerySessionTimeline 后的
// 键面冻结——direct（controller 定投目标格角色）与 chat 双向（board 发 sender=0
// /agent 回复 sender≠0）三类形态各一进同一时间线，断言行键集合恰六键
// {seq,body,from_board,created_at,kind,from}、From 装配三分支（direct/chat agent
// 发=sender_label 非空、chat board 发="-"）、顶层 positions{mailbox,dialog} 恒在
// 且为数值。
func TestBoardDialogTimelineKeys(t *testing.T) {
	h, st := newTestEnv(t)
	pid, cid := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	exe := seedBoardSession(t, h, st, "p-a", "05", "executor-A", "executor")
	ctl := seedBoardSession(t, h, st, "p-a", "05", "controller-A", "controller")
	exe2 := seedBoardSession(t, h, st, "p-a", "05", "executor-B", "executor")

	// 夹具（插入序=seq 升序，时间线返回 seq DESC）：direct 到格（目标=本栏目
	// executor 角色——exe 会话所在信箱定向消息合流进其时间线，b3-W1 Q2 谓词）+
	// chat board 发（sender_session_id=0）+ chat agent 回复（sender=exe2）。
	seedBoardMessage(t, st, store.MessageInput{
		ProjectID: pid, ColumnID: cid,
		Kind: store.MessageKindDirect, TargetRole: "executor",
		SenderSessionID: ctl, SenderLabel: "controller-A@05",
		Level: store.MessageLevelNormal, Body: "direct 指令",
	})
	seedBoardMessage(t, st, store.MessageInput{
		ProjectID: pid, ColumnID: cid,
		Kind: store.MessageKindChat, TargetSessionID: exe,
		SenderLabel: "board-user", Level: store.MessageLevelImportant, Body: "board 问询",
	})
	agentMsg := seedBoardMessage(t, st, store.MessageInput{
		ProjectID: pid, ColumnID: cid,
		Kind: store.MessageKindChat, TargetSessionID: exe, SenderSessionID: exe2,
		SenderLabel: "executor-B@05", Level: store.MessageLevelNormal, Body: "agent 回复",
	})

	code, m := boardGet(t, h, fmt.Sprintf("/api/v1/board/sessions/%d/dialog", exe))
	if code != http.StatusOK {
		t.Fatalf("GET #25 status = %d, want 200（body: %v）", code, m)
	}
	list := assertBoardMessages(t, m)
	if len(list) != 3 {
		t.Fatalf("合流时间线条数 = %d, want 3（direct+chat 双向）", len(list))
	}

	// ① 行键集合恰六键（b3-W2 冻结扩面——多一字段即契约漂移）。
	for i, row := range list {
		r, _ := row.(map[string]any)
		keys := map[string]bool{}
		for k := range r {
			keys[k] = true
		}
		if len(keys) != 6 || !keys["seq"] || !keys["body"] || !keys["from_board"] ||
			!keys["created_at"] || !keys["kind"] || !keys["from"] {
			t.Errorf("messages[%d] 键集合 = %v, want 恰 {seq,body,from_board,created_at,kind,from}", i, keys)
		}
	}

	// ② seq DESC（最新在前）+ From 装配三分支：agent 回复=sender_label、board 发
	//    （sender_session_id=0）="-"、direct=发送方 sender_label 非空（会话@栏目形）。
	first, _ := list[0].(map[string]any)
	if first["seq"] != float64(agentMsg.Seq) {
		t.Errorf("首条 seq = %v, want %d（seq DESC 最新在前）", first["seq"], agentMsg.Seq)
	}
	if first["kind"] != store.MessageKindChat || first["from"] != "executor-B@05" {
		t.Errorf("agent 回复 kind/from = (%v,%v), want (chat,executor-B@05)", first["kind"], first["from"])
	}
	second, _ := list[1].(map[string]any)
	if second["kind"] != store.MessageKindChat || second["from"] != "-" {
		t.Errorf("board 发 kind/from = (%v,%v), want (chat,-)", second["kind"], second["from"])
	}
	third, _ := list[2].(map[string]any)
	if third["kind"] != store.MessageKindDirect {
		t.Errorf("direct 条目 kind = %v, want direct 原值直出", third["kind"])
	}
	if from, _ := third["from"].(string); from != "controller-A@05" {
		t.Errorf("direct 条目 from = %v, want 发送方 sender_label controller-A@05（非空）", third["from"])
	}

	// ③ 顶层 positions{mailbox,dialog} 恒在且为数值（双维度消费位点透出，b3-W2）。
	data, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 data 对象: %v", m)
	}
	pos, ok := data["positions"].(map[string]any)
	if !ok {
		t.Fatalf("data.positions 缺失或非对象: %v", data["positions"])
	}
	for _, k := range []string{"mailbox", "dialog"} {
		if _, ok := pos[k].(float64); !ok {
			t.Errorf("positions.%s = %v, want 数值", k, pos[k])
		}
	}
}

// ---- TestBusStream：#26 倒序+level 齐+preview 截断+非 bus 不混入+limit ----

func TestBusStream(t *testing.T) {
	h, st := newTestEnv(t)
	pid, cid := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	longBody := strings.Repeat("长", 90) // 90 rune > 80：触发 preview 截断（B5-T3）
	busRows := []struct {
		level string
		body  string
	}{
		{store.MessageLevelNormal, "bus 一号"},
		{store.MessageLevelImportant, longBody},
		{store.MessageLevelBlock, "bus 三号"},
	}
	var busSeqs []int64
	for _, b := range busRows {
		m := seedBoardMessage(t, st, store.MessageInput{
			ProjectID: pid, ColumnID: cid, Kind: store.MessageKindBus,
			SenderLabel: "controller-A@05", Level: b.level, Body: b.body,
		})
		busSeqs = append(busSeqs, m.Seq)
	}
	// 干扰消息：direct/chat 各一——非 bus kind 不得混入。
	seedBoardMessage(t, st, store.MessageInput{
		ProjectID: pid, ColumnID: cid, Kind: store.MessageKindDirect, TargetRole: "executor",
		SenderLabel: "controller-A@05", Level: store.MessageLevelNormal, Body: "direct 干扰",
	})
	seedBoardMessage(t, st, store.MessageInput{
		ProjectID: pid, ColumnID: cid, Kind: store.MessageKindChat,
		SenderLabel: "board-user", Level: store.MessageLevelImportant, Body: "chat 干扰",
	})

	// ① 无四头 GET → 200；倒序+level 齐+sender/created_at/body_preview 键齐。
	code, m := boardGet(t, h, "/api/v1/board/bus-stream")
	if code != http.StatusOK {
		t.Fatalf("GET #26 status = %d, want 200（body: %v）", code, m)
	}
	list := assertBoardMessages(t, m)
	if len(list) != 3 {
		t.Fatalf("总线流条数 = %d, want 3（非 bus kind 不混入）", len(list))
	}
	prev := int64(-1)
	for i, row := range list {
		r, _ := row.(map[string]any)
		seq, _ := r["seq"].(float64)
		if prev >= 0 && int64(seq) >= prev {
			t.Errorf("messages[%d].seq = %d 不严格小于前条 %d（AC14.3 倒序）", i, int64(seq), prev)
		}
		prev = int64(seq)
		level, _ := r["level"].(string)
		if level != store.MessageLevelNormal && level != store.MessageLevelImportant && level != store.MessageLevelBlock {
			t.Errorf("messages[%d].level = %q 非法枚举", i, level)
		}
		if r["sender"] == nil || r["created_at"] == nil || r["body_preview"] == nil {
			t.Errorf("messages[%d] 缺 sender/created_at/body_preview 键（§2.2 #26 字段面）", i)
		}
	}
	if first, _ := list[0].(map[string]any); first["seq"] != float64(busSeqs[2]) {
		t.Errorf("最新一条 seq = %v, want %d（倒序首位）", first["seq"], busSeqs[2])
	}

	// ② preview 截断（B5-T3：rune 80 + "..."，中文不截半字）；≤80 原文。
	mid, _ := list[1].(map[string]any) // longBody 所在（倒序第二条）
	wantPreview := strings.Repeat("长", 80) + "..."
	if mid["body_preview"] != wantPreview {
		t.Errorf("长消息 preview = %d rune, want 83（80 rune + ...）",
			len([]rune(mid["body_preview"].(string))))
	}
	if got, _ := list[0].(map[string]any); got["body_preview"] != "bus 三号" {
		t.Errorf("短消息 preview = %v, want 原文「bus 三号」", got["body_preview"])
	}

	// ③ limit 默认 50：再插 51 条（共 54）默认取最新 50；?limit=2 显式生效；非法 400。
	for i := 0; i < 51; i++ {
		seedBoardMessage(t, st, store.MessageInput{
			ProjectID: pid, ColumnID: cid, Kind: store.MessageKindBus,
			SenderLabel: "controller-A@05", Level: store.MessageLevelNormal, Body: fmt.Sprintf("b%03d", i),
		})
	}
	_, mAll := boardGet(t, h, "/api/v1/board/bus-stream")
	if got := len(assertBoardMessages(t, mAll)); got != 50 {
		t.Errorf("默认 limit 条数 = %d, want 50（§2.2 #26 默认）", got)
	}
	_, mTwo := boardGet(t, h, "/api/v1/board/bus-stream?limit=2")
	if got := len(assertBoardMessages(t, mTwo)); got != 2 {
		t.Errorf("?limit=2 条数 = %d, want 2", got)
	}
	for _, bad := range []string{"limit=0", "limit=-3", "limit=xyz"} {
		wantErrBody(t, doReq(t, h, http.MethodGet, "/api/v1/board/bus-stream?"+bad, nil),
			http.StatusBadRequest, types.CodeParamInvalid)
	}
}

// ---- TestHeaderExempt：B5-T1 豁免面（六端点可选/#24 恒豁免/强制面零回归） ----

func TestHeaderExempt(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	// ① 无四头 → 200 且 sessions 零新增（缺头放行不 upsert——看板路径）。
	for _, target := range []string{
		"/api/v1/status",           // #17
		"/api/v1/resources",        // #20
		"/api/v1/windows/now",      // #22
		"/api/v1/board/bus-stream", // #26
		"/api/v1/sessions",         // #27
	} {
		rr := doHeartbeatReq(t, h, http.MethodGet, target, "", "", "", "")
		if rr.Code != http.StatusOK {
			t.Errorf("无四头 GET %s status = %d, want 200（B5-T1 可选豁免；body: %s）",
				target, rr.Code, rr.Body.String())
		}
	}
	if n := countTable(t, st, "sessions"); n != 0 {
		t.Errorf("无四头遍历后 sessions 行数 = %d, want 0（缺头放行不 upsert）", n)
	}

	// ② 带四头 → upsert 照常（CLI 路径不回归）：#27 带四头注册会话，再无四头打 #25。
	sessID := seedBoardSession(t, h, st, "p-a", "05", "executor-A", "executor")
	if n := countTable(t, st, "sessions"); n != 1 {
		t.Fatalf("带四头心跳后 sessions 行数 = %d, want 1（带=完整心跳链）", n)
	}
	rr := doHeartbeatReq(t, h, http.MethodGet,
		fmt.Sprintf("/api/v1/board/sessions/%d/dialog", sessID), "", "", "", "")
	if rr.Code != http.StatusOK {
		t.Errorf("无四头 GET #25 status = %d, want 200（动态段端点可选豁免；body: %s）",
			rr.Code, rr.Body.String())
	}
	// 带四头打 #25 同样走完整心跳链（upsert 幂等不膨胀）。
	rr = doHeartbeatReq(t, h, http.MethodGet,
		fmt.Sprintf("/api/v1/board/sessions/%d/dialog", sessID), "p-a", "05", "executor-A", "executor")
	if rr.Code != http.StatusOK {
		t.Errorf("带四头 GET #25 status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
	}
	if n := countTable(t, st, "sessions"); n != 1 {
		t.Errorf("带四头 #25 后 sessions 行数 = %d, want 1（upsert 幂等）", n)
	}

	// ③ #24 恒无四头可用（恒豁免=不走心跳链，sessions 零新增）。
	code, m := boardSend(t, h, `{"session":`+fmt.Sprint(sessID)+`,"body":"豁免面探针"}`)
	if code != http.StatusCreated {
		t.Fatalf("无四头 POST #24 status = %d, want 201（恒豁免；body: %v）", code, m)
	}
	if n := countTable(t, st, "sessions"); n != 1 {
		t.Errorf("#24 恒豁免后 sessions 行数 = %d, want 1（恒豁免不 upsert）", n)
	}

	// ④ 非白名单端点缺四头照旧 400 missing_header（B1 强制面零回归）。
	//    注：POST #1（/api/v1/projects）缺头豁免是 B2 自举裁定既有行为
	//    （TestProjectRegisterExemptMissingHeaders 锚定 201），不作强制面代表。
	for _, tc := range []struct{ name, method, target string }{
		{"POST 消息域强制", http.MethodPost, "/api/v1/messages"},
		{"GET 信箱强制", http.MethodGet, "/api/v1/mailbox"},
		{"GET 项目列表强制", http.MethodGet, "/api/v1/projects"},
	} {
		rr := doHeartbeatReq(t, h, tc.method, tc.target, "", "", "", "")
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeMissingHeader)
	}

	// ⑤ ping/version 豁免不回归。
	for _, target := range []string{"/api/v1/ping", "/api/v1/version"} {
		if rr := doHeartbeatReq(t, h, http.MethodGet, target, "", "", "", ""); rr.Code != http.StatusOK {
			t.Errorf("无四头 GET %s status = %d, want 200（既有豁免回归）", target, rr.Code)
		}
	}

	// ⑥ 豁免不外溢：精确/段形态匹配外的近邻路径缺头照旧 400（表勿前缀化、
	//    动态段判定勿宽匹配）。
	for _, target := range []string{
		"/api/v1/status/",                                    // 尾斜杠近邻
		"/api/v1/board/sessions/",                            // #25 缺 id 段
		"/api/v1/board/sessions/" + fmt.Sprint(sessID) + "/", // #25 尾多斜杠
		"/api/v1/board/sessions/x/dialog/y",                  // #25 深层
	} {
		rr := doHeartbeatReq(t, h, http.MethodGet, target, "", "", "", "")
		wantErrBody(t, rr, http.StatusBadRequest, types.CodeMissingHeader)
	}

	// ⑦ #24 豁免按方法+路径精确：GET 同路径缺头不豁免（强制面 400 前置于 405，
	//    同 TestProjectRegisterExemptNotLeaky「同路径其余方法」口径）。
	wantErrBody(t, doHeartbeatReq(t, h, http.MethodGet, "/api/v1/board/messages", "", "", "", ""),
		http.StatusBadRequest, types.CodeMissingHeader)
}

// queryBoardMessages 拉全部消息行（seq 序，落库断言面）。
func queryBoardMessages(t *testing.T, st *store.Store) []store.Message {
	t.Helper()
	rows, err := st.DB.Query(`SELECT seq, project_id, column_id, kind, target_role,
		 target_session_id, sender_session_id, sender_label, level, body, created_at
		 FROM messages ORDER BY seq`)
	if err != nil {
		t.Fatalf("查询 messages 失败: %v", err)
	}
	defer rows.Close()
	out := make([]store.Message, 0)
	for rows.Next() {
		var msg store.Message
		if err := rows.Scan(&msg.Seq, &msg.ProjectID, &msg.ColumnID, &msg.Kind, &msg.TargetRole,
			&msg.TargetSessionID, &msg.SenderSessionID, &msg.SenderLabel, &msg.Level, &msg.Body, &msg.CreatedAt); err != nil {
			t.Fatalf("扫描 messages 行失败: %v", err)
		}
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 messages 失败: %v", err)
	}
	return out
}
