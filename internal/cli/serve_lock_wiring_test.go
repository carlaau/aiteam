package cli

// serve 单例锁接线测试族（b1 W3）：锁原语（W2）接入 serve 装配链的行为断言——
// 同 dbDir 第二实例启动即拒（三要件文案、退 1 语义）、创建失败≠被占两类文案区分、
// 启动→停机锁生命周期（Listen 后摘要回写完整形态、停机后同 dbDir 可立即再取锁）。
// 测试隔离口径同 serve_test.go 头注：t.TempDir+127.0.0.1:0 随机端口，零固定端口、
// 零 ~/.aiteam 触碰；进程内同构实测（unix 另一 fd/windows 另一句柄同被拒，见
// servelock_unix.go/servelock_windows.go 各自注）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestServeLockHeldMessage 被占类文案构造（纯函数面）：三要件=锁文件路径+持有者
// 信息+按 PID 处置指引；持有者摘要解析出 pid/listen 则如例格式化，读取失败/为空/
// 不可解析降级「持有者信息不可读」句式不变；netstat 定位端口优先取持有者 listen。
func TestServeLockHeldMessage(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), serveLockFile)

	t.Run("持有者pid与listen齐备", func(t *testing.T) {
		holder, err := json.Marshal(serveLockInfo{PID: 1234, Listen: "0.0.0.0:8310"})
		if err != nil {
			t.Fatalf("构造持有者摘要失败: %v", err)
		}
		msg := serveLockHeldMessage(lockPath, string(holder), "0.0.0.0:9999")
		for _, want := range []string{lockPath, "pid=1234", "listen 0.0.0.0:8310", "按 PID 处置", "findstr :8310"} {
			if !strings.Contains(msg, want) {
				t.Errorf("文案 %q 应含 %q（三要件+定位端口取持有者 listen）", msg, want)
			}
		}
		if strings.Contains(msg, "findstr :9999") {
			t.Errorf("定位端口应取持有者 listen（8310），不得回落本次配置端口: %q", msg)
		}
	})

	t.Run("摘要不可读降级句式不变", func(t *testing.T) {
		msg := serveLockHeldMessage(lockPath, "", "0.0.0.0:8310")
		for _, want := range []string{lockPath, "持有者信息不可读", "按 PID 处置", "findstr :8310"} {
			if !strings.Contains(msg, want) {
				t.Errorf("降级文案 %q 应含 %q（句式结构不变，端口回落本次配置）", msg, want)
			}
		}
		if strings.Contains(msg, "pid=") {
			t.Errorf("降级文案不得出现 pid=: %q", msg)
		}
	})

	t.Run("摘要不可解析降级", func(t *testing.T) {
		msg := serveLockHeldMessage(lockPath, "not-json{", "127.0.0.1:0")
		for _, want := range []string{lockPath, "持有者信息不可读", "按 PID 处置"} {
			if !strings.Contains(msg, want) {
				t.Errorf("降级文案 %q 应含 %q", msg, want)
			}
		}
		// 随机端口（:0）无定位意义：netstat 提示省略
		if strings.Contains(msg, "findstr") {
			t.Errorf("配置端口为 :0 时不得给出 netstat 提示: %q", msg)
		}
	})

	t.Run("取锁后未及Listen的残留只有pid", func(t *testing.T) {
		holder, err := json.Marshal(serveLockInfo{PID: 77})
		if err != nil {
			t.Fatalf("构造持有者摘要失败: %v", err)
		}
		msg := serveLockHeldMessage(lockPath, string(holder), "0.0.0.0:8321")
		for _, want := range []string{"pid=77", "按 PID 处置", "findstr :8321"} {
			if !strings.Contains(msg, want) {
				t.Errorf("文案 %q 应含 %q（listen 缺省时省略、端口回落本次配置）", msg, want)
			}
		}
		if strings.Contains(msg, "listen ") {
			t.Errorf("残留摘要无 listen 时文案不得杜撰: %q", msg)
		}
	})
}

// TestServeLockHeldSecondInstanceRejected AC1 进程内同构：同 dbDir 已有活锁时
// 第二个 serve 启动即拒——err 非 nil、不包 ErrUsage（退 1 语义，main 层映射），
// 文案含锁文件路径与持有者 pid（三要件）。5s 超时兜底：接线缺失（红态）时第二
// 实例会真启动阻塞，超时取消使其以 nil 收尾、测试以「应拒却无错」判红不挂死。
func TestServeLockHeldSecondInstanceRejected(t *testing.T) {
	dbDir := t.TempDir()
	rel, _, err := acquireServeLock(dbDir) // 首实例：真锁在手（同包锁原语实测）
	if err != nil {
		t.Fatalf("首实例取锁失败: %v", err)
	}
	defer rel()
	lockPath := filepath.Join(dbDir, serveLockFile)

	dbPath := filepath.Join(dbDir, "aiteam.db")
	cfgPath := writeTempConfig(t, dbPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = serve(ctx, []string{"--config", cfgPath}, nil)

	if err == nil {
		t.Fatal("同 dbDir 第二个 serve 应被单例锁拒绝，实际无错误返回（已启动？）")
	}
	msg := err.Error()
	if errors.Is(err, ErrUsage) {
		t.Errorf("被占拒绝属运行期错误应退 1，不得包 ErrUsage（退 2）: %v", err)
	}
	if !strings.Contains(msg, lockPath) {
		t.Errorf("错误文本 %q 应含锁文件路径 %s（三要件之一）", msg, lockPath)
	}
	if !strings.Contains(msg, fmt.Sprintf("pid=%d", os.Getpid())) {
		t.Errorf("错误文本 %q 应含持有者 pid=%d（三要件之二）", msg, os.Getpid())
	}
	if !strings.Contains(msg, "按 PID 处置") {
		t.Errorf("错误文本 %q 应含按 PID 处置指引（三要件之三）", msg)
	}
}

// TestServeLockHeldSecondInstanceReadsListen AC1 完整形态端到端：持锁运行中的实例
// （非裸 acquireServeLock 模拟）Listen 后摘要已回写 listen——第二实例被拒的文案
// 含「listen <A 的实际监听地址>」，钉死「持锁运行中→被占方读到 listen 并格式化」
// 的完整数据流（TestServeLockHeldSecondInstanceRejected 只钉 pid，摘要无 listen）。
// A 的实际监听地址从 ready 回调拿（随机端口），断言用该真实地址串；5s 超时兜底
// 形态同上（红态时 B 会真启动阻塞，超时取消以 nil 收尾判红不挂死）。
func TestServeLockHeldSecondInstanceReadsListen(t *testing.T) {
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "aiteam.db")
	cfgA := writeTempConfig(t, dbPath)

	ready, errs, _, _ := startServeForTest(t, []string{"--config", cfgA})
	addr := waitReady(t, ready, errs) // ready 即同步点：摘要回写已在 Listen 后完成

	cfgB := writeTempConfig(t, dbPath) // B 同 dbDir：锁目录一致即被拒（拒在开库前，同 dbPath 无碍）
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := serve(ctx, []string{"--config", cfgB}, nil)

	if err == nil {
		t.Fatal("持锁实例运行中同 dbDir 第二个 serve 应被拒，实际无错误返回")
	}
	if !strings.Contains(err.Error(), "listen "+addr) {
		t.Errorf("错误文本 %q 应含持有者实际监听地址 listen %s（C1 回写后的完整诊断）", err.Error(), addr)
	}
}

// TestServeLockCreateFailureNoHolderDiag 两类文案区分：锁文件被目录占位令创建必败
// （MkdirAll 建的是 dbDir 本身，不受影响）——错误属普通系统错误（非「被占」类）、
// 不附持有者诊断、如实上报；拒启发生在开库前，db 文件零副作用。
func TestServeLockCreateFailureNoHolderDiag(t *testing.T) {
	dbDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dbDir, serveLockFile), 0o755); err != nil {
		t.Fatalf("预置目录占位失败: %v", err)
	}
	dbPath := filepath.Join(dbDir, "aiteam.db")
	cfgPath := writeTempConfig(t, dbPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := serve(ctx, []string{"--config", cfgPath}, nil)

	if err == nil {
		t.Fatal("锁文件创建失败应拒启，实际无错误返回")
	}
	if errors.Is(err, ErrLocked) {
		t.Errorf("创建失败不得归入「被占」类（errors.Is(err, ErrLocked) 成立）: %v", err)
	}
	msg := err.Error()
	if strings.Contains(msg, "持有者") {
		t.Errorf("创建失败类错误不得附持有者诊断: %q", msg)
	}
	if !strings.Contains(msg, "创建 serve 单例锁失败") {
		t.Errorf("创建失败类错误应如实上报系统错误（创建 serve 单例锁失败口径）: %q", msg)
	}
	// 拒启发生在开库前：db 文件不应生成
	if _, statErr := os.Stat(dbPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("拒绝路径不应开库，db 文件已生成（stat err=%v）", statErr)
	}
}

// TestServeLockLifecycleRewriteAndRelease AC2 进程内同构：启动→锁在盘且 Listen 后
// 摘要回写为完整形态（listen=实际监听地址、db=绝对路径、pid=当前）；停机后同 dbDir
// 可立即再取锁（「已释放」语义可得）。
func TestServeLockLifecycleRewriteAndRelease(t *testing.T) {
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "aiteam.db")
	cfgPath := writeTempConfig(t, dbPath)
	lockPath := filepath.Join(dbDir, serveLockFile)

	ready, errs, done, cancel := startServeForTest(t, []string{"--config", cfgPath})
	addr := waitReady(t, ready, errs)

	// 启动后锁文件在盘
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("serve 启动后锁文件应在盘: %v", err)
	}
	// Listen 成功后摘要回写完整形态（readSummaryForTest 走偏移绕行独立读取）
	raw := readSummaryForTest(t, lockPath)
	var info serveLockInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("摘要应可解析为 JSON，实际 %q: %v", raw, err)
	}
	if info.Listen != addr {
		t.Errorf("摘要 listen=%q 应为实际监听地址 %q（C1 回写）", info.Listen, addr)
	}
	if info.DB != dbPath {
		t.Errorf("摘要 db=%q 应为库文件绝对路径 %q（C1 回写）", info.DB, dbPath)
	}
	if info.PID != os.Getpid() {
		t.Errorf("摘要 pid=%d 应为当前进程 %d", info.PID, os.Getpid())
	}

	cancel()
	select {
	case <-done: // serve goroutine 收敛（含显式 release 与句柄关闭）
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内 serve 未收敛")
	}

	// 停机后同 dbDir 可立即再取锁：锁确已释放
	rel, _, err := acquireServeLock(dbDir)
	if err != nil {
		t.Fatalf("停机后同 dbDir 应可立即再取锁（锁未释放？）: %v", err)
	}
	rel()
}
