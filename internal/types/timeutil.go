// Package types 定义 API 请求/响应结构体（server 与 client 共用）。
package types

import "time"

// timeLayoutISO8601Z 服务端时间文本统一格式：ISO8601 UTC 秒级（技术设计 §3.1 第 1 条）。
// 字典序=时间序，直接用于比较/排序/索引。
const timeLayoutISO8601Z = "2006-01-02T15:04:05Z"

// NowUTC 服务端时间注入点：一律服务端进程生成（AC5.4 多机时钟免疫），不使用客户端传入时间。
// 包级变量以便测试替换注入固定时间；业务代码勿在运行期改写。
var NowUTC = func() string {
	return time.Now().UTC().Format(timeLayoutISO8601Z)
}
