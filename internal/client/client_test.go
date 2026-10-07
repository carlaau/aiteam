package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aiteam/internal/types"
)

// fakeConfigPaths 伪造两级 cli.json 查找路径（传空串=该级指向不存在文件），
// 测试结束恢复原实现——ResolveServer/ResolveToken 据此可测五级链全分支。
func fakeConfigPaths(t *testing.T, repoPath, userPath string) {
	t.Helper()
	origRepo, origUser := repoCliJSONPath, userCliJSONPath
	t.Cleanup(func() { repoCliJSONPath, userCliJSONPath = origRepo, origUser })
	repoCliJSONPath = func() string { return repoPath }
	userCliJSONPath = func() string { return userPath }
}

// writeCliJSON 向 dir 写一份 cli.json（content 原样写入），返回文件完整路径。
func writeCliJSON(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "cli.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写测试配置 %s 失败: %v", path, err)
	}
	return path
}

// clearEnv 清空两个环境变量，防测试机外部环境污染查找链（t.Setenv 结束自动恢复）。
func clearEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvServer, "")
	t.Setenv(EnvToken, "")
}

func TestHeadersInjected(t *testing.T) {
	var gotPath string
	var gotHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeader = r.Header.Clone()
		types.WriteData(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer srv.Close()

	// BaseURL 故意带尾斜杠：验证 Do 拼接 path 不产生双斜杠。
	c := NewClient(srv.URL+"/", "", Identity{
		Project: "proj-a",
		Column:  "05",
		Session: "controller-A",
		Role:    "controller",
	})
	err := c.Do(context.Background(), http.MethodPost, "/api/v1/messages",
		map[string]any{"body": "你好"}, nil)
	if err != nil {
		t.Fatalf("Do 失败: %v", err)
	}

	if gotPath != "/api/v1/messages" {
		t.Errorf("请求路径 = %q, 期望 %q（BaseURL 尾斜杠应被剔除）", gotPath, "/api/v1/messages")
	}
	want := map[string]string{
		types.HeaderAiteamProject: "proj-a",
		types.HeaderAiteamColumn:  "05",
		types.HeaderAiteamSession: "controller-A",
		types.HeaderAiteamRole:    "controller",
	}
	for k, v := range want {
		if got := gotHeader.Get(k); got != v {
			t.Errorf("请求头 %s = %q, 期望 %q", k, got, v)
		}
	}
	if got := gotHeader.Get("Authorization"); got != "" {
		t.Errorf("无 token 时不应携带 Authorization 头, 实际 = %q", got)
	}
	if got := gotHeader.Get("Content-Type"); got != types.ContentTypeJSON {
		t.Errorf("有 body 时 Content-Type = %q, 期望 %q", got, types.ContentTypeJSON)
	}
}

func TestServerResolution(t *testing.T) {
	clearEnv(t)

	t.Run("flag优先级最高", func(t *testing.T) {
		t.Setenv(EnvServer, "http://env:9003")
		repoJSON := writeCliJSON(t, t.TempDir(), `{"server":"http://repo:9001"}`)
		userJSON := writeCliJSON(t, t.TempDir(), `{"server":"http://user:9002"}`)
		fakeConfigPaths(t, repoJSON, userJSON)

		got, err := ResolveServer("http://flag:9000/")
		if err != nil {
			t.Fatalf("ResolveServer 失败: %v", err)
		}
		if got != "http://flag:9000" {
			t.Errorf("flag 存在时服务地址 = %q, 期望 %q（并应剔除尾斜杠）", got, "http://flag:9000")
		}
	})

	t.Run("env次之", func(t *testing.T) {
		t.Setenv(EnvServer, "http://env:9003")
		repoJSON := writeCliJSON(t, t.TempDir(), `{"server":"http://repo:9001"}`)
		userJSON := writeCliJSON(t, t.TempDir(), `{"server":"http://user:9002"}`)
		fakeConfigPaths(t, repoJSON, userJSON)

		got, err := ResolveServer("")
		if err != nil {
			t.Fatalf("ResolveServer 失败: %v", err)
		}
		if got != "http://env:9003" {
			t.Errorf("env 存在时服务地址 = %q, 期望 %q", got, "http://env:9003")
		}
	})

	t.Run("仓库级cli.json第三", func(t *testing.T) {
		repoJSON := writeCliJSON(t, t.TempDir(),
			`{"server":"http://repo:9001","_doc":"团队共享连接配置（cli.json 只读不写，AC12.3）"}`)
		userJSON := writeCliJSON(t, t.TempDir(), `{"server":"http://user:9002"}`)
		fakeConfigPaths(t, repoJSON, userJSON)

		got, err := ResolveServer("")
		if err != nil {
			t.Fatalf("ResolveServer 失败: %v", err)
		}
		if got != "http://repo:9001" {
			t.Errorf("仓库级存在时服务地址 = %q, 期望 %q", got, "http://repo:9001")
		}
	})

	t.Run("用户级cli.json第四", func(t *testing.T) {
		userJSON := writeCliJSON(t, t.TempDir(), `{"server":"http://user:9002"}`)
		fakeConfigPaths(t, filepath.Join(t.TempDir(), "absent.json"), userJSON)

		got, err := ResolveServer("")
		if err != nil {
			t.Fatalf("ResolveServer 失败: %v", err)
		}
		if got != "http://user:9002" {
			t.Errorf("仅用户级存在时服务地址 = %q, 期望 %q", got, "http://user:9002")
		}
	})

	t.Run("全部缺省走默认值", func(t *testing.T) {
		fakeConfigPaths(t, filepath.Join(t.TempDir(), "absent.json"),
			filepath.Join(t.TempDir(), "absent.json"))

		got, err := ResolveServer("")
		if err != nil {
			t.Fatalf("ResolveServer 失败: %v", err)
		}
		if got != DefaultServerURL {
			t.Errorf("全缺省服务地址 = %q, 期望 %q", got, DefaultServerURL)
		}
	})

	t.Run("配置文件损坏返回错误", func(t *testing.T) {
		broken := writeCliJSON(t, t.TempDir(), `{"server": 不是JSON}`)
		fakeConfigPaths(t, broken, filepath.Join(t.TempDir(), "absent.json"))

		_, err := ResolveServer("")
		if err == nil {
			t.Fatal("cli.json 损坏应报错（fail-fast，不静默跳过）")
		}
		if !strings.Contains(err.Error(), "cli.json") {
			t.Errorf("错误信息应指明配置文件路径, 实际 = %q", err.Error())
		}
	})

	t.Run("非法地址返回错误", func(t *testing.T) {
		got, err := ResolveServer("127.0.0.1:8310")
		if err == nil {
			t.Fatalf("无协议地址 %q 应报错, 实际返回 %q", "127.0.0.1:8310", got)
		}
	})
}

func TestTokenResolution(t *testing.T) {
	clearEnv(t)

	t.Run("flag优先于env与配置", func(t *testing.T) {
		t.Setenv(EnvToken, "tok-env")
		repoJSON := writeCliJSON(t, t.TempDir(), `{"token":"tok-repo"}`)
		fakeConfigPaths(t, repoJSON, filepath.Join(t.TempDir(), "absent.json"))

		got, err := ResolveToken("tok-flag")
		if err != nil {
			t.Fatalf("ResolveToken 失败: %v", err)
		}
		if got != "tok-flag" {
			t.Errorf("token = %q, 期望 %q", got, "tok-flag")
		}
	})

	t.Run("env优先于配置", func(t *testing.T) {
		t.Setenv(EnvToken, "tok-env")
		repoJSON := writeCliJSON(t, t.TempDir(), `{"token":"tok-repo"}`)
		userJSON := writeCliJSON(t, t.TempDir(), `{"token":"tok-user"}`)
		fakeConfigPaths(t, repoJSON, userJSON)

		got, err := ResolveToken("")
		if err != nil {
			t.Fatalf("ResolveToken 失败: %v", err)
		}
		if got != "tok-env" {
			t.Errorf("token = %q, 期望 %q", got, "tok-env")
		}
	})

	t.Run("仓库级cli.json优先于用户级", func(t *testing.T) {
		repoJSON := writeCliJSON(t, t.TempDir(), `{"token":"tok-repo"}`)
		userJSON := writeCliJSON(t, t.TempDir(), `{"token":"tok-user"}`)
		fakeConfigPaths(t, repoJSON, userJSON)

		got, err := ResolveToken("")
		if err != nil {
			t.Fatalf("ResolveToken 失败: %v", err)
		}
		if got != "tok-repo" {
			t.Errorf("token = %q, 期望 %q", got, "tok-repo")
		}
	})

	t.Run("全部缺省返回空串", func(t *testing.T) {
		fakeConfigPaths(t, filepath.Join(t.TempDir(), "absent.json"),
			filepath.Join(t.TempDir(), "absent.json"))

		got, err := ResolveToken("")
		if err != nil {
			t.Fatalf("ResolveToken 失败: %v", err)
		}
		if got != "" {
			t.Errorf("全缺省 token = %q, 期望空串（鉴权默认关, §2.1）", got)
		}
	})

	t.Run("token注入Authorization头", func(t *testing.T) {
		clearEnv(t) // 子测试独立再清一次：防上层用例 Setenv 泄漏语义混淆
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			types.WriteData(w, http.StatusOK, map[string]any{})
		}))
		defer srv.Close()

		// 模拟 B1-8 消费链：ResolveToken 从配置链解析 → NewClient → Do 自动附头。
		userJSON := writeCliJSON(t, t.TempDir(), `{"token":"tok-json"}`)
		fakeConfigPaths(t, filepath.Join(t.TempDir(), "absent.json"), userJSON)
		token, err := ResolveToken("")
		if err != nil {
			t.Fatalf("ResolveToken 失败: %v", err)
		}

		c := NewClient(srv.URL, token, Identity{})
		if err := c.Do(context.Background(), http.MethodGet, "/api/v1/ping", nil, nil); err != nil {
			t.Fatalf("Do 失败: %v", err)
		}
		if gotAuth != "Bearer tok-json" {
			t.Errorf("Authorization = %q, 期望 %q", gotAuth, "Bearer tok-json")
		}
	})
}

func TestParseData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		types.WriteData(w, http.StatusCreated, map[string]any{
			"seq":        881,
			"created_at": "2026-10-02T13:04:05Z",
			"level":      "block",
		})
	}))
	defer srv.Close()

	// out 目标结构与 §2.2 #9 响应字段同形（201 也属 2xx 成功）。
	var out struct {
		Seq       int64  `json:"seq"`
		CreatedAt string `json:"created_at"`
		Level     string `json:"level"`
	}
	c := NewClient(srv.URL, "", Identity{})
	err := c.Do(context.Background(), http.MethodPost, "/api/v1/messages",
		map[string]any{"body": "开工"}, &out)
	if err != nil {
		t.Fatalf("Do 失败: %v", err)
	}
	if out.Seq != 881 {
		t.Errorf("out.Seq = %d, 期望 881", out.Seq)
	}
	if out.CreatedAt != "2026-10-02T13:04:05Z" {
		t.Errorf("out.CreatedAt = %q, 期望 %q", out.CreatedAt, "2026-10-02T13:04:05Z")
	}
	if out.Level != "block" {
		t.Errorf("out.Level = %q, 期望 %q", out.Level, "block")
	}
}

func TestParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		types.WriteError(w, http.StatusConflict, "project_exists", "项目 code 已存在: proj-a")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", Identity{Project: "proj-a"})
	err := c.Do(context.Background(), http.MethodPost, "/api/v1/projects",
		map[string]any{"code": "proj-a"}, nil)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误应可断言为 *APIError, 实际 = %v（%T）", err, err)
	}
	if apiErr.StatusCode != http.StatusConflict {
		t.Errorf("StatusCode = %d, 期望 %d", apiErr.StatusCode, http.StatusConflict)
	}
	if apiErr.Code != "project_exists" {
		t.Errorf("Code = %q, 期望 %q", apiErr.Code, "project_exists")
	}
	if apiErr.Message != "项目 code 已存在: proj-a" {
		t.Errorf("Message = %q, 期望 %q", apiErr.Message, "项目 code 已存在: proj-a")
	}
}

func TestUnreachable(t *testing.T) {
	// 起一个真服务再关掉：拿到确定已关闭的端口（AC17.4 不可达判据）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()

	c := NewClient(addr, "", Identity{})
	err := c.Do(context.Background(), http.MethodGet, "/api/v1/ping", nil, nil)

	var ue *UnreachableError
	if !errors.As(err, &ue) {
		t.Fatalf("错误应可断言为 *UnreachableError, 实际 = %v（%T）", err, err)
	}
	if ue.ServerAddr != addr {
		t.Errorf("ServerAddr = %q, 期望 %q（CLI 层 stderr 需回显 server=<addr>）", ue.ServerAddr, addr)
	}
	if ue.Err == nil {
		t.Error("Err 应携带底层原因（连接拒绝等），供 stderr 展示 <原因>")
	}
	msg := ue.Error()
	if !strings.Contains(msg, "服务不可达") || !strings.Contains(msg, addr) {
		t.Errorf("错误文本应含「服务不可达」与地址 %q, 实际 = %q", addr, msg)
	}
}

func TestHTTPStatusMap(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		code       string
		message    string
		wantStatus int
	}{
		{name: "400参数错误透传", status: http.StatusBadRequest,
			code: "param_invalid", message: "缺少必填字段: code", wantStatus: 400},
		{name: "401鉴权失败透传", status: http.StatusUnauthorized,
			code: "auth_required", message: "缺少或非法 token", wantStatus: 401},
		{name: "404不存在透传", status: http.StatusNotFound,
			code: "project_not_found", message: "项目未登记: proj-a", wantStatus: 404},
		{name: "409冲突透传", status: http.StatusConflict,
			code: "column_exists", message: "栏目 code 冲突: 05", wantStatus: 409},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				types.WriteError(w, tc.status, tc.code, tc.message)
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "", Identity{})
			err := c.Do(context.Background(), http.MethodPost, "/api/v1/whatever",
				map[string]any{}, nil)

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("HTTP %d 应断言为 *APIError, 实际 = %v（%T）", tc.status, err, err)
			}
			if apiErr.StatusCode != tc.wantStatus {
				t.Errorf("StatusCode = %d, 期望 %d", apiErr.StatusCode, tc.wantStatus)
			}
			if apiErr.Code != tc.code {
				t.Errorf("Code = %q, 期望 %q", apiErr.Code, tc.code)
			}
			if apiErr.Message != tc.message {
				t.Errorf("Message = %q, 期望 %q", apiErr.Message, tc.message)
			}
		})
	}
}

func TestNonJSONErrorBody(t *testing.T) {
	// 误指向反代/别的服务时错误体非 {error:{...}} 形状：兜底为无码 APIError，
	// 状态码仍透传（CLI 退出 4），原始片段进 Message 不静默丢弃。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", Identity{})
	err := c.Do(context.Background(), http.MethodGet, "/api/v1/ping", nil, nil)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("非 JSON 错误体应兜底为 *APIError, 实际 = %v（%T）", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, 期望 %d", apiErr.StatusCode, http.StatusBadGateway)
	}
	if apiErr.Code != "" {
		t.Errorf("非标准错误体 Code 应为空串, 实际 = %q", apiErr.Code)
	}
	if !strings.Contains(apiErr.Message, "502") {
		t.Errorf("Message 应含原始响应片段, 实际 = %q", apiErr.Message)
	}
}

func TestRequestBodyAndNilBody(t *testing.T) {
	var gotBody []byte
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotContentType = r.Header.Get("Content-Type")
		types.WriteData(w, http.StatusOK, map[string]any{})
	}))
	defer srv.Close()

	t.Run("nil body不设Content-Type", func(t *testing.T) {
		c := NewClient(srv.URL, "", Identity{})
		if err := c.Do(context.Background(), http.MethodGet, "/api/v1/projects", nil, nil); err != nil {
			t.Fatalf("Do 失败: %v", err)
		}
		if len(gotBody) != 0 {
			t.Errorf("nil body 不应携带请求体, 实际 = %q", gotBody)
		}
		if gotContentType != "" {
			t.Errorf("nil body 不应设 Content-Type, 实际 = %q", gotContentType)
		}
	})

	t.Run("body序列化为JSON", func(t *testing.T) {
		c := NewClient(srv.URL, "", Identity{})
		if err := c.Do(context.Background(), http.MethodPost, "/api/v1/projects",
			map[string]any{"code": "proj-a"}, nil); err != nil {
			t.Fatalf("Do 失败: %v", err)
		}
		if !strings.Contains(string(gotBody), `"code":"proj-a"`) {
			t.Errorf("请求体应含序列化字段, 实际 = %q", gotBody)
		}
		if gotContentType != types.ContentTypeJSON {
			t.Errorf("Content-Type = %q, 期望 %q", gotContentType, types.ContentTypeJSON)
		}
	})
}

func TestRedirectNotFollowed(t *testing.T) {
	// 重定向一律不跟随（审查 Important 1）：302 落非 2xx 分支转 APIError 透传
	// （CLI 退出 4，配置错误立刻可见），并杜绝 307 保留 method+body+四头跨域重发。
	var followed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/redirect" {
			http.Redirect(w, r, "/api/v1/other", http.StatusFound)
			return
		}
		followed = true
		types.WriteData(w, http.StatusOK, map[string]any{})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", Identity{})
	err := c.Do(context.Background(), http.MethodGet, "/api/v1/redirect", nil, nil)

	if followed {
		t.Error("不应跟随重定向到 /api/v1/other")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("302 应落 APIError 分支, 实际 = %v（%T）", err, err)
	}
	if apiErr.StatusCode != http.StatusFound {
		t.Errorf("StatusCode = %d, 期望 %d", apiErr.StatusCode, http.StatusFound)
	}
}

func TestOversizedResponseBody(t *testing.T) {
	// 响应体超 8MB 上限（审查 Important 2）：LimitReader 多读 1 字节判定超限，
	// 返回普通错误（协议异常→CLI 退出 1），不得误判为不可达或业务拒绝。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", maxRespBodyBytes+1)))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", Identity{})
	err := c.Do(context.Background(), http.MethodGet, "/api/v1/ping", nil, nil)

	if err == nil {
		t.Fatal("超限响应应返回错误")
	}
	var apiErr *APIError
	var ue *UnreachableError
	if errors.As(err, &apiErr) || errors.As(err, &ue) {
		t.Fatalf("超限属协议异常, 不应归类为 APIError/UnreachableError, 实际 = %v", err)
	}
	if !strings.Contains(err.Error(), "上限") {
		t.Errorf("错误信息应含「上限」, 实际 = %q", err.Error())
	}
}

// TestTokenOffNoAuthHeader AC17.2 CLI 面「token 关=零感知」对照锚定（B6-2）：
// 同一 httptest 下 token 关/开各发一请求，锁 Authorization 头的有无只由 Token
// 空非空决定。与既有覆盖分工不重复——TestHeadersInjected 主场是身份四头（附带
// 断言无 token 无 Authorization）、TestTokenResolution「token注入Authorization头」
// 主场是查找链→注入消费链；本测试把开关两面收敛成一对直接对照，头不存在用
// map 存在性判定（区别于值为空）。
func TestTokenOffNoAuthHeader(t *testing.T) {
	var gotAuth string
	var authPresent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, authPresent = r.Header[http.CanonicalHeaderKey("Authorization")]
		gotAuth = r.Header.Get("Authorization")
		types.WriteData(w, http.StatusOK, map[string]any{})
	}))
	defer srv.Close()

	t.Run("token关=头完全不存在", func(t *testing.T) {
		c := NewClient(srv.URL, "", Identity{Project: "proj-a"})
		if err := c.Do(context.Background(), http.MethodGet, "/api/v1/ping", nil, nil); err != nil {
			t.Fatalf("Do 失败: %v", err)
		}
		if authPresent {
			t.Errorf("token 关时请求仍带 Authorization 头（值 %q），违背零感知（§2.1 鉴权默认关）", gotAuth)
		}
	})

	t.Run("token开=Bearer头", func(t *testing.T) {
		c := NewClient(srv.URL, "tok", Identity{Project: "proj-a"})
		if err := c.Do(context.Background(), http.MethodGet, "/api/v1/ping", nil, nil); err != nil {
			t.Fatalf("Do 失败: %v", err)
		}
		if !authPresent || gotAuth != BearerPrefix+"tok" {
			t.Errorf("token 开时 Authorization 存在=%v 值=%q，期望存在且为 %q", authPresent, gotAuth, BearerPrefix+"tok")
		}
	})
}

func TestMalformedSuccessBody(t *testing.T) {
	// 2xx 但 body 非 JSON（审查 Minor 6）：「响应解析失败」普通错误（CLI 退出 1），
	// 非 APIError（服务端并未拒绝）、非 UnreachableError（连接是好的）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", Identity{})
	var out struct {
		Version string `json:"version"`
	}
	err := c.Do(context.Background(), http.MethodGet, "/api/v1/ping", nil, &out)

	if err == nil {
		t.Fatal("2xx 非 JSON body 应返回错误")
	}
	var apiErr *APIError
	var ue *UnreachableError
	if errors.As(err, &apiErr) || errors.As(err, &ue) {
		t.Fatalf("解析失败属协议异常, 不应归类为 APIError/UnreachableError, 实际 = %v", err)
	}
	if !strings.Contains(err.Error(), "响应解析失败") {
		t.Errorf("错误信息应含「响应解析失败」, 实际 = %q", err.Error())
	}
}
