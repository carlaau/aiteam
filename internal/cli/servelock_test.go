package cli

// serve 单例锁测试族（b1 W1）：进程内同 dbDir 两次 acquire 即真锁实测——unix 侧
// flock 对同进程另一 fd 视为独立竞争者、windows 侧 LockFileEx 强制锁对另一句柄
// 必拒（探针实测 ERROR_LOCK_VIOLATION），无需跨进程。零网络、零 ~/.aiteam 触碰。

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// readSummaryForTest 从锁文件锁字节之后（偏移 1）独立读取摘要原文：与实现层
// readServeLockSummary 分路——本 helper 走裸 os.ReadAt，防「用被测函数自己验证
// 自己」的循环断言（windows 强制锁下整文件读会撞锁字节，必经偏移绕行）。
func readSummaryForTest(t *testing.T, lockPath string) string {
	t.Helper()
	f, err := os.Open(lockPath)
	if err != nil {
		t.Fatalf("打开锁文件失败: %v", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatalf("锁文件 Stat 失败: %v", err)
	}
	if st.Size() <= serveLockSummaryOffset {
		return ""
	}
	b := make([]byte, st.Size()-serveLockSummaryOffset)
	if _, err := f.ReadAt(b, serveLockSummaryOffset); err != nil {
		t.Fatalf("锁文件偏移 %d 起读失败: %v", serveLockSummaryOffset, err)
	}
	return strings.Trim(string(b), "\x00 \t\r\n")
}

// TestServelockMutualExclusion 断言 1：同 dbDir 第二次 acquire 必败，且 err 属
// 「被占」类（errors.Is(err, ErrLocked)）；被占方同时应读到含当前 pid 的持有者
// 摘要（摘要读取降级语义由断言 5 之外的结构保证，此处断言正常读取路径）。
func TestServelockMutualExclusion(t *testing.T) {
	dbDir := t.TempDir()
	rel, _, err := acquireServeLock(dbDir)
	if err != nil {
		t.Fatalf("首次取锁应成功，实际: %v", err)
	}
	defer rel()

	rel2, holder, err := acquireServeLock(dbDir)
	if err == nil {
		rel2()
		t.Fatal("同 dbDir 第二次取锁应失败（互斥失效）")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("第二次取锁的 err 应属「被占」类（errors.Is(err, ErrLocked)），实际: %v", err)
	}
	if rel2 != nil {
		t.Fatal("被占时 release 应为 nil")
	}
	// 紧邻形态断言（"pid":<pid>）：松散含 pid 数字会与 db_dir 随机路径里的数字
	// 巧合命中，紧凑 JSON 的键值相邻对才是摘要真实含 pid 的证据。
	want := `"pid":` + strconv.Itoa(os.Getpid())
	if !strings.Contains(holder, want) {
		t.Fatalf("被占方应读到含 %s 的持有者摘要，实际 holder=%q", want, holder)
	}
}

// TestServelockReleaseReacquire 断言 2：release 幂等可多次调，释放后同进程可重新
// 取得（顺带覆盖「摘要重写」路径：二次 acquire 会对首次留下的摘要做截断重写）。
func TestServelockReleaseReacquire(t *testing.T) {
	dbDir := t.TempDir()
	rel, _, err := acquireServeLock(dbDir)
	if err != nil {
		t.Fatalf("首次取锁失败: %v", err)
	}
	rel()
	rel() // 幂等：重复调用不得 panic/报错

	rel2, _, err := acquireServeLock(dbDir)
	if err != nil {
		t.Fatalf("释放后重新取锁应成功，实际: %v", err)
	}
	defer rel2()
}

// TestServelockSummaryJSON 断言 3：取得锁后锁文件含可解析的单行 JSON 摘要，
// pid 字段=当前进程；db_dir/start_at 字段在位。
func TestServelockSummaryJSON(t *testing.T) {
	dbDir := t.TempDir()
	rel, _, err := acquireServeLock(dbDir)
	if err != nil {
		t.Fatalf("取锁失败: %v", err)
	}
	defer rel()

	lockPath := filepath.Join(dbDir, serveLockFile)
	raw := readSummaryForTest(t, lockPath)
	if raw == "" {
		t.Fatal("锁文件摘要为空")
	}
	if strings.Contains(strings.TrimLeft(raw, "\x00"), "\n") {
		t.Fatalf("摘要应单行，实际含换行: %q", raw)
	}
	var info serveLockInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("摘要应可解析为 JSON，实际 %q: %v", raw, err)
	}
	if info.PID != os.Getpid() {
		t.Fatalf("摘要 pid=%d 应为当前进程 %d", info.PID, os.Getpid())
	}
	if info.DBDir == "" || info.StartAt == "" {
		t.Fatalf("摘要 db_dir/start_at 应在位，实际: %q", raw)
	}
}

// TestServelockInjectableStub 断言 4：注入点生效——换桩包级 acquireServeLock 变量
// 后调用走桩路径（同包 exeDir 惯例），证明 os 依赖可替换、serve 接线层测试可注入。
func TestServelockInjectableStub(t *testing.T) {
	old := acquireServeLock
	called := false
	acquireServeLock = func(dbDir string) (func(), string, error) {
		called = true
		return func() {}, "桩持有者", nil
	}
	t.Cleanup(func() { acquireServeLock = old })

	rel, holder, err := acquireServeLock(t.TempDir())
	if err != nil {
		t.Fatalf("桩路径不应报错: %v", err)
	}
	if !called {
		t.Fatal("应走注入桩路径，实际仍走真实现")
	}
	if holder != "桩持有者" {
		t.Fatalf("holder 应来自桩，实际 %q", holder)
	}
	if rel == nil {
		t.Fatal("桩 release 应原样透传（非 nil）")
	}
	rel()
}

// TestServelockCreateFailureNotHeld 断言 5：创建失败≠被占——锁文件路径被目录占位
// 令创建必败，此时 err 属普通系统错误（errors.Is(err, ErrLocked) 不成立）、不附
// 持有者诊断（holder 空串）。
func TestServelockCreateFailureNotHeld(t *testing.T) {
	dbDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dbDir, serveLockFile), 0o755); err != nil {
		t.Fatalf("预置目录占位失败: %v", err)
	}
	rel, holder, err := acquireServeLock(dbDir)
	if err == nil {
		rel()
		t.Fatal("锁文件被目录占位时取锁应失败")
	}
	if errors.Is(err, ErrLocked) {
		t.Fatalf("创建失败的 err 不得归入「被占」类，实际: %v", err)
	}
	if holder != "" {
		t.Fatalf("创建失败不得附持有者诊断，实际 holder=%q", holder)
	}
	if rel != nil {
		t.Fatal("失败路径 release 应为 nil")
	}
}
