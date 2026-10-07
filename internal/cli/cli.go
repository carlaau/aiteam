package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"aiteam/internal/client"
)

// 本文件为 CLI 公共基础（B1-8）：身份四参/连接参数的全局 flag 装配与本地校验、
// 客户端构造、错误输出与退出码映射（§4.1/§4.7）、登记组与辅助命令的组级分发。
// 各命令实现在 register.go——公共参数处理抽共享函数，project/column 两族镜像
// 结构禁隐式分叉（base.md §3.4）。

// identityFlags 全局身份四参 + 连接参数（§4.1）：project/column/session/role
// 每次调用转成身份四头发服务端（隐式注册+心跳，AC12.1）；server/token 走
// 五级查找链（flag > env > 两级 cli.json > 默认，client.ResolveServer/ResolveToken）。
type identityFlags struct {
	project string
	column  string
	session string
	role    string
	server  string
	token   string
}

// registerCommonFlags 把全局 flag 挂上 fs（全部命令共用，镜像收敛于此一处）。
func registerCommonFlags(fs *flag.FlagSet, id *identityFlags) {
	fs.StringVar(&id.project, "project", "", "身份：项目 code（必填）")
	fs.StringVar(&id.column, "column", "", "身份：栏目 code（必填）")
	fs.StringVar(&id.session, "session", "", "身份：会话名（必填）")
	fs.StringVar(&id.role, "role", "", "身份：角色（必填）")
	fs.StringVar(&id.server, "server", "", "服务地址（缺省走五级查找链）")
	fs.StringVar(&id.token, "token", "", "Bearer token（缺省走五级查找链）")
}

// identityUsage 用法提示尾缀：身份四参必填的一句话说明。
const identityUsage = "身份四参 --project <code> --column <code> --session <名> --role <角色> 必填"

// newFlagSet 建命令 FlagSet：ContinueOnError（错误自输出），Usage 收敛为单行提示。
func newFlagSet(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(fs.Output(), usage) }
	return fs
}

// parseFlags 公共 flag 解析骨架：-h/--help 视为成功但不执行（proceed=false，
// 退出 0，flag 包已输出用法）；解析错误/多余位置参数为用法错误（退出 2，
// flag 包已自行输出错误与用法）；通过后统一做占位符原样复制扫描（#15，FX
// 断点④）——一处公共校验全命令生效。
func parseFlags(fs *flag.FlagSet, args []string) (proceed bool, code int) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, 0
		}
		return false, 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "ERROR: 无法识别的位置参数 %q\n", fs.Args())
		return false, 2
	}
	if err := rejectPlaceholderFlags(fs); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return false, 2
	}
	return true, 0
}

// placeholderExemptFlags 占位符扫描豁免名单（#15）：--body 消息正文与 --on-hit
// 钩子命令行同为自由文本，合法含尖括号/重定向符（「List<int> 泛型」类描述、
// `echo $AITEAM_HITS_JSON >> hits.log` 落盘类钩子）——b8-W1 落盘钩子实证：钩子值
// 含重定向符被本防线误拦（退出 2），同性质入列豁免。其余值类参数（标识/路径/
// 名目）出现尖括号即原样复制占位符的形态。
var placeholderExemptFlags = map[string]bool{"body": true, "on-hit": true}

// rejectPlaceholderFlags 占位符原样复制防线（FX 断点④·S6 用户实测）：README
// 示例以 <xxx> 标注占位，用户原样复制——Windows cmd 将 < 解析为输入重定向
// （「找不到文件」）、bash/PowerShell 下字面尖括号值静默入参错投。非布尔 flag
// 值含 < 或 > → ErrUsage 教学式报错（调用方退 2 不发请求），逐 flag 列明原值
// 并教替换。布尔 flag（IsBoolFlag）值恒 true/false 不涉占位，跳过。
func rejectPlaceholderFlags(fs *flag.FlagSet) error {
	var bad []string
	fs.Visit(func(f *flag.Flag) {
		if placeholderExemptFlags[f.Name] {
			return
		}
		if bv, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bv.IsBoolFlag() {
			return
		}
		if i := strings.IndexAny(f.Value.String(), "<>"); i >= 0 {
			bad = append(bad, fmt.Sprintf("--%s=%q", f.Name, f.Value.String()))
		}
	})
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("%w: 参数值看起来原样复制了占位符: %s——请把 <xxx> 替换为实际值（不含尖括号）",
		ErrUsage, strings.Join(bad, "; "))
}

// validateIdentity 身份四参本地校验+归一化（§4.1「CLI 本地校验缺漏=退出 2 不发
// 请求」）：TrimSpace 清洗后判空视为缺失、逐个指明缺失 flag 名；通过则返回清洗
// 后的身份四元组（后续请求头/路径拼接一律用返回值，不透传原始空白）。
func validateIdentity(id identityFlags) (client.Identity, error) {
	proj, col, sess, role :=
		strings.TrimSpace(id.project), strings.TrimSpace(id.column),
		strings.TrimSpace(id.session), strings.TrimSpace(id.role)
	var missing []string
	for _, f := range []struct{ name, val string }{
		{"--project", proj},
		{"--column", col},
		{"--session", sess},
		{"--role", role},
	} {
		if f.val == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return client.Identity{}, fmt.Errorf("%w: 缺少必填身份参数: %s", ErrUsage, strings.Join(missing, ", "))
	}
	return client.Identity{Project: proj, Column: col, Session: sess, Role: role}, nil
}

// buildClient 解析服务地址/token 并构造客户端（ident=validateIdentity 归一化产物；
// serverOverride 非空时覆盖 --server，供测试注入 httptest 地址）。地址链损坏
// （cli.json 非法）属本地配置/用法错误，包 ErrUsage 退 2（§4.7：非网络不可达、
// 非服务端拒绝，最贴近参数错误）。
func buildClient(id identityFlags, ident client.Identity, serverOverride string) (*client.Client, error) {
	serverFlag := id.server
	if serverOverride != "" {
		serverFlag = serverOverride
	}
	addr, err := client.ResolveServer(serverFlag)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUsage, err)
	}
	token, err := client.ResolveToken(id.token)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUsage, err)
	}
	return client.NewClient(addr, token, ident), nil
}

// mapExitCode 错误 → 退出码映射（§4.7）：用法错误=2（ErrUsage）；服务不可达=3
// （client.UnreachableError，errors.As 判定）；服务端拒绝=4（client.APIError）；
// 其余未知内部错误兜底=1。
func mapExitCode(err error) int {
	switch {
	case errors.Is(err, ErrUsage):
		return 2
	default:
		var ue *client.UnreachableError
		if errors.As(err, &ue) {
			return 3
		}
		var ae *client.APIError
		if errors.As(err, &ae) {
			return 4
		}
		return 1
	}
}

// fail 错误统一出口：stderr 打 `ERROR: <信息>` 并返回映射退出码。不可达错误
// 的 Error() 文本天然对齐 §4.1 形状（服务不可达 <原因>（server=<addr>））。
func fail(err error) int {
	fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
	return mapExitCode(err)
}

// ─────────────────────────────────────────────────────────────────────────────
// 组级分发（main.go 一级词入口；未实现动词报用法错误退 2，为后续批次预留扩展位）
// ─────────────────────────────────────────────────────────────────────────────

// RunProjectGroup `aiteam project <动词>` 分发（§4.3 登记组 project 族）。
func RunProjectGroup(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: aiteam project <register|update|archive|list> [参数]（"+identityUsage+"）")
		return 2
	}
	ctx := context.Background()
	switch args[0] {
	case "register":
		return runProjectRegister(ctx, args[1:], "")
	case "update":
		return runProjectUpdate(ctx, args[1:], "")
	case "archive":
		return runProjectArchive(ctx, args[1:], "")
	case "list":
		return runProjectList(ctx, args[1:], "")
	default:
		fmt.Fprintf(os.Stderr, "ERROR: 未知 project 动词 %q\n用法: aiteam project <register|update|archive|list>\n", args[0])
		return 2
	}
}

// RunColumnGroup `aiteam column <动词>` 分发（§4.3 登记组 column 族）。
func RunColumnGroup(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: aiteam column <register|update|archive|list> [参数]（"+identityUsage+"）")
		return 2
	}
	ctx := context.Background()
	switch args[0] {
	case "register":
		return runColumnRegister(ctx, args[1:], "")
	case "update":
		return runColumnUpdate(ctx, args[1:], "")
	case "archive":
		return runColumnArchive(ctx, args[1:], "")
	case "list":
		return runColumnList(ctx, args[1:], "")
	default:
		fmt.Fprintf(os.Stderr, "ERROR: 未知 column 动词 %q\n用法: aiteam column <register|update|archive|list>\n", args[0])
		return 2
	}
}

// RunSessionGroup `aiteam session <动词>` 分发（§4.6，本批仅 list）。
func RunSessionGroup(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: aiteam session list [参数]（"+identityUsage+"）")
		return 2
	}
	if args[0] != "list" {
		fmt.Fprintf(os.Stderr, "ERROR: 未知 session 动词 %q\n用法: aiteam session list\n", args[0])
		return 2
	}
	return runSessionList(context.Background(), args[1:], "")
}

// RunAudit `aiteam audit` 直接执行（§4.6 一级命令，无二级动词）。
func RunAudit(args []string) int {
	return runAudit(context.Background(), args, "")
}

// RunProgressGroup `aiteam progress [list]` 分发（FR23 + §2.2 #31/#32）：无二级
// 动词=上报（flag 直接挂），二级动词 list=查询（git stash / stash list 同型可选
// 二级动词）；其余首词落上报分支被位置参数校验拦下退 2。命令实现在 progress.go。
func RunProgressGroup(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: aiteam progress --batch <批次> --task <任务> [--commit <hash>] [--branch <分支>] [--tests pass|fail|unknown] [--summary <摘要>] | aiteam progress list [--project <code>] [--session <名>] [--limit <n>]（"+identityUsage+"）")
		return 2
	}
	if args[0] == "list" {
		return runProgressList(context.Background(), args[1:], "")
	}
	return runProgressReport(context.Background(), args, "")
}
