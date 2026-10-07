// handler_sentinels.go —— 哨兵三端点（B3-4）：#14 注册（B3-T4 机械闸）/ #15 poll 三合一
// （T3 双续期+hits 探测不推进位点）/ #16 注销幂等。
// 规格：技术设计 §2.2 #14/#15/#16、§7.1 T3 心跳链、§7.3 哨兵活性、D2 裁定（watch 5s/失活 15s）；
// b3-plan B3-4。数据面全在 store/sentinels.go（B3-1/B3-4）。
//
// 身份口径（总控 #10 终态）：X-Aiteam-* 四头直读。feat/b3 无 B1 中间件——会话三元组
// 查无即 401（B1 在位时中间件先行 upsert，此分支自然收窄）；#15 心跳续期不经中间件，
// handler 经 PingSentinelWithHeartbeat 自做 UPDATE（T3「哨兵活着=会话活着」）。
package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"aiteam/internal/config"
	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 身份 Role 头名收敛裁定（B3-8）：hdrRole 本地定义已删，改消费 B1 产物
// types.HeaderAiteamRole（§7.1 冻结头名单一来源，B1 中间件同源）——B3-4 期「Role
// 参与哨兵定位，与资源/窗域不参与定位不同」的差异在定位语义，不在头名字面；
// Role 头名现单一出口 types.HeaderAiteamRole。Project/Column/Session 三头同语义
// 常量已由 handler_resources.go 定义（B4 rebase 收敛：同值删定义同包直用）——
// 该三头仍为资源域本地字面量（资源域声明 Role 不参与定位故未定义常量），其收敛
// 与否不属哨兵域裁定面。

// 哨兵特有错误码收敛裁定（B3-8）：同值去重已完成——auth_required/bad_json/
// param_invalid/column_not_found/internal_error 与资源/窗域同值，已由
// handler_resources.go / handler_window.go 定义（B4 rebase 收敛），同包直用；
// 仅 sentinel_not_found 为哨兵域特有（types 错误码表无此码），本地保留。
const codeSentinelNotFound = "sentinel_not_found" // 哨兵不存在/已注销（404）

// defaultWatchIntervalSec watch 轮询间隔兜底常量（D2 裁定 5s）。
// #14 interval_sec 缺省=服务配置生效值（config.Watch.PollIntervalSec，Default()=5）；
// 配置为 0/负值时回退本常量（§9.3 默认口径，配置坏值不放大到端点行为）。
const defaultWatchIntervalSec = 5

// sentinelTimeoutSec 哨兵失活阈值生效值：服务配置 cfg.Watch.SentinelTimeoutSec
// （Default()=15，§7.3/D2 裁定）；配置 ≤0 时回退 store.DefaultSentinelTimeoutSec
// （status.go 既有常量导出，不另立新值——审查 K1 修复口径）。B3-T4 闸值与 #14 幂等
// 活哨兵判定同源此值——轮询节奏与哨兵活性判定同处一个时钟尺度，相容性校验才有意义。
func sentinelTimeoutSec(cfg *config.ServerConfig) int64 {
	if v := int64(cfg.Watch.SentinelTimeoutSec); v > 0 {
		return v
	}
	return store.DefaultSentinelTimeoutSec
}

// SentinelRegisterData #14 响应载荷（§2.2 #14：interval_sec=生效值回显——缺省时为
// 服务配置值，显式传入时为请求值）。b8-W2：幂等命中（200）增 holder 持有者诊断
// （omitempty——201 新建/接管路径无此键，响应面 additive 向后兼容）。
type SentinelRegisterData struct {
	SentinelID  int64               `json:"sentinel_id"`
	IntervalSec int64               `json:"interval_sec"`
	Holder      *SentinelHolderData `json:"holder,omitempty"`
}

// SentinelHolderData 幂等命中的持有者诊断（b8-W2①）：session=持有者会话名、
// last_ping_at=既有行 ping 时刻、age_sec=now−last_ping_at（types.NowUTC 同源钟，
// CLI 侧从响应值渲染不自行取时钟）。
type SentinelHolderData struct {
	Session    string `json:"session"`
	LastPingAt string `json:"last_ping_at"`
	AgeSec     int64  `json:"age_sec"`
}

// SentinelPollData #15 响应载荷（§2.2 #15：hits 空=未命中继续睡；now=服务端注入时钟）。
type SentinelPollData struct {
	Hits []store.Hit `json:"hits"`
	Now  string      `json:"now"`
}

// sentinelRegisterBody #14 请求体（可选）：IntervalSec 指针区分「缺省」与「显式 0」——
// 显式 0/负值=非法（无法表达轮询节奏），缺省=走服务配置。b8-W2：Force=true 显式
// 接管——幂等命中时同事务旧删新插（仅活哨兵命中分支消费；未命中=既有注册路径
// 不变，force no-op）。
type sentinelRegisterBody struct {
	IntervalSec *int64 `json:"interval_sec"`
	Force       bool   `json:"force"`
}

// handleSentinelRegister POST /api/v1/sentinels（§2.2 #14）：身份四头定位会话与目标信箱，
// 注册哨兵。interval_sec 可选：
//   - 缺省 → 服务配置 cfg.Watch.PollIntervalSec（默认 5s，D2）；
//   - 显式 → B3-T4 机械闸：3×interval > sentinel_timeout_sec（哨兵失活阈值，审查 K1
//     勘误口径——非会话失联阈值）→ 400 param_invalid。真缺口=60s 才 ping 一次在 15s
//     失活阈值下恒 dead：轮询间隔超失活阈值 1/3，哨兵活性判定失去预警意义。
//
// 注册幂等（信箱#12）：同 (column,role,session) 已有活哨兵 → 200 返回既有 sentinel_id
// （零写：不刷新时间列）+ holder 持有者诊断（b8-W2①）；force=true（b8-W2②）→ 同
// 事务旧删新插接管 → 201 新 id；无活哨兵（含无行/死行）→ RegisterSentinel upsert →
// 201（新插或复活——B3-T2 kill 重启即复活同 id；force 在此分支 no-op——既有注册
// 路径不变）。实现选择=注册前查活哨兵（FindAliveSentinel）而非改 upsert 返回形态：
// 幂等命中零写语义（不刷 started_at/last_ping_at）只有前置查才能表达，且 store
// upsert 的「命中即刷新」契约（B3-T2）原样保留给复活路径。
//
// 错误面：401 auth_required（Session 头缺失/三元组查无）/ 404 column_not_found /
// 400 bad_json / 400 param_invalid。审计面无（契约未列哨兵注册审计）。
func (s *Server) handleSentinelRegister(w http.ResponseWriter, r *http.Request) {
	project := r.Header.Get(hdrProject)
	column := r.Header.Get(hdrColumn)
	session := r.Header.Get(hdrSession)
	role := r.Header.Get(types.HeaderAiteamRole)
	if session == "" {
		types.WriteError(w, http.StatusUnauthorized, codeAuthRequired, "缺少 X-Aiteam-Session 身份头")
		return
	}
	if role == "" {
		types.WriteError(w, http.StatusBadRequest, codeParamInvalid, "缺少 X-Aiteam-Role 身份头（watch 目标信箱角色）")
		return
	}

	// 请求体可选：空体=缺省 interval；非空但非法 JSON → 400 bad_json。
	var body sentinelRegisterBody
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, codeBadJSON, "请求体读取失败")
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			types.WriteError(w, http.StatusBadRequest, codeBadJSON, "请求体不是合法 JSON")
			return
		}
	}

	// interval 生效值：缺省=服务配置（坏值回退常量 5）；显式非正=400。
	interval := int64(s.cfg.Watch.PollIntervalSec)
	if interval <= 0 {
		interval = defaultWatchIntervalSec
	}
	if body.IntervalSec != nil {
		if *body.IntervalSec <= 0 {
			types.WriteError(w, http.StatusBadRequest, codeParamInvalid, "interval_sec 须为正整数（秒）")
			return
		}
		interval = *body.IntervalSec
	}

	// B3-T4 机械闸（纯配置面判定，先于查库——非法 interval 无论幂等与否都拒绝）：
	// 3×interval ≤ sentinel_timeout_sec（恰等放行——`>` 严格大于才拒）。
	timeout := sentinelTimeoutSec(s.cfg)
	if 3*interval > timeout {
		types.WriteError(w, http.StatusBadRequest, codeParamInvalid,
			"interval_sec 须满足 3×interval ≤ 哨兵失活阈值：当前 3×"+
				strconv.FormatInt(interval, 10)+"="+strconv.FormatInt(3*interval, 10)+
				" > "+strconv.FormatInt(timeout, 10))
		return
	}

	// 前置探针 1：栏目存在性（项目/栏目查无 → 404；审查 K1/M-L2 后无阈值输出面）。
	columnID, err := s.st.LookupColumnID(project, column)
	if errors.Is(err, sql.ErrNoRows) {
		types.WriteError(w, http.StatusNotFound, codeColumnNotFound,
			"栏目在项目域不存在（project="+project+", column="+column+"）")
		return
	}
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "服务内部错误")
		return
	}

	// 前置探针 2：会话三元组定位（栏目存在而会话查无 → 401；B1 在位时中间件已先行 upsert）。
	loc, err := s.st.LookupSessionID(project, column, session)
	if errors.Is(err, store.ErrSessionMissing) {
		types.WriteError(w, http.StatusUnauthorized, codeAuthRequired,
			"会话未注册（project="+project+", column="+column+", session="+session+"）")
		return
	}
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "服务内部错误")
		return
	}

	// 注册幂等（信箱#12）：活哨兵在 → b8-W2 分流——
	//   - force=true：显式接管，同事务 DELETE 旧行+INSERT 新行（新 id）→ 201；
	//     旧孤儿哨兵下次 poll 404 sentinel_not_found 自然退出，一格一哨单例恢复。
	//     死行 15s 阈值已是设计内自动接管，孤儿（活进程）服务端无法区分——人判+
	//     显式接管（R3 裁定，不做自动接管）。
	//   - 缺省：200 既有 id 零写返回 + holder 持有者诊断（会话名/ping 时刻/年龄
	//     ——「谁在值班、还活着吗」的人判依据，CLI 據此打印接管指引）。诊断列随
	//     FindAliveSentinel 单语句带出（质量审查修 1）：无二次查询，故不存在「命中
	//     后持有者行被并发删除」的 500 竞态分支（store 层单语句原子读语义锚）。
	now := types.NowUTC()
	if h, alive, err := s.st.FindAliveSentinel(loc.SessionID, columnID, role, now, timeout); err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "服务内部错误")
		return
	} else if alive {
		if body.Force {
			newID, err := s.st.TakeoverSentinel(loc.SessionID, columnID, role, now)
			if err != nil {
				types.WriteError(w, http.StatusInternalServerError, codeInternalError, "服务内部错误")
				return
			}
			types.WriteData(w, http.StatusCreated, SentinelRegisterData{SentinelID: newID, IntervalSec: interval})
			return
		}
		types.WriteData(w, http.StatusOK, SentinelRegisterData{
			SentinelID:  h.ID,
			IntervalSec: interval,
			Holder:      &SentinelHolderData{Session: h.Session, LastPingAt: h.LastPingAt, AgeSec: h.AgeSec},
		})
		return
	}

	// 无活哨兵 → upsert 注册（新插或死哨兵复活，同 id，时间列刷新——B3-T2）→ 201。
	id, err := s.st.RegisterSentinel(loc.SessionID, columnID, role, now)
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "服务内部错误")
		return
	}
	types.WriteData(w, http.StatusCreated, SentinelRegisterData{SentinelID: id, IntervalSec: interval})
}

// handleSentinelPoll POST /api/v1/sentinels/{id}/poll（§2.2 #15，T3 三合一）：哨兵 ping +
// 会话心跳续期（双续期同事务）+ 新消息探测。query: column/role/session（探测目标信箱
// 与会话名），项目域取 X-Aiteam-Project 头。
// 不推进位点（AC10.4——hits 探测纯读，推进只归 #11 ack）。
// 错误面：404 sentinel_not_found（{id} 非数字/行不存在/归属不匹配——审查 I1）/ 401
// auth_required / 400 param_invalid（query 缺参——审查 I2）。
func (s *Server) handleSentinelPoll(w http.ResponseWriter, r *http.Request) {
	// {id} 非数字 → 404 sentinel_not_found（语义上即无此哨兵，不 500）。
	sentinelID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		types.WriteError(w, http.StatusNotFound, codeSentinelNotFound,
			"哨兵 id 须为数字（got "+r.PathValue("id")+"）")
		return
	}
	// Session 头非空校验（M-L1）：B1 缺位期最低身份保障（冻结契约「三个写端点身份门槛」
	// 原文要求，不可删）；query 三元组才是本端点探测定位的权威入参——B1 在位后四头
	// 校验/upsert 归中间件，此分支自然收窄。
	if r.Header.Get(hdrSession) == "" {
		types.WriteError(w, http.StatusUnauthorized, codeAuthRequired, "缺少 X-Aiteam-Session 身份头")
		return
	}
	q := r.URL.Query()
	qColumn, qRole, qSession := q.Get("column"), q.Get("role"), q.Get("session")

	// 缺参校验（审查 I2）：三 query 参任一为空即 400——空串落谓词会让 direct/bus 永不
	// 命中被 200 空轮掩盖（静默降级），也会让 401 消息出现「column=, session=」空插值。
	if qColumn == "" || qRole == "" || qSession == "" {
		types.WriteError(w, http.StatusBadRequest, codeParamInvalid,
			"query 须携带 column、role、session 三参数")
		return
	}

	// 身份门槛+探测定位合一：三元组=（project 头, query column, query session），
	// 一条查询带出探测所需三 id（§3.6 谓词 :p/:c/:s）。
	loc, err := s.st.LookupSessionID(r.Header.Get(hdrProject), qColumn, qSession)
	if errors.Is(err, store.ErrSessionMissing) {
		types.WriteError(w, http.StatusUnauthorized, codeAuthRequired,
			"会话未注册（column="+qColumn+", session="+qSession+"）")
		return
	}
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "服务内部错误")
		return
	}

	// 双续期（同事务）：ping 0 行（不存在/归属不匹配）→ ErrSentinelNotFound → 404，
	// 心跳不孤儿前进（审查 I1 绑定校验）。
	now := types.NowUTC()
	if err := s.st.PingSentinelWithHeartbeat(sentinelID, loc.SessionID, now); err != nil {
		if errors.Is(err, store.ErrSentinelNotFound) {
			types.WriteError(w, http.StatusNotFound, codeSentinelNotFound, "哨兵不存在或已注销")
			return
		}
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "服务内部错误")
		return
	}

	// 命中探测（纯 SELECT，seq 升序；空=未命中继续睡）。
	hits, err := s.st.PollHits(loc.ProjectID, loc.ColumnID, qRole, loc.SessionID)
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "服务内部错误")
		return
	}
	// b8-W3 命中留痕（尽力面）：非空 hits 时回写会话行 sentinel_last_hit_at（命中
	// 即注销哨兵行——留痕落会话行不落哨兵行；一轮命中至多 +1 UPDATE）。失败降级
	// 不阻塞 poll 响应（b7 cross_grid_hint 降级哲学同款），吞错留 WARN 观测面。
	if len(hits) > 0 {
		if err := s.st.MarkSentinelLastHit(loc.SessionID, now); err != nil {
			slog.Warn("sentinel_last_hit_at 留痕降级", "err", err)
		}
	}
	types.WriteData(w, http.StatusOK, SentinelPollData{Hits: hits, Now: now})
}

// handleSentinelDelete DELETE /api/v1/sentinels/{id}（§2.2 #16）：watch 正常退出注销。
// 重复注销 → 404 幂等语义（效果已达成——store DeleteSentinel 固化口径）。
// {id} 非数字 → 404 同口径。审计面无（契约未列哨兵注销审计）。
func (s *Server) handleSentinelDelete(w http.ResponseWriter, r *http.Request) {
	sentinelID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		types.WriteError(w, http.StatusNotFound, codeSentinelNotFound,
			"哨兵 id 须为数字（got "+r.PathValue("id")+"）")
		return
	}
	if err := s.st.DeleteSentinel(sentinelID); err != nil {
		if errors.Is(err, store.ErrSentinelNotFound) {
			types.WriteError(w, http.StatusNotFound, codeSentinelNotFound, "哨兵不存在或已注销")
			return
		}
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "服务内部错误")
		return
	}
	types.WriteData(w, http.StatusOK, struct{}{})
}
