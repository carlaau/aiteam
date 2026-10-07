// handler_window.go —— B4-5 时间窗域两端点（技术设计 §2.2 #21/#22）。
//
// 写口（#21）复用资源域身份解析面（s.resolveSession+hdrProject/hdrColumn/hdrSession
// 同包常量，Role 头仍不参与定位）；读口（#22）权威查询不强制身份头。
// 判窗算法单一实现在 store（InWindow/ListWindowStatus，§6.2/§6.3），本文件纯映射。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 窗域特有错误码（§2.4：端点特有码随业务域就近定义；column_not_found 等通用码
// 复用 handler_resources.go 同包常量，不重复定义）。
const (
	codeProjectNotFound   = "project_not_found"
	codeStageInvalid      = "stage_invalid"
	codeTimeFormatInvalid = "time_format_invalid"
	codeParamInvalid      = "param_invalid" // §2.4 400 池通用码：重复 stage 等结构性非法
)

// setWindowsReq #21 请求体（§2.2 #21）。覆盖式：items 即该域全部窗，缺席=删除；
// 空 items=清空该域（store.SetWindows 已支持）。
type setWindowsReq struct {
	Items []windowItemReq `json:"items"`
}

// windowItemReq 单窗（请求字段名 start/end；store.WindowInput 审计快照 tag 为
// from/to——两形态各自冻结，handler 负责映射，不改 store 快照契约）。
type windowItemReq struct {
	Stage   string `json:"stage"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Enabled bool   `json:"enabled"`
}

// windowItemData #21 响应 items 条目（落库后回读形态，与请求同构）。
type windowItemData struct {
	Stage   string `json:"stage"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Enabled bool   `json:"enabled"`
}

// setWindowsData #21 响应 data。
type setWindowsData struct {
	Items []windowItemData `json:"items"`
}

// windowsNowData #22 响应 data（entries 空=空数组非 null，由 handler 保证非 nil）。
type windowsNowData struct {
	AsOf    string            `json:"as_of"`
	Entries []windowEntryData `json:"entries"`
}

// windowEntryData #22 聚合条目（§2.2 #22；window 恒在字段，未配置/停用时值为 null）。
type windowEntryData struct {
	Project string          `json:"project"`
	Column  string          `json:"column"`
	Stage   string          `json:"stage"`
	Status  string          `json:"status"` // allowed | waiting
	Window  *windowSpanData `json:"window"` // nil → JSON null
}

// windowSpanData 窗快照（start>end=跨午夜原样透出，AC3.3）。
type windowSpanData struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// validStageFormat 阶段名格式校验：^S\d+$（B4-T2 裁定——S9/S10 等两位数收，
// 不硬限 S0~S7；"S"/"X4"/空串拒）。
func validStageFormat(stage string) bool {
	if len(stage) < 2 || stage[0] != 'S' {
		return false
	}
	for i := 1; i < len(stage); i++ {
		if stage[i] < '0' || stage[i] > '9' {
			return false
		}
	}
	return true
}

// validHHMM 时间格式校验：HH:MM 两位等宽（小时 00-23、分钟 00-59；信箱#8 裁定
// ——反例 9:00/25:00/23:5/0900 一律拒）。start>end 是跨午夜语义，此处不比大小
// （AC3.3：大小关系即语义，格式合法即收）。
func validHHMM(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	hh, mm := s[0:2], s[3:5]
	for _, i := range []int{0, 1} {
		if hh[i] < '0' || hh[i] > '9' || mm[i] < '0' || mm[i] > '9' {
			return false
		}
	}
	return hh <= "23" && mm <= "59" // 等宽两位数字串字典序=数值序
}

// handleSetWindows PUT /api/v1/projects/{code}/columns/{col}/windows（§2.2 #21）：
// 覆盖式写阶段时间窗。
// 错误序：bad_json 400 → auth_required 401（身份门槛先于域解析，对齐 #18）
// → project_not_found 404 → column_not_found 404 → 逐 item 校验 400
// （stage_invalid / time_format_invalid）→ param_invalid 400（同提交重复 stage）
// → internal_error 500 兜底。
//
// 校验序口径（锚定用例「校验序锚定_*」）：逐 item 按数组序、item 内 stage 先于
// time——首个违规即返，不收集全量。
//
// scope 定位口径（锚定用例「scope=project_落column维度0_col不参与定位」）：
// 默认 scope 写栏目级域（路径 {col} 参与定位，404 两级分列：项目不存在
// project_not_found、栏目不存在 column_not_found——{col} 为 TEXT code，非数字值
// 照 code 精确查询，查无即 404，对齐 #19「解析不了=不存在」口径，锚定用例
// 「col非数字_404口径对齐19」）；?scope=project 写项目级默认窗（column 维度 0），
// 此时 {col} 不参与定位（不校验存在性）。
// resolveWindowDomain 窗域两 handler 共享的域解析面（base.md §3.4 双胞胎收敛：
// #21 PUT 写口与 GET 读口同参同判据，镜像同步=抽共享的条件触发；校验序/scope
// 锚定/两级 404 由 TestSetWindows/TestGetWindows 锚定用例共同抓回归）：
// 先项目后栏目（两级 404 分列）；scope=project 时栏目级不参与定位（columnID=0
// 即项目级域）。错误响应在 helper 内写出，ok=false 即返。
func (s *Server) resolveWindowDomain(w http.ResponseWriter, r *http.Request) (projectID, columnID int64, ok bool) {
	projCode := r.PathValue("code")
	projectID, err := s.st.ResolveProjectID(projCode)
	if errors.Is(err, store.ErrProjectNotFound) {
		types.WriteError(w, http.StatusNotFound, codeProjectNotFound,
			fmt.Sprintf("项目 %s 不存在", projCode))
		return 0, 0, false
	}
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "项目解析失败")
		return 0, 0, false
	}
	columnID = int64(0) // 项目级默认窗维度
	if r.URL.Query().Get("scope") != "project" {
		ref, cerr := s.st.ResolveColumn(projCode, r.PathValue("col"))
		if errors.Is(cerr, store.ErrColumnNotFound) {
			types.WriteError(w, http.StatusNotFound, codeColumnNotFound,
				fmt.Sprintf("栏目 %s 在项目 %s 下不存在", r.PathValue("col"), projCode))
			return 0, 0, false
		}
		if cerr != nil {
			types.WriteError(w, http.StatusInternalServerError, codeInternalError, "栏目解析失败")
			return 0, 0, false
		}
		columnID = ref.ColumnID
	}
	return projectID, columnID, true
}

func (s *Server) handleSetWindows(w http.ResponseWriter, r *http.Request) {
	var req setWindowsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, codeBadJSON, "请求体不是合法 JSON")
		return
	}
	sessID, ok := s.resolveSession(w, r)
	if !ok {
		return // 401 已写出
	}
	projectID, columnID, ok := s.resolveWindowDomain(w, r)
	if !ok {
		return // 两级 404/500 已写出（scope 定位口径见 resolveWindowDomain doc 注）
	}

	// 逐 item 校验（数组序、item 内 stage 先 time；首个违规即返）。
	for _, it := range req.Items {
		if !validStageFormat(it.Stage) {
			types.WriteError(w, http.StatusBadRequest, codeStageInvalid,
				fmt.Sprintf("stage %q 非法：须为 S+数字（如 S4/S10）", it.Stage))
			return
		}
		if !validHHMM(it.Start) || !validHHMM(it.End) {
			types.WriteError(w, http.StatusBadRequest, codeTimeFormatInvalid,
				fmt.Sprintf("时间 %q/%q 非法：须为 HH:MM 两位等宽（小时 00-23、分钟 00-59）", it.Start, it.End))
			return
		}
	}

	windows := make([]store.WindowInput, 0, len(req.Items))
	for _, it := range req.Items {
		enabled := 0
		if it.Enabled {
			enabled = 1
		}
		windows = append(windows, store.WindowInput{
			Stage: it.Stage, From: it.Start, To: it.End, Enabled: enabled,
		})
	}

	// 服务端时钟：types.NowUTC 为包级注入点（测试替换即生效，不读墙钟）。
	now := types.NowUTC()
	if err := s.st.SetWindows(projectID, columnID, windows, sessID, now); err != nil {
		if errors.Is(err, store.ErrDuplicateStage) { // store 预校验兜底（并发同域双写等）
			types.WriteError(w, http.StatusBadRequest, codeParamInvalid,
				"同一提交内存在重复 stage")
			return
		}
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "时间窗写入失败")
		return
	}

	// 落库后回读形态：SetWindows 覆盖式成功=落库即本次提交内容，直接映射。
	items := make([]windowItemData, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, windowItemData{
			Stage: it.Stage, Start: it.Start, End: it.End, Enabled: it.Enabled,
		})
	}
	types.WriteData(w, http.StatusOK, setWindowsData{Items: items})
}

// handleGetWindows GET /api/v1/projects/{code}/columns/{col}/windows（技术设计
// #21 修订行：同 path 加 GET，B4-6b）——读当前域窗配置原始行（管理面全量：含
// enabled=false 停用行，不做 #22 判定态过滤），支撑 CLI window list（层级归属
// 显式）与 window set --clear --stage（先读后拼全量再 PUT 的单窗删除）。
// scope 定位口径与 PUT 同参对齐：?scope=project 读项目级域（{col} 不参与定位）；
// 默认读栏目级域。错误码同 PUT（bad_json 不适用无请求体 GET；身份门槛/两级 404/
// internal_error 500 兜底——解析逻辑镜像 handleSetWindows，冻结语义勿分叉）。
func (s *Server) handleGetWindows(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.resolveSession(w, r); !ok {
		return // 身份门槛同 PUT（401/400 已写出）
	}
	projectID, columnID, ok := s.resolveWindowDomain(w, r)
	if !ok {
		return // 两级 404 同 PUT（已写出）
	}

	rows, err := s.st.ListWindows(projectID, columnID)
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "时间窗配置查询失败")
		return
	}
	items := make([]windowItemData, 0, len(rows)) // 非 nil 保证：空域序列化为 []
	for _, row := range rows {
		items = append(items, windowItemData{
			Stage: row.Stage, Start: row.From, End: row.To, Enabled: row.Enabled != 0,
		})
	}
	types.WriteData(w, http.StatusOK, setWindowsData{Items: items}) // 两方法 data 同构
}

// handleWindowsNow GET /api/v1/windows/now（§2.2 #22）：「当前允许阶段」权威查询口。
// 读口宽容：不强制身份四头；project query 可选（省略=全部；不存在的项目=空 entries，
// 读口宽容对齐 #20 口径）。
// as_of 产生（§9.4 D4）：store.NowInTz(s.cfg.Timezone)——服务端配置时区
// （config.timezone，默认 Local，IANA 可配）下的当前 HH:MM；时区解析失败映射
// internal_error 500（配置错误 fail-fast，不静默兜底改钟面）。
// 聚合判定全在 store.ListWindowStatus（§6.3 单一实现），本 handler 纯映射零自有逻辑
// （同源判据见 handler_window_test.go TestSingleSource）。
func (s *Server) handleWindowsNow(w http.ResponseWriter, r *http.Request) {
	asOf, err := store.NowInTz(s.cfg.Timezone)
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError,
			fmt.Sprintf("服务端时区配置 %q 不可用", s.cfg.Timezone))
		return
	}
	rows, err := s.st.ListWindowStatus(asOf, r.URL.Query().Get("project"))
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "时间窗聚合查询失败")
		return
	}
	entries := make([]windowEntryData, 0, len(rows)) // 非 nil 保证：空集序列化为 []
	for _, row := range rows {
		e := windowEntryData{
			Project: row.ProjectCode, Column: row.ColumnCode, Stage: row.Stage, Status: row.Status,
		}
		if row.Window != nil { // nil 保持 null（未配置/停用无窗可判）
			e.Window = &windowSpanData{Start: row.Window.Start, End: row.Window.End}
		}
		entries = append(entries, e)
	}
	types.WriteData(w, http.StatusOK, windowsNowData{AsOf: asOf, Entries: entries})
}
