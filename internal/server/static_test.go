package server

import (
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"aiteam/internal/store"
)

// boardAnchors 技术设计 §5.2 五区布局锚点全量清单（B5-T1）：
// ① global-bar 全局状态条；② project-tree 项目-栏目-会话三层树（b3-W3）；
// ③ tab-bus/tab-resources/tab-windows/tab-column-detail 标签区四 tab；
// ④ dialog-panel/dialog-input/dialog-send 对话面板；⑤ footer-status 底栏。
// B5-6 增补（b5-spec §八「看板配置展示区」）：config-bar 只读配置区（①区下方
// 次行）——入清单防误删（机械核验）。
var boardAnchors = []string{
	"global-bar",
	"config-bar",
	"project-tree",
	"tab-bus",
	"tab-resources",
	"tab-windows",
	"tab-column-detail",
	"dialog-panel",
	"dialog-input",
	"dialog-send",
	"footer-status",
}

// getIndexBody 拉取看板页面原文供盘点类断言共用（非 200 直接 Fatal）。
func getIndexBody(t *testing.T, h http.Handler) string {
	t.Helper()
	rr := doReq(t, h, http.MethodGet, "/", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

// TestIndexServed §2.2 #23：GET / → 200、Content-Type=text/html; charset=utf-8、
// body 含 §5.2 五区布局锚点全量 10 个（逐个断言，页面骨架完整性机械核验）。
func TestIndexServed(t *testing.T) {
	h := newTestServer(t)

	rr := doReq(t, h, http.MethodGet, "/", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
	}
	const wantCT = "text/html; charset=utf-8"
	if got := rr.Header().Get("Content-Type"); got != wantCT {
		t.Errorf("Content-Type = %q, want %q", got, wantCT)
	}
	body := rr.Body.String()
	for _, id := range boardAnchors {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("页面缺锚点 id=%q（§5.2 五区布局锚点清单）", id)
		}
	}
}

// TestStaticAssets 静态资产可载：/app.js 与 /style.css 均 200 且 body 非空，
// Content-Type 精确匹配。服务端手动设头而非依赖 mime 扩展表——Windows 下 mime
// 可能读注册表映射不定，测试与响应均须确定。
func TestStaticAssets(t *testing.T) {
	h := newTestServer(t)

	tests := []struct {
		path  string
		ctype string
	}{
		{"/app.js", "text/javascript; charset=utf-8"},
		{"/dialog.js", "text/javascript; charset=utf-8"},
		{"/style.css", "text/css; charset=utf-8"},
	}
	for _, tt := range tests {
		rr := doReq(t, h, http.MethodGet, tt.path, nil)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200（body: %s）", tt.path, rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("Content-Type"); got != tt.ctype {
			t.Errorf("GET %s Content-Type = %q, want %q", tt.path, got, tt.ctype)
		}
		if rr.Body.Len() == 0 {
			t.Errorf("GET %s body 为空, want 非空", tt.path)
		}
		// embed 资产 URL 无指纹，no-cache 防 B5-3 起 JS 迭代后浏览器缓存旧版
		if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", tt.path, got)
		}
	}
}

// TestOnlyOneForm AC14.5「全页唯一表单」机械半：表单控件盘点——
//   - <input> 全页仅 dialog-input 一个；
//   - 提交类按钮（type="submit"）仅 dialog-send 一个（tab 切换按钮均为
//     type="button" 非提交类，不占唯一表单名额）；
//   - 无 textarea/select 等其他输入控件——管理写控件（发信号/ack/资源登记/
//     栏目登记）零存在。
func TestOnlyOneForm(t *testing.T) {
	h := newTestServer(t)
	body := getIndexBody(t, h)

	if n := strings.Count(body, "<input"); n != 1 {
		t.Errorf("<input 计数 = %d, want 1（AC14.5 全页唯一表单：仅对话输入框）", n)
	}
	if n := strings.Count(body, `type="submit"`); n != 1 {
		t.Errorf(`type="submit" 计数 = %d, want 1（提交类仅对话发送按钮）`, n)
	}
	for _, tag := range []string{"<textarea", "<select"} {
		if n := strings.Count(body, tag); n != 0 {
			t.Errorf("%s 计数 = %d, want 0（AC14.5：除对话输入框外无任何输入控件）", tag, n)
		}
	}
	if !strings.Contains(body, `id="dialog-input"`) || !strings.Contains(body, `id="dialog-send"`) {
		t.Error("页面缺对话控件锚点 id=\"dialog-input\" / id=\"dialog-send\"")
	}
}

// TestStaticFallback 非静态资产路径回 B0 的 404 JSON 兜底（不回归）：
// "/" subtree 接住一切未注册路径后，未命中三资产的路径仍须回统一 JSON 错误形状。
// 非 GET 方法打 "/" 及静态路径同为 404 而非 405——"/" 是兜底 subtree 而非 API 端点，
// 显式取舍（405 仅适用于 handle() 注册的业务端点，见 server.go TestMethodNotAllowed）。
func TestStaticFallback(t *testing.T) {
	h := newTestServer(t)

	wantErrBody(t, doReq(t, h, http.MethodGet, "/no-such-asset.js", nil), http.StatusNotFound, "not_found")
	wantErrBody(t, doReq(t, h, http.MethodGet, "/assets/logo.png", nil), http.StatusNotFound, "not_found")
	// 静态 handler 方法拒绝分支：命中资产路径但方法非 GET/HEAD → 404 JSON
	wantErrBody(t, doReq(t, h, http.MethodPost, "/", nil), http.StatusNotFound, "not_found")
	wantErrBody(t, doReq(t, h, http.MethodPost, "/app.js", nil), http.StatusNotFound, "not_found")
}

// TestStaticAssetsTokenExempt AC14.6/#23「页面本身无敏感数据，token 校验在数据口」：
// token 开态下页面三资产（/、/app.js、/style.css）无凭证可载（豁免面已扩展）。
// 对照面锚定豁免语义未被放大：健康检查两豁免口不受影响（200）、
// 数据口无凭证仍 401 先于 404（B6 行为不回归）。
func TestStaticAssetsTokenExempt(t *testing.T) {
	h, _ := newTokenOnServer(t)

	for _, path := range []string{"/", "/app.js", "/dialog.js", "/style.css"} {
		rr := doReq(t, h, http.MethodGet, path, nil)
		if rr.Code != http.StatusOK {
			t.Errorf("开态无凭证 GET %s status = %d, want 200（页面资产无鉴权可载，#23）", path, rr.Code)
		}
	}
	// 对照面：豁免口健康检查
	for _, path := range []string{"/api/v1/ping", "/api/v1/version"} {
		rr := doReq(t, h, http.MethodGet, path, nil)
		if rr.Code != http.StatusOK {
			t.Errorf("开态无凭证 GET %s status = %d, want 200（豁免失效）", path, rr.Code)
		}
	}
	// 对照面：数据口不豁免，401 优先于 404
	wantErrBody(t, doReq(t, h, http.MethodGet, dataPath, nil), http.StatusUnauthorized, "auth_required")
}

// TestTokenStatusPathQueryForm B5-5 token JS 流转的服务端前提固化（AC14.6/§5.3）：
// 看板 JS 以 URL ?token= 形态首入（app.js initTokenFromUrl 存 localStorage 后转头
// 走 Authorization 头），而页面主数据口实调形态是 /api/v1/status?mode=overview。
// B5-2 的 TestStaticAssetsTokenExempt 已锚定「页面+资产无凭证可载」，此处补齐
// status 路径上 ?token= 双形态与带查询串无凭证变体的组合面：
//   - 对 token 的 ?token= 形态：放行（非 401）。rebase 二次演进（B3 合并 #17 实存
//     后先锚 400 missing_header；B5-T1 看板豁免落地 #17 四头可选）：无四头请求过
//     tokenAuth 后经可选豁免直达 handler → 200——「token 形态放行先于心跳」锚定
//     以最强形态成立（401=被 tokenAuth 拦、200=过鉴权且豁免直达）；
//   - 对 token + 身份四头 + ?token= 组合：200 完整通路（token 与四头两道闸串联
//     全过——心跳链 upsert 照常+handler 正常响应）；
//   - 错 token 的 ?token= 形态：401 auth_required（错误 token 不放行）；
//   - 无 token 带查询串变体（?mode=overview，页面实调形态）：401 auth_required
//     ——B6 已测未注册裸路径（dataPath），此处锚定带查询串不改变鉴权判定。
func TestTokenStatusPathQueryForm(t *testing.T) {
	h, st := newTokenOnServer(t)
	// 带四头通路过心跳 ② 存在性校验：预登记项目/栏目夹具。
	seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	// 对 token ?token= 形态 → 放行（B5-T1 可选豁免直达 #17 handler 得 200）
	rr := doReq(t, h, http.MethodGet,
		"/api/v1/status?token="+url.QueryEscape(testToken), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("对 token 无四头 GET /api/v1/status?token= status = %d, want 200（B5-T1 豁免直达；body: %s）",
			rr.Code, rr.Body.String())
	}
	// 对 token + 身份四头 + ?token= 组合 → 200 完整通路（两道闸串联全过）
	rr = doHeartbeatReq(t, h, http.MethodGet,
		"/api/v1/status?token="+url.QueryEscape(testToken), "p-a", "05", "executor-A", "executor")
	if rr.Code != http.StatusOK {
		t.Fatalf("对 token+四头 GET /api/v1/status?token= status = %d, want 200（body: %s）",
			rr.Code, rr.Body.String())
	}
	if n := countSessions(t, st); n != 1 {
		t.Errorf("通路断言后 sessions 行数 = %d, want 1（心跳链 upsert 照常）", n)
	}
	// 错 token ?token= 形态 → 401
	wantErrBody(t, doReq(t, h, http.MethodGet, "/api/v1/status?token=wrong-token", nil),
		http.StatusUnauthorized, "auth_required")
	// 无 token 带查询串变体（页面实调形态）→ 401
	wantErrBody(t, doReq(t, h, http.MethodGet, "/api/v1/status?mode=overview", nil),
		http.StatusUnauthorized, "auth_required")
}

// appJsAPIWhitelist 看板页面 JS（app.js + dialog.js）允许调用的 API 路径白名单
// （b5-spec §四「白名单机械核验」，白名单注八端点减去前缀条目后的精确匹配面）：
// 页面仅消费读口+ping+对话三口（#24/#25/#27 在 dialog.js）。
// 新增页面调用（尤其写口）必须先改本表——「看板加写口冲动先红测试」的显式决策闸。
var appJsAPIWhitelist = map[string]bool{
	"/api/v1/status":           true, // #17 overview（①②③主数据，AC14.1）
	"/api/v1/resources":        true, // #20 资源分组表（AC14.4）
	"/api/v1/windows/now":      true, // #22 时间窗矩阵（FR3）
	"/api/v1/board/bus-stream": true, // #26 总线流薄壳（AC14.3）
	"/api/v1/sessions":         true, // #27 心跳面数据口（返回数字主键 id，B5-4 建 name→id 映射）
	"/api/v1/ping":             true, // #29 版本（底栏展示）
	"/api/v1/board/messages":   true, // #24 唯一写口（POST 看板对话，B5-4 消费）
}

// appJsAPIPrefixWhitelist 前缀型条目：#25 对话流读取
// GET /api/v1/board/sessions/{id}/dialog（{id}=sessions 数字主键，B5-4/B5-5
// 消费）——模板串止于 ${，抽取得前缀形态放行该子树。页面写口仅 #24 一个
// （POST /api/v1/board/messages），AC14.5 红线。
var appJsAPIPrefixWhitelist = []string{"/api/v1/board/sessions/"}

// appJsAPIRe 抽取 app.js 中全部 /api/v1/... 路径字面量。字符类刻意排除
// `$`/`{`（模板串止于 ${，天然得前缀，如 /api/v1/board/sessions/${id}/dialog
// → /api/v1/board/sessions/）、`?`/引号/空白（查询串与字符串边界截断，得路径
// 本体）。注释内出现的路径同样命中——注释承诺的调用面也在闸内，保守核验。
var appJsAPIRe = regexp.MustCompile(`/api/v1/[A-Za-z0-9_\-/]*`)

// TestAppJsApiWhitelist AC14.5 接口清单半的常驻机械闸：遍历全部 js 资产
// （boardStaticAssets 内 .js 项——app.js 与 dialog.js，B5-4 拆分后对话三口
// #24/#25/#27 字面量在 dialog.js，只扫 app.js 会静默掏空核验面），逐文件经
// httptest 全链路读资产（同生产服务路径），正则抽取全部 API 路径字面量（含
// 模板串前缀形态），断言抽取结果 ⊆ 白名单集合。后续任何「看板加写口」冲动
// （发信号/ack/资源登记/栏目登记等管理写口）都会先红本测试，强制显式决策留痕。
//
// 约束面声明（审查盲区封堵）：API 路径必须为 /api/v1 开头的源码字面量，
// 禁止运行时变量拼路径——fetch(变量) 形态逃出静态抽取闸；动态拼路径需求
// 一律先落字面量+白名单表再重构。
func TestAppJsApiWhitelist(t *testing.T) {
	h := newTestServer(t)

	var jsAssets []string
	for path := range boardStaticAssets {
		if strings.HasSuffix(path, ".js") {
			jsAssets = append(jsAssets, path)
		}
	}
	slices.Sort(jsAssets) // 稳定扫描序（map 遍历随机，失败输出可复现）

	for _, asset := range jsAssets {
		rr := doReq(t, h, http.MethodGet, asset, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200（body: %s）", asset, rr.Code, rr.Body.String())
		}
		js := rr.Body.String()

		found := map[string]bool{}
		for _, p := range appJsAPIRe.FindAllString(js, -1) {
			found[p] = true
		}
		// 完备性防呆：单资产一个都抽不到说明正则或资产加载失效，核验空转比红更危险
		if len(found) == 0 {
			t.Fatalf("%s 未抽取到任何 /api/v1/ 路径——抽取正则或资产加载失效，白名单核验空转", asset)
		}
		for p := range found {
			if appJsAPIWhitelist[p] {
				continue
			}
			prefixed := false
			for _, pre := range appJsAPIPrefixWhitelist {
				if strings.HasPrefix(p, pre) {
					prefixed = true
					break
				}
			}
			if !prefixed {
				t.Errorf("%s 出现白名单外 API 路径 %q（AC14.5：页面仅可调用白名单八端点；新增调用先改白名单表留决策痕）", asset, p)
			}
		}
	}
}

// TestAppJsSentinelHitBadge b8-W3/AC8 看板面源码锚（本仓看板 JS 无 DOM 测试基建，
// 白名单测试同款源码扫描裁量——真实渲染行为走查兜底）：app.js 会话行渲染须含
// sentinel_last_hit_at 非空守卫下的命中徽标构造——文案「哨兵·N 分钟前命中」、
// title 属性、年龄取响应 generated_at 同源（禁本地时钟取值）。
func TestAppJsSentinelHitBadge(t *testing.T) {
	h := newTestServer(t)
	rr := doReq(t, h, http.MethodGet, "/app.js", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /app.js status = %d，期望 200", rr.Code)
	}
	js := rr.Body.String()
	for _, anchor := range []string{
		"sentinel_last_hit_at", // 守卫与取值键（SessionEntry 新键消费面）
		"哨兵·",                  // 徽标文案前缀
		"分钟前命中",                // 徽标文案后缀（spec 冻结形态「哨兵·N 分钟前命中」）
		".title =",             // title 属性（悬停看原始时刻）
		"generated_at",         // 年龄同源响应钟（禁本地时钟）
	} {
		if !strings.Contains(js, anchor) {
			t.Errorf("app.js 缺命中徽标锚 %q（b8-W3 会话行哨兵徽标）", anchor)
		}
	}
}

// bareColorRe 闸④机械判据：十六进制色值（3~8 位）或 rgb/rgba( 函数形态。
// 与 bash 闸④ grep -nE "#[0-9a-fA-F]{3,8}|rgba?\(" style.css | grep -v -- "--"
// 同一定义面（色值只准出现在含 -- 的变量定义行）。
var bareColorRe = regexp.MustCompile(`#[0-9a-fA-F]{3,8}|rgba?\(`)

// TestWebResponsiveDarkGates b4/AC7.1/AC8.1/8.2 看板 CSS/JS 源码锚（白名单测试同款
// 源码扫描裁量——无 DOM 测试基建，真实渲染走查兜底）：响应式断点与暗色三态的机械
// 锚，防后续样式重构静默掏空 PRD AC7.1/AC8.x 机械判据（§四 grep 闸族的 go 侧固化）；
// 尾段附闸④加固（T3 可选授权）：逐行扫 style.css，裸色值越界行（含色值却无 --）
// 直接红——发布闸从脚本前置到 go test 常驻。
func TestWebResponsiveDarkGates(t *testing.T) {
	h := newTestServer(t)
	get := func(path string) string {
		rr := doReq(t, h, http.MethodGet, path, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d，期望 200", path, rr.Code)
		}
		return rr.Body.String()
	}
	css := get("/style.css")
	for _, anchor := range []string{
		"@media (max-width: 1099.98px)",       // PRD AC7.1 断点 1100（b4-T1 勘误口径）
		"@media (prefers-color-scheme: dark)", // PRD AC8.1 暗色变量组
		`data-theme="dark"`,                   // 手动暗色覆写块
		`data-theme="light"`,                  // 手动亮色覆写块（系统暗+手动亮回亮）
	} {
		if !strings.Contains(css, anchor) {
			t.Errorf("style.css 缺响应式/主题锚 %q（b4-W1/W5）", anchor)
		}
	}
	js := get("/app.js")
	for _, anchor := range []string{
		"aiteam.theme", // PRD AC8.2 localStorage 键（tech-design §5.3 冻结）
		"#/session/",   // 会话页 hash 前缀（b4-T5 三元组形态）
		"#/board",      // 主页 hash（§三.6 冻结面锚——代码缺省路由不落字面量，命中 app.js 注释即足）
		"matchMedia",   // 断点监听（路由类随断点+hash 双条件）
	} {
		if !strings.Contains(js, anchor) {
			t.Errorf("app.js 缺路由/主题锚 %q（b4-W1/W3）", anchor)
		}
	}

	// 闸④加固（T3 可选授权，任务 2 质量审查建议收口）：逐行等价复刻
	// grep -nE "#[0-9a-fA-F]{3,8}|rgba?\(" | grep -v -- "--" 零输出判据，
	// 行号对齐 grep -n 便于红时直接定位越界行。
	for i, line := range strings.Split(css, "\n") {
		if bareColorRe.MatchString(line) && !strings.Contains(line, "--") {
			t.Errorf("style.css:%d 裸色值越界行（闸④：色值只准出现在变量定义行）：%s", i+1, line)
		}
	}
}
