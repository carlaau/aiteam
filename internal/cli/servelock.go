package cli

// serve 单例锁（b1）：同一 db 目录起第二个 aiteam serve 时启动即拒并给出持有者
// 诊断。机制=db 目录级 OS 独占锁文件（平台原语见 servelock_unix.go /
// servelock_windows.go）——持有者死亡=锁由内核自动释放，零陈旧态零误判，
// 「能锁上=没有活实例」是 OS 保证的真值，不做 PID 存活探测。锁文件留壳不删
// （删文件有并发竞态窗口，见 serveLockFile 注）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// serveLockFile serve 单例锁文件名：落 db 文件同目录（锁粒度=db 目录，不做全局
// 单例）。锁文件不删只留壳：删文件有「旧持有者已解锁未删、新持有者恰好新建并
// 锁上、旧持有者才删掉新壳」的竞态窗口；锁的排他性来自 OS 锁而非文件存在性，
// 留壳无害。
const serveLockFile = "aiteam-serve.lock"

// serveLockSummaryOffset 持有者摘要区起始偏移：字节 0 是锁字节——windows 强制锁
// 锁 [0,1)，锁区对所有其他句柄的读写必拒（含同进程，实测 ERROR_LOCK_VIOLATION），
// 摘要后置一字节、读写均绕开锁字节，unix/windows 两侧同构。
const serveLockSummaryOffset = 1

// serveLockSummaryMaxRead 摘要读取上限：锁文件可能被外部污染成超大文件，诊断
// 读取按前缀封顶防巨量分配（单行 JSON 摘要远小于此，前缀足够诊断）。
const serveLockSummaryMaxRead = 4 << 10

// ErrLocked 「锁被占」类错误哨兵：取锁失败两类区分的关键——被占时
// errors.Is(err, ErrLocked) 成立且 holder 带持有者诊断；锁文件创建失败等系统
// 拒绝不落入此类、不附持有者诊断（创建失败≠被占）。
var ErrLocked = errors.New("serve 锁被占")

// errLockRegionHeld 平台原语层内部哨兵：flock/LockFileEx 返回「锁区已被占」类
// errno 时映射到它，编排层据此归入 ErrLocked 类，不外泄。
var errLockRegionHeld = errors.New("锁区已被其他持有者占用")

// serveLockHeldError 锁被占错误：携带锁文件路径与持有者摘要（可空=信息不可读），
// Unwrap 归到 ErrLocked 供调用方分类。
type serveLockHeldError struct {
	path   string
	holder string
}

func (e *serveLockHeldError) Error() string {
	if e.holder == "" {
		return fmt.Sprintf("serve 锁被占（%s），另一实例可能正在运行（持有者信息不可读）", e.path)
	}
	return fmt.Sprintf("serve 锁被占（%s），另一实例可能正在运行；持有者: %s", e.path, e.holder)
}

func (e *serveLockHeldError) Unwrap() error { return ErrLocked }

// serveLockInfo 锁文件持有者摘要（单行 JSON）。pid/db_dir/start_at 在取锁时点写入；
// listen（实际监听地址）/db（库文件绝对路径）在取锁时点不可得（取锁先于 Listen），
// 由 serve 接线在 Listen 成功后回写补齐（rewriteServeLockSummary）——第二实例被拒
// 时诊断才含 listen。omitempty：取锁时点摘要保持三件形态。
type serveLockInfo struct {
	PID     int    `json:"pid"`
	DBDir   string `json:"db_dir"`
	Listen  string `json:"listen,omitempty"`
	DB      string `json:"db,omitempty"`
	StartAt string `json:"start_at"`
}

// acquireServeLock 取 db 目录级 serve 单例锁（spec 冻结签名）。包级函数变量以便
// 测试换桩（同包 exeDir 惯例）。
//
// 返回值语义：成功时 err=nil、release 幂等可多次调（sync.Once）、holder 空串；
// 被占时 err 属 ErrLocked 类、holder=锁文件读到的摘要文本（可空串）；创建/取锁
// 的系统性失败为普通错误（非 ErrLocked 类）、holder 空、release nil。
var acquireServeLock = acquireServeLockOS

// acquireServeLockOS acquireServeLock 真实现：建/开锁文件→平台非阻塞独占锁→
// 尽力写持有者摘要。摘要写在锁到手之后且失败不回滚（写失败只 Warn 不影响锁的
// 取得）——排他性由 OS 锁保证，被占方读不到摘要自然降级为「不可读」。
func acquireServeLockOS(dbDir string) (release func(), holder string, err error) {
	path := filepath.Join(dbDir, serveLockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, "", fmt.Errorf("创建 serve 锁文件失败（%s）: %w", path, err)
	}
	rel, err := lockExclusive(f)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, errLockRegionHeld) {
			diag := readServeLockSummary(path)
			return nil, diag, &serveLockHeldError{path: path, holder: diag}
		}
		return nil, "", fmt.Errorf("取 serve 锁失败（%s）: %w", path, err)
	}
	writeServeLockSummary(f, dbDir)
	var once sync.Once
	return func() {
		once.Do(func() {
			rel()
			_ = f.Close() // 进程退出/句柄关闭亦由系统兜底释放，此处是正常路径收尾
		})
	}, "", nil
}

// writeServeLockSummary 写持有者摘要（单行 JSON，尽力而为）：经锁句柄本身在锁字节
// 之后写入（windows 强制锁下同句柄持有者可写）；写前截断到锁字节之后——崩溃重启
// 留下的旧摘要可能比新摘要长，不截断会残尾致 JSON 不可解析。失败只 Warn 不影响
// 锁的取得。
func writeServeLockSummary(f *os.File, dbDir string) {
	b, err := json.Marshal(serveLockInfo{
		PID:     os.Getpid(),
		DBDir:   dbDir,
		StartAt: time.Now().Format(time.RFC3339),
	})
	if err != nil {
		// 理论不可达：字段全是可序列化标量；对冲未来加字段引入序列化失败
		slog.Warn("serve 锁摘要序列化失败（不影响锁的持有）", "err", err)
		return
	}
	if err := f.Truncate(serveLockSummaryOffset); err != nil {
		slog.Warn("serve 锁摘要截断失败（不影响锁的持有）", "lock", f.Name(), "err", err)
		return
	}
	if _, err := f.WriteAt(b, serveLockSummaryOffset); err != nil {
		slog.Warn("serve 锁摘要写入失败（不影响锁的持有）", "lock", f.Name(), "err", err)
	}
}

// readServeLockSummary 读持有者摘要（锁字节之后起读，绕开强制锁）：任何失败/为空
// 一律返回空串——诊断降级为「持有者信息不可读」，不影响错误归类。
func readServeLockSummary(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() <= serveLockSummaryOffset {
		return ""
	}
	b := make([]byte, min(st.Size()-serveLockSummaryOffset, serveLockSummaryMaxRead))
	if _, err := f.ReadAt(b, serveLockSummaryOffset); err != nil {
		return ""
	}
	return string(bytes.Trim(b, "\x00 \t\r\n"))
}
