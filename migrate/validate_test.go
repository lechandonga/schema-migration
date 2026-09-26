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
	_, err := e.Migrate(t.Context(), []Migration{
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
