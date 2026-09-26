package migrate

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openDBAt(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(60000)", path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestLeaseMutualExclusion 持有租约期间，其他实例在超时前无法获取执行权。
func TestLeaseMutualExclusion(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	holder := newLease(db, "instance-A", 10*time.Second)
	ok, err := holder.acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("首次获取租约失败: ok=%v err=%v", ok, err)
	}

	// 同一时刻另一实例获取应失败。
	other := newLease(db, "instance-B", 10*time.Second)
	ok, err = other.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("租约被两个实例同时持有")
	}

	// 持有者释放后，其他实例可立即接管。
	if err := holder.release(ctx); err != nil {
		t.Fatal(err)
	}
	ok, err = other.acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("释放后仍无法获取: ok=%v err=%v", ok, err)
	}
}

// TestLeaseExpiresAfterCrash 持有者崩溃（不释放、不续期）后，
// 租约在 TTL 到期自动失效，其他实例可接管。
func TestLeaseExpiresAfterCrash(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	crashed := newLease(db, "crashed-instance", 300*time.Millisecond)
	ok, err := crashed.acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("获取租约失败: ok=%v err=%v", ok, err)
	}
	// 模拟崩溃：不调用 release，也没有心跳续期。

	cfg := fastConfig()
	cfg.AcquireTimeout = 5 * time.Second
	e := New(db, cfg, "survivor")
	start := time.Now()
	if err := e.Migrate(ctx, sampleMigrations()); err != nil {
		t.Fatalf("崩溃接管后迁移失败: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("接管早于租约过期时间: %v", elapsed)
	}
	applied, _ := e.Applied(ctx)
	if len(applied) != 3 {
		t.Fatalf("接管后迁移未完整执行: %+v", applied)
	}
}

// TestConcurrentMigrateMutualExclusion 两个实例并发启动迁移，
// 同一时刻只有一个执行，另一个等待后跳过已完成版本。
func TestConcurrentMigrateMutualExclusion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	dbA := openDBAt(t, path)
	dbB := openDBAt(t, path)

	cfgA := fastConfig()
	cfgA.AcquireTimeout = 30 * time.Second
	cfgB := fastConfig()
	cfgB.AcquireTimeout = 30 * time.Second

	// 用一个较慢的迁移拉长执行窗口，确保两实例真正重叠。
	slow := append(sampleMigrations(), Migration{
		Version: 4, Name: "slow_backfill",
		Up: []string{`CREATE TABLE big AS
			WITH RECURSIVE cnt(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM cnt WHERE x < 2000000)
			SELECT x FROM cnt`},
		Down: []string{"DROP TABLE big"},
	})

	engA := New(dbA, cfgA, "instance-A")
	engB := New(dbB, cfgB, "instance-B")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = engA.Migrate(t.Context(), slow) }()
	go func() { defer wg.Done(); errs[1] = engB.Migrate(t.Context(), slow) }()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("实例 %d 迁移失败: %v", i, err)
		}
	}

	applied, err := engA.Applied(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 4 {
		t.Fatalf("期望 4 个版本恰好应用一次, got %+v", applied)
	}
	// 大表只应被创建一次且数据完整。
	var n int
	if err := dbA.QueryRow(`SELECT COUNT(*) FROM big`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2000000 {
		t.Fatalf("迁移被重复应用或部分应用: %d 行", n)
	}
}

// TestAcquireTimeoutReturnsBusy 租约被长期持有且 AcquireTimeout 较短时，
// 等待方应收到可区分的 ErrLeaseBusy，而不是永久阻塞。
func TestAcquireTimeoutReturnsBusy(t *testing.T) {
	db := openTestDB(t)
	holder := newLease(db, "long-runner", 30*time.Second)
	ok, err := holder.acquire(t.Context())
	if err != nil || !ok {
		t.Fatal("获取租约失败")
	}
	t.Cleanup(func() { _ = holder.release(t.Context()) })

	cfg := fastConfig()
	cfg.AcquireTimeout = 200 * time.Millisecond
	e := New(db, cfg, "waiter")
	start := time.Now()
	err = e.Migrate(t.Context(), sampleMigrations())
	if !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("期望 ErrLeaseBusy, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("等待方未在超时后返回: %v", elapsed)
	}
}

// TestYieldPolicyReturnsImmediately 让出策略下，执行权被其他实例持有时
// 立即正常返回 ResultYielded，不执行任何迁移，也不视为失败。
func TestYieldPolicyReturnsImmediately(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	holder := newLease(db, "long-runner", 30*time.Second)
	ok, err := holder.acquire(ctx)
	if err != nil || !ok {
		t.Fatal("获取租约失败")
	}
	t.Cleanup(func() { _ = holder.release(ctx) })

	cfg := fastConfig()
	cfg.ContentionPolicy = ContentionYield
	e := New(db, cfg, "yielder")
	start := time.Now()
	res, err := e.MigrateWithResult(ctx, sampleMigrations())
	if err != nil {
		t.Fatalf("让出不应视为失败: %v", err)
	}
	if res != ResultYielded {
		t.Fatalf("期望 ResultYielded, got %v", res)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("让出策略应立即返回: %v", elapsed)
	}
	// 未执行任何迁移。
	applied, _ := e.Applied(ctx)
	if len(applied) != 0 {
		t.Fatalf("让出的实例不应执行迁移: %+v", applied)
	}
}

// TestYieldTakeoverAfterLeaseExpiry 持有执行权的实例异常退出（不释放、
// 不续期）后，让出策略的实例在租约失效后能自动接手执行。
func TestYieldTakeoverAfterLeaseExpiry(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	crashed := newLease(db, "crashed-instance", 300*time.Millisecond)
	ok, err := crashed.acquire(ctx)
	if err != nil || !ok {
		t.Fatal("获取租约失败")
	}
	// 模拟崩溃：不释放、不心跳。

	cfg := fastConfig()
	cfg.ContentionPolicy = ContentionYield
	e := New(db, cfg, "survivor")

	// 租约未失效前：让出。
	res, err := e.MigrateWithResult(ctx, sampleMigrations())
	if err != nil || res != ResultYielded {
		t.Fatalf("失效前应让出: res=%v err=%v", res, err)
	}

	// 租约失效后：接手并执行完成。
	time.Sleep(400 * time.Millisecond)
	res, err = e.MigrateWithResult(ctx, sampleMigrations())
	if err != nil {
		t.Fatalf("失效接管失败: %v", err)
	}
	if res != ResultExecuted {
		t.Fatalf("期望 ResultExecuted, got %v", res)
	}
	applied, _ := e.Applied(ctx)
	if len(applied) != 3 {
		t.Fatalf("接管后迁移未完整执行: %+v", applied)
	}
}

// TestConcurrentYieldBothYield 执行权被持有时，多个让出策略实例并发启动，
// 全部正常让出，无一执行迁移。
func TestConcurrentYieldBothYield(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	dbA := openDBAt(t, path)
	dbB := openDBAt(t, path)

	// 第三方实例长期持有执行权。
	holder := newLease(dbA, "long-runner", 30*time.Second)
	ok, err := holder.acquire(t.Context())
	if err != nil || !ok {
		t.Fatal("获取租约失败")
	}
	t.Cleanup(func() { _ = holder.release(t.Context()) })

	cfg := fastConfig()
	cfg.ContentionPolicy = ContentionYield
	engA := New(dbA, cfg, "instance-A")
	engB := New(dbB, cfg, "instance-B")

	var wg sync.WaitGroup
	results := make([]Result, 2)
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); results[0], errs[0] = engA.MigrateWithResult(t.Context(), sampleMigrations()) }()
	go func() { defer wg.Done(); results[1], errs[1] = engB.MigrateWithResult(t.Context(), sampleMigrations()) }()
	wg.Wait()
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("实例 %d 让出不应报错: %v", i, errs[i])
		}
		if results[i] != ResultYielded {
			t.Fatalf("实例 %d 期望让出, got %v", i, results[i])
		}
	}
	applied, err := engA.Applied(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("让出的实例不应执行迁移: %+v", applied)
	}
}

// TestWaitPolicyStillReportsBusyOnTimeout 等待策略下超时仍返回
// 可区分的 ErrLeaseBusy（三种结果：执行完成 / 让出 / 超时）。
func TestWaitPolicyStillReportsBusyOnTimeout(t *testing.T) {
	db := openTestDB(t)
	holder := newLease(db, "long-runner", 30*time.Second)
	ok, err := holder.acquire(t.Context())
	if err != nil || !ok {
		t.Fatal("获取租约失败")
	}
	t.Cleanup(func() { _ = holder.release(t.Context()) })

	cfg := fastConfig()
	cfg.AcquireTimeout = 200 * time.Millisecond
	e := New(db, cfg, "waiter")
	res, err := e.MigrateWithResult(t.Context(), sampleMigrations())
	if !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("期望 ErrLeaseBusy, got res=%v err=%v", res, err)
	}
}
