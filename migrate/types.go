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

// Outcome 表示一次迁移调用的结果，仅在返回的 error 为 nil 时有意义。
type Outcome int

const (
	// OutcomeExecuted 本次调用由当前实例取得执行权并执行完成
	// （包括没有待执行迁移的空跑）。
	OutcomeExecuted Outcome = iota
	// OutcomeYielded 执行权已被其他实例持有，本次调用按 ContentionYield
	// 策略立即让出，未执行任何迁移；由调用方决定后续怎么做。
	OutcomeYielded
)

func (o Outcome) String() string {
	if o == OutcomeYielded {
		return "yielded"
	}
	return "executed"
}
