// Package migrate 提供支持在线滚动迁移、兼容性校验与多实例编排的
// 数据库模式迁移引擎。
package migrate

// Direction 表示迁移方向。
type Direction int

const (
	// Up 升级方向。
	Up Direction = iota
	// Down 回滚方向。
	Down
)

func (d Direction) String() string {
	if d == Down {
		return "down"
	}
	return "up"
}

// Migration 表示一个版本的迁移，包含升级与回滚定义。
type Migration struct {
	Version uint
	Name    string
	// Up 为升级时按顺序执行的 SQL 语句。
	Up []string
	// Down 为回滚时按顺序执行的 SQL 语句。
	Down []string
}

// AppliedRecord 记录一个已应用迁移的状态。
type AppliedRecord struct {
	Version   uint
	Name      string
	Direction Direction
	// State 为 "applied" 或 "failed"。
	State     string
	AppliedAt int64
}
