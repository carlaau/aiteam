package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// 本文件为进度上报域 CLI（B8-3，FR23 手动层 + §2.2 #31/#32）：上报命令
// `aiteam progress` + 查询子命令 `aiteam progress list`。消费 B1 client 公共件
// （身份四参/五级查找链/mapExitCode 退出码映射同款，零新造）；可选字段枚举
// （--tests）与 summary 超限透传服务端 400 → 退 4，仓内惯例不本地重复校验
// （project update --heartbeat-timeout 负数透传同款口径）。
//
// 过滤 flag 与身份撞名的一参两用裁定（session list 先例）：#32 的 --project/
// --session 过滤与全局身份四参同名（Go flag 同名不可双注册），一参两用=身份值
// 兼过滤透传。代价：身份必填 → 「project 可选=全部」的跨项目巡检形态在 CLI 不可
// 达，看板直调 #32（无过滤=全部）兜底——与 audit 身份/目标分离裁定同源的规格
// 张力；CLI 主形态是操作者查自己域（AC23.2 验收原文即同域上报+list 可见）。

// ─────────────────────────────────────────────────────────────────────────────
// 响应结构（对齐 §2.2 #31/#32 字段面，只取 CLI 输出所需字段）
// ─────────────────────────────────────────────────────────────────────────────

// progressCreatedResp POST /api/v1/progress 响应 data（§2.2 #31 恰两键）。
type progressCreatedResp struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
}

// progressItem 端点 #32 列表行（§2.2 #32 冻结九键）：stale 三态不在 #32 字段面
// （仅 LatestProgressBySession 消费面，#17/B3-5），历史明细列表不做陈旧标注。
type progressItem struct {
	ID         int64  `json:"id"`
	Session    string `json:"session"`
	Batch      string `json:"batch"`
	Task       string `json:"task"`
	CommitHash string `json:"commit_hash"`
	Branch     string `json:"branch"`
	TestStatus string `json:"test_status"`
	Summary    string `json:"summary"`
	CreatedAt  string `json:"created_at"`
}

type progressListResp struct {
	Items []progressItem `json:"items"`
}

// progressListHeader list 表格表头（列=冻结九键，TestProgressList 锁定）。
const progressListHeader = "id session batch task commit_hash branch test_status summary created_at"

// ─────────────────────────────────────────────────────────────────────────────
// 上报：aiteam progress --batch <B> --task <T> [--commit] [--branch] [--tests] [--summary]
// ─────────────────────────────────────────────────────────────────────────────

// runProgressReport `aiteam progress --batch <B> --task <T> [--commit <hash>]
// [--branch <b>] [--tests pass|fail|unknown] [--summary <s>]` +身份四参（§4.2
// progress 手动层、§2.2 #31）→ POST /api/v1/progress。输出 `OK progress=<id>`
// 退出 0；4xx → 退 4+错误码透传。POST 走会话中间件链=上报即心跳（§2.2 #31）。
func runProgressReport(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam progress",
		"用法: aiteam progress --batch <批次> --task <任务> [--commit <hash>] [--branch <分支>] [--tests pass|fail|unknown] [--summary <摘要≤512B>] "+identityUsage+
			"（查询: aiteam progress list [--project <code>] [--session <名>] [--limit <n>]）")
	var id identityFlags
	var batch, task, commit, branch, tests, summary string
	registerCommonFlags(fs, &id)
	fs.StringVar(&batch, "batch", "", "批次号（必填，如 B8）")
	fs.StringVar(&task, "task", "", "任务号（必填，如 B8-3）")
	fs.StringVar(&commit, "commit", "", "commit hash（可选）")
	fs.StringVar(&branch, "branch", "", "分支名（可选）")
	fs.StringVar(&tests, "tests", "", "测试状态 pass|fail|unknown（可选，缺省=未提供）")
	fs.StringVar(&summary, "summary", "", "进度摘要，≤512 字节（可选）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	if err := requireBatchTask(batch, task, commit); err != nil {
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
	body := map[string]any{
		"batch": strings.TrimSpace(batch),
		"task":  strings.TrimSpace(task),
	}
	// 可选字段非空才放键（§2.2 #31 可选语义）；summary 不 Trim（摘要原文透传，
	// 服务端按字节校验 ≤512B）。
	if v := strings.TrimSpace(commit); v != "" {
		body["commit_hash"] = v
	}
	if v := strings.TrimSpace(branch); v != "" {
		body["branch"] = v
	}
	if v := strings.TrimSpace(tests); v != "" {
		body["test_status"] = v
	}
	if summary != "" {
		body["summary"] = summary
	}
	var resp progressCreatedResp
	if err := c.Do(ctx, http.MethodPost, "/api/v1/progress", body, &resp); err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stdout, "OK progress=%d\n", resp.ID)
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// 查询：aiteam progress list [--project] [--session] [--limit]
// ─────────────────────────────────────────────────────────────────────────────

// runProgressList `aiteam progress list [--project <code>] [--session <名>]
// [--limit <n>]` +身份四参（§2.2 #32）→ GET /api/v1/progress。人读表格输出：
// 表头+一行一条（列=冻结九键，summary 压单行截断同 audit）。过滤参数透传见
// 文件头注一参两用裁定；limit >0 才透传（audit 同款：≤0 落服务端默认 100）。
func runProgressList(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam progress list",
		"用法: aiteam progress list [--project <code>] [--session <名>] [--limit <n>] "+identityUsage+
			"（--project/--session 一参两用=身份兼过滤）")
	var id identityFlags
	var limit int
	registerCommonFlags(fs, &id)
	fs.IntVar(&limit, "limit", 0, "返回条数上限（0=服务默认 100，最大 1000 服务端钳制）")
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
	q.Set("project", ident.Project)
	if ident.Session != "" { // 身份校验后必非空，判空保持与 session list 同款防御对称
		q.Set("session", ident.Session)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	var resp progressListResp
	if err := c.Do(ctx, http.MethodGet, "/api/v1/progress?"+q.Encode(), nil, &resp); err != nil {
		return fail(err)
	}
	fmt.Fprintln(os.Stdout, progressListHeader)
	for _, r := range resp.Items {
		fmt.Fprintf(os.Stdout, "%d %s %s %s %s %s %s %s %s\n",
			r.ID, r.Session, r.Batch, r.Task, r.CommitHash, r.Branch, r.TestStatus,
			summarizeDetail(r.Summary), r.CreatedAt)
	}
	return 0
}

// requireBatchTask 上报必填两参本地校验（双层形态，b8-spec §1.1/B8-T2）：手动层
// --batch/--task 必填；--commit 非空（post-commit hook 自动层形态——commit_hash
// 自动层必有、batch/task 可空，表 11 DDL 注释口径）时豁免。commit 空且 batch/task
// 缺失按用法错误退 2 不发请求（§4.7 本地校验；TestProgressReportValidation 锁定
// 手动层语义不变）。
func requireBatchTask(batch, task, commit string) error {
	if strings.TrimSpace(commit) != "" {
		return nil // 自动层形态：hook 采值 commit 必有，batch/task 空=合法形态
	}
	var missing []string
	if strings.TrimSpace(batch) == "" {
		missing = append(missing, "--batch")
	}
	if strings.TrimSpace(task) == "" {
		missing = append(missing, "--task")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: 缺少必填参数: %s", ErrUsage, strings.Join(missing, ", "))
	}
	return nil
}
