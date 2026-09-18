package migrate

import "errors"

var (
	// ErrLeaseBusy 表示在超时时间内未能获取迁移执行权。
	ErrLeaseBusy = errors.New("migrate: 未能在超时前获取迁移租约")
	// ErrNoMigration 表示目标版本无可执行迁移。
	ErrNoMigration = errors.New("migrate: 没有需要执行的迁移")
)

// CompatibilityError 表示迁移计划未通过兼容性校验，Reason 可区分具体原因。
type CompatibilityError struct {
	Violations []Violation
}

func (e *CompatibilityError) Error() string { return "migrate: 迁移计划包含破坏性变更" }
