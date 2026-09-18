//go:build !windows

package migrator

import (
	"os"
	"syscall"
)

// flockFile 打开一个用于跨进程建议锁的文件。
func flockDir(dir string) (*os.File, error) {
	return os.Open(dir)
}

// tryFLock 尝试对目录加排他建议锁（非阻塞）。
func tryFLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if err == syscall.EWOULDBLOCK {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// fUnlock 释放目录上的建议锁。
func fUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
