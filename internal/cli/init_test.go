package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	aiteam "aiteam"
)

// wantPlanningFiles init 完整形态渲染产物清单（slash 相对路径全集；b10 播种面
// 重排：项目级件留 docs/planning/ 根——b9 三件+新增 maintenance/CONTEXT/
// s0-s7-stages，迭代四件+specs 登记簿移位 iterations/v0.1/——与 init.go
// planningArtifacts 落点一一对应）。
var wantPlanningFiles = []string{
	"docs/planning/project-status.md",
	"docs/planning/executor-contract.md",
	"docs/planning/methodology.md",
	"docs/planning/maintenance.md",
	"docs/planning/CONTEXT.md",
	"docs/planning/s0-s7-stages.md",
	"docs/planning/iterations/v0.1/charter.md",
	"docs/planning/iterations/v0.1/PRD.md",
	"docs/planning/iterations/v0.1/tech-design.md",
	"docs/planning/iterations/v0.1/development-task.md",
	"docs/planning/iterations/v0.1/specs/INDEX.md",
}

// TestWantPlanningFilesMatchesArtifacts 双清单一致性对拍（b10 质量审查建议②）：
// wantPlanningFiles（测试侧渲染产物清单）与 init.go planningArtifacts（实现侧
// 播种面）必须一一对应——init.go 增删播种件而测试清单漏跟的静默漂移在此爆红。
func TestWantPlanningFilesMatchesArtifacts(t *testing.T) {
	if len(wantPlanningFiles) != len(planningArtifacts) {
		t.Fatalf("双清单长度漂移: wantPlanningFiles=%d planningArtifacts=%d",
			len(wantPlanningFiles), len(planningArtifacts))
	}
	want := make(map[string]bool, len(wantPlanningFiles))
	for _, rel := range wantPlanningFiles {
		want[rel] = true
	}
	for _, art := range planningArtifacts {
		rel := "docs/planning/" + art.dst
		if !want[rel] {
			t.Errorf("planningArtifacts 落点 %s 不在 wantPlanningFiles（测试清单漏跟）", rel)
			continue
		}
		delete(want, rel)
	}
	for rel := range want {
		t.Errorf("wantPlanningFiles 的 %s 不在 planningArtifacts（实现侧无此落点）", rel)
	}
}

// runInitForTest 在 dir 内执行一次 init（chdir 隔离 + stdout 捕获），返回命令输出。
func runInitForTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	chdirForTest(t, dir)
	var out bytes.Buffer
	if err := runInit(args, &out); err != nil {
		t.Fatalf("init 执行失败: %v", err)
	}
	return out.String()
}

// snapshotTree 递归快照 dir 下全部常规文件（相对路径→内容 sha256），用于幂等比对。
func snapshotTree(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	snap := map[string][32]byte{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		snap[rel] = sha256.Sum256(data)
		return nil
	})
	if err != nil {
		t.Fatalf("快照 %s 失败: %v", dir, err)
	}
	return snap
}

// readProjectFile 读目标项目内相对路径文件内容，缺失即判失败。
func readProjectFile(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读取产物 %s 失败: %v", rel, err)
	}
	return string(data)
}

// TestInitGeneratesSkeleton AC：init 后骨架齐——planning 渲染件全集（b10：项目级
// 件+maintenance/CONTEXT/s0-s7-stages+iterations/v0.1 迭代骨架）+.aiteam/cli.json+
// AGENTS.md+CLAUDE.md 在位且非空。
func TestInitGeneratesSkeleton(t *testing.T) {
	dir := t.TempDir()
	out := runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")

	for _, rel := range wantPlanningFiles {
		if got := readProjectFile(t, dir, rel); strings.TrimSpace(got) == "" {
			t.Errorf("产物 %s 为空", rel)
		}
	}
	for _, rel := range []string{".aiteam/cli.json", "AGENTS.md", "CLAUDE.md"} {
		if got := readProjectFile(t, dir, rel); strings.TrimSpace(got) == "" {
			t.Errorf("产物 %s 为空", rel)
		}
	}

	// 渲染落点抽查：项目名与 --name 参数一致；cli.json 含 server 字段（技术设计 §4.1 形态）
	if got := readProjectFile(t, dir, "docs/planning/project-status.md"); !strings.Contains(got, "示例项目") {
		t.Errorf("project-status.md 未按 --name 渲染项目名")
	}
	if got := readProjectFile(t, dir, ".aiteam/cli.json"); !strings.Contains(got, `"server"`) || !strings.Contains(got, `"token"`) {
		t.Errorf("cli.json 缺 server/token 字段（技术设计 §4.1 形态）: %s", got)
	}
	if !strings.Contains(out, "生成") {
		t.Errorf("汇总输出缺「生成」统计: %s", out)
	}
}

// TestInitNoPlaceholderResidue AC 硬判据：渲染产物零 {{ 残留。
// 范围=init 渲染的产物（onboarding+planning 渲染件全集含 b10 迭代骨架+dev-guide+
// cli.json+AGENTS.md+CLAUDE.md）；默认安装的技能包是方法论原文（自带 {{ 示例）原样
// 落地不属渲染责任、_template 播种件是模板源（占位符必须原样保留）——两者均不在
// 检查范围。
func TestInitNoPlaceholderResidue(t *testing.T) {
	dir := t.TempDir()
	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")

	rendered := []string{
		b9OnboardingRel, // b9：两形态同款通讯全集产物同样走渲染，纳入零 {{ 检查
		".aiteam/cli.json",
		"AGENTS.md",
		"CLAUDE.md",
		"docs/dev-guide/base.md", // B8-4：拓扑区块注入+占位符渲染后同样零 {{ 残留
	}
	rendered = append(rendered, wantPlanningFiles...)
	for _, rel := range rendered {
		if data := readProjectFile(t, dir, rel); strings.Contains(data, "{{") {
			t.Errorf("渲染产物 %s 残留占位符 {{", rel)
		}
	}
}

// TestInitIdempotent 增量幂等硬判据：跑两遍——第二遍零变更（全树 hash 一致）+
// 汇总「生成 0/更新 0/跳过 M」。
func TestInitIdempotent(t *testing.T) {
	dir := t.TempDir()
	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")
	before := snapshotTree(t, dir)

	out2 := runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")
	after := snapshotTree(t, dir)

	if len(before) != len(after) {
		t.Fatalf("第二遍文件数变化: before=%d after=%d", len(before), len(after))
	}
	for rel, sum := range before {
		if after[rel] != sum {
			t.Errorf("第二遍变更了 %s（planning 是活文档，init 不得覆盖）", rel)
		}
	}
	if !strings.Contains(out2, "生成 0") {
		t.Errorf("第二遍汇总应「生成 0」: %s", out2)
	}
	if !strings.Contains(out2, "更新 0") {
		t.Errorf("第二遍汇总应「更新 0」: %s", out2)
	}
	if !strings.Contains(out2, "跳过") {
		t.Errorf("第二遍汇总应含跳过统计: %s", out2)
	}
}

// assertOneLinerBlock b9 一句话区块形态断言（AC1）：标记对+一句话引导——含
// .aiteam/onboarding.md 引导路径；大区块旧正文零残留（接入小节/哨兵纪律正文/
// planning 指针/官方链接行——正文唯一源收敛在 onboarding.md，区块恒定）。
func assertOneLinerBlock(t *testing.T, agents string) {
	t.Helper()
	begin := "<!-- aiteam:begin (do not edit between these markers) -->"
	end := "<!-- aiteam:end -->"
	if !strings.Contains(agents, begin) || !strings.Contains(agents, end) {
		t.Errorf("缺 aiteam 标记对（begin/end）:\n%s", agents)
	}
	if !strings.Contains(agents, ".aiteam/onboarding.md") {
		t.Errorf("区块缺 .aiteam/onboarding.md 引导路径（AC1）:\n%s", agents)
	}
	for _, banned := range []string{
		"### ",                             // 大区块小节头（接入/开工第一读/铁律/哨兵）——一句话形态无小节
		"docs/planning/",                   // planning 指针（方法论域引用不进通讯区块）
		"`send`",                           // 五命令正文（一句话只留指针词）
		"纯传呼机",                             // 哨兵纪律正文
		"docs/接入指南.md",                     // 官方指引行（收敛进 onboarding.md）
		"禁止 push", "git add", "中文 message", // 铁律旧条文（b6 W1/R2 清退，防回归）
		"本仓无远端", "git diff --cached", // vcsRule/stagingRule 残留探测（b6 W5 清退）
		"executor_<标识>", // 身份规约正文
	} {
		if strings.Contains(agents, banned) {
			t.Errorf("区块含一句话形态不应有的内容 %q（AC1：正文收敛 onboarding.md）:\n%s", banned, agents)
		}
	}
}

// TestAgentsBlockThreeStates AGENTS.md 区块管理三态（b9 一句话区块下语义重定义：
// 区块内容恒定，断言聚焦三态机制本身）：
// ①无文件→新建含标记；②已存在无标记→用户内容逐字保留+区块末尾追加；
// ③已存在含标记→区块内刷新、区块外哨兵逐字不动；畸形标记→报错且原文不写回。
func TestAgentsBlockThreeStates(t *testing.T) {
	const (
		begin = "<!-- aiteam:begin (do not edit between these markers) -->"
		end   = "<!-- aiteam:end -->"
	)

	// 态①：无文件→新建含 begin/end 标记
	d1 := t.TempDir()
	runInitForTest(t, d1, "--project", "proj-a", "--name", "示例项目")
	agents := readProjectFile(t, d1, "AGENTS.md")
	if !strings.Contains(agents, begin) || !strings.Contains(agents, end) {
		t.Errorf("态①新建 AGENTS.md 缺 aiteam 标记区块:\n%s", agents)
	}
	assertOneLinerBlock(t, agents)

	// 态②：已存在无标记→用户内容原样保留（在文件前部），区块追加在末尾
	d2 := t.TempDir()
	userContent := "# 我的项目说明\n\n这是用户自己维护的内容，init 不得改动。\n"
	if err := os.WriteFile(filepath.Join(d2, "AGENTS.md"), []byte(userContent), 0o644); err != nil {
		t.Fatalf("预置用户 AGENTS.md 失败: %v", err)
	}
	runInitForTest(t, d2, "--project", "proj-a", "--name", "示例项目")
	agents2 := readProjectFile(t, d2, "AGENTS.md")
	if !strings.HasPrefix(agents2, userContent) {
		t.Errorf("态②用户内容被改动（应逐字保留在文件前部）:\n%s", agents2)
	}
	idxUser := strings.Index(agents2, userContent)
	idxBegin := strings.Index(agents2, begin)
	if idxBegin < 0 || idxBegin < idxUser {
		t.Errorf("态②区块应追加在用户内容之后")
	}

	// 态③：已存在含标记→区块内刷新（旧区块内容替换），区块外哨兵逐字不动
	d3 := t.TempDir()
	sentinelHead := "哨兵头部内容 marker-head\n"
	sentinelTail := "\n哨兵尾部内容 marker-tail\n"
	oldBlock := begin + "\n旧区块内容 old-block-content\n" + end + "\n"
	if err := os.WriteFile(filepath.Join(d3, "AGENTS.md"), []byte(sentinelHead+oldBlock+sentinelTail), 0o644); err != nil {
		t.Fatalf("预置含标记 AGENTS.md 失败: %v", err)
	}
	runInitForTest(t, d3, "--project", "proj-a", "--name", "示例项目")
	agents3 := readProjectFile(t, d3, "AGENTS.md")
	if !strings.Contains(agents3, "marker-head") || !strings.Contains(agents3, "marker-tail") {
		t.Errorf("态③区块外哨兵内容被触碰:\n%s", agents3)
	}
	if strings.Contains(agents3, "old-block-content") {
		t.Errorf("态③旧区块内容未刷新:\n%s", agents3)
	}
	if !strings.Contains(agents3, ".aiteam/onboarding.md") {
		t.Errorf("态③区块内应刷新为一句话引导形态（onboarding.md 引导路径）:\n%s", agents3)
	}
	if strings.Count(agents3, begin) != 1 || strings.Count(agents3, end) != 1 {
		t.Errorf("态③标记应恰好各一处:\n%s", agents3)
	}
	// 态③刷新后新区块为一句话形态（b9：区块演进随刷新传播）。
	assertOneLinerBlock(t, agents3)

	// 边界回归（b7b-7 质量审查）：半截/逆序标记三种畸形——runInit 必须报错
	// 且 AGENTS.md 原文逐字未改写（锁死 e>b 边界条件，防重构静默回退成误追加/误刷新）。
	malformed := []struct {
		title   string
		content string
	}{
		{"只有 begin", "原文甲\n" + begin + "\n残块\n"},
		{"只有 end", "原文乙\n" + end + "\n"},
		{"end 在 begin 前", "原文丙\n" + end + "\n" + begin + "\n"},
	}
	for _, tc := range malformed {
		d := t.TempDir()
		chdirForTest(t, d)
		if err := os.WriteFile("AGENTS.md", []byte(tc.content), 0o644); err != nil {
			t.Fatalf("%s: 预置失败: %v", tc.title, err)
		}
		if err := runInit([]string{"--project", "proj-a", "--name", "示例项目"}, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: 应返回 error，实际 nil", tc.title)
		}
		got, err := os.ReadFile("AGENTS.md")
		if err != nil {
			t.Fatalf("%s: 读回失败: %v", tc.title, err)
		}
		if string(got) != tc.content {
			t.Errorf("%s: AGENTS.md 被改写——畸形标记必须原样保留交用户处理:\n%s", tc.title, got)
		}
	}
}

// TestInitVcsVariants --vcs git vs svn 渲染差异可见：REMOTE_POLICY/BRANCH_PREFIX 两套口径，
// 两套产物均零 {{ 残留。
func TestInitVcsVariants(t *testing.T) {
	dGit := t.TempDir()
	runInitForTest(t, dGit, "--project", "proj-a", "--name", "示例项目", "--vcs", "git")
	contractGit := readProjectFile(t, dGit, "docs/planning/executor-contract.md")

	dSvn := t.TempDir()
	runInitForTest(t, dSvn, "--project", "proj-a", "--name", "示例项目", "--vcs", "svn")
	contractSvn := readProjectFile(t, dSvn, "docs/planning/executor-contract.md")

	// REMOTE_POLICY 渲染值落位断言锚更新（b6-spec W5：git 值中性化——去「本仓无远端；
	// 主干推进仅靠…」断言）。改查新口径特征词「push 权限由项目自行约定」：该词不在
	// 契约模板静态文本中，可证明渲染值真正落位而非模板残留（旧锚随中性化过时清退）。
	if !strings.Contains(contractGit, "push 权限由项目自行约定") {
		t.Errorf("git 口径 REMOTE_POLICY 渲染值未落位（W5 中性化口径）:\n%s", contractGit)
	}
	if !strings.Contains(contractSvn, "svn") || !strings.Contains(contractSvn, "update") {
		t.Errorf("svn 口径应含 svn update 纪律（总纲 VCS 适配节）:\n%s", contractSvn)
	}
	// BRANCH_PREFIX 两套口径：git=批次前缀（与模板 feat/ 拼接），svn=集中式无分支制提示
	// （feat/ 是模板静态文本，产物必然含——差异断言只看占位符渲染结果）。
	if !strings.Contains(contractGit, "<批次号>-") {
		t.Errorf("git 口径分支前缀应渲染批次制占位:\n%s", contractGit)
	}
	if !strings.Contains(contractSvn, "trunk") {
		t.Errorf("svn 口径分支前缀应渲染集中式无分支制提示:\n%s", contractSvn)
	}
	if contractGit == contractSvn {
		t.Errorf("git/svn 两套口径渲染结果应可见差异")
	}
}

// TestInitRequiredFlags 缺 --project/--name→ErrUsage；非法 --vcs→ErrUsage；位置参数→ErrUsage。
func TestInitRequiredFlags(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)

	cases := []struct {
		title string
		args  []string
	}{
		{"缺 --project", []string{"--name", "示例项目"}},
		{"缺 --name", []string{"--project", "proj-a"}},
		{"全缺", nil},
		{"非法 --vcs", []string{"--project", "proj-a", "--name", "示例项目", "--vcs", "hg"}},
		{"位置参数", []string{"--project", "proj-a", "--name", "示例项目", "extra"}},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		err := runInit(tc.args, &out)
		if !errors.Is(err, ErrUsage) {
			t.Errorf("%s: err = %v, 期望 ErrUsage", tc.title, err)
		}
	}
	// 用法错误不得产生任何产物
	if _, err := os.Stat(filepath.Join(dir, "docs")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("用法错误路径不应产出 docs/（stat err=%v）", err)
	}
}

// TestInitSkills 技能包默认装到 .agents/skills/；merge 语义三断言：同名保留
// 现有版（预置同名文件不被覆盖）、缺失新增、--no-skills 显式跳过；重复执行零生成。
func TestInitSkills(t *testing.T) {
	dir := t.TempDir()

	// 预置同名技能（模拟用户已装同源技能——merge 语义的保留侧）
	preset := filepath.Join(dir, ".agents", "skills", "test-driven-development")
	if err := os.MkdirAll(preset, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(preset, "SKILL.md"), []byte("用户已有版本"), 0o644); err != nil {
		t.Fatal(err)
	}

	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")

	// 保留侧：同名文件原样保留（用户版本优先）
	if got := readProjectFile(t, dir, ".agents/skills/test-driven-development/SKILL.md"); got != "用户已有版本" {
		t.Errorf("同名技能应保留用户版本, got %q", got)
	}
	// 新增侧：未预置的技能落地且非空
	skill := readProjectFile(t, dir, ".agents/skills/using-git-worktrees/SKILL.md")
	if strings.TrimSpace(skill) == "" {
		t.Errorf("技能包 SKILL.md 为空")
	}

	// 幂等：第二遍技能文件不重写（内容 hash 由 TestInitIdempotent 全树覆盖，此处验计数）
	out2 := runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")
	if !strings.Contains(out2, "生成 0") {
		t.Errorf("第二遍应零生成: %s", out2)
	}
	if !strings.Contains(out2, "保留现有") {
		t.Errorf("汇总应含技能保留计数: %s", out2)
	}
}

// TestInitNoSkills --no-skills 显式跳过技能包（.agents/skills 零产生）。
func TestInitNoSkills(t *testing.T) {
	dir := t.TempDir()
	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目", "--no-skills")
	if _, err := os.Stat(filepath.Join(dir, ".agents", "skills")); !os.IsNotExist(err) {
		t.Errorf("--no-skills 不应产生 .agents/skills: %v", err)
	}
}

// ---------- B8-4 init --topology：拓扑两口径 + 标记区块渲染（AC24.1/24.2） ----------

// dev-guide 拓扑节标记区块字面量（与实现侧常量同源口径；测试用字面量保证
// 实现侧常量漂移时测试仍然红——防实现自证）。
const (
	topoBegin = "<!-- aiteam:topology:begin -->"
	topoEnd   = "<!-- aiteam:topology:end -->"
)

// stripTopologyBlock 提取 dev-guide 内容中拓扑区块以外的部分（区块整段切除，
// 含标记行），并把 dir 路径归一化为占位（{{REPO_PATH}} 渲染自各自 TempDir，
// 两树路径必不同，归一后才能逐字比对）。
func stripTopologyBlock(t *testing.T, content, dir string) string {
	t.Helper()
	s := strings.ReplaceAll(content, dir, "<DIR>")
	b := strings.Index(s, topoBegin)
	e := strings.Index(s, topoEnd)
	if b < 0 || e < 0 || e < b {
		t.Fatalf("dev-guide 缺完整拓扑标记区块（b=%d e=%d）:\n%s", b, e, s)
	}
	return s[:b] + s[e+len(topoEnd):]
}

// TestTopologySingle AC24.1 单机口径：--topology single 生成的 dev-guide 含
// worktree 隔离纪律+拓扑标记区块，且全树不含多机口径特征词「push 频率」。
func TestTopologySingle(t *testing.T) {
	dir := t.TempDir()
	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目", "--topology", "single")

	base := readProjectFile(t, dir, "docs/dev-guide/base.md")
	if !strings.Contains(base, "worktree 隔离") {
		t.Errorf("single 口径应含 worktree 隔离纪律:\n%s", base)
	}
	if !strings.Contains(base, topoBegin) || !strings.Contains(base, topoEnd) {
		t.Errorf("dev-guide 缺拓扑标记区块:\n%s", base)
	}
	if strings.Contains(base, "push 频率") {
		t.Errorf("single 树不得残留多机口径特征词「push 频率」（AC24.1 互不残留）:\n%s", base)
	}
}

// TestTopologyDistributed AC24.1 多机口径：--topology distributed 生成的 dev-guide
// 含 WIP push 频率+fetch 前置纪律，且全树不含单机口径特征词「worktree 隔离」。
func TestTopologyDistributed(t *testing.T) {
	dir := t.TempDir()
	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目", "--topology", "distributed")

	base := readProjectFile(t, dir, "docs/dev-guide/base.md")
	if !strings.Contains(base, "push 频率") {
		t.Errorf("distributed 口径应含 WIP push 频率纪律:\n%s", base)
	}
	if !strings.Contains(base, "fetch") {
		t.Errorf("distributed 口径应含 fetch 前置自查纪律:\n%s", base)
	}
	if strings.Contains(base, "worktree 隔离") {
		t.Errorf("distributed 树不得残留单机口径特征词「worktree 隔离」（AC24.1 互不残留）:\n%s", base)
	}
	// 拓扑区块级互斥（比全树更严）：single 特有词不得渗入 distributed 区块内
	b := strings.Index(base, topoBegin)
	e := strings.Index(base, topoEnd)
	if b < 0 || e < 0 {
		t.Fatalf("dev-guide 缺拓扑标记区块:\n%s", base)
	}
	block := base[b:e]
	if strings.Contains(block, "worktree") {
		t.Errorf("distributed 拓扑区块内不得出现 worktree 字样（多机口径各自 clone，不走 worktree）:\n%s", block)
	}
}

// TestTopologyTreesDifferOnlyInBlock AC24.1 diff 断言：single 与 distributed 两树
// 的 dev-guide 除拓扑区块外逐字一致（归一化各自 TempDir 路径后比对）。
func TestTopologyTreesDifferOnlyInBlock(t *testing.T) {
	dSingle := t.TempDir()
	runInitForTest(t, dSingle, "--project", "proj-a", "--name", "示例项目", "--topology", "single")
	dDist := t.TempDir()
	runInitForTest(t, dDist, "--project", "proj-a", "--name", "示例项目", "--topology", "distributed")

	singleBase := readProjectFile(t, dSingle, "docs/dev-guide/base.md")
	distBase := readProjectFile(t, dDist, "docs/dev-guide/base.md")
	if stripTopologyBlock(t, singleBase, dSingle) != stripTopologyBlock(t, distBase, dDist) {
		t.Errorf("两口径 dev-guide 除拓扑区块外应逐字一致（AC24.1 仅拓扑节不同）")
	}
	if singleBase == distBase {
		t.Errorf("两口径 dev-guide 应可见差异")
	}
}

// TestTopologyRerun AC24.2：已 single 初始化项目重跑 --topology distributed——
// 拓扑节刷新为新口径，全树 sha256 对比仅 dev-guide 一文件差异（其余零漂移），
// 且 base.md 区块外部分零变更（其余节+用户增补零触碰）。
func TestTopologyRerun(t *testing.T) {
	dir := t.TempDir()
	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目", "--topology", "single")
	before := snapshotTree(t, dir)
	baseBefore := readProjectFile(t, dir, "docs/dev-guide/base.md")

	out2 := runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目", "--topology", "distributed")
	after := snapshotTree(t, dir)

	if len(before) != len(after) {
		t.Fatalf("重跑后文件数变化: before=%d after=%d", len(before), len(after))
	}
	drifted := 0
	for rel, sum := range before {
		if after[rel] != sum {
			drifted++
			if rel != filepath.FromSlash("docs/dev-guide/base.md") {
				t.Errorf("重跑变更了 %s（应仅 dev-guide 一文件差异，AC24.2 零漂移）", rel)
			}
		}
	}
	if drifted != 1 {
		t.Errorf("重跑应恰好变更 1 个文件（dev-guide），实际 %d", drifted)
	}

	// 拓扑节刷新为 distributed 口径；区块外零变更（其余节零触碰）
	baseAfter := readProjectFile(t, dir, "docs/dev-guide/base.md")
	if !strings.Contains(baseAfter, "push 频率") {
		t.Errorf("重跑后拓扑节应刷新为 distributed 口径:\n%s", baseAfter)
	}
	if strings.Contains(baseAfter, "worktree 隔离") {
		t.Errorf("重跑后 single 口径应被刷新掉（不得残留）:\n%s", baseAfter)
	}
	if stripTopologyBlock(t, baseBefore, dir) != stripTopologyBlock(t, baseAfter, dir) {
		t.Errorf("重跑后 base.md 区块外内容被触碰（AC24.2 其余节零变更）")
	}
	if !strings.Contains(out2, "更新") {
		t.Errorf("重跑汇总应含更新统计: %s", out2)
	}
}

// TestMarkerBlock dev-guide 拓扑节标记区块三态（与 AGENTS.md 三态机制同构）：
// ①文件不存在→生成含标记；②已存在无标记→用户内容逐字保留+区块末尾追加；
// ③已存在含标记→区块内刷新、区外哨兵逐字不动；畸形标记→报错且原文不写回。
func TestMarkerBlock(t *testing.T) {
	// 态②：已存在无标记的用户 dev-guide——init 不得改动用户内容，区块追加在末尾
	d2 := t.TempDir()
	userGuide := "# 我们团队的工程纪律\n\n这是用户自己维护的 dev-guide，init 不得改动。\n"
	if err := os.MkdirAll(filepath.Join(d2, "docs", "dev-guide"), 0o755); err != nil {
		t.Fatalf("预置目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(d2, "docs", "dev-guide", "base.md"), []byte(userGuide), 0o644); err != nil {
		t.Fatalf("预置用户 base.md 失败: %v", err)
	}
	runInitForTest(t, d2, "--project", "proj-a", "--name", "示例项目", "--topology", "single")
	got2 := readProjectFile(t, d2, "docs/dev-guide/base.md")
	if !strings.HasPrefix(got2, userGuide) {
		t.Errorf("态②用户内容被改动（应逐字保留在文件前部）:\n%s", got2)
	}
	idxUser := strings.Index(got2, userGuide)
	idxBegin := strings.Index(got2, topoBegin)
	if idxBegin < 0 || idxBegin < idxUser {
		t.Errorf("态②拓扑区块应追加在用户内容之后:\n%s", got2)
	}

	// 态③：已存在含标记——区块内刷新（旧口径换新口径），区外哨兵逐字不动
	d3 := t.TempDir()
	sentinelHead := "哨兵头部内容 guide-head\n"
	sentinelTail := "\n哨兵尾部内容 guide-tail\n"
	oldBlock := topoBegin + "\n旧拓扑内容 old-topology-content worktree 隔离\n" + topoEnd + "\n"
	if err := os.MkdirAll(filepath.Join(d3, "docs", "dev-guide"), 0o755); err != nil {
		t.Fatalf("预置目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(d3, "docs", "dev-guide", "base.md"),
		[]byte(sentinelHead+oldBlock+sentinelTail), 0o644); err != nil {
		t.Fatalf("预置含标记 base.md 失败: %v", err)
	}
	runInitForTest(t, d3, "--project", "proj-a", "--name", "示例项目", "--topology", "distributed")
	got3 := readProjectFile(t, d3, "docs/dev-guide/base.md")
	if !strings.Contains(got3, "guide-head") || !strings.Contains(got3, "guide-tail") {
		t.Errorf("态③区块外哨兵内容被触碰:\n%s", got3)
	}
	if strings.Contains(got3, "old-topology-content") {
		t.Errorf("态③旧拓扑内容未刷新:\n%s", got3)
	}
	if !strings.Contains(got3, "push 频率") {
		t.Errorf("态③区块内应按本次 --topology 口径刷新:\n%s", got3)
	}
	if strings.Count(got3, topoBegin) != 1 || strings.Count(got3, topoEnd) != 1 {
		t.Errorf("态③拓扑标记应恰好各一处:\n%s", got3)
	}

	// 畸形标记回归：半截/逆序——init 必须报错且 base.md 原文逐字未改写
	malformed := []struct {
		title   string
		content string
	}{
		{"只有 begin", "原文甲\n" + topoBegin + "\n残块\n"},
		{"只有 end", "原文乙\n" + topoEnd + "\n"},
		{"end 在 begin 前", "原文丙\n" + topoEnd + "\n" + topoBegin + "\n"},
	}
	for _, tc := range malformed {
		d := t.TempDir()
		chdirForTest(t, d)
		if err := os.MkdirAll(filepath.Join("docs", "dev-guide"), 0o755); err != nil {
			t.Fatalf("%s: 建目录失败: %v", tc.title, err)
		}
		if err := os.WriteFile(filepath.Join("docs", "dev-guide", "base.md"), []byte(tc.content), 0o644); err != nil {
			t.Fatalf("%s: 预置失败: %v", tc.title, err)
		}
		if err := runInit([]string{"--project", "proj-a", "--name", "示例项目", "--topology", "single"}, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: 应返回 error，实际 nil", tc.title)
		}
		got, err := os.ReadFile(filepath.Join("docs", "dev-guide", "base.md"))
		if err != nil {
			t.Fatalf("%s: 读回失败: %v", tc.title, err)
		}
		if string(got) != tc.content {
			t.Errorf("%s: base.md 被改写——畸形标记必须原样保留交用户处理:\n%s", tc.title, got)
		}
	}
}

// TestTopologyInvalidFlag 非法 --topology 值本地退 2（ErrUsage），不产生任何产物。
func TestTopologyInvalidFlag(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	var out bytes.Buffer
	err := runInit([]string{"--project", "proj-a", "--name", "示例项目", "--topology", "mercurial"}, &out)
	if !errors.Is(err, ErrUsage) {
		t.Errorf("非法 --topology: err = %v, 期望 ErrUsage", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "docs")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("用法错误路径不应产出 docs/（stat err=%v）", err)
	}
}

// TestPlaceholderCoverage 占位符键一致性（b7b-7 质量审查建议 2）：遍历 init 经
// buildValues 渲染的全部模板（planningArtifacts 全部模板源+onboarding，b9 起
// onboarding 承载 {{PROJECT_CODE}}）收集全部 {{TOKEN}} 实存集合，断言——
// ① todoPlaceholders 每个 key 都是模板实存占位符（模板改名后 map 死键立即爆红）；
// ② buildValues 全部 key 均在实存集合内（渲染值不落空）；
// ③ fallback 兜底数有下界记录（实存数-映射覆盖数的差值——模板大改/映射异常增长时
// 从下界破口暴露，防止死键与漏渲染静默积累）。
func TestPlaceholderCoverage(t *testing.T) {
	all := map[string]bool{} // 模板实存 {{TOKEN}} 全集
	scan := []string{onboardingTemplate}
	for _, art := range planningArtifacts {
		scan = append(scan, art.src)
	}
	for _, fname := range scan {
		src, err := aiteam.TemplateFS.ReadFile("docs/planning/_template/" + fname)
		if err != nil {
			t.Fatalf("读内嵌模板 %s 失败: %v", fname, err)
		}
		for _, tok := range placeholderRe.FindAllString(string(src), -1) {
			all[tok] = true
		}
	}
	if len(all) == 0 {
		t.Fatal("模板未收集到任何占位符——embed 面或模板路径异常")
	}

	// ① todoPlaceholders 无死键
	for name := range todoPlaceholders {
		tok := "{{" + name + "}}"
		if !all[tok] {
			t.Errorf("todoPlaceholders 死键 %s：模板已无此占位符（改名/删除后未同步映射表）", tok)
		}
	}

	// ② buildValues 键全部实存（含可推导 7 项+todoMap 展开）
	values := buildValues("proj-a", "示例项目", `/x/y`, vcsPolicies["git"])
	keys := make([]string, 0, len(values))
	for tok := range values {
		keys = append(keys, tok)
		if !all[tok] {
			t.Errorf("buildValues 键 %s 不在任何模板中（拼写漂移/模板已删该占位符）", tok)
		}
	}
	sort.Strings(keys) // 稳定遍历，仅供调试断点用

	// ③ fallback 兜底数下界：当前模板实存 ~200 种、映射覆盖 ~34，兜底 ~164；
	// 下界取保守值 50——跌破说明模板大面积改名（映射表需同步）或占位符被误清。
	fallback := len(all) - len(values)
	if fallback < 50 {
		t.Errorf("fallback 兜底数 = %d（实存 %d - 映射覆盖 %d），低于下界 50——模板/映射表大面积漂移，人工核对", fallback, len(all), len(values))
	}
}

// ---------- B8-5 init：post-commit hook 安装（AC23.1/B8-T2 自动层） ----------

// hookRel hook 安装产物相对路径（报告展示名与落盘同源）。
const hookRel = ".git/hooks/post-commit"

// hookPath 返回 dir 内 post-commit hook 绝对路径。
func hookPath(dir string) string {
	return filepath.Join(dir, ".git", "hooks", "post-commit")
}

// gitAvailable 环境有 git 才跑真仓用例（极简容器无 git 时跳过并说明——
// hook 用例全部依赖真仓形态，模拟不出 .git 目录语义）。
func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("环境无 git，跳过真仓 hook 用例")
	}
}

// initGitRepo 在 dir 建真 git 仓：init.defaultBranch 固定 feat-hook，不依赖
// 用户全局 init.defaultBranch 配置（老 git 不认该配置时落默认分支名，用例
// 对分支名的断言一律采 git 实际输出，不锚具体名）。
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	gitAvailable(t)
	if out, err := exec.Command("git", "-c", "init.defaultBranch=feat-hook", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init 失败: %v\n%s", err, out)
	}
}

// gitHookEnv hook 端到端用例的子进程环境：AITEAM_* 身份/地址/二进制三组 +
// git 提交身份（不依赖全局 git config）。addr 为测试 serve 地址，bin 为真构建
// 的 aiteam 二进制路径。
func gitHookEnv(bin, addr string) []string {
	return append(os.Environ(),
		"AITEAM_BIN="+bin,
		"AITEAM_SERVER="+addr,
		"AITEAM_PROJECT="+regAuthProject,
		"AITEAM_COLUMN="+regAuthColumn,
		"AITEAM_SESSION=hook-s1",
		"AITEAM_ROLE="+regAuthRole,
		"GIT_AUTHOR_NAME=hook-test",
		"GIT_AUTHOR_EMAIL=hook-test@test",
		"GIT_COMMITTER_NAME=hook-test",
		"GIT_COMMITTER_EMAIL=hook-test@test",
	)
}

// gitInRepo 在 repo 内执行 git 子命令（env 注入），返回 stdout（去尾换行）；
// 非零退出即判失败。
func gitInRepo(t *testing.T, repo string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v 失败: %v\n%s", args, err, out)
	}
	return strings.TrimRight(string(out), "\r\n")
}

// buildAiteamBin 构建真实 aiteam 二进制（TestHookRealRun 端到端用）：hook 内
// ${AITEAM_BIN} 指向它，走真 CLI→HTTP→store 全链（AC23.1 的「可见」才有含金量）。
func buildAiteamBin(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败，无法定位 module root")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // internal/cli → 仓根
	name := "aiteam-hook-test"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/aiteam")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建 aiteam 二进制失败: %v\n%s", err, out)
	}
	return bin
}

// TestHookInstalled AC23.1 前置：git 仓内 init 后 .git/hooks/post-commit 存在，
// 内容含自动层特征（采值+progress 调用+静默兜底），报告计入生成清单。
// 可执行位按平台断言：POSIX 断 0o111 任一执行位；Windows 下 Go 写 0o755 落盘
// 后 Stat 回读写位语义（exec 位弱语义），以内容特征断言兜底（git for windows
// 执行 hook 也不检查 exec 位）。
func TestHookInstalled(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	out := runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")

	data, err := os.ReadFile(hookPath(dir))
	if err != nil {
		t.Fatalf("init 后 hook 未安装: %v", err)
	}
	script := string(data)
	for _, want := range []string{
		"#!/bin/sh", "rev-parse HEAD", "--abbrev-ref", "progress",
		"--commit", "--branch", "AITEAM_BIN", "|| true", "exit 0",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("hook 脚本缺特征 %q\n%s", want, script)
		}
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(hookPath(dir))
		if err != nil {
			t.Fatalf("stat hook 失败: %v", err)
		}
		if fi.Mode()&0o111 == 0 {
			t.Errorf("hook 应带可执行位（0o755），实际 %v", fi.Mode())
		}
	}
	if !strings.Contains(out, hookRel) {
		t.Errorf("汇总输出应含 hook 安装清单行: %s", out)
	}
}

// TestHookIdempotent 幂等+不覆盖：重跑 init 不重复安装、用户对 hook 的自定义
// 改动原样保留（hook 语义=已存在即跳过，与 planning 活文档同级保护）。
func TestHookIdempotent(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")

	// 模拟用户改动：追加自定义行
	hp := hookPath(dir)
	orig, err := os.ReadFile(hp)
	if err != nil {
		t.Fatalf("读 hook 失败: %v", err)
	}
	customized := string(orig) + "# 用户自定义：本地告警钩子\n"
	if err := os.WriteFile(hp, []byte(customized), 0o755); err != nil {
		t.Fatalf("预置用户 hook 改动失败: %v", err)
	}

	out2 := runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")
	got, err := os.ReadFile(hp)
	if err != nil {
		t.Fatalf("重跑后读 hook 失败: %v", err)
	}
	if string(got) != customized {
		t.Errorf("重跑 init 覆盖了用户 hook 改动（应已存在即跳过）:\n%s", got)
	}
	if !strings.Contains(out2, "跳过") {
		t.Errorf("重跑汇总应含 hook 跳过统计: %s", out2)
	}
}

// TestHookDisabled AC23.1 后半：progress.auto_hook=off 时 init 不装 hook
// （配置探测与 serve 同构：CWD aiteam-config.json 存在则加载）。
func TestHookDisabled(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	cfg := `{"progress": {"auto_hook": false}}`
	if err := os.WriteFile(filepath.Join(dir, "aiteam-config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	out := runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")

	if _, err := os.Stat(hookPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("auto_hook=off 不应安装 hook（stat err=%v）", err)
	}
	if !strings.Contains(out, "auto_hook=off") {
		t.Errorf("汇总应说明 hook 因 auto_hook=off 跳过: %s", out)
	}
}

// TestHookRealRun AC23.1 端到端：临时真仓 init→真 git commit（hook 经 env 携带
// 身份/地址/真构建二进制）→10 秒内 progress list 可见该 hash+branch。
func TestHookRealRun(t *testing.T) {
	gitAvailable(t)
	addr, _ := startProgressTestServer(t)
	bin := buildAiteamBin(t)

	repo := t.TempDir()
	initGitRepo(t, repo)
	runInitForTest(t, repo, "--project", regAuthProject, "--name", "hook 端到端")

	env := gitHookEnv(bin, addr)
	gitInRepo(t, repo, env, "-c", "user.name=hook-test", "-c", "user.email=hook-test@test",
		"commit", "--allow-empty", "-m", "hook 端到端")
	hash := gitInRepo(t, repo, nil, "rev-parse", "HEAD")
	branch := gitInRepo(t, repo, nil, "rev-parse", "--abbrev-ref", "HEAD")

	// 10 秒轮询窗口（AC23.1 原文口径）：list 按 hook 会话过滤（身份一参两用）。
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for {
		stdout, _ := runWantCode(t, "progress list", 0, func() int {
			return runProgressList(ctx, progressIdentityArgs("hook-s1"), addr)
		})
		if strings.Contains(stdout, hash) && strings.Contains(stdout, branch) {
			return
		}
		last = stdout
		if time.Now().After(deadline) {
			t.Fatalf("10 秒内 list 未可见 hash=%s branch=%s，末次输出:\n%s", hash, branch, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestHookSilentFail 静默失败（FR23 自动层红线：进度是观测数据不挡工作流）：
// hook 内 aiteam 不可达（真二进制+确定性拒绝地址）→ 真 git commit 照常成功；
// 脱离 git 的 post-commit 容错兜底，直接执行 hook 断退出 0（环境有 sh 时）。
func TestHookSilentFail(t *testing.T) {
	gitAvailable(t)
	bin := buildAiteamBin(t)
	dead := closedServerAddr(t) // 确定性连接拒绝（aiteam 退 3 不可达语义）

	repo := t.TempDir()
	initGitRepo(t, repo)
	runInitForTest(t, repo, "--project", regAuthProject, "--name", "静默失败")

	env := gitHookEnv(bin, dead)
	gitInRepo(t, repo, env, "-c", "user.name=hook-test", "-c", "user.email=hook-test@test",
		"commit", "--allow-empty", "-m", "静默失败用例")
	if hash := gitInRepo(t, repo, nil, "rev-parse", "HEAD"); strings.TrimSpace(hash) == "" {
		t.Fatal("hook 失败场景下 commit 应成功产生（rev-parse 无输出）")
	}

	// 直接执行 hook 断退出 0：aiteam 失败被 || true + exit 0 兜住，不经 git 容错。
	if _, err := exec.LookPath("sh"); err == nil {
		cmd := exec.Command("sh", hookPath(repo))
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("hook 应退出 0 不阻塞，实际 err=%v\n%s", err, out)
		}
	}
}

// TestInitCommOnlyArtifactSurface --comm-only 产物面（#17 小批派工·FX 断点⑥；
// b9 AC4 四件）：AGENTS.md（一句话区块）+.aiteam/cli.json+CLAUDE.md（同款区块）+
// .aiteam/onboarding.md（b9 通讯域全集，两形态同款）四件在；planning 渲染件+
// _template 模板全套/dev-guide/skills 零新增（docs/planning 目录不存在）；二跑增量幂等零变更。
// 全装对照=既有 TestInit* 全套用例（不带 --comm-only 即全装形态，planning 在位
// 断言散布其间）。
func TestInitCommOnlyArtifactSurface(t *testing.T) {
	dir := t.TempDir()
	out := runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目", "--comm-only")

	// 通讯四件在位：AGENTS/CLAUDE 一句话区块+cli.json+onboarding 产物。
	agents := readProjectFile(t, dir, "AGENTS.md")
	assertOneLinerBlock(t, agents)
	readProjectFile(t, dir, ".aiteam/cli.json")
	readProjectFile(t, dir, "CLAUDE.md")
	readProjectFile(t, dir, b9OnboardingRel)

	// 方法论产物零新增：planning 目录不存在、dev-guide 不存在、.agents/skills 不存在。
	for _, rel := range []string{"docs/planning", "docs/dev-guide", ".agents/skills"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Errorf("--comm-only 不应产生 %s（方法论形态产物）: %v", rel, err)
		}
	}
	// 汇总须可辨通讯形态（下一步指引头「下一步（通讯形态）」）。
	if !strings.Contains(out, "通讯") {
		t.Errorf("comm-only 汇总输出缺通讯形态指引: %s", out)
	}

	// 二跑增量幂等：产物面不变（三态机制下区块刷新或跳过，planning 仍零新增）。
	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目", "--comm-only")
	for _, rel := range []string{"docs/planning", "docs/dev-guide"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Errorf("二跑不应新增 %s", rel)
		}
	}
	agents2 := readProjectFile(t, dir, "AGENTS.md")
	if agents2 != agents {
		t.Error("二跑 AGENTS.md 应逐字不变（区块内容无变化计跳过——增量幂等硬判据）")
	}
}

// ---------- b9 一句话区块：形态恒定性+接线面（语义承接 b6 W1/W2/W5 断言面；
// b6 的形态分支/哨兵纪律节/官方指引行断言随大区块收敛 onboarding.md 清退，
// 旧条文回归探测并入 assertOneLinerBlock 禁词表） ----------

// TestAgentsBlockOneLiner 渲染函数级：agentsBlock 一句话形态恒定性——输出与
// project/name 参数无关（b9：区块恒定不随项目/版本变化，commOnly 形态参数已删，
// 语义承接原 TestAgentsBlockCommOnlyShape 的两形态断言），标记对+onboarding.md
// 引导路径在位，大区块旧正文零残留（AC1）。
func TestAgentsBlockOneLiner(t *testing.T) {
	block := agentsBlock("proj-a", "示例项目")
	assertOneLinerBlock(t, block)
	if again := agentsBlock("proj-b", "另一项目"); again != block {
		t.Errorf("区块应与 project/name 参数无关（b9 恒定形态）:\n got=%q\nwant=%q", again, block)
	}
}

// TestInitCommOnlyBlockNoiseZero 接线面：--comm-only 落盘的 AGENTS.md 全文无
// docs/planning/ 断链引用+一句话形态（含旧条文回归探测）——验证 runInit 真实
// 落盘形态，非仅渲染函数单测；CLAUDE.md 双写同款一句话区块（b9 W2④）。
func TestInitCommOnlyBlockNoiseZero(t *testing.T) {
	dir := t.TempDir()
	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目", "--comm-only")
	agents := readProjectFile(t, dir, "AGENTS.md")
	if strings.Contains(agents, "docs/planning/") {
		t.Errorf("comm-only AGENTS.md 引用 docs/planning/（断链噪音未清）:\n%s", agents)
	}
	assertOneLinerBlock(t, agents)
	assertOneLinerBlock(t, readProjectFile(t, dir, "CLAUDE.md"))
}

// TestInitFullFormBlockNoiseZero 完整形态接线面：全装 AGENTS.md/CLAUDE.md 同款
// 一句话区块（b9：planning 指向清退出区块，方法论入口归 onboarding.md 与
// docs/planning/methodology.md，指向正确性由 TestInitProductTreeEnglish 树断言覆盖）。
func TestInitFullFormBlockNoiseZero(t *testing.T) {
	dir := t.TempDir()
	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")
	assertOneLinerBlock(t, readProjectFile(t, dir, "AGENTS.md"))
	assertOneLinerBlock(t, readProjectFile(t, dir, "CLAUDE.md"))
}

// TestInitCLAUDELegacyPointerMigration b6→b9 升级迁移面：b6 旧版 CLAUDE.md（无标记
// 指针行，writeIfAbsent 产物）重跑 b9 init——指针行精确移除+一句话区块落位（态②
// 迁移）；指针行与用户内容并存时仅指针行消失、用户内容逐字保留（态②乙）；重跑
// 幂等（态③零变更）。断言用字面量防实现侧常量漂移自证（同 topoBegin 惯例）。
func TestInitCLAUDELegacyPointerMigration(t *testing.T) {
	const legacyLine = "本文件为指针：项目指令见 [AGENTS.md](AGENTS.md)（aiteam 多会话流水线管理）。"

	// 态②甲：文件恰为旧指针行→整文件迁移为区块（无指针残留、无区块前空行叠积）
	d1 := t.TempDir()
	if err := os.WriteFile(filepath.Join(d1, "CLAUDE.md"), []byte(legacyLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runInitForTest(t, d1, "--project", "proj-a", "--name", "示例项目")
	got := readProjectFile(t, d1, "CLAUDE.md")
	if strings.Contains(got, "本文件为指针") {
		t.Errorf("旧指针行未移除（init 自产内容应自清理）:\n%s", got)
	}
	if strings.HasPrefix(got, "\n") {
		t.Errorf("迁移后文件不应以空行开头:\n%s", got)
	}
	assertOneLinerBlock(t, got)

	// 态②乙：指针行与用户内容并存→仅指针行消失，用户内容逐字保留
	d2 := t.TempDir()
	userHead := "# 项目说明\n\n"
	userTail := "自定义内容 keep-me\n"
	if err := os.WriteFile(filepath.Join(d2, "CLAUDE.md"),
		[]byte(userHead+legacyLine+"\n"+userTail), 0o644); err != nil {
		t.Fatal(err)
	}
	runInitForTest(t, d2, "--project", "proj-a", "--name", "示例项目")
	got2 := readProjectFile(t, d2, "CLAUDE.md")
	if strings.Contains(got2, "本文件为指针") {
		t.Errorf("并存场景旧指针行未移除:\n%s", got2)
	}
	if !strings.Contains(got2, userHead) || !strings.Contains(got2, "keep-me") {
		t.Errorf("用户自有内容被触碰（应逐字保留）:\n%s", got2)
	}
	assertOneLinerBlock(t, got2)

	// 态②丙：空行开头的用户文件不含指针行（strip 零命中）→用户内容含前导空白零触碰
	d3 := t.TempDir()
	userLead := "\n\n# 用户文档\n"
	if err := os.WriteFile(filepath.Join(d3, "CLAUDE.md"), []byte(userLead), 0o644); err != nil {
		t.Fatal(err)
	}
	runInitForTest(t, d3, "--project", "proj-a", "--name", "示例项目")
	got3 := readProjectFile(t, d3, "CLAUDE.md")
	if !strings.HasPrefix(got3, userLead) {
		t.Errorf("零命中场景用户前导空白被触碰（应零触碰）:\n%q", got3)
	}
	assertOneLinerBlock(t, got3)

	// 态③：重跑幂等——CLAUDE.md 等全树零变更
	before := snapshotTree(t, d1)
	runInitForTest(t, d1, "--project", "proj-a", "--name", "示例项目")
	after := snapshotTree(t, d1)
	if len(before) != len(after) {
		t.Fatalf("重跑文件数变化: before=%d after=%d", len(before), len(after))
	}
	for rel, sum := range before {
		if after[rel] != sum {
			t.Errorf("重跑变更了 %s（态③幂等）", rel)
		}
	}
}

// ---------- b9 init 渲染新测试（AC2/AC4/AC5/AC8，TDD 红驱动 T4 渲染层改造） ----------

// b9OnboardingRel b9 终态两形态同款产物落点：.aiteam/onboarding.md（通讯域全集
// 单文件，--comm-only 与 full 字节级同源渲染；旧 .aiteam/启动指令.md 清退）。
const b9OnboardingRel = ".aiteam/onboarding.md"

// cjkFileNameRe 产物树文件名中文字符探测（b9 终态：产物树零中文文件名）。
var cjkFileNameRe = regexp.MustCompile(`[\x{4e00}-\x{9fff}]`)

// TestInitOnboardingPerFormGuidance 两形态引导分叉（用户 2026-10-07 拍板，改判 b9
// AC2 字节同款）：--comm-only 与 full 各跑一个临时目录，.aiteam/onboarding.md 均非
// 空、含 aiteam 标记对、占位真值落位（PROJECT_CODE 渲染为 --project 值、
// PROJECT_NAME 渲染为 --name 值）、零 {{ 残留；**full 形态开工第一读多一条方法论
// 引导（project-status.md 红绿灯+methodology.md），comm-only 无 planning 引导
// （无 docs/planning/ 面，引用悬空即缺陷）**。
func TestInitOnboardingPerFormGuidance(t *testing.T) {
	dirComm := t.TempDir()
	runInitForTest(t, dirComm, "--comm-only", "--project", "proj-a", "--name", "示例项目")
	dirFull := t.TempDir()
	runInitForTest(t, dirFull, "--project", "proj-a", "--name", "示例项目")

	comm := readProjectFile(t, dirComm, b9OnboardingRel)
	full := readProjectFile(t, dirFull, b9OnboardingRel)

	if strings.TrimSpace(comm) == "" {
		t.Errorf("%s 为空（comm-only 形态未播种）", b9OnboardingRel)
	}
	if strings.TrimSpace(full) == "" {
		t.Errorf("%s 为空（full 形态未播种）", b9OnboardingRel)
	}
	// full 引导行在位：开工第一读补 methodology 第二站（project-status+methodology）
	for _, want := range []string{
		"docs/planning/project-status.md",
		"docs/planning/methodology.md",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("full 形态 %s 开工第一读应含 %q 引导（完整形态方法论第二站）", b9OnboardingRel, want)
		}
	}
	// comm-only 零 planning 引导行：无 docs/planning/ 面（模板 HTML 注释里的
	// 「另有方法论补全」字样为预存瑕疵，非引导行，不在断言面）
	if strings.Contains(comm, "开工第一读=docs/planning/project-status.md") {
		t.Errorf("comm-only 形态 %s 不应含 methodology 引导行（未播种 planning 面，引导悬空）", b9OnboardingRel)
	}

	// 标记对在位（模板源自带 aiteam 区块标记，字面量断言防实现自证，同 topoBegin 惯例）
	begin := "<!-- aiteam:begin (do not edit between these markers) -->"
	end := "<!-- aiteam:end -->"
	if !strings.Contains(comm, begin) || !strings.Contains(comm, end) {
		t.Errorf("%s 缺 aiteam 标记对（begin/end）:\n%s", b9OnboardingRel, comm)
	}

	// 占位真值落位：PROJECT_CODE=--project 值、PROJECT_NAME=--name 值
	if !strings.Contains(comm, "proj-a") {
		t.Errorf("%s 缺 PROJECT_CODE 真值 proj-a（--project 未渲染落位）:\n%s", b9OnboardingRel, comm)
	}
	if !strings.Contains(comm, "示例项目") {
		t.Errorf("%s 缺 PROJECT_NAME 真值示例项目（--name 未渲染落位）:\n%s", b9OnboardingRel, comm)
	}
	if strings.Contains(comm, "{{") {
		t.Errorf("%s 残留占位符 {{（渲染兜底未收敛）:\n%s", b9OnboardingRel, comm)
	}
}

// TestInitProductTreeEnglish AC4/AC5 产物树英文化+两形态产物面：
// full 形态——planning 渲染件全集逐件在位且非空（见 wantPlanningFiles；
// methodology.md 为 full 专属）、
// 通讯五件+dev-guide 在位、技能包在位非空、产物树零中文文件名；
// comm-only 形态——产物恰四件（onboarding/cli.json/AGENTS.md/CLAUDE.md），
// docs 与 .agents 零产生（walk 产物全集恰四件）。
func TestInitProductTreeEnglish(t *testing.T) {
	// full 形态：planning 渲染件全集+通讯面+dev-guide+技能包
	dirFull := t.TempDir()
	runInitForTest(t, dirFull, "--project", "proj-a", "--name", "示例项目")

	for _, rel := range wantPlanningFiles {
		if got := readProjectFile(t, dirFull, rel); strings.TrimSpace(got) == "" {
			t.Errorf("产物 %s 为空", rel)
		}
	}
	for _, rel := range []string{
		b9OnboardingRel,
		".aiteam/cli.json",
		"AGENTS.md",
		"CLAUDE.md",
		"docs/dev-guide/base.md",
	} {
		if got := readProjectFile(t, dirFull, rel); strings.TrimSpace(got) == "" {
			t.Errorf("产物 %s 为空", rel)
		}
	}

	// 技能包在位且非空（walk 计文件数）
	skillCount := 0
	err := filepath.WalkDir(filepath.Join(dirFull, ".agents", "skills"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			skillCount++
		}
		return nil
	})
	if err != nil || skillCount == 0 {
		t.Errorf(".agents/skills 应在位且非空（count=%d err=%v）", skillCount, err)
	}

	// 产物树零中文文件名（b9 终态英文化硬判据）：walk 全树收文件名逐个过正则。
	// README.md/AGENTS.md/CLAUDE.md/PRD.md 文件名本身英文天然不命中；探测目标是
	// 旧中文产物名（如 启动指令.md）残留。
	var cjkHits []string
	err = filepath.WalkDir(dirFull, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && cjkFileNameRe.MatchString(d.Name()) {
			rel, relErr := filepath.Rel(dirFull, path)
			if relErr != nil {
				return relErr
			}
			cjkHits = append(cjkHits, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 full 产物树失败: %v", err)
	}
	if len(cjkHits) > 0 {
		t.Errorf("产物树存在中文文件名（b9 终态零中文文件名）: %v", cjkHits)
	}

	// comm-only 形态：产物恰四件
	dirComm := t.TempDir()
	runInitForTest(t, dirComm, "--project", "proj-a", "--name", "示例项目", "--comm-only")

	// docs 与 .agents 零产生（显式 stat 供直观失败信息；恰四件断言在下方 walk 全集）
	for _, rel := range []string{"docs", ".agents"} {
		if _, err := os.Stat(filepath.Join(dirComm, rel)); !os.IsNotExist(err) {
			t.Errorf("--comm-only 不应产生 %s/: %v", rel, err)
		}
	}
	var gotFiles []string
	err = filepath.WalkDir(dirComm, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, relErr := filepath.Rel(dirComm, path)
			if relErr != nil {
				return relErr
			}
			gotFiles = append(gotFiles, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 comm-only 产物树失败: %v", err)
	}
	sort.Strings(gotFiles)
	wantFiles := []string{
		".aiteam/cli.json",
		b9OnboardingRel,
		"AGENTS.md",
		"CLAUDE.md",
	}
	sort.Strings(wantFiles)
	if strings.Join(gotFiles, "|") != strings.Join(wantFiles, "|") {
		t.Errorf("comm-only 产物应恰四件:\n got=%v\nwant=%v", gotFiles, wantFiles)
	}
}

// TestInitUpgradeHintLegacy AC8 升级行为：目录已有旧中文名产物（.aiteam/启动指令.md、
// docs/planning/启动指令.md）时重跑 init——播种新名 .aiteam/onboarding.md、旧文件
// 不删除不覆盖（读回逐字比对）、输出含升级提示行；新名文件已存在时不覆盖
// （PREEXISTING 内容保留）。
func TestInitUpgradeHintLegacy(t *testing.T) {
	// 场景甲：旧产物在位→播种新名+旧文件原样保留+输出提示
	dir := t.TempDir()
	legacyComm := filepath.Join(dir, ".aiteam", "启动指令.md")
	legacyPlan := filepath.Join(dir, "docs", "planning", "启动指令.md")
	if err := os.MkdirAll(filepath.Dir(legacyComm), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(legacyPlan), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyCommContent := "旧版通讯启动指令内容 LEGACY-COMM-CONTENT\n"
	legacyPlanContent := "旧版规划启动指令内容 LEGACY-PLAN-CONTENT\n"
	if err := os.WriteFile(legacyComm, []byte(legacyCommContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPlan, []byte(legacyPlanContent), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")

	// 新名产物已播种（在位非空）
	if got := readProjectFile(t, dir, b9OnboardingRel); strings.TrimSpace(got) == "" {
		t.Errorf("升级重跑未播种新名产物 %s", b9OnboardingRel)
	}

	// 旧文件不删除不覆盖：逐字读回比对（读回失败=被删，内容不符=被改写）
	gotComm, err := os.ReadFile(legacyComm)
	if err != nil {
		t.Fatalf("旧产物 .aiteam/启动指令.md 被删除（AC8 不自动删）: %v", err)
	}
	if string(gotComm) != legacyCommContent {
		t.Errorf("旧产物 .aiteam/启动指令.md 内容被改动（AC8 不覆盖）:\n%s", gotComm)
	}
	gotPlan, err := os.ReadFile(legacyPlan)
	if err != nil {
		t.Fatalf("旧产物 docs/planning/启动指令.md 被删除（AC8 不自动删）: %v", err)
	}
	if string(gotPlan) != legacyPlanContent {
		t.Errorf("旧产物 docs/planning/启动指令.md 内容被改动（AC8 不覆盖）:\n%s", gotPlan)
	}

	// 升级提示行（断言从宽：提及旧产物文件名+提示语义词任一，措辞不钉死——
	// T4 实现时微调措辞不应破测试）
	if !strings.Contains(out, "启动指令.md") {
		t.Errorf("升级输出未提及旧产物文件名 启动指令.md:\n%s", out)
	}
	hintHit := false
	for _, kw := range []string{"提示", "检测到", "旧版", "遗留"} {
		if strings.Contains(out, kw) {
			hintHit = true
			break
		}
	}
	if !hintHit {
		t.Errorf("升级输出缺提示语义词（提示/检测到/旧版/遗留 任一）:\n%s", out)
	}

	// 场景乙：新名文件已存在→不覆盖（PREEXISTING 内容保留）
	dir2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir2, ".aiteam"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, ".aiteam", "onboarding.md"), []byte("PREEXISTING"), 0o644); err != nil {
		t.Fatal(err)
	}
	runInitForTest(t, dir2, "--project", "proj-a", "--name", "示例项目")
	if got := readProjectFile(t, dir2, b9OnboardingRel); !strings.Contains(got, "PREEXISTING") {
		t.Errorf("已有新名文件被覆盖（AC8 不覆盖语义，PREEXISTING 应保留）:\n%s", got)
	}
}

// ---------- b10 init 播种树完备化（TDD 红驱动 T3 播种重排）：_template 整目录播种
// +五新件（maintenance/CONTEXT/s0-s7-stages/iterations 迭代骨架）。播种面同构裁定
// （开工令）：init 播种面与本仓结构同构（狗粮原则）——项目级新件落 docs/planning/
// 根、迭代件落 iterations/ 子目录，一并消解 b9 遗留的「writeIfAbsent 只探 planning
// 根，实况在 iterations/ 下时重跑在根播种空模板污染」问题（AC11）。

// b10TemplateMinFiles b10 终态模板目录文件数下界：b9 后 19 件+T2 新建
// maintenance-template.md=20 件+T3 新建 index-template.md=21 件（T3 播种面落地
// 后按终态断言）。
const b10TemplateMinFiles = 21

// countSeededTemplateFiles 统计目标项目内 docs/planning/_template/ 播种文件数
// （递归计文件）；目录缺失即 Fatal——红态死因=「_template 须整目录播种」。
func countSeededTemplateFiles(t *testing.T, dir string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(filepath.Join(dir, "docs", "planning", "_template"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("_template 须整目录播种（walk docs/planning/_template 失败）: %v", err)
	}
	return count
}

// TestInitSeedsFullTemplateSuite AC1：full init 后 docs/planning/_template/ 整目录
// 播种（≥21 件），模板件原样保留占位符不渲染——模板是供复制的源，init 只搬运不
// 加工；抽验 spec-template.md 与 embed 源的工作树文件字节一致（防搬运时被静默
// 渲染或改写）。comm-only 回归不在此重复断言：comm-only 产物恰四件、docs/planning
// 零产生由 TestInitProductTreeEnglish 的 walk 全集断言覆盖（_template 属
// docs/planning 子树，自动被该断言排除）。
func TestInitSeedsFullTemplateSuite(t *testing.T) {
	// embed 源的工作树原文件先行读取：go test 运行 cwd=包目录（internal/cli），
	// 相对路径 ../../ = 仓根；runInitForTest 会 chdir 进目标目录，源读取必须在其前。
	srcBytes, err := os.ReadFile(filepath.Join("..", "..", "docs", "planning", "_template", "spec-template.md"))
	if err != nil {
		t.Fatalf("读取 embed 源工作树文件 ../../docs/planning/_template/spec-template.md 失败: %v", err)
	}

	dir := t.TempDir()
	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")

	if n := countSeededTemplateFiles(t, dir); n < b10TemplateMinFiles {
		t.Errorf("_template 播种文件数 %d < %d（AC1 整目录播种，b10 终态 21 件）", n, b10TemplateMinFiles)
	}

	// 原样性抽验：与源字节一致+占位符保留（含 {{）——渲染是落到实况件产物时的
	// 事，模板本体必须带 {{ 供用户复制后自行替换。
	seeded := readProjectFile(t, dir, "docs/planning/_template/spec-template.md")
	if seeded != string(srcBytes) {
		t.Errorf("播种的 _template/spec-template.md 与 embed 源工作树文件不一致（须原样播种，占位符不渲染）")
	}
	if !strings.Contains(seeded, "{{") {
		t.Errorf("播种模板应原样保留占位符（含 {{），不得预渲染:\n%s", seeded)
	}
}

// TestInitSeedsNewArtifacts AC2/AC3/AC11：full init 后五类新件骨架锚——
// maintenance.md（七列表头+纪律头部，断言从宽：T2 落 maintenance-template 后自然
// 满足，红态=文件缺失）；CONTEXT.md（渲染既有 context-template：零 {{ 残留+
// {{REPO_PATH}}/{{PROJECT_NAME}} 真值落位——T2 已为模板补 {{PROJECT_NAME}} 占位
// 符）；s0-s7-stages.md（渲染既有模板，{{PROJECT_NAME}} 真值=demo）；项目级三件
// 回归（project-status/executor-contract/methodology 已有产物，防播种重排挤掉）
// +iterations/v0.1 迭代骨架四件渲染+specs/INDEX.md 登记簿（双形态命名锚从宽，
// INDEX 骨架模板 T2/T3 落地）。
// 本测 --project/--name 均取 demo：{{PROJECT_NAME}} 渲染自 --name，真值锚统一字面
// demo（context-template 与 s0-s7-stages 均含 {{PROJECT_NAME}} 占位符）。
// AC11 狗粮对拍：项目级新件落点前缀恰为 docs/planning/ 根、迭代件归 iterations/
// 子目录——与本仓 b10 后实况结构同构。
func TestInitSeedsNewArtifacts(t *testing.T) {
	dir := t.TempDir()
	runInitForTest(t, dir, "--project", "demo", "--name", "demo")

	// 新件②（任务面顺序①）maintenance.md：七列表头锚（编号/处置建议/状态 从宽
	// 三锚）+纪律头部关键词（S7 或 缓行池 任一）。
	maint := readProjectFile(t, dir, "docs/planning/maintenance.md")
	for _, anchor := range []string{"编号", "处置建议", "状态"} {
		if !strings.Contains(maint, anchor) {
			t.Errorf("maintenance.md 缺七列表头锚 %q:\n%s", anchor, maint)
		}
	}
	if !strings.Contains(maint, "S7") && !strings.Contains(maint, "缓行池") {
		t.Errorf("maintenance.md 缺纪律头部关键词（S7/缓行池 任一）:\n%s", maint)
	}

	// 新件③ CONTEXT.md：context-template 是模板源，渲染产物零 {{ 残留（TERM 系列
	// 走渲染兜底 TODO 文案）；{{REPO_PATH}} 真值=目标目录 slash 形态、
	// {{PROJECT_NAME}} 真值=demo 落位。
	ctxDoc := readProjectFile(t, dir, "docs/planning/CONTEXT.md")
	if strings.TrimSpace(ctxDoc) == "" {
		t.Errorf("docs/planning/CONTEXT.md 为空")
	}
	if strings.Contains(ctxDoc, "{{") {
		t.Errorf("CONTEXT.md 残留占位符 {{（渲染兜底未收敛）:\n%s", ctxDoc)
	}
	if !strings.Contains(ctxDoc, filepath.ToSlash(dir)) {
		t.Errorf("CONTEXT.md 缺 {{REPO_PATH}} 渲染真值（目标目录路径）:\n%s", ctxDoc)
	}
	if !strings.Contains(ctxDoc, "demo") {
		t.Errorf("CONTEXT.md 缺 {{PROJECT_NAME}} 渲染真值 demo:\n%s", ctxDoc)
	}

	// 新件④ s0-s7-stages.md：渲染既有模板，{{PROJECT_NAME}} 真值 demo 落位。
	stages := readProjectFile(t, dir, "docs/planning/s0-s7-stages.md")
	if strings.TrimSpace(stages) == "" {
		t.Errorf("docs/planning/s0-s7-stages.md 为空")
	}
	if !strings.Contains(stages, "demo") {
		t.Errorf("s0-s7-stages.md 缺 {{PROJECT_NAME}} 渲染真值 demo:\n%s", stages)
	}

	// 项目级三件回归（b9 已有产物，播种重排不得挤掉）。
	for _, f := range []string{"project-status.md", "executor-contract.md", "methodology.md"} {
		rel := "docs/planning/" + f
		if got := readProjectFile(t, dir, rel); strings.TrimSpace(got) == "" {
			t.Errorf("产物 %s 为空", rel)
		}
	}

	// 新件⑤ 迭代骨架：iterations/v0.1/ 四件渲染（同 planningArtifacts 迭代件落位）+specs/INDEX.md
	// 空登记簿（双形态命名锚从宽：bN-spec 或 spec.md 与 INDEX 并存）。
	for _, f := range []string{"charter.md", "PRD.md", "tech-design.md", "development-task.md"} {
		rel := "docs/planning/iterations/v0.1/" + f
		if got := readProjectFile(t, dir, rel); strings.TrimSpace(got) == "" {
			t.Errorf("产物 %s 为空", rel)
		}
	}
	index := readProjectFile(t, dir, "docs/planning/iterations/v0.1/specs/INDEX.md")
	if strings.TrimSpace(index) == "" {
		t.Errorf("docs/planning/iterations/v0.1/specs/INDEX.md 为空")
	}
	namingHit := strings.Contains(index, "bN-spec") || strings.Contains(index, "spec.md")
	if !namingHit || !strings.Contains(index, "INDEX") {
		t.Errorf("specs/INDEX.md 缺双形态命名锚（bN-spec/spec.md 与 INDEX）:\n%s", index)
	}

	// AC11 狗粮对拍硬断言：三件项目级新件不得落入 iterations/（上文 readProjectFile
	// 全字面根路径读取成功即前缀恰为 docs/planning/ 根的证明，此处补反证防根+迭代
	// 目录双份播种）。
	for _, rel := range []string{
		"docs/planning/iterations/maintenance.md",
		"docs/planning/iterations/CONTEXT.md",
		"docs/planning/iterations/s0-s7-stages.md",
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("项目级新件不得落入 iterations/（AC11 同构对拍）: %s", rel)
		}
	}
}

// TestInitUpgradeSeedsIncremental AC8：旧版项目（有七件套无 b10 新件）升级重跑——
// 新件全部增量播种回来，已有文件零覆盖。造法：full init 后删除 _template 整目录+
// maintenance/CONTEXT/s0-s7-stages 三件+iterations 整目录树，模拟 v0.1 旧版产物面；
// 重跑前改写 AGENTS.md 为 USERMARK（模拟用户自有内容），重跑后仍含 USERMARK
// （无标记=态②末尾追加，用户内容逐字保留）。另以 snapshotTree 对拍：被删新件与
// 有意改写的 AGENTS.md 之外全树 hash 零漂移（AC8 增量语义硬判据）。
// 旧名提示行为回归不在此重复：.aiteam/启动指令.md 预置+输出提示行由既有
// TestInitUpgradeHintLegacy（b9 段）覆盖。
func TestInitUpgradeSeedsIncremental(t *testing.T) {
	dir := t.TempDir()
	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")
	before := snapshotTree(t, dir)

	// 删除 b10 新件模拟旧版项目：os.RemoveAll 对不存在路径返回 nil——红态下首跑
	// 本就没播种这些件，删除即空操作，重跑断言照常红。
	for _, rel := range []string{
		"docs/planning/_template",
		"docs/planning/maintenance.md",
		"docs/planning/CONTEXT.md",
		"docs/planning/s0-s7-stages.md",
		"docs/planning/iterations",
	} {
		if err := os.RemoveAll(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("删除 %s 失败: %v", rel, err)
		}
	}
	// 用户自有内容标记（重跑后必须保留——不覆盖已有的硬证据）。
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("USERMARK\n"), 0o644); err != nil {
		t.Fatalf("改写 AGENTS.md 失败: %v", err)
	}

	runInitForTest(t, dir, "--project", "proj-a", "--name", "示例项目")

	// 新件全部播种回来：_template ≥21 件+maintenance/CONTEXT/s0-s7-stages 三件+
	// iterations/v0.1/ 五件（四件渲染+INDEX）。
	if n := countSeededTemplateFiles(t, dir); n < b10TemplateMinFiles {
		t.Errorf("升级重跑未播种 _template（文件数 %d < %d）", n, b10TemplateMinFiles)
	}
	for _, rel := range []string{
		"docs/planning/maintenance.md",
		"docs/planning/CONTEXT.md",
		"docs/planning/s0-s7-stages.md",
		"docs/planning/iterations/v0.1/charter.md",
		"docs/planning/iterations/v0.1/PRD.md",
		"docs/planning/iterations/v0.1/tech-design.md",
		"docs/planning/iterations/v0.1/development-task.md",
		"docs/planning/iterations/v0.1/specs/INDEX.md",
	} {
		if got := readProjectFile(t, dir, rel); strings.TrimSpace(got) == "" {
			t.Errorf("升级重跑未播种 %s", rel)
		}
	}

	// 不覆盖已有：USERMARK 保留+既有文件 hash 零漂移。
	if agents := readProjectFile(t, dir, "AGENTS.md"); !strings.Contains(agents, "USERMARK") {
		t.Errorf("升级重跑覆盖了用户 AGENTS.md 内容（USERMARK 应保留）:\n%s", agents)
	}
	// b9 旧版项目已在 planning 根的三件（本升级模拟中在位未删，不属被删新件面，
	// 零漂移对拍必须覆盖它们）。
	b9LegacyPlanningFiles := map[string]bool{
		"project-status.md":    true,
		"executor-contract.md": true,
		"methodology.md":       true,
	}
	// removedRel 被删新件判定（目录树前缀+根级 b10 新件）：这些 rel 重跑后重新播种，
	// 不参与零漂移对拍（红态下首跑本就没有这些件，判定天然空转）。根级判定从
	// planningArtifacts 派生（dst 无子目录且非 b9 旧三件）——实现侧增删根级新件时
	// 白名单自动跟随，防清单滞后（质量审查建议②）；_template/iterations 两前缀为
	// 结构性目录（整目录被删整目录重生），保留字面。
	removedRel := func(rel string) bool {
		s := filepath.ToSlash(rel)
		switch {
		case s == "docs/planning/_template" || strings.HasPrefix(s, "docs/planning/_template/"):
			return true
		case s == "docs/planning/iterations" || strings.HasPrefix(s, "docs/planning/iterations/"):
			return true
		}
		if !strings.HasPrefix(s, "docs/planning/") {
			return false
		}
		base := strings.TrimPrefix(s, "docs/planning/")
		if strings.Contains(base, "/") || b9LegacyPlanningFiles[base] {
			return false
		}
		for _, art := range planningArtifacts {
			if art.dst == base {
				return true
			}
		}
		return false
	}
	after := snapshotTree(t, dir)
	for rel, sum := range before {
		if filepath.ToSlash(rel) == "AGENTS.md" {
			continue // 有意改写：不覆盖语义由上方 USERMARK 断言承载
		}
		if removedRel(rel) {
			continue
		}
		if h, ok := after[rel]; !ok || h != sum {
			t.Errorf("升级重跑变更/丢失了既有文件 %s（AC8 增量语义：已有文件不覆盖）", rel)
		}
	}
	// after 侧扫尾（b10 质量审查采纳项）：重跑产生的预期外新文件也要报——新增 rel
	// 应恰为被删新件的重新播种（removedRel 命中），集合外新文件=播种面失控。
	for rel := range after {
		if _, ok := before[rel]; ok {
			continue
		}
		if removedRel(rel) {
			continue
		}
		t.Errorf("升级重跑产生了预期外新文件 %s（新增面应恰为被删新件的重新播种）", rel)
	}
}
