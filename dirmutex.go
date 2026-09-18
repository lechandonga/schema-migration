package migrator

import "sync"

// dirMutex 是同一进程内针对某个数据目录的互斥量。
// 多个“实例”（goroutine/引擎对象）打开同一目录时，借助它串行化
// 状态文件与租约文件的读改写，配合原子 rename 保证一致性。
type dirMutex struct{ mu sync.Mutex }

func (d *dirMutex) Lock()   { d.mu.Lock() }
func (d *dirMutex) Unlock() { d.mu.Unlock() }

var (
	dirMutexesMu sync.Mutex
	dirMutexes   = map[string]*dirMutex{}
)

func getDirMutex(dir string) *dirMutex {
	dirMutexesMu.Lock()
	defer dirMutexesMu.Unlock()
	d, ok := dirMutexes[dir]
	if !ok {
		d = &dirMutex{}
		dirMutexes[dir] = d
	}
	return d
}
