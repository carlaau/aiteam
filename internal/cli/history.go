package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"aiteam/internal/store"
)

// 本文件为 history 命令实现（B2-7，技术设计 §4.6/§2.2 #13、b2-spec §二 CLI
// history 行）：通用历史/审计查询（AC1.3/AC2.3 历史保留、AC7.2/7.3 回执状态
// 判据）。输出一行一条含回执状态列——block 回执前「待回执」、回执后「已回执
// by/at」（§4.6「--level block 输出含回执状态列」）；--json 与 #13 响应同构。

// historyReceipt #13 响应 receipt 字段（§2.2 #13「receipt:{by,at}|null」）。
type historyReceipt struct {
	By string `json:"by"`
	At string `json:"at"`
}

// historyMessage #13 响应 messages 元素（§2.2 #13 恰七键，receipt 指针 nil=未
// 回执 JSON null）。
type historyMessage struct {
	Seq       int64           `json:"seq"`
	Kind      string          `json:"kind"`
	Level     string          `json:"level"`
	Body      string          `json:"body"`
	Sender    pollSenderData  `json:"sender"`
	Receipt   *historyReceipt `json:"receipt"`
	CreatedAt string          `json:"created_at"`
}

// historyData 端点 #13 响应 data（§2.2 #13 恰 {messages} 一键）——--json 输出
// 直接序列化本结构，与 #13 响应同构。
type historyData struct {
	Messages []historyMessage `json:"messages"`
}

// RunHistory `aiteam history` 一级动词入口（main.go 分发，§4.6）。
func RunHistory(args []string) int {
	return runHistory(context.Background(), args, "")
}

// runHistory history 命令主链（serverOverride 供测试注入 httptest 地址）。
//
// --project/--column 一参两用（session list 先例同型，register.go 头注）：身份
// 四参必填（§4.1 心跳中间件强制四头，#13 无豁免），过滤值即身份值——历史查询
// 天然按操作者域锚定（消息都挂在项目下）；§4.6 原文 [--project]/[--column] 在
// 身份必填前提下与身份同名，跨域审计口归 audit 命令（其「身份/目标分离」裁定
// 同源），本命令不做异域过滤。其余过滤 flag 独立直通 #13 query。
func runHistory(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam history",
		"用法: aiteam history [--kind direct|bus|chat|receipt] [--level normal|important|block] "+
			"[--sent-by <会话名>] [--since-seq <n>] [--before-seq <n>] [--limit <n>] [--order asc|desc] "+
			"[--json] "+identityUsage+"（--project/--column 兼作域过滤）")
	var id identityFlags
	var kind, level, sentBy, sinceSeq, beforeSeq, order string
	var limit int
	var asJSON bool
	registerCommonFlags(fs, &id)
	fs.StringVar(&kind, "kind", "", "按 kind 精确过滤 direct|bus|chat|receipt")
	fs.StringVar(&level, "level", "", "按 level 精确过滤 normal|important|block（block 输出含回执状态列）")
	fs.StringVar(&sentBy, "sent-by", "", "按发送方会话名过滤")
	fs.StringVar(&sinceSeq, "since-seq", "", "只查 seq >= 该值（0=不过滤）")
	fs.StringVar(&beforeSeq, "before-seq", "", "只查 seq < 该值（0=不过滤）")
	fs.IntVar(&limit, "limit", 0, "返回条数上限（0=服务默认 50，最大 500；负值=用法错误）")
	fs.StringVar(&order, "order", "", "排序 asc|desc（缺省=服务默认 desc）")
	fs.BoolVar(&asJSON, "json", false, "机器可读 JSON 输出（与 #13 响应同构）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	// 数值/枚举参数本地校验（§4.1 退 2 不发请求；服务端 400 保留为权威防线）。
	since, err := parseNonNegFlag("--since-seq", sinceSeq)
	if err != nil {
		return fail(err)
	}
	before, err := parseNonNegFlag("--before-seq", beforeSeq)
	if err != nil {
		return fail(err)
	}
	if order != "" && order != "asc" && order != "desc" {
		return fail(fmt.Errorf("%w: --order 须为 asc 或 desc，得到 %q", ErrUsage, order))
	}
	// limit 负值本地退 2（B2-7 复核 S2：与同批 poll --limit、history
	// --since-seq/--before-seq 的非负口径批内统一，不再静默落默认——静默改写
	// 用户输入易误判查询范围）。audit 命令静默落默认是 B1 既有先例，不越界改。
	if limit < 0 {
		return fail(fmt.Errorf("%w: --limit 须为非负整数（0=服务默认 50），得到 %d", ErrUsage, limit))
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	q := url.Values{}
	q.Set("project", ident.Project)
	q.Set("column", ident.Column)
	for _, f := range []struct{ name, val string }{
		{"kind", kind}, {"level", level}, {"sent_by", sentBy},
	} {
		if f.val != "" {
			q.Set(f.name, f.val)
		}
	}
	if since > 0 {
		q.Set("since_seq", strconv.FormatInt(since, 10))
	}
	if before > 0 {
		q.Set("before_seq", strconv.FormatInt(before, 10))
	}
	// limit>0 才透传；0=不发 limit 键（服务端默认 50 语义，audit 同型口径；
	// 负值已在上方本地拦截退 2）。
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if order != "" {
		q.Set("order", order)
	}
	var resp historyData
	if err := c.Do(ctx, http.MethodGet, "/api/v1/messages?"+q.Encode(), nil, &resp); err != nil {
		return fail(err)
	}
	if asJSON {
		b, err := json.Marshal(resp)
		if err != nil {
			return fail(fmt.Errorf("JSON 序列化失败: %w", err))
		}
		fmt.Fprintln(os.Stdout, string(b))
		return 0
	}
	fmt.Fprint(os.Stdout, renderHistoryOutput(resp))
	return 0
}

// renderHistoryOutput 一行一条的纯函数渲染（data→文本，零 IO，测试逐字对拍）：
// `[seq] level kind sender 回执状态 created_at body摘要`，列单空格分隔。
//
// 输出形状裁量（§4.6 无输出样例，注释留痕）：对齐 audit「一行一条：id action
// session detail摘要 created_at」先例——history 是审计列表非消费流（poll 已承
// 担逐条正文展示），body 压单行摘要不整段展开；回执状态列恒在（列位稳定，非
// block 行以 - 占位；§4.6「--level block 输出含回执状态列」的判据在过滤场景
// 自然满足）。状态值：block 未回执=待回执（AC7.2）；已回执=已回执 by=<会话名>
// at=<服务端时间>（AC7.3）；其余=-（#13 非 block 行 receipt 恒 null）。
func renderHistoryOutput(d historyData) string {
	var b strings.Builder
	for _, m := range d.Messages {
		fmt.Fprintf(&b, "[%d] %s %s %s %s %s %s\n",
			m.Seq, m.Level, m.Kind, senderLabel(m.Sender),
			receiptStatus(m), m.CreatedAt, summarizeDetail(m.Body))
	}
	return b.String()
}

// receiptStatus 回执状态列（AC7.2/7.3 判据渲染面，规则见 renderHistoryOutput）。
func receiptStatus(m historyMessage) string {
	if m.Level != store.MessageLevelBlock {
		return "-"
	}
	if m.Receipt == nil {
		return "待回执"
	}
	return "已回执 by=" + m.Receipt.By + " at=" + m.Receipt.At
}
