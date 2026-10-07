package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// poll/ack/history 三命令 CLI 测试（B2-7，in-process 端到端）：真 store + 真
// server 挂 httptest（复用 register_test.go 夹具 startRegTestServer），CLI 命令
// 函数注入其地址（serverOverride）；send 复用 B2-6 的 runSend 种消息（镜像根一律
// --mirror-root 指向 t.TempDir()）。覆盖：§4.2 poll/ack 输出样例逐字对拍、
// AC8.1 幂等重拉、AC9.1/9.2 位点推进与幂等、AC7.2/7.3 回执状态端到端、AC15.2
// 会话对话命令面、AC13.4 poll 半边（镜像损坏不影响 poll）、退出码 0/2/4。

// sendOK 以全局身份跑一次 send 退 0，返回服务端分配的 seq 与 created_at
// （B2-6 的 parseOKLine 解析 OK 行；extra 为目标/level 等追加 flag）。
func sendOK(t *testing.T, addr, mirrorRoot string, extra ...string) (int64, string) {
	t.Helper()
	stdout, _ := runWantCode(t, "send 种消息", 0, func() int {
		return runSend(context.Background(),
			append(append(regIdentityArgs(), extra...), "--mirror-root", mirrorRoot), addr)
	})
	return parseOKLine(t, stdout)
}

// identityArgs 变体构造：以 regIdentityArgs 为底替换 session/role（executor-B
// 等第二会话身份；project/column 恒引导域——消息与位点都在该域）。
func identityArgsAs(session, role string) []string {
	// integArgs 泛化版消重收编（B2 整批自审 R3）：引导域定参薄封装。
	return integArgs(regAuthProject, regAuthColumn, session, role)
}

// TestPollOutput §4.2 poll 输出样例逐字对拍：五要素逐条（seq/level/created_at/
// sender 身份/body）+kind 标注+多行正文缩进展示+尾部摘要行；--json 与 #10 响应
// data 同构（恰三键，messages 元素恰六键）。
func TestPollOutput(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()

	// 种两条：direct（block 级多行正文，发往本格子）+ bus（important）。
	// 引导栏目 seed 时位点预置 0（InitColumnPositions，当时 MAX(seq)=0）且未
	// ack，故 poll 响应双位点均为 0——期望串据此逐字拼装。
	seq1, ts1 := sendOK(t, addr, root, "--to-role", regAuthRole, "--level", "block",
		"--body", "第一行\n第二行")
	seq2, ts2 := sendOK(t, addr, root, "--bus", "--level", "important", "--body", "bus 正文")

	stdout, stderr := runWantCode(t, "poll", 0, func() int {
		return runPoll(ctx, regIdentityArgs(), addr)
	})
	if stderr != "" {
		t.Errorf("poll 成功不应有 stderr 输出，实际 %q", stderr)
	}
	label := regAuthSession + "@" + regAuthColumn
	want := fmt.Sprintf(
		"[%d] block  %s  %s(direct)\n      第一行\n      第二行\n"+
			"[%d] important  %s  %s(bus)\n      bus 正文\n"+
			"# pending=2 mailbox_pos=0 dialog_pos=0\n",
		seq1, ts1, label, seq2, ts2, label)
	if stdout != want {
		t.Errorf("poll 输出逐字对拍失败\n得到: %q\n期望: %q", stdout, want)
	}

	// --json 与 #10 响应 data 同构：恰三键 + messages 元素恰六键 + 字段值。
	stdout, _ = runWantCode(t, "poll --json", 0, func() int {
		return runPoll(ctx, append(regIdentityArgs(), "--json"), addr)
	})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &raw); err != nil {
		t.Fatalf("poll --json 输出非合法 JSON: %v\n%s", err, stdout)
	}
	if len(raw) != 3 {
		t.Errorf("poll --json 键数 = %d，期望恰 3 键与 #10 响应同构（实际键 %v）", len(raw), raw)
	}
	for _, k := range []string{"mailbox_position", "dialog_position", "messages"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("poll --json 缺键 %q（#10 响应同构）", k)
		}
	}
	var parsed struct {
		MailboxPosition int64 `json:"mailbox_position"`
		DialogPosition  int64 `json:"dialog_position"`
		Messages        []struct {
			Seq    int64  `json:"seq"`
			Kind   string `json:"kind"`
			Level  string `json:"level"`
			Body   string `json:"body"`
			Sender struct {
				Session string `json:"session"`
				Role    string `json:"role"`
				Column  string `json:"column"`
			} `json:"sender"`
			CreatedAt string `json:"created_at"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &parsed); err != nil {
		t.Fatalf("poll --json 解析失败: %v", err)
	}
	if len(parsed.Messages) != 2 {
		t.Fatalf("poll --json messages 应 2 条，实际 %d", len(parsed.Messages))
	}
	m0 := parsed.Messages[0]
	if m0.Seq != seq1 || m0.Kind != "direct" || m0.Level != "block" || m0.Body != "第一行\n第二行" ||
		m0.Sender.Session != regAuthSession || m0.Sender.Column != regAuthColumn || m0.CreatedAt != ts1 {
		t.Errorf("poll --json messages[0] = %+v，与首条 direct 消息不符", m0)
	}
	if parsed.MailboxPosition != 0 || parsed.DialogPosition != 0 {
		t.Errorf("poll --json 位点 = %d/%d，期望 0/0（GET 零推进）", parsed.MailboxPosition, parsed.DialogPosition)
	}
}

// TestPollIdempotentReread AC8.1 端到端：send×3 → poll 3 条升序 → 未 ack 重复
// poll 输出同批（逐字节相等，GET 零推进）。
func TestPollIdempotentReread(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()

	seqs := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		seq, _ := sendOK(t, addr, root, "--to-role", regAuthRole, "--body", fmt.Sprintf("第 %d 条", i+1))
		seqs = append(seqs, seq)
	}
	if seqs[0] >= seqs[1] || seqs[1] >= seqs[2] {
		t.Fatalf("send seq 应严格递增: %v", seqs)
	}

	pollOnce := func() string {
		stdout, _ := runWantCode(t, "poll", 0, func() int {
			return runPoll(ctx, regIdentityArgs(), addr)
		})
		return stdout
	}
	first, second := pollOnce(), pollOnce()
	if first != second {
		t.Errorf("幂等重拉输出不一致（AC8.1）\n第一次: %q\n第二次: %q", first, second)
	}
	for i, seq := range seqs {
		if !strings.Contains(first, fmt.Sprintf("[%d] normal", seq)) {
			t.Errorf("第 %d 条 seq=%d 未出现在 poll 输出（或顺序非升序）:\n%s", i+1, seq, first)
		}
	}
	if !strings.Contains(first, fmt.Sprintf("# pending=3 mailbox_pos=%d", 0)) {
		t.Errorf("poll 摘要行不符（期望 pending=3 位点未推进）:\n%s", first)
	}
}

// TestAckFlow AC9.1/9.2 端到端：ack --seq N 后 poll 空；省略 seq 一键推进到
// 各自最大可见；重复 ack（显式 seq 与省略两形态）幂等输出一致。
func TestAckFlow(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()

	_, _ = sendOK(t, addr, root, "--to-role", regAuthRole, "--body", "甲")
	seq2, _ := sendOK(t, addr, root, "--to-role", regAuthRole, "--body", "乙")

	// 显式 --seq 推进：无 --receipt 时成功行省略 receipts 段（§4.2 样例 receipts
	// 段仅回执场景出现；裁量见 ack.go 实现注）。
	stdout, _ := runWantCode(t, "ack --seq", 0, func() int {
		return runAck(ctx, append(regIdentityArgs(), "--seq", fmt.Sprintf("%d", seq2)), addr)
	})
	if want := fmt.Sprintf("OK position=%d\n", seq2); stdout != want {
		t.Errorf("ack --seq 输出 = %q，期望 %q", stdout, want)
	}
	// AC9.1：ack 后 poll 空（摘要行 pending=0，信箱位点=seq2；#11 显式 seq 双
	// 维度推进——对话位点同步到 seq2，AdvancePositions 单事务双 upsert）。
	stdout, _ = runWantCode(t, "ack 后 poll", 0, func() int {
		return runPoll(ctx, regIdentityArgs(), addr)
	})
	if want := fmt.Sprintf("# pending=0 mailbox_pos=%d dialog_pos=%d\n", seq2, seq2); stdout != want {
		t.Errorf("ack 后 poll = %q，期望 %q", stdout, want)
	}

	// 省略 seq 一键推进：再种一条，ack 无 --seq → 推进到当前最大可见 seq3。
	seq3, _ := sendOK(t, addr, root, "--to-role", regAuthRole, "--body", "丙")
	stdout, _ = runWantCode(t, "ack 省略 seq", 0, func() int {
		return runAck(ctx, regIdentityArgs(), addr)
	})
	if want := fmt.Sprintf("OK position=%d\n", seq3); stdout != want {
		t.Errorf("ack 省略 seq 输出 = %q，期望 %q", stdout, want)
	}

	// AC9.2 幂等输出一致：显式 seq 与省略 seq 两形态各重跑一次，输出逐字节相等
	// （position 取 max 不回退；省略形态的最大可见只依赖消息表不依赖当前位点）。
	rerun := func(name string, args []string) string {
		stdout, _ := runWantCode(t, name, 0, func() int {
			return runAck(ctx, args, addr)
		})
		return stdout
	}
	explicit := fmt.Sprintf("--seq=%d", seq3)
	if a, b := rerun("ack 显式重放", append(regIdentityArgs(), explicit)), rerun("ack 显式重放二", append(regIdentityArgs(), explicit)); a != b {
		t.Errorf("重复 ack（显式 seq）输出不一致（AC9.2）\n%q vs %q", a, b)
	}
	if a, b := rerun("ack 省略重放", regIdentityArgs()), rerun("ack 省略重放二", regIdentityArgs()); a != b {
		t.Errorf("重复 ack（省略 seq）输出不一致（AC9.2）\n%q vs %q", a, b)
	}
	// 幂等后 poll 仍空（重复 ack 零副作用）。
	stdout, _ = runWantCode(t, "幂等后 poll", 0, func() int {
		return runPoll(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, "# pending=0") {
		t.Errorf("幂等 ack 后 poll 应仍空:\n%s", stdout)
	}

	// 本地校验：--seq 非数字/负值 → 退 2 零请求（死端口手法，误发将退 3）。
	dead := closedServerAddr(t)
	for name, bad := range map[string]string{"非数字": "abc", "负值": "-1"} {
		_, stderr := runWantCode(t, "ack --seq "+name, 2, func() int {
			return runAck(ctx, append(regIdentityArgs(), "--seq", bad), dead)
		})
		if !strings.Contains(stderr, "--seq") {
			t.Errorf("ack --seq %s stderr = %q，期望指明 --seq", name, stderr)
		}
	}
}

// TestAckReceipt §4.2 回执行+AC7 全链命令面：ack --receipt 输出
// `OK position=... receipts=[...]`；位点推进与回执并存一次调用；回告消息到达
// 原发送方对话分支；不存在的 seq 退 4 且 stderr 指明该 seq（已成功前缀不回滚）。
func TestAckReceipt(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	executor := identityArgsAs("executor-B", "executor")

	// send block 给 executor 格子 → executor-B 一键 ack（省略 seq=位点推进到
	// 最大可见）+回执并存一次调用。
	seq1, _ := sendOK(t, addr, root, "--to-role", "executor", "--level", "block",
		"--body", "需要回执的阻断消息")
	stdout, _ := runWantCode(t, "ack --receipt", 0, func() int {
		return runAck(ctx, append(executor, "--receipt", fmt.Sprintf("%d", seq1)), addr)
	})
	if want := fmt.Sprintf("OK position=%d receipts=[%d]\n", seq1, seq1); stdout != want {
		t.Errorf("ack --receipt 输出 = %q，期望 %q", stdout, want)
	}
	// 位点确已推进：executor-B poll 空（AC9.1）。
	stdout, _ = runWantCode(t, "回执方 poll", 0, func() int {
		return runPoll(ctx, executor, addr)
	})
	if !strings.Contains(stdout, "# pending=0") {
		t.Errorf("回执方 ack 后 poll 应空:\n%s", stdout)
	}

	// 显式 --seq 与 --receipt 并存一次调用（§4.2「多位点回执并存于一次调用」）。
	seq2, _ := sendOK(t, addr, root, "--to-role", "executor", "--level", "block",
		"--body", "第二条阻断消息")
	stdout, _ = runWantCode(t, "ack seq+receipt 并存", 0, func() int {
		return runAck(ctx, append(executor,
			"--seq", fmt.Sprintf("%d", seq2),
			"--receipt", fmt.Sprintf("%d", seq2)), addr)
	})
	if want := fmt.Sprintf("OK position=%d receipts=[%d]\n", seq2, seq2); stdout != want {
		t.Errorf("ack 并存输出 = %q，期望 %q", stdout, want)
	}

	// AC7 全链：回告消息到达原发送方（controller-A）poll 对话分支（kind=receipt，
	// body=服务端回告格式 §2.3）。两次回执各产生一条回告（seq1 与 seq2 的回执），
	// controller-A 对话位点未动（0）→ 两条都拉到；信箱位点不受对话拉取影响。
	stdout, _ = runWantCode(t, "原发送方 poll 回告", 0, func() int {
		return runPoll(ctx, regIdentityArgs(), addr)
	})
	for _, want := range []string{
		"(receipt)",
		fmt.Sprintf("receipt: seq=%d by executor-B at ", seq1),
		fmt.Sprintf("receipt: seq=%d by executor-B at ", seq2),
		"# pending=2 mailbox_pos=0 dialog_pos=0",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("回告 poll 输出缺 %q：\n%s", want, stdout)
		}
	}

	// 失败路径一（首条即失败）：--receipt 单个不存在的 seq → 退 4，stderr 指明
	// 失败 seq 与错误码；首条失败无已成功前缀，不应出现「已成功回执 []」空段
	// （B2-7 复核 R1）。
	_, stderr := runWantCode(t, "ack --receipt 404", 4, func() int {
		return runAck(ctx, append(executor, "--receipt", "999999"), addr)
	})
	if !strings.Contains(stderr, "999999") || !strings.Contains(stderr, "message_not_found") {
		t.Errorf("ack --receipt 404 stderr = %q，期望指明 seq=999999 与 message_not_found", stderr)
	}
	if strings.Contains(stderr, "已成功回执") {
		t.Errorf("首条即失败不应出现已成功回执段: %q", stderr)
	}

	// 失败路径二（次条失败）：seq2 已回执过（重放幂等 200）、999999 失败 →
	// stderr 含已成功前缀 [seq2]（不回滚，可重跑幂等补齐）与失败 seq。
	_, stderr = runWantCode(t, "ack --receipt 次条失败", 4, func() int {
		return runAck(ctx, append(executor,
			"--receipt", fmt.Sprintf("%d,999999", seq2)), addr)
	})
	if !strings.Contains(stderr, fmt.Sprintf("已成功回执 [%d]", seq2)) ||
		!strings.Contains(stderr, "999999") {
		t.Errorf("次条失败 stderr = %q，期望含已成功回执 [%d] 与 999999", stderr, seq2)
	}
}

// TestAckReceiptLocalValidation M1 本地校验（B2-7 复核）：--receipt 空片段
// （尾随逗号/纯空白段）与 0 值片段（纯零/混入）→ 本地退 2 零 HTTP。死端口手法：
// 误发请求将退 3（不可达）而非 2，退出码 2 即零请求证据；stderr 须指明 --receipt。
func TestAckReceiptLocalValidation(t *testing.T) {
	dead := closedServerAddr(t)
	ctx := context.Background()
	for _, tc := range []struct{ name, val string }{
		{"尾随空片段", "881,"},
		{"纯零值", "0"},
		{"零值混入", "881,0"},
		{"纯空白片段", "881, ,882"},
	} {
		_, stderr := runWantCode(t, "ack --receipt "+tc.name, 2, func() int {
			return runAck(ctx, append(regIdentityArgs(), "--receipt", tc.val), dead)
		})
		if !strings.Contains(stderr, "--receipt") {
			t.Errorf("ack --receipt %s(%q) stderr = %q，期望指明 --receipt", tc.name, tc.val, stderr)
		}
	}
}

// TestHistoryBlock §4.6 history + AC7.2/7.3 端到端：--level block 输出含回执
// 状态列——回执前「待回执」、回执后「已回执 by/at」；非 block 行状态列占位。
func TestHistoryBlock(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	executor := identityArgsAs("executor-B", "executor")

	seq1, ts1 := sendOK(t, addr, root, "--to-role", "executor", "--level", "block",
		"--body", "阻断消息甲")
	seq2, _ := sendOK(t, addr, root, "--to-role", "executor", "--level", "important",
		"--body", "普通重要消息")

	// AC7.2 回执前：block 行状态列=待回执；非 block 行状态列=占位符。
	stdout, _ := runWantCode(t, "history --level block", 0, func() int {
		return runHistory(ctx, append(regIdentityArgs(), "--level", "block"), addr)
	})
	label := regAuthSession + "@" + regAuthColumn
	wantLine := fmt.Sprintf("[%d] block direct %s 待回执 %s 阻断消息甲\n", seq1, label, ts1)
	if !strings.Contains(stdout, wantLine) {
		t.Errorf("history --level block 输出缺待回执行\n得到: %q\n期望含: %q", stdout, wantLine)
	}
	stdout, _ = runWantCode(t, "history 全量", 0, func() int {
		return runHistory(ctx, regIdentityArgs(), addr)
	})
	if !strings.Contains(stdout, fmt.Sprintf("[%d] important direct %s - ", seq2, label)) {
		t.Errorf("history 非 block 行状态列应为占位符:\n%s", stdout)
	}

	// AC7.3 回执后：状态流转=已回执 by=回执方会话 at=服务端时间。
	runWantCode(t, "回执", 0, func() int {
		return runAck(ctx, append(executor, "--receipt", fmt.Sprintf("%d", seq1)), addr)
	})
	stdout, _ = runWantCode(t, "history --level block 回执后", 0, func() int {
		return runHistory(ctx, append(regIdentityArgs(), "--level", "block"), addr)
	})
	if !strings.Contains(stdout, fmt.Sprintf("[%d]", seq1)) ||
		!strings.Contains(stdout, "已回执 by=executor-B at=") {
		t.Errorf("回执后 history 状态列未流转:\n%s", stdout)
	}
	if strings.Contains(stdout, "待回执") {
		t.Errorf("回执后不应再出现待回执:\n%s", stdout)
	}
}

// TestChatRoundtrip AC15.2 命令面：send --to-session 后目标会话 poll 对话分支
// 拉到（chat 消息，自发不回流由服务端谓词保证——此处验证命令面贯通）。
func TestChatRoundtrip(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	executor := identityArgsAs("executor-B", "executor")

	// 目标会话预注册（零消息杂音）：以 executor-B 身份 GET #13 upsert 会话
	// （send_test.go 的 historyProbe 同款手法）。
	historyProbe(t, addr, "executor-B", "executor", "")

	seq1, ts1 := sendOK(t, addr, root, "--to-session", "executor-B", "--body", "对话正文")
	stdout, _ := runWantCode(t, "目标会话 poll", 0, func() int {
		return runPoll(ctx, executor, addr)
	})
	label := regAuthSession + "@" + regAuthColumn
	want := fmt.Sprintf("[%d] normal  %s  %s(chat)\n      对话正文\n# pending=1 mailbox_pos=0 dialog_pos=0\n",
		seq1, ts1, label)
	if stdout != want {
		t.Errorf("目标会话 poll 输出逐字对拍失败\n得到: %q\n期望: %q", stdout, want)
	}
}

// TestPollMirrorTampered AC13.4 poll 半边：镜像文件被外部篡改/删除后 poll 命令
// 照常（判定不依赖镜像内容；poll 本不写镜像——断言镜像保持原样零触碰）。
func TestPollMirrorTampered(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	mirrorFile := sendMirrorPath(root, regAuthProject, regAuthColumn)

	seq1, _ := sendOK(t, addr, root, "--to-role", regAuthRole, "--body", "镜像将遭篡改的正文")
	pollExpectHit := func(stage string) {
		t.Helper()
		stdout, _ := runWantCode(t, stage+" 后 poll", 0, func() int {
			return runPoll(ctx, regIdentityArgs(), addr)
		})
		if !strings.Contains(stdout, fmt.Sprintf("[%d] normal", seq1)) || !strings.Contains(stdout, "镜像将遭篡改的正文") {
			t.Errorf("%s 后 poll 未拉到消息（判定不应依赖镜像）:\n%s", stage, stdout)
		}
	}

	// 外部篡改：poll 照常拉到，镜像字节保持篡改后原样（零触碰）。
	tampered := "外部篡改poll"
	if err := os.WriteFile(mirrorFile, []byte(tampered), 0o644); err != nil {
		t.Fatalf("篡改镜像文件失败: %v", err)
	}
	pollExpectHit("篡改")
	got, err := os.ReadFile(mirrorFile)
	if err != nil {
		t.Fatalf("读镜像文件失败: %v", err)
	}
	if string(got) != tampered {
		t.Errorf("篡改后镜像被 poll 触碰: %q，期望原样 %q", got, tampered)
	}

	// 外部删除：poll 照常同批（幂等重拉 AC8.1），且不重建镜像文件（零触碰）。
	if err := os.Remove(mirrorFile); err != nil {
		t.Fatalf("删除镜像文件失败: %v", err)
	}
	pollExpectHit("删除")
	if _, err := os.Stat(mirrorFile); !os.IsNotExist(err) {
		t.Errorf("poll 不应重建镜像文件，stat: %v", err)
	}
}

// TestHistoryLocalValidation S2/S3：--limit 负值本地退 2（B2-7 复核——与同批
// poll --limit、history --since-seq/--before-seq 的非负口径统一，负值=用法错误
// 不再静默落默认）、--order 白名单外退 2。死端口手法：误发请求将退 3 而非 2，
// 退出码 2 即零请求证据；stderr 须指明对应 flag。
func TestHistoryLocalValidation(t *testing.T) {
	dead := closedServerAddr(t)
	ctx := context.Background()
	for _, tc := range []struct{ name, flag, val string }{
		{"limit 负值", "--limit", "-1"},
		{"order 非法", "--order", "bogus"},
	} {
		_, stderr := runWantCode(t, "history "+tc.name, 2, func() int {
			return runHistory(ctx, append(regIdentityArgs(), tc.flag, tc.val), dead)
		})
		if !strings.Contains(stderr, tc.flag) {
			t.Errorf("history %s stderr = %q，期望指明 %s", tc.name, stderr, tc.flag)
		}
	}
}

// TestHistoryLimitZeroPassthrough S3：--limit 0 直通=不发 limit query 键（服务端
// #13 落默认 50，D8 的 0=全量仅属 poll #10；CLI audit「>0 才透传」同型口径）。
// query 参数层面断言而非种 51 条数结果数（轻量裁量）；对照 --limit 25 显式透传。
func TestHistoryLimitZeroPassthrough(t *testing.T) {
	ctx := context.Background()
	queryCh := make(chan url.Values, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queryCh <- r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"messages":[]}}`))
	}))
	t.Cleanup(ts.Close)

	runWantCode(t, "history --limit 0", 0, func() int {
		return runHistory(ctx, append(regIdentityArgs(), "--limit", "0"), ts.URL)
	})
	if q := <-queryCh; q.Has("limit") {
		t.Errorf("--limit 0 应不发 limit query 键（服务端默认 50 语义），实际 query=%v", q)
	}

	runWantCode(t, "history --limit 25", 0, func() int {
		return runHistory(ctx, append(regIdentityArgs(), "--limit", "25"), ts.URL)
	})
	if q := <-queryCh; q.Get("limit") != "25" {
		t.Errorf("--limit 25 应透传 limit=25，实际 query=%v", q)
	}
}

// ---- b7-W3 poll 跨格防呆提示（b7-spec §二.3，AC1 主验/AC2 零噪音/AC3 additive）----
//
// 提示逐字模板（spec §二.3，多格逐格一行）：
//
//	提示：本格（role=<自己的role>）无定向消息；同栏目 role=executor_A 格有 3 条未消费定向——若你的身份应为该格，检查 --role 是否填错
//
// stdout 渲染零改动（提示只进 stderr，管道消费者不受扰）。

// crossGridHintLine 拼一条防呆提示期望串（文本模式 stderr 恰一行+\n）。
func crossGridHintLine(ownRole, targetRole string, pending int) string {
	return fmt.Sprintf(
		"提示：本格（role=%s）无定向消息；同栏目 role=%s 格有 %d 条未消费定向——若你的身份应为该格，检查 --role 是否填错\n",
		ownRole, targetRole, pending)
}

// TestPollCrossGridHintAC1 AC1 实战剧本端到端（主验）：同栏目 executor_A 格落
// 3 条未消费 direct，CLI 以 --role executor poll（错格）→ rc=0、stdout 无消息体
// （仅摘要行——位点=登记预置 0）、stderr 恰一行防呆提示带 executor_A 与条数 3。
// 改造前红：cross_grid_hint 面不存在恒空、零提示。--json 段（AC3 additive）：
// 命中态 JSON 含 cross_grid_hint 结构化字段（恰一元素 {role,pending}），stderr
// 提示同样在场（提示面向人，不因 --json 消失）。
func TestPollCrossGridHintAC1(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		sendOK(t, addr, root, "--to-role", "executor_A", "--body", fmt.Sprintf("给 executor_A 的第 %d 条", i+1))
	}
	wrongRole := identityArgsAs("executor-A-sess", "executor")

	stdout, stderr := runWantCode(t, "错格 poll", 0, func() int {
		return runPoll(ctx, wrongRole, addr)
	})
	// stdout 零消息体（仅摘要行——pending=0、双位点=登记预置 0）。
	if want := "# pending=0 mailbox_pos=0 dialog_pos=0\n"; stdout != want {
		t.Errorf("错格 poll stdout = %q，期望仅摘要行 %q（零消息体、提示不进 stdout）", stdout, want)
	}
	// stderr 逐字模板（AC1：带 executor_A 与条数 3）。
	if want := crossGridHintLine("executor", "executor_A", 3); stderr != want {
		t.Errorf("错格 poll stderr = %q，期望恰一行 %q", stderr, want)
	}

	// --json 命中态：cross_grid_hint 结构化字段 additive + stderr 提示仍在。
	stdout, stderr = runWantCode(t, "错格 poll --json", 0, func() int {
		return runPoll(ctx, append(wrongRole, "--json"), addr)
	})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &raw); err != nil {
		t.Fatalf("错格 poll --json 非合法 JSON: %v\n%s", err, stdout)
	}
	if len(raw) != 4 {
		t.Errorf("--json 命中态键数 = %d，期望恰四键（三键+cross_grid_hint）: %v", len(raw), raw)
	}
	var hint []struct {
		Role    string `json:"role"`
		Pending int    `json:"pending"`
	}
	if err := json.Unmarshal(raw["cross_grid_hint"], &hint); err != nil {
		t.Fatalf("cross_grid_hint 解析失败: %v（raw: %s）", err, raw["cross_grid_hint"])
	}
	if len(hint) != 1 || hint[0].Role != "executor_A" || hint[0].Pending != 3 {
		t.Errorf("--json cross_grid_hint = %+v，期望恰 [{executor_A 3}]（AC5 最小面）", hint)
	}
	if want := crossGridHintLine("executor", "executor_A", 3); stderr != want {
		t.Errorf("--json 模式 stderr = %q，期望恰一行 %q（提示面向人不因 --json 消失）", stderr, want)
	}
}

// TestPollCrossGridHintZeroNoise AC2 CLI 零噪音两面：①他格无积压——空域 poll
// stderr 零输出；②本格有 direct——即使他格积压也不出提示（stdout 照常渲染）。
func TestPollCrossGridHintZeroNoise(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	executor := identityArgsAs("executor-sess", "executor")

	// ① 空域：无任何消息 → stderr 零输出、stdout 摘要照常。
	stdout, stderr := runWantCode(t, "空域 poll", 0, func() int {
		return runPoll(ctx, executor, addr)
	})
	if stderr != "" {
		t.Errorf("空域 poll stderr = %q，期望空（AC2 零噪音）", stderr)
	}
	if !strings.Contains(stdout, "# pending=0") {
		t.Errorf("空域 poll stdout 缺摘要行:\n%s", stdout)
	}

	// ② 他格积压（executor_A 2 条）+本格 own direct 1 条可见 → 零提示。
	sendOK(t, addr, root, "--to-role", "executor_A", "--body", "给 executor_A 甲")
	sendOK(t, addr, root, "--to-role", "executor_A", "--body", "给 executor_A 乙")
	seqOwn, _ := sendOK(t, addr, root, "--to-role", "executor", "--body", "给 executor 自己")
	stdout, stderr = runWantCode(t, "本格有 direct poll", 0, func() int {
		return runPoll(ctx, executor, addr)
	})
	if stderr != "" {
		t.Errorf("本格有 direct 时 stderr = %q，期望空（AC2：即使他格积压零提示）", stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf("[%d] normal", seqOwn)) || !strings.Contains(stdout, "给 executor 自己") {
		t.Errorf("本格 direct 未出现在 stdout（既有渲染应不变）:\n%s", stdout)
	}
	if !strings.Contains(stdout, "# pending=1") {
		t.Errorf("本格 direct poll 摘要行不符:\n%s", stdout)
	}
}

// TestRenderCrossGridHints 提示渲染纯函数逐字对拍：空态零行、逐格模板行、
// 多格逐行、聚合哨兵元素（服务端 >5 格截断面，role=「等 N 格」）走聚合行。
func TestRenderCrossGridHints(t *testing.T) {
	if got := renderCrossGridHints("executor", nil); got != nil {
		t.Errorf("空态渲染 = %v，期望 nil（AC2 零噪音）", got)
	}
	if got := renderCrossGridHints("executor", []pollCrossGridHint{}); got != nil {
		t.Errorf("空切片渲染 = %v，期望 nil", got)
	}

	// 单格与多格：逐格一行，逐字模板。
	got := renderCrossGridHints("executor", []pollCrossGridHint{
		{Role: "executor_A", Pending: 3},
		{Role: "executor_B", Pending: 1},
	})
	want := []string{
		strings.TrimSuffix(crossGridHintLine("executor", "executor_A", 3), "\n"),
		strings.TrimSuffix(crossGridHintLine("executor", "executor_B", 1), "\n"),
	}
	if len(got) != len(want) {
		t.Fatalf("多格渲染行数 = %d，期望 %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("渲染行[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}

	// 聚合哨兵元素：role=「等 2 格」→ 聚合行（另有 N 格共 M 条）。
	got = renderCrossGridHints("executor", []pollCrossGridHint{
		{Role: "executor_A", Pending: 3},
		{Role: "等 2 格", Pending: 2},
	})
	if len(got) != 2 {
		t.Fatalf("含聚合元素渲染行数 = %d，期望 2: %q", len(got), got)
	}
	wantAgg := "提示：本格（role=executor）无定向消息；同栏目另有 2 格共 2 条未消费定向——若你的身份应为其中一格，检查 --role 是否填错"
	if got[1] != wantAgg {
		t.Errorf("聚合行 = %q，期望 %q", got[1], wantAgg)
	}

	// parseAggregateGrids 边界：非聚合 role 不误判、非数字/非正数不误判；
	// 「等 格」=前缀+后缀同时命中但长度不足（T2 审查修 1：切片越界 panic 形态，
	// 守卫后回归 false 不 panic）。
	for _, tc := range []struct {
		role string
		ok   bool
	}{
		{"executor_A", false}, {"", false}, {"等 X 格", false}, {"等 0 格", false},
		{"等 -1 格", false}, {"等 3 格", true}, {"等 格", false}, {"等", false},
	} {
		if _, ok := parseAggregateGrids(tc.role); ok != tc.ok {
			t.Errorf("parseAggregateGrids(%q) ok = %v，期望 %v", tc.role, ok, tc.ok)
		}
	}
}
