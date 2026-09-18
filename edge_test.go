package migrator

import (
	"context"
	"testing"
)

func TestReRunSkipsAppliedDestructiveVersion(t *testing.T) {
	store, _ := OpenStore(t.TempDir())
	ms := append(testMigrations(), Migration{
		Version: "3.0.0",
		Up:      []Change{{Kind: DropColumn, Table: "users", Column: "email", Type: "string"}},
		Down:    []Change{{Kind: AddColumn, Table: "users", Column: "email", Type: "string", Nullable: true}},
	})
	eng := NewEngine(store, ms, "A", fastCfg())
	// 分阶段把 3.0.0 上线。
	if _, _, err := eng.MigrateStaged(context.Background(), "3.0.0"); err != nil {
		t.Fatal(err)
	}
	// 此时再以 Latest 调用普通 Migrate：3.0.0 的阶段版本已应用，
	// 计划中不再包含破坏性步骤，应返回 no-op 而非拒绝。
	res, err := eng.Migrate(context.Background(), Latest)
	if err != nil {
		t.Fatalf("rerun after staged apply must not reject: %v", err)
	}
	if res.Status != StatusNoop {
		t.Fatalf("want noop, got %+v", res)
	}
}

func TestUnknownTargetVersion(t *testing.T) {
	eng, _ := newTestEngine(t, "A", fastCfg())
	if _, err := eng.Migrate(context.Background(), "9.9.9"); err == nil {
		t.Fatal("unknown target should error")
	}
}

func TestPlanUpDoesNotRepeatApplied(t *testing.T) {
	ms := testMigrations()
	mp := map[Version]*Migration{}
	for i := range ms {
		mp[ms[i].Version] = &ms[i]
	}
	applied := []Version{"1.0.0"}
	plan, err := PlanUp(applied, mp, Latest)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Migration.Version != "2.0.0" {
		t.Fatalf("plan should only contain unapplied 2.0.0: %+v", plan.Steps)
	}
}

func TestSemVerOrdering(t *testing.T) {
	cases := []struct {
		a, b Version
		want int
	}{
		{"1.0.0", "1.0.1", -1},
		{"2.0.0", "1.9.9", 1},
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0", "1.0.0", 0},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("Compare(%s,%s)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}
