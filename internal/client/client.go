package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"aiteam/internal/types"
)

// ─────────────────────────────────────────────────────────────────────────────
// 契约常量与查找链环境变量（技术设计 §4.1）
// ─────────────────────────────────────────────────────────────────────────────

const (
	// EnvServer / EnvToken 服务地址与 token 的环境变量名（§4.1 查找链第 2 级）。
	EnvServer = "AITEAM_SERVER"
	EnvToken  = "AITEAM_TOKEN"

	// DefaultServerURL 服务地址查找链兜底默认值（§4.1 第 5 级）。
	DefaultServerURL = "http://127.0.0.1:8310"
)

// BearerPrefix token 注入 Authorization 头的前缀（§2.1 鉴权行）。
const BearerPrefix = "Bearer "

// Identity 会话身份四元组（CLI 命令行身份参数，§4.1「身份参数」）：一条命令一个
// 会话身份，调用方构造 Client 时持有，Do 每请求自动转成身份四头（§2.1——服务端
// 心跳中间件据此隐式注册+刷心跳，AC12.1）。看板等无会话语义的调用传零值。
type Identity struct {
	Project string // 项目 code（X-Aiteam-Project）
	Column  string // 栏目 code（X-Aiteam-Column）
	Session string // 会话名（X-Aiteam-Session）
	Role    string // 角色（X-Aiteam-Role）
}

// Client CLI 侧 HTTP 客户端基座（B1-8 全部命令经它发请求）：
// 统一注入身份四头 + Bearer token，统一解析 {data}/{error} 包裹（§2.1），
// 统一不可达语义（AC17.4）。client 层只返回错误类型，退出码映射归 CLI 层（§4.7，
// 本包不做 os.Exit）。
type Client struct {
	// HTTP 底层客户端，nil 时用 http.DefaultClient（NewClient 已注入带超时默认），
	// 测试可替换。
	HTTP     *http.Client
	BaseURL  string   // 服务地址（查找链产物，不带尾斜杠）
	Token    string   // 空=不注入 Authorization（§2.1 鉴权默认关）
	Identity Identity // 身份四元组，每请求转四头
}

// NewClient 构造 CLI 客户端：默认 30s 整请求超时——连接拒绝即刻失败，网络黑洞
// 场景（防火墙丢包）靠超时收敛进 AC17.4 不可达语义，不让命令无限挂死；重定向
// 一律不跟随（ErrUseLastResponse）——302/307 落非 2xx 分支转 APIError 透传，
// 配置错误（误指向反代等）立刻可见，并杜绝 307 保留 method+body+身份四头跨域
// 重发的安全面。深定制（如 watch 长循环）直接改返回值的 HTTP 字段。
func NewClient(baseURL, token string, id Identity) *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		BaseURL:  strings.TrimSuffix(baseURL, "/"),
		Token:    token,
		Identity: id,
	}
}

// maxRespBodyBytes 响应体读取上限：服务端请求侧限 1MB（§2.1），响应侧放宽到
// 8MB 纯防御——误指向大文件服务（反代/静态站）时不至于整包读进内存。
const maxRespBodyBytes = 8 << 20

// Do 执行一次 API 调用（§2.2 端点总表的 method/path 照表传入）：
//
//   - body 非 nil 时序列化为 JSON 请求体并设 Content-Type（nil=无体，不设头）；
//   - out 非 nil 时把 2xx 响应的 {"data":{...}} 反序列化到 out——out 必须传
//     指针（如 &struct{...}），传值类型时 encoding/json 无法写入、数据静默丢弃；
//   - 非 2xx 统一解析为 *APIError（服务端拒绝 → CLI 层映射退出 4；重定向不
//     跟随，同样落此分支）；
//   - 网络层失败/响应中途断链统一包装为 *UnreachableError（AC17.4 → 退出 3）；
//   - 响应体超限/2xx 但 body 非 JSON 属协议异常，返回普通错误（CLI 层归退出 1）。
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	// 缺头斜杠补齐：调用方手滑传 "api/v1/ping" 不至于拼出 ...8310api/v1/ping。
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("请求体序列化失败: %w", err)
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method,
		strings.TrimSuffix(c.BaseURL, "/")+path, payload) // TrimSuffix：未走 NewClient 直构也有尾斜杠防御
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Accept", types.ContentTypeJSON)
	if body != nil {
		req.Header.Set("Content-Type", types.ContentTypeJSON)
	}
	c.applyHeaders(req)

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		// 连接拒绝/超时/DNS 失败等网络层错误统一不可达语义（AC17.4 全命令
		// 适用）：保留底层原因供 stderr 展示，CLI 层映射退出 3。
		return &UnreachableError{Err: err, ServerAddr: c.BaseURL}
	}
	defer resp.Body.Close()

	// 多读 1 字节判超限：LimitReader 截断与恰满上限在截断视角下不可区分，
	// 超限时读完 max+1 即可判定，无需拉全量。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBodyBytes+1))
	if err != nil {
		// 响应中途断链对用户表现同样是「服务不可达」，归 AC17.4 语义。
		return &UnreachableError{Err: err, ServerAddr: c.BaseURL}
	}
	if len(raw) > maxRespBodyBytes {
		// 协议异常非网络不可达（连接是好的）、也非 {error} 业务错（无 code
		// 可透传）——普通错误由 CLI 层归退出 1（未知内部错误兜底）。
		return fmt.Errorf("响应体超过上限 %d 字节（server=%s）", maxRespBodyBytes, c.BaseURL)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeAPIError(resp.StatusCode, raw)
	}
	if out != nil {
		// {data} 包裹解析：Data 持有的非 nil 指针会被 encoding/json 直接填充。
		if err := json.Unmarshal(raw, &types.DataResponse{Data: out}); err != nil {
			return fmt.Errorf("响应解析失败（HTTP %d）: %w", resp.StatusCode, err)
		}
	}
	return nil
}

// applyHeaders 注入身份四头（types 共享契约常量）与 Bearer token：空值字段跳过，
// 由服务端心跳中间件对缺失头统一 400（§7.1 ①），client 不重复校验。
func (c *Client) applyHeaders(req *http.Request) {
	if c.Identity.Project != "" {
		req.Header.Set(types.HeaderAiteamProject, c.Identity.Project)
	}
	if c.Identity.Column != "" {
		req.Header.Set(types.HeaderAiteamColumn, c.Identity.Column)
	}
	if c.Identity.Session != "" {
		req.Header.Set(types.HeaderAiteamSession, c.Identity.Session)
	}
	if c.Identity.Role != "" {
		req.Header.Set(types.HeaderAiteamRole, c.Identity.Role)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", BearerPrefix+c.Token)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 错误类型（§4.7：client 层返回类型，CLI 层映射退出码——3/4 经 errors.As 判定）
// ─────────────────────────────────────────────────────────────────────────────

// UnreachableError 服务不可达（CLI 层映射退出码 3，AC17.4）：连接拒绝/超时/
// 响应中断等网络层失败。Error() 文本对齐 §4.1「服务不可达 <原因>（server=<addr>）」
// 形状（stderr 的 "ERROR: " 前缀由 CLI 层补）。
type UnreachableError struct {
	Err        error  // 底层网络错误（*url.Error 等），Unwrap 透传
	ServerAddr string // 目标服务地址，stderr 回显 server=<addr>
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("服务不可达 %v（server=%s）", e.Err, e.ServerAddr)
}

// Unwrap 透传底层错误：调用方可 errors.Is(err, context.DeadlineExceeded) 等细分。
func (e *UnreachableError) Unwrap() error { return e.Err }

// APIError 服务端拒绝（CLI 层映射退出码 4，§4.7）：4xx/5xx 全部业务错，
// 错误码+信息原样透传（错误码枚举 §2.4）。
type APIError struct {
	StatusCode int    // HTTP 状态码
	Code       string // 机器码（§2.4）；非标准错误体兜底时为空串
	Message    string // 人类可读信息；非标准错误体时为原始响应片段（截断）
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("服务端错误（HTTP %d）: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("服务端错误（HTTP %d）%s: %s", e.StatusCode, e.Code, e.Message)
}

// decodeAPIError 把非 2xx 响应体解析为 *APIError：标准 {"error":{code,message}}
// 形状逐字段透传；非标准形状（反代 HTML 等）兜底保留状态码+原始片段，不静默丢弃。
func decodeAPIError(status int, body []byte) *APIError {
	var er types.ErrorResponse
	if err := json.Unmarshal(body, &er); err == nil && er.Error.Code != "" {
		return &APIError{StatusCode: status, Code: er.Error.Code, Message: er.Error.Message}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		// 截断点回退到 rune 起始字节，防把多字节中文切碎成非法 UTF-8。
		n := 200
		for n > 0 && !utf8.RuneStart(msg[n]) {
			n--
		}
		msg = msg[:n] + "…"
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &APIError{StatusCode: status, Message: msg}
}

// ─────────────────────────────────────────────────────────────────────────────
// 服务地址/token 查找链（§4.1 五级）：flag > env > 仓库级 cli.json > 用户级
// cli.json > 默认。cli.json 只读不写——「零本地状态文件」红线（T1/AC12.3）指
// 会话身份不落盘，连接配置属只读输入不违反，本包不提供任何写配置函数。
// ─────────────────────────────────────────────────────────────────────────────

// cliConfig cli.json 配置形状（§4.1 CLI 客户端配置样例）：{"server":...,"token":...}。
// _doc 为人类说明字段，encoding/json 默认忽略未知键，无需显式声明。
type cliConfig struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

// 两级配置文件路径（包级函数变量：测试注入伪造 cli.json 用，生产代码勿改）。
// 仓库级=进程 CWD 下 ./.aiteam/cli.json（可提交共享）；用户级=~/.aiteam/cli.json。
// userCliJSONPath 取不到 home 目录时返回空串，查找链静默跳过该级。
var (
	repoCliJSONPath = func() string {
		return filepath.Join(".aiteam", "cli.json")
	}
	userCliJSONPath = func() string {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, ".aiteam", "cli.json")
	}
)

// ResolveServer 解析服务地址（§4.1 五级查找链）：flagVal 非空直接胜出，逐级
// 降落到默认值。配置文件存在但损坏/地址非法时返回错误（fail-fast，不静默跳过——
// 静默降级会让用户以为配置已生效）。
func ResolveServer(flagVal string) (string, error) {
	if v, ok, err := firstNonEmpty(flagVal, EnvServer, func(c cliConfig) string { return c.Server }); err != nil {
		return "", err
	} else if ok {
		return normalizeServerURL(v)
	}
	return DefaultServerURL, nil
}

// ResolveToken 解析 token（§4.1 查找链同型）：flagVal > env > 仓库级 > 用户级；
// 全部缺省返回空串（§2.1 鉴权默认关，Client 空 token 不注入 Authorization）。
func ResolveToken(flagVal string) (string, error) {
	if v, ok, err := firstNonEmpty(flagVal, EnvToken, func(c cliConfig) string { return c.Token }); err != nil {
		return "", err
	} else if ok {
		return v, nil
	}
	return "", nil
}

// firstNonEmpty 查找链公共骨架：flag → env → 仓库级 cli.json → 用户级 cli.json，
// 返回首个非空值。ok=false 表示四级全空（调用方落默认值）。
func firstNonEmpty(flagVal, envKey string, pick func(cliConfig) string) (string, bool, error) {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v, true, nil
	}
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v, true, nil
	}
	for _, pathFn := range []func() string{repoCliJSONPath, userCliJSONPath} {
		cfg, err := loadCliConfig(pathFn())
		if err != nil {
			return "", false, err
		}
		if v := strings.TrimSpace(pick(cfg)); v != "" {
			return v, true, nil
		}
	}
	return "", false, nil
}

// loadCliConfig 读取单份 cli.json：不存在=查找链正常一环（返回零值不报错）；
// 存在但损坏=配置错误，报错并指明路径（fail-fast）。
func loadCliConfig(path string) (cliConfig, error) {
	if path == "" {
		return cliConfig{}, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cliConfig{}, nil
	}
	if err != nil {
		return cliConfig{}, fmt.Errorf("读取配置 %s 失败: %w", path, err)
	}
	var cfg cliConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cliConfig{}, fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	return cfg, nil
}

// normalizeServerURL 规范化并校验服务地址：剔除尾斜杠（Do 拼 path 防双斜杠），
// 要求 http/https 绝对地址——`127.0.0.1:8310` 这类缺协议写法尽早报错，不留到
// 请求期以不可达假象掩盖配置笔误。
func normalizeServerURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("服务地址无效: %q（需形如 http://127.0.0.1:8310）", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}
