package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrStageOrder 表示阶段推进或回退顺序非法（如跳过中间阶段、
// 回退的不是最近完成的阶段）。
var ErrStageOrder = errors.New("migrate: 阶段顺序非法")

// Stage 表示一次破坏性变更拆分出的、可独立推进的阶段。
// 每个阶段对应一次部署：阶段之间可以发布新的应用版本、观察或暂停。
// 一个阶段内的所有迁移对该阶段结束时尚未升级的旧版本实例保持可用。
type Stage struct {
	// ID 为全局递增的阶段序号，必须按 +1 顺序推进、按逆序回退。
	ID uint
	// Name 为阶段名称，便于运维识别。
	Name string
	// Migrations 为本阶段要执行的迁移，按版本升序执行。
	Migrations []Migration
}

// StageRecord 记录一个已完成阶段的状态。
type StageRecord struct {
	ID        uint
	Name      string
	State     string // "applied"
	AppliedAt int64
}

// ensureStageTable 创建阶段簿记表（幂等）。
func (e *Engine) ensureStageTable(ctx context.Context) error {
	_, err := e.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migration_stages (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		state TEXT NOT NULL CHECK (state IN ('applied')),
		applied_at INTEGER NOT NULL
	)`)
	return err
}

// ApplyStage 推进一个阶段：执行该阶段尚未应用的迁移并记录阶段完成。
//
// 幂等与可恢复：已完成的阶段重复调用直接跳过；阶段内已应用的迁移
// 不会重复执行，进程中断或失败后重新发起会从中断处继续。
// 顺序约束：阶段必须按 ID 递增逐一推进（首个阶段可为任意起始 ID），
// 跳过中间阶段会返回 ErrStageOrder，避免留下说不清进度的状态。
// 兼容性校验只针对本阶段真正要执行的迁移。
func (e *Engine) ApplyStage(ctx context.Context, stage Stage) (Outcome, error) {
	sorted := sortMigrations(stage.Migrations)
	err := e.withLease(ctx, func(ctx context.Context) error {
		if err := e.ensureStageTable(ctx); err != nil {
			return err
		}
		rec, err := e.stageRecord(ctx, stage.ID)
		if err != nil {
			return err
		}
		if rec != nil {
			return nil // 阶段已完成，幂等跳过
		}
		maxID, err := e.maxAppliedStage(ctx)
		if err != nil {
			return err
		}
		if maxID != nil && stage.ID != *maxID+1 {
			return fmt.Errorf("migrate: 阶段 %d 不能跳过当前进度 %d 推进: %w",
				stage.ID, *maxID, ErrStageOrder)
		}
		todo, err := e.pending(ctx, sorted)
		if err != nil {
			return err
		}
		if err := e.checkPlan(todo, Up); err != nil {
			return err
		}
		for _, m := range todo {
			if err := e.applyOne(ctx, m, Up); err != nil {
				return err
			}
		}
		// 记录阶段完成。若在迁移完成后、簿记写入前崩溃，重跑时迁移
		// 已应用会被跳过，仅补写簿记，状态始终可判定。
		_, err = e.db.ExecContext(ctx, `INSERT INTO schema_migration_stages (id, name, state, applied_at)
			VALUES (?, ?, 'applied', ?)
			ON CONFLICT(id) DO UPDATE SET state='applied', applied_at=excluded.applied_at`,
			stage.ID, stage.Name, time.Now().UnixNano())
		return err
	})
	return outcomeOf(err)
}

// RollbackStage 回退一个已完成的阶段：逆序回滚该阶段已应用的迁移并
// 移除阶段簿记。只能回退最近完成的阶段（有序回退），否则返回
// ErrStageOrder。已回退过的阶段重复调用属于顺序非法。
func (e *Engine) RollbackStage(ctx context.Context, stage Stage) (Outcome, error) {
	sorted := sortMigrations(stage.Migrations)
	err := e.withLease(ctx, func(ctx context.Context) error {
		if err := e.ensureStageTable(ctx); err != nil {
			return err
		}
		maxID, err := e.maxAppliedStage(ctx)
		if err != nil {
			return err
		}
		if maxID == nil || *maxID != stage.ID {
			return fmt.Errorf("migrate: 只能回退最近完成的阶段（当前进度 %v，请求回退阶段 %d）: %w",
				stageProgress(maxID), stage.ID, ErrStageOrder)
		}
		// 逆序回滚本阶段已应用的迁移；未应用的（如中断时未执行到）跳过。
		for i := len(sorted) - 1; i >= 0; i-- {
			rec, err := e.recordOf(ctx, sorted[i].Version)
			if err != nil {
				return err
			}
			if rec == nil || rec.State != "applied" {
				continue
			}
			if err := e.applyOne(ctx, sorted[i], Down); err != nil {
				return err
			}
		}
		_, err = e.db.ExecContext(ctx,
			`DELETE FROM schema_migration_stages WHERE id = ?`, stage.ID)
		return err
	})
	return outcomeOf(err)
}

// Stages 返回已完成的阶段记录，按阶段 ID 升序。
func (e *Engine) Stages(ctx context.Context) ([]StageRecord, error) {
	if err := e.ensureTables(ctx); err != nil {
		return nil, err
	}
	if err := e.ensureStageTable(ctx); err != nil {
		return nil, err
	}
	rows, err := e.db.QueryContext(ctx,
		`SELECT id, name, state, applied_at FROM schema_migration_stages ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StageRecord
	for rows.Next() {
		var rec StageRecord
		if err := rows.Scan(&rec.ID, &rec.Name, &rec.State, &rec.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// outcomeOf 把内部哨兵错误转换为公开的三态结果。
func outcomeOf(err error) (Outcome, error) {
	if errors.Is(err, errYielded) {
		return OutcomeYielded, nil
	}
	if err != nil {
		return OutcomeExecuted, err
	}
	return OutcomeExecuted, nil
}

func (e *Engine) stageRecord(ctx context.Context, id uint) (*StageRecord, error) {
	row := e.db.QueryRowContext(ctx,
		`SELECT id, name, state, applied_at FROM schema_migration_stages WHERE id = ?`, id)
	var rec StageRecord
	err := row.Scan(&rec.ID, &rec.Name, &rec.State, &rec.AppliedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// maxAppliedStage 返回当前已完成的最高阶段 ID；无已完成阶段时返回 nil。
func (e *Engine) maxAppliedStage(ctx context.Context) (*uint, error) {
	row := e.db.QueryRowContext(ctx,
		`SELECT MAX(id) FROM schema_migration_stages WHERE state='applied'`)
	var max sql.NullInt64
	if err := row.Scan(&max); err != nil {
		return nil, err
	}
	if !max.Valid {
		return nil, nil
	}
	id := uint(max.Int64)
	return &id, nil
}

func stageProgress(maxID *uint) string {
	if maxID == nil {
		return "无已完成阶段"
	}
	return fmt.Sprintf("阶段 %d", *maxID)
}
