package migrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func testMigrations() []Migration {
	return []Migration{
		{
			Version: "1.0.0",
			Up: []Change{
				{Kind: CreateTable, Table: "users", Columns: []Column{
					{Name: "id", Type: "int", Nullable: false, HasDefault: true, Default: 0},
				}},
				{Kind: AddColumn, Table: "users", Column: "email", Type: "string", Nullable: true},
			},
			Down: []Change{
				{Kind: DropColumn, Table: "users", Column: "email"},
				{Kind: DropTable, Table: "users"},
			},
		},
		{
			Version: "2.0.0",
			Up: []Change{
				{Kind: CreateTable, Table: "orders", Columns: []Column{
					{Name: "id", Type: "int", Nullable: false, HasDefault: true, Default: 0},
					{Name: "uid", Type: "int", Nullable: false, HasDefault: true, Default: 0},
				}},
			},
			Down: []Change{{Kind: DropTable, Table: "orders"}},
		},
	}
}

func newTestEngine(t *testing.T, owner string, cfg Config) (*Engine, *Store) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewEngine(store, testMigrations(), owner, cfg), store
}

func TestMigrateHappyPath(t *testing.T) {
	eng, store := newTestEngine(t, "A", fastCfg())
	ctx := context.Background()
	res, err := eng.Migrate(ctx, Latest)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusApplied || len(res.Applied) != 2 {
		t.Fatalf("bad result: %+v", res)
	}
	if got := store.AppliedVersions(); len(got) != 2 || got[0] != "1.0.0" || got[1] != "2.0.0" {
		t.Fatalf("applied order wrong: %v", got)
	}
	sc := store.CurrentSchema()
	if len(sc.Tables) != 2 {
		t.Fatalf("want 2 tables, got %d", len(sc.Tables))
	}
	// rerun is no-op, no duplicate execution.
	res2, err := eng.Migrate(ctx, Latest)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != StatusNoop || len(res2.Applied) != 0 {
		t.Fatalf("rerun should be noop, got %+v", res2)
	}
}

func TestMigrateToSpecificVersion(t *testing.T) {
	eng, store := newTestEngine(t, "A", fastCfg())
	res, err := eng.Migrate(context.Background(), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 1 || !store.IsApplied("1.0.0") || store.IsApplied("2.0.0") {
		t.Fatalf("only 1.0.0 should be applied: %+v", res)
	}
}

func TestRollbackReverseOrder(t *testing.T) {
	eng, store := newTestEngine(t, "A", fastCfg())
	ctx := context.Background()
	if _, err := eng.Migrate(ctx, Latest); err != nil {
		t.Fatal(err)
	}
	// rollback to 1.0.0 (2.0.0 undone, 1.0.0 kept)
	res, err := eng.Rollback(ctx, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 1 || res.Applied[0] != "2.0.0" {
		t.Fatalf("bad rollback result: %+v", res)
	}
	if !store.IsApplied("1.0.0") || store.IsApplied("2.0.0") {
		t.Fatalf("applied set after rollback wrong: %v", store.AppliedVersions())
	}
	if len(store.CurrentSchema().Tables) != 1 || store.CurrentSchema().Tables[0].Name != "users" {
		t.Fatalf("schema after rollback wrong: %+v", store.CurrentSchema().Tables)
	}
	// rollback all the way to empty.
	if _, err := eng.Rollback(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if len(store.AppliedVersions()) != 0 || len(store.CurrentSchema().Tables) != 0 {
		t.Fatalf("expected empty schema, got %v / %+v", store.AppliedVersions(), store.CurrentSchema().Tables)
	}
}

func TestMigrateRejectsDestructive(t *testing.T) {
	store, _ := OpenStore(t.TempDir())
	ms := append(testMigrations(), Migration{
		Version: "3.0.0",
		Up:      []Change{{Kind: DropColumn, Table: "users", Column: "email", Type: "string"}},
		Down:    []Change{{Kind: AddColumn, Table: "users", Column: "email", Type: "string", Nullable: true}},
	})
	eng := NewEngine(store, ms, "A", fastCfg())
	// 1.0.0/2.0.0 apply fine.
	if _, err := eng.Migrate(context.Background(), "2.0.0"); err != nil {
		t.Fatal(err)
	}
	_, err := eng.Migrate(context.Background(), "3.0.0")
	var rj *RejectedPlanError
	if !errors.As(err, &rj) {
		t.Fatalf("want RejectedPlanError, got %v", err)
	}
	if len(rj.Violations) != 1 || rj.Violations[0].Reason != RejectDropColumn {
		t.Fatalf("bad violations: %+v", rj.Violations)
	}
	if len(rj.Staged) != 1 || len(rj.Staged[0].Phases) != 2 {
		t.Fatalf("bad staged path: %+v", rj.Staged)
	}
	// destructive migration must not have been applied.
	if store.IsApplied("3.0.0") {
		t.Fatal("rejected migration must not be applied")
	}
	if !errors.Is(err, ErrPlanRejected) {
		t.Fatalf("error must wrap ErrPlanRejected, got %v", err)
	}
}

var _ = fmt.Sprintf
