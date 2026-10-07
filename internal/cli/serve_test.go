package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// 检查 db 表清单需直接开 sqlite 连接（驱动经 store 间接引入，此处补注册）。
	_ "modernc.org/sqlite"
)

// serve 测试族端口策略（b2-spec W3 口径，改动前必读，防后人误改回）：
//
// 六个已隔离测试对本机端口占用零敏感——TestServeDefaultConfigFile / CustomConfig /
// GracefulShutdown / StartupEmptyToken 监听面一律 127.0.0.1:0 随机端口（writeTempConfig
// / writeTempConfigWithAuth 及 DefaultConfigFile 的刻意内联写配置固化该形态，见各自
// 用例自注）；RejectsPositionalArgs / HelpExitsZero 在监听前即拒绝/返回，不绑任何端口。
//
// 唯一例外 TestServeDefaultBoot 绑真默认端口 0.0.0.0:8310：其被测语义是「无配置文件→
// 内建默认配置生效」（AC16.1/AC17.2 拷贝即跑），默认监听地址本身就是断言对象，随机
// 端口替代不了——因此前置端口守卫（试绑 0.0.0.0:8310，与 serve 实际绑定同址）：8310
// 空闲才真跑，被占（本机常驻 aiteam serve）即 Skip 并注明原因，不是静默跳过。勿把
// DefaultBoot 改回随机端口（默认端口语义失真），也勿给其余六测绑固定端口（无必要，
// 且与常驻服务/并行测试互踩）。
//
// 序 9 部署结构拍平增补：serve_config_resolve_test.go 另有「自占 8310」形态（试绑
// 并持有 8310 令 serve 的 Listen 步必败，确定性断言 Listen 前的落盘行为，不依赖
// 环境端口空闲）——真启动语义仍只有 DefaultBoot 一家。

// TestMain 测试期静音 slog 默认 logger：serve 的 INFO 日志不混入测试输出。
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// wantServeTables §3.2 十一表清单（与 internal/store/store_test.go 同源；
// cli 全链测试不 import store 内部测试变量，此处独立断言「11 表齐」）。
// 第 11 表 progress_reports 系 d722b93 全局变更窗（FR23 进度上报）增补。
var wantServeTables = []string{
	"projects", "columns", "sessions", "sentinels", "messages",
	"message_receipts", "ack_positions", "resources", "stage_windows", "audit_log",
	"progress_reports",
}

// chdirForTest 切换进程工作目录至 dir，测试结束自动还原。
// serve 解析链已改 exe 目录基准（序 9，经 injectExeDir 隔离，见
// serve_config_resolve_test.go）；本 helper 仍服务 config init 等 CWD 语义的
// 维护命令测试（go.mod go 1.22 无 t.Chdir，手写等价物）。
func chdirForTest(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取工作目录失败: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("切换工作目录至 %s 失败: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Fatalf("还原工作目录失败: %v", err)
		}
	})
}

// startServeForTest 后台 goroutine 启动 serve（in-process，不起子进程），返回：
//   - readyCh：监听就绪后的实际地址（带缓冲；listen :0 随机端口场景的唯一同步点）
//   - errCh：  serve 的返回错误
//   - doneCh： serve goroutine 退出后关闭（无论 errCh 值是否已被读走）
//   - cancel： 停机触发（等价 SIGINT）
//
// Cleanup 兜底：cancel 后带超时等 doneCh 关闭（goroutine 退出、文件句柄全释放）
// 再交还 TempDir 清理——Windows 下避免文件锁竞速（审查 Minor 2）。
func startServeForTest(t *testing.T, args []string) (readyCh <-chan string, errCh <-chan error, doneCh <-chan struct{}, cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	errs := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		errs <- serve(ctx, args, func(addr string) { ready <- addr })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second): // 兜底不挂死：正常路径毫秒级返回
		}
	})
	return ready, errs, done, cancel
}

// waitReady 等待监听就绪并返回实际地址，5s 超时判失败。
// serve 提前退出时直接以其真实错误判失败（如端口被占的「监听失败」），
// 不落在笼统的「未就绪」上。
func waitReady(t *testing.T, ready <-chan string, errCh <-chan error) string {
	t.Helper()
	select {
	case addr := <-ready:
		return addr
	case err := <-errCh:
		t.Fatalf("serve 未就绪即退出: %v", err)
		return ""
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内监听未就绪")
		return ""
	}
}

// pingURL 把监听地址转成可 dial 的 URL：wildcard host（0.0.0.0/::）换回环 127.0.0.1。
func pingURL(addr string) string {
	if host, port, err := net.SplitHostPort(addr); err == nil && (host == "0.0.0.0" || host == "::") {
		return "http://" + net.JoinHostPort("127.0.0.1", port) + "/api/v1/ping"
	}
	return "http://" + addr + "/api/v1/ping"
}

// getHTTPStatus 对 url 发 GET，返回响应状态码。
func getHTTPStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// listUserTables 打开 path 处 sqlite 库，返回用户表名集合（过滤 sqlite_% 内部对象）。
func listUserTables(t *testing.T, path string) map[string]bool {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开 %s 检查表清单失败: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("查询 %s 表清单失败: %v", path, err)
	}
	defer func() { _ = rows.Close() }()
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("扫描表名失败: %v", err)
		}
		tables[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历表清单失败: %v", err)
	}
	return tables
}

// writeTempConfig 写一份最小自定义配置（随机回环端口 + 指定 db 路径），返回配置文件路径。
func writeTempConfig(t *testing.T, dbPath string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "aiteam-config.json")
	content := fmt.Sprintf(`{"listen": "127.0.0.1:0", "db": {"path": %s}}`, fmt.Sprintf("%q", dbPath))
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	return cfgPath
}

// assertPingOK 断言 addr 上 ping 返回 200。
func assertPingOK(t *testing.T, addr string) {
	t.Helper()
	if got := getHTTPStatus(t, pingURL(addr)); got != http.StatusOK {
		t.Errorf("GET ping 状态码 = %d，期望 %d", got, http.StatusOK)
	}
}

// assertTablesComplete 断言 dbPath 处库中 §3.2 十表齐。
func assertTablesComplete(t *testing.T, dbPath string) {
	t.Helper()
	tables := listUserTables(t, dbPath)
	for _, want := range wantServeTables {
		if !tables[want] {
			t.Errorf("表 %s 缺失（实际表: %v）", want, tables)
		}
	}
	if len(tables) != len(wantServeTables) {
		t.Errorf("用户表数量 = %d，期望 %d（多余对象: %v）", len(tables), len(wantServeTables), tables)
	}
}

// TestServeDefaultBoot AC16.1 全链：exe 目录空+无配置文件→默认配置启动→
// ping 可达→默认路径 db 文件（exe 目录 db/ 子目录，序 9 拍平语义）生成且
// §3.2 十表齐（拷贝即跑）。首跑会同时自生成 aiteam-config.json（W2，值面同
// 内建默认，不影响本测断言）。
//
// 前置端口守卫（b2-spec W2）：本测试是 serve 测试族唯一绑真默认端口 0.0.0.0:8310
// 的用例（被测语义=「无配置走内建默认」），8310 被本机常驻服务占用时无法真跑——
// 守卫用 net 试绑（跨平台，不依赖 netstat 外部命令），且必须试绑 serve 将绑的同一
// 地址 0.0.0.0:8310：试绑 127.0.0.1 探测不到 wildcard 占用（Windows 实测 0.0.0.0:8310
// 被占时 127.0.0.1:8310 仍可叠绑成功，守卫会成死代码）。成功即关并放行（8310 空闲，
// 真默认端口语义可验），被占即 Skipf 注明占用与执行环境口径。默认值字面已由
// internal/config 的 TestDefaults 单元断言锚定（不起服务的单元面）。
func TestServeDefaultBoot(t *testing.T) {
	if ln, err := net.Listen("tcp", "0.0.0.0:8310"); err != nil {
		t.Skipf("本地常驻服务占用 8310（试绑 0.0.0.0:8310 失败: %v）——本测试验证默认端口语义，须在 CI 干净环境执行", err)
	} else {
		_ = ln.Close()
	}

	tmp := t.TempDir()
	injectExeDir(t, tmp) // serve 解析链 exe 目录基准（序 9）：探测/生成/db 落位全锚此处

	ready, errs, _, _ := startServeForTest(t, nil) // 无参数=无 --config，走内建默认
	addr := waitReady(t, ready, errs)

	// 对实际监听地址探活（默认 0.0.0.0:8310 → 回环访问）
	assertPingOK(t, addr)

	// 默认路径 db 文件已生成（exe 目录 db/ 子目录，db/ 由 serve 自动创建）
	dbPath := filepath.Join(tmp, "db", "aiteam.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("默认路径 db 文件未生成: %v", err)
	}

	// §3.2 十表齐
	assertTablesComplete(t, dbPath)
}

// TestServeDefaultConfigFile §4.6 默认配置查找（序 9 拍平后基准=exe 目录）：
// exe 目录放 aiteam-config.json 且无 --config 启动→配置生效（回归：serve 层曾缺
// 探测逻辑致配置被静默忽略）。
func TestServeDefaultConfigFile(t *testing.T) {
	tmp := t.TempDir()
	injectExeDir(t, tmp)

	dbPath := filepath.Join(tmp, "from-default-config", "aiteam.db")
	// 文件名 aiteam-config.json 即回归核心，落 exe 目录（部署根）不藏 helper；
	// db 嵌套目录不预建——serve 的 MkdirAll 自动创建（序 9）
	content := fmt.Sprintf(`{"listen": "127.0.0.1:0", "db": {"path": %s}}`, fmt.Sprintf("%q", dbPath))
	if err := os.WriteFile(filepath.Join(tmp, "aiteam-config.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("写默认配置文件失败: %v", err)
	}

	ready, errs, _, _ := startServeForTest(t, nil) // 无参启动：应探测到 exe 目录的 aiteam-config.json
	addr := waitReady(t, ready, errs)

	// 按配置文件监听（而非内建默认 0.0.0.0:8310）
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("实际监听地址 = %s，期望按默认配置文件 127.0.0.1:<随机端口>", addr)
	}
	if strings.HasSuffix(addr, ":8310") {
		t.Errorf("实际监听地址 = %s，默认配置文件未生效（落在内建默认端口）", addr)
	}

	// 按配置文件新地址可达 + 新路径建库
	assertPingOK(t, addr)
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("按默认配置文件路径的 db 未生成: %v", err)
	}
	assertTablesComplete(t, dbPath)
}

// TestServeCustomConfig AC17.1：--config 生效——按配置新地址可达、按配置新路径建库。
func TestServeCustomConfig(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "custom", "aiteam.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("建自定义 db 目录失败: %v", err)
	}
	cfgPath := writeTempConfig(t, dbPath)

	ready, errs, _, _ := startServeForTest(t, []string{"--config", cfgPath})
	addr := waitReady(t, ready, errs)

	// listen 配置为 127.0.0.1:0：实际地址应是随机端口的回环地址（而非默认 0.0.0.0:8310）
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("实际监听地址 = %s，期望 127.0.0.1:<随机端口>", addr)
	}
	if strings.HasSuffix(addr, ":8310") {
		t.Errorf("实际监听地址 = %s，不应落在默认端口 8310", addr)
	}

	// 按新地址可达
	assertPingOK(t, addr)

	// 按新路径建库
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("自定义路径 db 文件未生成: %v", err)
	}
	assertTablesComplete(t, dbPath)
}

// TestGracefulShutdown §9.5 优雅停机：ctx 取消（等价 SIGINT）→
// serve 在 Shutdown 10s 上限内无错误返回。
func TestGracefulShutdown(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "aiteam.db")
	cfgPath := writeTempConfig(t, dbPath)

	ready, errs, _, cancel := startServeForTest(t, []string{"--config", cfgPath})
	addr := waitReady(t, ready, errs)

	// 先确认服务在正常工作
	assertPingOK(t, addr)

	start := time.Now()
	cancel() // 等价 SIGINT（§9.5：生产路径经 signal.NotifyContext 转成同一 ctx 取消）
	select {
	case err := <-errs:
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("停机返回错误: %v", err)
		}
		if elapsed >= 10*time.Second {
			t.Errorf("停机耗时 %v，应在 10s 收敛上限内（实测应远小于）", elapsed)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("12s 内停机未返回（Shutdown 疑似死等）")
	}

	// 停机后文件锁应释放：重新打开同一 db 文件可读写（store.Close 回归）
	reopen, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("停机后重新打开 db 失败: %v", err)
	}
	defer func() { _ = reopen.Close() }()
	if err := reopen.Ping(); err != nil {
		t.Errorf("停机后 db Ping 失败（文件锁未释放？）: %v", err)
	}
}

// TestServeRejectsPositionalArgs 审查 Important 1：`aiteam serve myconfig.json`
// 忘打 --config，位置参数曾被静默忽略并按默认配置启动——多余位置参数必须
// 拒绝（ErrUsage，§4.7 退出码 2），且不得监听、不得开库。
func TestServeRejectsPositionalArgs(t *testing.T) {
	tmp := t.TempDir()
	injectExeDir(t, tmp)

	ready, errs, _, _ := startServeForTest(t, []string{"myconfig.json"})
	select {
	case err := <-errs:
		if !errors.Is(err, ErrUsage) {
			t.Errorf("错误 = %v，期望 ErrUsage（参数/用法错误）", err)
		}
	case addr := <-ready:
		t.Fatalf("位置参数应被拒绝，实际却启动监听于 %s", addr)
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内未返回，位置参数疑似被静默忽略并正常启动")
	}

	// 拒绝发生在开库前：exe 目录不应生成库文件/库目录（序 9：默认库锚 exe 目录 db/）
	if _, err := os.Stat(filepath.Join(tmp, "aiteam.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("拒绝路径不应开库，exe 目录出现 aiteam.db（stat err=%v）", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("拒绝路径不应建库目录，exe 目录出现 db/（stat err=%v）", err)
	}
}

// writeTempConfigWithAuth 写一份带 auth 段的最小配置（随机回环端口 + 指定 db 路径
// + token 开关与令牌值），返回配置文件路径。
func writeTempConfigWithAuth(t *testing.T, dbPath string, tokenEnabled bool, token string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "aiteam-config.json")
	content := fmt.Sprintf(`{"listen": "127.0.0.1:0", "db": {"path": %s}, "auth": {"token_enabled": %t, "token": %s}}`,
		fmt.Sprintf("%q", dbPath), tokenEnabled, fmt.Sprintf("%q", token))
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	return cfgPath
}

// TestServeStartupEmptyToken B6-1 启动校验：auth.token_enabled=true 且 auth.token
// 为空 → serve 拒启，错误信息含「token 开启但为空」（校验在 Load 后、开库前——
// 拒启路径不得生成 db 文件）；合法组合（开启且有 token）正常启动。
// 直调 serve(ctx, args, nil) 断言错误串——放 server 包会 import cli 成环，故落本包。
// 拒启用例的 ctx 带 5s 超时兜底：校验未生效（红阶段）时 serve 会真启动并阻塞，
// 超时取消使其以 nil 收尾、测试以「应拒启却无错误」判红，而非挂死。
func TestServeStartupEmptyToken(t *testing.T) {
	t.Run("开启但空token拒启", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "aiteam.db")
		cfgPath := writeTempConfigWithAuth(t, dbPath, true, "")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := serve(ctx, []string{"--config", cfgPath}, nil)

		if err == nil {
			t.Fatal("token 开启但为空应拒启，实际无错误返回")
		}
		if !strings.Contains(err.Error(), "token 开启但为空") {
			t.Errorf("错误信息 %q 不含「token 开启但为空」", err.Error())
		}
		// 拒启发生在开库前：db 文件不应生成
		if _, statErr := os.Stat(dbPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("拒绝路径不应开库，db 文件已生成（stat err=%v）", statErr)
		}
	})

	t.Run("开启且有token正常启动", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "aiteam.db")
		cfgPath := writeTempConfigWithAuth(t, dbPath, true, "boot-token")

		ready, errs, _, _ := startServeForTest(t, []string{"--config", cfgPath})
		addr := waitReady(t, ready, errs)
		assertPingOK(t, addr) // ping 豁免鉴权：开态下无 token 探活仍 200
	})
}

// TestServeHelpExitsZero 审查 Minor 1：`aiteam serve --help` 输出用法后按 CLI
// 惯例以 nil 返回（等价退出码 0），而非当作用法错误退 2。
func TestServeHelpExitsZero(t *testing.T) {
	ready, errs, _, _ := startServeForTest(t, []string{"--help"})
	select {
	case err := <-errs:
		if err != nil {
			t.Errorf("--help 应以 nil 返回（等价退出码 0），实际返回 %v", err)
		}
	case addr := <-ready:
		t.Fatalf("--help 不应启动监听，实际监听于 %s", addr)
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内未返回")
	}
}
