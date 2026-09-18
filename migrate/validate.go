package migrate

import (
	"fmt"
	"regexp"
	"strings"
)

// ViolationKind 表示破坏性变更类别。
type ViolationKind string

const (
	// ViolationDropColumn 删除列：旧版本实例仍可能读写该列。
	ViolationDropColumn ViolationKind = "drop_column"
	// ViolationDropTable 删除表：旧版本实例仍可能访问该表。
	ViolationDropTable ViolationKind = "drop_table"
	// ViolationAlterType 修改列类型：旧版本实例按旧类型读写可能失败。
	ViolationAlterType ViolationKind = "alter_column_type"
	// ViolationAddNotNull 新增非空约束（无默认值）：旧版本实例插入时
	// 不提供该列将直接报错。
	ViolationAddNotNull ViolationKind = "add_not_null"
	// ViolationRenameColumn 重命名列：旧版本实例仍按旧名访问。
	ViolationRenameColumn ViolationKind = "rename_column"
)

// Violation 描述一处破坏性变更及其分阶段安全上线路径。
type Violation struct {
	Kind      ViolationKind
	Statement string
	Reason    string
	// StagedPath 为可分阶段安全上线的替代执行路径（按阶段排序）。
	StagedPath []string
}

// ViolationError 是单条违规的可区分错误原因，实现 error 接口。
func (v Violation) Error() string {
	return fmt.Sprintf("%s: %s", v.Kind, v.Reason)
}

// Plan 为对一组迁移的校验与执行计划。
type Plan struct {
	Migrations []Migration
	Violations []Violation
}

// Compatible 报告计划是否不含破坏性变更。
func (p *Plan) Compatible() bool { return len(p.Violations) == 0 }

var (
	reDropTable  = regexp.MustCompile(`(?i)\bDROP\s+TABLE\b`)
	reDropColumn = regexp.MustCompile(`(?i)\bALTER\s+TABLE\s+\S+\s+DROP\s+COLUMN\s+(\S+)`)
	reAlterType  = regexp.MustCompile(`(?i)\bALTER\s+TABLE\s+\S+\s+ALTER\s+(?:COLUMN\s+)?(\S+)\s+(?:SET\s+DATA\s+)?TYPE\b`)
	reRenameCol  = regexp.MustCompile(`(?i)\bALTER\s+TABLE\s+\S+\s+RENAME\s+COLUMN\s+(\S+)\s+TO\s+(\S+)`)
	reAddColumn  = regexp.MustCompile(`(?i)\bALTER\s+TABLE\s+(\S+)\s+ADD\s+COLUMN\s+(\S+)\s+(.+)`)
	reNotNull    = regexp.MustCompile(`(?i)\bNOT\s+NULL\b`)
	reHasDefault = regexp.MustCompile(`(?i)\bDEFAULT\b`)
	reSetNotNull = regexp.MustCompile(`(?i)\bALTER\s+(?:COLUMN\s+)?\S+\s+SET\s+NOT\s+NULL\b`)
)

// ValidatePlan 校验一组迁移的兼容性，识别破坏性变更并为每处变更
// 给出可分阶段安全上线的执行路径。
func ValidatePlan(migrations []Migration) *Plan {
	p := &Plan{Migrations: migrations}
	for _, m := range migrations {
		for _, stmt := range m.Up {
			p.Violations = append(p.Violations, checkStatement(m.Version, stmt)...)
		}
	}
	return p
}

func checkStatement(version uint, stmt string) []Violation {
	s := strings.TrimSpace(stmt)
	var out []Violation

	if reDropTable.MatchString(s) {
		out = append(out, Violation{
			Kind:      ViolationDropTable,
			Statement: s,
			Reason:    "删除表会使仍在运行的旧版本实例访问该表时报错",
			StagedPath: []string{
				"阶段1：发布不再读写该表的应用版本，等待全部实例完成滚动升级",
				"阶段2：在独立迁移中执行 DROP TABLE",
			},
		})
		return out
	}
	if m := reDropColumn.FindStringSubmatch(s); m != nil {
		col := m[1]
		out = append(out, Violation{
			Kind:      ViolationDropColumn,
			Statement: s,
			Reason:    fmt.Sprintf("删除列 %s 会使仍在运行的旧版本实例读写该列时报错", col),
			StagedPath: []string{
				fmt.Sprintf("阶段1：发布停止读写列 %s 的应用版本，等待全部实例完成滚动升级", col),
				fmt.Sprintf("阶段2：在独立迁移中执行 ALTER TABLE ... DROP COLUMN %s", col),
			},
		})
		return out
	}
	if m := reAlterType.FindStringSubmatch(s); m != nil {
		col := m[1]
		out = append(out, Violation{
			Kind:      ViolationAlterType,
			Statement: s,
			Reason:    fmt.Sprintf("修改列 %s 的类型会使按旧类型读写的旧版本实例出错", col),
			StagedPath: []string{
				fmt.Sprintf("阶段1：新增目标类型列 %s_new，并双写新旧两列", col),
				fmt.Sprintf("阶段2：回填历史数据到 %s_new 并校验一致性", col),
				fmt.Sprintf("阶段3：发布只读写 %s_new 的应用版本", col),
				fmt.Sprintf("阶段4：在独立迁移中删除旧列并将 %s_new 重命名为 %s", col, col),
			},
		})
		return out
	}
	if m := reRenameCol.FindStringSubmatch(s); m != nil {
		out = append(out, Violation{
			Kind:      ViolationRenameColumn,
			Statement: s,
			Reason:    fmt.Sprintf("重命名列 %s 为 %s 会使按旧名访问的旧版本实例报错", m[1], m[2]),
			StagedPath: []string{
				fmt.Sprintf("阶段1：新增列 %s 并双写 %s 与 %s", m[2], m[1], m[2]),
				"阶段2：回填历史数据并发布只读新列的应用版本",
				fmt.Sprintf("阶段3：在独立迁移中删除旧列 %s", m[1]),
			},
		})
		return out
	}
	if reSetNotNull.MatchString(s) {
		out = append(out, Violation{
			Kind:      ViolationAddNotNull,
			Statement: s,
			Reason:    "对已有列直接 SET NOT NULL 会在存在 NULL 数据时失败，且旧版本实例可能写入 NULL",
			StagedPath: []string{
				"阶段1：回填存量 NULL 数据",
				"阶段2：发布始终写入非空值的应用版本",
				"阶段3：在独立迁移中执行 SET NOT NULL",
			},
		})
		return out
	}
	if m := reAddColumn.FindStringSubmatch(s); m != nil {
		table, col, rest := m[1], m[2], m[3]
		if reNotNull.MatchString(rest) && !reHasDefault.MatchString(rest) {
			out = append(out, Violation{
				Kind:      ViolationAddNotNull,
				Statement: s,
				Reason:    fmt.Sprintf("向表 %s 新增无默认值的非空列 %s，旧版本实例插入时不提供该列将报错", table, col),
				StagedPath: []string{
					fmt.Sprintf("阶段1：以可空或带默认值方式新增列 %s", col),
					"阶段2：回填历史数据，并发布始终写入该列的应用版本",
					fmt.Sprintf("阶段3：在独立迁移中为列 %s 添加 NOT NULL 约束", col),
				},
			})
		}
	}
	return out
}
