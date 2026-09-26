package migrate

import (
	"errors"
	"testing"
)

// stagedPlan 模拟一次"删列"破坏性变更的分阶段拆解：
// 阶段1 扩展（兼容），阶段2 收缩（破坏性，需显式放行）。
func stagedPlan() []Stage {
	return []Stage{
		{ID: 1, Name: "expand_add_name_new",
			Migrations: []Migration{
				{Version: 1, Name: "create_users",
					Up:   []string{"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)"},
					Down: []string{"DROP TABLE users"}},
				{Version: 2, Name: "add_name_new",
					Up:   []string{"ALTER TABLE users ADD COLUMN name_new TEXT"},
					Down: []string{"ALTER TABLE users DROP COLUMN name_new"}},
			}},
		{ID: 2, Name: "contract_drop_name",
			Migrations: []Migration{
				{Version: 3, Name: "drop_name",
					Up:   []string{"ALTER TABLE users DROP COLUMN name"},
					Down: []string{"ALTER TABLE users ADD COLUMN name TEXT"}},
			}},
	}
}

// TestApplyStageInOrder 逐阶段推进：每阶段完成后进度可查，
// 已完成的阶段重复调用幂等跳过。
func TestApplyStageInOrder(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	e := New(db, fastConfig(), "i1")
	stages := stagedPlan()

	// 阶段1（兼容变更）默认配置即可推进。
	out, err := e.ApplyStage(ctx, stages[0])
	if err != nil || out != OutcomeExecuted {
		t.Fatalf("阶段1推进失败: out=%v err=%v", out, err)
	}
	// 阶段1结束后，旧版本实例仍可使用 name 列（尚未删除）。
	if _, err := db.Exec(`INSERT INTO users (name) VALUES ('old-app')`); err != nil {
		t.Fatalf("阶段1后旧版本实例应仍可用: %v", err)
	}

	// 重复调用已完成的阶段：幂等跳过，不报错不重复执行。
	if _, err := e.ApplyStage(ctx, stages[0]); err != nil {
		t.Fatalf("已完成阶段重复调用应幂等: %v", err)
	}

	// 阶段2 含破坏性变更，未放行时应被拒绝且不执行。
	if _, err := e.ApplyStage(ctx, stages[1]); err == nil {
		t.Fatal("阶段2未放行破坏性变更应被拒绝")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE name='old-app'`).Scan(&n); err != nil {
		t.Fatal("被拒绝的阶段不应执行任何迁移")
	}

	// 显式放行后推进阶段2。
	cfgAllow := fastConfig()
	cfgAllow.AllowDestructive = true
	if _, err := New(db, cfgAllow, "i1").ApplyStage(ctx, stages[1]); err != nil {
		t.Fatalf("阶段2推进失败: %v", err)
	}

	got, err := e.Stages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("阶段进度错误: %+v", got)
	}
}

// TestApplyStageRejectsSkipping 跳过中间阶段推进会被拒绝，
// 避免留下说不清进度的状态。
func TestApplyStageRejectsSkipping(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	e := New(db, fastConfig(), "i1")
	stages := stagedPlan()

	if _, err := e.ApplyStage(ctx, stages[0]); err != nil {
		t.Fatal(err)
	}
	skip := Stage{ID: 3, Name: "too_far", Migrations: stages[1].Migrations}
	_, err := e.ApplyStage(ctx, skip)
	if !errors.Is(err, ErrStageOrder) {
		t.Fatalf("跳过阶段应返回 ErrStageOrder, got %v", err)
	}
}

// TestApplyStageResumeAfterInterruption 阶段内迁移失败后，重新发起
// 会从中断处继续：已完成的迁移不重复执行，阶段簿记最终完整。
func TestApplyStageResumeAfterInterruption(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	e := New(db, fastConfig(), "i1")

	broken := Stage{ID: 1, Name: "expand", Migrations: []Migration{
		{Version: 1, Name: "create_users",
			Up:   []string{"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)"},
			Down: []string{"DROP TABLE users"}},
		{Version: 2, Name: "broken",
			Up:   []string{"CREATE TABLE t2 (id INTEGER)", "THIS IS NOT SQL"},
			Down: []string{"DROP TABLE t2"}},
	}}
	if _, err := e.ApplyStage(ctx, broken); err == nil {
		t.Fatal("期望阶段推进失败")
	}
	// 中断点：迁移1已应用，迁移2整体回滚，阶段簿记未写入。
	applied, _ := e.Applied(ctx)
	if len(applied) != 1 || applied[0].Version != 1 {
		t.Fatalf("中断点迁移状态错误: %+v", applied)
	}
	stages, _ := e.Stages(ctx)
	if len(stages) != 0 {
		t.Fatalf("中断后不应有阶段簿记: %+v", stages)
	}

	// 修复后重新发起同一阶段：迁移1不重复执行（否则 CREATE TABLE 报错），
	// 仅续跑迁移2并补写阶段簿记。
	fixed := broken
	fixed.Migrations[1] = Migration{Version: 2, Name: "fixed",
		Up:   []string{"CREATE TABLE t2 (id INTEGER)"},
		Down: []string{"DROP TABLE t2"}}
	if _, err := e.ApplyStage(ctx, fixed); err != nil {
		t.Fatalf("续跑失败: %v", err)
	}
	applied, _ = e.Applied(ctx)
	if len(applied) != 2 {
		t.Fatalf("续跑后迁移状态错误: %+v", applied)
	}
	stages, _ = e.Stages(ctx)
	if len(stages) != 1 || stages[0].ID != 1 {
		t.Fatalf("续跑后阶段簿记错误: %+v", stages)
	}
}

// TestRollbackStageInReverseOrder 只能有序回退最近完成的阶段，
// 阶段内迁移按逆序回滚。
func TestRollbackStageInReverseOrder(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	cfg := fastConfig()
	cfg.AllowDestructive = true
	e := New(db, cfg, "i1")
	stages := stagedPlan()
	for _, s := range stages {
		if _, err := e.ApplyStage(ctx, s); err != nil {
			t.Fatalf("推进阶段 %d 失败: %v", s.ID, err)
		}
	}

	// 不能先回退阶段1（不是最近完成的阶段）。
	_, err := e.RollbackStage(ctx, stages[0])
	if !errors.Is(err, ErrStageOrder) {
		t.Fatalf("回退非最近阶段应返回 ErrStageOrder, got %v", err)
	}

	// 有序回退：先阶段2，name 列恢复。
	if _, err := e.RollbackStage(ctx, stages[1]); err != nil {
		t.Fatalf("回退阶段2失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (name) VALUES ('restored')`); err != nil {
		t.Fatalf("回退阶段2后 name 列应恢复: %v", err)
	}
	// 再回退阶段1，users 表删除。
	if _, err := e.RollbackStage(ctx, stages[0]); err != nil {
		t.Fatalf("回退阶段1失败: %v", err)
	}
	got, _ := e.Stages(ctx)
	if len(got) != 0 {
		t.Fatalf("阶段簿记应清空: %+v", got)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='users'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("回退阶段1后 users 表应删除")
	}
	// 全部回退后再回退属于顺序非法。
	if _, err := e.RollbackStage(ctx, stages[0]); !errors.Is(err, ErrStageOrder) {
		t.Fatalf("无已完成阶段时回退应返回 ErrStageOrder, got %v", err)
	}
}
