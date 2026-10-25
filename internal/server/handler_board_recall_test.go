package server

// b5-T4 看板墓碑行测试（b5-spec R5/AC3②③，M-36）：三看板数据端点（#13 历史
// 时间线/#25 会话对话流/#26 总线流）对墓碑 seq 带 recalled:true 标记且 body
// 服务端剥离（占位行文案归前端装配，wire 恒空串）；未撤回消息键面/正文零漂移
// ——#13 七键（§2.2 #13）与 #25 六键（b3-W2 冻结面）靠 recalled 的 omitempty
// 加档保持非墓碑行键数不变（b5-W3 progress 可选键同款先例）。

import (
	"fmt"
	"net/http"
	"testing"

	"aiteam/internal/store"
)

// findRowBySeq 按 seq 取 messages 数组元素（形状错 Fatal）。
func findRowBySeq(t *testing.T, rows []any, seq int64) map[string]any {
	t.Helper()
	for _, r := range rows {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("messages 元素非对象: %v", r)
		}
		if int64(row["seq"].(float64)) == seq {
			return row
		}
	}
	t.Fatalf("messages 中未找到 seq=%d 的行: %v", seq, rows)
	return nil
}

// TestBoardTombstoneWireMarkers 三端点墓碑标记+body 剥离+正常面零漂移：
//  1. 造数：controller-A 经 #9 发两条 direct 到 executor-B 格，#recall 撤第一条；
//     #24 发一条 board chat 到 executor-B（看板身份不可撤，恒正常行）；
//  2. #13：墓碑行 recalled=true+body 剥空；保留行七键面不变、无 recalled 键；
//  3. #25：executor-B 时间线合流面内墓碑 direct 行同上；board chat 行六键不变；
//  4. #26：bus v1 不可撤（R1.2 排除面），正常行五键面不变（recalled 键不出现）
//     ——统一读面扩档对墓碑不可达分支零扰动。
func TestBoardTombstoneWireMarkers(t *testing.T) {
	h, st := newTestEnv(t)
	pid, cid := seedProjectColumn(t, st, authProj, store.ProjectStatusActive, authCol, store.ColumnStatusActive)
	exeID := seedBoardSession(t, h, st, authProj, authCol, "executor-B", "executor")
	const t0 = "2026-01-01T08:00:00Z"
	injectFixedClock(t, t0)

	// ① 造数+撤回（recall 走 T2 端点全链路，发送方=authed controller-A）。
	seqRecall := sendDirect(t, h, "executor", store.MessageLevelNormal, "已撤回正文")
	seqKept := sendDirect(t, h, "executor", store.MessageLevelNormal, "保留正文")
	rr := doReq(t, h, http.MethodPost, "/api/v1/board/messages",
		[]byte(fmt.Sprintf(`{"session":%d,"body":"看板正常消息"}`, exeID)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST #24 status = %d，期望 201（body: %s）", rr.Code, rr.Body.String())
	}
	if recallRR := doAuthedReq(t, h, http.MethodPost, recallURL(seqRecall), `{}`, heads4()...); recallRR.Code != http.StatusOK {
		t.Fatalf("POST recall seq=%d status = %d，期望 200（body: %s）", seqRecall, recallRR.Code, recallRR.Body.String())
	}

	// ② #13 历史时间线（消息域端点走四头心跳链非看板豁免面；升序便于按下标配对）。
	rr13 := doAuthedReq(t, h, http.MethodGet,
		"/api/v1/messages?project="+authProj+"&kind=direct&order=asc", "", heads4()...)
	data13 := wantData(t, rr13, http.StatusOK)
	rows, ok := data13["messages"].([]any)
	if !ok {
		t.Fatalf("#13 data.messages 非数组: %v", data13)
	}
	if len(rows) != 2 {
		t.Fatalf("#13 direct 行数 = %d，期望 2（墓碑行留在观测面只标记不删）", len(rows))
	}
	tomb := findRowBySeq(t, rows, seqRecall)
	if tomb["recalled"] != true {
		t.Errorf("#13 墓碑行 seq=%d recalled = %v，期望 true（AC3②）", seqRecall, tomb["recalled"])
	}
	if tomb["body"] != "" {
		t.Errorf("#13 墓碑行 seq=%d body = %q，期望空串（服务端剥离硬闸——占位文案归前端）", seqRecall, tomb["body"])
	}
	kept := findRowBySeq(t, rows, seqKept)
	if kept["body"] != "保留正文" {
		t.Errorf("#13 保留行 seq=%d body = %q，期望原文（正常消息零漂移）", seqKept, kept["body"])
	}
	if _, has := kept["recalled"]; has {
		t.Errorf("#13 保留行不应出现 recalled 键（omitempty 加档，七键冻结面零漂移）")
	}
	if len(kept) != 7 {
		t.Errorf("#13 保留行键数 = %d，期望恰 7（§2.2 #13 {seq,kind,level,body,sender,receipt,created_at}）", len(kept))
	}

	// ③ #25 会话对话流（executor-B 时间线：2 direct+1 board chat 合流）。
	code25, m25 := boardGet(t, h, fmt.Sprintf("/api/v1/board/sessions/%d/dialog", exeID))
	if code25 != http.StatusOK {
		t.Fatalf("GET #25 status = %d（body: %v）", code25, m25)
	}
	rows = assertBoardMessages(t, m25)
	if len(rows) != 3 {
		t.Fatalf("#25 行数 = %d，期望 3（chat+2 direct 合流）", len(rows))
	}
	tomb = findRowBySeq(t, rows, seqRecall)
	if tomb["kind"] != "direct" {
		t.Fatalf("#25 墓碑行 kind = %v，期望 direct（夹具错位）", tomb["kind"])
	}
	if tomb["recalled"] != true {
		t.Errorf("#25 墓碑行 seq=%d recalled = %v，期望 true（AC3③）", seqRecall, tomb["recalled"])
	}
	if tomb["body"] != "" {
		t.Errorf("#25 墓碑行 seq=%d body = %q，期望空串（服务端剥离硬闸）", seqRecall, tomb["body"])
	}
	chatRow := findRowBySeq(t, rows, seqOfBody(t, rows, "看板正常消息"))
	if chatRow["body"] != "看板正常消息" {
		t.Errorf("#25 board chat 行 body = %q，期望原文（正常消息零漂移）", chatRow["body"])
	}
	if _, has := chatRow["recalled"]; has {
		t.Errorf("#25 正常行不应出现 recalled 键（omitempty 加档，六键冻结面零漂移）")
	}
	if len(chatRow) != 6 {
		t.Errorf("#25 board chat 行键数 = %d，期望恰 6（b3-W2 {seq,body,from_board,kind,from,created_at}）", len(chatRow))
	}

	// ④ #26 总线流：bus 行正常面零漂移（v1 不可撤，recalled 键恒不出现）。
	seedBoardMessage(t, st, store.MessageInput{
		ProjectID:       pid,
		ColumnID:        cid,
		Kind:            store.MessageKindBus,
		SenderSessionID: 0,
		SenderLabel:     "controller-A@05",
		Level:           store.MessageLevelNormal,
		Body:            "总线正常广播",
	})
	code26, m26 := boardGet(t, h, "/api/v1/board/bus-stream")
	if code26 != http.StatusOK {
		t.Fatalf("GET #26 status = %d（body: %v）", code26, m26)
	}
	rows = assertBoardMessages(t, m26)
	if len(rows) != 1 {
		t.Fatalf("#26 行数 = %d，期望 1", len(rows))
	}
	busRow := rows[0].(map[string]any)
	if busRow["body_preview"] != "总线正常广播" {
		t.Errorf("#26 bus 行 body_preview = %q，期望原文（正常消息零漂移）", busRow["body_preview"])
	}
	if _, has := busRow["recalled"]; has {
		t.Errorf("#26 bus 行不应出现 recalled 键（v1 不可撤，五键冻结面零漂移）")
	}
	if len(busRow) != 5 {
		t.Errorf("#26 bus 行键数 = %d，期望恰 5（§2.2 #26）", len(busRow))
	}
}

// seqOfBody 按正文取 seq（#25 行配对辅助）。
func seqOfBody(t *testing.T, rows []any, body string) int64 {
	t.Helper()
	for _, r := range rows {
		row := r.(map[string]any)
		if row["body"] == body {
			return int64(row["seq"].(float64))
		}
	}
	t.Fatalf("rows 中未找到 body=%q 的行", body)
	return 0
}
