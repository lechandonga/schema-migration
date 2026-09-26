package migrate

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrLeaseBusy 表示在超时时间内未能获取迁移执行权。
	ErrLeaseBusy = errors.New("migrate: 未能在超时前获取迁移租约")
	// ErrNoMigration 表示目标版本无可执行迁移。
	ErrNoMigration = errors.New("migrate: 没有需要执行的迁移")
	// errYielded 为内部哨兵：按 ContentionYield 策略让出本次执行，
	// 在公开 API 层被转换为 OutcomeYielded，不作为错误暴露。
	errYielded = errors.New("migrate: 执行权已被其他实例持有，本次让出")
)

// CompatibilityError 表示迁移计划未通过兼容性校验，Violations 中每条
// 记录的 Kind 可区分具体是哪一类破坏性变更被拦下。
type CompatibilityError struct {
	Violations []Violation
}

func (e *CompatibilityError) Error() string {
	kinds := make([]string, 0, len(e.Kinds()))
	for _, k := range e.Kinds() {
		kinds = append(kinds, string(k))
	}
	return fmt.Sprintf("migrate: 迁移计划包含破坏性变更 (%s)", strings.Join(kinds, ", "))
}

// Kinds 返回本次被拦下的破坏性变更类别（去重、按出现顺序）。
func (e *CompatibilityError) Kinds() []ViolationKind {
	seen := map[ViolationKind]bool{}
	var out []ViolationKind
	for _, v := range e.Violations {
		if !seen[v.Kind] {
			seen[v.Kind] = true
			out = append(out, v.Kind)
		}
	}
	return out
}

// HasKind 报告是否因指定类别的破坏性变更被拒绝。
func (e *CompatibilityError) HasKind(k ViolationKind) bool {
	for _, v := range e.Violations {
		if v.Kind == k {
			return true
		}
	}
	return false
}
