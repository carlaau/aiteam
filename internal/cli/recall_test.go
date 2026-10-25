package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// recall 命令 CLI 测试（b5-T3，b5-spec §二.R4/AC4，M-36）：in-process 端到端
// （真 store + 真 server 挂 httptest，复用 startRegTestServer 夹具，命令函数
// serverOverride 注入地址）。覆盖 AC4 四形态：①happy path `OK recalled seq=<n>`
// （send 种消息→同发送方撤回）②409 家族文案透传（非发送方撤回→not_sender
// 服务端文案经 APIError 零改动直透，TestSendAgentConflictPassthrough 同锚定法）
// ③帮助面两处含 recall（总帮助源 grep+recall --help 用法行，TestUnarchiveHelpListsVerb
// 读源验收先例）④缺 --seq 本地退 2 不发请求（死端口手法）。
// 红线对位：五核心命令零触碰（本文件纯新增）；服务端文案为 b5-T2 产出，CLI 只
// 锚透传不锚实现。

// seedRecallable 种一条可撤消息：regAuth 身份（controller）发 direct 给 executor
// ——无 ack 位点推进，目标格信箱位点 < seq（全量未消费），撤回窗口在位（R1.3）。
// --no-mirror 跳过镜像双写（CLI 层测试不涉镜像面，send_test.go TestNoMirror 同款）。
func seedRecallable(t *testing.T, ctx context.Context, addr, body string) int64 {
	t.Helper()
	stdout, _ := runWantCode(t, "send 种消息", 0, func() int {
		return runSend(ctx, append(regIdentityArgs(),
			"--to-role", "executor", "--body", body, "--no-mirror"), addr)
	})
	seq, _ := parseOKLine(t, stdout)
	return seq
}

// TestRecallHappyPath AC4①：send 种消息后同发送方身份 recall → 退出 0 输出
// `OK recalled seq=<n>`（b5-spec R4 输出形状），stderr 零杂音。
func TestRecallHappyPath(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()
	seq := seedRecallable(t, ctx, addr, "发错了投错格的正文")

	stdout, stderr := runWantCode(t, "recall happy", 0, func() int {
		return runRecall(ctx, append(regIdentityArgs(),
			"--seq", strconv.FormatInt(seq, 10), "--reason", "投错格撤回"), addr)
	})
	if want := "OK recalled seq=" + strconv.FormatInt(seq, 10); !strings.Contains(stdout, want) {
		t.Errorf("recall 输出 = %q，期望含 %q（AC4① b5-spec R4 输出形状）", stdout, want)
	}
	if stderr != "" {
		t.Errorf("recall 成功不应有 stderr 输出，实际 %q", stderr)
	}
}

// TestRecallNotSenderPassthrough AC4②：非发送方撤回 → 409 not_sender 服务端
// 文案经 CLI fail→APIError 零改动直透（透传层零改动=设计约束，本测试是锚定而
// 非实现）：退出 4，stderr 逐字含错误码 not_sender 与服务端 message 骨架
// （「仅发送方本人可撤回」）。
func TestRecallNotSenderPassthrough(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()
	seq := seedRecallable(t, ctx, addr, "别人不能替我撤的正文")

	// 非发送方身份（executor-B/executor，同项目栏目已登记域）撤回 → 退 4。
	_, stderr := runWantCode(t, "非发送方撤回", 4, func() int {
		return runRecall(ctx, append(identityArgsAs("executor-B", "executor"),
			"--seq", strconv.FormatInt(seq, 10)), addr)
	})
	for _, want := range []string{"not_sender", "仅发送方本人可撤回"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("非发送方撤回 stderr = %q，不含 %q（AC4② 409 家族文案透传）", stderr, want)
		}
	}
}

// TestRecallHelpListsCommand AC4③：帮助面两处含 recall——①recall --help 用法行
// （退出 0，stderr 含 recall 用法与身份四参提示）；②总帮助子命令列表（cmd/aiteam/
// main.go usage() os.Exit 直退不可进程内调用，照 TestUnarchiveHelpListsVerb 读源
// 验收先例：源码文本含总帮助 recall 词条行）。
func TestRecallHelpListsCommand(t *testing.T) {
	// ① recall --help：退出 0（flag 包已输出用法），用法行含命令名与 --seq。
	var code int
	_, stderr := captureStdoutStderr(t, func() {
		code = runRecall(context.Background(), []string{"--help"}, "")
	})
	if code != 0 {
		t.Errorf("recall --help 退出码 = %d，期望 0（AC4③ 帮助面）", code)
	}
	for _, want := range []string{"aiteam recall", "--seq"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("recall --help 用法 = %q，不含 %q（AC4③）", stderr, want)
		}
	}

	// ② 总帮助子命令列表：读源 grep 词条行（行首两空格+recall 起头——与
	// case "recall": 分发行形状区分，锚定的是 usage() 列表面）。
	raw, err := os.ReadFile(filepath.Join("..", "..", "cmd", "aiteam", "main.go"))
	if err != nil {
		t.Fatalf("读总帮助源 cmd/aiteam/main.go 失败: %v", err)
	}
	if !strings.Contains(string(raw), "\n  recall ") {
		t.Errorf("总帮助源无 recall 子命令词条行（AC4③ 帮助面两处之二）")
	}
}

// TestRecallMissingSeq AC4④：缺 --seq → CLI 本地校验退 2 不发请求（§4.1 校验
// 缺漏不发请求；死端口手法：若误发请求将得退 3，实测 2 即证明本地拦截），
// 错误指明 --seq。
func TestRecallMissingSeq(t *testing.T) {
	dead := closedServerAddr(t)
	ctx := context.Background()

	_, stderr := runWantCode(t, "recall 缺 --seq", 2, func() int {
		return runRecall(ctx, regIdentityArgs(), dead)
	})
	if !strings.Contains(stderr, "--seq") {
		t.Errorf("缺 --seq stderr = %q，期望指明 --seq（AC4④）", stderr)
	}
}
