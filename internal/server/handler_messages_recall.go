package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 消息域 recall 动作（b5-T2 绿笔，M-36：未消费窗口消息撤回 handler 层，b5-spec
// R3）：POST /api/v1/messages/{seq}/recall。非豁免端点——走完整心跳链（四头校验
// +upsert 会话+sessionIDFromCtx 归因，中间件口径与既有消息域端点一致，本文件
// 零豁免表登记）。

// recallRequest 端点请求体（b5-spec R3）：仅 reason 一键，可选（缺体/空体/空串
// 均视为未填）——与 CLI --reason 可选参数对位。
type recallRequest struct {
	Reason string `json:"reason"`
}

// recallData 响应 data（b5-spec R3 恰两键）：幂等重撤同形 200（重复撤回=幂等
// 成功非报错，b5-spec 裁量②）。
type recallData struct {
	Seq    int64  `json:"seq"`
	Status string `json:"status"`
}

// recallAuditDetail message.recall 审计 detail 快照：四字段全取权威墓碑行
// （detailFn 收到的 rm 由 store 事务内回读注入）——幂等重放时 reason/recalled_at
// 均为首撤要素，与墓碑行同源不漂移（RecallMessageWithAudit 契约）；recalled_by
// 取四头会话名（receipt handler 的 receiptByName 同款知情权衡，零查库——心跳
// 中间件 upsert 保证会话名=四头值）。
type recallAuditDetail struct {
	Seq        int64  `json:"seq"`
	Reason     string `json:"reason"`
	RecalledBy string `json:"recalled_by"`
	RecalledAt string `json:"recalled_at"`
}

// handleMessageRecall 端点 recall（b5-spec R3/AC1，M-36）：校验链（前置参数闸
// +五语义闸）序从简到繁早退——
//  1. 路径 seq 解析：非数字/非正整数 → 400 param_invalid（#12 receipt 同款）；
//  2. 存在性：GetMessageBySeq 未命中 → 404 message_not_found（writeStoreError
//     映射，信息指明 seq）；
//  3. 发送方：messages.sender_session_id ≠ 调用方会话 id（sessionIDFromCtx）
//     → 409 not_sender（b5-spec R1.1——含 sender=0 的看板/系统消息，任何登录
//     会话均非本人）；
//  4. kind 白名单：非 direct/chat → 409 kind_not_recallable，文案区分 bus
//     （多格消费进度不一暂不可撤）与 receipt（系统件）（R1.2）；
//  5. block 拒：level=block → 409 block_not_recallable，即使未消费（R1.4 承诺
//     语义——防撤回已响铃 block）；
//  6. 未消费判定：目标格位点 ≥ seq → 409 already_consumed（R1.3——direct=
//     目标 role 格信箱位点、chat=目标会话对话位点，ConsumerPosition 纯读探测
//     缺行=0=全量未消费）。
//
// 全过 → RecallMessageWithAudit 单事务落墓碑+审计（幂等重放=幂等成功且照记审计，
// T1 口径）→ 200 data:{seq,status:"recalled"}。拒绝路径（2~6）零墓碑落库——
// 校验链全在 store 写之前（AC1 各形态断言面）。
//
// 裁量留痕：本端点未判 archived 域（spec R3 五闸未列）——b2 动作端点家族一致性
// 缺口，报备总控裁定补闸或维持。
func (s *Server) handleMessageRecall(w http.ResponseWriter, r *http.Request) {
	// ① 路径参数 seq 解析（Go 1.22 PathValue；非数字/非正整数=参数问题）。
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil || seq <= 0 {
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
			"seq 须为正整数（消息全局唯一序号）")
		return
	}

	// ② 存在性（一步带回消息本体+归属域状态，404 优先于一切语义校验）。
	md, err := s.st.GetMessageBySeq(seq)
	if err != nil {
		// ErrMessageNotFound→404 message_not_found（信息指明 seq，#12 同源口径）。
		s.writeStoreError(w, err)
		return
	}

	// ③ 发送方本人（R1.1）：心跳中间件注入的调用方会话 id 对拍消息归因会话。
	caller := sessionIDFromCtx(r.Context())
	if md.SenderSessionID != caller {
		types.WriteError(w, http.StatusConflict, types.CodeNotSender,
			fmt.Sprintf("消息 seq=%d 仅发送方本人可撤回（当前调用方会话与发送方会话不一致）", seq))
		return
	}

	// ④ kind 白名单（R1.2）：仅 direct/chat 可撤，拒绝文案按 kind 指明语义。
	switch md.Kind {
	case store.MessageKindDirect, store.MessageKindChat:
		// 可撤两 kind，过闸。
	case store.MessageKindBus:
		types.WriteError(w, http.StatusConflict, types.CodeKindNotRecallable,
			fmt.Sprintf("消息 seq=%d 为 bus 广播消息，多格消费进度不一，暂不可撤回（v1 排除面）", seq))
		return
	case store.MessageKindReceipt:
		types.WriteError(w, http.StatusConflict, types.CodeKindNotRecallable,
			fmt.Sprintf("消息 seq=%d 为 receipt 系统回执件，不可撤回（v1 排除面）", seq))
		return
	default:
		// DDL kind 枚举外理论不可达，防御同 409（不 500——语义面拒绝先例）。
		types.WriteError(w, http.StatusConflict, types.CodeKindNotRecallable,
			fmt.Sprintf("消息 seq=%d 的 kind=%s 不可撤回（仅 direct/chat 可撤）", seq, md.Kind))
		return
	}

	// ⑤ block 拒（R1.4）：承诺语义一律拒，即使未消费——防撤回已响铃 block。
	if md.Level == store.MessageLevelBlock {
		types.WriteError(w, http.StatusConflict, types.CodeBlockNotRecallable,
			fmt.Sprintf("消息 seq=%d 为 block 级消息，block 强制回执闭环=承诺语义，即使未消费也不可撤回", seq))
		return
	}

	// ⑥ 未消费判定（R1.3）：目标格位点 ≥ seq 即已消费。consumer 按 kind 取维度
	//    ——direct=目标角色名（信箱位点）、chat=目标会话对话位点；位点行归属
	//    栏目=消息归属栏目（send 链路 kind 归属规则保证与 ack 写行同键）。
	consumer := md.TargetRole
	if md.Kind == store.MessageKindChat {
		consumer = store.DialogConsumer(md.TargetSessionID)
	}
	pos, err := s.st.ConsumerPosition(md.ColumnID, consumer)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if pos >= seq {
		types.WriteError(w, http.StatusConflict, types.CodeAlreadyConsumed,
			fmt.Sprintf("消息 seq=%d 已被目标消费（消费位点 %d ≥ %d），已消费不可撤，可追发更正", seq, pos, seq))
		return
	}

	// ⑦ 请求体 reason 可选解析：空体（ContentLength=0，CLI 不带 --reason 的
	//    零体形态）跳过解码；带体走 decodeJSONBody（非法 JSON → 400 bad_json）。
	var req recallRequest
	if r.ContentLength != 0 {
		if !decodeJSONBody(w, r, &req) {
			return
		}
	}

	// ⑧ 单事务落墓碑+审计（T1 数据面）：域锚 ProjectID/ColumnID 由 store 从消息
	//    归属域回填，Detail 由 detailFn 以权威墓碑行装配（幂等重放=首撤要素）。
	_, _, err = s.st.RecallMessageWithAudit(seq, caller, strings.TrimSpace(req.Reason),
		store.AuditEntry{SessionID: caller, Action: store.AuditMessageRecall},
		func(rm store.RecalledMessage) string {
			return auditDetailJSON(recallAuditDetail{
				Seq:        rm.MessageSeq,
				Reason:     rm.Reason,
				RecalledBy: strings.TrimSpace(r.Header.Get(types.HeaderAiteamSession)),
				RecalledAt: rm.CreatedAt,
			})
		})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	types.WriteData(w, http.StatusOK, recallData{Seq: seq, Status: "recalled"})
}
