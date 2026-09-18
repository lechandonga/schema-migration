package migrator

import (
	"context"
	"errors"
	"testing"
)

func TestOrphanJournalMismatchRejected(t *testing.T) {
	store, _ := OpenStore(t.TempDir())
	eng := NewEngine(store, threeStepMigrations(), "A", fastCfg())
	// 手工写入一个属于 1.0.0 up 的未完成日志，但当前没有任何已应用版本。
	if err := store.SaveJournal(&StepJournal{
		Version: "9.9.9", Direction: Up, Applied: []int{0}, Total: 2,
	}); err != nil {
		t.Fatal(err)
	}
	// 9.9.9 缺少迁移定义 => recoverPending 应报 ErrMissingMigration，而不是覆盖日志。
	_, err := eng.Migrate(context.Background(), Latest)
	if !errors.Is(err, ErrMissingMigration) {
		t.Fatalf("want ErrMissingMigration for unknown journal version, got %v", err)
	}
	// 日志必须保留，便于人工/带定义的实例介入。
	j, _ := store.LoadJournal()
	if j == nil || j.Version != "9.9.9" {
		t.Fatalf("journal must be preserved, got %+v", j)
	}
}
