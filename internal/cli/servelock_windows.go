//go:build windows

package cli

import (
	"errors"
	"log/slog"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// lockExclusive 对已打开的锁文件取 LockFileEx 非阻塞独占锁（强制锁）：字节范围
// offset 0 len 1，进程死/句柄闭即由系统释放，零陈旧态。同进程另一句柄重复取锁
// 同样被拒（ERROR_LOCK_VIOLATION，实测），进程内互斥测试即真锁实测。
// 锁区字节 0 在强制锁下对所有其他句柄读写皆拒——持有者摘要因此后置到偏移 1
// （见 servelock.go serveLockSummaryOffset），读写均绕开锁字节。
func lockExclusive(f *os.File) (release func(), err error) {
	h := windows.Handle(f.Fd())
	if err := windows.LockFileEx(h,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &windows.Overlapped{}); err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errLockRegionHeld
		}
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if uerr := windows.UnlockFileEx(h, 0, 1, 0, &windows.Overlapped{}); uerr != nil {
				// 释放失败不致命：进程退出/句柄关闭时系统兜底释放
				slog.Warn("serve 锁释放失败（进程退出亦会释放）", "err", uerr)
			}
		})
	}, nil
}
