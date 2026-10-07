package server

import (
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"aiteam/internal/assets"
	"aiteam/internal/config"
	"aiteam/internal/store"
	"aiteam/internal/types"
)

// Version 版本默认值：构建期未经 ldflags 注入时的兜底 "dev"（b7b-1 硬化收口）。
// 注入路径：build.sh -ldflags "-X aiteam/internal/server.Version=$VER"，
// $VER 默认日期戳 v0.1.0-dev.<yyyyMMddHHmm>（b7b-T1 裁定；S7 打 tag 后切 git describe）。
// 用 var 而非 const：go build -ldflags -X 只能注入 string 变量，const 会使
// 注入编译失败（审查 Important 2）。
var Version = "dev"

// maxBodyBytes 请求体上限 1MB（技术设计 §2.1：整包防线，防误用）。
const maxBodyBytes = 1 << 20

// middleware 中间件统一签名：B1 会话心跳 / B6 token 鉴权照此形状挂入中间件链。
type middleware func(http.Handler) http.Handler

// Server HTTP 服务端骨架（技术设计 §10）：ServeMux 组装+中间件+404/405 统一 JSON 兜底。
// st 仅装配（业务端点 B1 起消费）；cfg 的 auth 段供 token 鉴权中间件消费（B6-1 已接通）。
type Server struct {
	mux *http.ServeMux
	// methodsByPath pattern → 已注册方法集（注册期单线程写、运行期只读），405 兜底据此回 Allow 头。
	methodsByPath map[string][]string
	st            *store.Store
	cfg           *config.ServerConfig
	version       string
}

// handle 注册单个端点 + method-agnostic 同 pattern 405 兜底。
//
// 依赖 Go 1.22+ ServeMux 文档化语义（审查实测七场景等价的根据）：
//   - method-specific pattern（"GET /x"）比 method-agnostic pattern（"/x"）更具体，
//     同 pattern 两者共存时对应方法命中业务 handler、其余方法落到 405 兜底；
//   - GET pattern 隐式匹配 HEAD（HEAD 无独立注册）；
//   - 路径规范化 redirect（如 "//api/v1/ping"→307）是 ServeMux 匹配前置行为，
//     先于一切 pattern 匹配，不受兜底影响。
//
// ⚠️ 绕过本方法直接 mux 注册的端点不会进 404/405/413 统一 JSON 兜底
// （后续批次增端点一律经本方法，勿直接操作 s.mux）。
func (s *Server) handle(method, pattern string, h http.HandlerFunc) {
	if _, seen := s.methodsByPath[pattern]; !seen {
		s.methodsByPath[pattern] = nil
		// method-agnostic 兜底：同 pattern 其余方法 → 405 JSON + Allow（运行期只读 map）
		s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if allow := allowMethods(s.methodsByPath[pattern]); allow != "" {
				w.Header().Set("Allow", allow)
			}
			types.WriteError(w, http.StatusMethodNotAllowed, types.CodeMethodNotAllowed, "请求方法不被该端点支持")
		})
	}
	s.methodsByPath[pattern] = append(s.methodsByPath[pattern], strings.ToUpper(method))
	s.mux.Handle(method+" "+pattern, h)
}

// allowMethods 生成 Allow 头值：GET 隐式含 HEAD，一并列入与标准库 405 行为对齐。
func allowMethods(methods []string) string {
	hasGET := false
	for _, m := range methods {
		hasGET = hasGET || m == http.MethodGet
	}
	if hasGET {
		methods = append(append([]string{}, methods...), http.MethodHead)
	}
	return strings.Join(methods, ", ")
}

// boardStaticAssets 看板静态资产精确映射表（包级：staticHandler 分发与
// static_test 白名单闸共用同一份——新增 js 资产进本表即自动纳入
// TestAppJsApiWhitelist 扫描面，防拆 js 文件静默掏空 AC14.5 核验）。
var boardStaticAssets = map[string]struct {
	file  string // embed FS 内资产名（前缀已由 fs.Sub 剥去）
	ctype string // 精确 Content-Type（不依赖 mime 扩展表）
}{
	"/":          {file: "index.html", ctype: "text/html; charset=utf-8"},
	"/app.js":    {file: "app.js", ctype: "text/javascript; charset=utf-8"},
	"/dialog.js": {file: "dialog.js", ctype: "text/javascript; charset=utf-8"},
	"/style.css": {file: "style.css", ctype: "text/css; charset=utf-8"},
}

// staticHandler "/" subtree 单点：看板静态资产分发（§2.2 #23；B5-T1）+ B0 404 兜底
// 合并（合并依据见 NewServer 内注释——ServeMux 冲突规则禁止 "GET /" 与 method-agnostic
// pattern 共存）。按 URL.Path 精确映射资产（表见 boardStaticAssets），不用
// http.FileServerFS——它有目录索引/路径穿越面且 404 为纯文本，与本服务「一律
// JSON 错误形状」口径冲突；精确映射同时规避 mime 扩展表的平台差异（Windows 下
// Content-Type 可能读注册表，须确定）。仅 GET/HEAD 出资产（GET pattern 隐式含
// HEAD 的语义对齐），其余路径/方法回 404 JSON，与 B0 兜底行为一致。
func staticHandler(sub fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		e, ok := boardStaticAssets[r.URL.Path]
		if !ok || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			types.WriteError(w, http.StatusNotFound, types.CodeNotFound, "请求的路径不存在")
			return
		}
		b, err := fs.ReadFile(sub, e.file)
		if err != nil {
			// embed 资产在编译期由 go:embed 验证存在，运行期读取失败理论不可达；
			// 兜底 500 纯文本防静默空响应（对齐 writeJSON 的 Marshal 失败先例）
			http.Error(w, "看板静态资产读取失败", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", e.ctype)
		// embed 资产 URL 无内容指纹（路径固定 /app.js 等），no-cache 强制协商校验，
		// 防 B5-3 起 JS 迭代后浏览器缓存旧版「改了没生效」
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	}
}

// NewServer 组装完整 HTTP 骨架：mux 注册 + 中间件链 + 404/405 统一 JSON 兜底。
// version 进 ping/version 响应；production 由 cmd 层经 ldflags 注入（B7）。
func NewServer(st *store.Store, cfg *config.ServerConfig, version string) http.Handler {
	s := &Server{
		mux:           http.NewServeMux(),
		methodsByPath: make(map[string][]string),
		st:            st,
		cfg:           cfg,
		version:       version,
	}
	// 端点注册面：健康检查两端点（§2.2 #29/#30）+ 资源域三端点（B4-2，§2.2 #18/#19/#20）；
	// 后续业务端点按功能域在各自批次追加注册。
	s.handle(http.MethodGet, "/api/v1/ping", s.handlePing)
	s.handle(http.MethodGet, "/api/v1/version", s.handleVersion)
	// 登记域八端点（§2.2 #1~#8，B1-5）：pattern 照表逐字（{code}/{col} 为
	// Go 1.22 路径参数，handler 内 r.PathValue 取值）。
	s.handle(http.MethodPost, "/api/v1/projects", s.handleProjectCreate)                        // #1
	s.handle(http.MethodPatch, "/api/v1/projects/{code}", s.handleProjectUpdate)                // #2
	s.handle(http.MethodDelete, "/api/v1/projects/{code}", s.handleProjectArchive)              // #3
	s.handle(http.MethodGet, "/api/v1/projects", s.handleProjectList)                           // #4
	s.handle(http.MethodPost, "/api/v1/projects/{code}/columns", s.handleColumnCreate)          // #5
	s.handle(http.MethodPatch, "/api/v1/projects/{code}/columns/{col}", s.handleColumnUpdate)   // #6
	s.handle(http.MethodDelete, "/api/v1/projects/{code}/columns/{col}", s.handleColumnArchive) // #7
	s.handle(http.MethodGet, "/api/v1/projects/{code}/columns", s.handleColumnList)             // #8
	// 会话/审计辅助（§2.2 #27/#28，B1-6）：心跳面观测口+操作留痕核验口。
	s.handle(http.MethodGet, "/api/v1/sessions", s.handleSessionList) // #27
	s.handle(http.MethodGet, "/api/v1/audit", s.handleAuditList)      // #28
	// 进度上报域（§2.2 #31/#32，B8-2）：FR23 手动层+看板/总控巡检数据面；POST
	// 「上报即心跳」与 GET 的四头必填均由 sessionHeartbeat 中间件统一达成（§7.1
	// 全部 /api/v1 端点，handler 不重复做会话逻辑）。
	s.handle(http.MethodPost, "/api/v1/progress", s.handleProgressPost) // #31
	s.handle(http.MethodGet, "/api/v1/progress", s.handleProgressList)  // #32
	// 消息域（§2.2 #9/#10/#11/#12/#13，B2-3/4/5）：send 写路径+poll/ack 消费
	// 端点+block 回执+历史查询（路由照端点总表原文；#13 与 #9 同 pattern 异
	// 方法——GET 历史口）。
	s.handle(http.MethodPost, "/api/v1/messages", s.handleMessageSend)                  // #9
	s.handle(http.MethodGet, "/api/v1/mailbox", s.handleMessagePoll)                    // #10
	s.handle(http.MethodPost, "/api/v1/acks", s.handleMessageAck)                       // #11
	s.handle(http.MethodPost, "/api/v1/messages/{seq}/receipt", s.handleMessageReceipt) // #12
	s.handle(http.MethodGet, "/api/v1/messages", s.handleMessageHistory)                // #13
	// B4-2 资源域三端点（§2.2 #18/#19/#20）
	s.handle(http.MethodPost, "/api/v1/resources", s.handleRegisterResource)
	s.handle(http.MethodDelete, "/api/v1/resources/{id}", s.handleReleaseResource)
	s.handle(http.MethodGet, "/api/v1/resources", s.handleListResources)
	// B4-5 窗域两端点（§2.2 #21/#22）；#21 修订行（B4-6b）：同 path 加 GET
	// 读窗配置原始行（管理面全量），支撑 CLI window list 与 --clear 单窗删除。
	s.handle(http.MethodPut, "/api/v1/projects/{code}/columns/{col}/windows", s.handleSetWindows)
	s.handle(http.MethodGet, "/api/v1/projects/{code}/columns/{col}/windows", s.handleGetWindows)
	s.handle(http.MethodGet, "/api/v1/windows/now", s.handleWindowsNow)
	// 哨兵域（§2.2 #14/#15/#16，B3-4）：注册 / poll（T3 三合一）/ 注销幂等
	s.handle(http.MethodPost, "/api/v1/sentinels", s.handleSentinelRegister)
	s.handle(http.MethodPost, "/api/v1/sentinels/{id}/poll", s.handleSentinelPoll)
	s.handle(http.MethodDelete, "/api/v1/sentinels/{id}", s.handleSentinelDelete)
	// 状态聚合（§2.2 #17，B3-5）：CLI status 与看板首屏同源数据口（AC14.1），
	// mode=column 单栏目过滤同函数透传。
	s.handle(http.MethodGet, "/api/v1/status", s.handleStatus) // #17
	// 看板域三端点（§2.2 #24/#25/#26，B5-1）：#24 看板唯一写口（AC14.5——恒不带
	// 身份四头，中间件恒豁免面 boardAlwaysExemptPaths）；#25 对话流（AC15 数据面，
	// {id}=sessions 数字主键）；#26 总线流薄壳（AC14.3，store 复用 #13 历史查询）。
	// 三端点四头豁免口径见 B5-T1（boardAlwaysExemptPaths/boardOptionalHeaderPaths）。
	s.handle(http.MethodPost, "/api/v1/board/messages", s.handleBoardSend)              // #24
	s.handle(http.MethodGet, "/api/v1/board/sessions/{id}/dialog", s.handleBoardDialog) // #25
	s.handle(http.MethodGet, "/api/v1/board/bus-stream", s.handleBoardBusStream)        // #26

	// 看板静态页 + 404 兜底（§2.2 #23 / §5.2 五区布局；B5-T1）：二者合并为单一
	// method-agnostic subtree handler 注册于 "/"。不能按「"GET /" 静态 + "/" 404 兜底」
	// 分开注册——ServeMux 冲突规则下 "GET /"（方法收窄的 subtree）与 method-agnostic
	// API pattern（如 "/api/v1/ping"）请求集相交且互不包含，注册即 panic；
	// 合并后 API 路由仍由更具体的 pattern 优先命中（最长匹配语义，见 handle 文档），
	// 未命中三资产的路径与非 GET 方法一律 404 JSON，B0 兜底行为不回归。
	webSub, err := fs.Sub(assets.WebFS, "web")
	if err != nil {
		// "web" 为编译期 go:embed 已验证存在的字面量目录，err 理论不可达；
		// fail-fast 不降级（对齐 DbRouteCtx 缺失 panic 的项目哲学）
		panic("aiteam: 内嵌 web/ 目录缺失: " + err.Error())
	}
	s.mux.Handle("/", staticHandler(webSub))

	// 中间件链，外→内执行顺序（§10 server.go 组装固化）：bodyLimit → tokenAuth → 会话心跳 → mux。
	//   B6-1 token 鉴权（B0 预留转正，rebase B1 时按预定顺序落位）：cfg.Auth.TokenEnabled
	//            时校验 Bearer 头 / ?token=（AC14.6/AC17），豁免 ping/version/页面资产；
	//            须在心跳外层（未过鉴权不刷心跳）。
	//   B1-4 会话心跳（§7.1）：按身份四头校验存在性+upsert 会话+刷 last_seen_at+
	//            事件审计，中间件内按 path 精确排除 /api/v1/ping、/api/v1/version
	//            （健康检查无会话语义）。
	var h http.Handler = s.mux
	h = s.sessionHeartbeat(h)
	h = tokenAuth(&cfg.Auth)(h)
	h = bodyLimit(maxBodyBytes)(h)
	return h
}

// bodyLimit 请求体上限中间件（§2.1）：Content-Length 预检超限直接 413；
// chunked 等无长度场景由 MaxBytesReader 读时兜底（B1 起业务读 body 撞
// *http.MaxBytesError 时应回 413 body_too_large）。
func bodyLimit(max int64) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > max {
				types.WriteError(w, http.StatusRequestEntityTooLarge, types.CodeBodyTooLarge,
					fmt.Sprintf("请求体超过 %dMB 上限", max>>20))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, max)
			next.ServeHTTP(w, r)
		})
	}
}
