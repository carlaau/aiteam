package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// status.go —— `aiteam status` 观测命令（B3-7，技术设计 §4.2 status 段）：
// 单栏目模式（缺省，四项+block 未回执，AC11.1）/ 全局模式（--global，跨项目
// 一行式 §4.2 样例，AC11.2）+ --all（all=1 扩面含 archived，总控 #20 裁定）/
// --json（与 #17 响应 data 同构）。数据源=#17 GET /api/v1/status（B3-5）——CLI
// 纯消费渲染，零自含聚合复刻（b3-plan B3-7 数据源锚定）。
//
// 结构裁定：响应面经 CLI 侧镜像 DTO（resource.go/poll.go 惯例——生产文件不
// import store 包，json tag 对齐 §2.3 端点字段，响应面变化时两侧同检）；渲染
// 抽纯函数 renderGlobalLines/renderColumnLines（data→[]string，零 IO 零时钟，
// 测试逐行分词对拍——b3-plan B3-7「渲染函数纯化便于断言」）。
//
// 模式选择裁定：--project/--column 为身份四参成员（心跳链冻结四头必填）兼单
// 栏目定位域（一参两用，resource/window 组同款）——「--global 与 --project/
// --column 混给=用法冲突退 2」（b3-plan B3-7 退出码段）在身份恒必填的实现下
// 无可触发输入形态（flag 复用使两模式选择收敛为 --global 单开关，缺省=单栏目，
// 技术设计 §4.2「两模式二选一，缺省=单栏目」），不存在独立模式参数可与之相撞。

// ─────────────────────────────────────────────────────────────────────────────
// 响应结构（对齐 §2.3 #17 端点 data 字段面，store.Overview 同构镜像）
// ─────────────────────────────────────────────────────────────────────────────

// statusData #17 响应 data（顶层五键）——--json 直接序列化本结构（与 #17 响应
// data 同构，poll.go 先例；同构契约由 TestStatusJSON 端到端深等机械执行——
// B5-6 起 #17 增 config 回显段，本镜像同步加键，文本渲染不消费）。
type statusData struct {
	GeneratedAt      string          `json:"generated_at"`      // 注入时钟（=服务端查询时刻）
	Projects         []statusProject `json:"projects"`          // 仅 active 项目
	BlockUnreceipted []statusBlock   `json:"block_unreceipted"` // 全局 block 未回执清单（seq DESC）
	ResourcesSummary statusResources `json:"resources_summary"` // in_use 资源摘要
	Config           statusConfig    `json:"config"`            // 三阈值生效值回显（B5-6）
}

// statusConfig §2.3 config 回显段（B5-6 增补——服务侧三阈值生效值，与
// store.OverviewConfig 同构镜像）：心跳失联/哨兵失活/进度 stale 黄档阈值秒。
type statusConfig struct {
	HeartbeatTimeoutSec   int64 `json:"heartbeat_timeout_sec"`
	SentinelTimeoutSec    int64 `json:"sentinel_timeout_sec"`
	ProgressStaleAfterSec int   `json:"progress_stale_after_sec"`
}

// statusProject §2.3 项目层。
type statusProject struct {
	Code     string          `json:"code"`
	Name     string          `json:"name"`
	Status   string          `json:"status"`
	Columns  []statusColumn  `json:"columns"`
	Sessions []statusSession `json:"sessions"`
}

// statusColumn §2.3 栏目层：windows 为压缩视图（key=stage，只列已配置启用阶段，
// 未配置栏目={} 键恒在）。
type statusColumn struct {
	Code      string                       `json:"code"`
	Status    string                       `json:"status"`
	Mailboxes []statusMailbox              `json:"mailboxes"`
	Windows   map[string]statusWindowPhase `json:"windows"`
}

// statusWindowPhase windows 压缩视图值面（status=allowed|waiting 判定态透传，
// start/end=HH:MM，start>end=跨午夜原样）。
type statusWindowPhase struct {
	Status string `json:"status"`
	Start  string `json:"start"`
	End    string `json:"end"`
}

// statusMailbox §2.3 信箱层（pending=direct+bus 待消费；latest=最新一条含已消费；
// sentinel 三态 alive/dead/none）。
type statusMailbox struct {
	Role     string        `json:"role"`
	Pending  int64         `json:"pending"`
	Position int64         `json:"position"`
	Latest   *statusLatest `json:"latest"`
	Sentinel string        `json:"sentinel"`
}

// statusLatest 信箱最新一条消息摘要（seq/level/created_at 三键）。
type statusLatest struct {
	Seq       int64  `json:"seq"`
	Level     string `json:"level"`
	CreatedAt string `json:"created_at"`
}

// statusSession §2.3 会话行（全字段镜像——--json 同构所需；sentinels/progress
// 两段人类可读渲染不消费，反序列化面保留）。SentinelLastHitAt 为 b8-W3 命中
// 留痕——人类可读渲染消费（「·命中Ns前」后缀），空串=从未命中。unread 两分项
// 为 b3-W2 镜像同步——#17 载荷新增 unread_mailbox/unread_dialog（store 侧
// SessionEntry b3-W1 直序列化），人类可读渲染不消费，--json 同构所需（「响应面
// 变化时两侧同检」兑现）。
type statusSession struct {
	ID                int64                   `json:"id"`
	Project           string                  `json:"project"`
	Column            string                  `json:"column"`
	Name              string                  `json:"name"`
	Role              string                  `json:"role"`
	LastSeenAt        string                  `json:"last_seen_at"`
	Alive             bool                    `json:"alive"` // false=超失联阈值（lost=`>` 严格大于）
	LostForSec        int64                   `json:"lost_for_sec"`
	SentinelLastHitAt string                  `json:"sentinel_last_hit_at"` // b8-W3：最近命中时刻（''=从未命中）
	Sentinels         []statusSessionSentinel `json:"sentinels"`            // #27 口径段
	UnreadMailbox     int64                   `json:"unread_mailbox"`       // b3-W2：未读信箱数（#17 载荷字段直抄——store.SessionEntry b3-W1 同键名镜像）
	UnreadDialog      int64                   `json:"unread_dialog"`        // b3-W2：未读对话数（同上；两键恒在无 omitempty，--json 序列化面透出）
	Progress          *statusSessionProgress  `json:"progress,omitempty"`   // FR23 段（无上报不出键）
}

// statusSessionSentinel #27 sentinels 数组元素。
type statusSessionSentinel struct {
	ID    int64  `json:"id"`
	Role  string `json:"role"`
	Alive bool   `json:"alive"`
}

// statusSessionProgress sessions[].progress 段（stale：""=正常/stale=黄/dead=红）。
type statusSessionProgress struct {
	CommitHash string `json:"commit_hash"`
	Batch      string `json:"batch"`
	Task       string `json:"task"`
	CreatedAt  string `json:"created_at"`
	Stale      string `json:"stale"`
}

// statusBlock §2.3 block 未回执清单元素（target="项目code/栏目code/目标角色"，
// sender=sender_label 冗余显示）。
type statusBlock struct {
	Seq       int64  `json:"seq"`
	Level     string `json:"level"`
	Target    string `json:"target"`
	Sender    string `json:"sender"`
	CreatedAt string `json:"created_at"`
}

// statusResources §2.3 资源摘要（by_type 键=服务端 rtype 规范名）。
type statusResources struct {
	InUseCount int64            `json:"in_use_count"`
	ByType     map[string]int64 `json:"by_type"`
}

// ─────────────────────────────────────────────────────────────────────────────
// 命令入口
// ─────────────────────────────────────────────────────────────────────────────

// RunStatus `aiteam status` 一级命令入口（main.go 挂载；无二级动词）。
func RunStatus(args []string) int {
	return runStatus(context.Background(), args, "")
}

// runStatus status 命令主链（serverOverride 供测试注入 httptest 地址，B1 命令
// 同款）。退出码（§4.7）：0 成功 / 2 用法本地（缺身份、单栏目给 --all）/
// 3 不可达 / 4 服务端拒绝（mode=column 未登记栏目 404 column_not_found 透传）。
func runStatus(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam status",
		"用法: aiteam status [--global] [--all] [--json] "+identityUsage+
			"（缺省=单栏目模式，--project/--column 一参两用即定位域）")
	var id identityFlags
	var global, all, asJSON bool
	registerCommonFlags(fs, &id)
	fs.BoolVar(&global, "global", false, "全局模式：跨项目一行式（AC11.2；与单栏目二选一，缺省=单栏目）")
	fs.BoolVar(&all, "all", false, "全局模式含 archived 项目（all=1；缺省仅 active）")
	fs.BoolVar(&asJSON, "json", false, "机器可读 JSON 输出（与 #17 响应 data 同构）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	// --all 语义面=全局模式含 archived（§4.2 flag 表）——单栏目模式给 --all 属
	// 用法冲突，本地退 2 不发请求。
	if all && !global {
		return fail(fmt.Errorf("%w: --all 仅全局模式（--global）合法", ErrUsage))
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	// #17 query 面：--global=mode=overview 全局全量（--all 追加 all=1 扩面参数，
	// 总控 #20 裁定——消 TODO(B3-7-all) 占位）；缺省=mode=column 单栏目
	// （定位域=身份 --project/--column 一参两用，双参数同传 §2.2 #17 口径）。
	q := url.Values{}
	if global {
		q.Set("mode", "overview")
		if all {
			q.Set("all", "1")
		}
	} else {
		q.Set("mode", "column")
		q.Set("project", ident.Project)
		q.Set("column", ident.Column)
	}
	var resp statusData
	if err := c.Do(ctx, http.MethodGet, "/api/v1/status?"+q.Encode(), nil, &resp); err != nil {
		return fail(err) // UnreachableError→3 / APIError→4（mapExitCode 既有分流）
	}
	if asJSON {
		b, err := json.Marshal(resp)
		if err != nil {
			return fail(fmt.Errorf("JSON 序列化失败: %w", err))
		}
		fmt.Fprintln(os.Stdout, string(b))
		return 0
	}
	var lines []string
	if global {
		lines = renderGlobalLines(&resp)
	} else {
		lines = renderColumnLines(&resp, ident.Project, ident.Column)
	}
	for _, line := range lines {
		fmt.Fprintln(os.Stdout, line)
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// 渲染纯函数（data→[]string，零 IO 零时钟——测试逐行分词对拍）
// ─────────────────────────────────────────────────────────────────────────────

// renderGlobalLines 全局模式逐行渲染（§4.2 样例逐行形态；样例多空格为对齐填充，
// 本渲染统一两空格分隔、窗标记/资源键值间单空格——断言按分词序列对拍）：
//
//	aiteam status <generated_at>
//	<proj>  <col>  <role>  pending=N  pos=N  sentinel=alive|dead*|none  <窗标记|->
//	sessions: <name>@<col> OK | <name>@<col> LOST(分钟m)* | ...
//	block 未回执: N 条
//	  #<seq> <target> <- <sender> (<created_at>)
//	resources in_use: port=N account=N data=N
//
// 样例尾行「（*=异常标记；LOST=超失联阈值；sentinel=dead=哨兵超时未 ping）」为
// 设计文档图例说明（全角括号注释形态），非输出内容不渲染（poll.go renderPollOutput
// 「冻结原文为设计示意」同款裁量）。
func renderGlobalLines(ov *statusData) []string {
	lines := []string{"aiteam status " + ov.GeneratedAt}
	for _, p := range ov.Projects {
		for _, col := range p.Columns {
			for _, mb := range col.Mailboxes {
				lines = append(lines, fmt.Sprintf("%s  %s  %s  pending=%d  pos=%d  sentinel=%s  %s",
					p.Code, col.Code, mb.Role, mb.Pending, mb.Position,
					statusSentinelCell(mb.Sentinel), statusWindowCell(col.Windows)))
			}
		}
	}
	if sess := statusSessionsCell(ov.Projects, ov.GeneratedAt); sess != "" {
		lines = append(lines, sess)
	}
	lines = append(lines, statusBlockLines(ov.BlockUnreceipted)...)
	lines = append(lines, statusResourcesCell(ov.ResourcesSummary))
	return lines
}

// renderColumnLines 单栏目模式逐行渲染（AC11.1 四项：待消费数/最新摘要/哨兵
// 活性/时间窗状态 + block 未回执段）：行形态与全局同源（两空格分隔），信箱行
// 增 latest 列（全局样例无此列）；sessions/resources 段不在四项范围不渲染。
// block 清单走端点单栏目口径原样透出（store 侧已按调用方项目过滤——过滤域=
// 项目非栏目；CLI 纯消费不做二次过滤）。
func renderColumnLines(ov *statusData, project, column string) []string {
	lines := []string{"aiteam status " + ov.GeneratedAt}
	for _, p := range ov.Projects {
		if p.Code != project {
			continue
		}
		for _, col := range p.Columns {
			if col.Code != column {
				continue
			}
			for _, mb := range col.Mailboxes {
				latest := "-"
				if mb.Latest != nil {
					latest = fmt.Sprintf("#%d %s %s", mb.Latest.Seq, mb.Latest.Level, mb.Latest.CreatedAt)
				}
				lines = append(lines, fmt.Sprintf("%s  %s  %s  pending=%d  pos=%d  latest=%s  sentinel=%s  %s",
					p.Code, col.Code, mb.Role, mb.Pending, mb.Position, latest,
					statusSentinelCell(mb.Sentinel), statusWindowCell(col.Windows)))
			}
		}
	}
	return append(lines, statusBlockLines(ov.BlockUnreceipted)...)
}

// statusSentinelCell 哨兵列：dead 追加 * 异常标记（§4.2 `sentinel=dead*`；
// *=异常标记图例），alive/none 原样。
func statusSentinelCell(v string) string {
	if v == "dead" {
		return "dead*"
	}
	return v
}

// statusWindowCell 窗标记列（§4.2 形态 STAGE:STATUS(HH:MM-HH:MM)）：status 大写
// 同构（waiting→WAITING、allowed→ALLOWED）；多窗按 stage 字典序空格连排（map
// 无序，机械稳定序——stage 集合常态 S0..S7 单字符，字典序即语义序）；空对象=
// 未配置/停用视同不受限，输出 `-`（AC3.4）。
func statusWindowCell(windows map[string]statusWindowPhase) string {
	if len(windows) == 0 {
		return "-"
	}
	stages := make([]string, 0, len(windows))
	for stage := range windows {
		stages = append(stages, stage)
	}
	slices.Sort(stages)
	parts := make([]string, 0, len(stages))
	for _, stage := range stages {
		w := windows[stage]
		parts = append(parts, fmt.Sprintf("%s:%s(%s-%s)", stage, strings.ToUpper(w.Status), w.Start, w.End))
	}
	return strings.Join(parts, " ")
}

// statusSessionsCell sessions 行（§4.2 单行）：`sessions: <name>@<col> OK | ...
// | <name>@<col> LOST(分钟m)*`——OK=alive；LOST=超失联阈值，分钟数=lost_for_sec/60
// （整数除法截断）；异常标记 * 随 LOST。全部项目会话连拼一行（端点按项目嵌套，
// 样例为全局单行形态——拉平透出）；零会话返回空串（调用方整行省略）。
// progress/sentinels 段不加列（§4.2 样例无据，AC23.3 已由端点面验证——B3-7 不扩）。
// b8-W3：sentinel_last_hit_at 有值时会话态后追加「·命中Ns前」（statusHitSuffix）。
func statusSessionsCell(projects []statusProject, generatedAt string) string {
	var parts []string
	for _, p := range projects {
		for _, s := range p.Sessions {
			mark := "OK"
			if !s.Alive {
				mark = fmt.Sprintf("LOST(%dm)*", s.LostForSec/60)
			}
			parts = append(parts, s.Name+"@"+s.Column+" "+mark+statusHitSuffix(generatedAt, s.SentinelLastHitAt))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "sessions: " + strings.Join(parts, " | ")
}

// statusHitSuffix 命中后缀（b8-W3/AC8 CLI 面）：sentinel_last_hit_at 有值且
// generated_at 与之均可解析、秒差非负时返回「·命中Ns前」；无值/钟面回拨（负差）/
// 时刻串不可解析=防御性不加缀。年龄=generated_at−sentinel_last_hit_at 秒差
// （响应值同源钟——CLI 不取本地时钟，与看板徽标同口径）；固定秒粒度（「Ns 前」
// 冻结形态，不做分钟/小时换算——status 行内缀段保持可机械 grep）。
func statusHitSuffix(generatedAt, hitAt string) string {
	if hitAt == "" {
		return ""
	}
	gen, errGen := time.Parse(time.RFC3339, generatedAt)
	hit, errHit := time.Parse(time.RFC3339, hitAt)
	if errGen != nil || errHit != nil {
		return ""
	}
	age := int64(gen.Sub(hit) / time.Second)
	if age < 0 {
		return ""
	}
	return fmt.Sprintf("·命中%ds前", age)
}

// statusBlockLines block 未回执段：标题行恒在（零条也是恢复问卷「阻断未回执」
// 的答案，AC11.4），清单行两空格缩进仅 N>0 时输出（§4.2 样例形态）。
func statusBlockLines(blocks []statusBlock) []string {
	lines := []string{fmt.Sprintf("block 未回执: %d 条", len(blocks))}
	for _, b := range blocks {
		lines = append(lines, fmt.Sprintf("  #%d %s <- %s (%s)", b.Seq, b.Target, b.Sender, b.CreatedAt))
	}
	return lines
}

// statusResourcesCell resources in_use 行（§4.2 形态 `resources in_use: port=3
// account=2 data=1`）：by_type 键为服务端 rtype 规范名，展示转 CLI 简写
// （mapResourceType 逆映射——port/account_range/data_range→port/account/data），
// 固定序=样例序；未收录 rtype 原样键名字典序追加尾部（向前兼容）；空 by_type
// 以 in_use_count 兜底输出（行恒在可解析，0=无在用资源）。
func statusResourcesCell(rs statusResources) string {
	known := []struct{ key, label string }{
		{"port", "port"},
		{"account_range", "account"},
		{"data_range", "data"},
	}
	var parts []string
	for _, k := range known {
		if n, ok := rs.ByType[k.key]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", k.label, n))
		}
	}
	var rest []string
	for k := range rs.ByType {
		if !slices.ContainsFunc(known, func(x struct{ key, label string }) bool { return x.key == k }) {
			rest = append(rest, k)
		}
	}
	slices.Sort(rest)
	for _, k := range rest {
		parts = append(parts, fmt.Sprintf("%s=%d", k, rs.ByType[k]))
	}
	if len(parts) == 0 {
		parts = append(parts, strconv.FormatInt(rs.InUseCount, 10))
	}
	return "resources in_use: " + strings.Join(parts, " ")
}
