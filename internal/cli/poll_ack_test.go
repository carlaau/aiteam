package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"aiteam/internal/config"
	"aiteam/internal/server"
	"aiteam/internal/store"
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
	// b2-T1（M-33）口径更新：本会话身份=controller，bus 为自发——poll 谓词已
	// 排除发送方自发 bus（不回投自己信箱），poll 只拉到 direct 一条；bus 发送
	// 保留在场（自发回声场景断言按新语义更新，spec/plan 声明的豁免面）。
	seq1, ts1 := sendOK(t, addr, root, "--to-role", regAuthRole, "--level", "block",
		"--body", "第一行\n第二行")
	seq2, _ := sendOK(t, addr, root, "--bus", "--level", "important", "--body", "bus 正文")

	stdout, stderr := runWantCode(t, "poll", 0, func() int {
		return runPoll(ctx, regIdentityArgs(), addr)
	})
	if stderr != "" {
		t.Errorf("poll 成功不应有 stderr 输出，实际 %q", stderr)
	}
	label := regAuthSession + "@" + regAuthColumn
	want := fmt.Sprintf(
		"[%d] block  %s  %s(direct)\n      第一行\n      第二行\n"+
			"# pending=1 mailbox_pos=0 dialog_pos=0\n",
		seq1, ts1, label)
	if stdout != want {
		t.Errorf("poll 输出逐字对拍失败\n得到: %q\n期望: %q", stdout, want)
	}
	// 自发 bus 不回投负断言（b2-T1 排除面）：输出不含 bus 行与该 seq。
	if strings.Contains(stdout, "(bus)") || strings.Contains(stdout, fmt.Sprintf("[%d]", seq2)) {
		t.Errorf("自发 bus(seq=%d) 不应回投发送方自己信箱:\n%s", seq2, stdout)
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
	if len(parsed.Messages) != 1 {
		t.Fatalf("poll --json messages 应 1 条（自发 bus 不回投，b2-T1），实际 %d", len(parsed.Messages))
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

// ---- b2-T3 poll --ack 一键消费（b2-spec §二.R1/AC1，M-32）----
//
// R1 口径四形态：①拉到信箱消息即推进到本次最大信箱 seq（再 poll 零重复）
// ②空信箱零动作零输出差异（与不带 --ack 逐字节一致，文本/--json 两形态）
// ③--json 尾注 acked_to（拉到=推进落定位点数值；空箱=键缺省——省略口径）
// ④不带 --ack 默认只读回归锚=既有 TestPollOutput/TestPollIdempotentReread
// （零改动零复述）。推进目标只取信箱域（direct/bus）最大 seq——chat/receipt
// 归对话流不纳入（ack --seq 语义推的就是信箱位点，§3.6 谓词三分支）。

// TestPollAckAdvance AC1① 主链：send direct×2 → poll --ack 展示照常（输出与
// 只读 poll 同形零增行——文本形态纯副作用）→ 再 poll 零重复（空箱+双位点推进
// 到最大 seq）。尾段 bus 补面：另一 controller 会话拉到 bus（T1 排除只对发送
// 方本人）同样纳入推进目标。
func TestPollAckAdvance(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()

	seq1, ts1 := sendOK(t, addr, root, "--to-role", regAuthRole, "--body", "甲")
	seq2, ts2 := sendOK(t, addr, root, "--to-role", regAuthRole, "--body", "乙")

	stdout, stderr := runWantCode(t, "poll --ack", 0, func() int {
		return runPoll(ctx, append(regIdentityArgs(), "--ack"), addr)
	})
	if stderr != "" {
		t.Errorf("poll --ack 成功不应有 stderr 输出，实际 %q", stderr)
	}
	// 展示照常且零增行：与不带 --ack 的只读输出同形（GET 时点双位点 0）——
	// --ack 在文本形态是纯副作用，推进结果只经后续位点与 --json acked_to 呈现。
	label := regAuthSession + "@" + regAuthColumn
	want := fmt.Sprintf(
		"[%d] normal  %s  %s(direct)\n      甲\n"+
			"[%d] normal  %s  %s(direct)\n      乙\n"+
			"# pending=2 mailbox_pos=0 dialog_pos=0\n",
		seq1, ts1, label, seq2, ts2, label)
	if stdout != want {
		t.Errorf("poll --ack 文本输出应与只读 poll 同形（零增行）\n得到: %q\n期望: %q", stdout, want)
	}

	// AC1① 双断言链后半：再 poll 零重复——空箱+双位点=seq2（#11 显式 seq 双
	// 维度同推到该值，既有语义）。
	stdout, _ = runWantCode(t, "ack 后再 poll", 0, func() int {
		return runPoll(ctx, regIdentityArgs(), addr)
	})
	if want := fmt.Sprintf("# pending=0 mailbox_pos=%d dialog_pos=%d\n", seq2, seq2); stdout != want {
		t.Errorf("poll --ack 后再 poll = %q，期望零重复且双位点=%d（AC1①）", stdout, seq2)
	}

	// bus 同属信箱域：另一 controller 会话 poll --ack 拉到 bus（自发回声排除
	// 只挡发送方本人，他人广播照投）即推进到位点=bus seq。
	seqBus, _ := sendOK(t, addr, root, "--bus", "--body", "总线消息")
	watcher := identityArgsAs("controller-B", "controller")
	stdout, _ = runWantCode(t, "controller-B poll --ack bus", 0, func() int {
		return runPoll(ctx, append(watcher, "--ack"), addr)
	})
	if !strings.Contains(stdout, fmt.Sprintf("[%d]", seqBus)) || !strings.Contains(stdout, "(bus)") {
		t.Errorf("controller-B poll --ack 应展示 bus 消息:\n%s", stdout)
	}
	stdout, _ = runWantCode(t, "bus 推进后再 poll", 0, func() int {
		return runPoll(ctx, watcher, addr)
	})
	if want := fmt.Sprintf("# pending=0 mailbox_pos=%d dialog_pos=%d\n", seqBus, seqBus); stdout != want {
		t.Errorf("bus --ack 后再 poll = %q，期望零重复且双位点=%d", stdout, seqBus)
	}
}

// TestPollAckMailboxDomainOnly 口径守门：①信箱空（仅 chat 在场）--ack 零动作
// ——与只读 poll 输出逐字节一致；②chat seq 高于信箱最大时只推到信箱最大，
// chat 保持未消费（若实现误取全消息最大 seq 将连 chat 一并吞掉，此处即红）。
func TestPollAckMailboxDomainOnly(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	executor := identityArgsAs("executor-B", "executor")

	// 目标会话预注册（TestChatRoundtrip 同款手法：--to-session 要求会话已在，
	// 以 executor-B 身份 GET #13 upsert，先于任何 chat 种消息）。
	historyProbe(t, addr, "executor-B", "executor", "")

	// ① 仅 chat（信箱空）：--ack 零动作——两次均只读 GET 状态不变，输出应逐
	// 字节一致（stdout+stderr），chat 保持可见。
	chat1, _ := sendOK(t, addr, root, "--to-session", "executor-B", "--body", "仅对话")
	plain, plainErr := runWantCode(t, "仅 chat 只读 poll", 0, func() int {
		return runPoll(ctx, executor, addr)
	})
	acked, ackedErr := runWantCode(t, "仅 chat poll --ack", 0, func() int {
		return runPoll(ctx, append(executor, "--ack"), addr)
	})
	if plain != acked || plainErr != ackedErr {
		t.Errorf("空信箱（仅 chat）--ack 与不带 --ack 输出应逐字节一致（AC1②）\n"+
			"stdout: %q vs %q\nstderr: %q vs %q", plain, acked, plainErr, ackedErr)
	}
	if !strings.Contains(acked, fmt.Sprintf("[%d] normal", chat1)) {
		t.Errorf("仅 chat 场景 chat 应保持可见（零动作证据）:\n%s", acked)
	}

	// ② chat 高于信箱最大：direct×2 + chat（seq 最高）→ poll --ack 只推到
	// direct 最大 seq——再 poll 仍拉到该 chat（对话位点=信箱最大，chat 越位未消费）。
	sendOK(t, addr, root, "--to-role", "executor", "--body", "信箱甲")
	seqB, _ := sendOK(t, addr, root, "--to-role", "executor", "--body", "信箱乙")
	chatHi, tsHi := sendOK(t, addr, root, "--to-session", "executor-B", "--body", "高于信箱的对话")
	runWantCode(t, "混合 poll --ack", 0, func() int {
		return runPoll(ctx, append(executor, "--ack"), addr)
	})
	stdout, _ := runWantCode(t, "推进后再 poll", 0, func() int {
		return runPoll(ctx, executor, addr)
	})
	want := fmt.Sprintf("[%d] normal  %s  %s(chat)\n      高于信箱的对话\n"+
		"# pending=1 mailbox_pos=%d dialog_pos=%d\n",
		chatHi, tsHi, regAuthSession+"@"+regAuthColumn, seqB, seqB)
	if stdout != want {
		t.Errorf("chat 高于信箱最大时应只推到信箱最大（chat 保持可见）\n得到: %q\n期望: %q", stdout, want)
	}
}

// TestPollAckJSON AC1③ --json 尾注：拉到=恰四键（pollData 三键+acked_to=推进
// 落定值），前三键保持 GET 时点原值（尾注非改写）；空信箱（刚消费殆尽）--ack
// --json 与只读 --json 逐字节一致——acked_to 键缺省=省略口径（spec「未拉到为
// null/省略」二择一取省略：与 cross_grid_hint 的 additive/omitempty 口径一致，
// 且空箱两形态均逐字节同形）。
func TestPollAckJSON(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	executor := identityArgsAs("executor-B", "executor")

	seq1, _ := sendOK(t, addr, root, "--to-role", "executor", "--body", "甲")
	seq2, _ := sendOK(t, addr, root, "--to-role", "executor", "--body", "乙")

	stdout, _ := runWantCode(t, "poll --ack --json", 0, func() int {
		return runPoll(ctx, append(executor, "--ack", "--json"), addr)
	})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &raw); err != nil {
		t.Fatalf("poll --ack --json 非合法 JSON: %v\n%s", err, stdout)
	}
	if len(raw) != 4 {
		t.Errorf("--ack --json 键数 = %d，期望恰四键（pollData 三键+acked_to 尾键）: %v", len(raw), raw)
	}
	if got := strings.TrimSpace(string(raw["acked_to"])); got != strconv.FormatInt(seq2, 10) {
		t.Errorf("acked_to = %s，期望 %d（本次最大信箱 seq=推进落定值）", got, seq2)
	}
	// 前三键保持 GET 时点原值（acked_to 是尾注非改写）：双位点=推进前 0/0、
	// messages=本次拉到的 2 条（seq1/seq2 升序）。
	var base struct {
		MailboxPosition int64 `json:"mailbox_position"`
		DialogPosition  int64 `json:"dialog_position"`
		Messages        []struct {
			Seq int64 `json:"seq"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &base); err != nil {
		t.Fatalf("poll --ack --json 解析失败: %v", err)
	}
	if base.MailboxPosition != 0 || base.DialogPosition != 0 ||
		len(base.Messages) != 2 || base.Messages[0].Seq != seq1 || base.Messages[1].Seq != seq2 {
		t.Errorf("--ack --json 前三键应保持 GET 时点值（双位点 0/0、messages=[%d %d]），实际 %+v",
			seq1, seq2, base)
	}

	// 空信箱（上一段已消费殆尽）：--ack --json 与只读 --json 逐字节一致
	// （acked_to 键缺省，AC1② 的 --json 形态）。
	plainJSON, plainJErr := runWantCode(t, "空箱只读 --json", 0, func() int {
		return runPoll(ctx, append(executor, "--json"), addr)
	})
	ackJSON, ackJErr := runWantCode(t, "空箱 --ack --json", 0, func() int {
		return runPoll(ctx, append(executor, "--ack", "--json"), addr)
	})
	if plainJSON != ackJSON || plainJErr != ackJErr {
		t.Errorf("空信箱 --ack --json 与只读 --json 应逐字节一致\nstdout: %q vs %q\nstderr: %q vs %q",
			plainJSON, ackJSON, plainJErr, ackJErr)
	}
}

// TestPollAckEmptyMailboxIdentity AC1② 真空信箱（零消息）：poll --ack 与不带
// --ack 输出逐字节一致（文本+--json 两形态、stdout+stderr 双通道）——--ack 纯
// opt-in，默认只读面（AC1④）的镜像回归（只读侧锚=既有 TestPollOutput/
// TestPollIdempotentReread，零改动零复述）。
func TestPollAckEmptyMailboxIdentity(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()
	executor := identityArgsAs("executor-B", "executor")

	textPlain, textPlainErr := runWantCode(t, "空箱只读 poll", 0, func() int {
		return runPoll(ctx, executor, addr)
	})
	textAck, textAckErr := runWantCode(t, "空箱 poll --ack", 0, func() int {
		return runPoll(ctx, append(executor, "--ack"), addr)
	})
	if textPlain != textAck || textPlainErr != textAckErr {
		t.Errorf("空信箱 --ack 与不带 --ack 文本输出应逐字节一致\n"+
			"stdout: %q vs %q\nstderr: %q vs %q", textPlain, textAck, textPlainErr, textAckErr)
	}

	jsonPlain, jsonPlainErr := runWantCode(t, "空箱只读 poll --json", 0, func() int {
		return runPoll(ctx, append(executor, "--json"), addr)
	})
	jsonAck, jsonAckErr := runWantCode(t, "空箱 poll --ack --json", 0, func() int {
		return runPoll(ctx, append(executor, "--ack", "--json"), addr)
	})
	if jsonPlain != jsonAck || jsonPlainErr != jsonAckErr {
		t.Errorf("空信箱 --ack 与不带 --ack --json 输出应逐字节一致\n"+
			"stdout: %q vs %q\nstderr: %q vs %q", jsonPlain, jsonAck, jsonPlainErr, jsonAckErr)
	}
}

// TestPollAckPostFailureInjected 质量审加固（b2-T3 审查建议采纳）：GET #10 成功
// 但 POST #11 失败 → 非零退出且不展示——poll.go「推进先于渲染、失败零展示」
// 保证此前仅靠代码顺序阅读核实，此处注入失败直接测试。夹具=真 store+真 server
// 外包自包装 handler（TestHistoryLimitZeroPassthrough 同款手法+真服务代理）：
// POST /api/v1/acks 命中开关时注入 500 {error} 包裹（client.APIError → 退 4），
// 其余请求原样透传真服务——故失败后可直接验真库位点状态与修复重跑恢复面。
// failAcks 走 atomic（handler 在服务端 goroutine 读、测试 goroutine 写）。
func TestPollAckPostFailureInjected(t *testing.T) {
	// 本地装配真服务（seed 引导域同 startRegTestServer 骨架）——夹具需持有
	// handler 供外层包装注入，故不走 startRegTestServer（其只回地址）。
	st, err := store.Open(filepath.Join(t.TempDir(), "poll-ack-fail.db"))
	if err != nil {
		t.Fatalf("打开临时 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateProject(store.Project{Code: regAuthProject, Name: "引导项目"}); err != nil {
		t.Fatalf("seed 项目失败: %v", err)
	}
	if _, _, err := st.CreateColumnWithAudit(regAuthProject, store.Column{Code: regAuthColumn, Name: "引导栏目"},
		store.AuditEntry{Action: store.AuditColumnRegister, Detail: "{}"}); err != nil {
		t.Fatalf("seed 栏目失败: %v", err)
	}
	real := server.NewServer(st, config.Default(), server.Version)
	var failAcks atomic.Bool
	failAcks.Store(true)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failAcks.Load() && r.Method == http.MethodPost && r.URL.Path == "/api/v1/acks" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"注入的 ack 失败"}}`))
			return
		}
		real.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	ctx := context.Background()
	root := t.TempDir()

	seq1, _ := sendOK(t, ts.URL, root, "--to-role", regAuthRole, "--body", "甲")
	seq2, _ := sendOK(t, ts.URL, root, "--to-role", regAuthRole, "--body", "乙")

	// poll --ack：GET 成功、ack POST 注入失败 → 退 4（APIError）、stdout 零输出
	// （不展示）、stderr 含 ERROR 错误行。
	stdout, stderr := runWantCode(t, "ack 注入失败 poll --ack", 4, func() int {
		return runPoll(ctx, append(regIdentityArgs(), "--ack"), ts.URL)
	})
	if stdout != "" {
		t.Errorf("ack 失败时不应有任何 stdout 展示（推进先于渲染的失败零展示保证），实际 %q", stdout)
	}
	if !strings.Contains(stderr, "ERROR") || !strings.Contains(stderr, "注入的 ack 失败") {
		t.Errorf("ack 失败 stderr = %q，期望含 ERROR 错误行与注入错误信息", stderr)
	}

	// 位点未推进（零副作用）：只读 poll 消息仍全部在场、双位点 0。
	stdout, _ = runWantCode(t, "失败后只读 poll", 0, func() int {
		return runPoll(ctx, regIdentityArgs(), ts.URL)
	})
	if !strings.Contains(stdout, fmt.Sprintf("[%d] normal", seq1)) || !strings.Contains(stdout, fmt.Sprintf("[%d] normal", seq2)) {
		t.Errorf("ack 失败后消息应全部在场（位点零推进）:\n%s", stdout)
	}
	if !strings.Contains(stdout, "# pending=2 mailbox_pos=0 dialog_pos=0") {
		t.Errorf("ack 失败后双位点应仍为 0:\n%s", stdout)
	}

	// 修好夹具重跑：poll --ack 成功（退 0）→ 再 poll 零重复（恢复面，GET 只读
	// 重跑无损的实证）。
	failAcks.Store(false)
	runWantCode(t, "修复后 poll --ack", 0, func() int {
		return runPoll(ctx, append(regIdentityArgs(), "--ack"), ts.URL)
	})
	stdout, _ = runWantCode(t, "恢复后再 poll", 0, func() int {
		return runPoll(ctx, regIdentityArgs(), ts.URL)
	})
	if want := fmt.Sprintf("# pending=0 mailbox_pos=%d dialog_pos=%d\n", seq2, seq2); stdout != want {
		t.Errorf("恢复后 poll = %q，期望零重复且双位点=%d", stdout, seq2)
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
