package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 登记族 unarchive 动词 CLI 测试（M-31，AC3①~③）：照 archive 族测试骨架
// （in-process 端到端：真 store + 真 server 挂 httptest，命令函数注入其地址），
// 断言退出码+stdout/stderr 文本；帮助面验收两处——族内用法行（RunXxxGroup）
// 与总帮助登记组行（cmd/aiteam/main.go usage 文本源）。

// TestProjectUnarchive AC3① 端到端：归档后 unarchive 退出 0 输出 `OK unarchived`，
// 默认 project list 恢复可见（archive→unarchive 往返数据面闭环）。
func TestProjectUnarchive(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()

	// 前置：登记 proj-r 并归档。
	runWantCode(t, "register proj-r", 0, func() int {
		return runProjectRegister(ctx, append(regIdentityArgs(), "--code", "proj-r", "--name", "项目R"), addr)
	})
	runWantCode(t, "archive proj-r", 0, func() int {
		return runProjectArchive(ctx, append(regIdentityArgs(), "--code", "proj-r"), addr)
	})

	// AC3①：unarchive 退出 0 输出 OK unarchived。
	stdout, stderr := runWantCode(t, "project unarchive", 0, func() int {
		return runProjectUnarchive(ctx, append(regIdentityArgs(), "--code", "proj-r"), addr)
	})
	if !strings.Contains(stdout, "OK unarchived") {
		t.Errorf("project unarchive 输出 = %q，期望含 OK unarchived（AC3①，b1-spec §2.3）", stdout)
	}
	if stderr != "" {
		t.Errorf("project unarchive 成功不应有 stderr 输出，实际 %q", stderr)
	}

	// 数据面回归：默认 list 恢复可见 active（与 archive 测试的归档不可见对偶）。
	stdout, _ = runWantCode(t, "project list 复查", 0, func() int {
		return runProjectList(ctx, regIdentityArgs(), addr)
	})
	if want := "proj-r 项目R active 0\n"; !strings.Contains(stdout, want) {
		t.Errorf("unarchive 后默认 list 输出 = %q，期望含行 %q（反归档恢复可见）", stdout, want)
	}
}

// TestColumnUnarchive AC3① 端到端：column unarchive 同型（镜像 project 族），
// 归档后反归档输出 `OK unarchived`，默认 column list 恢复可见。
func TestColumnUnarchive(t *testing.T) {
	addr := startRegTestServer(t)
	ctx := context.Background()

	// 前置：登记栏目 05（挂引导项目下，--project 一参两用）并归档。
	runWantCode(t, "column register 05", 0, func() int {
		return runColumnRegister(ctx, append(regIdentityArgs(),
			"--project", regAuthProject, "--code", "05", "--name", "栏目05"), addr)
	})
	runWantCode(t, "column archive 05", 0, func() int {
		return runColumnArchive(ctx, append(regIdentityArgs(),
			"--project", regAuthProject, "--code", "05"), addr)
	})

	// AC3①：unarchive 退出 0 输出 OK unarchived。
	stdout, stderr := runWantCode(t, "column unarchive", 0, func() int {
		return runColumnUnarchive(ctx, append(regIdentityArgs(),
			"--project", regAuthProject, "--code", "05"), addr)
	})
	if !strings.Contains(stdout, "OK unarchived") {
		t.Errorf("column unarchive 输出 = %q，期望含 OK unarchived（AC3①，b1-spec §2.3）", stdout)
	}
	if stderr != "" {
		t.Errorf("column unarchive 成功不应有 stderr 输出，实际 %q", stderr)
	}

	// 数据面回归：默认 list 恢复可见 active。
	stdout, _ = runWantCode(t, "column list 复查", 0, func() int {
		return runColumnList(ctx, append(regIdentityArgs(), "--project", regAuthProject), addr)
	})
	if want := "05 栏目05 active\n"; !strings.Contains(stdout, want) {
		t.Errorf("unarchive 后默认 column list 输出 = %q，期望含行 %q（反归档恢复可见）", stdout, want)
	}
}

// TestUnarchiveHelpListsVerb AC3②：动词列表两处含 unarchive——族内用法行
// （RunXxxGroup 用法提示）与总帮助登记组行（cmd/aiteam/main.go usage()，os.Exit
// 直退不可进程内调用，照 skills_trees_sync_test.go 源码文本同步闸先例读源验收）。
func TestUnarchiveHelpListsVerb(t *testing.T) {
	for name, run := range map[string]func([]string) int{
		"project": RunProjectGroup,
		"column":  RunColumnGroup,
	} {
		var code int
		_, stderr := captureStdoutStderr(t, func() { code = run(nil) })
		if code != 2 {
			t.Errorf("%s 族空参退出码 = %d，期望 2", name, code)
		}
		if !strings.Contains(stderr, "unarchive") {
			t.Errorf("%s 族内用法行 = %q，期望含 unarchive（AC3②）", name, stderr)
		}
	}

	// 总帮助登记组行：project/column 两行各含一次 unarchive。
	raw, err := os.ReadFile(filepath.Join("..", "..", "cmd", "aiteam", "main.go"))
	if err != nil {
		t.Fatalf("读总帮助源 cmd/aiteam/main.go 失败: %v", err)
	}
	if got := strings.Count(string(raw), "<register|update|archive|unarchive|list>"); got != 2 {
		t.Errorf("总帮助登记组行含 unarchive 动词串处数 = %d，期望 2（project/column 各一，AC3②）", got)
	}
}

// TestUnarchiveMissingCode AC3③：缺 --code 目标参数 → CLI 本地校验退 2 不发请求
// （requireCode 先于 buildClient，dead server 下实测 2 即证明本地拦截，§4.7）。
func TestUnarchiveMissingCode(t *testing.T) {
	dead := closedServerAddr(t)
	ctx := context.Background()

	_, stderr := runWantCode(t, "project unarchive 缺 --code", 2, func() int {
		return runProjectUnarchive(ctx, regIdentityArgs(), dead)
	})
	if !strings.Contains(stderr, "--code") {
		t.Errorf("project unarchive 缺 --code stderr = %q，期望指明 --code（AC3③）", stderr)
	}

	_, stderr = runWantCode(t, "column unarchive 缺 --code", 2, func() int {
		return runColumnUnarchive(ctx, regIdentityArgs(), dead)
	})
	if !strings.Contains(stderr, "--code") {
		t.Errorf("column unarchive 缺 --code stderr = %q，期望指明 --code（AC3③）", stderr)
	}
}
