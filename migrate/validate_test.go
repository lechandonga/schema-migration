package migrate

import (
	"errors"
	"testing"
)

func TestValidatePlanDetectsDestructiveChanges(t *testing.T) {
	cases := []struct {
		name string
		stmt string
		kind ViolationKind
	}{
		{"drop column", "ALTER TABLE users DROP COLUMN age", ViolationDropColumn},
		{"drop table", "DROP TABLE legacy_orders", ViolationDropTable},
		{"alter type", "ALTER TABLE users ALTER COLUMN score TYPE BIGINT", ViolationAlterType},
		{"rename column", "ALTER TABLE users RENAME COLUMN name TO full_name", ViolationRenameColumn},
		{"add not null without default", "ALTER TABLE users ADD COLUMN email TEXT NOT NULL", ViolationAddNotNull},
		{"set not null", "ALTER TABLE users ALTER COLUMN email SET NOT NULL", ViolationAddNotNull},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := ValidatePlan([]Migration{{Version: 1, Name: "m", Up: []string{tc.stmt}}})
			if plan.Compatible() {
				t.Fatalf("期望识别破坏性变更: %s", tc.stmt)
			}
			if plan.Violations[0].Kind != tc.kind {
				t.Fatalf("违规类别不符: got %s want %s", plan.Violations[0].Kind, tc.kind)
			}
			if len(plan.Violations[0].StagedPath) == 0 {
				t.Fatal("应提供分阶段安全上线路径")
			}
		})
	}
}

func TestValidatePlanAllowsCompatibleChanges(t *testing.T) {
	plan := ValidatePlan([]Migration{{Version: 1, Name: "ok", Up: []string{
		"CREATE TABLE users (id INTEGER PRIMARY KEY)",
		"ALTER TABLE users ADD COLUMN email TEXT",
		"ALTER TABLE users ADD COLUMN age INTEGER NOT NULL DEFAULT 0",
		"CREATE INDEX idx_users_email ON users(email)",
	}}})
	if !plan.Compatible() {
		t.Fatalf("兼容性变更被误判: %+v", plan.Violations)
	}
}

func TestMigrateRejectsDestructivePlan(t *testing.T) {
	db := openTestDB(t)
	e := New(db, DefaultConfig(), "instance-1")
	err := e.Migrate(t.Context(), []Migration{
		{Version: 1, Name: "drop", Up: []string{"ALTER TABLE t DROP COLUMN c"}, Down: []string{"ALTER TABLE t ADD COLUMN c TEXT"}},
	})
	var ce *CompatibilityError
	if !errors.As(err, &ce) {
		t.Fatalf("期望 CompatibilityError, got %v", err)
	}
	if len(ce.Violations) == 0 || ce.Violations[0].Kind != ViolationDropColumn {
		t.Fatalf("拒绝原因不可区分: %+v", ce.Violations)
	}
}

// TestMigrateIgnoresAppliedDestructiveHistory 历史里已应用的破坏性版本
// 不应挡住后续只含兼容变更的迁移；兼容性判断只覆盖本次真正待执行的版本。
func TestMigrateIgnoresAppliedDestructiveHistory(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	destructive := Migration{Version: 1, Name: "create_then_drop",
		Up: []string{
			"CREATE TABLE t (id INTEGER PRIMARY KEY, c TEXT)",
			"ALTER TABLE t DROP COLUMN c",
		},
		Down: []string{"DROP TABLE t"}}

	// 收缩阶段：显式放行后执行破坏性版本。
	cfg := fastConfig()
	cfg.AllowDestructive = true
	if err := New(db, cfg, "i1").Migrate(ctx, []Migration{destructive}); err != nil {
		t.Fatalf("放行后破坏性迁移应成功: %v", err)
	}

	// 后续常规迭代：追加兼容版本，不再开启 AllowDestructive。
	compatible := Migration{Version: 2, Name: "add_email",
		Up:   []string{"ALTER TABLE t ADD COLUMN email TEXT"},
		Down: []string{"ALTER TABLE t DROP COLUMN email"}}
	e := New(db, fastConfig(), "i1")
	if err := e.Migrate(ctx, []Migration{destructive, compatible}); err != nil {
		t.Fatalf("历史破坏性版本不应挡住兼容迁移: %v", err)
	}
	applied, _ := e.Applied(ctx)
	if len(applied) != 2 {
		t.Fatalf("期望 2 个版本已应用, got %+v", applied)
	}
}

// TestMigrateStillRejectsPendingDestructive 待执行（未应用）的破坏性版本
// 在没有明确许可时仍必须被拒绝，不会被默默执行。
func TestMigrateStillRejectsPendingDestructive(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	e := New(db, fastConfig(), "i1")
	ok := Migration{Version: 1, Name: "create_t",
		Up:   []string{"CREATE TABLE t (id INTEGER PRIMARY KEY, c TEXT)"},
		Down: []string{"DROP TABLE t"}}
	if err := e.Migrate(ctx, []Migration{ok}); err != nil {
		t.Fatal(err)
	}
	pending := Migration{Version: 2, Name: "drop_c",
		Up:   []string{"ALTER TABLE t DROP COLUMN c"},
		Down: []string{"ALTER TABLE t ADD COLUMN c TEXT"}}
	err := e.Migrate(ctx, []Migration{ok, pending})
	var ce *CompatibilityError
	if !errors.As(err, &ce) {
		t.Fatalf("期望 CompatibilityError, got %v", err)
	}
	if ce.Violations[0].Kind != ViolationDropColumn {
		t.Fatalf("违规类别不符: %+v", ce.Violations)
	}
	// 被拒绝的计划不得产生任何执行效果。
	applied, _ := e.Applied(ctx)
	if len(applied) != 1 || applied[0].Version != 1 {
		t.Fatalf("被拒绝的迁移不应落库: %+v", applied)
	}
}
