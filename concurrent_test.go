package migrator

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// slowCreateTable 借助故障钩子阻塞，直到 gate 打开，用于把迁移“拉长”制造并发窗口。
func gateFault(gate <-chan struct{}) FaultHook {
	return func(ch Change, attempt int) error {
		if ch.Kind == CreateTable {
			<-gate
		}
		return nil
	}
}

func newSharedEngines(t *testing.T, n int, cfg Config) (*Store, []*Engine) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	engines := make([]*Engine, n)
	for i := range engines {
		engines[i] = NewEngine(store, threeStepMigrations(), fmt.Sprintf("inst-%d", i), cfg)
	}
	return store, engines
}

func TestConcurrent_SkipPolicy(t *testing.T) {
	cfg := fastCfg()
	cfg.WaitPolicy = Skip
	store, engines := newSharedEngines(t, 2, cfg)
	gate := make(chan struct{})
	store.Fault = gateFault(gate)

	var wg sync.WaitGroup
	results := make([]Result, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, en := range engines {
		wg.Add(1)
		go func(idx int, e *Engine) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = e.Migrate(context.Background(), Latest)
		}(i, en)
	}
	close(start)
	time.Sleep(80 * time.Millisecond) // 等 A 拿租约并停在 gate
	close(gate)
	wg.Wait()

	var applied, skipped int
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("engine %d error: %v", i, errs[i])
		}
		switch r.Status {
		case StatusApplied:
			applied++
		case StatusSkipped:
			skipped++
		}
	}
	if applied != 1 || skipped != 1 {
		t.Fatalf("want 1 applied & 1 skipped, got applied=%d skipped=%d (%+v)", applied, skipped, results)
	}
	if got := store.AppliedVersions(); len(got) != 2 {
		t.Fatalf("after all done want 2 versions, got %v", got)
	}
}

func TestConcurrent_WaitPolicyConverges(t *testing.T) {
	cfg := fastCfg()
	cfg.WaitPolicy = Wait
	store, engines := newSharedEngines(t, 3, cfg)
	gate := make(chan struct{})
	store.Fault = gateFault(gate)

	var wg sync.WaitGroup
	results := make([]Result, 3)
	errs := make([]error, 3)
	start := make(chan struct{})
	for i, en := range engines {
		wg.Add(1)
		go func(idx int, e *Engine) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = e.Migrate(context.Background(), Latest)
		}(i, en)
	}
	close(start)
	time.Sleep(80 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("engine %d: %v", i, err)
		}
	}
	// 等待策略下所有调用最终都认为已 applied，且底层只有一个实例真正执行。
	executors := 0
	for _, r := range results {
		if r.Status != StatusApplied {
			t.Fatalf("all should converge to applied, got %s", r.Status)
		}
		if r.Owner != "" {
			executors++
		}
	}
	if executors != 1 {
		t.Fatalf("exactly one engine should execute, got %d", executors)
	}
	if got := store.AppliedVersions(); len(got) != 2 {
		t.Fatalf("versions wrong: %v", got)
	}
}

func TestConcurrent_CrashTakeover(t *testing.T) {
	// 实例 A 在第一个变更处“崩溃”（永久阻塞、停止推进，且随后我们放弃它），
	// 租约到期后 B 自动接管并独立完成；版本只被应用一次。
	cfg := fastCfg()
	cfg.WaitPolicy = Wait
	cfg.LeaseDuration = 100 * time.Millisecond
	cfg.HeartbeatInterval = 30 * time.Millisecond
	cfg.AcquisitionTimeout = 5 * time.Second
	dir := t.TempDir()
	store, _ := OpenStore(dir)

	crash := make(chan struct{}) // 永不关闭 => A 永久阻塞
	engA := NewEngine(store, threeStepMigrations(), "A", cfg)
	aHeartbeatStopped := make(chan struct{})
	engA.SetFaultHook(func(ch Change, attempt int) error {
		if ch.Kind == CreateTable {
			// 模拟进程崩溃：工作线程僵死且不再续租，随后租约到期可被接管。
			select {
			case <-aHeartbeatStopped:
			default:
				close(aHeartbeatStopped)
			}
			<-crash
		}
		return nil
	})
	engA.SetHeartbeatStopper(func() <-chan struct{} { return aHeartbeatStopped })
	go func() { _, _ = engA.Migrate(context.Background(), Latest) }()
	// 等待 A 确实成为持有者。
	deadline := time.Now().Add(time.Second)
	for {
		if l, _ := engA.lease.CurrentLease(); l != nil && l.Owner == "A" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("A never acquired the lease")
		}
		time.Sleep(5 * time.Millisecond)
	}

	store2, _ := OpenStore(dir)
	engB := NewEngine(store2, threeStepMigrations(), "B", cfg)
	// B 不注入故障，可以一路执行完。
	startB := time.Now()
	res, err := engB.Migrate(context.Background(), Latest)
	elapsed := time.Since(startB)
	if err != nil {
		t.Fatalf("B should take over and succeed: %v", err)
	}
	if res.Owner != "B" {
		t.Fatalf("B must be the actual executor, owner=%q result=%+v", res.Owner, res)
	}
	// B 必须等到 A 的租约过期（约 100ms）才能接管，而不是立即抢到。
	if elapsed < 70*time.Millisecond {
		t.Fatalf("B took over before lease expiry (%v)", elapsed)
	}
	if got := store2.AppliedVersions(); len(got) != 2 || got[0] != "1.0.0" || got[1] != "2.0.0" {
		t.Fatalf("post-takeover versions wrong: %v", got)
	}
	// A 没有提交任何变更；若 B 误判状态重复 create table，会失败。
	sc := store2.CurrentSchema()
	if len(sc.Tables) != 1 {
		t.Fatalf("expected exactly 1 table, got %d", len(sc.Tables))
	}
	tbl := sc.Tables[0]
	if len(tbl.Columns) != 4 {
		t.Fatalf("expected 4 columns (id,a,b,c), got %+v", tbl.Columns)
	}
	// B 正常完成后应释放租约；若未释放（被 A 残留），会看到非 B 的持有者。
	lease, _ := engB.lease.CurrentLease()
	if lease != nil {
		t.Fatalf("lease should be released after completion, got %+v", lease)
	}
}
