# {{BATCH_N}} 批次 plan——{{TASK_COUNT}} 任务（{{PLAN_ONE_LINER}}）

> 【模板说明】本文件=流水线「批次 plan」模板：spec 的执行切片——执行模式行（字面）+任务总览表+任务块（四要素）+收尾审查项；主会话按任务块逐个派子代理执行。
> 用法：复制到 `{{REPO_PATH}}/docs/planning/specs/{{BATCH_ID}}-plan.md`，替换全部占位符（大写下划线样式），删除各节 `<!-- -->` 指导注释；**执行模式行必须字面保留**（它与执行者契约/总控派工指令三方一致，改一处须三处同改）。

> {{EXECUTION_MODE_LINE}}
>
> <!-- 执行模式行写什么：与总控派工指令逐字一致的那段纪律——必需子技能（subagent-driven-development：每个实现任务 spawn 独立子代理、主会话禁止直接写实现代码与测试、禁止单会话直干模式）+分支与合并纪律（feat/ 分支、自测全绿后合并回 master、禁止 push）+批次特殊声明（先行批/并行波次/零冲突面）。此行是「机械核验」对象：总控派工前核对本行字面合规。 -->
>
> spec：[{{BATCH_ID}}-spec.md]({{BATCH_ID}}-spec.md) | 分支：**feat/{{BATCH_ID}}**（using-git-worktrees 建 worktree+分支，自测全绿后合并回 master；禁止 push）| {{BATCH_WAVE_NOTE}}
> 提交纪律：{{ENG_SPEC_DOC}} 提交纪律节——逐文件 add、`git diff --stat --cached` 核对、中文 commit（`{{COMMIT_TYPE}}: ...` 型）、每任务结束 {{CHECK_SCRIPT}} 全绿
> {{TDD_OR_MECHANICAL_NOTE}}
> 待裁点 {{BATCH_ID}}-T 系列按推荐口径实现（总控预审若改判，先改 spec 再开工对应任务）

## 任务总览

<!-- 此节写什么：全任务一览表+执行序一段——每行：任务号/任务名/产物层/对应 spec 验收；任务号={{BATCH_ID}}-1 起连续编号；表尾「执行序」写清依赖与并行关系（谁可与谁并行、谁必须在谁后、收口任务是谁）。 -->

| # | 任务 | 产物层 | 对应 spec 验收 |
|---|------|--------|---------------|
| {{BATCH_ID}}-1 | {{TASK_NAME}} | {{ARTIFACT_LAYER}} | {{AC_REF}} |
| {{BATCH_ID}}-2 | {{TASK_NAME}} | {{ARTIFACT_LAYER}} | {{AC_REF}} |

执行序：{{EXECUTION_ORDER}}

---

## {{BATCH_ID}}-1 {{TASK_TITLE}}

<!-- 任务块四要素（每任务一组，格式如下）：①对应 spec 验收项（可追溯到 spec §一/§二）②预期产物文件（精确路径）③执行步骤（TDD 任务=先写哪个测试再实现什么，逐步可派给单个子代理；文档任务=结构要点逐条）④完成判据（机械验收——可 grep/可跑/可对拍的判据清单）。四要素缺一=任务块不合格，review 退回。 -->

- **对应 spec 验收项**：{{AC_REF}}；spec §{{SPEC_SECTION}} 落点行
- **预期产物文件**：{{FILE_PATHS}}
- **执行步骤**（{{TDD_OR_MECHANICAL}}——由子代理执行，主会话按判据审）：
  1. {{STEP_1}}——TDD 任务先写：失败测试清单与断言点
  2. {{STEP_2}}——实现到绿：最小实现使上述测试通过
  3. {{STEP_3}}——重构与边界补测
- **机械验收**：{{ACCEPTANCE_CRITERIA}}（逐条可机械核验：grep 判据/命令退出码/对拍清单/零命中声明）

## {{BATCH_ID}}-2 {{TASK_TITLE}}

- **对应 spec 验收项**：{{AC_REF}}
- **预期产物文件**：{{FILE_PATHS}}
- **执行步骤**：
  1. {{STEP_1}}
  2. {{STEP_2}}
- **机械验收**：{{ACCEPTANCE_CRITERIA}}

---

## 收尾（主会话审查项）

<!-- 此节写什么：全部任务完成后、合并前的主会话审查清单——逐项复核（脱敏零引用/占位符自洽/上游模板零改动/跨产物一致性/脚本走查）+状态回写动作（由总控侧执行的最后一条）；审查项每条必须是「看得到对错」的核对动作，不写「检查质量」类空话。 -->

1. {{FINAL_CHECK_1}}
2. {{FINAL_CHECK_2}}
4. {{FINAL_CHECK_N}}
5. 状态回写任务档 §4 批次表（{{BATCH_N}} 行）由总控侧执行。
