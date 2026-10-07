package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 消息域端点（§2.2 #10/#11/#12/#13，B2-4/5；send 动作 #9 拆至
// handler_messages_send.go——b2-spec §五拆分预案）。本文件：共享 helper
// （rejectArchivedDomain/rejectArchivedColumn）+ #10 poll/#11 ack +
// #12 receipt/#13 history。

// rejectArchivedDomain 域 archived 判定（AC1.3/AC2.3 服务端判据，B2-T2 裁定
// 落点=send/poll 等动作端点判 409，中间件只挡不存在→404；「column_not_found
// 优先于 column_archived」语义由调用方先经 GetProjectColumnForSession 的存在性
// 错误短路、本函数只对已存在行判状态而天然满足）。项目先于栏目（外层域先拒）。
// 契约：ColumnStatus 零值/空串=调用方未提供栏目段=放行（b5-W2 chat 目标项目层
// partial ref 调用面依赖此口径，改语义须同步该调用点——若改「空串=防御性拒绝」
// 会令跨项目 chat 目标项目层判定静默全量 409）。
// 返回 true=已写 409 响应，调用方立即 return。
func rejectArchivedDomain(w http.ResponseWriter, ref store.ProjectColumnRef, projCode, colCode string) bool {
	if ref.ProjectStatus == store.ProjectStatusArchived {
		types.WriteError(w, http.StatusConflict, types.CodeProjectArchived, "项目已归档，拒绝操作: "+projCode)
		return true
	}
	return rejectArchivedColumn(w, ref.ColumnStatus, projCode, colCode)
}

// rejectArchivedColumn 栏目 archived 单项判定（AC2.3：send/poll/ack 动作端点对
// archived 栏目→409 column_archived）：send 发送方/直接目标栏目经
// rejectArchivedDomain、chat 目标会话所属栏目经 GetSessionByName 带回的
// ColumnStatus、poll/ack 消费视角栏目经 rejectArchivedDomain 共用本口径，错误码
// 与消息格式单点维护不漂移。返回 true=已写 409 响应，调用方立即 return。
// （#12 receipt 的消息域 archived 判定不走本函数：消息域状态随 GetMessageBySeq
// 带回、错误信息以 seq 指明对象，见 handleMessageReceipt ④。）
func rejectArchivedColumn(w http.ResponseWriter, colStatus, projCode, colCode string) bool {
	if colStatus != store.ColumnStatusArchived {
		return false
	}
	types.WriteError(w, http.StatusConflict, types.CodeColumnArchived,
		"栏目已归档，拒绝操作: 项目 "+projCode+" 栏目 "+colCode)
	return true
}

// ---- #10 GET /api/v1/mailbox + #11 POST /api/v1/acks（B2-4）----

// messageSenderData #10 响应 sender 身份三件（§2.2 #10 响应结构逐字段：
// sender:{session,role,column}——AC8.4 五要素之身份面）。
type messageSenderData struct {
	Session string `json:"session"`
	Role    string `json:"role"`
	Column  string `json:"column"`
}

// mailboxMessageData #10 响应 messages 元素（§2.2 #10 响应结构逐字段：恰
// {seq,kind,level,body,sender,created_at} 六键，按 seq 升序 AC8.1）。
type mailboxMessageData struct {
	Seq       int64             `json:"seq"`
	Kind      string            `json:"kind"`
	Level     string            `json:"level"`
	Body      string            `json:"body"`
	Sender    messageSenderData `json:"sender"`
	CreatedAt string            `json:"created_at"`
}

// mailboxData 端点 #10 响应 data（§2.2 #10 响应结构逐字段：三键+b7-W2 additive
// 第四键；GET 不推进位点——返回位点=当前读值，幂等重拉 AC8.1）。
//
// CrossGridHint b7-W2 跨格防呆面（additive，旧消费者不解析不受影响）：仅当本格
// 可见集 direct 面为 0 且他格有未消费 direct 时装配（条件与截断聚合见
// buildCrossGridHint）；空态/降级=键缺省（omitempty，禁 null 惯例下不用空数组
// 占位）。
type mailboxData struct {
	MailboxPosition int64                `json:"mailbox_position"`
	DialogPosition  int64                `json:"dialog_position"`
	Messages        []mailboxMessageData `json:"messages"`
	CrossGridHint   []crossGridHintData  `json:"cross_grid_hint,omitempty"`
}

// ackData 端点 #11 响应 data（§2.2 #11 响应结构逐字段：恰双位点两键）。
type ackData struct {
	MailboxPosition int64 `json:"mailbox_position"`
	DialogPosition  int64 `json:"dialog_position"`
}

// resolveMailboxDomain poll/ack 共用前置（§2.2 #10/#11 请求面同构：query 传
// column/role/session 三参为消费视角，技术设计 #10 原文）：
//   - column/role 必填（空白=400 param_invalid）；
//   - 域校验：项目域取四头（心跳中间件已保证存在），query column 独立校验——
//     未登记→404 column_not_found（writeStoreError 映射，AC2.2）、archived→409
//     （B2-T2 动作端点判）；存在性先于 archived 由查询短路顺序天然满足；
//   - session：会话名→id（对话位点 consumer 定位）。非空未注册→404
//     session_not_found（§2.4 404 族既有码；显式报错优于静默空对话——CLI 身份
//     拼写错误不可被吞）；requireSession（ack）时缺失→400（AdvancePositions
//     双维度 upsert 需真实会话 id，放行 sessionID=0 会写 chat:session:0 位点
//     垃圾行）；poll 缺省=0（对话维度自然落空——target_session_id=0 不命中
//     谓词第三分支；GetPositions 读路径对缺失对话行零写入，B2-T3）。
//     位点行的栏目归属恒传本函数返回的 ref.ColumnID（消费视角栏目，正常形态=
//     会话所属栏目；对话谓词不筛栏目、归属仅是位点键组织维度——口径详见
//     positions.go GetPositions 注释，勿改传目标会话栏目）。
//
// 与 send handler 的知情权衡同款（见 handleMessageSend ③）：中间件+handler 各查
// 一次域——中间件查的是四头栏目（心跳 upsert 定位），本函数查 query 栏目（消费
// 视角定位），两次查询目标不同非重复。返回 ok=false=响应已写出，调用方即 return。
func (s *Server) resolveMailboxDomain(w http.ResponseWriter, r *http.Request, requireSession bool) (ref store.ProjectColumnRef, role string, sessionID int64, ok bool) {
	projCode := strings.TrimSpace(r.Header.Get(types.HeaderAiteamProject))
	colCode := strings.TrimSpace(r.URL.Query().Get("column"))
	role = strings.TrimSpace(r.URL.Query().Get("role"))
	if colCode == "" || role == "" {
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
			"column 与 role 为必填 query 参数（消费视角定位）")
		return ref, "", 0, false
	}
	ref, err := s.st.GetProjectColumnForSession(projCode, colCode)
	if err != nil {
		s.writeStoreError(w, err)
		return ref, "", 0, false
	}
	if rejectArchivedDomain(w, ref, projCode, colCode) {
		return ref, "", 0, false
	}
	if sess := strings.TrimSpace(r.URL.Query().Get("session")); sess != "" {
		se, err := s.st.GetSessionByName(ref.ProjectID, sess)
		if errors.Is(err, store.ErrSessionNotFound) {
			types.WriteError(w, http.StatusNotFound, types.CodeSessionNotFound, "消费视角会话未注册: "+sess)
			return ref, "", 0, false
		}
		if err != nil {
			s.writeStoreError(w, err) // 非「不存在」类错误→default 500+日志
			return ref, "", 0, false
		}
		sessionID = se.ID
	} else if requireSession {
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
			"ack 须提供 session（对话位点定位）")
		return ref, "", 0, false
	}
	return ref, role, sessionID, true
}

// crossGridHintData #10 响应 cross_grid_hint 元素（b7-W2：`[]{role,pending}` 最小
// 面——零正文/零 level/零时间戳泄漏，b7 AC5）。与服务端直查面 store.CrossGridPending
// 同构但独立声明：store 类型不带 json tag（直透序列化会成 Role/Pending，T1 陷阱警
// 示见其 doc），wire 键名归本 DTO；CLI 侧同构声明在 poll.go pollCrossGridHint。
type crossGridHintData struct {
	Role    string `json:"role"`
	Pending int    `json:"pending"`
}

// crossGridHintCap 服务端截断上限（b7-spec §二.1 末句「列表上限 5 格……服务端截
// 断，防大栏目刷屏」）：hint 元素至多 cap 个真实 role 格；超出部分聚合为末尾一个
// 哨兵元素 {role:"等 N 格", pending:剩余条数求和}（N=剩余格数）——wire 形状仍为
// `[]{role,pending}` 不破契约，role 值携带聚合短语由 CLI 渲染侧识别（解析规则两
// 侧同检：前缀「等 」后缀「 格」，见 poll.go renderCrossGridHints）。裁量为「Top5
// 条数降序+单聚合元素」：积压多的格排前（最先该去看的格），刷屏有界（至多 6 行）。
const crossGridHintCap = 5

// crossGridAggregateRole 聚合哨兵元素 role 值格式（N=剩余格数；CLI 渲染侧按此前
// 缀/后缀识别，勿改措辞——两侧同检）。
const crossGridAggregateRole = "等 %d 格"

// buildCrossGridHint cross_grid_hint 装配纯函数（b7-W2，可单测）：入参为
// CrossGridDirectPending 查询产物与错误——err 非 nil 降级 nil（提示属尽力面，不反
// 噬主响应，b7-spec §二.2）；空切片降级 nil（他格无积压零噪音，AC2；omitempty 下
// nil 与键缺省等价，禁 null 惯例下不用空数组占位）；非空映射为 wire DTO，超出
// crossGridHintCap 截断并追加聚合哨兵元素（保留 SQL 的 COUNT(*) DESC 序）。
func buildCrossGridHint(pending []store.CrossGridPending, err error) []crossGridHintData {
	if err != nil || len(pending) == 0 {
		return nil // 降级/零噪音：键缺省（omitempty），主响应不受影响
	}
	n := min(len(pending), crossGridHintCap)
	out := make([]crossGridHintData, 0, n+1)
	for _, p := range pending[:n] {
		out = append(out, crossGridHintData{Role: p.Role, Pending: p.Pending})
	}
	if rest := pending[n:]; len(rest) > 0 {
		sum := 0
		for _, p := range rest {
			sum += p.Pending
		}
		out = append(out, crossGridHintData{
			Role:    fmt.Sprintf(crossGridAggregateRole, len(rest)),
			Pending: sum,
		})
	}
	return out
}

// countVisibleDirect 可见集内 kind=direct 条数（b7-W2 装配条件面：仅计数为 0 才
// 触发跨格计算——R4 直接口径：实战签名=chat 在而 direct 空，按消息总数口径会漏
// 防；本格 direct 面非空即零计算零字段，热路径零增负）。
func countVisibleDirect(msgs []store.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Kind == store.MessageKindDirect {
			n++
		}
	}
	return n
}

// newMailboxData store 位点+消息行+发送方装配表 → #10 响应 data（§2.2 #10 逐字
// 段）。发送方未命中（sender_session_id=0=系统/看板发送的弱关联语义）sender
// 三件零值空串，装配缺失非数据错误（GetSendersByIDs 同口径）。hint 入参为调用方
// 条件装配产物（见 handleMessagePoll ④），nil=键缺省。
func newMailboxData(pos store.Positions, msgs []store.Message, senders map[int64]store.SenderRef, hint []crossGridHintData) mailboxData {
	rows := make([]mailboxMessageData, 0, len(msgs))
	for _, m := range msgs {
		ref := senders[m.SenderSessionID] // nil map/miss 读均安全零值
		rows = append(rows, mailboxMessageData{
			Seq:       m.Seq,
			Kind:      m.Kind,
			Level:     m.Level,
			Body:      m.Body,
			Sender:    messageSenderData{Session: ref.Session, Role: ref.Role, Column: ref.Column},
			CreatedAt: m.CreatedAt,
		})
	}
	return mailboxData{MailboxPosition: pos.Mailbox, DialogPosition: pos.Dialog, Messages: rows, CrossGridHint: hint}
}

// handleMessagePoll 端点 #10 GET /api/v1/mailbox（§2.2 #10、§3.6 谓词，路由照
// 端点总表原文）：流程=消费视角解析+域校验（resolveMailboxDomain）→limit 解析
// →GetPositions（信箱缺失惰性 upsert=当时 MAX(seq)、对话缺失按 0，D7/B2-T3）→
// PollVisible（§3.6 谓词三分支单条 SQL，拉取不推进位点）→sender 批量装配→
// cross_grid_hint 条件装配（b7-W2）。
//
// DB 往返（b2-spec §七 poll 行）：位点单条 SELECT（+首次惰性 upsert）+谓词单条
// SELECT+sender 装配一条 IN（空批零往返）——无逐行循环查库；limit：缺省=
// DefaultPollLimit 500（D8 覆盖端点表草案「默认 0=全量」表述）、显式 0=全量、
// 负值/非数字=400 param_invalid。成功 200 data:{mailbox_position,dialog_position,
// messages[,cross_grid_hint]}（五要素齐 AC8.4；hint 键缺省=未触发面，additive）。
func (s *Server) handleMessagePoll(w http.ResponseWriter, r *http.Request) {
	ref, role, sessionID, ok := s.resolveMailboxDomain(w, r, false)
	if !ok {
		return
	}
	limit := store.DefaultPollLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
				"limit 须为非负整数（缺省 500，0=全量）")
			return
		}
		limit = n
	}
	pos, err := s.st.GetPositions(ref.ProjectID, ref.ColumnID, role, sessionID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	msgs, err := s.st.PollVisible(ref.ProjectID, ref.ColumnID, role, sessionID, pos.Mailbox, pos.Dialog, limit)
	if err != nil {
		// ErrInvalidLimit→400（store 数据面兜底，handler 上方已拦主责场景）。
		s.writeStoreError(w, err)
		return
	}
	// sender 身份装配（AC8.4）：一条批量 IN 查询按 distinct 发送方取回（往返账
	// 无逐行循环），空批零往返；miss（系统/看板发送 id=0）零值兜底。
	var senders map[int64]store.SenderRef
	if len(msgs) > 0 {
		ids := make([]int64, 0, len(msgs))
		for _, m := range msgs {
			if m.SenderSessionID != 0 {
				ids = append(ids, m.SenderSessionID)
			}
		}
		if senders, err = s.st.GetSendersByIDs(ids); err != nil {
			s.writeStoreError(w, err)
			return
		}
	}
	// cross_grid_hint 条件装配（b7-W2，④）：仅当本格可见集 direct 面为 0 才发起
	// 跨格计数（热路径零增负，R4 直接口径——实战签名=chat 在而 direct 空）；错误
	// 降级 nil 不反噬主响应（提示属尽力面，b7-spec §二.2），但吞错留 WARN 观测面
	// （T2 审查修 2：对齐 writeStoreError/middleware 吞错留痕惯例，零可观测性则
	// 降级不可诊断）。
	var hint []crossGridHintData
	if countVisibleDirect(msgs) == 0 {
		pending, err := s.st.CrossGridDirectPending(ref.ColumnID, role)
		if err != nil {
			slog.Warn("cross_grid_hint 装配降级", "err", err)
		}
		hint = buildCrossGridHint(pending, err)
	}
	types.WriteData(w, http.StatusOK, newMailboxData(pos, msgs, senders, hint))
}

// handleMessageAck 端点 #11 POST /api/v1/acks（§2.2 #11，路由照端点总表原文）：
// 流程=消费视角解析+域校验（resolveMailboxDomain，session 必填）→seq 解析→
// AdvancePositions 单事务双维度推进（seq>0 显式、省略=各自最大可见——一键处理
// 完；position=max 幂等 AC9.2）。请求/响应往返：单事务 2 upsert（§七 ack 行）。
// 成功 200 data:{mailbox_position,dialog_position}。
func (s *Server) handleMessageAck(w http.ResponseWriter, r *http.Request) {
	ref, role, sessionID, ok := s.resolveMailboxDomain(w, r, true)
	if !ok {
		return
	}
	var seq int64
	if raw := r.URL.Query().Get("seq"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
				"seq 须为非负整数（不传=推进到当前各自最大可见）")
			return
		}
		seq = n
	}
	pos, err := s.st.AdvancePositions(ref.ProjectID, ref.ColumnID, role, sessionID, seq)
	if err != nil {
		// ErrInvalidSeq→400（store 数据面兜底，handler 上方已拦主责场景）。
		s.writeStoreError(w, err)
		return
	}
	types.WriteData(w, http.StatusOK, ackData{MailboxPosition: pos.Mailbox, DialogPosition: pos.Dialog})
}

// ---- #12 POST /api/v1/messages/{seq}/receipt + #13 GET /api/v1/messages（B2-5）----

// receiptResponseData 端点 #12 响应 data（§2.2 #12 恰三键：seq=被回执消息、
// receipt_at=服务端回执时间 AC7.3、receipt_by=回执方会话名 AC7.3）。成功状态码
// 统一 200：端点表对 #12 未标注 201（对比 #9 有 §1.4 时序明示 201），且
// already_recepted 幂等命中亦为「200 返回已有记录」——#12 语义=状态记录（幂等
// 写）而非资源创建，成功面统一 200（首次与重执响应形状一致，AC9.2）。
type receiptResponseData struct {
	Seq       int64  `json:"seq"`
	ReceiptAt string `json:"receipt_at"`
	ReceiptBy string `json:"receipt_by"`
}

// handleMessageReceipt 端点 #12 POST /api/v1/messages/{seq}/receipt（§2.2 #12、
// §2.3 副作用链、T4/A12）：block 消息回执+自动回告。校验链（存在性 404 →
// not_block_level 409 → archived 409 → 幂等 200）：
//   - 消息存在 404 message_not_found（GetMessageBySeq 一步带回消息本体+归属域
//     状态，免三次读往返）；
//   - level=block 校验（AC7.4，非 block 不可回执）；
//   - archived 判定为本 handler 主责（B2-T2 动作端点判 409；#12 原文特有错误
//     未列 archived——按动作端点同口径判：回执产生写副作用，与 send/poll 同族。
//     项目先于栏目，对齐 send 的 rejectArchivedDomain 顺序）；
//   - 幂等命中 200 返回已有记录（AC9.2 already_recepted 语义，见
//     InsertReceiptWithNotify——重执零副作用不重复发回告）。
//
// 回告消息归属（§3.2 表 5 注）：column_id=原发送方会话所属栏目（非原消息栏目
// ——direct 跨栏目场景二者不同），target_session_id=原发送方会话。原发送方会话
// 定位复用 GetSession（消息项目 active 已先行保证——archived 已被 409 拦）。
//
// DB 往返（b2-spec §七）：域校验读 2（消息+域状态 1、原发送方会话 1）+事务内
// 2 INSERT；幂等命中=同 2 读+回执方名装配 1 读（GetSendersByIDs 取原回执方会
// 话名）+空事务（零副作用）。
func (s *Server) handleMessageReceipt(w http.ResponseWriter, r *http.Request) {
	// ① 路径参数 seq 解析（Go 1.22 PathValue；非数字/非正整数=参数问题）。
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil || seq <= 0 {
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
			"seq 须为正整数（消息全局唯一序号）")
		return
	}

	// ② 消息存在性+域状态（404 优先于一切语义校验）。
	md, err := s.st.GetMessageBySeq(seq)
	if err != nil {
		// ErrMessageNotFound→404 message_not_found（信息指明 seq，AC2.2 口径）。
		s.writeStoreError(w, err)
		return
	}

	// ③ level=block 校验（AC7.4）：非 block 级不产生待回执状态。
	if md.Level != store.MessageLevelBlock {
		types.WriteError(w, http.StatusConflict, types.CodeNotBlockLevel,
			fmt.Sprintf("消息 seq=%d 的 level=%s，仅 block 级消息可回执", seq, md.Level))
		return
	}

	// ④ archived 判定（B2-T2 动作端点裁量，注释见函数头）：项目先于栏目；
	//    错误信息以 seq 指明对象（消息域视角，code 装配不在此查询面）。
	if md.ProjectStatus == store.ProjectStatusArchived {
		types.WriteError(w, http.StatusConflict, types.CodeProjectArchived,
			fmt.Sprintf("消息归属项目已归档，拒绝回执: seq=%d", seq))
		return
	}
	if md.ColumnStatus == store.ColumnStatusArchived {
		types.WriteError(w, http.StatusConflict, types.CodeColumnArchived,
			fmt.Sprintf("消息归属栏目已归档，拒绝回执: seq=%d", seq))
		return
	}

	// ⑤ 原发送方会话定位（回告归属依据，§3.2 表 5 注）：send 链路落库保证
	//    sender_session_id 为真实会话 id；0=系统语义行理论不可回执（防御 404
	//    ——回执前提不成立：回告无处投递）。GetSession 的 active 筛不误伤：
	//    消息项目 archived 已被 ④ 拦，active 项目内会话行照常命中。
	if md.SenderSessionID <= 0 {
		types.WriteError(w, http.StatusNotFound, types.CodeMessageNotFound,
			fmt.Sprintf("消息 seq=%d 无原发送方会话（系统消息不可回执）", seq))
		return
	}
	senderSess, err := s.st.GetSession(md.SenderSessionID, types.NowUTC(), s.cfg.Session.HeartbeatTimeoutSec)
	if err != nil {
		// ErrSessionNotFound 经 writeStoreError 落 default 500（清单二口径）：
		// 消息存在而发送方会话缺失=数据完整性异常，理论不可达（send 落库保证）。
		s.writeStoreError(w, err)
		return
	}

	// ⑥ 回执方会话名（AC7.3 身份面）：心跳中间件 upsert 保证会话名=四头值
	//    （senderLabel 同款知情权衡，零查库）。
	receiptByName := strings.TrimSpace(r.Header.Get(types.HeaderAiteamSession))

	// ⑦ 同事务双 INSERT（§2.3 副作用链）：回执行+回告消息原子；already=true
	//    =幂等命中（200 返回已有记录，零副作用）。
	rec, already, err := s.st.InsertReceiptWithNotify(seq, sessionIDFromCtx(r.Context()), store.ReceiptNotifyInput{
		ProjectID:       md.ProjectID,
		TargetSessionID: md.SenderSessionID,
		ColumnID:        senderSess.ColumnID,
		ReceiptByName:   receiptByName,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	// receipt_by=已有记录的回执方：首执=当前请求者（四头值）；重执=原回执方
	// （T4 不校验回执方，重执者可异人——返回已有记录视角，AC9.2）。
	receiptBy := receiptByName
	if already {
		refs, err := s.st.GetSendersByIDs([]int64{rec.ReceiptBy})
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		receiptBy = refs[rec.ReceiptBy].Session
	}
	types.WriteData(w, http.StatusOK, receiptResponseData{
		Seq: rec.MessageSeq, ReceiptAt: rec.ReceiptAt, ReceiptBy: receiptBy,
	})
}

// receiptData #13 响应 receipt 字段（§2.2 #13「receipt:{by,at}|null」逐字段：
// by=回执方会话名、at=回执服务端时间——AC7.2 回执前 null、AC7.3 回执后对象）。
// 指针类型承载三态：nil → JSON null，非 nil → {by,at}。
type receiptData struct {
	By string `json:"by"`
	At string `json:"at"`
}

// historyMessageData #13 响应 messages 元素（§2.2 #13 恰七键）。sender 与 #10
// 同构（messageSenderData 复用）；receipt 指针 nil=未回执。
type historyMessageData struct {
	Seq       int64             `json:"seq"`
	Kind      string            `json:"kind"`
	Level     string            `json:"level"`
	Body      string            `json:"body"`
	Sender    messageSenderData `json:"sender"`
	Receipt   *receiptData      `json:"receipt"`
	CreatedAt string            `json:"created_at"`
}

// historyData 端点 #13 响应 data（§2.2 #13：恰 {messages} 一键）。
type historyData struct {
	Messages []historyMessageData `json:"messages"`
}

// handleMessageHistory 端点 #13 GET /api/v1/messages（§2.2 #13 通用历史/审计
// 查询口，AC1.3/AC2.3 历史可查、AC7.2/7.3 回执状态判据）：过滤参数全量直通
// store（project/column/kind/level/sent_by 空串=不过滤；未命中=空结果非 404，
// store 数据面口径）；archived 域不拒（#13 是 B2-T2「历史查询不拒」例外端点，
// AC1.3 历史保留——QueryHistory 不筛 status，本 handler 亦无域校验步）。
//
// limit：缺省/0=DefaultHistoryLimit 50（端点表「默认 50」；D8 的 0=全量语义
// 仅属 poll #10，#13 无全量语义——0 走默认）；负值/非数字=400（对齐 #28
// 「看似成功实未生效」拒绝先例）；上限由 store clamp 500。order 白名单 handler
// 前置拦截（无效请求零读往返；store ErrInvalidOrder 数据面兜底同映射 400）。
//
// receipt 字段装配（b2-spec §七「禁逐 seq 循环查回执」）：block 消息 seq 一条
// IN 批量取回执（GetReceiptsBySeqs）+发送方/回执方会话名一条 IN 批量装配
// （GetSendersByIDs 两类 id 合并去重一次查询）——固定 3 条 SELECT（历史+回执+
// 会话名，空批各减 1），不随结果行数增长。
func (s *Server) handleMessageHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.HistoryFilter{
		ProjectCode: strings.TrimSpace(q.Get("project")),
		ColumnCode:  strings.TrimSpace(q.Get("column")),
		Kind:        strings.TrimSpace(q.Get("kind")),
		Level:       strings.TrimSpace(q.Get("level")),
		SentBy:      strings.TrimSpace(q.Get("sent_by")),
	}
	// seq 区间参数：非数字/负值 → 400（0=不过滤）。
	for _, p := range []struct {
		name string
		dst  *int64
	}{{"since_seq", &f.SinceSeq}, {"before_seq", &f.BeforeSeq}} {
		if raw := q.Get(p.name); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 {
				types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
					p.name+" 须为非负整数（0=不过滤）")
				return
			}
			*p.dst = n
		}
	}
	// limit：非数字/负值 → 400；0/缺省 → store 默认 50。
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
				"limit 须为非负整数（缺省 50，最大 500）")
			return
		}
		f.Limit = n
	}
	// order 白名单（值拼入 SQL 文本，前置拦截防注入读往返；store 同构兜底）。
	f.Order = strings.TrimSpace(q.Get("order"))
	if f.Order != "" && f.Order != "desc" && f.Order != "asc" {
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
			"order 须为 desc/asc（缺省 desc）")
		return
	}

	msgs, err := s.st.QueryHistory(f)
	if err != nil {
		// ErrInvalidOrder→400（store 数据面兜底，上方已拦主责场景）。
		s.writeStoreError(w, err)
		return
	}

	// receipt 批量装配：仅 block 消息可能有回执（回执只对 block，§2.2 #12）。
	blockSeqs := make([]int64, 0, len(msgs))
	senderIDs := make([]int64, 0, len(msgs))
	seenSender := make(map[int64]struct{}, len(msgs))
	for _, m := range msgs {
		if m.Level == store.MessageLevelBlock {
			blockSeqs = append(blockSeqs, m.Seq)
		}
		// 发送方 id 收集（0=系统/看板弱关联，GetSendersByIDs miss 零值兜底）。
		if m.SenderSessionID != 0 {
			if _, dup := seenSender[m.SenderSessionID]; !dup {
				seenSender[m.SenderSessionID] = struct{}{}
				senderIDs = append(senderIDs, m.SenderSessionID)
			}
		}
	}
	receipts := map[int64]store.Receipt{}
	if len(blockSeqs) > 0 {
		if receipts, err = s.st.GetReceiptsBySeqs(blockSeqs); err != nil {
			s.writeStoreError(w, err)
			return
		}
		// 回执方 id 并入同一装配批次（一条 IN 取回执方会话名）。
		for _, rec := range receipts {
			if _, dup := seenSender[rec.ReceiptBy]; !dup {
				seenSender[rec.ReceiptBy] = struct{}{}
				senderIDs = append(senderIDs, rec.ReceiptBy)
			}
		}
	}
	var refs map[int64]store.SenderRef
	if len(senderIDs) > 0 {
		if refs, err = s.st.GetSendersByIDs(senderIDs); err != nil {
			s.writeStoreError(w, err)
			return
		}
	}

	rows := make([]historyMessageData, 0, len(msgs))
	for _, m := range msgs {
		row := historyMessageData{
			Seq:       m.Seq,
			Kind:      m.Kind,
			Level:     m.Level,
			Body:      m.Body,
			Sender:    messageSenderData{Session: refs[m.SenderSessionID].Session, Role: refs[m.SenderSessionID].Role, Column: refs[m.SenderSessionID].Column},
			CreatedAt: m.CreatedAt,
		}
		if rec, ok := receipts[m.Seq]; ok {
			row.Receipt = &receiptData{By: refs[rec.ReceiptBy].Session, At: rec.ReceiptAt}
		}
		rows = append(rows, row)
	}
	types.WriteData(w, http.StatusOK, historyData{Messages: rows})
}
