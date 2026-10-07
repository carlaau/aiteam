// handler_board.go —— B5-1 看板域三端点（§2.2 #24/#25/#26，b5-plan B5-1；一域
// 一文件 §4.8）：#24 看板唯一写口（AC14.5 白名单写口只此一个）+ #25 对话流
// （AC15 全家数据面）+ #26 总线流薄壳（AC14.3）。
//
// 三端点均不看身份四头：#24 恒豁免（boardAlwaysExemptPaths——恒不带四头），
// #25/#26 可选豁免（boardOptionalHeaderPaths——无四头直接放行，handler 不消费
// session context）。#26 store 层复用 #13 QueryHistory 同一实现（禁第二份 bus
// 谓词，b5-spec §2.2 #26/base.md §3.4 双胞胎纪律）。
package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// maxBoardBodyBytes #24 消息正文上限 4KB（§2.2 #24「body(必填,≤4KB)」；1MB 整包
// 防线由 bodyLimit 中间件先行，此处为看板消息级语义校验——前端 DIALOG_BODY_MAX_BYTES
// 预检同口径，服务端 413 是兜底非首道闸）。
const maxBoardBodyBytes = 4096

// boardPreviewRunes #26 body_preview 截断长度（B5-T3 裁定：按 rune 截 80+"..."，
// 中文不截半字；全文走对话/历史口）。
const boardPreviewRunes = 80

// ---- #24 POST /api/v1/board/messages（看板唯一写口） ----

// boardSendRequest #24 请求体（§2.2 #24 三字段）：session=目标会话数字 id
// （b5-spec §1.1「对话目标定位（2026-10-03 总控裁定注记）」——按唯一 id 定位禁按
// name：跨栏目同名会话 name 不唯一，#24 恒无四头无项目上下文，id 直达零歧义）；
// body 必填 ≤4KB；from 可选显示名（可缺省——看板全页唯一表单 AC14.5 不设第二
// 输入控件）。
type boardSendRequest struct {
	Session int64  `json:"session"`
	Body    string `json:"body"`
	From    string `json:"from"`
}

// boardSendData #24 响应 data（§2.2 #24「响应 {seq}」）。
type boardSendData struct {
	Seq int64 `json:"seq"`
}

// boardMessageAudit board.message 审计 detail 快照：session=目标会话名、from=
// 显示名（可省）、seq=落库消息序号（总控 #14③——seq 服务端取号，经
// InsertBoardMessageWithAudit 的 detailFn 事务内注入，审计行与 messages 行精确
// 关联，免按时间窗猜测）。body 不入审计（≤4KB 文本膨胀审计行，全文本已在
// messages 表可溯——审计定位「谁在何时向谁发过」而非内容副本）。
type boardMessageAudit struct {
	Session string `json:"session"`
	From    string `json:"from,omitempty"`
	Seq     int64  `json:"seq"`
}

// handleBoardSend 端点 #24（§2.2 #24、AC15.1）：请求面校验 → 目标会话 id 直达
// 定位（404 target_session_not_found）→ 落库 chat 消息（恒 important 级、
// sender_label=board-user[:from]、column_id=目标会话所属栏目）+ 同事务审计
// board.message（AC1.4；看板无会话身份 session_id=0 系统动作）。恒不走心跳链
// （boardAlwaysExemptPaths）——sessionIDFromCtx 恒 0 与本 handler 无关。
//
// DB 往返（b5-spec §七）：1 SELECT 目标会话+单事务 2 INSERT（messages+audit）
// ——固定 3。
func (s *Server) handleBoardSend(w http.ResponseWriter, r *http.Request) {
	// ① 请求面校验（§2.2 #24）：bad_json/1MB 由 decodeJSONBody → session 正整数
	//    → body 非空 → body 4KB，全过才做域校验读（无效请求零读往返）。
	var req boardSendRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Session <= 0 {
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
			"session 须为目标会话 id（正整数）")
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		types.WriteError(w, http.StatusBadRequest, types.CodeBodyEmpty, "消息正文为空（body 必填）")
		return
	}
	if len(req.Body) > maxBoardBodyBytes {
		types.WriteError(w, http.StatusRequestEntityTooLarge, types.CodeBodyTooLarge,
			fmt.Sprintf("消息正文超过 4KB 上限（%d 字节）", len(req.Body)))
		return
	}

	// ② 目标会话定位（id 直达，裁定注记口径）：GetSession 筛 active 项目——
	//    archived 项目域读不可见（看板②区数据源 #17 同口径，用户无从选中），
	//    404 即存在性语义（§2.2 #24 未定义 409 归档拒绝面，不加戏）。
	target, err := s.st.GetSession(req.Session, types.NowUTC(), s.cfg.Session.HeartbeatTimeoutSec)
	if errors.Is(err, store.ErrSessionNotFound) {
		types.WriteError(w, http.StatusNotFound, types.CodeTargetSessionNotFound,
			fmt.Sprintf("目标会话不存在: id=%d", req.Session))
		return
	}
	if err != nil {
		s.writeStoreError(w, err) // 非「不存在」类错误 → writeStoreError/500 兜底
		return
	}

	// ③ sender_label 两形态（§2.2 #24：board-user / board-user:<from>）；from
	//    纯空白按缺省形态（TrimSpace 与 #9 senderLabel 同口径）。
	from := strings.TrimSpace(req.From)
	label := "board-user"
	if from != "" {
		label += ":" + from
	}

	// ④ 落库+审计单事务（§2.2 #24；chat 归属=目标会话栏目，§3.2 表 5 注——
	//    column_id/target_session_id 由 handler 按归属规则算好传入，store 纯落库）。
	m, err := s.st.InsertBoardMessageWithAudit(
		store.MessageInput{
			ProjectID:       target.ProjectID,
			ColumnID:        target.ColumnID,
			Kind:            store.MessageKindChat,
			TargetSessionID: target.ID,
			SenderSessionID: 0, // 看板独立 sender（弱关联 0=系统/看板）
			SenderLabel:     label,
			Level:           store.MessageLevelImportant, // 恒 important（§2.2 #24）
			Body:            req.Body,
		},
		store.AuditEntry{
			ProjectID: target.ProjectID,
			ColumnID:  target.ColumnID,
			SessionID: 0, // 系统动作（§3.2 表 10 注：0 直插合法）
			Action:    store.AuditBoardMessage,
			// Detail 事务内构造（detailFn 注入落库 seq，零额外往返）
		},
		func(seq int64) string {
			return auditDetailJSON(boardMessageAudit{Session: target.Name, From: from, Seq: seq})
		},
	)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	types.WriteData(w, http.StatusCreated, boardSendData{Seq: m.Seq})
}

// ---- #25 GET /api/v1/board/sessions/{id}/dialog（对话流） ----

// dialogMessageRow #25 单条（b3-W2 六字段面：恰四字段原面+kind/from 两扩键——
// 数据面换 QuerySessionTimeline 合流后 direct 条目也进流，kind 原值直出区分流别；
// from=发送方 sender_label 冗余显示，board 发 sender_session_id=0 无会话身份 →
// "-"。无已读字段——AC15.3 结构判据不变（positions/kind/from 键名不含已读类
// 子串，本批不引入已读语义；六键外多一字段即契约漂移）。
type dialogMessageRow struct {
	Seq       int64  `json:"seq"`
	Body      string `json:"body"`
	FromBoard bool   `json:"from_board"` // 是否看板侧发出（sender_session_id=0，§2.2 #25；chat 专用语义，b3-W2 装配式不动）
	Kind      string `json:"kind"`       // 消息流别原值直出（chat/direct，合流面）
	From      string `json:"from"`       // 发送方 sender_label（direct/chat agent 发）；board 发（sender_session_id=0）="-"
	CreatedAt string `json:"created_at"`
}

// boardPositions #25 顶层双维度消费位点（b3-W2 扩键）：GetPositions 原值透出——
// mailbox=信箱位（consumer=角色）、dialog=对话位（consumer=chat:session:<id>）。
// 位点≠已读状态（无已读语义，AC15.3 口径不涉）。
type boardPositions struct {
	Mailbox int64 `json:"mailbox"`
	Dialog  int64 `json:"dialog"`
}

// boardDialogData #25 响应 data（b3-W2 增顶层 positions）。
type boardDialogData struct {
	Messages  []dialogMessageRow `json:"messages"`
	Positions boardPositions     `json:"positions"`
}

// handleBoardDialog 端点 #25（§2.2 #25、AC15.2/15.3/15.4；b3-W2 数据面换 Timeline）：
// 会话存在校验（404 session_not_found 消费视角码，与 #24 target_session_not_found
// 专属码分立）→ 会话时间线合流视图（store.QuerySessionTimeline——chat 目标会话
// 命中与 direct 到格（栏目,角色）命中两路 OR 合一，seq DESC 最新在前；board 发送
// 与 agent 回复两端都展示，与 poll「自发不回流」的拉取通道语义不冲突，spec §2.2
// #25 明文）+ 双维度消费位点透出（GetPositions 复用——信箱位缺行惰性落行一次后
// 幂等、对话位读路径零写，B2-T3 已核，勿改 store）。
//
// DB 往返（b5-spec §七，b3-W2 随扩键基线+1）：1 存在校验+1 SELECT LIMIT+1 位点读
// （+信箱位首次惰性 upsert 冷路径，落行一次后回归 3——poll 同款随注口径）——固定
// 2+1=3。
func (s *Server) handleBoardDialog(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseBoardLimit(w, r)
	if !ok {
		return
	}
	// ① 存在校验：PathValue id 非数字=资源不存在语义归 404（路径定位符形态错与
	//    不存在同判——对齐「不存在不泄露更多状态」口径）。
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		types.WriteError(w, http.StatusNotFound, types.CodeSessionNotFound, "会话不存在")
		return
	}
	// GetSession 筛 active 项目（与 #17/#27 会话面同口径——archived 项目会话不可
	// 见即不可对话）；b3-W2 起接住返回 Session——ColumnID/Role 供 Timeline direct
	// 分支到格谓词、ProjectID/ID 供位点读键；失联计算列本端点仍不消费。
	sess, err := s.st.GetSession(id, types.NowUTC(), s.cfg.Session.HeartbeatTimeoutSec)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			types.WriteError(w, http.StatusNotFound, types.CodeSessionNotFound,
				fmt.Sprintf("会话不存在: id=%d", id))
			return
		}
		s.writeStoreError(w, err)
		return
	}
	msgs, err := s.st.QuerySessionTimeline(id, sess.ColumnID, sess.Role, limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	pos, err := s.st.GetPositions(sess.ProjectID, sess.ColumnID, sess.Role, sess.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	rows := make([]dialogMessageRow, 0, len(msgs))
	for _, msg := range msgs {
		// From 装配（b3-W2 冻结）：发送方 sender_label 直出；sender_session_id=0
		// （board 发送，弱关联 0=系统/看板）无会话身份 → "-"。
		from := msg.SenderLabel
		if msg.SenderSessionID == 0 {
			from = "-"
		}
		rows = append(rows, dialogMessageRow{
			Seq:       msg.Seq,
			Body:      msg.Body,
			FromBoard: msg.SenderSessionID == 0,
			Kind:      msg.Kind,
			From:      from,
			CreatedAt: msg.CreatedAt,
		})
	}
	types.WriteData(w, http.StatusOK, boardDialogData{
		Messages:  rows,
		Positions: boardPositions{Mailbox: pos.Mailbox, Dialog: pos.Dialog},
	})
}

// ---- #26 GET /api/v1/board/bus-stream（总线流薄壳） ----

// busMessageRow #26 单条（§2.2 #26 五字段；body_preview 截断 B5-T3）。
type busMessageRow struct {
	Seq         int64  `json:"seq"`
	Level       string `json:"level"`
	Sender      string `json:"sender"` // sender_label 冗余显示直出（AC8.4 发送方要素）
	CreatedAt   string `json:"created_at"`
	BodyPreview string `json:"body_preview"`
}

// boardBusStreamData #26 响应 data。
type boardBusStreamData struct {
	Messages []busMessageRow `json:"messages"`
}

// handleBoardBusStream 端点 #26（§2.2 #26、AC14.3）：#13 历史查询的 bus 专用薄壳
// ——store 层复用 QueryHistory 同一实现（kind='bus' 过滤+seq DESC+LIMIT 默认 50
// 即 DefaultHistoryLimit，禁另写第二份谓词），本 handler 只做 limit 解析与
// preview 装配。
//
// DB 往返（b5-spec §七）：1 SELECT LIMIT——固定 1。
func (s *Server) handleBoardBusStream(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseBoardLimit(w, r)
	if !ok {
		return
	}
	msgs, err := s.st.QueryHistory(store.HistoryFilter{
		Kind:  store.MessageKindBus,
		Order: "desc", // AC14.3 时间倒序（最新在前）
		Limit: limit,  // 0=store 默认 DefaultHistoryLimit(50)
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	rows := make([]busMessageRow, 0, len(msgs))
	for _, msg := range msgs {
		rows = append(rows, busMessageRow{
			Seq:         msg.Seq,
			Level:       msg.Level,
			Sender:      msg.SenderLabel,
			CreatedAt:   msg.CreatedAt,
			BodyPreview: truncateBoardPreview(msg.Body),
		})
	}
	types.WriteData(w, http.StatusOK, boardBusStreamData{Messages: rows})
}

// parseBoardLimit 看板读端点 ?limit= 解析：缺省返回 0（默认值语义归 store 层注入
// ——#25=DefaultDialogLimit(100)、#26=DefaultHistoryLimit(50)）；显式非数字/≤0 →
// 400 param_invalid 并返回 ok=false（防「看似成功实未生效」，对齐 #28 limit 口径）。
func parseBoardLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
			"limit 须为正整数")
		return 0, false
	}
	return n, true
}

// truncateBoardPreview #26 preview 截断（B5-T3）：>80 rune 截 80+"..."（rune 切片
// 按字符边界切，中文不截半字）；≤80 原文返回不加省略号。
func truncateBoardPreview(body string) string {
	rs := []rune(body)
	if len(rs) <= boardPreviewRunes {
		return body
	}
	return string(rs[:boardPreviewRunes]) + "..."
}
