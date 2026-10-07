package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// 本文件为五核心 ack 命令实现（B2-7，技术设计 §4.2/§2.2 #11/#12、b2-spec §二
// CLI ack 行）：位点推进（#11）与 block 逐条回执（#12，可多位并存一次调用）。
// 成功输出 `OK position=<信箱位点> receipts=[a,b]`（§4.2 样例；无 --receipt 时
// 省略 receipts 段——样例只在回执场景给出该段）。幂等（AC9.2）：重复 ack 位点
// 取 max 零副作用、重复回执 already_recepted 200 返回已有记录，同参重放输出一致。

// ackData 端点 #11 响应 data（§2.2 #11 恰双位点两键）。
type ackData struct {
	MailboxPosition int64 `json:"mailbox_position"`
	DialogPosition  int64 `json:"dialog_position"`
}

// receiptData 端点 #12 响应 data（§2.2 #12 恰三键；CLI 只消费成功/失败事实，
// 结构体保留字段面备 --json 扩展与文档对拍）。
type receiptData struct {
	Seq       int64  `json:"seq"`
	ReceiptAt string `json:"receipt_at"`
	ReceiptBy string `json:"receipt_by"`
}

// RunAck `aiteam ack` 一级动词入口（main.go 分发，§4.2 五核心）。
func RunAck(args []string) int {
	return runAck(context.Background(), args, "")
}

// runAck ack 命令主链（serverOverride 供测试注入 httptest 地址，B1 命令同款）。
//
// 两步时序裁量（§4.2/§1.4 原文未定义步序，注释留痕）：①先 POST #11 位点推进，
// ②再逐条 POST #12 回执。依据=§4.2 flag 表与输出样例均位点在先（--seq 主面、
// position 领衔），主操作先行的失败恢复最简：#11 失败则整条命令零副作用，重跑
// 即可；#12 中途失败时位点已推进、已成功前缀不回滚（服务端每条 #12 内部回执行
// +回告同事务已落库），凭发送方未回执清单或重跑 ack --receipt <剩余seq> 幂等补
// 齐（回执不依赖位点）。反序（先回执后位点）恢复成本相当，不取。
func runAck(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam ack",
		"用法: aiteam ack [--seq <n>] [--receipt <seq[,seq...]>] "+identityUsage+
			"（位点归 --column+--role，对话位点归 --session；--seq 省略=推进到当前各自最大可见）")
	var id identityFlags
	var seqFlag, receiptFlag string
	registerCommonFlags(fs, &id)
	fs.StringVar(&seqFlag, "seq", "", "显式推进到该序号（省略=一键处理完拉到的全部）")
	fs.StringVar(&receiptFlag, "receipt", "", "block 逐条回执，逗号分隔多位（如 --receipt 881,884）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	// 本地校验（§4.1 退 2 不发请求）：--seq 非负整数（空=省略语义）；--receipt
	// 逐个正整数、空片段与 0 拒绝（口径分叉缘由见 parseReceiptFlag 注）。重复
	// seq 不去重：服务端 #12 幂等 200，重放安全。
	seq, err := parseNonNegFlag("--seq", seqFlag)
	if err != nil {
		return fail(err)
	}
	receiptSeqs, err := parseReceiptFlag(receiptFlag)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}

	// ① 位点推进（#11）：column/role/session/seq 全走 query（§2.2 #11 请求面，
	//    服务端 resolveMailboxDomain 同口径）；seq=0 不传=服务端推进到各自最大
	//    可见。响应双位点取信箱位点入成功行（§4.2 样例 position 语境=信箱位点，
	//    与 status 样例 pos 同源；对话位点在 #11 响应内落定、CLI 不再展示）。
	q := url.Values{}
	q.Set("column", ident.Column)
	q.Set("role", ident.Role)
	q.Set("session", ident.Session)
	if seq > 0 {
		q.Set("seq", strconv.FormatInt(seq, 10))
	}
	var pos ackData
	if err := c.Do(ctx, http.MethodPost, "/api/v1/acks?"+q.Encode(), nil, &pos); err != nil {
		return fail(err)
	}

	// ② 逐条回执（#12）：服务端是单 seq 端点（POST /api/v1/messages/{seq}/receipt），
	//    多条=N 次 HTTP 调用——API 形态决定的唯一解。b2-spec §七「--receipt N 条=
	//    N INSERT receipts+N INSERT 回告同事务」约束的是服务端每次 #12 内部的
	//    原子性（单条回执行+回告消息同事务，服务端 B2-5 已保证），非 CLI 侧跨
	//    请求合并（HTTP 无此机制）；CLI 逐条调用，任一条失败即停、报错指明失败
	//    seq（次条起另报已成功前缀——不回滚，已成功各条服务端已原子落库，重跑
	//    幂等补齐；首条即失败无前缀可报，见循环内 R1 注）。
	for i, rs := range receiptSeqs {
		var rec receiptData
		if err := c.Do(ctx, http.MethodPost,
			"/api/v1/messages/"+strconv.FormatInt(rs, 10)+"/receipt", nil, &rec); err != nil {
			// 首条即失败（B2-7 复核 R1）：无已成功前缀，省略「已成功回执」段
			// ——避免「已成功回执 [] 不回滚」空段误导（[] 并无任何既成事实）。
			if i == 0 {
				return fail(fmt.Errorf("回执 seq=%d 失败（可重跑本命令幂等补齐）: %w", rs, err))
			}
			return fail(fmt.Errorf("回执 seq=%d 失败（已成功回执 %s 不回滚，剩余可重跑本命令幂等补齐）: %w",
				rs, formatSeqs(receiptSeqs[:i]), err))
		}
	}

	if len(receiptSeqs) > 0 {
		fmt.Fprintf(os.Stdout, "OK position=%d receipts=%s\n", pos.MailboxPosition, formatSeqs(receiptSeqs))
	} else {
		fmt.Fprintf(os.Stdout, "OK position=%d\n", pos.MailboxPosition)
	}
	return 0
}

// formatSeqs seq 列表渲染为 `[a,b]`（§4.2 receipts=[881] 同形；逗号分隔与
// --receipt 输入格式一致。fmt.Sprint 对切片是空格分隔，不合样例形状）。
func formatSeqs(seqs []int64) string {
	parts := make([]string, len(seqs))
	for i, s := range seqs {
		parts[i] = strconv.FormatInt(s, 10)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// parseNonNegFlag 非负整数 flag 解析（--seq 等）：空串=省略语义返回 0；非数字/
// 负值=用法错误（服务端 400 param_invalid 保留为权威防线）。
func parseNonNegFlag(name, raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: %s 须为非负整数（不传=省略语义），得到 %q", ErrUsage, name, raw)
	}
	return n, nil
}

// parseReceiptFlag --receipt 值解析：逗号拆分逐段正整数，保序返回（输出
// receipts=[...] 按请求顺序呈现）；空白片段（尾随逗号/纯空白段，TrimSpace 后
// 空串 ParseInt 必失败自然落入同一分支）与 0 值片段一并拒绝。缺省=空切片（无
// 回执步）。
//
// 口径分叉留痕（B2-7 复核 M1）：此处正整数、#11 --seq 走 parseNonNegFlag 非负
// ——两契约本就不同：#12 回执目标是消息 seq（从 1 起，服务端对 seq=0 恒 400
// param_invalid），#11 的 0 则是「省略=推进到各自最大可见」语义。故不复用
// parseNonNegFlag 的 0 合法分支：放行 0 会造成位点已推进+部分回执成功后 seq=0
// 请求撞服务端 400 退 4 的半成功态与误导报错，本地拦截退 2（§4.1 校验缺漏不
// 发请求）才是干净失败。
func parseReceiptFlag(raw string) ([]int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var seqs []int64
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%w: --receipt 须为逗号分隔的正整数（如 881,884），片段 %q 非法（正整数，空片段与 0 不接受）", ErrUsage, part)
		}
		seqs = append(seqs, n)
	}
	return seqs, nil
}
