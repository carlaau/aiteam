package server

// Package server 内中间件实现与测试（心跳/鉴权两段——B1-4 与 B6-1 rebase 合并）。

import (
	"context"

	"crypto/subtle"

	"encoding/json"

	"errors"

	"log/slog"

	"net/http"

	"strings"

	"aiteam/internal/config"

	"aiteam/internal/store"

	"aiteam/internal/types"
)

// heartbeatExemptPaths 心跳豁免路径（§7.1「全部 /api/v1 端点，ping 除外」+

// b1-spec §二「ping/version 豁免」）：健康检查无会话语义——不校验四头、不

// upsert、不审计（豁免=跳过整段心跳链）。仅处理 /api/v1 前缀内的豁免，按

// URL.Path 精确匹配（B0-4 注释口径）：大小写变体（/api/v1/PING）不豁免照常

// 校验；百分号编码经 URL 解码后同 Path 的（/api/v1/%70ing）命中豁免。看板

// 端点豁免口径归 B5 裁量（b1-spec §三.5），本表勿预置。

var heartbeatExemptPaths = map[string]bool{
	"/api/v1/ping":    true,
	"/api/v1/version": true,
}

// heartbeatExemptSystemActions 系统动作豁免表（B2 首任务前置修，总控裁定
// 4168d76）：首条 project.register 天然无已登记身份域可挂身份四头（全新空库
// 自举引导死锁），豁免 POST /api/v1/projects 的缺头强制。与 heartbeatExemptPaths
// 两点差异：①按「方法+路径」双重精确匹配（同路径其余方法与其余端点不外溢——
// 任何匿名写不得借豁免绕过心跳）；②只豁缺头环节：四头齐全的登记请求照常走
// 完整心跳链（upsert+会话级审计不降级，TestCreateProjectEndpoint 口径）。
// 缺头放行不经 ②~④：不 upsert、不注入 session context，handler 审计
// session_id 经 sessionIDFromCtx 取 0=系统动作（audit_log 三弱关联 0 直插合法，
// §3.2 表 10 注——系统动作留痕不丢）。
var heartbeatExemptSystemActions = map[string]map[string]bool{
	"/api/v1/projects": {http.MethodPost: true},
}

// isColumnRegisterPath 栏目登记路径判定（#9 小批派工：空库自举死结第二层，
// 对称 projects 豁免论证——首栏目建不了则空库 CLI column register 同死，S6
// 用户验收前必修）：路由含动态段（/api/v1/projects/{code}/columns，B1 #5），
// 表精确匹配不适用，按段形态判定——段数恰六且 projects 段字面、code 段非空、
// 末段 columns 字面。大小写/编码经 r.URL.Path（已解码）天然精确，与表口径
// 一致；深层路径（…/columns/01）段数不符不命中。豁免语义同表：仅缺头环节
// 放行（不 upsert、不注入 context），带四头照常完整心跳链。
func isColumnRegisterPath(path string) bool {
	parts := strings.Split(path, "/")
	return len(parts) == 6 && parts[1] == "api" && parts[2] == "v1" &&
		parts[3] == "projects" && parts[4] != "" && parts[5] == "columns"
}

// boardAlwaysExemptPaths 看板恒豁免端点（B5-T1：#24 看板唯一写口恒不带身份四头
// ——board 独立 sender 语义 sender_session_id=0，带四头也无会话语义可挂，故为
// 完全豁免=跳过整段心跳链，ping/version 同款；对照下方可选豁免=带四头照常
// upsert）。方法+路径双重精确匹配（同路径其余方法不外溢，
// heartbeatExemptSystemActions 同款——任何匿名写不得借豁免绕过心跳）。
var boardAlwaysExemptPaths = map[string]map[string]bool{
	"/api/v1/board/messages": {http.MethodPost: true},
}

// boardOptionalHeaderPaths 看板可达 GET 端点精确路径表（B5-T1 四头可选口径——
// b1-spec §三.5 显式预留裁量点落本批）：无四头=直接放行不 upsert（看板路径，
// sessions 表零写入）；带四头=完整心跳链（隐式注册+刷心跳，CLI 路径不回归）。
// 动态段端点 #25（/api/v1/board/sessions/{id}/dialog）不入精确表，由
// isBoardDialogPath 段形态判定（isColumnRegisterPath 先例同法）。
var boardOptionalHeaderPaths = map[string]bool{
	"/api/v1/status":           true, // #17 status（CLI status 与看板首屏同源数据口）
	"/api/v1/resources":        true, // #20 资源分组表
	"/api/v1/windows/now":      true, // #22 时间窗矩阵
	"/api/v1/board/bus-stream": true, // #26 总线流薄壳
	"/api/v1/sessions":         true, // #27 心跳面数据口
}

// isBoardDialogPath #25 对话流路径判定：路由含动态段（{id}=sessions 数字主键，
// B5-1 #25），精确表不适用，按段形态判定——段数恰七且 board/sessions 段字面、
// id 段非空、末段 dialog 字面（isColumnRegisterPath 同法；大小写/编码经已解码
// r.URL.Path 天然精确；深层路径段数不符不命中）。
func isBoardDialogPath(path string) bool {
	parts := strings.Split(path, "/")
	return len(parts) == 7 && parts[1] == "api" && parts[2] == "v1" &&
		parts[3] == "board" && parts[4] == "sessions" && parts[5] != "" && parts[6] == "dialog"
}

// isBoardOptionalHeaderPath 看板可选四头豁免判定（方法+路径模式匹配——精确表 +
// #25 段形态）。方法收窄 GET：看板读口全部 GET（写口仅 #24 且归恒豁免面），
// 同路径非 GET 方法缺头照旧 400（TestProjectRegisterExemptNotLeaky「同路径其余
// 方法」口径——匿名写不得借豁免探路）。
func isBoardOptionalHeaderPath(method, path string) bool {
	return method == http.MethodGet &&
		(boardOptionalHeaderPaths[path] || isBoardDialogPath(path))
}

// sessionHeartbeat 会话隐式注册/心跳刷新中间件（§7.1 全流程，T1/T3）：
//
//	⓪ 作用域：仅 /api/v1/ 前缀路径（spec §7.1 冻结口径「全部 /api/v1 端点」；
//	  非 /api/v1 前缀直接放行——B5 看板静态资源将挂非 /api/v1 路径，且 404
//	  （路径不存在）对外部探活语义优先于 400（配置错误）。B1-4 审查 Important 1
//	  总控裁定选项①）；
//	① 四头必填校验：X-Aiteam-Project/Column/Session/Role 缺任一 → 400
//	  missing_header，错误信息逐个指明缺失头名（b1-spec §二 流程①）；例外：
//	  POST /api/v1/projects 命中系统动作豁免表缺头放行（B2 前置修，总控裁定
//	  4168d76——首条登记自举引导，见 heartbeatExemptSystemActions）；
//	② 存在性短路：项目/栏目未登记 → 404 project_not_found/column_not_found
//	  （AC2.2 服务端判据，错误信息指明实体；一条 JOIN 取两行，spec §七往返账）；
//	③ upsert sessions：首插=隐式注册（AC12.3），重调刷 last_seen_at+覆盖 role
//	  （AC12.1 心跳，D5 role 静默覆盖）；
//	④ 事件审计：首插落 session.auto_register（T1）、role 实际变更落
//	  session.role_change（D5 强制留痕，detail 记 before/after）——审计写失败
//	  500 短路不放行（AC1.4 留痕强制，静默丢审计=违规）；
//	⑤ 放行 handler。
//
// archived 域本批放行（b1-spec §二：本批只挡不存在→404，archived 409 拒绝归
// B2 动作端点——若总控裁反向，仅 ② 处加 status 判断一行）。
// 挂链位置：bodyLimit → [B6 token 预留] → 本中间件 → mux（NewServer 组装固化）。

func (s *Server) sessionHeartbeat(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ⓪ 非 /api/v1/ 前缀直接放行（B1-4 审查 Important 1 总控裁定选项①）。
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		// 完全豁免（跳过整段心跳链）：健康检查（ping/version，无会话语义）+
		// 看板恒豁免写口 #24（boardAlwaysExemptPaths——恒不带四头，带四头也无
		// 会话语义可挂，B5-T1）。
		if heartbeatExemptPaths[r.URL.Path] || boardAlwaysExemptPaths[r.URL.Path][r.Method] {
			next.ServeHTTP(w, r)
			return
		}

		// ① 四头必填校验：TrimSpace 后空串视为缺失（纯空白头等同未携带）。
		fields := []struct {
			name  string
			value string
		}{
			{types.HeaderAiteamProject, strings.TrimSpace(r.Header.Get(types.HeaderAiteamProject))},
			{types.HeaderAiteamColumn, strings.TrimSpace(r.Header.Get(types.HeaderAiteamColumn))},
			{types.HeaderAiteamSession, strings.TrimSpace(r.Header.Get(types.HeaderAiteamSession))},
			{types.HeaderAiteamRole, strings.TrimSpace(r.Header.Get(types.HeaderAiteamRole))},
		}
		var missing []string
		for _, f := range fields {
			if f.value == "" {
				missing = append(missing, f.name)
			}
		}
		if len(missing) > 0 {
			// 豁免放行（缺头不经 ②~④，session context 不注入、sessions 零写入）：
			//   - 系统动作豁免（heartbeatExemptSystemActions + isColumnRegisterPath，
			//     总控裁定 4168d76 与 #9 小批派工对称扩围——登记自举引导）；
			//   - 看板可选豁免（isBoardOptionalHeaderPath，B5-T1：六端点四头可选，
			//     不带=直接放行看板路径；带四头照常走下方完整心跳链=CLI 路径）。
			if heartbeatExemptSystemActions[r.URL.Path][r.Method] ||
				(r.Method == http.MethodPost && isColumnRegisterPath(r.URL.Path)) ||
				isBoardOptionalHeaderPath(r.Method, r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			types.WriteError(w, http.StatusBadRequest, types.CodeMissingHeader,
				"缺少身份请求头: "+strings.Join(missing, ", "))
			return
		}
		projCode, colCode, sessName, role := fields[0].value, fields[1].value, fields[2].value, fields[3].value

		// ①′ role 前缀轻校验（B2 批裁定）：合法形态恰三种——controller 单值 /
		// executor 裸值（换班场景）/ executor_ 前缀多执行者（executor_A、
		// executor_web-1 等），其余 400 invalid_role——防乱起 role 名导致定向
		// 投递混乱（信箱一格一人）。executor_（前缀后空标识）放行：轻校验不
		// 限制标识字符，空标识语义等价裸 executor 变体。类广播（面向全员）
		// 不走 role 目标（用 --bus），与本规约正交。位置在存在性校验之前：
		// 纯输入形态校验不触库，与 ① 缺头校验同层短路。
		if role != "controller" && role != "executor" && !strings.HasPrefix(role, "executor_") {
			types.WriteError(w, http.StatusBadRequest, types.CodeInvalidRole,
				"invalid_role: "+role+"（合法：controller / executor / executor_<标识>）")
			return
		}

		// ② 存在性短路（AC2.2）：哨兵错误映射 404，错误信息指明实体。
		ref, err := s.st.GetProjectColumnForSession(projCode, colCode)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrProjectNotFound):
				types.WriteError(w, http.StatusNotFound, types.CodeProjectNotFound, "项目未登记: "+projCode)
			case errors.Is(err, store.ErrColumnNotFound):
				types.WriteError(w, http.StatusNotFound, types.CodeColumnNotFound,
					"栏目未登记: 项目 "+projCode+" 栏目 "+colCode)
			default:
				slog.Error("会话存在性校验失败", "err", err)
				types.WriteError(w, http.StatusInternalServerError, types.CodeInternalError, "会话存在性校验失败")
			}
			return
		}

		// ③ upsert（§7.1 步骤 2 SQL 在 store 层）：四头非空已由 ① 保证、
		// FK 由 ② 保证，ErrSessionInvalid 理论不可达，500 兜底。
		up, err := s.st.UpsertSession(ref.ProjectID, ref.ColumnID, sessName, role)
		if err != nil {
			slog.Error("会话心跳写入失败", "err", err)
			types.WriteError(w, http.StatusInternalServerError, types.CodeInternalError, "会话心跳写入失败")
			return
		}

		// ④ 事件审计（低频：仅首次/role 变更；三弱关联 id 取 upsert 返回会话行，
		// 公共字段一次装配，分支只填 action+detail——B1-4 审查 Minor 1 收敛）。
		e := store.AuditEntry{
			ProjectID: ref.ProjectID,
			ColumnID:  ref.ColumnID,
			SessionID: up.Session.ID,
		}
		switch {
		case up.Created:
			e.Action = store.AuditSessionAutoRegister
			e.Detail = auditDetailJSON(sessionSnapshot{
				Project: projCode, Column: colCode, Session: sessName, Role: role,
			})
		case up.RoleChanged:
			e.Action = store.AuditRoleChange
			e.Detail = auditDetailJSON(roleChange{Before: up.OldRole, After: role})
		}
		if e.Action != "" {
			if err := s.writeHeartbeatAudit(e); err != nil {
				slog.Error("会话审计写入失败", "err", err)
				types.WriteError(w, http.StatusInternalServerError, types.CodeInternalError, "会话审计写入失败")
				return
			}
		}

		// ⑤ 放行：注入操作会话 id 进 context（B1-5 登记写路径审计弱关联
		// session_id 消费——handler 免再查 sessions 表，零额外往返）。
		ctx := context.WithValue(r.Context(), ctxKeySessionID{}, up.Session.ID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ctxKeySessionID context 注入键（私有类型防跨包碰撞）。

type ctxKeySessionID struct{}

// sessionIDFromCtx 取心跳中间件注入的操作会话 id；未注入（handler 单测直调等）
// 返回 0，语义=系统动作（audit_log.session_id 的 0 值口径，§3.2 表 10 注）。

func sessionIDFromCtx(ctx context.Context) int64 {
	id, _ := ctx.Value(ctxKeySessionID{}).(int64)
	return id
}

// sessionSnapshot session.auto_register 审计 detail 快照（b1-spec §二 建议口径：
// 会话四元组按四头原值记录）。

type sessionSnapshot struct {
	Project string `json:"project"`
	Column  string `json:"column"`
	Session string `json:"session"`
	Role    string `json:"role"`
}

// roleChange session.role_change 审计 detail 快照（D5：before/after）。

type roleChange struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

// auditDetailJSON 序列化审计 detail（全 string 字段 struct 实际不可达 marshal
// 失败，兜底 "{}" 对齐 DDL DEFAULT '{}' 形状）。

func auditDetailJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// writeHeartbeatAudit 落心跳事件审计；错误原样返回，由调用方统一 500 短路
// （AC1.4/D5 留痕强制：审计静默丢失即违规，fail-fast 优于带病放行）。

func (s *Server) writeHeartbeatAudit(e store.AuditEntry) error {
	_, err := s.st.InsertAudit(e)
	return err
}

// bearerPrefix Authorization 头的标准 scheme 前缀。RFC 7235：auth-scheme 大小写

// 不敏感，故取值时按 EqualFold 比对（"bearer xxx" / "BEARER xxx" 同样接受）。

const bearerPrefix = "Bearer "

// tokenExemptPaths token 鉴权豁免面（B6-T1 裁定口径，path 精确匹配——非前缀、
// 非后缀，/api/v1/ping/ 等近邻路径一律不豁免）：
//   - /api/v1/ping、/api/v1/version：健康检查（探活客户端/负载均衡无 token 配置，
//     version 与 ping 同判 B6-T1）；
//   - /、/app.js、/dialog.js、/style.css：看板页面本体与静态资产（B5-T1 扩展两键，
//     B5-4 拆分增 dialog.js）——同一豁免语义：技术设计 §2.2 #23「页面本身无敏感
//     数据，数据全走 API」，AC14.6「token 校验在数据口」；资产不豁免则开态下
//     浏览器拿不到 JS/CSS，页面必然白屏。
var tokenExemptPaths = map[string]struct{}{
	"/api/v1/ping":    {},
	"/api/v1/version": {},
	"/":               {},
	"/app.js":         {},
	"/dialog.js":      {},
	"/style.css":      {},
}

// tokenAuth Bearer Token 鉴权中间件（AC17.3 服务端 / AC14.6 服务端半；§2.1 B6 挂点）：
//   - TokenEnabled=false（默认）直通——拷贝即跑零配置可用（AC17.2）；
//   - true 时校验双形态凭证：Authorization: Bearer <token> 头优先，?token= 查询参数
//     兜底（看板首次经 URL 进入后 JS 存 localStorage 转头传递，AC14.6）；比对用
//     subtle.ConstantTimeCompare 常量时间比较，防时序侧信道逐字节猜 token；
//   - 豁免面（tokenExemptPaths）无条件放行；
//   - 其余路径无/错凭证一律 401 auth_required，先于 mux 判定——未注册数据口得 401
//     而非 404，鉴权失败不泄露路径注册状态（AC14.6「数据口拒」判据）。
//
// 链序：tokenAuth 在 mux 外层、B1 会话心跳外层（未过鉴权不刷心跳，§2.1 组装固化）。

func tokenAuth(auth *config.AuthConfig) middleware {
	if !auth.TokenEnabled {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, exempt := tokenExemptPaths[r.URL.Path]; exempt {
				next.ServeHTTP(w, r)
				return
			}
			provided := extractToken(r)
			if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(auth.Token)) != 1 {
				types.WriteError(w, http.StatusUnauthorized, types.CodeAuthRequired, "缺少或错误的访问令牌")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// extractToken 双形态取凭证：Authorization: Bearer <token> 头优先（scheme 大小写
// 不敏感；头存在但非 Bearer 形态视为未用此形态，回退 query——「兜底」取值序），
// 无头形态时取 ?token= 查询参数。两形态均无/均为空返回 ""。

func extractToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); len(v) >= len(bearerPrefix) &&
		strings.EqualFold(v[:len(bearerPrefix)], bearerPrefix) {
		return strings.TrimSpace(v[len(bearerPrefix):])
	}
	return r.URL.Query().Get("token")
}
