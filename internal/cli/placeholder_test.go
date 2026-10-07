package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #15 小批派工（FX 断点④·占位符原样复制防线·S6 用户实测）：README 示例以
// <xxx> 标注占位，用户原样复制——Windows cmd 将 < 解析为输入重定向（「找不到
// 文件」），bash/PowerShell 下字面尖括号值静默入参同样错。防线=parseFlags 解析
// 骨架统一扫描非布尔 flag 值（--body 正文豁免），含 < 或 > 即 ErrUsage 教学式
// 报错（退 2 不发请求）。三类反例：init（--project）/身份四参（send --session）/
// 登记族（register --code）；布尔 flag 与正常值不误伤。

// runWantPlaceholderRejected 在 dir 内跑 fn（fn 内部自分流退出码并打 stderr），
// 断言退 2 且 stderr 含 wants 全部片段，返回 stderr 供文案细查。
func runWantPlaceholderRejected(t *testing.T, dir, name string, wants []string, fn func() int) string {
	t.Helper()
	chdirForTest(t, dir)
	var code int
	_, stderr := captureStdoutStderr(t, func() { code = fn() })
	if code != 2 {
		t.Fatalf("%s 退出码 = %d，期望 2（占位符用法错误，stderr: %q）", name, code, stderr)
	}
	for _, w := range wants {
		if !strings.Contains(stderr, w) {
			t.Errorf("%s stderr 缺 %q: %q", name, w, stderr)
		}
	}
	return stderr
}

// TestPlaceholderInit 占位符反例·init：--project 原样尖括号 → ErrUsage 教学
// 文案（RunInit 返回 error 语义，main 层 configCmd/ initCmd 同款分流退 2——
// 文案断言直接对 error；ExitCodeInit 占位符反例·退码直证见
// TestPlaceholderInitReturnCode）。
func TestPlaceholderInit(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	err := RunInit([]string{"--project", "<erp_test>", "--name", "测试"})
	if err == nil {
		t.Fatal("init 占位符未拦截")
	}
	msg := err.Error()
	for _, w := range []string{"占位符", "--project", "不含尖括号", "<erp_test>"} {
		if !strings.Contains(msg, w) {
			t.Errorf("init 错误信息缺 %q: %q", w, msg)
		}
	}
	// 拒绝后零产物（不发请求不写盘——教学性防线在解析层短路）。
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); !os.IsNotExist(err) {
		t.Errorf("拒绝后不应有产物落盘: %v", err)
	}
}

// TestPlaceholderInitReturnCode init 的 ErrUsage→退 2 直证（不经等价模拟）：
// errors.Is 语义在 RunInit 返回值上判定。
func TestPlaceholderInitReturnCode(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	err := RunInit([]string{"--project", "<erp_test>", "--name", "测试"})
	if !errorsIsUsage(err) {
		t.Fatalf("init 占位符期望 ErrUsage，得 %v", err)
	}
}

// TestPlaceholderIdentity 占位符反例·身份四参：--session 原样尖括号 → 退 2。
// send 本地校验前置（退 2 而非退 3 即证明未发请求——死端口地址退 3 是发过的
// 不可达，此处 2 是本地拦）。
func TestPlaceholderIdentity(t *testing.T) {
	runWantPlaceholderRejected(t, t.TempDir(), "send --session <name>",
		[]string{"占位符", "--session", "不含尖括号"},
		func() int {
			return RunSend([]string{
				"--project", "p-a", "--column", "05", "--session", "<name>", "--role", "controller",
				"--server", "http://127.0.0.1:1", "--to-role", "executor", "--no-mirror", "--body", "hi",
			})
		})
}

// TestPlaceholderRegister 占位符反例·登记族：project register --code 原样尖括号
// → 退 2（身份占位与 --code 占位逐 flag 列明）。
func TestPlaceholderRegister(t *testing.T) {
	msg := runWantPlaceholderRejected(t, t.TempDir(), "project register --code <code>",
		[]string{"占位符", "--code", "不含尖括号"},
		func() int {
			return RunProjectGroup([]string{
				"register", "--project", "p-x", "--column", "05", "--session", "s-a", "--role", "controller",
				"--server", "http://127.0.0.1:1", "--code", "<code>",
			})
		})
	if !strings.Contains(msg, "--code") {
		t.Errorf("错误信息应指明占位 flag --code: %q", msg)
	}
}

// TestPlaceholderNoFalsePositive 正常值不误伤两态：①标识类参数合法值（连字符/
// 下划线/中文）放行；②--body 消息正文豁免占位符扫描（正文是自由文本，合法含
// 尖括号——如「List<int> 泛型」类描述；豁免名单仅此一项，parseFlags 注释口径）。
func TestPlaceholderNoFalsePositive(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	if err := RunInit([]string{"--project", "erp_test-web_2", "--name", "项目示例（中文与-_/混排）"}); err != nil {
		t.Errorf("标识类正常值被误伤: %v", err)
	}
	code := RunSend([]string{
		"--project", "p-a", "--column", "05", "--session", "s-a", "--role", "controller",
		"--server", "http://127.0.0.1:1", "--to-role", "executor", "--no-mirror",
		"--body", "类型写作 List<int> 的消息正文应放行",
	})
	if code == 2 {
		t.Error("--body 含尖括号被误伤（正文自由文本豁免面，退 2=占位符拦截）")
	}
}

// errorsIsUsage ErrUsage 判定别名（init 路径返回 error 语义；send/register 族
// 内部自分流退码不经此）。
func errorsIsUsage(err error) bool {
	return err != nil && strings.Contains(err.Error(), "用法")
}
