package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"aiteam/internal/client"
)

// 本文件为时间窗命令族实现（B4-6，技术设计 §4.5 窗组）：set/list/now 三命令 +
// 组级分发 RunWindowGroup。身份域一参两用（--project/--column 既是身份又是目标
// 域，同 resource 组口径；now 侧身份值兼过滤）；set 另有 --project-level=
// 项目级默认窗（#21 ?scope=project，服务端不校验路径 {col} 段，填身份栏目 code
// 占位）。stage/HH:MM 本地校验快路径（判定同服务端，不合退 2 不发请求；服务端
// 校验仍是权威，错误码经 client.APIError 透传退 4）。
//
// list 数据源（B4-6b 落地，技术设计 #21 修订行冻结口径：同 path 加 GET 读当前域
// 窗配置原始行，含 enabled=false 停用窗=管理面全量）——list 两次 GET（栏目级 +
// 项目级 ?scope=project）渲染层级归属显式；set --clear --stage 单窗删除=先 GET
// 域全量、剔除目标 stage、PUT 剩余全量（先读后拼全量再 PUT）。now 仍走 #22 判定
// 态权威查询（list=管理面配置原始行，now=判定态权威查询，两读口并存分工）。

// windowSpan #22 窗快照（start>end=跨午夜原样透出，AC3.3）。
type windowSpan struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// windowEntry #22 聚合条目：window 恒在字段，未配置/停用时 null；窗内 allowed
// 与 waiting 均 window 非 null（store.ListWindowStatus 判定面，渲染据此区分
// 带窗值/无窗值）。
type windowEntry struct {
	Project string      `json:"project"`
	Column  string      `json:"column"`
	Stage   string      `json:"stage"`
	Status  string      `json:"status"` // allowed | waiting
	Window  *windowSpan `json:"window"` // nil → 未配置/停用
}

// windowsNowResp #22 响应 data 字段面（as_of 本组命令不消费，反序列化面保留）。
type windowsNowResp struct {
	AsOf    string        `json:"as_of"`
	Entries []windowEntry `json:"entries"`
}

// RunWindowGroup `aiteam window <动词>` 分发（§4.5 窗组，形态对齐 RunResourceGroup；
// 未知动词/空参数→用法错误退 2）。
func RunWindowGroup(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: aiteam window <set|list|now> [参数]（"+identityUsage+"）")
		return 2
	}
	ctx := context.Background()
	switch args[0] {
	case "set":
		return runWindowSet(ctx, args[1:], "")
	case "list":
		return runWindowList(ctx, args[1:], "")
	case "now":
		return runWindowNow(ctx, args[1:], "")
	default:
		fmt.Fprintf(os.Stderr, "ERROR: 未知 window 动词 %q\n用法: aiteam window <set|list|now>\n", args[0])
		return 2
	}
}

// runWindowSet `aiteam window set [--stage S4 --from 23:00 --to 09:00 | --clear
// [--stage S4]] [--project-level]` +身份（§4.5）：#21 覆盖式写该域全部窗（items
// 即该域全量，缺席=删除）——单窗提交与「覆盖该窗」在该域仅一窗时等价。--clear
// 两形态：不带 --stage=清空该域（PUT items:[]）；与 --stage 同给=删除该 stage
// 单窗（B4-6b #21 GET 落地解锁：先 GET 域全量、剔除目标 stage、PUT 剩余全量，
// 多窗场景不连坐其他窗；目标 stage 不在域内=剔除后与原全量相同，PUT 幂等无破坏）。
// 输出 `OK window S4=23:00-09:00 (column 05)` / `OK window S4 cleared (column 05)`
// / `OK window cleared (project)`（尾缀按写入层级）。
func runWindowSet(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam window set",
		"用法: aiteam window set --stage <S4> --from <23:00> --to <09:00> [--project-level]\n"+
			"      aiteam window set --clear [--stage <S4>] [--project-level] "+identityUsage+"（--column 即目标栏目）")
	var id identityFlags
	var stage, from, to string
	var projectLevel, clear bool
	registerCommonFlags(fs, &id)
	fs.StringVar(&stage, "stage", "", "阶段名 S+数字（如 S4；与 --clear 同给=删除该窗）")
	fs.StringVar(&from, "from", "", "窗开始 HH:MM 两位等宽（晚于 --to 即跨午夜，合法）")
	fs.StringVar(&to, "to", "", "窗结束 HH:MM 两位等宽")
	fs.BoolVar(&projectLevel, "project-level", false, "写项目级默认窗（缺省=身份栏目级）")
	fs.BoolVar(&clear, "clear", false, "清空该域全部窗；与 --stage 同给=仅删除该 stage 单窗（先读后拼全量回写）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	items := []map[string]any{} // 恒非 nil：--clear 序列化为 [] 非 null
	if clear {
		if from != "" || to != "" {
			return fail(fmt.Errorf("%w: --clear 与 --from/--to 互斥", ErrUsage))
		}
		if stage != "" {
			if !validStageFormat(stage) {
				return fail(fmt.Errorf("%w: --stage 非法值 %q（须为 S+数字，如 S4）", ErrUsage, stage))
			}
		}
	} else {
		var missing []string
		for _, f := range []struct{ name, val string }{
			{"--stage", stage}, {"--from", from}, {"--to", to},
		} {
			if f.val == "" {
				missing = append(missing, f.name)
			}
		}
		if len(missing) > 0 {
			return fail(fmt.Errorf("%w: 缺少必填参数: %s", ErrUsage, strings.Join(missing, ", ")))
		}
		if !validStageFormat(stage) {
			return fail(fmt.Errorf("%w: --stage 非法值 %q（须为 S+数字，如 S4）", ErrUsage, stage))
		}
		if !validHHMM(from) {
			return fail(fmt.Errorf("%w: --from 非法值 %q（须为 HH:MM 两位等宽，如 23:00）", ErrUsage, from))
		}
		if !validHHMM(to) {
			return fail(fmt.Errorf("%w: --to 非法值 %q（须为 HH:MM 两位等宽，如 09:00）", ErrUsage, to))
		}
		items = append(items, map[string]any{
			"stage": stage, "start": from, "end": to, "enabled": true,
		})
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	path := windowsPath(ident, projectLevel)
	if clear && stage != "" {
		// 单窗删除（#21 GET 冻结口径：先读后拼全量再 PUT）：GET 当前域全量（含
		// 停用行原样保留），剔除目标 stage 后剩余全量回写；剔除后空=items:[]。
		// GET 失败透传（退 3/4），不静默改写域。
		var cur windowsConfigResp
		if err := c.Do(ctx, http.MethodGet, path, nil, &cur); err != nil {
			return fail(err)
		}
		for _, it := range cur.Items {
			if it.Stage == stage {
				continue
			}
			items = append(items, map[string]any{
				"stage": it.Stage, "start": it.Start, "end": it.End, "enabled": it.Enabled,
			})
		}
	}
	// 回显不消费（输出由本地提交参数拼装），resp 传 nil。
	if err := c.Do(ctx, http.MethodPut, path, map[string]any{"items": items}, nil); err != nil {
		return fail(err)
	}
	scope := "column " + ident.Column
	if projectLevel {
		scope = "project"
	}
	if clear {
		if stage != "" {
			fmt.Fprintf(os.Stdout, "OK window %s cleared (%s)\n", stage, scope)
		} else {
			fmt.Fprintf(os.Stdout, "OK window cleared (%s)\n", scope)
		}
		return 0
	}
	fmt.Fprintf(os.Stdout, "OK window %s=%s-%s (%s)\n", stage, from, to, scope)
	return 0
}

// windowsConfigItem #21 GET 响应 items 条目（与 PUT 请求/响应 items 同构：
// stage/start/end/enabled——原始配置行，enabled=false 停用行原样透出）。
type windowConfigItem struct {
	Stage   string `json:"stage"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Enabled bool   `json:"enabled"`
}

// windowsConfigResp #21 GET 响应 data 字段面（两方法 data 同构）。
type windowsConfigResp struct {
	Items []windowConfigItem `json:"items"`
}

// runWindowList `aiteam window list` +身份（§4.5）：#21 GET 冻结口径（B4-6b）——
// 两次 GET 读原始配置行（栏目级 + 项目级 ?scope=project，同参语义与 PUT 一致），
// 渲染层级归属显式（项目级段前置、栏目级段随后；停用窗 (disabled) 可见）。
// 身份域一参两用口径不变：恒查身份栏目域（项目级段=同项目 scope=project 域）。
func runWindowList(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam window list",
		"用法: aiteam window list "+identityUsage)
	var id identityFlags
	registerCommonFlags(fs, &id)
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	// #21 GET 按域读：栏目级与项目级两域各一次请求（停用行走原始行不过滤）。
	var colResp, projResp windowsConfigResp
	if err := c.Do(ctx, http.MethodGet, windowsPath(ident, false), nil, &colResp); err != nil {
		return fail(err)
	}
	if err := c.Do(ctx, http.MethodGet, windowsPath(ident, true), nil, &projResp); err != nil {
		return fail(err)
	}
	for _, line := range renderConfigLines(projResp.Items, colResp.Items, ident.Column) {
		fmt.Fprintln(os.Stdout, line)
	}
	return 0
}

// runWindowNow `aiteam window now` +身份（§4.5）：#22 权威查询。按栏目分组逐行
// （一栏目一行，stage 序=entries 原序勿重排）：`05: S4=waiting(23:00-09:00) |
// S0=allowed`——window 非 nil 带窗值（窗内 allowed 同型）、nil 无窗值（AC3.4
// 未配置/停用不受限）。
func runWindowNow(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam window now",
		"用法: aiteam window now "+identityUsage+"（--project/--column 兼过滤）")
	var id identityFlags
	registerCommonFlags(fs, &id)
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	var resp windowsNowResp
	if err := c.Do(ctx, http.MethodGet, windowsNowPath(ident), nil, &resp); err != nil {
		return fail(err)
	}
	for _, line := range renderNowLines(filterColumnEntries(resp.Entries, ident.Column)) {
		fmt.Fprintln(os.Stdout, line)
	}
	return 0
}

// windowsNowPath #22 请求路径：--project 一参两用的 query 半边（身份项目恒过滤；
// #22 读口宽容，栏目维度无服务端参数，栏目过滤归 filterColumnEntries）。
func windowsNowPath(ident client.Identity) string {
	q := url.Values{}
	q.Set("project", ident.Project)
	return "/api/v1/windows/now?" + q.Encode()
}

// windowsPath #21 请求路径（set 写口与 list/clear 单窗删除读口共用）：路径段
// PathEscape（同 register.go 先例——服务端栏目/项目 code 不限字符集，只 TrimSpace
// 非空，col?1 未转义会被 URL 截断致 PathValue 失真）；projectLevel 追加
// ?scope=project（同参语义：服务端 {col} 段不参与定位）。
func windowsPath(ident client.Identity, projectLevel bool) string {
	p := "/api/v1/projects/" + url.PathEscape(ident.Project) +
		"/columns/" + url.PathEscape(ident.Column) + "/windows"
	if projectLevel {
		p += "?scope=project"
	}
	return p
}

// filterColumnEntries --column 一参两用的过滤半边（#22 无栏目维度参数，CLI 本地
// 滤；身份栏目恒非空恒过滤，同 resource list 的 query 恒传口径）。
func filterColumnEntries(entries []windowEntry, column string) []windowEntry {
	filtered := make([]windowEntry, 0, len(entries))
	for _, e := range entries {
		if e.Column == column {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// splitByColumn 按 entries 原序切分为每栏目一段（服务端升序保证同栏目相邻，线性
// 扫描不重排——裁定 5 stage 序=entries 原序）。
func splitByColumn(entries []windowEntry) [][]windowEntry {
	var groups [][]windowEntry
	for _, e := range entries {
		if n := len(groups); n > 0 && groups[n-1][0].Column == e.Column {
			groups[n-1] = append(groups[n-1], e)
			continue
		}
		groups = append(groups, []windowEntry{e})
	}
	return groups
}

// renderNowLines now 逐行渲染纯函数：每栏目一行 `<col>: <段> | <段>`，段=
// formatNowStage。
func renderNowLines(entries []windowEntry) []string {
	var lines []string
	for _, group := range splitByColumn(entries) {
		parts := make([]string, 0, len(group))
		for _, e := range group {
			parts = append(parts, formatNowStage(e))
		}
		lines = append(lines, group[0].Column+": "+strings.Join(parts, " | "))
	}
	return lines
}

// formatNowStage now 单阶段段：window 非 nil → `S4=waiting(23:00-09:00)`（窗内
// allowed 同型），nil → `S0=allowed`（未配置/停用无窗值）。
func formatNowStage(e windowEntry) string {
	if e.Window == nil {
		return e.Stage + "=" + e.Status
	}
	return fmt.Sprintf("%s=%s(%s-%s)", e.Stage, e.Status, e.Window.Start, e.Window.End)
}

// formatConfigStage #21 GET 配置段：`S4=23:00-09:00`，停用行 `(disabled)` 尾缀
// （管理面停用窗可见——#21 GET 全量口径与 #22 判定态过滤的分野）。
func formatConfigStage(it windowConfigItem) string {
	s := it.Stage + "=" + it.Start + "-" + it.End
	if !it.Enabled {
		s += "(disabled)"
	}
	return s
}

// joinConfigStages 段串接（items 原序=服务端 stage 升序勿重排）；空集返回空串
// （调用方据此省略整行）。
func joinConfigStages(items []windowConfigItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, formatConfigStage(it))
	}
	return strings.Join(parts, " | ")
}

// renderConfigLines list 逐行渲染纯函数（#21 GET 冻结口径，层级归属显式）：
// 项目级段 `project: <段> | ...` 前置（无项目级窗时整段省略），栏目级段
// `<col>: <段> | ...` 随后（空域 `<col>: (未配置)`）。
func renderConfigLines(projectItems, columnItems []windowConfigItem, column string) []string {
	var lines []string
	if seg := joinConfigStages(projectItems); seg != "" {
		lines = append(lines, "project: "+seg)
	}
	if seg := joinConfigStages(columnItems); seg != "" {
		lines = append(lines, column+": "+seg)
	} else {
		lines = append(lines, column+": (未配置)")
	}
	return lines
}

// validStageFormat stage 本地校验：^S\d+$（S9/S10 等两位数收；"S"/"X4"/空串拒）
// ——与服务端 validStageFormat 同判据（server 包小写不导出，CLI 侧自持同逻辑）。
func validStageFormat(stage string) bool {
	if len(stage) < 2 || stage[0] != 'S' {
		return false
	}
	for i := 1; i < len(stage); i++ {
		if stage[i] < '0' || stage[i] > '9' {
			return false
		}
	}
	return true
}

// validHHMM HH:MM 两位等宽本地校验（小时 00-23、分钟 00-59；反例 9:00/25:00/
// 23:5/0900 一律拒）——与服务端 validHHMM 同判据。start>end 是跨午夜语义，此处
// 不比大小（AC3.3）。
func validHHMM(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	hh, mm := s[0:2], s[3:5]
	for i := range 2 {
		if hh[i] < '0' || hh[i] > '9' || mm[i] < '0' || mm[i] > '9' {
			return false
		}
	}
	return hh <= "23" && mm <= "59" // 等宽两位数字串字典序=数值序
}
