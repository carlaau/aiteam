package cli

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"aiteam/internal/assets"
)

// TestSkillsTreesSync 技能包双落盘副本同步闸：工作副本 .agents/skills/（狗粮/加载面）
// ↔ embed 发布副本 internal/assets/skills/（随二进制发布）两树必须逐文件字节相等。
// 由来：2026-10-06 上午改 .agents/skills/requirement-grilling/SKILL.md 漏同步 embed 侧，
// 既有 TestSkillsPathAlignment 只扫 embed 侧上游遗留路径残留、不比对两树内容，闸没红，
// 靠人眼才发现——本测试补上该机械闸：key 集合一致 + 逐 key 字节相等（报错附首差异
// 字节 offset 定位，纯行尾差异类红自解释）+ 空对空假绿防线。
// 技能包改动须两侧同步（.agents/skills 工作副本 ↔ internal/assets/skills embed 副本），
// 否则此处先红。
func TestSkillsTreesSync(t *testing.T) {
	// 工作副本（文件系统侧）：go test 的 cwd=internal/cli，相对路径回仓根 .agents/skills
	workRoot := filepath.Join("..", "..", ".agents", "skills")
	work := map[string][]byte{}
	werr := filepath.WalkDir(workRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(workRoot, path)
		if rerr != nil {
			return rerr
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// Windows 下 WalkDir 产出反斜杠路径，统一转 slash 形态对齐 key 空间
		work[filepath.ToSlash(rel)] = data
		return nil
	})
	if werr != nil {
		t.Fatalf("遍历工作副本 %s 失败（路径写错/目录缺失时此处先红）: %v", workRoot, werr)
	}

	// embed 发布副本：fs.WalkDir 恒用 slash 形态，剥 skills/ 前缀对齐同一 key 空间
	embed := map[string][]byte{}
	eerr := fs.WalkDir(assets.SkillsFS, "skills", func(path string, d fs.DirEntry, err error) error {
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
		embed[strings.TrimPrefix(path, "skills/")] = data
		return nil
	})
	if eerr != nil {
		t.Fatalf("遍历 embed 技能包失败: %v", eerr)
	}

	// 空对空假绿防线：任一侧为空（路径写错/树被误排空/embed 指令失效）直接 Fatal，
	// 不给「两边都空所以相等」留假绿空间
	if len(work) == 0 {
		t.Fatalf("工作副本 %s 遍历结果为空：路径写错或技能包被误排空，拒绝空对空假绿", workRoot)
	}
	if len(embed) == 0 {
		t.Fatalf("embed 技能包遍历结果为空：树被误排空或 go:embed 指令失效，拒绝空对空假绿")
	}

	// 断言一：key 集合完全一致——多出/缺失均报并列具体差集
	var onlyInEmbed, onlyInWork []string
	for k := range embed {
		if _, ok := work[k]; !ok {
			onlyInEmbed = append(onlyInEmbed, k)
		}
	}
	for k := range work {
		if _, ok := embed[k]; !ok {
			onlyInWork = append(onlyInWork, k)
		}
	}
	sort.Strings(onlyInEmbed)
	sort.Strings(onlyInWork)
	if len(onlyInEmbed) > 0 || len(onlyInWork) > 0 {
		t.Fatalf("两树文件集合不一致（技能包改动须两侧同步：.agents/skills 工作副本 ↔ internal/assets/skills embed 副本）\n  仅 embed 侧有（工作副本缺失）:\n    %s\n  仅工作副本有（embed 侧缺失）:\n    %s",
			strings.Join(onlyInEmbed, "\n    "), strings.Join(onlyInWork, "\n    "))
	}

	// 断言二：逐 key 字节相等（Errorf 逐条列全，不首败即停）；报错附首差异字节定位——
	// 最易混淆的红恰是纯行尾差异（diff 工具看不出内容差别），offset+双侧字节值让这类红自解释
	keys := make([]string, 0, len(work))
	for k := range work {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !bytes.Equal(work[k], embed[k]) {
			t.Errorf("%s: 两树内容不一致（%s；技能包改动须两侧同步：.agents/skills 工作副本 ↔ internal/assets/skills embed 副本）",
				k, firstDiffDetail(work[k], embed[k]))
		}
	}
}

// firstDiffDetail 生成内容不一致的定位片段：「长度 工作副本=N embed=M；首差异 @offset K:
// 工作副本=0x.. embed=0x..」。首差异=首个不相等字节下标；一方为另一方前缀（纯长度差，
// 如单侧追加）时 offset=较短方长度，短侧无字节以 <EOF> 占位。前提：bytes.Equal(a, b) 为假。
func firstDiffDetail(a, b []byte) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	off := n
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			off = i
			break
		}
	}
	hexAt := func(data []byte) string {
		if off >= len(data) {
			return "<EOF>"
		}
		return fmt.Sprintf("0x%02X", data[off])
	}
	return fmt.Sprintf("长度 工作副本=%d embed=%d；首差异 @offset %d: 工作副本=%s embed=%s",
		len(a), len(b), off, hexAt(a), hexAt(b))
}
