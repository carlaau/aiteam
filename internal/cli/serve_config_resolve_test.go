package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件是「序 9 部署结构拍平」W4 测试面：serve 解析链 exe 目录基准（W1）+
// 首跑自生成 config（W2）。既有 serve 测试基建（startServeForTest/waitReady/
// assertPingOK/assertTablesComplete/writeTempConfig）复用 serve_test.go 同包同源。
//
// 端口策略补充（serve_test.go 头注的延伸）：本文件两个用例（首跑生成/内建默认
// 落位）走「自占 8310」形态——测试先试绑 0.0.0.0:8310 并持有，令 serve 的 Listen
// 步骤必然失败，从而确定性地断言 Listen 之前的落盘行为（config 生成+db 建库落位），
// 不依赖环境端口空闲：试绑失败（本机常驻服务占用 8310）时 serve 同样在 Listen 步
// 失败，断言路径等价成立。其余用例一律 127.0.0.1:0 随机端口（套件头注口径：
// 除 DefaultBoot 外不给测试绑固定端口）。

// injectExeDir 换装包级 exeDir 桩至 dir，测试结束自动恢复——W4「可注入点覆盖」
// 的机械载体：go test 下 os.Executable 返回测试二进制的临时构建路径，注入
// t.TempDir 才能对「部署根落位」做隔离断言。cli 包测试零 t.Parallel（全包 grep
// 复核），全局换装串行无竞态。
func injectExeDir(t *testing.T, dir string) {
	t.Helper()
	old := exeDir
	exeDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { exeDir = old })
}

// occupy8310 试绑并持有 0.0.0.0:8310（自占形态，见文件头注）。绑定失败（8310
// 已被环境占用）不视为错误——serve 将在 Listen 步以同样方式失败，断言路径不变。
func occupy8310(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:8310")
	if err != nil {
		return
	}
	t.Cleanup(func() { _ = ln.Close() })
}

// waitServeError 等 serve 以错误收敛（Listen 自占必败形态），5s 超时判失败。
func waitServeError(t *testing.T, errs <-chan error) error {
	t.Helper()
	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("serve 应以错误退出（Listen 自占必败），实际 nil")
		}
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内 serve 未返回（未在 Listen 步失败？）")
		return nil
	}
}

// TestServeConfigResolutionStates W1 解析链三态（显式 --config > exe 目录探测 >
// 内建默认）：
//
//	①显式 --config 存在→加载该文件（自定义 listen 生效）；
//	②显式 --config 不存在→报错拒启（不静默回落默认、零落盘副作用）；
//	③exe 目录有 aiteam-config.json（无显式）→被探测加载且不覆盖；
//	④exe 目录无→内建默认 + db 落 exe 目录 db/ 子目录 + 首跑生成 config。
func TestServeConfigResolutionStates(t *testing.T) {
	t.Run("显式config存在则加载", func(t *testing.T) {
		root := t.TempDir()
		injectExeDir(t, root)
		dbPath := filepath.Join(t.TempDir(), "explicit", "aiteam.db")
		cfgPath := writeTempConfig(t, dbPath)

		ready, errs, _, _ := startServeForTest(t, []string{"--config", cfgPath})
		addr := waitReady(t, ready, errs)

		// 自定义 listen 生效：回环随机端口（非内建默认 0.0.0.0:8310）
		if !strings.HasPrefix(addr, "127.0.0.1:") || strings.HasSuffix(addr, ":8310") {
			t.Errorf("实际监听地址 = %s，期望按显式配置 127.0.0.1:<随机端口>", addr)
		}
		assertPingOK(t, addr)
		if _, err := os.Stat(dbPath); err != nil {
			t.Fatalf("显式配置 db 路径未生效: %v", err)
		}
		assertTablesComplete(t, dbPath)
	})

	t.Run("显式config不存在拒启", func(t *testing.T) {
		root := t.TempDir()
		injectExeDir(t, root)
		missing := filepath.Join(root, "no-such-config.json")

		// 直调 serve 断言拒启（拒启用例带 5s 超时兜底，形态同 TestServeStartupEmptyToken）
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := serve(ctx, []string{"--config", missing}, nil)

		if err == nil {
			t.Fatal("显式 --config 指向不存在文件应拒启，实际无错误（静默回落默认？）")
		}
		if !strings.Contains(err.Error(), "不存在") {
			t.Errorf("错误信息 %q 应包含「不存在」", err.Error())
		}
		// 拒启零副作用：exe 目录不生成 config、不建 db/
		if _, serr := os.Stat(filepath.Join(root, defaultConfigFile)); !errors.Is(serr, os.ErrNotExist) {
			t.Errorf("拒启路径不应生成配置文件（stat err=%v）", serr)
		}
		if _, serr := os.Stat(filepath.Join(root, "db")); !errors.Is(serr, os.ErrNotExist) {
			t.Errorf("拒启路径不应创建 db 目录（stat err=%v）", serr)
		}
	})

	t.Run("exe目录config被探测且不覆盖", func(t *testing.T) {
		root := t.TempDir()
		injectExeDir(t, root)
		dbPath := filepath.Join(t.TempDir(), "probed", "aiteam.db")
		cfgPath := filepath.Join(root, defaultConfigFile)
		marker := fmt.Sprintf(`{"_doc":"探测标记-勿覆盖","listen":"127.0.0.1:0","db":{"path":%s}}`,
			fmt.Sprintf("%q", dbPath))
		if err := os.WriteFile(cfgPath, []byte(marker), 0o644); err != nil {
			t.Fatalf("预写 exe 目录配置失败: %v", err)
		}

		ready, errs, _, _ := startServeForTest(t, nil) // 无参启动：应探测到 exe 目录的配置
		addr := waitReady(t, ready, errs)

		if !strings.HasPrefix(addr, "127.0.0.1:") || strings.HasSuffix(addr, ":8310") {
			t.Errorf("实际监听地址 = %s，期望按探测到的配置 127.0.0.1:<随机端口>", addr)
		}
		assertPingOK(t, addr)
		if _, err := os.Stat(dbPath); err != nil {
			t.Fatalf("探测配置 db 路径未生效: %v", err)
		}
		// 已存在不覆盖：标记内容原样
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatalf("复读配置失败: %v", err)
		}
		if string(data) != marker {
			t.Errorf("已存在的配置被覆盖:\n got=%s\nwant=%s", data, marker)
		}
	})

	t.Run("exe目录无config内建默认db落exe目录", func(t *testing.T) {
		root := t.TempDir()
		injectExeDir(t, root)
		occupy8310(t) // 令 Listen 必败：确定性地断言 Listen 前的落盘行为（见文件头注）

		_, errs, _, _ := startServeForTest(t, nil)
		err := waitServeError(t, errs)
		if !strings.Contains(err.Error(), "8310") {
			t.Errorf("错误信息 %q 应含 8310（应失败于 Listen 步而非更早）", err.Error())
		}

		// 首跑生成 + 内建默认 db 落 exe 目录 db/ 子目录（不再落 CWD）
		if _, serr := os.Stat(filepath.Join(root, defaultConfigFile)); serr != nil {
			t.Fatalf("exe 目录未生成默认配置: %v", serr)
		}
		dbPath := filepath.Join(root, "db", "aiteam.db")
		if _, serr := os.Stat(dbPath); serr != nil {
			t.Fatalf("内建默认 db 未落 exe 目录 db/ 子目录: %v", serr)
		}
		assertTablesComplete(t, dbPath)
	})
}

// TestServeFirstRunGeneratesConfig W2 首跑自生成：exe 目录无 config 时首跑生成
// 内容=configExampleTemplate 逐字节一致（与 config init 单一来源）；重启不覆盖
// 手改（幂等）。
func TestServeFirstRunGeneratesConfig(t *testing.T) {
	t.Run("首跑生成与模板逐字节一致", func(t *testing.T) {
		root := t.TempDir()
		injectExeDir(t, root)
		occupy8310(t)

		_, errs, _, _ := startServeForTest(t, nil)
		waitServeError(t, errs) // Listen 必败形态：config 生成/建库已先行完成

		data, err := os.ReadFile(filepath.Join(root, defaultConfigFile))
		if err != nil {
			t.Fatalf("首跑未生成配置文件: %v", err)
		}
		if string(data) != configExampleTemplate {
			t.Errorf("生成内容与 configExampleTemplate 漂移（单一来源破坏）:\n got=%s\nwant=%s", data, configExampleTemplate)
		}
		if _, err := os.Stat(filepath.Join(root, "db", "aiteam.db")); err != nil {
			t.Errorf("首跑默认库未落 exe 目录 db/: %v", err)
		}
	})

	t.Run("重启不覆盖手改配置", func(t *testing.T) {
		root := t.TempDir()
		injectExeDir(t, root)
		dbPath := filepath.Join(t.TempDir(), "keep", "aiteam.db")
		cfgPath := filepath.Join(root, defaultConfigFile)
		marker := fmt.Sprintf(`{"_doc":"手改标记-重启勿覆盖","listen":"127.0.0.1:0","db":{"path":%s}}`,
			fmt.Sprintf("%q", dbPath))
		if err := os.WriteFile(cfgPath, []byte(marker), 0o644); err != nil {
			t.Fatalf("预写手改配置失败: %v", err)
		}

		// 两跑同断言：预写即「第一跑已存在」，第二跑=重启——均不得覆盖
		for round := 1; round <= 2; round++ {
			ready, errs, done, cancel := startServeForTest(t, nil)
			addr := waitReady(t, ready, errs)
			assertPingOK(t, addr)
			cancel()
			select {
			case <-done: // 等 goroutine 收敛+句柄释放（Windows 文件锁，形态同 serve_test）
			case <-time.After(5 * time.Second):
				t.Fatalf("第 %d 跑 serve 未收敛", round)
			}
			data, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatalf("第 %d 跑后读配置失败: %v", round, err)
			}
			if string(data) != marker {
				t.Errorf("第 %d 跑后手改配置被覆盖:\n got=%s\nwant=%s", round, data, marker)
			}
		}
	})
}

// TestServeDBPathResolution W1 db 解析基准：相对值相对 exe 目录（非 config 文件
// 所在目录、非 CWD）；绝对路径照用；缺省 db 段走内建默认 db/aiteam.db 落 exe 目录。
func TestServeDBPathResolution(t *testing.T) {
	t.Run("相对值相对exe目录解析", func(t *testing.T) {
		root := t.TempDir()
		injectExeDir(t, root)
		// 显式 config 放另一临时目录（非 exe 目录）——db 相对值必须锚 exe 目录，
		// 不锚 config 文件所在目录（部署根语义的判定性区分）
		cfgPath := writeTempConfig(t, "custom/data.db")

		ready, errs, _, _ := startServeForTest(t, []string{"--config", cfgPath})
		addr := waitReady(t, ready, errs)
		assertPingOK(t, addr)

		dbPath := filepath.Join(root, "custom", "data.db")
		if _, err := os.Stat(dbPath); err != nil {
			t.Fatalf("相对 db 值未落 exe 目录（期望 %s）: %v", dbPath, err)
		}
		assertTablesComplete(t, dbPath) // custom/ 目录由 serve 自动创建
	})

	t.Run("绝对路径照用", func(t *testing.T) {
		root := t.TempDir()
		injectExeDir(t, root)
		dbPath := filepath.Join(t.TempDir(), "abs-anchor", "aiteam.db")
		cfgPath := writeTempConfig(t, dbPath)

		ready, errs, _, _ := startServeForTest(t, []string{"--config", cfgPath})
		addr := waitReady(t, ready, errs)
		assertPingOK(t, addr)

		if _, err := os.Stat(dbPath); err != nil {
			t.Fatalf("绝对 db 路径未照用（期望 %s）: %v", dbPath, err)
		}
		// 绝对路径不与部署根拼接：exe 目录不应出现 db/
		if _, err := os.Stat(filepath.Join(root, "db")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("绝对路径不应在 exe 目录产生 db/（stat err=%v）", err)
		}
	})

	t.Run("缺省db段落exe目录db子目录", func(t *testing.T) {
		root := t.TempDir()
		injectExeDir(t, root)
		// 配置只写 listen（db 段缺省）→ db.path 保持内建默认 db/aiteam.db
		cfgPath := filepath.Join(t.TempDir(), defaultConfigFile)
		if err := os.WriteFile(cfgPath, []byte(`{"listen":"127.0.0.1:0"}`), 0o644); err != nil {
			t.Fatalf("写仅 listen 配置失败: %v", err)
		}

		ready, errs, _, _ := startServeForTest(t, []string{"--config", cfgPath})
		addr := waitReady(t, ready, errs)
		assertPingOK(t, addr)

		dbPath := filepath.Join(root, "db", "aiteam.db")
		if _, err := os.Stat(dbPath); err != nil {
			t.Fatalf("内建默认 db.path 未落 exe 目录 db/ 子目录: %v", err)
		}
		assertTablesComplete(t, dbPath)
	})
}
