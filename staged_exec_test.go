package migrator

import (
	"context"
	"strings"
	"testing"
)

func TestMigrateStagedDropColumn(t *testing.T) {
	store, _ := OpenStore(t.TempDir())
	ms := []Migration{
		{
			Version: "1.0.0",
			Up: []Change{
				{Kind: CreateTable, Table: "users", Columns: []Column{{Name: "id", Type: "int", Nullable: false, HasDefault: true, Default: 0}}},
				{Kind: AddColumn, Table: "users", Column: "email", Type: "string", Nullable: true},
			},
			Down: []Change{{Kind: DropTable, Table: "users"}},
		},
		{
			Version: "2.0.0",
			Up:      []Change{{Kind: DropColumn, Table: "users", Column: "email", Type: "string"}},
			Down: []Change{
				{Kind: AddColumn, Table: "users", Column: "email", Type: "string", Nullable: true},
			},
		},
	}
	eng := NewEngine(store, ms, "A", fastCfg())
	if _, err := eng.Migrate(context.Background(), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	// 普通迁移被拒绝。
	if _, err := eng.Migrate(context.Background(), "2.0.0"); err == nil {
		t.Fatal("direct destructive migrate must be rejected")
	}
	// 分阶段执行成功。
	res, staged, err := eng.MigrateStaged(context.Background(), "2.0.0")
	if err != nil {
		t.Fatalf("staged migrate: %v", err)
	}
	if len(staged) != 1 || len(staged[0].Phases) != 2 {
		t.Fatalf("bad staged: %+v", staged)
	}
	if len(res.Applied) != 2 {
		t.Fatalf("should apply expand+contract phases, got %+v", res.Applied)
	}
	// expand 为 NoOp（email 还在），contract 后 email 被删除。
	tbl := store.CurrentSchema().Tables[0]
	for _, c := range tbl.Columns {
		if c.Name == "email" {
			t.Fatal("email should be dropped after contract phase")
		}
	}
	// 阶段版本已登记；重复执行 staged 应为 no-op（计划中无未应用版本）。
	applied := store.AppliedVersions()
	foundExpand, foundContract := false, false
	for _, v := range applied {
		if strings.Contains(string(v), "expand") {
			foundExpand = true
		}
		if strings.Contains(string(v), "contract") {
			foundContract = true
		}
	}
	if !foundExpand || !foundContract {
		t.Fatalf("phase versions not recorded: %v", applied)
	}
}
