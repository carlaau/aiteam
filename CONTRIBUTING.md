# 贡献指南

感谢关注 aiteam！本文面向想动手改这个项目的人类贡献者，给出从「我想改点东西」到「PR 合入」的完整动线。

三份门面分工：装与用（使用者视角）看 [README](README.md)；AI 会话接入（开工与纪律）看 [AGENTS.md](AGENTS.md)；本文只管「改与提」，只做指针与补充，不重复上述两文正文。

## 贡献流程

1. **先议后做**：新功能或行为变更先开 GitHub issue 讨论（问题边界、实现思路），对齐后再动手；明确的小缺陷可先 issue 报告再跟进修复。
2. **fork 本仓**，从主分支拉出 `feat/<主题>` 分支（如 `feat/my-feature`）；一个分支装一件事，保持 PR 小而聚焦、便于评审。
3. **本地验证**：提交前跑双闸并全绿（见下节）——这是 PR 能合入的机械前提。
4. **提 PR**：指向本仓主分支；描述里写清改了什么、为什么改、如何验证，能关联对应 issue 就关联。

报 issue 建议带齐四样：做了什么操作、期望什么、实际发生什么、环境信息（平台 / 版本号），有报错原文或最小复现步骤更佳。

不确定从哪下手？issue 列表里的文档纠错、用例补全、小缺陷修复都是低门槛起点；动手前在 issue 里说一声「我来认领」，避免与人撞车。

提交纪律（完整表述见 [AGENTS.md](AGENTS.md)「贡献最小纪律」）：commit message 用中文（项目传统）；逐文件 `git add`，禁 `git add -A`。

评审与合入：维护者评审后可能要求修改，按意见在同一分支追加提交即可（PR 会自动带上）；合并方式由维护者裁定，无需自行处理主分支。

## 开发环境与验证

- 工具链：Go 1.22+（自行编译的前置要求，见 README「前置要求」）；验证脚本均为 bash，Windows 上用 Git Bash 即可跑。
- `scripts/check.sh` —— 提交前一键检查五节：gofmt → go vet → go build → GOOS=linux 交叉编译 → go test；任一节失败即退出，全过输出 ALL GREEN。
- `scripts/check-sanitize.sh --strict` —— 全仓内容级脱敏机械闸：按规则正则扫描（私有 IP、盘符路径、用户目录等字面），任何命中即阻塞；`--strict` 为忽略白名单的全量扫描口径。
- `scripts/build.sh` —— 五平台交叉编译发布脚本（windows/amd64 + linux amd64/arm64 + darwin amd64/arm64）：产出 `dist/` 五个单二进制与 SHA256SUMS 校验和，版本号由 git describe 注入（可用环境变量 `VER` 覆盖）。

- 行尾是全仓 LF 硬口径（载体为 `.gitattributes`），编辑器注意别引入 CRLF，否则 gofmt 节会先卡住。

改动的快速验证回路：

- 改 Go 代码：`go test ./...` 先跑单节快速回路，收工前再跑整闸 `check.sh`。
- 改文档或模板：跑 `check-sanitize.sh --strict`；涉及 embed 面（方法论模板 / 看板静态源 / 技能包）的改动，追加跑一次 `build.sh` 确认产物正常。
- 本地快速构建与调试起服务的注意事项 → README「获取二进制」一节（含 `go run` 起 serve 的陷阱提示）。

## 仓库地图导航

只指路不复制，按需跳转：

- 各目录职责一览 → [AGENTS.md](AGENTS.md)「目录地图」。
- 本仓两条用法（用产品 / 改源码做贡献）与 embed 耦合点警示（动 `templates.go`、`internal/assets` 对应源目录前必读）→ [AGENTS.md](AGENTS.md)「这个仓库怎么用（两条路）」。
- AI 会话接入手续 → [AGENTS.md](AGENTS.md)「AI 会话开工」。
- 多机部署、配置字段与 CLI 命令参考 → [README](README.md) 文末「文档」一节所列指南。
- 方法论模板本体（`aiteam init` 播种进你项目的那些文件）→ `docs/planning/_template/`。

## 行为底线

- **脱敏零容忍**：公开发布面（源码、文档、示例文本）禁止出现内网 IP、盘符路径、用户目录、私有项目名的字面——脱敏闸命中即卡，提 PR 前请自跑 `bash scripts/check-sanitize.sh --strict` 确认输出 PASS。
- **MIT 义务**：本项目以 MIT 协议开源，贡献内容同样按 MIT 发布；请保留 [LICENSE](LICENSE) 文件与源码版权行，协议全文见 [LICENSE](LICENSE) 与 [README §协议](README.md#协议)。

---

> 三份门面若出现口径出入，以 README / AGENTS.md 正文与各脚本头注释为准；欢迎开 issue 指出，我们会回来修这份指南。
