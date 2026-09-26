package migrate

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openTestDB 打开一个基于临时文件的 SQLite 库，模拟真实嵌入式数据库。
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func fastConfig() Config {
	cfg := DefaultConfig()
	cfg.LeaseTTL = 2 * time.Second
	cfg.HeartbeatInterval = 200 * time.Millisecond
	cfg.AcquireTimeout = 5 * time.Second
	cfg.RetryInterval = 20 * time.Millisecond
	cfg.StatementTimeout = 30 * time.Second
	return cfg
}

func sampleMigrations() []Migration {
	return []Migration{
		{Version: 1, Name: "create_users",
			Up:   []string{"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)"},
			Down: []string{"DROP TABLE users"}},
		{Version: 2, Name: "add_email",
			Up:   []string{"ALTER TABLE users ADD COLUMN email TEXT"},
			Down: []string{"ALTER TABLE users DROP COLUMN email"}},
		{Version: 3, Name: "create_orders",
			Up:   []string{"CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER)"},
			Down: []string{"DROP TABLE orders"}},
	}
}

func TestMigrateAppliesInOrderAndSkipsApplied(t *testing.T) {
	db := openTestDB(t)
	e := New(db, fastConfig(), "i1")
	ms := sampleMigrations()

	if _, err := e.Migrate(t.Context(), ms[:2]); err != nil {
		t.Fatal(err)
	}
	applied, err := e.Applied(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 || applied[0].Version != 1 || applied[1].Version != 2 {
		t.Fatalf("应用顺序错误: %+v", applied)
	}

	// 再次运行：已应用版本不得重复执行，仅追加版本 3。
	if _, err := e.Migrate(t.Context(), ms); err != nil {
		t.Fatal(err)
	}
	applied, _ = e.Applied(t.Context())
	if len(applied) != 3 {
		t.Fatalf("期望 3 条记录, got %d", len(applied))
	}
}

func TestRollbackToRevertsInReverseOrder(t *testing.T) {
	db := openTestDB(t)
	e := New(db, fastConfig(), "i1")
	ms := sampleMigrations()
	if _, err := e.Migrate(t.Context(), ms); err != nil {
		t.Fatal(err)
	}
	if err := e.RollbackTo(t.Context(), ms, 1); err != nil {
		t.Fatal(err)
	}
	applied, _ := e.Applied(t.Context())
	if len(applied) != 1 || applied[0].Version != 1 {
		t.Fatalf("回滚后状态错误: %+v", applied)
	}
	// orders 表应已删除，users 表应保留。
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='orders'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("orders 表未被回滚删除")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='users'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("users 表不应被回滚")
	}
}

func TestResumeAfterFailureLeavesNoPartialState(t *testing.T) {
	db := openTestDB(t)
	e := New(db, fastConfig(), "i1")
	bad := []Migration{
		sampleMigrations()[0],
		{Version: 2, Name: "broken",
			Up:   []string{"CREATE TABLE t2 (id INTEGER)", "THIS IS NOT SQL"},
			Down: []string{"DROP TABLE t2"}},
	}
	if _, err := e.Migrate(t.Context(), bad); err == nil {
		t.Fatal("期望迁移失败")
	}
	// 失败的版本 2 在事务中整体回滚，不应留下 t2 表。
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='t2'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("失败迁移留下中间态 t2 表")
	}
	// 版本 1 已应用，不得重复执行。
	applied, _ := e.Applied(t.Context())
	if len(applied) != 1 || applied[0].Version != 1 {
		t.Fatalf("中断点状态错误: %+v", applied)
	}

	// 修复后重跑：从中断处继续，版本 1 不重复应用（否则 CREATE TABLE users 会报错）。
	fixed := []Migration{
		sampleMigrations()[0],
		{Version: 2, Name: "fixed",
			Up:   []string{"CREATE TABLE t2 (id INTEGER)"},
			Down: []string{"DROP TABLE t2"}},
	}
	if _, err := e.Migrate(t.Context(), fixed); err != nil {
		t.Fatalf("恢复执行失败: %v", err)
	}
	applied, _ = e.Applied(t.Context())
	if len(applied) != 2 {
		t.Fatalf("恢复后状态错误: %+v", applied)
	}
}

func TestRecordOfMissing(t *testing.T) {
	db := openTestDB(t)
	e := New(db, fastConfig(), "i1")
	if err := e.ensureTables(t.Context()); err != nil {
		t.Fatal(err)
	}
	rec, err := e.recordOf(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatal("期望无记录")
	}
}

// TestHistoryDestructiveDoesNotBlockCompatible 历史里已应用过破坏性版本后，
// 后续只追加常规兼容版本时不应再被拒绝。
func TestHistoryDestructiveDoesNotBlockCompatible(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	destructive := []Migration{
		{Version: 1, Name: "create_users",
			Up:   []string{"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)"},
			Down: []string{"DROP TABLE users"}},
		{Version: 2, Name: "drop_name",
			Up:   []string{"ALTER TABLE users DROP COLUMN name"},
			Down: []string{"ALTER TABLE users ADD COLUMN name TEXT"}},
	}
	// 收缩阶段：显式放行后落库破坏性版本。
	cfg := fastConfig()
	cfg.AllowDestructive = true
	if _, err := New(db, cfg, "i1").Migrate(ctx, destructive); err != nil {
		t.Fatalf("放行后破坏性迁移应成功: %v", err)
	}

	// 后续常规迭代：携带历史版本 + 新增兼容版本，默认配置（不放行）应正常执行。
	next := append(destructive, Migration{
		Version: 3, Name: "add_email",
		Up:   []string{"ALTER TABLE users ADD COLUMN email TEXT"},
		Down: []string{"ALTER TABLE users DROP COLUMN email"},
	})
	e := New(db, fastConfig(), "i1")
	if _, err := e.Migrate(ctx, next); err != nil {
		t.Fatalf("历史破坏性版本不应拦截兼容迁移: %v", err)
	}
	applied, _ := e.Applied(ctx)
	if len(applied) != 3 {
		t.Fatalf("期望 3 条记录, got %+v", applied)
	}
}

// TestPendingDestructiveStillRejected 待执行版本含破坏性变更时仍被拒绝，
// 且错误能区分具体类别。
func TestPendingDestructiveStillRejected(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	e := New(db, fastConfig(), "i1")
	if _, err := e.Migrate(ctx, sampleMigrations()); err != nil {
		t.Fatal(err)
	}
	pending := append(sampleMigrations(), Migration{
		Version: 4, Name: "drop_orders",
		Up:   []string{"DROP TABLE orders"},
		Down: []string{"CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER)"},
	})
	_, err := e.Migrate(ctx, pending)
	var cerr *CompatibilityError
	if !errors.As(err, &cerr) {
		t.Fatalf("期望 CompatibilityError, got %v", err)
	}
	if !cerr.HasKind(ViolationDropTable) {
		t.Fatalf("应识别 drop_table 类别: %v", cerr.Kinds())
	}
	// 被拒绝的版本不得默默执行。
	applied, _ := e.Applied(ctx)
	if len(applied) != 3 {
		t.Fatalf("破坏性版本不应被执行: %+v", applied)
	}
}
