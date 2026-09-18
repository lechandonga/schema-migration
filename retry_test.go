package migrator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryTransientFailure(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenStore(dir)
	cfg := fastCfg()
	cfg.Retry = RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}
	eng := NewEngine(store, threeStepMigrations(), "A", cfg)

	var calls int32
	eng.SetFaultHook(func(ch Change, attempt int) error {
		if ch.Kind == AddColumn && ch.Column == "a" {
			n := atomic.AddInt32(&calls, 1)
			if n < 3 {
				return errTransient // 前两次瞬时失败
			}
		}
		return nil
	})
	res, err := eng.Migrate(context.Background(), Latest)
	if err != nil {
		t.Fatalf("should succeed after retries: %v", err)
	}
	if len(res.Applied) != 2 {
		t.Fatalf("bad applied: %+v", res.Applied)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts for flaky change, got %d", calls)
	}
}

func TestRetryExhaustedLeavesJournal(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenStore(dir)
	cfg := fastCfg()
	cfg.Retry = RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond}
	eng := NewEngine(store, threeStepMigrations(), "A", cfg)
	eng.SetFaultHook(func(ch Change, attempt int) error {
		if ch.Column == "a" {
			return errTransient // 永久失败
		}
		return nil
	})
	if _, err := eng.Migrate(context.Background(), Latest); err == nil {
		t.Fatal("should fail after exhausting retries")
	}
	// 失败点 create table(成功) 之后 add column a 之前：无歧义中间态。
	j, _ := store.LoadJournal()
	if j == nil || len(j.Applied) != 1 || j.Applied[0] != 0 {
		t.Fatalf("journal should contain only committed prefix: %+v", j)
	}
	if store.IsApplied("1.0.0") {
		t.Fatal("version must not be marked applied on failure")
	}
}

func TestRollbackResumeAfterCrash(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenStore(dir)
	eng := NewEngine(store, threeStepMigrations(), "A", fastCfg())
	ctx := context.Background()
	if _, err := eng.Migrate(ctx, Latest); err != nil {
		t.Fatal(err)
	}
	// 回滚 1.0.0 的 Down 序列为 drop b, drop a, drop table t。
	// 在第 2 个变更（drop a，下标 1）处让第一次运行失败。
	eng.SetFaultHook(func(ch Change, attempt int) error {
		if ch.Kind == DropColumn && ch.Column == "a" {
			return errTransient
		}
		return nil
	})
	if _, err := eng.Rollback(ctx, ""); err == nil {
		t.Fatal("rollback should fail mid-way")
	}
	// 此时 2.0.0 已回滚完成（被移除），1.0.0 仍 applied 且其 Down 日志记录前缀 [0]。
	if store.IsApplied("2.0.0") || !store.IsApplied("1.0.0") {
		t.Fatalf("versions after partial rollback wrong: %v", store.AppliedVersions())
	}
	j, _ := store.LoadJournal()
	if j == nil || j.Direction != Down || len(j.Applied) != 1 {
		t.Fatalf("down journal wrong: %+v", j)
	}

	// 重启并解除故障，回滚应从断点继续到空库。
	store2, _ := OpenStore(dir)
	eng2 := NewEngine(store2, threeStepMigrations(), "B", fastCfg())
	res, err := eng2.Rollback(ctx, "")
	if err != nil {
		t.Fatalf("rollback resume failed: %v", err)
	}
	if !res.Recovered {
		t.Fatal("rollback resume should set Recovered")
	}
	if len(store2.AppliedVersions()) != 0 {
		t.Fatalf("expected empty applied, got %v", store2.AppliedVersions())
	}
	if len(store2.CurrentSchema().Tables) != 0 {
		t.Fatalf("expected empty schema, got %+v", store2.CurrentSchema().Tables)
	}
	if j2, _ := store2.LoadJournal(); j2 != nil {
		t.Fatal("journal should be cleared after rollback")
	}
}
