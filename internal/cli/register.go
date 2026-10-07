package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// 本文件为登记命令族实现（B1-8，技术设计 §4.3 登记组 + §4.6 辅助命令）：
// project/column 两族八命令 + session list + audit，共十个命令函数。
//
// 参数面裁定（b1-spec 死锁的工程解，测试与实现一致）：心跳中间件要求身份四头的
// project/column 已登记（否则 404 短路），而登记命令的目标实体尚未存在——身份与
// 目标必须分离。全局身份四参（--project/--column/--session/--role，§4.1）表达
// 操作者所在域；目标实体统一用 --code flag：
//   - project register/update/archive --code <待操作项目code>
//   - column register/update/archive --project <父项目> --code <待操作栏目code>
//
// column 族 --project 一参两用=身份 project+父项目（同值自洽，无死锁）；column
// update/archive 的 --code 目标已登记，身份栏目仍挂操作者自身域。查询命令按实际
// 行为分口径：session list 的 --project 兼作过滤（端点 #27 同口径）；audit 的
// --project 仅作身份、查询不带项目过滤——登记目标常是异项目，按身份过滤会滤掉其
// 留痕（AC1.4 可查性优先，规格 [--project] 过滤语义的取舍已随死锁上报总控）。
// 该偏离已随死锁一并上报总控裁定。

// ─────────────────────────────────────────────────────────────────────────────
// 响应结构（对齐 §2.2 端点字段面，只取 CLI 输出所需字段）
// ─────────────────────────────────────────────────────────────────────────────

// projectResp POST/PATCH /api/v1/projects 响应 data（§2.2 #1/#2 字段面子集）。
type projectResp struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
}

// archiveResp DELETE 响应 data（§2.2 #3/#7：恰 {status:"archived"}）。
type archiveResp struct {
	Status string `json:"status"`
}

// projectRow 端点 #4 列表行（§2.2 #4 字段面子集）。
type projectRow struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	ColumnsCount int    `json:"columns_count"`
}

type projectListResp struct {
	Projects []projectRow `json:"projects"`
}

// columnResp 端点 #5/#6 响应 data 与 #8 列表行（§2.2 字段面子集）。
type columnResp struct {
	ID     int64  `json:"id"`
	Code   string `json:"code"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type columnListResp struct {
	Columns []columnResp `json:"columns"`
}

// sessionRow 端点 #27 列表行（§2.2 #27 字段面子集：name/role/alive/last_seen_at）。
type sessionRow struct {
	Name       string `json:"name"`
	Role       string `json:"role"`
	LastSeenAt string `json:"last_seen_at"`
	Alive      bool   `json:"alive"`
}

type sessionListResp struct {
	Sessions []sessionRow `json:"sessions"`
}

// auditRow 端点 #28 列表行（§2.2 #28 字段面子集）。
type auditRow struct {
	ID        int64  `json:"id"`
	Action    string `json:"action"`
	Session   int64  `json:"session"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"created_at"`
}

type auditListResp struct {
	Entries []auditRow `json:"entries"`
}

// ─────────────────────────────────────────────────────────────────────────────
// project 族（§4.3：register/update/archive/list，与 column 族镜像结构）
// ─────────────────────────────────────────────────────────────────────────────

// runProjectRegister `aiteam project register --code <c> [--name <n>] [--heartbeat-timeout <sec>]`
// +身份（§4.3；--code=待登记项目 code，死锁裁定见文件头注）。输出 `OK project=<code> id=<n>`。
func runProjectRegister(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam project register",
		"用法: aiteam project register --code <项目code> [--name <名>] [--heartbeat-timeout <秒>] "+identityUsage)
	var id identityFlags
	var code, name string
	var timeout int
	registerCommonFlags(fs, &id)
	fs.StringVar(&code, "code", "", "待登记项目 code（必填）")
	fs.StringVar(&name, "name", "", "项目显示名（缺省=code）")
	fs.IntVar(&timeout, "heartbeat-timeout", 0, "项目级失联阈值秒数（0=服务默认 900）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	if err := requireCode(code); err != nil {
		return fail(err)
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	// name 缺省=code（§4.3 flag 文本承诺，落库面落实防空名行）。
	if name == "" {
		name = strings.TrimSpace(code)
	}
	var resp projectResp
	err = c.Do(ctx, http.MethodPost, "/api/v1/projects", map[string]any{
		"code":                  strings.TrimSpace(code),
		"name":                  name,
		"heartbeat_timeout_sec": timeout,
	}, &resp)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stdout, "OK project=%s id=%d\n", resp.Code, resp.ID)
	return 0
}

// runProjectUpdate `aiteam project update --code <c> [--name] [--heartbeat-timeout]`
// +身份（§4.3）：至少一项由服务端校验（空更新 400 → 退出 4）。输出 `OK`。
func runProjectUpdate(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam project update",
		"用法: aiteam project update --code <项目code> [--name <名>] [--heartbeat-timeout <秒>] "+identityUsage)
	var id identityFlags
	var code, name string
	var timeout int
	registerCommonFlags(fs, &id)
	fs.StringVar(&code, "code", "", "待更新项目 code（必填）")
	fs.StringVar(&name, "name", "", "新项目显示名")
	fs.IntVar(&timeout, "heartbeat-timeout", 0, "新失联阈值秒数（0=不更新）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	if err := requireCode(code); err != nil {
		return fail(err)
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	body := map[string]any{}
	if name != "" {
		body["name"] = name
	}
	// --heartbeat-timeout 显式提供且非 0 才放键（0=未提供走默认，服务端同语义；
	// 负数透传交服务端 400 param_invalid 拒绝，不静默吞——防「看似成功实未生效」）。
	if timeout != 0 {
		body["heartbeat_timeout_sec"] = timeout
	}
	var resp projectResp
	err = c.Do(ctx, http.MethodPatch, "/api/v1/projects/"+url.PathEscape(strings.TrimSpace(code)), body, &resp)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintln(os.Stdout, "OK")
	return 0
}

// runProjectArchive `aiteam project archive --code <c>` +身份（§4.3 软删）。
// 输出 `OK archived`。
func runProjectArchive(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam project archive",
		"用法: aiteam project archive --code <项目code> "+identityUsage)
	var id identityFlags
	var code string
	registerCommonFlags(fs, &id)
	fs.StringVar(&code, "code", "", "待归档项目 code（必填）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	if err := requireCode(code); err != nil {
		return fail(err)
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	var resp archiveResp
	err = c.Do(ctx, http.MethodDelete, "/api/v1/projects/"+url.PathEscape(strings.TrimSpace(code)), nil, &resp)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintln(os.Stdout, "OK archived")
	return 0
}

// runProjectList `aiteam project list [--all]` +身份（§4.3）：一行一项目，格式
// `<code> <name> <status> <栏目数>`（空格分隔，register_test 锁定；§2.2 #4 同源）。
func runProjectList(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam project list",
		"用法: aiteam project list [--all] "+identityUsage)
	var id identityFlags
	var all bool
	registerCommonFlags(fs, &id)
	fs.BoolVar(&all, "all", false, "含已归档项目（默认仅 active）")
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
	var resp projectListResp
	path := "/api/v1/projects"
	if all {
		path += "?include_archived=true"
	}
	if err := c.Do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return fail(err)
	}
	for _, r := range resp.Projects {
		fmt.Fprintf(os.Stdout, "%s %s %s %d\n", r.Code, r.Name, r.Status, r.ColumnsCount)
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// column 族（§4.3：与 project 族镜像结构，base.md §3.4 禁隐式分叉）
// ─────────────────────────────────────────────────────────────────────────────

// runColumnRegister `aiteam column register --project <P> --code <c> [--name]`
// +身份（§4.3；--project 一参两用=身份 project+父项目，--code=待登记栏目 code）。
// 输出 `OK column=<code> id=<n>`。
func runColumnRegister(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam column register",
		"用法: aiteam column register --project <父项目code> --code <栏目code> [--name <名>] "+identityUsage)
	var id identityFlags
	var code, name string
	registerCommonFlags(fs, &id)
	fs.StringVar(&code, "code", "", "待登记栏目 code（必填）")
	fs.StringVar(&name, "name", "", "栏目显示名（缺省=code）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	if err := requireCode(code); err != nil {
		return fail(err)
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	// name 缺省=code（§4.3 flag 文本承诺，落库面落实防空名行）。
	if name == "" {
		name = strings.TrimSpace(code)
	}
	var resp columnResp
	err = c.Do(ctx, http.MethodPost,
		"/api/v1/projects/"+url.PathEscape(ident.Project)+"/columns",
		map[string]any{
			"code": strings.TrimSpace(code),
			"name": name,
		}, &resp)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stdout, "OK column=%s id=%d\n", resp.Code, resp.ID)
	return 0
}

// runColumnUpdate `aiteam column update --project <P> --code <c> [--name]`
// +身份（§4.3 同型 project update）。输出 `OK`。
func runColumnUpdate(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam column update",
		"用法: aiteam column update --project <父项目code> --code <栏目code> [--name <名>] "+identityUsage)
	var id identityFlags
	var code, name string
	registerCommonFlags(fs, &id)
	fs.StringVar(&code, "code", "", "待更新栏目 code（必填）")
	fs.StringVar(&name, "name", "", "新栏目显示名")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	if err := requireCode(code); err != nil {
		return fail(err)
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	body := map[string]any{}
	if name != "" {
		body["name"] = name
	}
	var resp columnResp
	err = c.Do(ctx, http.MethodPatch, columnPath(ident.Project, code), body, &resp)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintln(os.Stdout, "OK")
	return 0
}

// runColumnArchive `aiteam column archive --project <P> --code <c>` +身份（§4.3）。
// 输出 `OK archived`。
func runColumnArchive(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam column archive",
		"用法: aiteam column archive --project <父项目code> --code <栏目code> "+identityUsage)
	var id identityFlags
	var code string
	registerCommonFlags(fs, &id)
	fs.StringVar(&code, "code", "", "待归档栏目 code（必填）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	if err := requireCode(code); err != nil {
		return fail(err)
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	var resp archiveResp
	err = c.Do(ctx, http.MethodDelete, columnPath(ident.Project, code), nil, &resp)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintln(os.Stdout, "OK archived")
	return 0
}

// runColumnList `aiteam column list --project <P> [--all]` +身份（§4.3 同型）：
// 一行一栏目，格式 `<code> <name> <status>`。
func runColumnList(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam column list",
		"用法: aiteam column list --project <父项目code> [--all] "+identityUsage)
	var id identityFlags
	var all bool
	registerCommonFlags(fs, &id)
	fs.BoolVar(&all, "all", false, "含已归档栏目（默认仅 active）")
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
	var resp columnListResp
	path := columnPath(ident.Project, "")
	if all {
		path += "?include_archived=true"
	}
	if err := c.Do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return fail(err)
	}
	for _, r := range resp.Columns {
		fmt.Fprintf(os.Stdout, "%s %s %s\n", r.Code, r.Name, r.Status)
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// 辅助命令（§4.6：session list / audit）
// ─────────────────────────────────────────────────────────────────────────────

// runSessionList `aiteam session list` +身份（§4.6 会话心跳面）：--project 一参
// 两用=身份 project+过滤（端点 #27 同口径）。一行一会话：name role alive last_seen_at
// （对齐 #27 响应字段，register_test 锁定）。
func runSessionList(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam session list",
		"用法: aiteam session list "+identityUsage+"（--project 兼作过滤）")
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
	// 过滤参数 url.Values 统一编码（与 audit 同型，Minor 6）。
	q := url.Values{}
	q.Set("project", ident.Project)
	var resp sessionListResp
	if err := c.Do(ctx, http.MethodGet, "/api/v1/sessions?"+q.Encode(), nil, &resp); err != nil {
		return fail(err)
	}
	for _, r := range resp.Sessions {
		fmt.Fprintf(os.Stdout, "%s %s %t %s\n", r.Name, r.Role, r.Alive, r.LastSeenAt)
	}
	return 0
}

// runAudit `aiteam audit [--action <a>] [--limit <n>]` +身份（§4.6 审计查询）：
// 一行一条：id action session detail摘要 created_at。--project 仅作身份（必填），
// 查询不带项目过滤——登记动作的目标实体常是另一个项目（身份/目标分离，见文件
// 头注），按身份项目过滤会滤掉其留痕（AC1.4 可查性优先于过滤维度，规格
// `[--project]` 过滤语义在身份强校验下的取舍已随死锁上报总控）。
func runAudit(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam audit",
		"用法: aiteam audit [--action <动作>] [--limit <条数>] "+identityUsage)
	var id identityFlags
	var action string
	var limit int
	registerCommonFlags(fs, &id)
	fs.StringVar(&action, "action", "", "按动作精确过滤（如 project.register）")
	fs.IntVar(&limit, "limit", 0, "返回条数上限（0=服务默认 100，最大 1000）")
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
	q := url.Values{}
	if action != "" {
		q.Set("action", action)
	}
	// limit >0 才透传；≤0 本地转服务端默认 100（与 flag 文本「0=服务默认 100」
	// 承诺一致，负数同样落默认不做二次报错）。
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	path := "/api/v1/audit"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var resp auditListResp
	if err := c.Do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return fail(err)
	}
	for _, e := range resp.Entries {
		fmt.Fprintf(os.Stdout, "%d %s %d %s %s\n", e.ID, e.Action, e.Session, summarizeDetail(e.Detail), e.CreatedAt)
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// 本文件私有 helper
// ─────────────────────────────────────────────────────────────────────────────

// columnPath 栏目域 API 路径：colCode 空=列表（#8），非空=单栏目（#6/#7）。
func columnPath(projectCode, colCode string) string {
	p := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectCode)) + "/columns"
	if colCode != "" {
		p += "/" + url.PathEscape(strings.TrimSpace(colCode))
	}
	return p
}

// requireCode 目标实体 --code 本地校验：缺失按用法错误退 2（§4.7 本地校验，
// 不发请求）。
func requireCode(code string) error {
	if strings.TrimSpace(code) == "" {
		return fmt.Errorf("%w: 缺少必填参数 --code（目标实体 code）", ErrUsage)
	}
	return nil
}

// summarizeDetail 审计 detail JSON 摘要：压成单行、超 48 rune 截断（§4.6 一行
// 一条的 detail 摘要列；rune 安全截断防切碎多字节中文）。
func summarizeDetail(detail string) string {
	line := strings.Join(strings.Fields(detail), " ")
	if runes := []rune(line); len(runes) > 48 {
		return string(runes[:48]) + "…"
	}
	return line
}
