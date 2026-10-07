package server

import (
	"fmt"
	"net/http"
	"strconv"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 本文件为进度上报域（FR23）端点 #31/#32（§2.2 逐字段、b8-spec §1.1）。
// 错误映射复用 handler_projects.go 共用 writeStoreError，请求体解析复用
// decodeJSONBody；#31 的身份四头校验/隐式注册/心跳刷新不在 handler——
// sessionHeartbeat 中间件挂链即达（「上报即心跳」，handler 消费其注入的
// session id，零额外会话查询）。

// maxSummaryBytes 端点 #31 summary 上限 512B（§2.2 #31「summary(可选≤512B)」）：
// 按 UTF-8 编码字节数计（Go string len 即字节长，与 CLI --summary 透传语义一致）。
const maxSummaryBytes = 512

// ---- #31 POST /api/v1/progress ----

// progressCreatedData 端点 #31 响应 data（§2.2 #31 字段面恰两键：{id,created_at}
// ——store InsertProgress RETURNING 直出面，id 自增/created_at 服务端注入时钟）。
type progressCreatedData struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
}

// handleProgressPost 端点 #31（§2.2 #31，FR23 手动层）：写一条进度上报，成功
// 201 Created。链路分工：四头必填/存在性短路/隐式注册+心跳（上报即心跳）由
// sessionHeartbeat 中间件先行完成，本 handler 只做字段校验与 INSERT——
//   - test_status 预校验合法枚举（空=未提供/pass/fail/unknown）：非法值在此
//     400 param_invalid 出明确 4xx（放行到 store 会撞 DDL CHECK 透成 500，
//     语义错位——校验主责在 handler，CHECK 约束仅数据面兜底）；
//   - summary ≤512B（按字节计），超限 400 param_invalid；
//   - batch/task 不做必填校验：post-commit hook 自动层「batch/task 空」是
//     设计形态（b8-spec §1.1），列本身可空（表 11 DDL）；
//   - INSERT 消费中间件注入的操作会话 id（ctx 注入，免再查 sessions——
//     b8-spec §七往返账「#31 = 中间件 3+业务 1=4 语句/次」）。
func (s *Server) handleProgressPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Batch      string `json:"batch"`
		Task       string `json:"task"`
		CommitHash string `json:"commit_hash"`
		Branch     string `json:"branch"`
		TestStatus string `json:"test_status"`
		Summary    string `json:"summary"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	switch req.TestStatus {
	case "", store.TestStatusPass, store.TestStatusFail, store.TestStatusUnknown:
		// 合法：空串=未提供（落 NULL），三枚举对齐 DDL CHECK。
	default:
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
			"test_status 须为 pass/fail/unknown 之一（缺省=未提供）")
		return
	}
	if len(req.Summary) > maxSummaryBytes {
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
			fmt.Sprintf("summary 超过 %dB 上限", maxSummaryBytes))
		return
	}
	got, err := s.st.InsertProgress(store.ProgressReport{
		SessionID:  sessionIDFromCtx(r.Context()),
		Batch:      req.Batch,
		Task:       req.Task,
		CommitHash: req.CommitHash,
		Branch:     req.Branch,
		TestStatus: req.TestStatus,
		Summary:    req.Summary,
	})
	if err != nil {
		s.writeStoreError(w, err) // 兜底 500（FK/CHECK 已由中间件+预校验挡在前面）
		return
	}
	types.WriteData(w, http.StatusCreated, progressCreatedData{ID: got.ID, CreatedAt: got.CreatedAt})
}

// ---- #32 GET /api/v1/progress ----

// progressRow 端点 #32 列表行（§2.2 #32 字段面恰九键）：session 为 JOIN 装配的
// 会话名（store ListProgress 内连接装配）；无 stale 键——stale 三态仅
// LatestProgressBySession 消费面（#17 聚合/看板，B3-5 消费），历史明细不做
// 陈旧标注（b8-spec §1.1 AC23.3 口径）。
type progressRow struct {
	ID         int64  `json:"id"`
	Session    string `json:"session"`
	Batch      string `json:"batch"`
	Task       string `json:"task"`
	CommitHash string `json:"commit_hash"`
	Branch     string `json:"branch"`
	TestStatus string `json:"test_status"`
	Summary    string `json:"summary"`
	CreatedAt  string `json:"created_at"`
}

// progressListData 端点 #32 响应 data。
type progressListData struct {
	Items []progressRow `json:"items"`
}

// handleProgressList 端点 #32（§2.2 #32，看板/总控巡检数据源）：全条件可选——
//   - project=项目 code：handler 先 GetProjectByCode 换 id（#28 同型换装先例），
//     未登记 404 project_not_found（writeStoreError 哨兵映射）；
//   - session=会话名精确匹配（空=全部会话）；
//   - limit 缺省走 store 默认 100（ProgressLimitDefault）、超界由 store clamp
//     1000（ProgressLimitMax 数据面收敛，「最大 1000」为钳制非报错）；显式
//     ≤0/非数字 400 param_invalid（防「看似成功实未生效」，#28 同型口径）。
//     倒序由 store 保证（created_at DESC,id DESC——同秒多报按 id 决序）。
//
// 四头必填与会话心跳照 §7.1「全部 /api/v1 端点」由中间件先行（GET 亦不豁免，
// ping/version 之外无例外——身份四头是全端点统一契约，非写端点特有）。
func (s *Server) handleProgressList(w http.ResponseWriter, r *http.Request) {
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
			s.writeStoreError(w, err) // ErrProjectNotFound→404
			return
		}
		projectID = p.ID
	}
	reports, err := s.st.ListProgress(projectID, q.Get("session"), limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	rows := make([]progressRow, 0, len(reports))
	for _, p := range reports {
		rows = append(rows, progressRow{
			ID:         p.ID,
			Session:    p.SessionName,
			Batch:      p.Batch,
			Task:       p.Task,
			CommitHash: p.CommitHash,
			Branch:     p.Branch,
			TestStatus: p.TestStatus,
			Summary:    p.Summary,
			CreatedAt:  p.CreatedAt,
		})
	}
	types.WriteData(w, http.StatusOK, progressListData{Items: rows})
}
