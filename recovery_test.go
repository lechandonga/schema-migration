package migrator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// threeStepMigrations 返回 1.0.0 含 3 个变更、2.0.0 含 1 个变更的迁移集。
func threeStepMigrations() []Migration {
	return []Migration{
		{
			Version: "1.0.0",
			Up: []Change{
				{Kind: CreateTable, Table: "t", Columns: []Column{{Name: "id", Type: "int", Nullable: true}}},
				{Kind: AddColumn, Table: "t", Column: "a", Type: "string", Nullable: true},
				{Kind: AddColumn, Table: "t", Column: "b", Type: "string", Nullable: true},
			},
			Down: []Change{
				{Kind: DropColumn, Table: "t", Column: "b"},
				{Kind: DropColumn, Table: "t", Column: "a"},
				{Kind: DropTable, Table: "t"},
			},
		},
		{
			Version: "2.0.0",
			Up:      []Change{{Kind: AddColumn, Table: "t", Column: "c", Type: "string", Nullable: true}},
			Down:    []Change{{Kind: DropColumn, Table: "t", Column: "c"}},
		},
	}
}

func TestResumeAfterCrash(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 第 3 个变更（b 列，下标 2）第一次执行时“崩溃”。
	var attempts int32
	var failAt int32 = 1
	store.Fault = func(ch Change, attempt int) error {
		if ch.Kind == AddColumn && ch.Column == "b" {
			if atomic.AddInt32(&attempts, 1) == failAt {
				return errors.New("simulated crash before commit")
			}
		}
		return nil
	}
	eng := NewEngine(store, threeStepMigrations(), "A", fastCfg())
	_, err = eng.Migrate(context.Background(), Latest)
	if err == nil {
		t.Fatal("first run should fail")
	}

	// 崩溃现场：1.0.0 未登记；日志记录已提交前缀 [0,1]；表 t 与列 a 存在，列 b 不存在。
	if store.IsApplied("1.0.0") || store.IsApplied("2.0.0") {
		t.Fatal("version must not be marked applied on crash")
	}
	j, err := store.LoadJournal()
	if err != nil || j == nil {
		t.Fatalf("journal must survive crash: %+v %v", j, err)
	}
	if len(j.Applied) != 2 || j.Applied[0] != 0 || j.Applied[1] != 1 {
		t.Fatalf("journal prefix wrong: %+v", j.Applied)
	}
	sc0 := store.CurrentSchema()
	tbl := findTable(&sc0, "t")
	if tbl == nil || findColumn(tbl, "a") == nil || findColumn(tbl, "b") != nil {
		t.Fatalf("schema at crash wrong: %+v", store.CurrentSchema().Tables)
	}

	// 模拟实例重启：重新打开引擎（故障解除），已提交的两个变更不得重复应用。
	store2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 若错误地重复应用 create table，会报 already exists。
	eng2 := NewEngine(store2, threeStepMigrations(), "A", fastCfg())
	res, err := eng2.Migrate(context.Background(), Latest)
	if err != nil {
		t.Fatalf("resume should succeed: %v", err)
	}
	if !res.Recovered {
		t.Fatal("resume result should indicate recovery")
	}
	if got := store2.AppliedVersions(); len(got) != 2 || got[0] != "1.0.0" || got[1] != "2.0.0" {
		t.Fatalf("applied after resume wrong: %v", got)
	}
	sc1 := store2.CurrentSchema()
	tbl2 := findTable(&sc1, "t")
	if tbl2 == nil || len(tbl2.Columns) != 4 { // id,a,b,c
		t.Fatalf("columns after resume wrong: %+v", tbl2)
	}
	if j2, _ := store2.LoadJournal(); j2 != nil {
		t.Fatal("journal should be cleared after success")
	}
}

func TestResumeIdempotentChangeTracking(t *testing.T) {
	// 多次失败后成功：已提交变更只执行一次，未提交变更按重试/恢复执行。
	dir := t.TempDir()
	store, _ := OpenStore(dir)
	var bSeen int32
	store.Fault = func(ch Change, attempt int) error {
		if ch.Column == "b" {
			atomic.AddInt32(&bSeen, 1)
		}
		return nil
	}
	eng := NewEngine(store, threeStepMigrations(), "A", fastCfg())
	if _, err := eng.Migrate(context.Background(), Latest); err != nil {
		t.Fatal(err)
	}
	if bSeen != 1 {
		t.Fatalf("column b change should run exactly once, ran %d", bSeen)
	}
}
