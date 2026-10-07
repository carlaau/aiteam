// Package cli 是 aiteam CLI 层：cli.go 为公共骨架（全局身份四参/连接参数 flag
// 装配与本地校验、客户端构造、§4.7 退出码映射、组级分放入口），register.go 为
// 登记命令族（project/column 两族+session list+audit），serve.go 为 `aiteam serve`
// 服务端子命令。每命令一函数，公共参数处理收敛共享（project/column 两族镜像
// 结构禁隐式分叉）。
package cli
