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
)

// 本文件为五核心 poll 命令实现（B2-7，技术设计 §4.2/§2.2 #10/§3.6、b2-spec §二
// CLI poll 行）：身份四参 → GET #10（query column/role/session=消费视角，§2.2
// #10 原文）→ 渲染输出。GET 零位点推进（幂等重拉 AC8.1），limit 语义照 D8：
// 缺省 500 条分页、--limit 0 显式要全量、负值本地退 2（服务端 400 保留防线）。

// pollMessageData #10 响应 messages 元素（§2.2 #10 恰六键，与服务端
// mailboxMessageData 同构——CLI 侧独立声明，响应面变化时两侧同检）。
type pollMessageData struct {
	Seq       int64          `json:"seq"`
	Kind      string         `json:"kind"`
	Level     string         `json:"level"`
	Body      string         `json:"body"`
	Sender    pollSenderData `json:"sender"`
	CreatedAt string         `json:"created_at"`
}

// pollSenderData #10 响应 sender 身份三件（§2.2 #10：sender:{session,role,column}，
// AC8.4 五要素之身份面）。
type pollSenderData struct {
	Session string `json:"session"`
	Role    string `json:"role"`
	Column  string `json:"column"`
}

// pollCrossGridHint #10 响应 cross_grid_hint 元素（b7-W3 additive：`[]{role,pending}`
// 最小面，AC5——零正文/零 level/零时间戳）。与服务端 handler_messages.go
// crossGridHintData 同构——CLI 侧独立声明，响应面变化时两侧同检。Role 为聚合哨兵
// 时（服务端截断面，形如「等 N 格」，识别规则见 renderCrossGridHints）渲染聚合行。
type pollCrossGridHint struct {
	Role    string `json:"role"`
	Pending int    `json:"pending"`
}

// pollData 端点 #10 响应 data（§2.2 #10 三键+b7-W2 additive 第四键）——--json 输出
// 直接序列化本结构，与 #10 响应同构（§4.2「--json 输出与 #10 响应同构」）。
// CrossGridHint 空态/降级=键缺省（服务端 omitempty，同构镜像）。
type pollData struct {
	MailboxPosition int64               `json:"mailbox_position"`
	DialogPosition  int64               `json:"dialog_position"`
	Messages        []pollMessageData   `json:"messages"`
	CrossGridHint   []pollCrossGridHint `json:"cross_grid_hint,omitempty"`
}

// defaultPollLimit poll 缺省上限（D8 裁定：默认 500 条分页，--limit 0 显式要
// 全量；与服务端 store.DefaultPollLimit 同值口径，服务端调整须同步）。
const defaultPollLimit = 500

// RunPoll `aiteam poll` 一级动词入口（main.go 分发，§4.2 五核心）。
func RunPoll(args []string) int {
	return runPoll(context.Background(), args, "")
}

// runPoll poll 命令主链（serverOverride 供测试注入 httptest 地址，B1 命令同款）。
// 身份四参即消费视角（§4.2「信箱=column+role；对话=session 同拉」）：project 走
// 身份四头，column/role/session 走 #10 query 三参（§2.2 #10 原文口径）。
func runPoll(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam poll",
		"用法: aiteam poll [--limit <n>] [--json] "+identityUsage+
			"（信箱=--column+--role，对话=--session 同拉）")
	var id identityFlags
	var limit int
	var asJSON bool
	registerCommonFlags(fs, &id)
	fs.IntVar(&limit, "limit", defaultPollLimit, "单次拉取上限（缺省 500 条分页；0=显式要全量）")
	fs.BoolVar(&asJSON, "json", false, "机器可读 JSON 输出（与 #10 响应同构）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	// 负值本地拦（§4.1 本地校验缺漏退 2 不发请求）；0=全量合法值照传。
	if limit < 0 {
		return fail(fmt.Errorf("%w: --limit 须为非负整数（缺省 500，0=全量），得到 %d", ErrUsage, limit))
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	q := url.Values{}
	q.Set("column", ident.Column)
	q.Set("role", ident.Role)
	q.Set("session", ident.Session)
	q.Set("limit", strconv.Itoa(limit))
	var resp pollData
	if err := c.Do(ctx, http.MethodGet, "/api/v1/mailbox?"+q.Encode(), nil, &resp); err != nil {
		return fail(err) // UnreachableError→3 / APIError→4（mapExitCode 既有分流）
	}
	if asJSON {
		b, err := json.Marshal(resp)
		if err != nil {
			return fail(fmt.Errorf("JSON 序列化失败: %w", err))
		}
		fmt.Fprintln(os.Stdout, string(b))
	} else {
		fmt.Fprint(os.Stdout, renderPollOutput(resp))
	}
	// b7-W3 跨格防呆提示（stderr，stdout 渲染零改动——管道消费者不受扰）：非空
	// cross_grid_hint 时逐格一行。--json 模式同样输出：提示面向人，JSON 消费者
	// 可自行解析该字段，文本行兜住不读字段的人。
	for _, line := range renderCrossGridHints(ident.Role, resp.CrossGridHint) {
		fmt.Fprintln(os.Stderr, line)
	}
	return 0
}

// senderLabel #10/#13 响应 sender 三件 → `session@column`（服务端 sender_label
// 冗余格式同构，§2.2 #9）；会话缺失（系统/看板/回告语义行三件空串）时以 - 占位
// 保持行形状（poll 与 history 两渲染器共用）。
func senderLabel(s pollSenderData) string {
	if s.Session == "" {
		return "-"
	}
	return s.Session + "@" + s.Column
}

// renderPollOutput §4.2 输出样例的纯函数渲染（data→文本，零 IO 零时钟，测试
// 逐字对拍）：每条消息一行头部 `[seq] level  created_at  sender(kind)` + 正文
// 逐行 6 空格缩进 + 尾部摘要行 `# pending=N mailbox_pos=X dialog_pos=Y`。
//
// 样例对拍裁量两处（冻结原文为设计示意，机械可复刻口径如下）：
//  1. kind 标注取 #10 响应 kind 字段原值（direct/bus/chat/receipt 枚举）——样例
//     首行括号内 controller 为发送方 role 值、第二行 chat 恰为 kind 值，两行不
//     同源无法用单一字段逐字复刻；取 kind 与任务书「kind 标注」及 #10 响应字段
//     面一致，第二行样例逐字命中。
//  2. sender 展示经 senderLabel（系统/回告语义行占位 -，见其注）。
func renderPollOutput(d pollData) string {
	var b strings.Builder
	for _, m := range d.Messages {
		fmt.Fprintf(&b, "[%d] %s  %s  %s(%s)\n", m.Seq, m.Level, m.CreatedAt,
			senderLabel(m.Sender), m.Kind)
		// 正文多行原样缩进展示（样例 6 空格）：仅去掉单个尾换行（stdin 管道
		// 读入的行尾符非正文内容），中部换行与空行原样保留逐行缩进。
		for _, line := range strings.Split(strings.TrimSuffix(m.Body, "\n"), "\n") {
			b.WriteString("      ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	// pending 口径留痕（B2-7 复核 S1）：pending=本次返回条数 len(messages)，
	// limit 截断时并非全量待消费数；与 B3 status 的 pending（服务端聚合真值）
	// 同词异义，B3 实现 status 时勿沿用此处口径。
	fmt.Fprintf(&b, "# pending=%d mailbox_pos=%d dialog_pos=%d\n",
		len(d.Messages), d.MailboxPosition, d.DialogPosition)
	return b.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// b7-W3 跨格防呆提示渲染（stderr 面；b7-spec §二.3 逐字模板，多格逐格一行）
// ─────────────────────────────────────────────────────────────────────────────

// crossGridAggregatePrefix/Suffix 聚合哨兵元素 role 值的识别边界（与服务端
// handler_messages.go crossGridAggregateRole 格式两侧同检——服务端以
// fmt.Sprintf("等 %d 格", 剩余格数) 构造，勿单侧改措辞）。
const (
	crossGridAggregatePrefix = "等 "
	crossGridAggregateSuffix = " 格"
)

// parseAggregateGrids 聚合哨兵元素识别（role 形如「等 N 格」）：返回 N 与是否命
// 中。服务端截断断面（>5 格积压时末元素），真实 role 命中该形态的概率可忽略，
// 误判后果仅是提示行措辞走聚合分支（展示层装饰，无语义损害）。
//
// 长度守卫（T2 审查修 1·必须）：role 名同时命中前后缀且短于前缀+后缀时（如
// 「等 格」7 字节），字节切片上下界颠倒 panic——send --to-role 目标值仅校验非
// 空，恶意/巧合 role 名可落库并回流 hint，渲染层必须先验长度再切片。
func parseAggregateGrids(role string) (int, bool) {
	if len(role) < len(crossGridAggregatePrefix)+len(crossGridAggregateSuffix) {
		return 0, false
	}
	if !strings.HasPrefix(role, crossGridAggregatePrefix) || !strings.HasSuffix(role, crossGridAggregateSuffix) {
		return 0, false
	}
	mid := role[len(crossGridAggregatePrefix) : len(role)-len(crossGridAggregateSuffix)]
	n, err := strconv.Atoi(mid)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// renderCrossGridHints 防呆提示纯函数渲染（data→文本行，零 IO，测试逐字对拍）：
// 逐格一行 spec §二.3 逐字模板；聚合哨兵元素（服务端 >5 格截断面，role=「等 N
// 格」）渲染聚合行。空切片/nil → 零行（AC2 零噪音——提示只在错格且有积压时出现）。
func renderCrossGridHints(ownRole string, hints []pollCrossGridHint) []string {
	if len(hints) == 0 {
		return nil
	}
	lines := make([]string, 0, len(hints))
	for _, h := range hints {
		if n, ok := parseAggregateGrids(h.Role); ok {
			lines = append(lines, fmt.Sprintf(
				"提示：本格（role=%s）无定向消息；同栏目另有 %d 格共 %d 条未消费定向——若你的身份应为其中一格，检查 --role 是否填错",
				ownRole, n, h.Pending))
			continue
		}
		lines = append(lines, fmt.Sprintf(
			"提示：本格（role=%s）无定向消息；同栏目 role=%s 格有 %d 条未消费定向——若你的身份应为该格，检查 --role 是否填错",
			ownRole, h.Role, h.Pending))
	}
	return lines
}
