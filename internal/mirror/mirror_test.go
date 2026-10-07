package mirror

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	projCode   = "proj-a" // 测试用项目 code
	columnCode = "05"     // 测试用栏目 code
)

// sampleEntry 构造与技术设计 §8.2 示例第一条同构的载荷，供各用例复用后按需覆盖字段。
func sampleEntry() Entry {
	return Entry{
		Seq:         881,
		CreatedAt:   "2026-10-02T13:04:05Z",
		Level:       "block",
		Target:      "proj-a/05/executor",
		SenderLabel: "controller-A@05",
		Body:        "端口 8080 已占用，请改用 8081",
	}
}

// mirrorPath 按冻结契约拼镜像文件路径（§8.1：<root>/.aiteam/mirror-<项目code>-<栏目code>.md）。
func mirrorPath(root string) string {
	return filepath.Join(root, ".aiteam", "mirror-"+projCode+"-"+columnCode+".md")
}

// readLines 读镜像文件并按行拆分（去掉末尾换行产生的空尾行），返回全部行。
func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取镜像文件 %s 失败: %v", path, err)
	}
	s := strings.TrimRight(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// TestAppendLine 行格式六字段逐项断言（§8.2）：分隔符 " | "；body 多行→字面 \n、
// 制表符→字面 \t；超长 body 不截断（审计完整性）。
func TestAppendLine(t *testing.T) {
	root := t.TempDir()
	e := sampleEntry()
	e.Body = "端口 8080 已占用，请改用 8081\n第二行\t缩进内容"
	if err := Append(root, projCode, columnCode, e); err != nil {
		t.Fatalf("Append 返回错误: %v", err)
	}
	lines := readLines(t, mirrorPath(root))
	if len(lines) != 2 {
		t.Fatalf("镜像应有头注释+1 条消息共 2 行，实际 %d 行: %q", len(lines), lines)
	}
	// 六字段逐项断言（测试 body 不含 " | "，可安全按分隔符拆分）。
	parts := strings.Split(lines[1], " | ")
	if len(parts) != 6 {
		t.Fatalf("消息行应拆出 6 个字段，实际 %d 个: %q", len(parts), lines[1])
	}
	wantParts := []string{
		"881",
		"2026-10-02T13:04:05Z",
		"block",
		"proj-a/05/executor",
		"controller-A@05",
		`端口 8080 已占用，请改用 8081\n第二行\t缩进内容`,
	}
	for i, want := range wantParts {
		if parts[i] != want {
			t.Errorf("字段 %d = %q，期望 %q", i, parts[i], want)
		}
	}
	// 不截断：超长 body（5000 字）完整落盘。
	long := strings.Repeat("长", 5000)
	e2 := sampleEntry()
	e2.Seq = 882
	e2.Body = long
	if err := Append(root, projCode, columnCode, e2); err != nil {
		t.Fatalf("Append(超长 body) 返回错误: %v", err)
	}
	lines = readLines(t, mirrorPath(root))
	last := lines[len(lines)-1]
	if !strings.Contains(last, long) {
		t.Errorf("超长 body 应完整保留（不截断），行长 %d，body 长 %d", len(last), len(long))
	}
}

// TestFirstCreate 首次写入：自动创建 .aiteam/ 目录与镜像文件（§8.1），
// 文件名 = mirror-<项目code>-<栏目code>.md，首行为头注释（§8.2）。
func TestFirstCreate(t *testing.T) {
	root := t.TempDir()
	if _, err := os.Stat(filepath.Join(root, ".aiteam")); !os.IsNotExist(err) {
		t.Fatalf("前置：.aiteam 应尚不存在，stat 错误: %v", err)
	}
	if err := Append(root, projCode, columnCode, sampleEntry()); err != nil {
		t.Fatalf("Append 返回错误: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(root, ".aiteam")); err != nil || !fi.IsDir() {
		t.Fatalf(".aiteam 目录应被自动创建，stat: %v, err: %v", fi, err)
	}
	lines := readLines(t, mirrorPath(root))
	if len(lines) != 2 {
		t.Fatalf("首建文件应含头注释+1 条消息共 2 行，实际 %d 行: %q", len(lines), lines)
	}
	if lines[0] != HeaderComment {
		t.Errorf("首行头注释 = %q，期望 %q", lines[0], HeaderComment)
	}
	if want := RenderLine(sampleEntry()); lines[1] != want {
		t.Errorf("第二行 = %q，期望与 RenderLine 全等 %q", lines[1], want)
	}
}

// TestAppendOnly 连发三条=三行累加（AC13.1），头注释只出现一次（仅首建写入）；
// 三条复刻技术设计 §8.2 示例三形态：角色 proj-a/05/executor、总线 BUS、
// 会话 session:executor-B，消息行与 RenderLine 逐行全等断言。
func TestAppendOnly(t *testing.T) {
	root := t.TempDir()
	e1 := sampleEntry() // 角色形态（§8.2 示例 881）
	if err := Append(root, projCode, columnCode, e1); err != nil {
		t.Fatalf("第 1 次 Append 返回错误: %v", err)
	}
	e2 := sampleEntry() // 总线形态（§8.2 示例 882）
	e2.Seq = 882
	e2.CreatedAt = "2026-10-02T13:05:11Z"
	e2.Level = "important"
	e2.Target = "BUS"
	e2.Body = "S4 阶段时间窗已设定 23:00-09:00"
	if err := Append(root, projCode, columnCode, e2); err != nil {
		t.Fatalf("第 2 次 Append 返回错误: %v", err)
	}
	e3 := sampleEntry() // 会话形态（§8.2 示例 883）
	e3.Seq = 883
	e3.CreatedAt = "2026-10-02T13:06:00Z"
	e3.Level = "normal"
	e3.Target = "session:executor-B"
	e3.Body = "收到，按 8081 调整"
	if err := Append(root, projCode, columnCode, e3); err != nil {
		t.Fatalf("第 3 次 Append 返回错误: %v", err)
	}
	lines := readLines(t, mirrorPath(root))
	if len(lines) != 4 {
		t.Fatalf("连发三条应共 4 行（1 头+3 消息），实际 %d 行: %q", len(lines), lines)
	}
	headCount := 0
	for _, l := range lines {
		if l == HeaderComment {
			headCount++
		}
	}
	if headCount != 1 {
		t.Errorf("头注释应只出现 1 次，实际 %d 次", headCount)
	}
	// 消息行按序累加且与 RenderLine 全等（三形态逐行精确断言）。
	for i, e := range []Entry{e1, e2, e3} {
		if want := RenderLine(e); lines[i+1] != want {
			t.Errorf("第 %d 条消息行 = %q，期望 %q", i+1, lines[i+1], want)
		}
	}
}

// assertManualLine 断言 err 为 *ManualLineError 且携带完整待补行材料（§8.3，
// 上层 CLI 据此退出 5 并向 stderr 输出「手工补行: <行>」）。
func assertManualLine(t *testing.T, err error, wantLine string) {
	t.Helper()
	if err == nil {
		t.Fatal("Append 应返回错误，实际为 nil")
	}
	var mle *ManualLineError
	if !errors.As(err, &mle) {
		t.Fatalf("错误应可断言为 *ManualLineError，实际 %T: %v", err, err)
	}
	if mle.Line != wantLine {
		t.Errorf("补行材料 = %q，期望完整行 %q", mle.Line, wantLine)
	}
	if !strings.Contains(err.Error(), "手工补行: "+wantLine) {
		t.Errorf("错误文本应含可粘贴补行，实际 %q", err.Error())
	}
}

// TestWriteFail 写失败路径返回 *ManualLineError 且携带完整待补行。
// 注：POSIX chmod 只读目录在 Windows 上不生效（仅清只读 attrib，不拦创建），
// 故用跨平台稳定的失败注入：root 为普通文件 / 镜像路径被同名目录占位。
func TestWriteFail(t *testing.T) {
	e := sampleEntry()
	wantLine := RenderLine(e)

	t.Run("root为普通文件", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "ordinary-file")
		if err := os.WriteFile(root, []byte("i am a file"), 0o644); err != nil {
			t.Fatalf("预置普通文件失败: %v", err)
		}
		assertManualLine(t, Append(root, projCode, columnCode, e), wantLine)
	})

	t.Run("镜像路径被目录占位", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(mirrorPath(root), 0o755); err != nil {
			t.Fatalf("预置占位目录失败: %v", err)
		}
		assertManualLine(t, Append(root, projCode, columnCode, e), wantLine)
	})
}

// TestNoGitignore 镜像包不创建也不修改 .gitignore（AC13.2：镜像须被 git 跟踪，
// 忽略规则归仓库主人管理，包内零参与）。
func TestNoGitignore(t *testing.T) {
	t.Run("不创建", func(t *testing.T) {
		root := t.TempDir()
		if err := Append(root, projCode, columnCode, sampleEntry()); err != nil {
			t.Fatalf("Append 返回错误: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
			t.Errorf(".gitignore 不应被创建，stat 错误: %v", err)
		}
	})
	t.Run("不修改", func(t *testing.T) {
		root := t.TempDir()
		gi := filepath.Join(root, ".gitignore")
		orig := "*.db\naiteam.db\n"
		if err := os.WriteFile(gi, []byte(orig), 0o644); err != nil {
			t.Fatalf("预置 .gitignore 失败: %v", err)
		}
		if err := Append(root, projCode, columnCode, sampleEntry()); err != nil {
			t.Fatalf("Append 返回错误: %v", err)
		}
		data, err := os.ReadFile(gi)
		if err != nil {
			t.Fatalf("读取 .gitignore 失败: %v", err)
		}
		if string(data) != orig {
			t.Errorf(".gitignore 不应被修改，实际 %q，期望 %q", data, orig)
		}
	})
}
