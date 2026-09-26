package migrate

import (
	"testing"
)

// stagedPlan 构造一个三阶段的分阶段迁移：v1 为普通兼容迁移（建日志表），
// v2 为分阶段迁移。每个阶段的 Down 向 rollback_log 记录自己的序号，
// 用于验证回退顺序。
func stagedPlan() []Migration {
	return []Migration{
		{Version: 1, Name: "create_log",
			Up:   []string{"CREATE TABLE rollback_log (stage INTEGER)"},
			Down: []string{"DROP TABLE rollback_log"}},
		{Version: 2, Name: "expand_contract", Stages: []Stage{
			{Name: "expand",
				Up:   []string{"CREATE TABLE users_new (id INTEGER PRIMARY KEY, name TEXT)"},
				Down: []string{"INSERT INTO rollback_log VALUES (0)", "DROP TABLE users_new"}},
			{Name: "backfill",
				Up:   []string{"INSERT INTO users_new VALUES (1, 'alice')"},
				Down: []string{"INSERT INTO rollback_log VALUES (1)", "DELETE FROM users_new"}},
			{Name: "contract",
				Up:   []string{"CREATE TABLE users_v2 (id INTEGER PRIMARY KEY)"},
				Down: []string{"INSERT INTO rollback_log VALUES (2)", "DROP TABLE users_v2"}},
		}},
	}
}

func rollbackLog(t *testing.T, e *Engine) []int {
	t.Helper()
	rows, err := e.db.QueryContext(t.Context(), `SELECT stage FROM rollback_log ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

// TestStagedMigrateCompletesAndRecordsProgress 分阶段迁移逐阶段执行，
// 每个阶段都有簿记，全部完成后版本整体标记为 applied。
func TestStagedMigrateCompletesAndRecordsProgress(t *testing.T) {
	db := openTestDB(t)
	e := New(db, fastConfig(), "i1")
	ms := stagedPlan()

	if err := e.Migrate(t.Context(), ms); err != nil {
		t.Fatal(err)
	}
	applied, _ := e.Applied(t.Context())
	if len(applied) != 2 {
		t.Fatalf("期望 2 个版本已应用, got %+v", applied)
	}
	stages, err := e.AppliedStages(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 3 || stages[0].Stage != 0 || stages[1].Stage != 1 || stages[2].Stage != 2 {
		t.Fatalf("阶段簿记不完整: %+v", stages)
	}
}

// TestStagedResumeAfterInterruption 阶段中途失败后重跑：已完成阶段不重复
// 执行，从失败阶段继续，且不会留下说不清进度的中间态。
func TestStagedResumeAfterInterruption(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	e := New(db, fastConfig(), "i1")
	ms := stagedPlan()

	// 制造中断：阶段 1（backfill）含非法 SQL。
	broken := stagedPlan()
	broken[1].Stages[1].Up = []string{"THIS IS NOT SQL"}
	if err := e.Migrate(ctx, broken); err == nil {
		t.Fatal("期望阶段失败")
	}
	// 阶段 0 已提交，阶段 1 在事务中整体回滚：users_new 存在但无数据。
	stages, _ := e.AppliedStages(ctx, 2)
	if len(stages) != 1 || stages[0].Stage != 0 {
		t.Fatalf("中断点阶段进度错误: %+v", stages)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users_new`).Scan(&n); err != nil {
		t.Fatal("阶段 0 的变更应已生效")
	}
	if n != 0 {
		t.Fatal("失败阶段留下中间态数据")
	}
	// 版本 2 未整体 applied。
	applied, _ := e.Applied(ctx)
	if len(applied) != 1 || applied[0].Version != 1 {
		t.Fatalf("中断点版本状态错误: %+v", applied)
	}

	// 修复后重跑：若阶段 0 被重复执行，CREATE TABLE users_new 会报错。
	if err := e.Migrate(ctx, ms); err != nil {
		t.Fatalf("续跑失败: %v", err)
	}
	applied, _ = e.Applied(ctx)
	if len(applied) != 2 {
		t.Fatalf("续跑后版本状态错误: %+v", applied)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM users_new`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("续跑后数据不完整: n=%d err=%v", n, err)
	}
}

// TestStagedRollbackInReverseOrder 回退按阶段逆序进行，
// 且每个已完成阶段都被有序回退。
func TestStagedRollbackInReverseOrder(t *testing.T) {
	db := openTestDB(t)
	e := New(db, fastConfig(), "i1")
	ms := stagedPlan()
	if err := e.Migrate(t.Context(), ms); err != nil {
		t.Fatal(err)
	}
	if err := e.RollbackTo(t.Context(), ms, 1); err != nil {
		t.Fatal(err)
	}
	// 回退顺序必须为阶段 2 -> 1 -> 0。
	log := rollbackLog(t, e)
	if len(log) != 3 || log[0] != 2 || log[1] != 1 || log[2] != 0 {
		t.Fatalf("回退顺序错误: %v", log)
	}
	// 版本与阶段簿记都应清除。
	applied, _ := e.Applied(t.Context())
	if len(applied) != 1 || applied[0].Version != 1 {
		t.Fatalf("回退后版本状态错误: %+v", applied)
	}
	stages, _ := e.AppliedStages(t.Context(), 2)
	if len(stages) != 0 {
		t.Fatalf("回退后阶段簿记未清除: %+v", stages)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='users_new'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("阶段变更未被回退")
	}
}

// TestStagedRollbackOfPartialProgress 中断在版本中途（部分阶段完成、版本
// 未整体 applied）时，RollbackTo 同样能有序回退已完成阶段，不留半成品。
func TestStagedRollbackOfPartialProgress(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	e := New(db, fastConfig(), "i1")

	broken := stagedPlan()
	broken[1].Stages[1].Up = []string{"THIS IS NOT SQL"}
	if err := e.Migrate(ctx, broken); err == nil {
		t.Fatal("期望阶段失败")
	}
	// 此时阶段 0 已完成、版本 2 未 applied。直接回退到 1。
	fixed := stagedPlan()
	if err := e.RollbackTo(ctx, fixed, 1); err != nil {
		t.Fatal(err)
	}
	log := rollbackLog(t, e)
	if len(log) != 1 || log[0] != 0 {
		t.Fatalf("部分进度回退顺序错误: %v", log)
	}
	stages, _ := e.AppliedStages(ctx, 2)
	if len(stages) != 0 {
		t.Fatalf("部分阶段簿记未清除: %+v", stages)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='users_new'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("部分完成的阶段变更未被回退")
	}
}
