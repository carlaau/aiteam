// Command aiteam 是多会话通讯中枢的唯一入口，按子命令分发（§9.5：flag 包+手写分发）。
package main

import (
	"errors"
	"fmt"
	"os"

	"aiteam/internal/cli"
	"aiteam/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		serveCmd(os.Args[2:])
	case "init":
		initCmd(os.Args[2:])
	case "backup":
		backupCmd(os.Args[2:])
	case "config":
		// 配置面（#11，S6 用户验收反馈）：config init 一键生成配置示例
		configCmd(os.Args[2:])
	case "version":
		// §4.6 辅助命令：stdout 一行版本信息，成功退出 0（§4.7）
		fmt.Printf("aiteam version %s\n", server.Version)
	case "send":
		// §4.2 五核心 send（B2-6）：目标三形态+镜像双写，退出码映射在 cli 内
		// 完成（§4.7）
		os.Exit(cli.RunSend(os.Args[2:]))
	case "poll":
		// §4.2 五核心 poll（B2-7）：GET #10 幂等重拉+五要素渲染
		os.Exit(cli.RunPoll(os.Args[2:]))
	case "ack":
		// §4.2 五核心 ack（B2-7）：#11 位点推进+#12 逐条回执
		os.Exit(cli.RunAck(os.Args[2:]))
	case "history":
		// §4.6 辅助：history（#13 通用历史查询，含回执状态列）
		os.Exit(cli.RunHistory(os.Args[2:]))
	case "project":
		// §4.3 登记组 project 族（B1-8）：退出码映射在 cli 组入口内完成（§4.7）
		os.Exit(cli.RunProjectGroup(os.Args[2:]))
	case "column":
		// §4.3 登记组 column 族
		os.Exit(cli.RunColumnGroup(os.Args[2:]))
	case "session":
		// §4.6 辅助：session list（心跳面）
		os.Exit(cli.RunSessionGroup(os.Args[2:]))
	case "audit":
		// §4.6 辅助：audit（留痕查询）
		os.Exit(cli.RunAudit(os.Args[2:]))
	case "progress":
		// 进度上报域（B8-3，FR23 手动层+B8-5 hook 自动层共用命令面）：上报与
		// list 查询两形态，退出码映射在组入口内完成（§4.7）。接线原留 B8-7 收口，
		// B8-5 hook 真跑用例（AC23.1 端到端）硬依赖命令可达而预支。
		os.Exit(cli.RunProgressGroup(os.Args[2:]))
	case "resource":
		// §4.4 资源组（B4）：register/release/list
		os.Exit(cli.RunResourceGroup(os.Args[2:]))
	case "window":
		// §4.5 时间窗组（B4）：set/list/now
		os.Exit(cli.RunWindowGroup(os.Args[2:]))
	case "watch":
		// 哨兵值守 <信箱轮询>（B3 范围）：注册→轮询→命中/收尾注销
		os.Exit(cli.RunWatch(os.Args[2:]))
	case "status":
		// §4.2 观测命令（B3 范围）：单栏目四项（缺省）/全局一行式（--global）
		os.Exit(cli.RunStatus(os.Args[2:]))
	default:
		usage()
	}
}

// initCmd 在目标项目目录生成方法论脚手架：错误分流同 serve（§4.7）。
func initCmd(args []string) {
	if err := cli.RunInit(args); err != nil {
		fmt.Fprintf(os.Stderr, "aiteam init: %v\n", err)
		if errors.Is(err, cli.ErrUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// serveCmd 启动服务端：错误按 §4.7 分流——用法错退出 2，其余运行错退出 1。
func serveCmd(args []string) {
	if err := cli.RunServe(args); err != nil {
		fmt.Fprintf(os.Stderr, "aiteam serve: %v\n", err)
		if errors.Is(err, cli.ErrUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// backupCmd 备份源库（§12 D6 VACUUM INTO 在线热备）：错误分流同 serveCmd——
// 用法错退出 2（ErrUsage），其余运行错退出 1。
func backupCmd(args []string) {
	if err := cli.RunBackup(args); err != nil {
		fmt.Fprintf(os.Stderr, "aiteam backup: %v\n", err)
		if errors.Is(err, cli.ErrUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// configCmd 配置面（#11）：config init 生成配置示例。错误分流同 backupCmd。
func configCmd(args []string) {
	if err := cli.RunConfig(args); err != nil {
		fmt.Fprintf(os.Stderr, "aiteam config: %v\n", err)
		if errors.Is(err, cli.ErrUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// usage 打印用法说明后退出 2（§4.7：参数/用法错误）。
func usage() {
	fmt.Fprintf(os.Stderr, `用法: aiteam <子命令> [参数]

子命令:
  serve     启动服务端 [--config <路径>]
  init      生成脚手架 --project <code> --name <名称> [--comm-only 只装通讯接入] [--skills] [--vcs git|svn]
  backup    备份源库 --out <路径> [--db <路径>] [--config <路径>]
  config    配置面 <init>（生成 aiteam-config.json 示例 [--force]）
  version   打印版本信息
  send      发送消息（五核心，B2 范围）
  poll      拉取信箱与对话（五核心，B2 范围）
  ack       推进消费位点+block 回执（五核心，B2 范围）
  history   通用历史查询（B2 范围）
  project   登记组 project 族 <register|update|archive|list>（B1 范围）
  column    登记组 column 族 <register|update|archive|list>（B1 范围）
  session   会话心跳面 <list>（B1 范围）
  audit     审计留痕查询（B1 范围）
  progress  进度上报 <上报|list>（FR23：手动层 --batch/--task，hook 自动层 --commit/--branch）
  resource  资源组 <register|release|list>（B4 范围）
  window    时间窗组 <set|list|now>（B4 范围）
  watch     哨兵值守 <信箱轮询>（B3 范围）
  status    状态观测 <单栏目|global>（B3 范围）
`)
	os.Exit(2)
}
