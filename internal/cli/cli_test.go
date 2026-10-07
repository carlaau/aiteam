package cli

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"aiteam/internal/client"
	"aiteam/internal/config"
	"aiteam/internal/server"
	"aiteam/internal/store"
)

// B6-2 测试收口（实现面 B1-6/B1-7 已吸收，本文件纯验收锚定，零实现改动）：
//   - TestMapExitCode：退出码矩阵（§4.7）+ 401/不可达语义区分（AC17.4）；
//   - TestAuthRequired401EndToEnd：token 开态无 token → 退出 4 + auth_required
//     透传（AC17.3 CLI 面），对照面锚定 --token flag → Bearer 注入链端到端；
//   - TestUnreachableStill3：不可达 → 退出 3（AC17.4 复验，与 401 严格区分）。

// startTokenEnabledServer 装配 token 开态 in-process serve（形态同 register_test.go
// startRegTestServer：临时目录真文件库 + 完整 server + httptest；server 包的
// newTestServer 跨包不可直用故自建），并 seed 引导域夹具——身份 project/column
// 已登记，使 401 唯一归因于缺 token（排除心跳中间件 404 短路的归因混淆），
// 对照用例（带正确 token）也才能穿过鉴权与心跳到达 mux。
func startTokenEnabledServer(t *testing.T, token string) string {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "auth-test.db"))
	if err != nil {
		t.Fatalf("打开临时 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.Auth.TokenEnabled = true
	cfg.Auth.Token = token
	ts := httptest.NewServer(server.NewServer(st, cfg, server.Version))
	t.Cleanup(ts.Close)
	if _, err := st.CreateProject(store.Project{Code: regAuthProject, Name: "引导项目"}); err != nil {
		t.Fatalf("seed 引导项目失败: %v", err)
	}
	if _, _, err := st.CreateColumnWithAudit(regAuthProject, store.Column{Code: regAuthColumn, Name: "引导栏目"},
		store.AuditEntry{Action: store.AuditColumnRegister, Detail: "{}"}); err != nil {
		t.Fatalf("seed 引导栏目失败: %v", err)
	}
	return ts.URL
}

// clearLookupChainEnv 清空服务地址/token 查找链的 env 两级：本文件用例经
// RunProjectGroup 走完整 flag 面，--server/--token flag 非空时 flag 直接胜出不受
// env 影响，但无 token 用例的期望前提是「查找链全空」——防测试机外部
// AITEAM_TOKEN/AITEAM_SERVER 污染（t.Setenv 结束自动恢复）。cli.json 两级指向
// 本包测试 CWD（internal/cli/.aiteam/ 与 ~/.aiteam/），仓库内无该文件，不设防。
func clearLookupChainEnv(t *testing.T) {
	t.Helper()
	t.Setenv(client.EnvServer, "")
	t.Setenv(client.EnvToken, "")
}

// TestMapExitCode 退出码矩阵锚定（§4.7）：用法=2 / 不可达=3 / 服务端拒绝=4
// （不分状态码，401 与 500 同判）/ 其余未知兜底=1。
func TestMapExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"用法错误退2", fmt.Errorf("包装: %w", ErrUsage), 2},
		{"服务不可达退3", &client.UnreachableError{Err: errors.New("connection refused"), ServerAddr: "http://127.0.0.1:1"}, 3},
		{"401鉴权拒绝退4", &client.APIError{StatusCode: 401, Code: "auth_required", Message: "缺少或错误的访问令牌"}, 4},
		{"500服务端拒绝同退4", &client.APIError{StatusCode: 500, Message: "internal"}, 4},
		{"未知错误兜底退1", errors.New("意外失败"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapExitCode(tc.err); got != tc.want {
				t.Errorf("mapExitCode(%v) = %d，期望 %d", tc.err, got, tc.want)
			}
		})
	}

	// 语义区分锚定（AC17.4 核心判据）：连着的服务端拒（401）与网络断（不可达）
	// 必须映射不同退出码——4≠3。用户据此分辨动作：4=查 token/配置，3=查网络/进程。
	got401 := mapExitCode(&client.APIError{StatusCode: 401, Code: "auth_required"})
	gotUnreach := mapExitCode(&client.UnreachableError{Err: errors.New("connection refused")})
	if got401 == gotUnreach {
		t.Errorf("401 与不可达映射了相同退出码 %d，违背 AC17.4 语义区分（期望 4≠3）", got401)
	}
}

// TestAuthRequired401EndToEnd AC17.3 CLI 端到端：token 开态 serve + 无 --token →
// 服务端 tokenAuth 先于 mux 拒 401 auth_required → CLI 退出 4 + stderr 原样透传
// 错误码。对照面（--token 正确 → 退出 0）锚定 B1 吸收的 --token flag → Client
// Bearer 注入链在完整组入口（RunProjectGroup，含查找链 flag 胜出）上端到端通。
func TestAuthRequired401EndToEnd(t *testing.T) {
	clearLookupChainEnv(t)
	const bootToken = "b6-auth-token"
	addr := startTokenEnabledServer(t, bootToken)

	// 无 --token：查找链全空 → Client 零 token → 401 → 退出 4。
	var code int
	_, stderr := captureStdoutStderr(t, func() {
		code = RunProjectGroup(append([]string{"list", "--server", addr}, regIdentityArgs()...))
	})
	if code != 4 {
		t.Errorf("token 开态无 --token 退出码 = %d（stderr: %q），期望 4（AC17.3）", code, stderr)
	}
	if !strings.Contains(stderr, "auth_required") {
		t.Errorf("stderr = %q，期望含 auth_required（服务端错误码原样透传）", stderr)
	}

	// 对照面：--token 正确 → 请求穿鉴权与心跳 → list 退出 0 且可见引导项目。
	var okCode int
	stdout, okStderr := captureStdoutStderr(t, func() {
		okCode = RunProjectGroup(append([]string{"list", "--server", addr, "--token", bootToken}, regIdentityArgs()...))
	})
	if okCode != 0 {
		t.Errorf("token 开态 --token 正确退出码 = %d（stderr: %q），期望 0（注入链端到端）", okCode, okStderr)
	}
	if !strings.Contains(stdout, regAuthProject) {
		t.Errorf("list stdout = %q，期望含引导项目 %s", stdout, regAuthProject)
	}
}

// TestUnreachableStill3 AC17.4 复验：--server 指向无监听端口 → 退出 3 + stderr
// 含「服务不可达」。与 TestAuthRequired401EndToEnd 成对锚定——同为请求失败，
// 网络断=3（查网络/进程）、服务端拒=4（查 token/配置），两语义不得混流。
func TestUnreachableStill3(t *testing.T) {
	clearLookupChainEnv(t)
	// 起后即关的 httptest 地址 = 127.0.0.1 上确定无监听的端口（closedServerAddr
	// 形态，确定性连接拒绝，不赌高位端口空闲）。
	dead := closedServerAddr(t)

	var code int
	_, stderr := captureStdoutStderr(t, func() {
		code = RunProjectGroup(append([]string{"list", "--server", dead}, regIdentityArgs()...))
	})
	if code != 3 {
		t.Errorf("服务不可达退出码 = %d（stderr: %q），期望 3（AC17.4）", code, stderr)
	}
	if !strings.Contains(stderr, "服务不可达") {
		t.Errorf("stderr = %q，期望含「服务不可达」", stderr)
	}

	// 与 401 严格区分的收口断言：同一命令函数上 3 与 4 互斥可辨。
	if code == 4 {
		t.Error("不可达误映射为 4（服务端拒绝），AC17.4 语义区分失效")
	}
}
