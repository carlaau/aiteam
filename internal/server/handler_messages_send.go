package server

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 消息域 send 动作（端点 #9，B2-3；自 handler_messages.go 拆出——b2-spec §五
// 拆分预案：handler_messages.go 追加 #12/#13 后超 500 行，按动作拆文件。纯移动
// 零行为变更，共享 helper（rejectArchivedDomain/rejectArchivedColumn）留主文件）。

// maxMessageBodyBytes 消息正文上限 256KB（§2.2 #9「body(必填,≤256KB)」；1MB
// 整包防线由 bodyLimit 中间件先行，此处为消息级语义校验）。
const maxMessageBodyBytes = 256 << 10

// msgLevels level 白名单（DDL CHECK 枚举，§3.2 表 5；store.InsertMessage 同构
// 兜底，handler 前置拦截使非法 level 不产生域校验读往返）。
var msgLevels = []string{store.MessageLevelNormal, store.MessageLevelImportant, store.MessageLevelBlock}

// sendRequest 端点 #9 请求体（§2.2 #9 请求结构逐字段）：target 三形态（T5）
// ——direct={kind,column,role} / bus={kind} / chat={kind,session}；level 三级；
// body 必填 ≤256KB。发送方身份不进请求体（取身份四头，§2.2 #9「发送方身份
// 取自四头」）——created_at/seq 亦无此二键（服务端生成，未知字段解码忽略）。
type sendRequest struct {
	Target struct {
		Kind    string `json:"kind"`
		Column  string `json:"column"`  // direct：目标栏目 code（项目内唯一）
		Role    string `json:"role"`    // direct：目标角色
		Session string `json:"session"` // chat：目标会话名
		// Project 跨项目目标项目 code（b5-W2/FR9，§2.3 增量）：direct/chat 可选——
		// 缺省/空/null=本项目（AC9.4 项目内行为不变）；显式=发送方自身→按项目内
		// 路径（SP1，from_project 不落）；bus 与本键同给→400 param_invalid（§2.4）。
		Project string `json:"project,omitempty"`
	} `json:"target"`
	Level string `json:"level"`
	Body  string `json:"body"`
}

// sendMessageData 端点 #9 响应 data（§2.2 #9 恰四字段：seq=全局唯一序号 AC5.1、
// created_at=服务端时间 AC5.4、level/kind 回显）。
type sendMessageData struct {
	Seq       int64  `json:"seq"`
	CreatedAt string `json:"created_at"`
	Level     string `json:"level"`
	Kind      string `json:"kind"`
}

// handleMessageSend 端点 #9 POST /api/v1/messages（§2.2 #9、§1.4 时序）：消息域
// 写路径心脏。流程=target 三形态解析（请求面校验）→ 发送方/目标域校验（存在+
// active，B2-T2 落点：动作端点判 archived 409）→ store.InsertMessage（§3.2 表 5
// 注 kind 归属规则的调用方：column_id/target 由本 handler 按形态算好传入）。
// b5-W2/FR9 增量：target.project 指向他项目时 direct/chat 按目标域解析，落行
// project_id/column_id=目标域+from_project=发送方 code（§6.3/§3.3）；项目内
// 路径行为逐字节不变（AC9.4）。
//
// DB 往返（b2-spec §七）：写路径单事务单往返=1 INSERT；读侧域校验 direct 同栏
// 目定向 1 条、bus 1 条、direct 跨栏目/chat 2 条（发送方域+目标域）；跨项目
// 再 +1 目标域读（§3.7 认可，单请求单目标域查询无 N+1）。
// 成功 201 {seq,created_at,level,kind}；不入审计（§3.2 audit_log 注——消息表
// 本身即审计）。
func (s *Server) handleMessageSend(w http.ResponseWriter, r *http.Request) {
	// ① 请求面校验（§2.2 #9）：bad_json/1MB 由 decodeJSONBody（MaxBytesReader）
	//    →level 白名单→body 非空→body 上限，全过才做域校验读（无效请求零读往返）。
	var req sendRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !slices.Contains(msgLevels, req.Level) {
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
			fmt.Sprintf("level 须为 normal/important/block，得到 %q", req.Level))
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		types.WriteError(w, http.StatusBadRequest, types.CodeBodyEmpty, "消息正文为空（body 必填）")
		return
	}
	if len(req.Body) > maxMessageBodyBytes {
		types.WriteError(w, http.StatusRequestEntityTooLarge, types.CodeBodyTooLarge,
			fmt.Sprintf("消息正文超过 256KB 上限（%d 字节）", len(req.Body)))
		return
	}

	// ② 发送方身份：四头经心跳中间件已校验非空+upsert 会话（session id 注入
	//    context）。TrimSpace 与中间件同口径（防头值带空白拼脏 label）。
	projCode := strings.TrimSpace(r.Header.Get(types.HeaderAiteamProject))
	colCode := strings.TrimSpace(r.Header.Get(types.HeaderAiteamColumn))

	// ③ 发送方域校验（存在+active）：存在性中间件步 ② 已保证（同请求内栏目
	//    行不可消失——无删除路径），查询错误理论不可达，走 writeStoreError 防御；
	//    archived 判定为本 handler 主责（B2-T2：中间件只挡不存在）。
	senderRef, err := s.st.GetProjectColumnForSession(projCode, colCode)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if rejectArchivedDomain(w, senderRef, projCode, colCode) {
		return
	}

	// ④ target 三形态解析+目标域校验（§3.2 表 5 注 kind 归属规则——handler
	//    负责按形态算 column_id/target 传 InsertMessage，store 纯落库不反推语义）。
	// 跨项目解析面（b5-W2/FR9，§6.3 冻结流程）：target.project 非空且≠发送方
	// 项目 code 才走目标域解析（=发送方自身→按项目内路径 SP1，from_project 不落）。
	// 项目内路径（不带 project 键/键=自身）走下方既有代码零变化（AC9.4）。
	var (
		in        store.MessageInput
		targetRef store.ProjectColumnRef // direct 实际落库域（=目标栏目；复用/重查件）
	)
	targetProj := strings.TrimSpace(req.Target.Project)
	crossProject := targetProj != "" && targetProj != projCode
	switch req.Target.Kind {
	case store.MessageKindDirect:
		// direct=目标栏目+目标角色（同项目内定位：以 X-Aiteam-Project 为域；
		// 跨项目定位：以 target.project 为域，§6.3）。
		targetCol := strings.TrimSpace(req.Target.Column)
		if targetCol == "" {
			types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid, "direct 形态须提供 target.column")
			return
		}
		role := strings.TrimSpace(req.Target.Role)
		if role == "" {
			types.WriteError(w, http.StatusBadRequest, types.CodeRoleInvalid, "direct 形态须提供 target.role（目标角色）")
			return
		}
		// 目标栏目所属项目：项目内=发送方项目；跨项目=目标项目（§6.3——direct
		// 用 GetProjectColumnForSession 一次查询判项目/栏目两层 404，错误文本
		// 「项目 X」「项目 X 栏目 Y」分层格式天然带目标 code，AC9.2）。
		resolveProj := projCode
		if crossProject {
			resolveProj = targetProj
		}
		targetRef = senderRef // 同栏目定向复用发送方域查询（省一条 SELECT）
		if resolveProj != projCode || targetCol != colCode {
			// 跨项目/跨栏目定向：目标域校验——ErrProjectNotFound/ErrColumnNotFound
			// →404（§2.2 #9 存在性优先于 archived：archived 前提是存在，404 判别
			// 在查询内先完成）。
			targetRef, err = s.st.GetProjectColumnForSession(resolveProj, targetCol)
			if err != nil {
				s.writeStoreError(w, err)
				return
			}
			if rejectArchivedDomain(w, targetRef, resolveProj, targetCol) {
				return
			}
		}
		// agent_conflict 拦截（2026-10-03 增补令·role 规约闸，技术设计 §2.2 #9
		// 已同步）：目标 (column,role) 格子存在 ≥2 个活会话 → 409 拒投+列活
		// 会话名+教学语。位置=域校验后、InsertMessage 前；活性判定与心跳同口径
		// （store ListAliveSessionNames，JOIN projects 取项目级阈值）。bus/chat
		// 分支不拦（bus 无目标角色、chat 定点到具体会话——多人共格的乱源只在
		// role 定向投递）。
		//
		// 「含自己在格子」读法留痕：增补令字面「目标 (column,role) 存在 ≥2 个
		// 活会话」未排除发送方自己——direct 发给自己所在格子时，发送方会话
		// （中间件本请求已 upsert、last_seen=now 恒活）计入格子活会话数，照
		// 字面实现；单活（含仅自己）/零活格子零影响。
		//
		// 并发窗口说明：本 COUNT 与 ⑤ INSERT 非原子（并发请求的中间件 upsert
		// 新会话与本判定之间理论竞窗）——本拦截是教学式防呆（引导个体化命名
		// 或改用 --bus）而非强一致约束，竞窗内极小概率漏拦可接受，不加锁不
		// 升事务。查询域=targetRef.ProjectID（目标栏目所属项目：项目内与
		// senderRef 恒等——direct 目标栏目总在发送方项目内；跨项目=目标项目，
		// b5-W2 硬约束：拦截按目标域生效）。
		alive, err := s.st.ListAliveSessionNames(targetRef.ProjectID, targetRef.ColumnID, role,
			types.NowUTC(), s.cfg.Session.HeartbeatTimeoutSec)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		if len(alive) >= 2 {
			// 跨项目补目标项目前缀（b5-W2 收口·任务 2 质量审留账）：项目内文案
			// 「目标 栏目/角色 格子…」逐字节不变（AC9.4）；跨项目「目标项目 X 栏目
			// Y/角色 格子…」指明目标域（同 chat 未命中文案的前缀形态——同 code
			// 栏目跨项目歧义时定位失败方）。
			cell := fmt.Sprintf("目标 %s/%s 格子", targetCol, role)
			if crossProject {
				cell = fmt.Sprintf("目标项目 %s 栏目 %s/%s 格子", resolveProj, targetCol, role)
			}
			types.WriteError(w, http.StatusConflict, types.CodeAgentConflict,
				fmt.Sprintf("%s存在 %d 个活会话（%s），请个体化命名 executor_<标识> 或改用 --bus 广播",
					cell, len(alive), strings.Join(alive, "、")))
			return
		}
		in = store.MessageInput{Kind: store.MessageKindDirect, ColumnID: targetRef.ColumnID, TargetRole: role}
		if crossProject {
			// 落库换目标域（§3.3）：project_id=目标项目、from_project=发送方
			// 项目 code（AC9.3；项目内路径两值均零变化——⑤ 才补 senderRef.ProjectID）。
			in.ProjectID = targetRef.ProjectID
			in.FromProject = projCode
		}
	case store.MessageKindBus:
		// bus=发送方所在栏目（审计锚点，§3.2 表 5 注），无目标参数——bus+project
		// 同给→400 param_invalid（§2.4：bus 是发送方栏目广播语义，无目标项目域
		// 可锚，b5-W2 拦点裁定在 bus case 内）。
		if targetProj != "" {
			types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
				"bus 形态不支持 target.project（bus=发送方所在栏目广播，跨项目定向请用 direct/chat）")
			return
		}
		in = store.MessageInput{Kind: store.MessageKindBus, ColumnID: senderRef.ColumnID}
	case store.MessageKindChat:
		// chat=目标会话栏目+target_session_id；目标会话必须已注册（sessions
		// 有该 name 的行，§2.2 #9——以 X-Aiteam-Project 为域；跨项目以
		// target.project 为域，§6.3：GetProjectByCode 判项目层 404/archived 后按
		// 目标 projectID 解析会话）。
		targetSess := strings.TrimSpace(req.Target.Session)
		if targetSess == "" {
			types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid, "chat 形态须提供 target.session（目标会话名）")
			return
		}
		sessProjID := senderRef.ProjectID // 会话解析域（项目内=发送方项目）
		sessProjCode := projCode
		if crossProject {
			tp, err := s.st.GetProjectByCode(targetProj)
			if err != nil {
				s.writeStoreError(w, err) // ErrProjectNotFound→404 project_not_found（既有映射）
				return
			}
			// 目标项目层 archived→409：ref 只填项目段（栏目段零值≠archived 不误拦，
			// 复用 rejectArchivedDomain 单点格式）；栏目层随 GetSessionByName 带回后判。
			if rejectArchivedDomain(w, store.ProjectColumnRef{ProjectStatus: tp.Status}, targetProj, "") {
				return
			}
			sessProjID = tp.ID
			sessProjCode = targetProj
		}
		target, err := s.st.GetSessionByName(sessProjID, targetSess)
		if errors.Is(err, store.ErrSessionNotFound) {
			// 跨项目未命中文案补目标项目前缀（spec W2⑤——同 code 会话跨项目
			// 歧义时指明域）；项目内文案逐字节不变（AC9.4）。
			if crossProject {
				types.WriteError(w, http.StatusNotFound, types.CodeTargetSessionNotFound,
					"目标项目 "+targetProj+" 无会话 "+targetSess)
			} else {
				types.WriteError(w, http.StatusNotFound, types.CodeTargetSessionNotFound, "目标会话未注册: "+targetSess)
			}
			return
		}
		if err != nil {
			s.writeStoreError(w, err) // 非「不存在」类错误→default 500+日志
			return
		}
		// 目标会话所属栏目 archived → 409 column_archived（AC2.3，与 direct 目标
		// 栏目同语义；status 随 GetSessionByName 同一查询带回，免第二次域读往返
		// ——B2-3 修复：缺失此判时消息落进 archived 栏目且 poll 可拉，与 direct
		// 行为不对称。项目级 archived：项目内=发送方项目 ③ 已拒；跨项目=目标项目
		// 上方已拒——两层均前置，此处只余栏目层）。
		if rejectArchivedColumn(w, target.ColumnStatus, sessProjCode, target.ColumnCode) {
			return
		}
		in = store.MessageInput{Kind: store.MessageKindChat, ColumnID: target.ColumnID, TargetSessionID: target.ID}
		if crossProject {
			// 落库换目标域（§3.3）：project_id=目标项目、from_project=发送方
			// 项目 code（AC9.3；chat 的 column_id=target.ColumnID 已是 B 域栏目）。
			in.ProjectID = sessProjID
			in.FromProject = projCode
		}
	default:
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
			fmt.Sprintf("target.kind 须为 direct/bus/chat，得到 %q", req.Target.Kind))
		return
	}

	// ⑤ 落库（1 INSERT 单往返，b2-spec §七）：seq/created_at 服务端生成；
	//    sender 身份=context 会话 id+四头拼 label（恒等免查，见 senderLabel）。
	//    落库项目域：项目内=发送方项目（零变化）；跨项目=目标项目（§3.3——
	//    direct/chat 分支已写 in.ProjectID，此处不覆盖）。
	if !crossProject {
		in.ProjectID = senderRef.ProjectID
	}
	in.SenderSessionID = sessionIDFromCtx(r.Context())
	in.SenderLabel = senderLabel(r)
	in.Level = req.Level
	in.Body = req.Body
	m, err := s.st.InsertMessage(in)
	if err != nil {
		// ErrMessageInvalid→400 param_invalid（store 数据面兜底，handler ①已拦
		// 主责场景）；其余（FK 等）经既有映射/500 兜底。
		s.writeStoreError(w, err)
		return
	}
	types.WriteData(w, http.StatusCreated, sendMessageData{
		Seq: m.Seq, CreatedAt: m.CreatedAt, Level: m.Level, Kind: m.Kind,
	})
}

// senderLabel 发送方冗余显示串（§3.2 表 5：'controller-A@05' 格式）。两段取
// 身份四头（TrimSpace 后与库中行恒等——sessions 行的 name 即四头 session 值、
// 栏目 code 即四头 column 值，中间件 upsert 以此二值为幂等键不改写），免查
// sessions 行、send 主路径零额外读往返（perf 默认纪律：逐行查库是例外）。
// #12 receipt handler 的 receipt_by 同款知情权衡（见其 ⑥ 步注释）。
func senderLabel(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(types.HeaderAiteamSession)) + "@" +
		strings.TrimSpace(r.Header.Get(types.HeaderAiteamColumn))
}
