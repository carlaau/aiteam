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

// 本文件为资源命令族实现（B4-3，技术设计 §4.4 资源组）：register/release/list 三
// 命令 + 组级分发 RunResourceGroup。资源归属域=身份四头域一参两用（--project/
// --column 既是身份又是归属，对齐 column register 模式，公共参数处理全走
// registerCommonFlags 不复制）；type 简写映射在 CLI 侧收敛（port|account|data →
// 服务端 rtype 规范名），映射不认识的值=本地用法错误退 2 不发请求（§4.7）。

// resourceResp 资源域三端点响应 data 字段面子集（§2.2 #18/#19/#20，服务端
// resourceData 同构）：#18 登记回显与 #20 清单行同构；#19 释放回显 CLI 侧不取字段。
// ReleasedAt 未释放时服务端 omitempty 不出现，反序列化为空串。
type resourceResp struct {
	ID         int64  `json:"id"`
	Project    string `json:"project"`
	Column     string `json:"column"`
	Type       string `json:"type"`
	Value      string `json:"value"`
	Status     string `json:"status"` // in_use / released
	ReleasedAt string `json:"released_at,omitempty"`
}

type resourceListResp struct {
	Resources []resourceResp `json:"resources"`
}

// RunResourceGroup `aiteam resource <动词>` 分发（§4.4 资源组，形态对齐
// RunProjectGroup；未知动词/空参数→用法错误退 2）。
func RunResourceGroup(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: aiteam resource <register|release|list> [参数]（"+identityUsage+"）")
		return 2
	}
	ctx := context.Background()
	switch args[0] {
	case "register":
		return runResourceRegister(ctx, args[1:], "")
	case "release":
		return runResourceRelease(ctx, args[1:], "")
	case "list":
		return runResourceList(ctx, args[1:], "")
	default:
		fmt.Fprintf(os.Stderr, "ERROR: 未知 resource 动词 %q\n用法: aiteam resource <register|release|list>\n", args[0])
		return 2
	}
}

// runResourceRegister `aiteam resource register --type <port|account|data> --value <规范串> [--note <说明>]`
// +身份（§4.4）：body.column 取身份栏目（#18 归属栏目在四头 project 域内，同值
// 自洽）。输出 `OK resource=<id> type=<规范名> value=<原始串>`；服务端 409 冲突
// message 已指明冲突对象，经 fail() 原样透传退出 4（AC4.2）。
func runResourceRegister(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam resource register",
		"用法: aiteam resource register --type <port|account|data> --value <规范串> [--note <说明>] "+
			identityUsage+"（--project/--column 即归属域）")
	var id identityFlags
	var rtype, value, note string
	registerCommonFlags(fs, &id)
	fs.StringVar(&rtype, "type", "", "资源类型 port|account|data（必填）")
	fs.StringVar(&value, "value", "", "资源规范串：port=8080；段=acct:1000-1999（前缀可省）（必填）")
	fs.StringVar(&note, "note", "", "用途说明（可空）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	mapped, err := mapResourceType(rtype)
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(value) == "" {
		return fail(fmt.Errorf("%w: 缺少必填参数 --value（资源规范串）", ErrUsage))
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	var resp resourceResp
	err = c.Do(ctx, http.MethodPost, "/api/v1/resources", map[string]any{
		"column": ident.Column,
		"type":   mapped,
		"value":  strings.TrimSpace(value),
		"note":   note,
	}, &resp)
	if err != nil {
		return fail(err)
	}
	// type 输出服务端回显的规范名（=mapped，account→account_range 面上可见）。
	fmt.Fprintf(os.Stdout, "OK resource=%d type=%s value=%s\n", resp.ID, resp.Type, resp.Value)
	return 0
}

// runResourceRelease `aiteam resource release --id <n>` +身份（§4.4）：#19 写口需
// 会话身份（审计 resource.release 落操作会话）。输出 `OK released`；--id 缺失/
// 非正整数本地退 2（非数字由 flag 包解析错误退 2 同口径）。
func runResourceRelease(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam resource release",
		"用法: aiteam resource release --id <资源id> "+identityUsage)
	var id identityFlags
	var resID int
	registerCommonFlags(fs, &id)
	fs.IntVar(&resID, "id", 0, "待释放资源 id（必填正整数）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	if resID <= 0 {
		return fail(fmt.Errorf("%w: --id 须为正整数", ErrUsage))
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	if err := c.Do(ctx, http.MethodDelete,
		"/api/v1/resources/"+strconv.Itoa(resID), nil, nil); err != nil {
		return fail(err)
	}
	fmt.Fprintln(os.Stdout, "OK released")
	return 0
}

// runResourceList `aiteam resource list [--type <port|account|data>] [--all]` +身份
// （§4.4）：--project 一参两用=身份兼过滤（#20 project query 同口径，同 session
// list 模式）；--all 映射 include_released=true（默认排除已释放，AC4.1/AC4.3）。
// 一行一条经 formatResourceRow（与看板同源，AC4.4）。
func runResourceList(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam resource list",
		"用法: aiteam resource list [--type <port|account|data>] [--all] "+
			identityUsage+"（--project 兼作过滤）")
	var id identityFlags
	var rtype string
	var all bool
	registerCommonFlags(fs, &id)
	fs.StringVar(&rtype, "type", "", "按类型过滤（port|account|data，简写同 register 映射）")
	fs.BoolVar(&all, "all", false, "含已释放资源（默认仅在用）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	// --type 过滤同样接受简写映射；非法值本地退 2 不发请求。
	q := url.Values{}
	if rtype != "" {
		mapped, err := mapResourceType(rtype)
		if err != nil {
			return fail(err)
		}
		q.Set("type", mapped)
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	q.Set("project", ident.Project)
	if all {
		q.Set("include_released", "true")
	}
	var resp resourceListResp
	if err := c.Do(ctx, http.MethodGet, "/api/v1/resources?"+q.Encode(), nil, &resp); err != nil {
		return fail(err)
	}
	for _, r := range resp.Resources {
		fmt.Fprintln(os.Stdout, formatResourceRow(r))
	}
	return 0
}

// mapResourceType CLI 侧 type 简写映射（§4.4）：服务端 rtype CHECK 只认三规范名
// （port/account_range/data_range），用户面简写在 CLI 收敛；不认识的值（含规范名
// 全称）=本地用法错误退 2，不发请求。
func mapResourceType(t string) (string, error) {
	switch t {
	case "port":
		return "port", nil
	case "account":
		return "account_range", nil
	case "data":
		return "data_range", nil
	default:
		return "", fmt.Errorf("%w: --type 非法值 %q（可选 port|account|data）", ErrUsage, t)
	}
}

// formatResourceRow 清单行渲染纯函数（AC4.4 与看板同源的一行一条）：固定六列
// id type value project column status；已释放行追加 released_at 第 7 列（在用行
// released_at 为空不输出该列）。
func formatResourceRow(r resourceResp) string {
	line := fmt.Sprintf("%d %s %s %s %s %s", r.ID, r.Type, r.Value, r.Project, r.Column, r.Status)
	if r.ReleasedAt != "" {
		line += " " + r.ReleasedAt
	}
	return line
}
