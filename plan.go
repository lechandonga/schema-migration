package migrator

import (
	"fmt"
	"sort"
)

// PlanUp 根据当前已应用版本与目标构造顺序升级计划。
//
// 规则：
//   - 已应用的版本不会重复执行；
//   - 未应用版本按语义化版本升序执行；
//   - target 为 Latest 或空时表示升到已注册的最高版本；
//   - target 低于当前版本、或目标无定义时返回错误（回滚请用 Rollback/PlanDown）。
func PlanUp(applied []Version, migrations map[Version]*Migration, target Version) (Plan, error) {
	cur := highestVersion(applied)
	if target == "" || target == Latest {
		target = highestRegistered(migrations)
	}
	if _, ok := migrations[target]; !ok {
		return Plan{}, fmt.Errorf("%w: target %q", ErrUnknownVersion, target)
	}
	if cur != "" && CompareVersions(cur, target) > 0 {
		return Plan{}, fmt.Errorf("target %q is below current %q; use Rollback", target, cur)
	}
	var steps []PlannedMigration
	// 只挑选 (cur, target] 区间内、未应用的版本，保证顺序执行且不重复。
	vs := make([]Version, 0, len(migrations))
	for v := range migrations {
		if containsVersion(applied, v) {
			continue
		}
		if (cur == "" || CompareVersions(v, cur) > 0) && CompareVersions(v, target) <= 0 {
			vs = append(vs, v)
		}
	}
	sort.Slice(vs, func(i, j int) bool { return CompareVersions(vs[i], vs[j]) < 0 })
	for _, v := range vs {
		steps = append(steps, PlannedMigration{Migration: migrations[v], Direction: Up})
	}
	return Plan{Current: cur, Target: target, Steps: steps}, nil
}

// PlanDown 根据当前已应用版本与目标构造逆序回滚计划。
// 回滚后 target 仍保持已应用；target 为空表示回滚到空库（全部撤销）。
// 回滚严格按应用的逆序进行。
func PlanDown(applied []Version, migrations map[Version]*Migration, target Version) (Plan, error) {
	if target != "" && target != Latest {
		if _, ok := migrations[target]; !ok {
			return Plan{}, fmt.Errorf("%w: target %q", ErrUnknownVersion, target)
		}
	}
	cur := highestVersion(applied)
	if target != "" && cur != "" && CompareVersions(target, cur) > 0 {
		return Plan{}, fmt.Errorf("rollback target %q is above current %q", target, cur)
	}
	// 以应用顺序为权威，取需要撤销的后缀；未知定义立即报错（无法安全回滚）。
	var steps []PlannedMigration
	for i := len(applied) - 1; i >= 0; i-- {
		v := applied[i]
		if target != "" && CompareVersions(v, target) <= 0 {
			break // target 及其以下保留
		}
		m, ok := migrations[v]
		if !ok {
			return Plan{}, fmt.Errorf("%w: %q", ErrMissingMigration, v)
		}
		steps = append(steps, PlannedMigration{Migration: m, Direction: Down})
	}
	return Plan{Current: cur, Target: target, Steps: steps}, nil
}

// highestVersion 返回按语义化版本比较的最大版本；空切片返回空版本。
func highestVersion(vs []Version) Version {
	var hi Version
	for _, v := range vs {
		if hi == "" || CompareVersions(v, hi) > 0 {
			hi = v
		}
	}
	return hi
}

// highestRegistered 返回已注册迁移中的最高版本。
func highestRegistered(ms map[Version]*Migration) Version {
	vs := make([]Version, 0, len(ms))
	for v := range ms {
		vs = append(vs, v)
	}
	return highestVersion(vs)
}
