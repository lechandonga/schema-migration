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
	if _, err := e.Migrate(ctx, sampleMigrations()); err != nil {
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
	go func() { defer wg.Done(); _, errs[0] = engA.Migrate(t.Context(), slow) }()
	go func() { defer wg.Done(); _, errs[1] = engB.Migrate(t.Context(), slow) }()
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
	_, err = e.Migrate(t.Context(), sampleMigrations())
	if !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("期望 ErrLeaseBusy, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("等待方未在超时后返回: %v", elapsed)
	}
}

// TestYieldPolicyReturnsImmediately 让出策略下，执行权被持有时本次调用
// 立即正常返回 OutcomeYielded，且不执行任何迁移。
func TestYieldPolicyReturnsImmediately(t *testing.T) {
	db := openTestDB(t)
	holder := newLease(db, "busy-instance", 30*time.Second)
	ok, err := holder.acquire(t.Context())
	if err != nil || !ok {
		t.Fatal("获取租约失败")
	}
	t.Cleanup(func() { _ = holder.release(t.Context()) })

	cfg := fastConfig()
	cfg.Contention = ContentionYield
	e := New(db, cfg, "yielder")
	start := time.Now()
	outcome, err := e.Migrate(t.Context(), sampleMigrations())
	if err != nil {
		t.Fatalf("让出不应作为错误返回: %v", err)
	}
	if outcome != OutcomeYielded {
		t.Fatalf("期望 OutcomeYielded, got %v", outcome)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("让出策略应立即返回, 耗时 %v", elapsed)
	}
	// 未执行任何迁移。
	applied, _ := e.Applied(t.Context())
	if len(applied) != 0 {
		t.Fatalf("让出的实例不应执行迁移: %+v", applied)
	}
}

// TestYieldThenTakeoverAfterExpiry 持有者异常退出（不释放、不续期）后，
// 让出策略的实例先让出，租约失效后重试可自动接手执行。
func TestYieldThenTakeoverAfterExpiry(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	crashed := newLease(db, "crashed-instance", 300*time.Millisecond)
	ok, err := crashed.acquire(ctx)
	if err != nil || !ok {
		t.Fatal("获取租约失败")
	}
	// 模拟崩溃：不释放、不心跳。

	cfg := fastConfig()
	cfg.Contention = ContentionYield
	e := New(db, cfg, "standby")

	// 租约未失效：立即让出。
	outcome, err := e.Migrate(ctx, sampleMigrations())
	if err != nil || outcome != OutcomeYielded {
		t.Fatalf("期望让出: outcome=%v err=%v", outcome, err)
	}

	// 等待租约失效后重试：自动接手并执行完成。
	time.Sleep(400 * time.Millisecond)
	outcome, err = e.Migrate(ctx, sampleMigrations())
	if err != nil {
		t.Fatalf("接管后执行失败: %v", err)
	}
	if outcome != OutcomeExecuted {
		t.Fatalf("期望 OutcomeExecuted, got %v", outcome)
	}
	applied, _ := e.Applied(ctx)
	if len(applied) != 3 {
		t.Fatalf("接管后迁移未完整执行: %+v", applied)
	}
}

// TestConcurrentYieldDoesNotDoubleApply 实例 A 持有执行权执行慢迁移期间，
// 实例 B 按让出策略立即让出；最终迁移恰好执行一次。
func TestConcurrentYieldDoesNotDoubleApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	dbA := openDBAt(t, path)
	dbB := openDBAt(t, path)

	slow := append(sampleMigrations(), Migration{
		Version: 4, Name: "slow_backfill",
		Up: []string{`CREATE TABLE big AS
			WITH RECURSIVE cnt(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM cnt WHERE x < 2000000)
			SELECT x FROM cnt`},
		Down: []string{"DROP TABLE big"},
	})

	// A 先取得执行权（模拟其正在执行）。
	holderA := newLease(dbA, "instance-A", 30*time.Second)
	ok, err := holderA.acquire(t.Context())
	if err != nil || !ok {
		t.Fatal("A 获取租约失败")
	}

	cfgB := fastConfig()
	cfgB.Contention = ContentionYield
	engB := New(dbB, cfgB, "instance-B")

	// B 发现执行权被拿住，立即让出而非等待或报错。
	outB, errB := engB.Migrate(t.Context(), slow)
	if errB != nil {
		t.Fatalf("B 让出不应报错: %v", errB)
	}
	if outB != OutcomeYielded {
		t.Fatalf("B 应让出, got %v", outB)
	}

	// A 完成执行（同一 holder，租约续期有效）。
	cfgA := fastConfig()
	engA := New(dbA, cfgA, "instance-A")
	outA, errA := engA.Migrate(t.Context(), slow)
	if errA != nil {
		t.Fatalf("A 执行失败: %v", errA)
	}
	if outA != OutcomeExecuted {
		t.Fatalf("A 应执行完成, got %v", outA)
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
