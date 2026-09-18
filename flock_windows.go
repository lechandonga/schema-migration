package migrator

import "os"

// Windows 环境退化为仅进程内互斥（dirMutex），不提供跨进程 flock。
func flockDir(dir string) (*os.File, error) { return nil, nil }
func tryFLock(f *os.File) (bool, error)     { return true, nil }
func fUnlock(f *os.File) error              { return nil }
