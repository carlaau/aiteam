package cli

import (
	"fmt"
	"os"
)

// configExampleTemplate 生成模板（#11 小批派工，S6 用户验收反馈①；序 9 部署
// 结构拍平 W2 起两用）：内建默认值+每段 _doc 中文说明字段（config.Load 对未知
// 字段按惯例忽略，_doc 可留可删）。值必须与 config.Default() 一致——
// TestConfigInitGeneratesDefaults 反射对拍 json tag 全集+Default 深比较守门，
// Config 演进漏更模板此处即红。两个生成方共用本模板（单一来源）：config init
// 在 CWD 生成（人跑的维护命令）、serve 首跑在 exe 目录生成（W2）；文件名对齐
// defaultConfigFile（serve.go）。db.path 相对值由 serve 相对 exe 所在目录解析。
const configExampleTemplate = `{
  "_doc": "aiteam 服务端配置（aiteam config init / serve 首跑生成）。各 _doc 字段是说明可删；未出现的字段走内建默认。改完重启 serve 生效。",
  "listen": "0.0.0.0:8310",
  "db": {
    "_doc": "数据库：type 一期仅 sqlite；path 相对服务端 exe 所在目录（部署根），绝对路径照用；父目录自动创建。",
    "type": "sqlite",
    "path": "db/aiteam.db"
  },
  "auth": {
    "_doc": "鉴权：token_enabled=true 时 token 必填（serve 拒启校验），客户端用 --token 或 ?token= 携带。",
    "token_enabled": false,
    "token": ""
  },
  "session": {
    "_doc": "会话：心跳超时秒数，超时判会话离线（看板灰显/agent_conflict 活性判定同源）。",
    "heartbeat_timeout_sec": 900
  },
  "watch": {
    "_doc": "文件监听：watch 轮询间隔与哨兵文件超时秒数。",
    "poll_interval_sec": 5,
    "sentinel_timeout_sec": 15
  },
  "board": {
    "_doc": "看板：页面轮询间隔秒数。",
    "poll_interval_sec": 5
  },
  "progress": {
    "_doc": "进度上报：stale_after 时长字符串（超 1 倍黄标/2 倍红标）；auto_hook=init 时自动安装 post-commit hook。",
    "stale_after": "60m",
    "auto_hook": true
  },
  "timezone": "Local"
}`

// RunConfig `aiteam config <子命令>` 组入口（#11）：当前仅 init（生成配置示例）。
// 退出码分流同 backupCmd：ErrUsage → main 层退 2，其余运行错退 1。
func RunConfig(args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return fmt.Errorf("%w: 用法: aiteam config init [--force]", ErrUsage)
	}
	return runConfigInit(args[1:])
}

// runConfigInit 在 CWD 生成 aiteam-config.json 示例（默认值+_doc 说明）。幂等
// 两态：已存在拒绝覆盖（对齐 backup B6-T2 拒覆盖先例——静默冲掉用户手改配置
// 比不生成更糟；文案给 --force 出路）；--force 显式覆盖。成功后提示「改完重启
// serve 生效」（配置加载在启动期一次性完成，AC17.2）。
func runConfigInit(args []string) error {
	fs := newFlagSet("config init", "用法: aiteam config init [--force]")
	force := fs.Bool("force", false, "目标已存在时覆盖（默认拒绝覆盖）")
	if proceed, code := parseFlags(fs, args); !proceed {
		if code == 0 {
			return nil // -h/--help：用法已输出，成功退出
		}
		return fmt.Errorf("%w: 参数解析失败", ErrUsage)
	}

	path := defaultConfigFile
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("配置文件已存在，拒绝覆盖: %s（--force 覆盖，或删除后重试）", path)
	}
	if err := os.WriteFile(path, []byte(configExampleTemplate), 0o644); err != nil {
		return fmt.Errorf("写入配置文件 %s 失败: %w", path, err)
	}
	fmt.Printf("已生成 %s（内建默认值+_doc 说明，放到服务端 exe 同目录即可被 serve 自动探测）——改完重启 serve 生效\n", path)
	return nil
}
