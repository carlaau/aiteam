package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	aiteam "aiteam"
	"aiteam/internal/assets"
	"aiteam/internal/config"
)

// init 命令（FR22）：开源用户第一步——在目标项目目录跑 `aiteam init`，生成接入
// 脚手架：.aiteam/onboarding.md 通讯域全集（b9 一源一渲染+形态分叉引导——comm-only
// 原样，full 开工第一读补 planning 第二站）+.aiteam/cli.json+
// AGENTS.md/CLAUDE.md 一句话自管区块；完整形态另有 planning 渲染件（项目级件+
// iterations/<首迭代名>（默认 v0.1）迭代骨架，见 planningArtifacts）+_template
// 模板整目录原样播种+dev-guide+技能包+post-commit hook。模板经 go:embed 编入
// 二进制（见模块根包 templates.go），占位符按参数渲染后写出。
//
// 增量幂等（硬判据）：逐文件检查——不存在才生成，已存在一律跳过不动；planning 是
// 活文档绝不可覆盖；不提供 --force（防误毁进度）。唯一例外=AGENTS.md/CLAUDE.md 的
// aiteam 自管区块：含标记时仅刷新区块内内容（区块外零触碰），内容无变化仍计跳过。

// AGENTS.md 区块标记（参照 superpowers 引入模式）：aiteam 管理内容只存在于
// begin/end 之间，升级重跑 init 安全重写自管段。
const (
	agentsBegin = "<!-- aiteam:begin (do not edit between these markers) -->"
	agentsEnd   = "<!-- aiteam:end -->"
)

// claudeLegacyPointerLine b6 时代 CLAUDE.md 指针行（无标记 writeIfAbsent 产物）：
// b9 区块落位后该行自述失真（文件已含实体区块）——重跑 init 时逐行精确剔除
// （init 自产内容自清理），非 init 产物的用户内容零触碰。
const claudeLegacyPointerLine = "本文件为指针：项目指令见 [AGENTS.md](AGENTS.md)（aiteam 多会话流水线管理）。"

// dev-guide 拓扑节标记（base.md §5.5 的 aiteam 自管区块，b8-spec §1.1 冻结标记名）。
// 与 AGENTS.md 区块共用同一套三态机制（markerBlock），标记名带 topology 中缀——
// 两类区块即便被用户混排进同一文件也互不误伤。
const (
	topologyBegin = "<!-- aiteam:topology:begin -->"
	topologyEnd   = "<!-- aiteam:topology:end -->"
)

// onboardingTemplate 通讯域全集模板（b9：启动指令四变体+接入指引合一）：init
// 两形态一源一渲染产物 .aiteam/onboarding.md（渲染后形态分叉引导——comm-only
// 原样，full 补 planning 第二站，见 applyOnboardingDoc）——模板自带文件级 aiteam
// 标记对，占位符替换后整文件落盘（b6 段级标记/一源两渲染机制整体废止）。
const onboardingTemplate = "onboarding.md"

// methodologyTemplate 方法论段专属模板（b9 瘦身：变体本体已迁 onboarding.md），
// 完整形态渲染落 docs/planning/methodology.md，comm-only 不产。
const methodologyTemplate = "methodology.md"

// onboardingRel onboarding 产物相对路径（slash 形态，落盘与报告展示同源）。
const onboardingRel = ".aiteam/onboarding.md"

// legacyStartupDocs 旧中文名产物路径（AC8 升级检测面，slash 形态）：b9 改名后
// init 播种新名并提示、不自动删旧文件（安全）；此处字面保留旧名属 AC8 功能引用
// （检测对象即旧名文件本身），非 AC6 旧名残留——grep 闸对账时按本块豁免。
var legacyStartupDocs = []string{
	".aiteam/启动指令.md",
	"docs/planning/启动指令.md",
}

// templateDirPrefix TemplateFS 内模板目录前缀（embed 保留完整目录路径，fs.Sub 剥离）。
const templateDirPrefix = "docs/planning/_template"

// firstIterationDir init 播种的起始迭代目录名（b10 迭代目录化：迭代四件+specs
// 登记簿落 docs/planning/iterations/<本名>/，新项目得到起步骨架）。默认名 v0.1，
// 项目按自身版本节奏改名即可——init 只负责播种不锁定命名。
const firstIterationDir = "v0.1"

// planningArtifact init 完整形态渲染播种件：src=TemplateFS 内模板文件名，
// dst=docs/planning/ 下落点（slash 形态，报告展示名与落盘同源）。
type planningArtifact struct {
	src string
	dst string
}

// planningArtifacts init 完整形态落盘的渲染件全集（b10 播种面重排：b9 七件套中
// 项目级三件留 docs/planning/ 根、迭代四件移位 iterations/<首迭代名>（默认
// v0.1）/——旧项目实况在
// planning 根时升级重跑不再被根级空模板覆盖污染（writeIfAbsent 根探问题的消解），
// 新项目得到迭代起步骨架；新增项目级三件 maintenance/CONTEXT/s0-s7-stages 与
// 迭代 specs/INDEX.md 登记簿。模板与产物异名三对：maintenance-template→
// maintenance.md、context-template→CONTEXT.md、index-template→specs/INDEX.md，
// 其余与模板源同名）。
var planningArtifacts = []planningArtifact{
	{src: "project-status.md", dst: "project-status.md"},
	{src: "executor-contract.md", dst: "executor-contract.md"},
	{src: methodologyTemplate, dst: "methodology.md"},
	{src: "maintenance-template.md", dst: "maintenance.md"},
	{src: "context-template.md", dst: "CONTEXT.md"},
	{src: "s0-s7-stages.md", dst: "s0-s7-stages.md"},
	{src: "charter.md", dst: "iterations/" + firstIterationDir + "/charter.md"},
	{src: "PRD.md", dst: "iterations/" + firstIterationDir + "/PRD.md"},
	{src: "tech-design.md", dst: "iterations/" + firstIterationDir + "/tech-design.md"},
	{src: "development-task.md", dst: "iterations/" + firstIterationDir + "/development-task.md"},
	{src: "index-template.md", dst: "iterations/" + firstIterationDir + "/specs/INDEX.md"},
}

// vcsPolicy --vcs 两套口径：REMOTE_POLICY/BRANCH_PREFIX 占位符渲染文案
// （executor-contract 红线节与 executor-contract/methodology 的分支段）。
// b6-spec W5：原 AGENTS.md 铁律两行（vcsRule/stagingRule）随区块铁律纯指针化
// （W1/R2）失去唯一消费者，字段清退——VCS/提交纪律归项目自身约定，aiteam 不代写条文。
type vcsPolicy struct {
	remotePolicy string // {{REMOTE_POLICY}}（executor-contract 红线节）
	branchPrefix string // {{BRANCH_PREFIX}}（executor-contract/methodology 的分支段）
}

// vcsPolicies git 主径 + svn 适配（总纲：aiteam 软件本身 VCS 无关，适配仅方法论层）。
// remotePolicy 值是模板「禁止 push/建远端（{{REMOTE_POLICY}}）」括号内的补充说明——
// git 值不重复「禁止 push」字样（模板静态前缀已说）、svn 值不带尾括号（防与模板
// 外层括号叠套）（b7b-7 质量审查：VCS 渲染文案去重）；git 值中性化（b6-spec W5）：
// 去「本仓无远端」项目断言，远端 push 权限由接入项目自行约定。svn 值为 SVN 集中式
// 通用纪律陈述，无项目断言，复核后维持。
var vcsPolicies = map[string]vcsPolicy{
	"git": {
		remotePolicy: "主干推进=feat/<批次> 分支开发+merge --no-ff 串行合并；远端 push 权限由项目自行约定",
		branchPrefix: "<批次号>-",
	},
	"svn": {
		remotePolicy: "SVN 集中式仓库：commit 前先 svn update（冲突先解再提交）；分支吸收主干变更=反向合并，svn 历史不可重写无 rebase",
		branchPrefix: "trunk（svn 集中式无分支制，提交串行）",
	},
}

// topologyPolicies --topology 两口径 → embed 变体节文件名（docs/planning/_template/
// 下）。纪律正文自包含零占位符（git 词汇；svn 团队按 base.md §五表头注映射总纲适配，
// 本批不做交叉口径——b8-spec §三.4 边界）。
var topologyPolicies = map[string]string{
	"single":      "topology-single.md",
	"distributed": "topology-distributed.md",
}

// devGuideTemplate init 落盘的工程纪律模板文件名（渲染至 docs/dev-guide/base.md，
// 其 §5.5 拓扑节为标记区块，按 --topology 口径注入）。
const devGuideTemplate = "dev-guide-base.md"

// hookScript post-commit hook 内容（FR23 自动层，B8-T2 裁定口径：shell 采 HEAD
// hash+branch 调 aiteam progress——batch/task 空=自动层形态；aiteam 调用失败静默
// 不阻塞 commit，进度是观测数据不挡工作流，故无 set -e、调用行 || true、末行
// exit 0 三重兜底）。
//
// 二进制与身份全部经环境变量自动携带：AITEAM_BIN 可换测试/定制二进制（探测链：
// AITEAM_BIN → 默认安装位 ~/.aiteam → PATH 的 aiteam，默认位存在即用免 PATH）；
// AITEAM_SERVER 走五级查找链同款 env 级（亦可用仓库级
// .aiteam/cli.json，init 产物已就位）；AITEAM_PROJECT/COLUMN/SESSION/ROLE 由
// 执行者会话导出（每次 CLI 调用本就携带身份四参，export 是零成本动线）——未导出
// 时 CLI 本地校验退 2，被 || true + exit 0 吞掉，不上报但不阻塞。
// POSIX sh 语法（B8-T2「纯 bash」取其「shell 采值+调 CLI，无网络重试」意图，
// 未用任何 bash 特有语法，Git for Windows / 精简容器均可执行）。
const hookScript = `#!/bin/sh
# aiteam init 生成——进度自动上报，失败静默不影响提交；重跑 init 不覆盖本文件。
hash=$(git rev-parse HEAD 2>/dev/null) || exit 0
branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null) || exit 0
# 二进制探测链：AITEAM_BIN 显式指定 → 默认安装位 ~/.aiteam（存在即用，免 PATH）→ PATH
cmd=${AITEAM_BIN:-}
[ -z "$cmd" ] && [ -x "$HOME/.aiteam/aiteam" ] && cmd=$HOME/.aiteam/aiteam
[ -z "$cmd" ] && cmd=aiteam
"$cmd" progress \
  --project "$AITEAM_PROJECT" --column "$AITEAM_COLUMN" \
  --session "$AITEAM_SESSION" --role "$AITEAM_ROLE" \
  --commit "$hash" --branch "$branch" >/dev/null 2>&1 || true
exit 0
`

// todoPlaceholders 不可推导占位符→中文含义（立项时由用户/总控回填）。只收立项级
// 高频项，含义说明集中维护于此；未收录占位符（PRD/技术设计的批次级模板占位符）
// 走 fallback 渲染——含义由模板各节「此节写什么」指导注释承载，仍保证零 {{ 残留。
// b6 W3/W4 清退三键：启动指令改字面尖括号占位 watch 命令行（<栏目号>/<会话名>，
// comm-only 产物直接可读不落 TODO）+执行者变体显式 A/B/C 化——{{WATCH_COMMAND}}/
// {{OWNER_X}}/{{SESSION_NAME_X}} 失去消费者（_template/README.md 登记表同步清退）。
var todoPlaceholders = map[string]string{
	"ONE_LINER":             "项目一句话定位（是什么、为谁解决什么问题、以什么形态交付）",
	"REQ_BASELINE":          "需求基线文档路径（冻结输入：分支全裁定+方案骨架所在）",
	"AUTONOMY_LEVEL":        "自治等级（如：全自动代批——总控预审代批+决策账本留痕）",
	"CONTROLLER_SESSION":    "总控会话名（如 controller-A）",
	"USER_TOUCHPOINTS":      "用户硬触点清单（如：启动确认、最终验收体验、发布终审）",
	"OWNER_A":               "执行者 A 身份名（如 executor-A）",
	"OWNER_B":               "执行者 B 身份名（如 executor-B）",
	"OWNER_C":               "执行者 C 身份名（如 executor-C）",
	"OWNER_D":               "执行者 D 身份名（如 executor-D）",
	"TIMEBOX_HOURS":         "单批次时间盒上限（小时数，方法论默认 8）",
	"ENG_SPEC_ENTRY":        "工程规范入口文档路径（S1 产出入仓后回填）",
	"SPEC_SOURCE":           "工程规范文档来源路径（S1 产出入仓）",
	"LICENSE":               "开源许可证类型（如 MIT/Apache-2.0/专有）",
	"APPROVAL_MODE":         "审批模式（如：总控 AI 预审代批+决策账本留痕）",
	"MVP_SCOPE":             "一期 MVP 范围一句话",
	"USAGE_STORY":           "典型使用故事（用户怎么把项目用起来）",
	"DELIVERY_CADENCE":      "交付节奏（如：周批/双周批）",
	"RELATION_TO_SOURCE":    "与方法论源仓的关系（独立项目/方法论的落地实例等）",
	"BACKGROUND_PROBLEM":    "背景问题（本项目要解决的根本问题）",
	"FROZEN_INPUTS":         "冻结输入清单（变更须走任务档 §5 决策账本）",
	"DONE_STAGES":           "已完成阶段清单（如：S0 立项、S1 规范）",
	"FINAL_REVIEW_ITEM":     "发布前用户终审项（如：脱敏扫描+体验验收）",
	"ORCHESTRATION_VERSION": "并行编排版本号（编排要点变更时递增）",
	"BATCH_N":               "批次号（如 b1/b2，按任务档批次表回填）",
	"BATCH_SCOPE":           "批次范围（该批覆盖哪些 FR）",
	"BATCH_STATUS":          "批次状态（已批/开发中/已合并待审/已关批）",
	"OWNER_COUNT_RATIONALE": "执行者数量定案依据（并行度裁定说明）",
}

// placeholderRe 占位符语法：{{UPPER_SNAKE}}（允许内嵌数字，如 DELIVERABLE_1）。
var placeholderRe = regexp.MustCompile(`\{\{[A-Za-z0-9_]+\}\}`)

// initReport 增量幂等汇总：生成/更新/跳过三类清单（rel 路径统一 / 分隔展示）+
// _template 整目录播种计数（b10：报告按模板件组汇总一行，不逐件刷屏；头部统计
// 口径含模板件——生成/跳过数均计入）。
type initReport struct {
	created        []string
	updated        []string
	skipped        []string
	tplCreated     int // _template 播种新增件数
	tplSkipped     int // _template 播种已存在跳过件数
	hookEnvSkipped int // hook 环境性跳过——非 git 仓根/auto_hook=off，非「已存在产物」，不计入升级提示 N
}

// RunInit 是 `aiteam init --project <code> --name <名称> [--skills] [--vcs git|svn]`
// 子命令入口：在 CWD（=目标项目目录）生成脚手架。
func RunInit(args []string) error {
	return runInit(args, os.Stdout)
}

// runInit init 主链：flag 解析→校验→渲染+写盘（增量幂等）→汇总→下一步指引。
// stdout 注入便于测试捕获汇总输出。
func runInit(args []string, stdout io.Writer) error {
	fset := flag.NewFlagSet("init", flag.ContinueOnError)
	project := fset.String("project", "", "项目 code（必填，如 proj-a）")
	name := fset.String("name", "", "项目名称（必填）")
	noSkills := fset.Bool("no-skills", false,
		"跳过技能包安装（默认装到 .agents/skills/，merge 语义：同名保留现有版、仅新增缺失——执行者契约引用的 Skill 实体一次到位）")
	commOnly := fset.Bool("comm-only", false,
		"只装通讯接入（AGENTS.md/CLAUDE.md aiteam 区块+.aiteam/cli.json+.aiteam/onboarding.md），跳过 planning 渲染件+_template 模板全套/dev-guide/skills/hook——已有自己 planning 体系的仓（FX 断点⑥）")
	vcs := fset.String("vcs", "git", "VCS 纪律口径：git 或 svn")
	topology := fset.String("topology", "single",
		"协作拓扑口径：single（单机 worktree 隔离）或 distributed（多机远端协作）")
	fset.Usage = func() {
		fmt.Fprintln(fset.Output(), "用法: aiteam init --project <code> --name <名称> [--comm-only] [--no-skills] [--vcs git|svn] [--topology single|distributed]")
	}
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // -h/--help：flag 包已输出用法，按 CLI 惯例视作成功
		}
		return fmt.Errorf("%w: %v", ErrUsage, err)
	}
	if fset.NArg() > 0 {
		return fmt.Errorf("%w: 无法识别的位置参数 %q", ErrUsage, fset.Args())
	}
	if err := rejectPlaceholderFlags(fset); err != nil {
		return err // #15：--project/--name 原样尖括号=复制占位符，教学式拦截在写盘前
	}
	var missing []string
	if *project == "" {
		missing = append(missing, "--project")
	}
	if *name == "" {
		missing = append(missing, "--name")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: 缺必填参数 %s", ErrUsage, strings.Join(missing, " "))
	}
	policy, ok := vcsPolicies[strings.ToLower(*vcs)]
	if !ok {
		return fmt.Errorf("%w: --vcs 仅支持 git|svn，得到 %q", ErrUsage, *vcs)
	}
	topoKey := strings.ToLower(*topology)
	if _, ok := topologyPolicies[topoKey]; !ok {
		return fmt.Errorf("%w: --topology 仅支持 single|distributed，得到 %q", ErrUsage, *topology)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("获取当前目录失败: %w", err)
	}

	values := buildValues(*project, *name, cwd, policy)
	rep := &initReport{}

	// 产物①：.aiteam/onboarding.md 通讯域全集（b9 一源一渲染+形态分叉引导：模板
	// 自带文件级 aiteam 标记对，占位符替换后整文件落盘——comm-only 原样，full 开工
	// 第一读补方法论第二站）。writeIfAbsent 幂等——已存在一律整文件跳过（升级重跑
	// 不覆盖；既有内容与区间外自行扩展的保护来自整文件跳过，与标记区间机制无关）。
	if err := applyOnboardingDoc(cwd, values, *commOnly, rep); err != nil {
		return err
	}

	// 产物②：_template 整目录原样播种（b10 W1：模板是供复制的源，init 只搬运
	// 不渲染；逐文件幂等——已存在跳过）+planning 渲染件（模板→渲染→逐文件幂等
	// 写出，落点见 planningArtifacts）。--comm-only 全跳过（FX 断点⑥：只要通讯
	// 不要方法论——已有自己 planning 体系的仓，新增总纲/模板属污染）。methodology.md
	// 整文件直渲染（b6 段级标记/一源两渲染机制废止——变体本体在 onboarding.md，
	// 本文件只余方法论段）。
	if !*commOnly {
		if err := seedTemplateDir(cwd, rep); err != nil {
			return err
		}
		templates, err := fs.Sub(aiteam.TemplateFS, templateDirPrefix)
		if err != nil {
			return fmt.Errorf("内嵌模板不可用: %w", err)
		}
		for _, art := range planningArtifacts {
			src, err := fs.ReadFile(templates, art.src)
			if err != nil {
				return fmt.Errorf("读取内嵌模板 %s 失败: %w", art.src, err)
			}
			rendered := renderTemplate(string(src), values)
			dst := filepath.Join("docs", "planning", filepath.FromSlash(art.dst))
			if err := writeIfAbsent(filepath.Join(cwd, dst), filepath.ToSlash(dst),
				[]byte(rendered), rep); err != nil {
				return err
			}
		}
	}

	// 产物③：.aiteam/cli.json 骨架（技术设计 §4.1 形态：server/token，服务地址占位）
	cliJSON := "{\n" +
		"  \"_doc\": \"CLI 连接配置（仓库级可提交共享；服务地址按实际部署修改，查找链见接入指南）\",\n" +
		"  \"server\": \"http://127.0.0.1:8310\",\n" +
		"  \"token\": \"\"\n" +
		"}\n"
	if err := writeIfAbsent(filepath.Join(cwd, ".aiteam", "cli.json"), ".aiteam/cli.json",
		[]byte(cliJSON), rep); err != nil {
		return err
	}

	// 产物④：AGENTS.md 与 CLAUDE.md 双写同款一句话区块（b9：接入细节全部收敛
	// 在 .aiteam/onboarding.md，区块恒定；三态管理——重跑只刷新区块/无变化跳过；
	// CLAUDE.md 落区块前精确剔除 b6 时代指针行——自产内容自清理）。
	block := agentsBlock(*project, *name)
	if err := applyManagedBlock(cwd, "AGENTS.md", "# AGENTS.md\n\n", block, nil, rep); err != nil {
		return err
	}
	if err := applyManagedBlock(cwd, "CLAUDE.md", "", block,
		[]string{claudeLegacyPointerLine}, rep); err != nil {
		return err
	}

	// 产物⑤：docs/dev-guide/base.md 工程通用纪律（§5.5 拓扑节为标记区块，按
	// --topology 口径注入；文件级三态与 AGENTS.md 区块同构，b8-spec §1.1）。
	// --comm-only 跳过（方法论形态产物，同②）。
	if !*commOnly {
		if err := applyDevGuide(cwd, values, topoKey, rep); err != nil {
			return err
		}
	}

	// 产物⑥：技能包落地 .agents/skills/（merge 语义，默认装）。--comm-only 与
	// --no-skills 跳过（技能包服务方法论执行者流程，通讯形态/显式关闭不涉）。
	var skillsNew, skillsKept int
	skillsInstalled := !*noSkills && !*commOnly
	if skillsInstalled {
		var err error
		skillsNew, skillsKept, err = copySkills(cwd)
		if err != nil {
			return fmt.Errorf("复制技能包失败: %w", err)
		}
	}

	// 产物⑦：.git/hooks/post-commit 进度自动上报（FR23 自动层；auto_hook=on 且
	// .git 目录存在才装，已存在跳过不覆盖——--topology 与 hook 安装正交，两 flag
	// 各自独立生效）。--comm-only 跳过（hook 归方法论形态，总控 #17 令面口径）。
	if !*commOnly {
		if err := installHook(cwd, rep); err != nil {
			return err
		}
	}

	writeReport(stdout, rep, skillsInstalled, skillsNew, skillsKept, *commOnly)

	// AC8 升级提示：播种新名后检测旧中文名产物（存在才提示，不自动删——安全）。
	// 提示行走 stdout（汇总之后，用户视线收尾处），含旧路径+手动删除指引。
	for _, rel := range legacyStartupDocs {
		if _, err := os.Stat(filepath.Join(cwd, filepath.FromSlash(rel))); err == nil {
			fmt.Fprintf(stdout,
				"提示: 检测到旧版产物 %s（新版为 %s 与 docs/planning/methodology.md）；确认新版在位后可手动删除旧文件，init 不自动删除。\n",
				rel, onboardingRel)
		}
	}
	return nil
}

// buildValues 占位符渲染值全集（三层策略汇成一个 map，key 含 {{}} 全 token）：
// 可推导四项 + VCS 两口径 + 立项级 TODO 提示；未收录占位符由 renderTemplate 兜底。
func buildValues(project, name, repoPath string, policy vcsPolicy) map[string]string {
	values := map[string]string{
		"{{PROJECT_NAME}}": name,
		"{{PROJECT_CODE}}": project, // b9：onboarding.md 实况行（项目：X，code：Y）
		"{{DATE}}":         time.Now().Format("2006-01-02"),
		// ToSlash：REPO_PATH 嵌进模板正斜杠路径/表格，Windows 反斜杠混排既难读
		// 又有 Markdown 转义风险（b7b-7 质量审查：可移植性）。
		"{{REPO_PATH}}":     filepath.ToSlash(repoPath),
		"{{PROJECT_BIN}}":   "aiteam",
		"{{REMOTE_POLICY}}": policy.remotePolicy,
		"{{BRANCH_PREFIX}}": policy.branchPrefix,
	}
	for tok, meaning := range todoPlaceholders {
		values["{{"+tok+"}}"] = "（TODO 立项时填写：" + meaning + "）"
	}
	return values
}

// renderTemplate 占位符替换：map 命中按值渲染；未命中兜底为显著中文提示——
// 保底「零 {{ 残留」（AC 硬判据），含义指向模板各节自带的「此节写什么」指导注释。
func renderTemplate(src string, values map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(src, func(tok string) string {
		if v, ok := values[tok]; ok {
			return v
		}
		return "（TODO 立项时填写：" + tok[2:len(tok)-2] + "——含义见本节指导注释）"
	})
}

// agentsBlock 组装 aiteam 自管区块文本（b9：一句话引导形态——接入细节全部
// 收敛在 .aiteam/onboarding.md，本区块恒定不随版本变更；升级不自动刷新
// onboarding.md——writeIfAbsent 已存在即整文件跳过，升级=手动删除后重跑重播种）。
func agentsBlock(project, name string) string {
	return agentsBegin + "\n" +
		"## aiteam 多会话通讯接入\n\n" +
		"本项目使用 aiteam 多会话通讯：AI 会话开工第一动作=读 `.aiteam/onboarding.md`" +
		"（服务地址/身份四参/五命令/哨兵值守纪律）。\n" +
		agentsEnd + "\n"
}

// markerBlock 标记区块三态合并——通用机制，AGENTS.md 自管区块与 dev-guide 拓扑节
// 共用单一实现（b8-spec §六旧链清退口径：泛化后原 applyAgentsBlock 内联实现改调用）：
//   - existing 无 begin/end 标记 → block 追加到末尾（自动补换行，用户内容逐字零改动）；
//   - 含完整标记（begin 在 end 之前）→ 仅替换 begin..end 区间为 block（区间外逐字
//     保留；end 之后的换行属区外原文不动，防重跑尾换行累加破坏幂等——b7b-7 回归）；
//   - 半截/逆序标记 → 报错，调用方不得写盘（交用户手动整理）。
//
// 返回合并后全文与是否发生变更；无变更时调用方计跳过（增量幂等硬判据）。
func markerBlock(existing, block, begin, end string) (string, bool, error) {
	b := strings.Index(existing, begin)
	e := strings.Index(existing, end)
	switch {
	case b < 0 && e < 0: // 态②：末尾追加，用户内容零改动
		appended := existing
		if !strings.HasSuffix(appended, "\n") {
			appended += "\n"
		}
		return appended + "\n" + block, true, nil
	case b >= 0 && e > b: // 态③：仅刷新区块区间，区块外逐字保留
		refreshed := existing[:b] + strings.TrimRight(block, "\n") + existing[e+len(end):]
		return refreshed, refreshed != existing, nil
	default: // 半截（恰好其一）或逆序（end 在 begin 前）= 异常状态
		return existing, false, fmt.Errorf("含不完整的标记区块（begin=%v end=%v），请手动整理标记后重试", b >= 0, e >= 0)
	}
}

// applyManagedBlock AGENTS.md/CLAUDE.md 自管区块三态落盘（b9 双写同款一句话区块，
// W2④；机制在 markerBlock/applyMarkedFile，本函数管文件读取与「不存在→新建」态）：
// ①不存在→新建（header 为新文件头，如 AGENTS.md 的 # 标题；CLAUDE.md 区块即全文）；
// ②存在无标记→区块追加到末尾（用户内容逐字保留）；③存在含标记→仅刷新区间，
// 内容无变化计跳过；畸形标记报错且原文不写回。legacyLines 为 init 历史自产行
// （如 b6 CLAUDE.md 指针行），落区块前逐行精确剔除（自产内容自清理，其余内容
// 零触碰）；剔除后仅余空白视同空文件按①形态重写。
func applyManagedBlock(dir, file, header, block string, legacyLines []string, rep *initReport) error {
	path := filepath.Join(dir, file)
	existing, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, []byte(header+block), 0o644); err != nil {
			return fmt.Errorf("写 %s 失败: %w", file, err)
		}
		rep.created = append(rep.created, file)
		return nil
	}
	if err != nil {
		return fmt.Errorf("读 %s 失败: %w", file, err)
	}
	content := string(existing)
	if len(legacyLines) > 0 {
		stripped, n := stripLegacyLines(content, legacyLines)
		content = stripped
		if n > 0 { // 仅真剔除过自产行才剥前导空行；零命中时用户内容（含前导空白）零触碰
			content = strings.TrimLeft(content, "\n")
		}
	}
	if strings.TrimSpace(content) == "" { // 剔除后仅余空白：按新建形态重写（防区块前叠空行）
		if err := os.WriteFile(path, []byte(header+block), 0o644); err != nil {
			return fmt.Errorf("写 %s 失败: %w", file, err)
		}
		rep.updated = append(rep.updated, file+"（写入aiteam 区块）")
		return nil
	}
	return applyMarkedFile(path, file, content, block,
		agentsBegin, agentsEnd, "aiteam 区块", rep)
}

// stripLegacyLines 逐行剔除 init 历史自产行（TrimSpace 后与常量全等匹配，容忍
// 行尾 \r 与首尾空白差异），其余行一字不动；返回剔除后的内容与剔除行数。
func stripLegacyLines(content string, legacy []string) (string, int) {
	var kept []string
	dropped := 0
	for _, ln := range strings.Split(content, "\n") {
		drop := false
		for _, l := range legacy {
			if strings.TrimSpace(ln) == l {
				drop = true
				break
			}
		}
		if drop {
			dropped++
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n"), dropped
}

// applyDevGuide docs/dev-guide/base.md 工程纪律落盘（拓扑节三态与 AGENTS.md 同构）：
// ①不存在→渲染整模板落盘（拓扑区块按 --topology 口径注入）；②存在无拓扑标记→
// 区块追加末尾（用户其余内容零触碰）；③存在含标记→仅刷新拓扑区间，内容无变化
// 计跳过；畸形标记报错且原文不写回。
func applyDevGuide(dir string, values map[string]string, topology string, rep *initReport) error {
	block, err := topologyBlock(topology, values)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "docs", "dev-guide", "base.md")
	existing, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) { // 态①：整模板渲染落盘
		templates, err := fs.Sub(aiteam.TemplateFS, templateDirPrefix)
		if err != nil {
			return fmt.Errorf("内嵌模板不可用: %w", err)
		}
		tpl, err := fs.ReadFile(templates, devGuideTemplate)
		if err != nil {
			return fmt.Errorf("读取内嵌模板 %s 失败: %w", devGuideTemplate, err)
		}
		// 先注入拓扑区块再渲染占位符：{{TOPOLOGY_BLOCK}} 不入 buildValues 值系
		// （区块按 flag 选 embed 变体属节级组合，非 token 取值），先替换防
		// renderTemplate 兜底把它吃成 TODO 文案。
		rendered := renderTemplate(strings.ReplaceAll(string(tpl), "{{TOPOLOGY_BLOCK}}", block), values)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
			return fmt.Errorf("写 docs/dev-guide/base.md 失败: %w", err)
		}
		rep.created = append(rep.created, "docs/dev-guide/base.md")
		return nil
	}
	if err != nil {
		return fmt.Errorf("读 docs/dev-guide/base.md 失败: %w", err)
	}
	return applyMarkedFile(path, "docs/dev-guide/base.md", string(existing), block,
		topologyBegin, topologyEnd, "拓扑节", rep)
}

// applyMarkedFile 标记区块三态公共落盘链（文件已存在，读取与「不存在→新建」态由
// 调用方自理——两文件新建形态不同）：markerBlock 合并 → 按变更与否写回并计数。
// display 为报告展示名，blockLabel 用于跳过/更新文案（「aiteam 区块」「拓扑节」）。
func applyMarkedFile(path, display, content, block, begin, end, blockLabel string, rep *initReport) error {
	merged, changed, err := markerBlock(content, block, begin, end)
	if err != nil {
		return fmt.Errorf("%s %w", display, err)
	}
	if !changed {
		rep.skipped = append(rep.skipped, display+"（"+blockLabel+"已是最新的）")
		return nil
	}
	if err := os.WriteFile(path, []byte(merged), 0o644); err != nil {
		return fmt.Errorf("写 %s 失败: %w", display, err)
	}
	rep.updated = append(rep.updated, display+"（写入"+blockLabel+"）")
	return nil
}

// topologyBlock 组装拓扑节标记区块全文（begin+变体正文+end）。变体正文过一遍
// renderTemplate：当前正文零占位符为恒等，未来正文引入 {{TOKEN}} 时自动承接渲染，
// 无需改本函数。
func topologyBlock(topology string, values map[string]string) (string, error) {
	fname, ok := topologyPolicies[topology]
	if !ok { // flag 层已校验，此处防御 embed 表与校验面漂移
		return "", fmt.Errorf("未知拓扑口径 %q（仅支持 single|distributed）", topology)
	}
	templates, err := fs.Sub(aiteam.TemplateFS, templateDirPrefix)
	if err != nil {
		return "", fmt.Errorf("内嵌模板不可用: %w", err)
	}
	src, err := fs.ReadFile(templates, fname)
	if err != nil {
		return "", fmt.Errorf("读取内嵌拓扑节 %s 失败: %w", fname, err)
	}
	body := strings.TrimRight(renderTemplate(string(src), values), "\n")
	return topologyBegin + "\n" + body + "\n" + topologyEnd + "\n", nil
}

// applyOnboardingDoc .aiteam/onboarding.md 落盘（b9 通讯域全集，形态分叉引导——
// 用户 2026-10-07 拍板改判 b9 两形态同款）：onboarding 模板一源一渲染——模板自带
// 文件级 aiteam 标记对（b6 段级标记裁剪/形态分支机制废止），占位符替换后整文件
// 写出。comm-only 原样（无 planning 面，引导即悬空）；full 在「开工第一读」补
// 方法论第二站（project-status 红绿灯+methodology 全文——stdout「下一步」是一次性
// 输出，持久引导链 AGENTS→onboarding 必须自含此站，防播好的 planning 面成孤儿）。
// writeIfAbsent 幂等：已存在一律整文件跳过——升级重跑不覆盖已有文件，既有内容与
// 区间外自行扩展的保护来自整文件跳过而非标记机制；升级=手动删除后重跑 init 重新
// 播种。
func applyOnboardingDoc(dir string, values map[string]string, commOnly bool, rep *initReport) error {
	templates, err := fs.Sub(aiteam.TemplateFS, templateDirPrefix)
	if err != nil {
		return fmt.Errorf("内嵌模板不可用: %w", err)
	}
	src, err := fs.ReadFile(templates, onboardingTemplate)
	if err != nil {
		return fmt.Errorf("读取内嵌模板 %s 失败: %w", onboardingTemplate, err)
	}
	rendered := renderTemplate(string(src), values)
	if !commOnly {
		// 完整形态开工第一读补方法论第二站（锚字面唯一——变体段措辞不同）：
		// comm-only 无 docs/planning/ 面原样跳过（引用悬空=缺陷）。
		const anchor = "接入信息+哨兵与值守纪律（见下）读毕即可开工"
		const guide = "接入信息+哨兵与值守纪律（见下）读毕即可接通通讯\n" +
			"2. 方法论流水线（init 完整形态，已播种 docs/planning/）：开工第一读=docs/planning/project-status.md（红绿灯：角色+允许/禁止动作+冷启动引导）→docs/planning/methodology.md（方法论全文）——读毕再开工"
		rendered = strings.Replace(rendered, anchor, guide, 1)
	}
	return writeIfAbsent(filepath.Join(dir, ".aiteam", onboardingTemplate),
		onboardingRel, []byte(rendered), rep)
}

// seedTemplateDir _template 整目录原样播种（b10 W1）：fs.WalkDir TemplateFS 子树，
// 读 embed 源字节直写目标项目 docs/planning/_template/ 同名相对路径——不渲染
// （模板是供复制的源，占位符必须原样保留），不挑拣（目录内全为模板 .md，整目录
// 搬运与单源 embed 惯例同构）。逐文件幂等：目标已存在跳过不动；新增/跳过件数计
// 入 initReport 计数（报告按模板件组汇总一行，不逐件刷屏）。写失败上抛 error。
func seedTemplateDir(dir string, rep *initReport) error {
	return fs.WalkDir(aiteam.TemplateFS, templateDirPrefix, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(templateDirPrefix, path)
		if err != nil {
			return err
		}
		data, err := fs.ReadFile(aiteam.TemplateFS, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, "docs", "planning", "_template", filepath.FromSlash(rel))
		if _, serr := os.Stat(dst); serr == nil {
			rep.tplSkipped++
			return nil
		}
		if merr := os.MkdirAll(filepath.Dir(dst), 0o755); merr != nil {
			return merr
		}
		if werr := os.WriteFile(dst, data, 0o644); werr != nil {
			return fmt.Errorf("写 %s 失败: %w", filepath.ToSlash(rel), werr)
		}
		rep.tplCreated++
		return nil
	})
}

// copySkills 技能包落地 <dir>/.agents/skills/（merge 语义）：逐文件复制，目标
// 已存在一律保留现有版——同名技能多为用户已装的同源技能（如 superpowers 系），
// 用户版本优先绝不覆盖，只新增缺失文件；已装全套时零写入纯计数。返回（新增数,
// 保留数）供汇总报告。
func copySkills(dir string) (newN, keptN int, err error) {
	err = fs.WalkDir(assets.SkillsFS, "skills", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, rerr := fs.ReadFile(assets.SkillsFS, path)
		if rerr != nil {
			return rerr
		}
		dst := filepath.Join(dir, ".agents", filepath.FromSlash(path))
		if _, serr := os.Stat(dst); serr == nil {
			keptN++
			return nil
		}
		if merr := os.MkdirAll(filepath.Dir(dst), 0o755); merr != nil {
			return merr
		}
		if werr := os.WriteFile(dst, data, 0o644); werr != nil {
			return werr
		}
		newN++
		return nil
	})
	return newN, keptN, err
}

// installHook post-commit hook 安装（AC23.1/B8-T2 自动层）。前提链：
// ①CWD 是 git 仓根（.git 为目录——worktree 的 .git 是文件同样跳过，hooks 归主仓管；
//
//	非 git 目录跳过保护 init 在任意目录可用）；②progress.auto_hook=on（配置探测
//	保持 CWD：aiteam-config.json 存在则加载，否则内建默认 on。init 为维护命令，
//	CWD 语义不动；serve 序 9 起已改 exe 目录基准，两者不再同构）。
//
// 安装幂等：post-commit 已存在一律跳过不覆盖（用户可能自有钩子，init 绝不夺权；
//
//	与 planning 活文档同级保护）。安装/跳过各计 initReport 一行，跳过行带原因
//	（「已存在不覆盖」/「auto_hook=off」/「非 git 仓根」三态可辨）。
func installHook(dir string, rep *initReport) error {
	display := ".git/hooks/post-commit"
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		rep.skipped = append(rep.skipped, display+"（非 git 仓根，未安装）")
		rep.hookEnvSkipped++ // b3：环境性跳过，不计入升级提示 N（摘要「跳过 N」口径零回归）
		return nil
	}
	cfgPath := ""
	if _, err := os.Stat(filepath.Join(dir, defaultConfigFile)); err == nil {
		cfgPath = defaultConfigFile
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}
	if !cfg.Progress.AutoHook {
		rep.skipped = append(rep.skipped, display+"（progress.auto_hook=off）")
		rep.hookEnvSkipped++ // b3：环境性跳过，不计入升级提示 N
		return nil
	}
	path := filepath.Join(dir, ".git", "hooks", "post-commit")
	if _, err := os.Stat(path); err == nil {
		rep.skipped = append(rep.skipped, display+"（已存在不覆盖）")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建 hooks 目录失败: %w", err)
	}
	// CRLF 防御：hook 由 shell 解释，shebang 行混入 \r 即 127；源文件在 Windows
	// checkout（autocrlf）下 raw string 常量可能携带 CRLF，写盘前统一归 LF。
	script := strings.ReplaceAll(hookScript, "\r\n", "\n")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return fmt.Errorf("写 post-commit hook 失败: %w", err)
	}
	rep.created = append(rep.created, display)
	return nil
}

// writeIfAbsent 幂等写盘：目标已存在一律跳过不动（planning 是活文档，绝不覆盖）；
// 仅不存在时创建父目录并写出，计入生成清单。display 为报告展示名（/ 分隔）。
// 写失败上抛 error（fail-fast，不静默吞掉半成品产物）。
func writeIfAbsent(path, display string, data []byte, rep *initReport) error {
	if _, err := os.Stat(path); err == nil {
		rep.skipped = append(rep.skipped, display)
		return nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("写 %s 失败: %w", display, err)
	}
	rep.created = append(rep.created, display)
	return nil
}

// writeReport 汇总输出（生成 N/更新 K/跳过 M 统计口径含模板件）+ 分三组展示：
// 实况件（+/~ /= 前缀区分生成/更新/跳过）、模板件（_template 播种汇总一行，不逐件
// 刷屏）、技能包 + 下一步指引。
func writeReport(w io.Writer, rep *initReport, skillsInstalled bool, skillsNew, skillsKept int, commOnly bool) {
	fmt.Fprintf(w, "aiteam init 完成：生成 %d，更新 %d，跳过 %d\n",
		len(rep.created)+rep.tplCreated, len(rep.updated), len(rep.skipped)+rep.tplSkipped)
	// b3 升级提示：跳过件存在时贴着摘要行（误读点）输出一行升级指引（哑计数→显式
	// 指引）。N 口径=实况件存在性跳过（排除 hook 环境性跳过）+模板件跳过+技能包
	// 保留；hook 环境跳过仍照旧计入摘要「跳过 N」，只有本行排除。位置硬约束：
	// writeReport 内、摘要行后第一行、comm-only 提前 return 之前（spec §7 裁量点 3）。
	if hintN := len(rep.skipped) - rep.hookEnvSkipped + rep.tplSkipped + skillsKept; hintN > 0 {
		fmt.Fprintf(w, "升级提示: 本次跳过已存在产物 %d 件（幂等保护不覆盖）——要更新某件=手动删除该文件后重跑 init\n", hintN)
	}
	if n := len(rep.created) + len(rep.updated) + len(rep.skipped); n > 0 {
		fmt.Fprintln(w, "实况件:")
		for _, s := range rep.created {
			fmt.Fprintf(w, "  + %s\n", s)
		}
		for _, s := range rep.updated {
			fmt.Fprintf(w, "  ~ %s\n", s)
		}
		for _, s := range rep.skipped {
			fmt.Fprintf(w, "  = %s\n", s)
		}
	}
	if rep.tplCreated > 0 || rep.tplSkipped > 0 {
		fmt.Fprintf(w, "模板件: docs/planning/_template/ 新增 %d 件，已存在跳过 %d 件（原样播种，占位符不渲染，供复制取用）\n",
			rep.tplCreated, rep.tplSkipped)
	}
	if skillsInstalled {
		fmt.Fprintf(w, "技能包 .agents/skills/: 新增 %d，保留现有 %d（同名不覆盖=你的版本优先；要换 aiteam 版=删该技能目录后重跑 init）\n", skillsNew, skillsKept)
		fmt.Fprintln(w, "技能是执行者契约引用的 Skill 实体——支持 .agents/skills 惯例的客户端自动发现；其余客户端按其技能目录机制接入（如 Claude Code 链接到 .claude/skills）")
	}
	if commOnly {
		// --comm-only 形态：方法论产物未装，下一步不含 planning 指引
		//（引用未生成的 docs/planning/* 会悬空——FX 断点⑥同批修正）。
		fmt.Fprintln(w, `下一步（通讯形态）:
  1. 服务机启动中枢: aiteam serve（默认监听 0.0.0.0:8310；改 .aiteam/cli.json 的 server/token 后重启生效）
  2. 登记项目与栏目: aiteam project register / aiteam column register（身份四参见 AGENTS.md aiteam 区块）
  3. AI 会话开窗: 打开 .aiteam/onboarding.md 按身份选变体，替换尖括号占位后全文贴窗（接入与哨兵纪律都在里面）。
  升级 onboarding: 手动删除 .aiteam/onboarding.md 后重跑 init --comm-only（增量补全不覆盖已有文件）。`)
		return
	}
	fmt.Fprintln(w, `下一步:
  1. 服务机启动中枢: aiteam serve（默认监听 0.0.0.0:8310）
  2. 登记项目: aiteam project register --project <code> --name <名称>
  3. 开工第一读: docs/planning/project-status.md（红绿灯：角色+允许/禁止动作+冷启动引导）
  4. 开总控会话窗口: 粘贴 .aiteam/onboarding.md 「一、总控版」全文+docs/planning/methodology.md 「一、方法论段」总控补全（两段连读），开工。
  升级单个模板: 手动删除该文件后重跑 init（增量补全不覆盖已有文件，绝不覆盖活文档）。`)
}
