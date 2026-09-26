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
	// Stages 非空时，本版本按阶段执行：每个阶段独立事务提交并单独簿记，
	// 阶段之间可以发布新应用版本、观察或暂停；此时 Up/Down 被忽略。
	Stages []Stage
}

// Stage 表示一个版本内的单个执行阶段。
type Stage struct {
	Name string
	// Up 为前进时按顺序执行的 SQL 语句。
	Up []string
	// Down 为回退该阶段时按顺序执行的 SQL 语句。
	Down []string
}

// StageRecord 记录一个已完成阶段的簿记状态。
type StageRecord struct {
	Version uint
	Stage   int
	Name    string
	// State 为 "applied" 或 "failed"。
	State     string
	AppliedAt int64
}

// Result 表示一次迁移调用的编排结果，供上层区分不同 outcome。
type Result int

const (
	// ResultExecuted 表示本次调用由本实例取得执行权并执行完成。
	ResultExecuted Result = iota
	// ResultYielded 表示发现执行权已被其他实例持有，本次按策略让出，
	// 未执行任何迁移；属正常返回而非失败。
	ResultYielded
)

func (r Result) String() string {
	if r == ResultYielded {
		return "yielded"
	}
	return "executed"
}

// ContentionPolicy 表示发现执行权被其他实例持有时的编排策略。
type ContentionPolicy int

const (
	// ContentionWait 为默认策略：在 AcquireTimeout 内等待并重试，
	// 超时仍未取得执行权则返回 ErrLeaseBusy。
	ContentionWait ContentionPolicy = iota
	// ContentionYield 为让出策略：首次获取失败即正常返回 ResultYielded，
	// 由调用方决定后续动作（稍后再试或放弃本次）。
	ContentionYield
)

// AppliedRecord 记录一个已应用迁移的状态。
type AppliedRecord struct {
	Version   uint
	Name      string
	Direction Direction
	// State 为 "applied" 或 "failed"。
	State     string
	AppliedAt int64
}
