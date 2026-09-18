package migrator

// Version 是迁移版本号，采用可比较的语义化版本字符串（如 "1.2.0"）。
type Version string

// Direction 表示迁移方向。
type Direction int

const (
	// Up 表示升级（应用 Up 变更）。
	Up Direction = iota
	// Down 表示回滚（应用 Down 变更）。
	Down
)

// ChangeKind 标识一种模式变更类型。
type ChangeKind string

// 支持的变更类型。
const (
	CreateTable  ChangeKind = "create_table"
	DropTable    ChangeKind = "drop_table"
	AddColumn    ChangeKind = "add_column"
	DropColumn   ChangeKind = "drop_column"
	AlterColumn  ChangeKind = "alter_column"
	RenameColumn ChangeKind = "rename_column"
	SetNullable  ChangeKind = "set_nullable" // true 表示放宽为可空，false 表示加非空
	AddIndex     ChangeKind = "add_index"
	NoOp         ChangeKind = "noop"
)

// Change 描述一次原子的模式变更。
type Change struct {
	Kind   ChangeKind `json:"kind"`
	Table  string     `json:"table"`
	Column string     `json:"column,omitempty"`
	// RenameTo 仅 RenameColumn 使用。
	RenameTo string `json:"rename_to,omitempty"`
	// 列定义相关字段。
	Type       string `json:"type,omitempty"`
	Nullable   bool   `json:"nullable,omitempty"`
	HasDefault bool   `json:"has_default,omitempty"`
	Default    any    `json:"default,omitempty"`
	// Columns 仅 CreateTable 使用，给出初始列定义。
	Columns []Column `json:"columns,omitempty"`
	// Index 仅 AddIndex 使用。
	Index string `json:"index,omitempty"`
}

// Migration 描述一个版本的完整迁移，包含升级与回滚定义。
type Migration struct {
	Version     Version  `json:"version"`
	Description string   `json:"description,omitempty"`
	Up          []Change `json:"up"`
	Down        []Change `json:"down"`
}

// PlannedMigration 是执行计划中的一个步骤。
type PlannedMigration struct {
	Migration *Migration `json:"-"`
	Direction Direction  `json:"direction"`
}

// Plan 是按版本顺序排列的迁移执行计划。
type Plan struct {
	Current Version            `json:"current"`
	Target  Version            `json:"target"`
	Steps   []PlannedMigration `json:"steps"`
}
