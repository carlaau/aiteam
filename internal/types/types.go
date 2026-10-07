package types

import (
	"encoding/json"
	"net/http"
)

// ContentTypeJSON API 响应统一 Content-Type（技术设计 §2.1：请求/响应一律此值）。
const ContentTypeJSON = "application/json; charset=utf-8"

// 身份四头（§2.1）：CLI 每个请求自动附带（来自命令行身份参数），服务端心跳
// 中间件据此 upsert 会话+刷心跳（§7.1）。server/client 共用契约常量，勿散写
// 字面量（B1-7 client 注入同消费）。
const (
	HeaderAiteamProject = "X-Aiteam-Project" // 项目 code
	HeaderAiteamColumn  = "X-Aiteam-Column"  // 栏目 code（项目内唯一）
	HeaderAiteamSession = "X-Aiteam-Session" // 会话名（--session 显式名）
	HeaderAiteamRole    = "X-Aiteam-Role"    // 角色（前缀轻校验：controller / executor / executor_<标识>，§7.1；类广播用 --bus 不走 role 目标）
)

// 通用错误码（§2.4）：全部端点共有的机器码集中此处；端点特有码随各业务域定义。
const (
	CodeNotFound         = "not_found"          // 路径未注册（404）
	CodeMethodNotAllowed = "method_not_allowed" // 方法不匹配（405）
	CodeBodyTooLarge     = "body_too_large"     // 请求体超上限（413）
	CodeInternalError    = "internal_error"     // 服务端内部错误（500，全端点通用兜底）
)

// B1 登记域错误码（§2.4 归组）：404 存在性族；missing_header 为总表外新增细分码
// （400 组「请求参数问题」语义，§7.1 心跳中间件特有——错误信息指明缺哪个头），
// 收口批回写 §2.4 总表。
const (
	CodeMissingHeader   = "missing_header"    // 身份四头缺失（400，§7.1 中间件）
	CodeInvalidRole     = "invalid_role"      // role 形态非法（400，§7.1 中间件前缀轻校验：controller / executor / executor_<标识>）
	CodeProjectNotFound = "project_not_found" // 项目不存在（404，AC2.2）
	CodeColumnNotFound  = "column_not_found"  // 栏目不存在（404，AC2.2）
)

// B1 登记写路径错误码（§2.4 逐字面量）：409 语义冲突族 + 400 参数问题族
// （bad_json/param_invalid 为总表 400 组既有码；*_exists 为总表 409 组既有码）。
const (
	CodeBadJSON       = "bad_json"       // 请求体非法 JSON（400，§2.4）
	CodeParamInvalid  = "param_invalid"  // 请求参数缺失/非法（400，§2.4：缺 code、空更新、布尔解析失败）
	CodeProjectExists = "project_exists" // 项目 code 已存在（409，AC1.2）
	CodeColumnExists  = "column_exists"  // 栏目 code 冲突（409，AC2.1）
)

// B2 消息域错误码（§2.4 逐字面量）：#9 send 特有语义——400 参数族两码 +
// 404 目标会话 + 409 归档拒绝族（AC1.3/AC2.3 服务端判据，B2-T2 落动作端点）。
// CodeSessionNotFound 属 §2.4 404 存在性族既有码（总表 session_not_found），
// B2-4 #10/#11 的消费视角会话参数未注册时启用（send 的目标会话沿用
// target_session_not_found 专属码，两码语义分立）。
// CodeAgentConflict 为 2026-10-03 增补令新增（role 规约闸，技术设计 §2.2 #9
// 已同步）：direct 目标格子 ≥2 活会话拒投，归 §2.4 409「语义冲突/状态拒绝」组。
const (
	CodeRoleInvalid           = "role_invalid"             // direct 目标角色空白（400）
	CodeBodyEmpty             = "body_empty"               // 消息正文为空（400）
	CodeTargetSessionNotFound = "target_session_not_found" // chat 目标会话未注册（404）
	CodeProjectArchived       = "project_archived"         // 项目已归档，send/poll 拒绝（409，AC1.3）
	CodeColumnArchived        = "column_archived"          // 栏目已归档，收发拒绝（409，AC2.3）
	CodeSessionNotFound       = "session_not_found"        // 消费视角会话未注册（404，§2.4 404 族）
	CodeAgentConflict         = "agent_conflict"           // direct 目标格子 ≥2 活会话拒投（409，2026-10-03 增补·role 规约闸）
)

// B2-5 回执/历史错误码（§2.4 逐字面量）：#12 特有语义二码。already_recepted
// 不设常量——端点表标注其 HTTP 表现为「200 幂等返回已有记录」，按 §2.1
// 200=成功 data 形状响应（AC9.2「重复回执幂等不报错」），非 4xx/5xx 错误形状
// 无机器码挂点；B2-7 CLI 如需程序化识别幂等命中再补（对拍响应逐字段一致）。
const (
	CodeMessageNotFound = "message_not_found" // 消息 seq 不存在（404）
	CodeNotBlockLevel   = "not_block_level"   // 非 block 级不可回执（409，AC7.4）
)

// B6 token 鉴权错误码（§2.1 鉴权行）：401 统一机器码。
const (
	CodeAuthRequired = "auth_required" // 缺失/错误的访问令牌（401，B6 token 鉴权 AC17.3/AC14.6）
)

// DataResponse 成功响应统一包裹（§2.1）：{"data": {...}}，HTTP 2xx。
type DataResponse struct {
	Data any `json:"data"`
}

// ErrorBody 错误明细（§2.1）：code=机器码（§2.4 枚举），message=人类可读中文。
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorResponse 错误响应统一包裹（§2.1）：{"error": {...}}，HTTP 4xx/5xx。
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// PingData 端点 #29 GET /api/v1/ping 的 data 载荷（§2.2 #29）。
// now=服务端时间（ISO8601 UTC 秒级，经 NowUTC 注入点生成）。
type PingData struct {
	Version string `json:"version"`
	Now     string `json:"now"`
}

// VersionData 端点 #30 GET /api/v1/version 的 data 载荷（§2.2 #30）。
// commit 构建提交号，未经 ldflags 注入时为占位空串。
type VersionData struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Goos    string `json:"goos"`
}

// WriteData 输出成功响应（§2.1 成功形状）：统一设置 Content-Type 与状态码。
func WriteData(w http.ResponseWriter, status int, v any) {
	writeJSON(w, status, DataResponse{Data: v})
}

// WriteError 输出错误响应（§2.1 错误形状）：code 为 §2.4 机器码，msg 为人类可读中文。
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorResponse{Error: ErrorBody{Code: code, Message: msg}})
}

// writeJSON 序列化并写出 JSON 响应体。Marshal 失败理论上不可达（载荷均为
// 可序列化结构体），兜底 500 纯文本防静默空响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "响应序列化失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
