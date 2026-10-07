# aiteam

> 开源组合体：**多会话通讯中枢**（Go 单二进制 + SQLite，send/poll/ack/status/watch 五核心命令 + Web 看板）+ **S0~S7 多会话流水线开发方法论文档包**。MIT 协议。v0.1.0 即由该方法论完整产出（开发过程实录未随开源发布）。

## 这个仓库怎么用（两条路）

- **用产品**：安装后在**你自己的项目**里跑 `aiteam init`——方法论骨架（planning 渲染件 + `_template/` 模板全套 + `.agents/skills/` 技能包 + `.aiteam/onboarding.md`）自动落位，照 `docs/planning/_template/` 的 S0~S7 流水线走；只想当纯信箱总线用可跑 `aiteam init --comm-only`。安装与接入细节见 README。
- **改源码/做贡献**：fork 本仓；构建 `bash scripts/build.sh`；提交前双闸 `bash scripts/check.sh`（gofmt/vet/build/GOOS=linux 交叉编译/test 五节）+ `bash scripts/check-sanitize.sh --strict`（内容级脱敏扫描）；embed 耦合点：`templates.go` ← `docs/planning/_template/`、`internal/assets` ← web 看板静态源与技能包——重构目录会断 init 与看板；MIT 义务=保留 LICENSE 与版权行。

## AI 会话开工

AI 会话开工第一动作=读 `.aiteam/onboarding.md`（服务地址/身份四参/五命令/哨兵值守纪律）；参与本仓自身开发另读 `docs/planning/project-status.md` 红绿灯（角色+允许/禁止动作+冷启动引导）。

## 目录地图

```
cmd/aiteam/          入口 main
internal/server/     HTTP 服务+看板路由（32 端点）
internal/store/      SQLite 存储层（schema.sql embed）
internal/cli/        CLI 命令族（send/poll/ack/status/watch/init/progress...）
internal/client/     HTTP 客户端封装
internal/config/     配置加载（aiteam-config.json）
internal/mirror/     send 镜像审计双写
internal/types/      请求/响应结构体
internal/assets/     embed 发布资产（web 看板静态源+方法论技能包）
templates.go         模块根包：方法论模板 embed 载体（docs/planning/_template）
scripts/             check.sh/check-sanitize.sh/build.sh/export-check.sh/watchdog.sh/setup-skills/acceptance 验收脚本族
docs/planning/       _template 方法论模板（跑 init 的仓另有实况渲染件与迭代实录——发布快照不携带）
.aiteam/             接入配置（cli.json 连接+onboarding.md 上手；mirror 审计行=运行态不入库）
dist/                构建产物（gitignore，本地构建生成）
```

## 贡献最小纪律

- 提交前双闸全绿再提 PR
- commit message 用中文（项目传统）
- 逐文件 `git add`，禁 `git add -A`

<!-- aiteam:begin (do not edit between these markers) -->
## aiteam 多会话通讯接入

本项目使用 aiteam 多会话通讯：AI 会话开工第一动作=读 `.aiteam/onboarding.md`（服务地址/身份四参/五命令/哨兵值守纪律）。
<!-- aiteam:end -->
