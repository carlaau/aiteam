//go:build unix

package cli

import (
	"errors"
	"log/slog"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// lockExclusive 对已打开的锁文件取 BSD flock 非阻塞独占锁（LOCK_EX|LOCK_NB）：
// 锁挂在 open file description 上，进程死即由内核释放，零陈旧态。同进程另一 fd
// 重复取锁同样被拒（flock 视不同 fd 为独立竞争者），进程内互斥测试即真锁实测。
func lockExclusive(f *os.File) (release func(), err error) {
	fd := int(f.Fd())
	// EINTR 小重试：信号打断的取锁失败与「被占」无关，不得误归被占类；共 3 次
	// 尝试，仍 EINTR 则按普通系统错误走原路径返回（启动失败如实上报，退 1）。
	var lerr error
	for i := 0; i < 3; i++ {
		lerr = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(lerr, unix.EINTR) {
			break
		}
	}
	if lerr != nil {
		// EAGAIN/EWOULDBLOCK 同值异名（部分平台两常量并立），双查防漏
		if errors.Is(lerr, unix.EAGAIN) || errors.Is(lerr, unix.EWOULDBLOCK) {
			return nil, errLockRegionHeld
		}
		return nil, lerr
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if uerr := unix.Flock(fd, unix.LOCK_UN); uerr != nil {
				// 释放失败不致命：进程退出/句柄关闭时内核兜底释放
				slog.Warn("serve 锁释放失败（进程退出亦会释放）", "err", uerr)
			}
		})
	}, nil
}
