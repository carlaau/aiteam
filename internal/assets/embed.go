// Package assets 承载运行期发布资产（看板静态源+方法论技能包）的 go:embed：
// 单二进制天然携带、拷贝即全功能。方法论模板的 embed 载体见模块根 templates.go
// （embed 只能引用包内文件，而模板源 docs/planning/_template 兼具仓内文档语义，
// 留根包引用是最简形态）。
package assets

import "embed"

// WebFS 内嵌看板静态源（web/ 目录：index.html / style.css / app.js / dialog.js，
// 目录级 embed 新增资产零改动自动进），internal/server 经 fs.Sub 剥前缀消费。

//go:embed web
var WebFS embed.FS

// SkillsFS 技能包：13 个技能目录+LICENSES.md（SKILL.md 等）。init 默认整包
// 释放到用户项目 .agents/skills/（merge 语义：同名保留用户版仅新增缺失）。
// 子树内无 ./_ 前缀文件（embed 会静默排除）。

//go:embed skills
var SkillsFS embed.FS
