package migrator

import (
	"fmt"
	"sort"
)

// RejectReason 标识兼容性校验拒绝直接执行的具体原因。
type RejectReason string

const (
	RejectDropColumn   RejectReason = "drop_column"
	RejectRenameColumn RejectReason = "rename_column"
	RejectAlterType    RejectReason = "alter_column_type"
	RejectAddNotNull   RejectReason = "add_not_null_constraint"
	RejectDropTable    RejectReason = "drop_table"
)

// Violation 描述一处会导致旧版本实例不可用的破坏性变更。
type Violation struct {
	Reason  RejectReason `json:"reason"`
	Version Version      `json:"version,omitempty"`
	Table   string       `json:"table"`
	Column  string       `json:"column,omitempty"`
	Message string       `json:"message"`
	// Staged 为 true 表示该变更可通过 expand/contract 分阶段安全上线；
	// 为 false 表示没有安全的在线路径（例如删表）。
	Staged bool `json:"staged"`
}

func (v Violation) Error() string {
	if v.Column != "" {
		return fmt.Sprintf("%s: %s.%s (%s)", v.Reason, v.Table, v.Column, v.Message)
	}
	return fmt.Sprintf("%s: %s (%s)", v.Reason, v.Table, v.Message)
}

// CompatibilityReport 是对一个计划的兼容性校验结果。
type CompatibilityReport struct {
	Compatible bool        `json:"compatible"`
	Violations []Violation `json:"violations"`
	// Staged 非空表示该计划可通过分阶段（expand/contract）安全上线。
	// 每个原始破坏性迁移对应一组按顺序执行的阶段迁移。
	Staged []StagedMigration `json:"staged,omitempty"`
}

// StagedMigration 是某个破坏性迁移对应的安全上线阶段序列。
type StagedMigration struct {
	// Source 是触发分阶段路径的原始版本。
	Source Version `json:"source"`
	// Phases 按顺序为：Expand（双写/兼容）、Cutover（切换非空默认）、Contract（清理）。
	Phases []Migration `json:"phases"`
	// PhaseKinds 与 Phases 一一对应，标识阶段类型，用于稳定排序。
	PhaseKinds []string `json:"phase_kinds"`
}

// CheckCompatibility 校验升级计划对仍在运行的旧版本实例是否兼容，
// 识别破坏性变更；对可分阶段完成的变更给出 expand/contract 阶段路径。
//
// 判定原则：滚动发布期间，旧版本实例仍会读写“升级后”的模式，
// 因此凡移除/重命名旧实例依赖的结构、收紧约束或改变数据类型的变更，
// 都判定为不兼容并拒绝一步执行。
func CheckCompatibility(plan Plan) CompatibilityReport {
	rep := CompatibilityReport{Compatible: true}
	for _, step := range plan.Steps {
		if step.Direction != Up || step.Migration == nil {
			continue
		}
		violations, staged := analyzeMigration(step.Migration)
		if len(violations) > 0 {
			rep.Compatible = false
			rep.Violations = append(rep.Violations, violations...)
			if staged.Phases != nil {
				rep.Staged = append(rep.Staged, staged)
			}
		}
	}
	return rep
}

func analyzeMigration(m *Migration) ([]Violation, StagedMigration) {
	var violations []Violation
	var phases []Migration
	var kinds []string
	add := func(v Violation, phase *Migration) {
		violations = append(violations, v)
		if phase != nil {
			phases = append(phases, *phase)
		}
	}
	emit := func(kind string, p Migration) {
		phases = append(phases, p)
		kinds = append(kinds, kind)
	}
	for _, ch := range m.Up {
		switch ch.Kind {
		case DropColumn:
			v := Violation{
				Reason: RejectDropColumn, Version: m.Version, Table: ch.Table, Column: ch.Column,
				Message: "旧版本实例仍会读写该列，直接删除会导致其失败",
				Staged:  true,
			}
			// expand 阶段无需改模式（先在应用层停止读写），contract 阶段才真正删列。
			expand := Migration{
				Version:     phaseVersion(m.Version, "expand"),
				Description: fmt.Sprintf("阶段1/2 expand：应用层停止使用 %s.%s（模式不变）", ch.Table, ch.Column),
				Up:          []Change{{Kind: NoOp}},
				Down:        []Change{{Kind: NoOp}},
			}
			contract := Migration{
				Version:     phaseVersion(m.Version, "contract"),
				Description: fmt.Sprintf("阶段2/2 contract：删除列 %s.%s", ch.Table, ch.Column),
				Up:          []Change{ch},
				Down: []Change{{
					Kind: AddColumn, Table: ch.Table, Column: ch.Column,
					Type: ch.Type, Nullable: true,
				}},
			}
			add(v, nil)
			emit("expand", expand)
			emit("contract", contract)
		case DropTable:
			add(Violation{
				Reason: RejectDropTable, Version: m.Version, Table: ch.Table,
				Message: "旧版本实例仍依赖该表，直接删表会导致其全部失败，无在线分阶段路径",
				Staged:  false,
			}, nil)
		case RenameColumn:
			v := Violation{
				Reason: RejectRenameColumn, Version: m.Version, Table: ch.Table, Column: ch.Column,
				Message: "旧版本实例按旧列名访问，重命名等同于删列+加列，会导致其失败",
				Staged:  true,
			}
			expand := Migration{
				Version:     phaseVersion(m.Version, "expand"),
				Description: fmt.Sprintf("阶段1/3 expand：新增影子列 %s.%s，与 %s 双写", ch.Table, ch.RenameTo, ch.Column),
				Up: []Change{{
					Kind: AddColumn, Table: ch.Table, Column: ch.RenameTo,
					Type: ch.Type, Nullable: true,
				}},
				Down: []Change{{Kind: DropColumn, Table: ch.Table, Column: ch.RenameTo}},
			}
			cutover := Migration{
				Version:     phaseVersion(m.Version, "cutover"),
				Description: fmt.Sprintf("阶段2/3 cutover：新版本切换到 %s 并回填数据", ch.RenameTo),
				Up:          []Change{{Kind: NoOp}}, // 数据回填由应用在切流时完成，模式不变
				Down:        []Change{{Kind: NoOp}},
			}
			contract := Migration{
				Version:     phaseVersion(m.Version, "contract"),
				Description: fmt.Sprintf("阶段3/3 contract：删除旧列 %s.%s", ch.Table, ch.Column),
				Up:          []Change{{Kind: DropColumn, Table: ch.Table, Column: ch.Column}},
				Down: []Change{{
					Kind: AddColumn, Table: ch.Table, Column: ch.Column,
					Type: ch.Type, Nullable: true,
				}},
			}
			add(v, nil)
			emit("expand", expand)
			emit("cutover", cutover)
			emit("contract", contract)
		case AlterColumn:
			v := Violation{
				Reason: RejectAlterType, Version: m.Version, Table: ch.Table, Column: ch.Column,
				Message: "直接修改列类型会使按旧类型读写的旧版本实例出错，应走影子列+双写+回填+切换",
				Staged:  true,
			}
			shadow := ch.Column + "_new"
			expand := Migration{
				Version:     phaseVersion(m.Version, "expand"),
				Description: fmt.Sprintf("阶段1/3 expand：新增类型为 %s 的影子列 %s.%s 并双写", ch.Type, ch.Table, shadow),
				Up: []Change{{
					Kind: AddColumn, Table: ch.Table, Column: shadow,
					Type: ch.Type, Nullable: true,
				}},
				Down: []Change{{Kind: DropColumn, Table: ch.Table, Column: shadow}},
			}
			cutover := Migration{
				Version:     phaseVersion(m.Version, "cutover"),
				Description: fmt.Sprintf("阶段2/3 cutover：回填 %s 并切换读写", shadow),
				Up:          []Change{{Kind: NoOp}},
				Down:        []Change{{Kind: NoOp}},
			}
			contract := Migration{
				Version:     phaseVersion(m.Version, "contract"),
				Description: fmt.Sprintf("阶段3/3 contract：以 %s 替换 %s", shadow, ch.Column),
				Up: []Change{
					{Kind: DropColumn, Table: ch.Table, Column: ch.Column},
					{Kind: RenameColumn, Table: ch.Table, Column: shadow, RenameTo: ch.Column},
				},
				Down: []Change{
					{Kind: RenameColumn, Table: ch.Table, Column: ch.Column, RenameTo: shadow},
					{Kind: AddColumn, Table: ch.Table, Column: ch.Column, Type: ch.Type, Nullable: true},
				},
			}
			add(v, nil)
			emit("expand", expand)
			emit("cutover", cutover)
			emit("contract", contract)
		case SetNullable:
			if !ch.Nullable && !ch.HasDefault {
				add(Violation{
					Reason: RejectAddNotNull, Version: m.Version, Table: ch.Table, Column: ch.Column,
					Message: "直接加 NOT NULL 且无默认值：旧实例写入空值/存量空行会失败",
					Staged:  true,
				}, nil)
				emit("expand", Migration{
					Version:     phaseVersion(m.Version, "expand"),
					Description: fmt.Sprintf("阶段1/2 expand：为 %s.%s 设置默认值并回填存量行", ch.Table, ch.Column),
					Up: []Change{{
						Kind: SetNullable, Table: ch.Table, Column: ch.Column,
						Nullable: true, HasDefault: true, Default: ch.Default,
					}},
					Down: []Change{{Kind: NoOp}},
				})
				emit("contract", Migration{
					Version:     phaseVersion(m.Version, "contract"),
					Description: "阶段2/2 contract：全部实例升级后再加 NOT NULL",
					Up:          []Change{ch},
					Down:        []Change{{Kind: SetNullable, Table: ch.Table, Column: ch.Column, Nullable: true}},
				})
			}
		}
	}
	staged := StagedMigration{Source: m.Version}
	if len(phases) > 0 {
		// 去重并保持阶段顺序（多个同类违规时合并）。
		staged.Phases, staged.PhaseKinds = dedupePhases(phases, kinds)
	}
	return violations, staged
}

// phaseOrder 定义阶段的标准上线顺序。
var phaseOrder = map[string]int{"expand": 0, "cutover": 1, "contract": 2}

func dedupePhases(phases []Migration, kinds []string) ([]Migration, []string) {
	seen := map[Version]int{}
	var out []Migration
	var outKinds []string
	for i, p := range phases {
		kind := kinds[i]
		if idx, ok := seen[p.Version]; ok {
			// 合并同名阶段的变更。
			out[idx].Up = append(out[idx].Up, p.Up...)
			out[idx].Down = append(out[idx].Down, p.Down...)
			continue
		}
		seen[p.Version] = len(out)
		cp := p
		cp.Up = append([]Change(nil), p.Up...)
		cp.Down = append([]Change(nil), p.Down...)
		out = append(out, cp)
		outKinds = append(outKinds, kind)
	}
	// 以阶段类型的标准顺序稳定排序。
	order := make([]int, len(out))
	for i := range order {
		order[i] = phaseOrder[outKinds[i]]
	}
	sort.SliceStable(out, func(i, j int) bool { return order[i] < order[j] })
	sort.SliceStable(outKinds, func(i, j int) bool { return order[i] < order[j] })
	return out, outKinds
}

// phaseVersion 生成确定性的阶段版本号，形如 "2.0.0+expand"。
func phaseVersion(v Version, phase string) Version {
	return Version(fmt.Sprintf("%s+%s", v, phase))
}

// BuildStagedMigrations 针对单个包含破坏性变更的迁移，构造可安全上线的
// 分阶段迁移序列（先 expand，灰度切换后再 contract）。
func BuildStagedMigrations(m Migration) []StagedMigration {
	violations, staged := analyzeMigration(&m)
	_ = violations
	if staged.Phases == nil {
		return nil
	}
	return []StagedMigration{staged}
}
