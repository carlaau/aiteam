package cli

import (
	"io/fs"
	"strings"
	"testing"

	"aiteam/internal/assets"
)

// TestSkillsPathAlignment b10-T4 AC4：技能包源（embed 发布面 assets.SkillsFS）
// 零上游遗留路径/前缀残留。上游 superpowers-zh 的遗留落盘路径（docs/superpowers/...）
// 与技能名前缀（superpowers:xxx）已全量归一到 aiteam 的 docs/planning/specs 体系
// （specs/<迭代>/ 目录体系+INDEX 登记，与 spec-template W4 口径一致）与裸技能名引用。
// 遍历 embed 技能包全部文件兜底（不只四技能）；四主断言对象=brainstorming /
// writing-plans / subagent-driven-development / requirement-grilling（spec 锚行逐处替换）。
// 「> 来源：」出处署名行豁免前缀断言（13 条署名行全此前缀）——出处声明非路径引用，
// 保留不动；非署名行的前缀残留不因行内恰含出处关键词而漏报。
func TestSkillsPathAlignment(t *testing.T) {
	skillN := 0
	err := fs.WalkDir(assets.SkillsFS, "skills", func(path string, d fs.DirEntry, err error) error {
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
		content := string(data)
		if strings.Contains(content, "docs/superpowers/") {
			t.Errorf("%s: 上游遗留路径残留 docs/superpowers/（应归一 specs/<迭代>/ 目录体系）", path)
		}
		for i, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "> 来源：") {
				continue // 出处署名行豁免（署名非路径引用）
			}
			if strings.Contains(line, "superpowers:") {
				t.Errorf("%s:%d: 上游技能名前缀残留 superpowers:（应裸技能名引用）: %s",
					path, i+1, strings.TrimSpace(line))
			}
		}
		if path == "skills/brainstorming/SKILL.md" && !strings.Contains(content, "specs/<迭代>/") {
			// 正锚：brainstorming 产物路径须落在 specs/<迭代>/ 体系——防未来空替换/漂移
			t.Errorf("skills/brainstorming/SKILL.md: 未见 specs/<迭代>/ 正锚（产物路径应落 specs/<迭代>/ 目录体系）")
		}
		if strings.HasSuffix(path, "SKILL.md") {
			skillN++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 embed 技能包失败: %v", err)
	}
	// 13 个技能 SKILL.md（+LICENSES.md 同在闸内）全在位——embed 树被误排空/误排除时此处先红
	if skillN != 13 {
		t.Errorf("embed 技能包 SKILL.md 数 = %d，期望 13（13 个技能目录全在位，外加 LICENSES.md）", skillN)
	}
}
