package migrate

import (
	"context"
	"fmt"
	"time"
)

// 本文件实现分阶段（staged）执行：一个版本的破坏性变更可拆成若干阶段，
// 每个阶段独立事务提交并单独簿记。阶段之间可以发布新应用版本、观察或
// 暂停；进程中断或某阶段失败后重新发起时，已完成阶段不会重复执行。
// 设计约束：每个阶段结束时，数据库对尚未升级的旧版本实例仍保持可用
// （expand/contract 模式，由迁移定义保证，引擎负责断点续跑与有序回退）。

// applyStaged 按方向执行一个分阶段迁移。
// Up：跳过已完成阶段，逐阶段前进，全部完成后才将版本标记为 applied。
// Down：按逆序回退已完成阶段，全部回退后删除版本簿记。
func (e *Engine) applyStaged(ctx context.Context, m Migration, dir Direction) error {
	if dir == Up {
		return e.stageUp(ctx, m)
	}
	return e.stageDown(ctx, m)
}

func (e *Engine) stageUp(ctx context.Context, m Migration) error {
	done, err := e.appliedStageSet(ctx, m.Version)
	if err != nil {
		return err
	}
	for i := range m.Stages {
		if done[i] {
			continue // 已完成阶段不重复执行
		}
		if err := e.applyStage(ctx, m, i, Up); err != nil {
			return err
		}
	}
	// 全部阶段完成后，版本整体标记为 applied。
	_, err = e.db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, state, applied_at)
		VALUES (?, ?, 'applied', ?)
		ON CONFLICT(version) DO UPDATE SET state='applied', applied_at=excluded.applied_at`,
		m.Version, m.Name, time.Now().UnixNano())
	return err
}

func (e *Engine) stageDown(ctx context.Context, m Migration) error {
	recs, err := e.AppliedStages(ctx, m.Version)
	if err != nil {
		return err
	}
	// 逆序回退已完成阶段。
	for i := len(recs) - 1; i >= 0; i-- {
		if err := e.applyStage(ctx, m, recs[i].Stage, Down); err != nil {
			return err
		}
	}
	_, err = e.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = ?`, m.Version)
	return err
}

// applyStage 在单个事务中执行一个阶段及其簿记：要么全部生效、要么整体
// 回滚，不会留下无法判断状态的中间态。
func (e *Engine) applyStage(ctx context.Context, m Migration, idx int, dir Direction) error {
	if idx < 0 || idx >= len(m.Stages) {
		return fmt.Errorf("migrate: 版本 %d 没有阶段 %d", m.Version, idx)
	}
	st := m.Stages[idx]
	stmts := st.Up
	if dir == Down {
		stmts = st.Down
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, stmt := range stmts {
		sctx, cancel := context.WithTimeout(ctx, e.cfg.StatementTimeout)
		_, err := tx.ExecContext(sctx, stmt)
		cancel()
		if err != nil {
			_ = tx.Rollback()
			e.markStageFailed(m, idx, st.Name)
			return fmt.Errorf("migrate: 版本 %d (%s) 阶段 %d (%s) %s 失败: %w",
				m.Version, m.Name, idx, st.Name, dir, err)
		}
	}
	if dir == Up {
		_, err = tx.ExecContext(ctx, `INSERT INTO schema_migration_stages (version, stage, name, state, applied_at)
			VALUES (?, ?, ?, 'applied', ?)
			ON CONFLICT(version, stage) DO UPDATE SET state='applied', applied_at=excluded.applied_at`,
			m.Version, idx, st.Name, time.Now().UnixNano())
	} else {
		_, err = tx.ExecContext(ctx, `DELETE FROM schema_migration_stages WHERE version = ? AND stage = ?`,
			m.Version, idx)
	}
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// markStageFailed 在阶段失败后记录 failed 状态（独立事务），便于运维观测；
// 阶段本身已在事务中回滚，重跑时会安全地重试该阶段。
func (e *Engine) markStageFailed(m Migration, idx int, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = e.db.ExecContext(ctx, `INSERT INTO schema_migration_stages (version, stage, name, state, applied_at)
		VALUES (?, ?, ?, 'failed', ?)
		ON CONFLICT(version, stage) DO UPDATE SET state='failed', applied_at=excluded.applied_at`,
		m.Version, idx, name, time.Now().UnixNano())
}

// AppliedStages 返回指定版本已完成（state='applied'）的阶段记录，按阶段序号升序。
func (e *Engine) AppliedStages(ctx context.Context, version uint) ([]StageRecord, error) {
	if err := e.ensureTables(ctx); err != nil {
		return nil, err
	}
	rows, err := e.db.QueryContext(ctx,
		`SELECT version, stage, name, state, applied_at FROM schema_migration_stages
		 WHERE version = ? AND state='applied' ORDER BY stage`, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StageRecord
	for rows.Next() {
		var rec StageRecord
		if err := rows.Scan(&rec.Version, &rec.Stage, &rec.Name, &rec.State, &rec.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// appliedStageSet 返回指定版本已完成阶段的序号集合。
func (e *Engine) appliedStageSet(ctx context.Context, version uint) (map[int]bool, error) {
	recs, err := e.AppliedStages(ctx, version)
	if err != nil {
		return nil, err
	}
	set := make(map[int]bool, len(recs))
	for _, r := range recs {
		set[r.Stage] = true
	}
	return set, nil
}

// stagedVersionsWithProgress 返回有阶段簿记（含部分完成）的版本号集合。
func (e *Engine) stagedVersionsWithProgress(ctx context.Context) (map[uint]bool, error) {
	if err := e.ensureTables(ctx); err != nil {
		return nil, err
	}
	rows, err := e.db.QueryContext(ctx,
		`SELECT DISTINCT version FROM schema_migration_stages WHERE state='applied'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint]bool{}
	for rows.Next() {
		var v uint
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}
