# `_template/` 模板目录与占位符规范

> 本目录 = S0~S7 多会话流水线方法论文档包模板集：任何团队复制本目录到自身仓库 `docs/planning/_template/`，按各模板头部「用法」替换占位符、删除指导注释，即得自身项目的规划文档包。
> **本 README 是全部模板占位符的权威规范**：语法、保留字清单、扩展规则、禁残留清单四节；新占位符必须先登记回 §三，未登记占位符视同违规（由 `scripts/check-sanitize.sh` 机械校验）。模板源自真实项目全流程实践（S0~S7）的通用化提炼。

## 一、模板清单

| 文件 | 用途（一句话） |
|---|---|
| `README.md` | 本规范：占位符语法/保留字/扩展规则/禁残留清单 |
| `s0-s7-stages.md` | S0~S7 七阶段总纲模板（每阶段：目标/进出条件/产物清单/动作要领；S4 重点展开四循环） |
| `pipeline-overview.md` | 流水线总纲手册：S0~S7 串成一本（角色契约/阶段总览/S4 编排与审循环/并行纪律/时间盒与 token 经济/文档元规则），任何会话的总入口 |
| `project-status.md` | 开发红绿灯：新会话开工第一读（当前允许/禁止动作+冷启动引导） |
| `charter.md` | S0 立项固化一页：背景/定位/边界/编制/基线/约束/检查清单 |
| `PRD.md` | S2 产品需求：FR/AC/NFR/产品验收/待裁点/YAGNI 六节 |
| `tech-design.md` | S3 技术设计：决策速览/架构/API/表结构/CLI/待裁点十二节 |
| `development-task.md` | 开发任务档：定位/交付物/批次表（行序=合并序号）/决策账本/检查点/风险/验收八节调度中枢 |
| `onboarding.md` | 启动指令·通讯域全集模板（使用规则+总控版+执行者 A/B/C 变体+接入指引）：init 两形态（--comm-only 与完整）同款产物 `.aiteam/onboarding.md`，用户贴新窗口的变体本体 |
| `methodology.md` | 启动指令·方法论补全段模板（读规划件/自治等级/五步纪律链/并行纪律/返工位）：仅完整形态渲染 `docs/planning/methodology.md`，粘贴时与 onboarding.md 对应变体两段连读 |
| `executor-contract.md` | 执行者行为契约：五步纪律链+收尾信号+返工时间盒+红线 |
| `spec-template.md` | 批次 spec 九节骨架（范围/技术落点/任务边界/测试策略/结构影响/旧链清退/往返账/待裁点/资产回写盘点） |
| `plan-template.md` | 批次 plan：执行模式行（字面）+任务总览+任务块四要素+收尾审查项 |
| `column-task-template.md` | 多栏目并行时每栏目一份的执行中枢（栏目级 S0~S7，与项目级七阶段分属两层语义） |
| `dev-guide-base.md` | 工程通用纪律模板（语言无关）：§二按项目语言整节替换 |
| `adr-template.md` | 架构决策记录（ADR）模板：背景/决策/后果三段式+三道门自检清单，append-only 只增不改 |
| `context-template.md` | 项目术语表模板：黑话大白话一条三件套（术语+一句话+关联词），新会话/新 agent 第一读物 |
| `maintenance-template.md` | 维护清单模板：跨迭代非缺陷遗留件+用户后置动作收纳（七列台账），S7 收口结转登记，init 渲染播种 `docs/planning/maintenance.md` |
| `index-template.md` | 迭代 specs 登记簿模板：spec/plan 文档登记行（条目/标题/状态/所在位四列）+目录体系说明，init 渲染播种 `docs/planning/iterations/v0.1/specs/INDEX.md`，新迭代立项复制取用 |
| `topology-single.md` | 单机协作拓扑纪律节（worktree 隔离/合并序号制）——init --topology single 渲染入 dev-guide §5.5 |
| `topology-distributed.md` | 多机远端协作拓扑纪律节（WIP push + fetch 前置）——init --topology distributed 渲染入 dev-guide §5.5 |

## 二、占位符语法

- 形态：`{{大写下划线}}` —— 双花括号包裹，内部只允许**大写字母 / 数字 / 下划线**（正则 `\{\{[A-Z0-9_]+\}\}`）；禁小写、空格、连字符。
- 系列占位符：`<n>` 表示数字序号（1、2、3…按实际条数取用），如 `{{YAGNI_1}}`/`{{YAGNI_2}}`/`{{YAGNI_3}}` 是 `{{YAGNI_<n>}}` 系列的实例。
- 字面尖括号占位：`onboarding.md` 变体段的 `<项目code>`/`<栏目号>`/`<会话名>` 等属**人工替换占位**——不经 init 占位符渲染（渲染器只处理 `{{}}` 形态，尖括号原样落产物），由用户贴窗前改实际值；不入 §三 登记表。
- 替换纪律：复制模板后**全部**占位符一次替换干净，不留未替换占位符入仓；`<!-- -->` 指导注释同步删除。

## 三、保留字清单（权威登记表）

### 3.1 身份类（项目与角色）

| 占位符 | 语义 |
|---|---|
| `{{PROJECT_NAME}}` | 项目名（全文标题与指令称呼） |
| `{{PROJECT_CODE}}` | 项目短代号（b9 启用：onboarding.md 实况行「项目：X，code：Y」消费；`{{PROJECT_NAME}}` 与 `{{PROJECT_CODE}}` 同源自 init --project/--name） |
| `{{ONE_LINER}}` | 项目一句话定位 |
| `{{PROJECT_BIN}}` | 项目 CLI 可执行名（通讯命令的根词） |
| `{{OWNER_A}}` `{{OWNER_B}}` `{{OWNER_C}}` `{{OWNER_D}}` | 执行者 A/B/C/D 并行身份（批次表 owner 列取值；更多执行者按 OWNER_E 起顺延并登记） |
| `{{CONTROLLER_SESSION}}` | 总控会话标识 |
| `{{USER_TOUCHPOINTS}}` | 用户硬触点清单（仅用户亲自、不可代批的事） |
| `{{AUTONOMY_LEVEL}}` | 自治等级（manual / semi / full-auto） |
| `{{FINAL_REVIEW_ITEM}}` | 用户终审硬触点内容（如发布前脱敏终审） |

### 3.2 流程类（阶段/批次/分支/命令/纪律与资产路径）

| 占位符 | 语义 |
|---|---|
| `{{DATE}}` | 日期（批准/裁定/检查点等记录通用） |
| `{{REPO_PATH}}` | 仓库根路径（文档落位锚点） |
| `{{REQ_BASELINE}}` | 需求基线（冻结输入文档及其定位） |
| `{{FROZEN_INPUTS}}` | 当前冻结输入清单（红绿灯禁改项） |
| `{{SPEC_SOURCE}}` | 工程规范源（红绿灯引用） |
| `{{ENG_SPEC_ENTRY}}` / `{{ENG_SPEC_DOC}}` / `{{ENG_SPEC_DIR}}` | 工程规范入口文档 / 引用文档 / 目录 |
| `{{APPROVAL_MODE}}` | 立项批准方式（呈报即过/逐项审批等） |
| `{{DONE_STAGES}}` | 已完成阶段清单（红绿灯状态面） |
| `{{ORCHESTRATION_VERSION}}` | S4 批次编排版本 |
| `{{STAGE}}` | 检查点行的阶段名 |
| `{{BATCH_N}}` | 批次号（B0、B1…展示形态） |
| `{{BATCH_ID}}` | 批次文件标识（specs/ 文件名用的小写形态） |
| `{{BATCH_TITLE}}` / `{{BATCH_ORDER}}` / `{{BATCH_SCOPE}}` / `{{BATCH_STATUS}}` / `{{BATCH_POSITIONING}}` / `{{BATCH_DEPENDENCIES}}` / `{{BATCH_WAVE_NOTE}}` | 批次属性：标题 / 开工序号 / 范围（FR）/ 状态 / 定位 / 依赖前提 / 波次注记 |
| `{{BRANCH_PREFIX}}` | feat/ 分支前缀 |
| `{{TIMEBOX_HOURS}}` | 单批次时间盒上限（小时） |
| `{{REMOTE_POLICY}}` | 远端政策（主干推进=feat 分支开发+merge --no-ff 串行合并，远端 push 权限归项目自定等口径） |
| `{{FORMAT_CMD}}` / `{{LINT_CMD}}` / `{{TYPECHECK_CMD}}` / `{{BUILD_CMD}}` / `{{TEST_CMD}}` | 工具链五命令：格式化 / 静态检查 / 类型检查 / 构建 / 测试（dev-guide §二语言适配） |
| `{{TOPOLOGY_BLOCK}}` | 协作拓扑纪律节占位（dev-guide §5.5）：`aiteam init --topology single\|distributed` 渲染两口径之一的标记区块（`<!-- aiteam:topology:begin/end -->`）入此位；手动复制改造本档的用户删除此占位符、自拟团队实际协作纪律 |
| `{{STATIC_CHECK_CMD}}` / `{{BUILD_CHECK_CMD}}` / `{{COMMENT_LANG}}` / `{{SPLIT_THRESHOLD}}` / `{{SOFT_LINE_LIMIT}}` / `{{HARD_LINE_LIMIT}}`（dev-guide 语言适配） | 语言适配纪律参数：静态检查命令（与 `{{LINT_CMD}}` 同位）/ 构建检查命令（与 `{{BUILD_CMD}}` 同位）/ 注释与沟通语言约定 / 拆文件行数阈值 / 行数红线软·硬上限 |
| `{{CHECK_SCRIPT}}` | 提交前检查脚本（plan 提交纪律引用） |
| `{{COMMIT_TYPE}}` | 提交信息 type（feat/fix/…） |
| `{{COLUMN_CODE}}` / `{{COLUMN_NAME}}` / `{{COLUMN_SLUG}}` / `{{SLUG}}` | 栏目编号 / 名称 / 文件名短码 / 变更档案文件名短码（changes/ 文档名） |
| `{{MENU_PATH}}` | 功能菜单 path / 权限码 |
| `{{PORTFOLIO_PLAN_DOC}}` | 分栏目规划文档路径 |
| `{{BUSINESS_RULES_SECTIONS}}` / `{{BUSINESS_RULES_DOC}}` | 业务规则章节定位 / 文档 |
| `{{DOMAIN_DOC_DIR}}` / `{{CONTEXT_DOC}}` / `{{SHOTS_DIR}}` / `{{ARCHIVE_DIR}}` | 领域文档目录 / 术语表 / 截图落盘目录 / 归档目录 |
| `{{ADR_NUMBER}}` | ADR 编号（NNNN 序号；docs/adr/ 文件名与标题） |

### 3.3 内容填空类（按模板系列登记；`<n>` 按需取用）

| 系列（所属模板） | 语义 |
|---|---|
| `{{BACKGROUND_PROBLEM}}` `{{USAGE_STORY}}` `{{MVP_SCOPE}}` `{{TECH_BASELINE_<n>}}` `{{LICENSE}}` `{{DELIVERY_CADENCE}}` `{{RELATION_TO_SOURCE}}`（立项档） | 背景问题 / 拿到仓库怎么用 / 一期 MVP 范围 / 技术基线条目 / 开源协议 / 交付节奏 / 与既有项目关系 |
| `{{YAGNI_<n>}}` `{{YAGNI_<n>_WHY}}`（立项档/PRD） `{{YAGNI_CHECK}}`（技术设计对拍） | 明确不做条目 / 不做理由 / 对拍零越界结论 |
| `{{ACCEPTANCE_<n>}}` `{{ACCEPTANCE_<n>_TITLE}}`（立项档/PRD/任务档） | 量化验收条目及其标题（S5 脚本化依据） |
| `{{OBSERVATION_WINDOW}}` `{{V1A_SCRIPT}}` `{{V1A_PASS}}` `{{V2_SCRIPT}}` `{{V2_PASS}}` `{{V3_SCRIPT}}` `{{V3_PASS}}`（PRD §4） | 验收观察窗 / V 系列验收子项的脚本形态与 PASS 判据 |
| `{{DELIVERABLE_<n>}}` `{{DELIVERABLE_<n>_DESC}}`（任务档） | 交付物及其说明（一期 MVP 清单行） |
| `{{MECHANISM_<n>}}`（任务档速览） `{{MECHANISM_<n>_TITLE}}` `{{MECHANISM_<n>_BODY}}`（技术设计 §6） | 核心机制速览条目 / 核心机制小节标题 / 正文 |
| `{{DECISION_ITEM}}` `{{DECISION}}` `{{DECISION_BASIS}}`（任务档 §5 账本） `{{DECISION_A1}}` `{{DECISION_A2}}` `{{A1_WHY}}` `{{A1_SECTION}}` `{{A2_WHY}}` `{{A2_SECTION}}`（技术设计 §0） `{{PRD_STATUS}}` `{{DESIGN_STATUS}}`（PRD/技术设计头） | 决策系列：账本行（事项/裁定/依据）/ 决策速览行（决策/理由/详细节号）/ 两档冻结状态行 |
| `{{WHAT_DONE}}` `{{NEXT_ACTION}}`（任务档 §6 检查点） `{{OWNER_COUNT_RATIONALE}}`（任务档 §4） | 检查点完成内容 / 下一动作 / 执行者数量定案依据 |
| `{{USER_TYPE_<n>}}` `{{USER_FORM_<n>}}` `{{USER_UI_<n>}}` `{{USER_NEED_<n>}}` `{{SCENARIO_<n>}}` `{{TERM_LIST}}` `{{NAMING_CONVENTION}}`（PRD §1） | 目标用户分类表四列 / 核心场景 / 术语表 / 命名约定 |
| `{{DOMAIN_<n>}}`（PRD §2 域名） `{{DOMAIN}}`（spec 结构表域列） `{{FR1_TITLE}}` `{{FR1_STATEMENT}}` `{{FR_LIST_DOMAIN_2}}`（PRD FR 示范） | 功能域 / FR 示范条目与第二域清单 |
| `{{AC_POSITIVE_CASE}}` `{{AC_NEGATIVE_CASE}}` `{{AC_UNREACHABLE_CASE}}` `{{AC_AUDIT_CASE}}`（PRD 四态示范） `{{AC_ID}}` `{{AC_BATCH_CRITERIA}}` `{{AC_FINAL_OWNER}}`（spec §1.2） `{{AC_REF}}`（plan 任务块） `{{AC_REACHABILITY}}`（技术设计对拍） `{{ACCEPTANCE_CRITERIA}}`（plan 机械验收） | AC 四态示范 / spec AC 口径行 / plan AC 引用 / 对拍可达性 / 任务块机械验收 |
| `{{NFR_ENV}}` `{{NFR_PERF}}` `{{NFR_DATA}}` `{{NFR_SEC}}` `{{NFR_PORT}}` `{{NFR_FAIL}}`（PRD §3） | NFR 六维：环境/性能/数据/安全/可移植/故障 |
| `{{GAP_<n>}}` `{{GAP_<n>_IMPACT}}` `{{GAP_<n>_DECISION}}`（PRD §5） `{{TGAP_<n>}}` `{{TGAP_<n>_IMPACT}}` `{{TGAP_<n>_DECISION}}`（技术设计 §10） `{{GAP}}` `{{RECOMMENDED_DECISION}}`（spec §八） | PRD 待裁点行 / 技术待裁点行 / spec 待裁点行与推荐裁定 |
| `{{ARCH_DIAGRAM}}` `{{ARCH_POINT_<n>}}` `{{COMPONENT_<n>}}` `{{COMPONENT_<n>_FORM}}` `{{COMPONENT_<n>_DUTY}}` `{{COMPONENT_<n>_BOUNDARY}}` `{{FORM_A}}` `{{FORM_A_PRO}}` `{{FORM_A_CON}}` `{{FORM_B}}` `{{FORM_B_PRO}}` `{{FORM_B_CON}}` `{{SEQ_DIAGRAM}}` `{{FAILURE_BRANCHES}}`（技术设计 §1） | 总体架构系列：架构图与要点 / 组件表四列 / 关键形态二选一对比 / 关键时序图 / 失败分支清单 |
| `{{API_PREFIX}}` `{{DATA_FORMAT}}` `{{SUCCESS_SHAPE}}` `{{AUTH_SHAPE}}` `{{BODY_LIMIT}}` `{{HEALTH_ENDPOINT}}`（技术设计 §2.1） | API 通用约定六项 |
| `{{ENDPOINT_PATH}}` `{{METHOD}}` `{{REQ_FIELDS}}` `{{RESP_FIELDS}}` `{{ERROR_SEMANTICS}}` `{{RESPONSE_EXAMPLE}}` `{{ERR_CODE}}` `{{HTTP_STATUS}}` `{{ERR_SEMANTIC}}` `{{CLI_BEHAVIOR}}`（技术设计 §2.2~2.4） | 接口行系列：端点行五列 / 响应样例 / 错误码行四列 |
| `{{FR_ID}}` `{{FR_ENDPOINTS}}` `{{FR_TABLES}}` `{{FR_CLI}}` `{{FR_NOTE}}`（技术设计 §2.5） | FR 落点矩阵行（API/表/命令三层落点） |
| `{{DB_RULE_<n>}}` `{{DDL_ALL_TABLES}}` `{{ENUM_LIST}}` `{{DB_CONSTRAINTS}}` `{{INDEX_DDL}}` `{{ER_DIAGRAM}}` `{{KEY_QUERIES}}` `{{ROUNDTRIP_BUDGET}}`（技术设计 §3） | 建表总则 / 全量 DDL / 枚举 / 约束 / 索引 / ER 图 / 关键查询 / 往返账预估 |
| `{{CLI_NAMING}}` `{{CLI_IDENTITY}}` `{{CLI_CONFIG_CHAIN}}` `{{CLI_OUTPUT}}` `{{CLI_UNREACHABLE}}`（技术设计 §4.1） `{{CLI_CORE_CMD_<n>}}` `{{FLAG}}` `{{REQUIRED}}` `{{FLAG_DESC}}` `{{AUX_CMD}}` `{{AUX_ARGS}}` `{{AUX_OUTPUT}}` `{{LOCAL_VALIDATE_CASE}}`（技术设计 §4.2~4.4） | 命令面约定 / 核心命令与 flag 表 / 辅助命令 / 退出码本地校验例 |
| `{{UI_OPTION_<n>}}` `{{UI_1_PRO}}` `{{UI_1_CON}}` `{{UI_1_VERDICT}}` `{{UI_LAYOUT}}` `{{TECH_CHOICE_<n>_TITLE}}` `{{TECH_CHOICE_<n>_WINNER}}` `{{WINNER_WHY}}` `{{TECH_CHOICE_<n>_LOSER}}` `{{LOSER_WHY}}` `{{DEPENDENCY_WHITELIST}}`（技术设计 §0/§5/§7） | 选型系列：表现层对比与页面分区 / 技术选型对比 / 依赖白名单 |
| `{{REPO_TREE}}` `{{DEP_DIRECTION}}` `{{MODULE_RULE}}` `{{LINE_LIMIT}}`（技术设计 §8） | 目录树 / 依赖方向 / 功能域分文件规则 / 行数红线 |
| `{{LEGACY_RETIREMENT}}` `{{STRUCT_IMPACT}}` `{{STRUCT_ASSESSMENT_NOTES}}` `{{COVERAGE_CHECK}}` `{{T_DECISION_CHECK}}`（技术设计 §9/对拍附录、spec §五~六） | 收尾系列：旧链清退盘点 / 结构影响评估及其备注 / FR 全覆盖与待裁点落地对拍结论 |
| `{{SCOPE_ITEM}}` `{{SCOPE_DESC}}` `{{SECTION}}` `{{FR_N}}` `{{EXEC_NOTES}}` `{{LANDING_SPOT}}` `{{EXPLICIT_EXCLUSIONS}}` `{{EXCLUSION_<n>}}` `{{EXCLUSION_<n>_OWNER}}`（spec） | 范围行 / 冻结依据节号 / 执行要点 / 落点 / 显式不纳入 / 任务边界行 |
| `{{TEST_GENERAL_RULES}}` `{{TEST_LAYER_<n>}}` `{{TEST_POINTS_<n>}}`（spec §四） | 测试总则 / 分层测试要点 |
| `{{FILE_PATH}}` `{{LINES}}`（spec §五行数预估） `{{FILE_PATHS}}`（plan 产物文件） | 文件路径与预估行数 / 产物文件清单 |
| `{{TASK_COUNT}}` `{{TASK_NAME}}` `{{TASK_TITLE}}` `{{ARTIFACT_LAYER}}` `{{AC_REF}}`（plan 总览） | 任务数 / 任务名与标题 / 产物层 / AC 引用 |
| `{{EXECUTION_MODE_LINE}}` `{{EXECUTION_ORDER}}` `{{SPEC_SECTION}}` `{{TDD_OR_MECHANICAL}}` `{{TDD_OR_MECHANICAL_NOTE}}` `{{STEP_<n>}}` `{{PLAN_ONE_LINER}}`（plan） | 执行模式行（字面保留）/ 执行序 / spec 节号 / TDD 或机械验收声明及备注 / 执行步骤 / plan 一句话 |
| `{{FINAL_CHECK_<n>}}`（plan 收尾） | 收尾主会话审查项 |
| `{{DECISION_TITLE}}` `{{DECISION_CONTEXT}}` `{{RELATED_REFS}}` `{{CONSEQUENCE_POSITIVE}}` `{{CONSEQUENCE_NEGATIVE}}` `{{CONSEQUENCE_NEUTRAL}}` `{{AFFECTED_MODULES}}` `{{GATE_<n>_VERDICT}}`（ADR 档） | 决策标题 / 决策背景（问题·触发·约束）/ 决策关联引用 / 正面·负面·中性后果条目 / 影响模块与目录 / 三道门自检结论（决策内容段复用任务档 `{{DECISION}}`，日期复用 `{{DATE}}`，用户触点复用 `{{USER_TOUCHPOINTS}}`） |
| `{{TERM_<n>}}` `{{TERM_<n>_DEF}}` `{{TERM_<n>_RELATED}}`（术语表档） | 术语条目三件：术语名 / 大白话一句话解释 / 关联词 |
| `{{LANGUAGE_IDIOMS}}`（dev-guide §2.1） | 语言现代写法/惯用法对照表或其落点指引（「场景 → 用 → 不用」三列形态） |
| `{{ASSET_DOMAIN_CHANGE}}` `{{ASSET_ADR_CHANGE}}` `{{ASSET_TERM_CHANGE}}` `{{ASSET_HELP_CHANGE}}` `{{ASSET_SUMMARY}}`（spec §九） | 资产回写盘点结论行：领域文档 / ADR / 术语表 / 用户文档 / 盘点总结论 |

## 四、扩展规则

1. **命名原则**：全大写+下划线、语义自明（见名知义，禁 X1/X2 类无义名）、与既有风格一致（名词_属性，如 `{{BATCH_STATUS}}`）；同构条目做成 `<n>` 系列，不逐条新造名字。
2. **先登记后使用**：新占位符须先登记回 §三 对应分组（注明所属模板与语义），再在模板中使用；**未登记占位符视同违规**，由 `scripts/check-sanitize.sh` 的 `_template/` 校验承载。
3. **改名=全量替换**：保留字不改名；确需改名时一次改齐全部模板与本表，禁止新旧并存。

## 五、禁残留清单（check-sanitize 机械判据）

以下内容在 `_template/` 全目录**零命中**（任何命中=违规，列出文件:行号退出非零）。判据形态在**本 README 条文中的展示不算命中**（check-sanitize 实现须豁免本文件的条文行；其余文件无条件零命中）：

1. **真实项目内部名**：内部项目名、内部系统名、内部代号（含其拼音/缩写变体）。
2. **栏目号**：真实项目的栏目编号硬编码（模板自身用 `{{COLUMN_CODE}}` 占位）。
3. **内网/私有网段 IP 字面**：`192.168.*.*`、`10.*.*.*`、`172.16~31.*.*` 等任何私有网段字面量。
4. **盘符私有路径**：`X:\` 形态的盘符路径（任何盘符）；`C:\Users` 用户目录路径。
5. **个人名**：任何真实人名（版权行用项目贡献者集体名）。
6. **开发期过渡物引用**：mailbox 系列脚本与 signals 信箱路径/文件名——通讯纪律一律以 aiteam CLI 语境表述（`{{PROJECT_BIN}}` 的 send/poll/ack/status/watch）。
