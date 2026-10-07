package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// b8-W4（AC9·文本基线对拍锚）：哨兵纪律第 2 条冻结改写（消费端适配前置口径）逐字
// 落五处——docs/planning/_template/onboarding.md（b9 起 init.go agentsBlock 纪律
// 节的一句话化模板源，测试从模板源取值）/ docs/planning/_template/pipeline-overview.md
// / docs/planning/_template/executor-contract.md / README.md / docs/接入指南.md。
// 哨兵纪律五处历史上各自改写漂移（b6 收敛过一次），spec §四判「文本基线五处漂移
// 复发」需 check 层拦截——本测试落 go test ./...（check.sh 第 5 节），漂移即红。
//
// 对拍粒度两级：
//  1. 核心锚句（「消费端适配前置/挂法前提=命中有会反应的消费端/哑炮」）——五处
//     原文必含，即 AC9「grep『消费端』口径命中」的测试化等价；
//  2. 冻结全文——归一化后五处全中。归一化=剥空白与 '#'/'*'：五处载体格式各异
//     （模板 md 与 README 的加粗与列表符 / 接入指南 bash 注释块的 "# " 前缀断行），
//     格式差异可容忍，文字内容必须逐字一致。

// sentinelDisciplineFrozen 冻结文案（spec b8 §1.1 W4 第 2 条，剥加粗后的纯文字内容）。
const sentinelDisciplineFrozen = "哨兵常挂（消费端适配前置）：" +
	"干活中也挂——值守与工作并行，不因开工而摘哨；" +
	"挂法前提=命中有会反应的消费端（宿主后台任务退出通知/人在终端/--on-hit 钩子），" +
	"无消费端的分离式挂法（无人读的日志/nohup）=哑炮，禁用"

// sentinelDisciplineAnchors 核心锚句：五处原文（未归一化）必含。锚句不得被载体
// 断行拆断——接入指南 bash 注释块内改写时保持锚句单行完整。
var sentinelDisciplineAnchors = []string{
	"消费端适配前置",
	"挂法前提=命中有会反应的消费端",
	"哑炮",
}

// normalizeDisciplineText 剥空白与 '#'/'*'，得纯文字内容流，供跨载体逐字对拍。
func normalizeDisciplineText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) || r == '#' || r == '*' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// readDisciplineLanding 读一个文档落点原文（测试 cwd=包目录，仓根文件走 ../..）。
func readDisciplineLanding(t *testing.T, rel ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, rel...)...))
	if err != nil {
		t.Fatalf("读落点文件失败 %v: %v", rel, err)
	}
	return string(data)
}

func TestSentinelDisciplineBaselineFiveLandings(t *testing.T) {
	// 落点 1：onboarding.md 模板源（b9 起 init.go agentsBlock 纪律节的一句话化来源，
	// 从模板源取值即等价覆盖 embed 面真值）。
	type landing struct {
		name string
		text string
	}
	landings := []landing{
		{"docs/planning/_template/onboarding.md", readDisciplineLanding(t, "docs", "planning", "_template", "onboarding.md")},
		// 落点 2~5：四份文档原文。
		{"docs/planning/_template/pipeline-overview.md", readDisciplineLanding(t, "docs", "planning", "_template", "pipeline-overview.md")},
		{"docs/planning/_template/executor-contract.md", readDisciplineLanding(t, "docs", "planning", "_template", "executor-contract.md")},
		{"README.md", readDisciplineLanding(t, "README.md")},
		{"docs/接入指南.md", readDisciplineLanding(t, "docs", "接入指南.md")},
	}

	frozen := normalizeDisciplineText(sentinelDisciplineFrozen)
	for _, l := range landings {
		for _, anchor := range sentinelDisciplineAnchors {
			if !strings.Contains(l.text, anchor) {
				t.Errorf("落点 %s 缺核心锚句 %q（AC9 grep『消费端』口径破防）", l.name, anchor)
			}
		}
		if got := normalizeDisciplineText(l.text); !strings.Contains(got, frozen) {
			t.Errorf("落点 %s 缺冻结第 2 条正文（归一化逐字对拍失配）\n期望含: %s", l.name, sentinelDisciplineFrozen)
		}
	}
}

// TestW5UserFacingDocs b8-W5（AC11·用户可见面文档断言）：README 与接入指南的用户
// 可见功能文档——--on-hit/--force 用法、per-OS 通知配方（四平台至少其二）、排障
// 小节存在性；README send 行零触碰回归锚（W5 明令 send/status 行不受本批影响）。
func TestW5UserFacingDocs(t *testing.T) {
	readme := readDisciplineLanding(t, "README.md")
	guide := readDisciplineLanding(t, "docs", "接入指南.md")

	// README：两 flag 用法在（W5①）；send 演练行零触碰（回归锚——W5① 只许动 watch 行注释面）
	for _, kw := range []string{"--on-hit", "--force"} {
		if !strings.Contains(readme, kw) {
			t.Errorf("README 缺 %s 用法说明（AC11）", kw)
		}
	}
	const sendLine = `--to-role executor --level important --body "接入示例：请 poll 确认并回复"`
	if !strings.Contains(readme, sendLine) {
		t.Errorf("README send 演练行锚失守（W5① send 行零触碰被破坏）: %s", sendLine)
	}

	// 接入指南：两 flag+排障小节存在性（标题+三关键词）
	for _, kw := range []string{"--on-hit", "--force", "哨兵值守健康与排障", "孤儿", "接管", "消费端"} {
		if !strings.Contains(guide, kw) {
			t.Errorf("接入指南缺 %q（AC11）", kw)
		}
	}

	// 通知配方关键词四平台至少其二（AC11 判据原文）
	recipeKeywords := []string{"toast", "osascript", "notify-send", "ntfy"}
	hits := 0
	for _, kw := range recipeKeywords {
		if strings.Contains(guide, kw) {
			hits++
		}
	}
	if hits < 2 {
		t.Errorf("接入指南通知配方关键词仅命中 %d/4（AC11 要求至少其二）", hits)
	}

	// W5④ 词汇统一：排障小节与 W4 冻结口径同套词汇（同一套叫法，不出现两种表述）
	section := guide[strings.Index(guide, "哨兵值守健康与排障"):]
	for _, kw := range []string{"消费端适配前置", "哑炮", "命中留痕"} {
		if !strings.Contains(section, kw) {
			t.Errorf("排障小节缺统一词汇 %q（W5④ 同一套词汇要求）", kw)
		}
	}
}
