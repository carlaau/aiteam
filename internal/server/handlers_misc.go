package server

import (
	"net/http"
	"runtime"
	"strconv"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// handlePing GET /api/v1/ping（技术设计 §2.2 #29）：无鉴权探活，返回版本与服务端时间。
func (s *Server) handlePing(w http.ResponseWriter, _ *http.Request) {
	types.WriteData(w, http.StatusOK, types.PingData{
		Version: s.version,
		Now:     types.NowUTC(), // 服务端时间注入点：测试可替换为固定值
	})
}

// buildCommit 构建提交号：默认占位空串，B7 起经 ldflags 单点注入（-X aiteam/internal/server.buildCommit=...）。
var buildCommit = ""

// handleVersion GET /api/v1/version（技术设计 §2.2 #30）：独立于 ping 的版本口，
// 便于脚本区分用途（探活 vs 版本核对）。
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	types.WriteData(w, http.StatusOK, types.VersionData{
		Version: s.version,
		Commit:  buildCommit,
		Goos:    runtime.GOOS,
	})
}

// ---- #27 GET /api/v1/sessions ----

// sessionRow 端点 #27 列表行（§2.2 #27 字段面逐字段）：project/column 为 JOIN
// 装配的项目/栏目 code（store.Session.ProjectCode/ColumnCode，B1-3 单条 SELECT
// 装配）；sentinels 每会话哨兵清单（id/role/alive 三键——b3-spec #27「sentinels
// 数组填充」B3-5 闭合 B1 恒空数组半成品；无哨兵=空数组非 null）。
type sessionRow struct {
	ID         int64                   `json:"id"`
	Project    string                  `json:"project"`
	Column     string                  `json:"column"`
	Name       string                  `json:"name"`
	Role       string                  `json:"role"`
	LastSeenAt string                  `json:"last_seen_at"`
	Alive      bool                    `json:"alive"`
	Sentinels  []store.SessionSentinel `json:"sentinels"`
}

// sessionListData 端点 #27 响应 data。
type sessionListData struct {
	Sessions []sessionRow `json:"sessions"`
}

// handleSessionList 端点 #27（§2.2 #27 心跳面数据口，AC12.1 观测；status
// overview 内嵌同源数据）：可选 project 过滤（项目 code，未命中 404
// project_not_found——store 哨兵经 writeStoreError 映射）；alive 失联计算在 SQL
// 内（§7.2：now-last_seen_at > COALESCE(项目级阈值, 服务默认)，严格大于即失联），
// now=服务端注入时钟、默认阈值=config session.heartbeat_timeout_sec；哨兵活性=
// 一次全量查询+内存按 session 分组装配（§3.7 尾注往返纪律，禁逐会话循环查——
// B3-5 填充后 #27 共 2 条往返：ListSessions+ListSentinelsWithLiveness）。
func (s *Server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	now := types.NowUTC() // 失联与哨兵活性共用同一判定时刻（时钟一致性）
	sessions, err := s.st.ListSessions(r.URL.Query().Get("project"),
		now, s.cfg.Session.HeartbeatTimeoutSec)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	// 哨兵全量+分组（按 id 序天然保序——ListSentinelsWithLiveness ORDER BY id，
	// 组内哨兵 id 升序与 #17 内嵌口径一致）；活性阈值=服务配置 sentinel_timeout_sec。
	// 元素复用 store.SessionSentinel（#27 与 overview 内嵌同源数据——b3-spec 行 44；
	// 与 sessionSentinelRow 两类型字段/tag 实证一致后收敛，独立 DTO 已删防两层漂移）。
	sentinels, err := s.st.ListSentinelsWithLiveness(now, int64(s.cfg.Watch.SentinelTimeoutSec))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	sentinelsBySession := make(map[int64][]store.SessionSentinel, len(sentinels))
	for _, st := range sentinels {
		sentinelsBySession[st.SessionID] = append(sentinelsBySession[st.SessionID],
			store.SessionSentinel{ID: st.ID, Role: st.Role, Alive: st.Alive})
	}
	rows := make([]sessionRow, 0, len(sessions))
	for _, se := range sessions {
		sen := sentinelsBySession[se.ID]
		if sen == nil {
			sen = []store.SessionSentinel{} // 无哨兵=空数组非 null（响应面契约）
		}
		rows = append(rows, sessionRow{
			ID:         se.ID,
			Project:    se.ProjectCode,
			Column:     se.ColumnCode,
			Name:       se.Name,
			Role:       se.Role,
			LastSeenAt: se.LastSeenAt,
			Alive:      se.Alive,
			Sentinels:  sen,
		})
	}
	types.WriteData(w, http.StatusOK, sessionListData{Sessions: rows})
}

// ---- #28 GET /api/v1/audit ----

// auditRow 端点 #28 列表行（§2.2 #28 字段面逐字段）：session/project/column 为
// audit_log 三弱关联 id（值面照 store.AuditEntry=§3.2 表 10 数据面；0=系统动作/
// 无对应实体——表 10 注「弱关联不设 FK」，直插合法）。名称 JOIN 装配不在本批
// （b1-spec §七「#28 单条 SELECT+LIMIT」，逐行查名称即 N+1 违规；后续批次如需
// 在 store 补批量装配查询）。detail=JSON 快照文本原样透传（store 不校验形状）。
type auditRow struct {
	ID        int64  `json:"id"`
	Action    string `json:"action"`
	Session   int64  `json:"session"`
	Project   int64  `json:"project"`
	Column    int64  `json:"column"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"created_at"`
}

// auditListData 端点 #28 响应 data。
type auditListData struct {
	Entries []auditRow `json:"entries"`
}

// handleAuditList 端点 #28（§2.2 #28 操作留痕查询，AC1.4/AC2.4 机械核验口）：
// 过滤 project=项目 code（CLI --project 同型口径；handler 先 GetProjectByCode 换
// id，未命中 404 project_not_found——对齐 #27 过滤语义）、action 精确匹配；limit
// 缺省走 store 默认 100（AuditLimitDefault）、超界由 store clamp 1000
// （AuditLimitMax 数据面收敛，「最大 1000」为钳制非报错）；显式 ≤0/非数字 400
// param_invalid（防「看似成功实未生效」，对齐 #2 负阈值拒绝先例）。按 id 倒序
// （最新在前，store idx_audit_project 支撑）。
func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var limit int
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
				"limit 须为正整数（缺省 100，最大 1000）")
			return
		}
		limit = n
	}
	var projectID int64
	if code := q.Get("project"); code != "" {
		p, err := s.st.GetProjectByCode(code)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		projectID = p.ID
	}
	entries, err := s.st.QueryAudit(projectID, q.Get("action"), limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	rows := make([]auditRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, auditRow{
			ID:        e.ID,
			Action:    e.Action,
			Session:   e.SessionID,
			Project:   e.ProjectID,
			Column:    e.ColumnID,
			Detail:    e.Detail,
			CreatedAt: e.CreatedAt,
		})
	}
	types.WriteData(w, http.StatusOK, auditListData{Entries: rows})
}
