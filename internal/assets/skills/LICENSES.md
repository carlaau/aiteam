# skills/ 许可与来源说明

本目录共 13 个技能，服务 aiteam 的 S0~S7 流水线方法论。取材自三个来源仓（均以 MIT 发布，与本仓协议兼容），逐技能来源与改写程度如下：

| # | 技能 | 来源 | 改写程度 |
|---|------|------|---------|
| 1 | brainstorming | [superpowers-zh](https://github.com/jnMetaCode/superpowers-zh)（中文社区版；上游 [obra/superpowers](https://github.com/obra/superpowers)） | 最小脱敏（业务示例改通用表述） |
| 2 | writing-plans | 同上（superpowers-zh） | 原样移植 |
| 3 | subagent-driven-development | 同上（superpowers-zh） | 原样移植 |
| 4 | test-driven-development | 同上（superpowers-zh） | 原样移植 |
| 5 | requesting-code-review | 同上（superpowers-zh） | 原样移植 |
| 6 | receiving-code-review | 同上（superpowers-zh） | 原样移植 |
| 7 | finishing-a-development-branch | 同上（superpowers-zh） | 原样移植 |
| 8 | using-git-worktrees | 同上（superpowers-zh） | 原样移植 |
| 9 | verification-before-completion | 同上（superpowers-zh） | 原样移植 |
| 10 | systematic-debugging | 同上（superpowers-zh） | 原样移植 |
| 11 | requirement-grilling | [mattpocock-skills-zh-CN](https://github.com/vinvcn/mattpocock-skills-zh-CN) 的 grilling 访谈方法论（上游 [mattpocock/skills](https://github.com/mattpocock/skills)） | 通用化改写（见下） |
| 12 | chinese-code-review | [superpowers-zh](https://github.com/jnMetaCode/superpowers-zh) 中国原创技能区 | 原样移植 |
| 13 | spec-review | aiteam 原创 | 通用化改写（见下） |

## 合规说明

- 三个来源仓均以 MIT 发布，允许再分发与改写；本包按 MIT 要求保留来源署名（各技能文末「> 来源」行 + 本文件汇总表）。
- superpowers 系（#1~#10、#12）正文为中文，实际取材自汉化仓 superpowers-zh——汉化作者的译文同样以 MIT 授权，署名指向汉化仓并链至上游英文原仓；如需英文原版与辅助文件（visual-companion / 提示词模板等），见上游 [obra/superpowers](https://github.com/obra/superpowers)。
- requirement-grilling 的追问式方法论本体（决策树分支穷举 / frontier 逐轮推进 / 每问附推荐答案 / facts 与 decisions 分离 / 反前提值来源穷举 / 落盘产出 / 交棒 brainstorming）源自 mattpocock 的 grilling 系技能（经中文版仓取材），aiteam 侧改写仅限路径占位化与示例通用化。

## 移植范围说明

- 每个技能目录只移植 `SKILL.md` 本身（保持最小面）。源技能目录中的辅助文件
  （如 brainstorming 的 `visual-companion.md`、subagent-driven-development 的
  `implementer-prompt.md` 等提示词模板、systematic-debugging 的 `root-cause-tracing.md`
  等辅助技术文档、requesting-code-review 引用的 `code-reviewer.md`、
  test-driven-development 引用的 `testing-anti-patterns.md`）**未随包移植**；
  正文中的对应引用保留原文或已加注说明，需要时可自上游仓库获取。
- superpowers 系技能正文中的方法论体系落位（如 specs/<迭代>/ 目录体系、
  spec/plan 同居+INDEX 统一登记）为方法论的默认落盘位置约定，使用时以各自项目的实际目录为准。
- 明确不含 executing-plans 技能（aiteam 契约禁止直干模式，故意排除）。

## 改写明细

- **brainstorming**（最小脱敏）：1 处业务示例改通用表述（「生成报价单」→「生成订单报告」）；
  文末补出处署名。
- **requirement-grilling**（通用化改写）：spec 落盘路径改 `{{SPECS_DIR}}` 占位；参考来源
  落实为 mattpocock-skills-zh-CN 的 grilling 系技能；字典/样本/术语表等 facts 自查清单改
  通用表述；反前提铁案与每问格式示例、边界场景示例、术语冲突示例换为电商/通用业务例子；
  「栏目制」表述改通用的「上位规划/模块」语境；文首来源行如实标注。追问式方法论本体完整
  保留（见合规说明第三条）。
- **spec-review**（通用化改写，aiteam 原创）：五项检查中的项目规范文件路径改为「项目自己
  的工程规范文档」类占位表述；示例换通用例子；「栏目制」「C1~C8」等内部制度黑话改通用
  表述。五项检查方法论（既有能力复用 / 反前提 / 约定合规 / 事实核对 / 四节完备 / 过渡态
  风险）为 aiteam 原创设计。
- **chinese-code-review**：原样移植自 superpowers-zh 中国原创技能区（仅文首来源行按本仓
  口径标注）。
