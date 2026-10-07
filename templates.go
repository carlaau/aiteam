// Package aiteam 是模块根包：方法论模板 embed 载体（看板与技能包的 embed 已收
// internal/assets/；模板源 docs/planning/_template 兼具仓内文档语义，embed 只能
// 引用包内文件，留根包是最简形态）。
package aiteam

import "embed"

// TemplateFS 方法论模板包：docs/planning/_template 全部 21 文件。b10 播种口径：
// init 完整形态将本目录整目录原样播种到目标项目 docs/planning/_template/（字节
// 级搬运，占位符不渲染——模板是供复制的源），另渲染其中 15 件为实况件（onboarding
// +planning 渲染件十一件+dev-guide-base+topology 两变体），详见 internal/cli/
// init.go 的 seedTemplateDir 与 planningArtifacts。读取路径带目录前缀，init 侧
// 用 fs.Sub 剥离。
//
//go:embed docs/planning/_template
var TemplateFS embed.FS
