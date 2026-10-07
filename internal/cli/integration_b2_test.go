package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"aiteam/internal/client"
	"aiteam/internal/mirror"
)

// 消息域全链集成验收（B2-8，纯测试收口零实现代码）：in-process serve+临时库
// +CLI 命令函数，六段动线按文件序各一测试函数、段内顺序步骤断言。夹具全量
// 复用既有测试面（startRegTestServer/runWantCode/closedServerAddr、sendOK/
// parseOKLine/readMirrorLines/sendMirrorPath/assertNoMirrorFile/probeMessage）。
//
// 六段与 AC 对照（b2-plan B2-8 节 × b2-spec §1.2 跨批复验行）：
//
//	段 1 TestIntegrationRegisterThenDeliver        AC2.1/AC2.2（登记→收发+未登记拒）
//	段 2 TestIntegrationArchivedRejectHistoryKept  AC1.3/AC2.3（注销拒绝+历史保留）
//	段 3 TestIntegrationConcurrentSendTwenty       AC5.2 端到端（V1a 脚本雏形）
//	段 4 TestIntegrationMirrorLinePerSend          AC13.1（V1b 脚本雏形）
//	段 5 TestIntegrationBlockReceiptFullChain      AC7 全链（block→回执→回告→状态流转）
//	段 6 TestIntegrationRecoverySurfaceWarmup      恢复面预热（B3 status 备数据面）
//
// 本文件同时是「手工冒烟两终端形态」的等价替代物：serve+send/poll/ack/history
// 的两终端手工流程在这里以进程内同链路自动化走全（同一真 server+真 store+CLI
// 命令函数，仅 HTTP 环回从跨进程换为 httptest），见函数级注释逐段说明。

// integArgs 任意身份四参变体构造（identityArgsAs 只换 session/role 恒挂引导
// 栏目；集成动线需要跨项目/栏目域的完整四参形态）。
func integArgs(project, column, session, role string) []string {
	return []string{
		"--project", project,
		"--column", column,
		"--session", session,
		"--role", role,
	}
}

// integRegisterProject 以引导域身份经 B1 project register 命令登记项目，断言
// 退 0（登记命令贯通是段 1 动线的第一环）。
func integRegisterProject(t *testing.T, addr, code, name string) {
	t.Helper()
	runWantCode(t, "project register "+code, 0, func() int {
		return runProjectRegister(context.Background(),
			append(regIdentityArgs(), "--code", code, "--name", name), addr)
	})
}

// integRegisterColumn 以引导域身份经 B1 column register 命令在引导项目下登记
// 栏目，断言退 0。
//
// 已知边界（b1-spec 上报总控的引导死锁，本任务不修）：column register 的父项目
// =身份 --project 一参两用，全新项目下无已登记栏目可挂身份四头（心跳中间件
// 存在性短路 404），首栏目登记命令面不可达；集成动线据此把栏目登记在引导项目
// 下（TestColumnFamily 同款口径），收发域同样锚定引导项目。
func integRegisterColumn(t *testing.T, addr, code string) {
	t.Helper()
	runWantCode(t, "column register "+code, 0, func() int {
		return runColumnRegister(context.Background(),
			append(regIdentityArgs(), "--project", regAuthProject, "--code", code,
				"--name", "集成栏目"+code), addr)
	})
}

// integHistory 以任意身份域 GET #13 历史查询（send_test.go historyProbe 的跨域
// 泛化版：project 参数化并支持追加 query 键——其固定引导项目且无 limit 面）。
// 返回结构化行供计数/seq 唯一性断言；带四头的 GET 同时经心跳中间件 upsert 探针
// 会话（不写 messages，零杂音）。
func integHistory(t *testing.T, addr, project, column, session, role string, extra url.Values) []probeMessage {
	t.Helper()
	c := client.NewClient(addr, "", client.Identity{
		Project: project, Column: column, Session: session, Role: role,
	})
	q := url.Values{}
	q.Set("project", project)
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	var resp struct {
		Messages []probeMessage `json:"messages"`
	}
	if err := c.Do(context.Background(), http.MethodGet, "/api/v1/messages?"+q.Encode(), nil, &resp); err != nil {
		t.Fatalf("GET #13 历史查询失败: %v", err)
	}
	return resp.Messages
}

// assertContains 断言 s 含 wants 全部片段（集成动线高频断言形状收敛，失败附
// 全文便于定位）。
func assertContains(t *testing.T, stage, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("%s 输出缺 %q\n全文: %q", stage, w, s)
		}
	}
}

// TestIntegrationRegisterThenDeliver 段 1（登记→收发，AC2.1/AC2.2）：
// ① B1 登记命令贯通——project register 新项目+column register 引导项目下新栏目；
// ② 登记后定向收发达通（AC2.1）：新栏目 direct send → 目标角色 poll 拉到（输出
// 逐字对拍=五要素+pending/位点面全验）；③ 向未登记栏目 send → 退 4 且错误信息
// 指明 column_not_found（AC2.2 命令面端到端），服务端拒绝零触碰镜像（§8.3 时序
// 在集成面复锁）。
func TestIntegrationRegisterThenDeliver(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	const newCol = "c9"

	// ① 登记环：新项目 proj-it（身份挂引导域）+引导项目下新栏目 c9。
	integRegisterProject(t, addr, "proj-it", "集成项目")
	integRegisterColumn(t, addr, newCol)

	// ② AC2.1 定向收发：controller-A 在 c9 direct send → executor-B 同栏目 poll
	//    拉到。c9 登记时位点预置=当时 MAX(seq)=0（InitColumnPositions），故 poll
	//    期望 pending=1、双位点 0——逐字对拍照 TestChatRoundtrip 样板。
	sender := integArgs(regAuthProject, newCol, "controller-A", "controller")
	body := "集成第一条定向消息"
	stdout, _ := runWantCode(t, "direct send", 0, func() int {
		return runSend(ctx, append(sender,
			"--to-role", "executor", "--body", body, "--mirror-root", root), addr)
	})
	seq, ts := parseOKLine(t, stdout)
	executor := integArgs(regAuthProject, newCol, "executor-B", "executor")
	stdout, _ = runWantCode(t, "目标角色 poll", 0, func() int {
		return runPoll(ctx, executor, addr)
	})
	want := fmt.Sprintf("[%d] normal  %s  controller-A@%s(direct)\n      %s\n"+
		"# pending=1 mailbox_pos=0 dialog_pos=0\n", seq, ts, newCol, body)
	if stdout != want {
		t.Errorf("AC2.1 目标角色 poll 逐字对拍失败\n得到: %q\n期望: %q", stdout, want)
	}

	// ③ AC2.2 未登记栏目拒绝：身份栏目指向未登记 code → 心跳中间件 404
	//    column_not_found → 退 4 且 stderr 指明；镜像零触碰（先服务端后镜像）。
	ghostRoot := t.TempDir()
	_, stderr := runWantCode(t, "未登记栏目 send", 4, func() int {
		return runSend(ctx, append(integArgs(regAuthProject, "ghost-col", "controller-A", "controller"),
			"--to-role", "executor", "--body", "x", "--mirror-root", ghostRoot), addr)
	})
	assertContains(t, "AC2.2 未登记栏目 send stderr", stderr, "column_not_found")
	assertNoMirrorFile(t, sendMirrorPath(ghostRoot, regAuthProject, "ghost-col"))
}

// TestIntegrationArchivedRejectHistoryKept 段 2（注销拒绝+历史保留，AC1.3/AC2.3）：
// 栏目级与项目级 archived 的 send/poll 双端点拒绝+注销后 history 全量可查。
// 顺序裁量（任务书字面为先项目后栏目）：项目归档会连坐全部栏目，栏目级
// column_archived 的独立验证必须在项目归档前完成，故先栏目后项目——项目归档
// 环节用的栏目 c1 未归档，命中 project_archived 恰好验证连坐口径本身。
func TestIntegrationArchivedRejectHistoryKept(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	const doomedCol = "c8"

	// 前置：登记第二栏目 c8 并种一条消息（历史保留的验证样本）；c1 保持零操作。
	integRegisterColumn(t, addr, doomedCol)
	seedBody := "注销前落库的历史消息"
	stdout, _ := runWantCode(t, "c8 种消息", 0, func() int {
		return runSend(ctx, append(integArgs(regAuthProject, doomedCol, "controller-A", "controller"),
			"--to-role", "executor", "--body", seedBody, "--mirror-root", root), addr)
	})
	seedSeq, _ := parseOKLine(t, stdout)

	// ① AC2.3 栏目级：c8 archive 后（项目仍 active）send/poll 双双退 4 且错误
	//    信息指明 column_archived。
	runWantCode(t, "column archive c8", 0, func() int {
		return runColumnArchive(ctx, append(regIdentityArgs(),
			"--project", regAuthProject, "--code", doomedCol), addr)
	})
	doomed := integArgs(regAuthProject, doomedCol, "controller-A", "controller")
	_, stderr := runWantCode(t, "archived 栏目 send", 4, func() int {
		return runSend(ctx, append(doomed,
			"--to-role", "executor", "--body", "x", "--mirror-root", root), addr)
	})
	assertContains(t, "AC2.3 栏目 archived send stderr", stderr, "column_archived")
	_, stderr = runWantCode(t, "archived 栏目 poll", 4, func() int {
		return runPoll(ctx, doomed, addr)
	})
	assertContains(t, "AC2.3 栏目 archived poll stderr", stderr, "column_archived")

	// ② AC1.3/AC2.3 项目级：management 归档后，未归档栏目 c1 被项目连坐——
	//    send/poll 退 4 且错误信息指明 project_archived（项目先于栏目判）。
	runWantCode(t, "project archive", 0, func() int {
		return runProjectArchive(ctx, append(regIdentityArgs(), "--code", regAuthProject), addr)
	})
	kept := integArgs(regAuthProject, regAuthColumn, "controller-A", "controller")
	_, stderr = runWantCode(t, "archived 项目 send", 4, func() int {
		return runSend(ctx, append(kept,
			"--to-role", "executor", "--body", "x", "--mirror-root", root), addr)
	})
	assertContains(t, "AC1.3 项目 archived send stderr", stderr, "project_archived")
	_, stderr = runWantCode(t, "archived 项目 poll", 4, func() int {
		return runPoll(ctx, kept, addr)
	})
	assertContains(t, "AC1.3 项目 archived poll stderr", stderr, "project_archived")

	// ③ AC1.3 历史保留：#13 放行（B2-T2 历史查询不拒）——用已注销栏目 c8 的
	//    域身份查 history（history 的 --project/--column 兼作域过滤，查询域=消息
	//    域）：archived 域身份经中间件放行+handler 无域校验双重口径一次锁定，
	//    注销前消息的 seq（位点系数据）与正文俱在。
	stdout, _ = runWantCode(t, "注销后 history", 0, func() int {
		return runHistory(ctx, doomed, addr)
	})
	assertContains(t, "AC1.3 注销后 history", stdout,
		fmt.Sprintf("[%d]", seedSeq), seedBody)
	msgs := integHistory(t, addr, regAuthProject, doomedCol, "it-probe", "controller", nil)
	if len(msgs) != 1 || msgs[0].Seq != seedSeq || msgs[0].Body != seedBody {
		t.Errorf("注销后 #13 数据面 = %+v，期望恰含 seq=%d 的历史消息", msgs, seedSeq)
	}
}

// TestIntegrationConcurrentSendTwenty 段 3（AC5.2 端到端，V1a 脚本雏形）：
// 20 goroutine 并行调 CLI send 命令函数（同栏目、会话名唯一、role 轮换混合），
// 两轮循环压测——判据三件套：全 0 退出、seq 两两不同、history COUNT=20×轮次。
// goroutine 内不触 *testing.T（非并发安全面为退出码切片，结果主协程断言）。
// 目标角色用无主的 executor_duty（2026-10-03 增补令 agent_conflict 拦截后，
// direct 目标格子 ≥2 活会话即 409 退 4——worker 发送方已占 executor 等格子，
// 定向到无主格子使压测聚焦 seq 取号与镜像写面，不受 role 规约闸拦截）。
func TestIntegrationConcurrentSendTwenty(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	const (
		n      = 20 // 单轮并发数（AC5.2 冻结口径）
		rounds = 2  // V1a 雏形多轮：重复压测下 seq 仍全局唯一
	)

	// 并发段 stdout/stderr 静默（DevNull 设备）：captureStdoutStderr 的全局
	// os.Stdout 替换是串行假设，20 goroutine 无法各自捕获；退出码即 V1a 判定面
	// （与脚本判 rc==0 同口径），seq 面改从 #13 history 结构化读取。os.Stdout
	// 是 *os.File 具体类型，io.Discard 进不去，用 os.DevNull 打开的 *os.File。
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", os.DevNull, err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devNull, devNull
	t.Cleanup(func() {
		os.Stdout, os.Stderr = origOut, origErr
		_ = devNull.Close()
	})

	for round := 1; round <= rounds; round++ {
		codes := make([]int, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				roles := []string{"executor", "executor_reviewer", "executor_tester"} // role 不枚举硬校验（§3.2 表 3 注）
				codes[i] = runSend(ctx,
					append(integArgs(regAuthProject, regAuthColumn,
						fmt.Sprintf("it-r%d-worker-%02d", round, i), roles[i%len(roles)]),
						"--to-role", "executor_duty", // 无主格子：agent_conflict 拦截适配（见函数头注）
						"--body", fmt.Sprintf("并发轮 %d 条目 %02d", round, i),
						"--mirror-root", root), addr)
			}()
		}
		wg.Wait()

		// 判据一：全 0 退出（含镜像双写成功——任一条镜像失败即退 5 在此暴露）。
		for i, c := range codes {
			if c != 0 {
				t.Errorf("轮 %d 并发 send #%02d 退出码 = %d，期望 0（AC5.2）", round, i, c)
			}
		}

		// 判据二/三：入库数=20×轮次、seq 两两不同（V1a 多轮=持续压测 seq 唯一性）。
		msgs := integHistory(t, addr, regAuthProject, regAuthColumn, "it-probe", "controller",
			url.Values{"limit": {"500"}})
		if len(msgs) != n*round {
			t.Fatalf("轮 %d 后 history COUNT = %d，期望 %d（AC5.2 入库数）", round, len(msgs), n*round)
		}
		seqs := make([]int64, 0, len(msgs))
		for _, m := range msgs {
			seqs = append(seqs, m.Seq)
		}
		slices.Sort(seqs)
		for i := 1; i < len(seqs); i++ {
			if seqs[i] == seqs[i-1] {
				t.Errorf("轮 %d seq 重复: %d（AC5.2 全局唯一）", round, seqs[i])
			}
		}
	}
}

// TestIntegrationMirrorLinePerSend 段 4（V1b 脚本雏形，AC13.1 端到端）：
// 串行 20 条 send，每条返回 0 后立即读镜像文件逐行比对——send 成功数=镜像新增
// 行数、新增行含该 seq 且六字段形状正确；头注释恒在首行（行数账排除重复写头）。
func TestIntegrationMirrorLinePerSend(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()
	mirrorFile := sendMirrorPath(root, regAuthProject, regAuthColumn)
	const n = 20

	// 首条前镜像不存在（AC13.1 的 0 基线：返回 0 ⇔ 新增行，先证「无 send 无行」）。
	assertNoMirrorFile(t, mirrorFile)

	for i := range n {
		body := fmt.Sprintf("V1b 逐行抽检 %02d", i)
		stdout, _ := runWantCode(t, fmt.Sprintf("V1b send #%02d", i), 0, func() int {
			return runSend(ctx, append(regIdentityArgs(),
				"--to-role", "executor", "--body", body, "--mirror-root", root), addr)
		})
		seq, _ := parseOKLine(t, stdout)

		// send 成功数=镜像新增行数：第 i+1 条后恰头注释+（i+1）行（AC13.1 主判据）。
		lines := readMirrorLines(t, mirrorFile)
		if len(lines) != i+2 {
			t.Fatalf("第 %d 条后镜像应 %d 行（头注释+%d 条），实际 %d 行: %q",
				i+1, i+2, i+1, len(lines), lines)
		}
		if lines[0] != mirror.HeaderComment {
			t.Errorf("第 %d 条后首行 = %q，期望恒为头注释（首建头只写一次）", i+1, lines[0])
		}
		// 新增行（尾行）逐字段比对：行含该 seq（AC13.1 判据二）+body 单行化原样。
		parts := strings.Split(lines[len(lines)-1], " | ")
		if len(parts) != 6 {
			t.Fatalf("第 %d 条镜像尾行应拆 6 字段，实际 %q", i+1, lines[len(lines)-1])
		}
		if parts[0] != strconv.FormatInt(seq, 10) {
			t.Errorf("第 %d 条镜像行 seq 字段 = %q，期望 %d（AC13.1 行含该 seq）", i+1, parts[0], seq)
		}
		if parts[5] != body {
			t.Errorf("第 %d 条镜像行 body 字段 = %q，期望 %q", i+1, parts[5], body)
		}
	}
}

// TestIntegrationBlockReceiptFullChain 段 5（AC7 全链）：block send → 回执前
// history 状态列=待回执（AC7.2）→ 对方 ack --receipt 回执（位点推进+回执一次
// 调用）→ 回告到达原发送方 poll 对话分支（kind=receipt，§2.3 回告格式）→ 回执
// 后 history 状态流转=已回执 by/at（AC7.3）。
func TestIntegrationBlockReceiptFullChain(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()

	sender := integArgs(regAuthProject, regAuthColumn, "controller-A", "controller")
	executor := integArgs(regAuthProject, regAuthColumn, "executor-B", "executor")
	label := "controller-A@" + regAuthColumn

	// ① block send。
	body := "需要阻断回执的指令"
	stdout, _ := runWantCode(t, "block send", 0, func() int {
		return runSend(ctx, append(sender,
			"--to-role", "executor", "--level", "block", "--body", body,
			"--mirror-root", root), addr)
	})
	seq, ts := parseOKLine(t, stdout)

	// ② AC7.2：回执前 history --level block 状态列=待回执。
	stdout, _ = runWantCode(t, "回执前 history", 0, func() int {
		return runHistory(ctx, append(sender, "--level", "block"), addr)
	})
	assertContains(t, "AC7.2 回执前 history", stdout,
		fmt.Sprintf("[%d] block direct %s 待回执 %s %s", seq, label, ts, body))

	// ③ 对方 ack --receipt：位点推进与回执并存一次调用，输出 §4.2 样例形状。
	stdout, _ = runWantCode(t, "ack --receipt", 0, func() int {
		return runAck(ctx, append(executor, "--receipt", strconv.FormatInt(seq, 10)), addr)
	})
	if want := fmt.Sprintf("OK position=%d receipts=[%d]\n", seq, seq); stdout != want {
		t.Errorf("ack --receipt 输出 = %q，期望 %q", stdout, want)
	}

	// ④ 回告到达原发送方（poll 对话分支）：kind=receipt 标注+§2.3 回告正文+
	//    pending=1（回告是 controller-A 唯一可见消息——自发 direct 不回流）。
	stdout, _ = runWantCode(t, "原发送方 poll 回告", 0, func() int {
		return runPoll(ctx, sender, addr)
	})
	assertContains(t, "AC7 回告 poll", stdout,
		fmt.Sprintf("receipt: seq=%d by executor-B at ", seq),
		"(receipt)",
		"# pending=1")

	// ⑤ AC7.3：回执后 history 状态流转=已回执 by=回执方 at=服务端时间，且不再
	//    有待回执。
	stdout, _ = runWantCode(t, "回执后 history", 0, func() int {
		return runHistory(ctx, append(sender, "--level", "block"), addr)
	})
	assertContains(t, "AC7.3 回执后 history", stdout,
		fmt.Sprintf("[%d]", seq), "已回执 by=executor-B at=")
	if strings.Contains(stdout, "待回执") {
		t.Errorf("回执后 history 仍含待回执:\n%s", stdout)
	}
}

// TestIntegrationRecoverySurfaceWarmup 段 6（恢复面预热）：混合消息面（direct
// block/direct normal/bus important）→ 消费方一键 ack 位点推进到最大可见 →
// poll 空+history 全量可查——为 B3 status 恢复现场备齐数据面（位点真值、待消
// 费清零、审计历史三要素在本段走通 CLI 全链）。
func TestIntegrationRecoverySurfaceWarmup(t *testing.T) {
	addr := startRegTestServer(t)
	root := t.TempDir()
	ctx := context.Background()

	sender := integArgs(regAuthProject, regAuthColumn, "controller-A", "controller")
	executor := integArgs(regAuthProject, regAuthColumn, "executor-B", "executor")

	// 种混合面三条（seq 严格递增；发送方走 sendOK 既有夹具=引导域默认会话，
	// 本段断言不依赖发送方会话名）。
	seqB, _ := sendOK(t, addr, root, "--to-role", "executor", "--level", "block",
		"--body", "恢复面阻断项")
	seqN, _ := sendOK(t, addr, root, "--to-role", "executor", "--level", "normal",
		"--body", "恢复面普通项")
	seqU, _ := sendOK(t, addr, root, "--bus", "--level", "important",
		"--body", "恢复面总线项")
	if !(seqB < seqN && seqN < seqU) {
		t.Fatalf("种消息 seq 应递增: %d/%d/%d", seqB, seqN, seqU)
	}

	// 消费方一键 ack：省略 seq=推进到各自最大可见。executor-B 可见两条 direct
	// （bus 仅 controller 可见，AC6.2），信箱最大可见=seqN。
	stdout, _ := runWantCode(t, "一键 ack", 0, func() int {
		return runAck(ctx, executor, addr)
	})
	if want := fmt.Sprintf("OK position=%d\n", seqN); stdout != want {
		t.Errorf("一键 ack 输出 = %q，期望 %q（信箱位点=executor 最大可见）", stdout, want)
	}

	// 恢复面要素一：位点已到最新 → poll 空（pending=0、双位点落定）。
	stdout, _ = runWantCode(t, "ack 后 poll", 0, func() int {
		return runPoll(ctx, executor, addr)
	})
	if want := fmt.Sprintf("# pending=0 mailbox_pos=%d dialog_pos=0\n", seqN); stdout != want {
		t.Errorf("恢复面 poll = %q，期望 %q（位点推进后待消费清零）", stdout, want)
	}

	// 恢复面要素二：history 全量可查（注销/消费皆不损历史——B3 status 的恢复
	// 现场以本查询为数据源）。bus 行 sender 恒 controller-A（同一发送方）。
	stdout, _ = runWantCode(t, "恢复面 history 全量", 0, func() int {
		return runHistory(ctx, append(sender, "--order", "asc", "--limit", "500"), addr)
	})
	assertContains(t, "恢复面 history", stdout,
		fmt.Sprintf("[%d] block direct", seqB),
		fmt.Sprintf("[%d] normal direct", seqN),
		fmt.Sprintf("[%d] important bus", seqU))
}
