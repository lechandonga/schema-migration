package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ErrLeaseLost 表示执行过程中租约被其他实例接管。
var ErrLeaseLost = errors.New("migrate: 执行过程中租约丢失")

// Engine 为迁移引擎，协调多实例并发执行与可恢复应用。
type Engine struct {
	db     *sql.DB
	cfg    Config
	holder string
}

// New 创建迁移引擎。holder 用于区分不同实例，通常为主机名+进程号。
func New(db *sql.DB, cfg Config, holder string) *Engine {
	return &Engine{db: db, cfg: cfg, holder: holder}
}

func (e *Engine) ensureTables(ctx context.Context) error {
	_, err := e.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		state TEXT NOT NULL CHECK (state IN ('applied','failed')),
		applied_at INTEGER NOT NULL
	)`)
	return err
}

// acquireWithRetry 在 AcquireTimeout 内按 RetryInterval 重试获取租约。
// 若配置为 ContentionYield 策略，首次尝试失败即返回 errYielded。
func (e *Engine) acquireWithRetry(ctx context.Context, l *lease) error {
	deadline := time.Now().Add(e.cfg.AcquireTimeout)
	for {
		ok, err := l.acquire(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if e.cfg.Contention == ContentionYield {
			return errYielded
		}
		if time.Now().After(deadline) {
			return ErrLeaseBusy
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.cfg.RetryInterval):
		}
	}
}

// heartbeat 在执行期间持续续期租约，直到 stop 关闭。
// 若续期失败（租约被接管），通过 lost 通知。
func (e *Engine) heartbeat(ctx context.Context, l *lease, stop <-chan struct{}, lost chan<- error) {
	t := time.NewTicker(e.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ok, err := l.acquire(ctx)
			if err != nil {
				select {
				case lost <- err:
				default:
				}
				return
			}
			if !ok {
				select {
				case lost <- ErrLeaseLost:
				default:
				}
				return
			}
		}
	}
}

// withLease 获取租约并在持有期间运行 fn，结束时释放。
func (e *Engine) withLease(ctx context.Context, fn func(ctx context.Context) error) error {
	if err := e.ensureTables(ctx); err != nil {
		return err
	}
	l := newLease(e.db, e.holder, e.cfg.LeaseTTL)
	if err := e.acquireWithRetry(ctx, l); err != nil {
		return err
	}
	stop := make(chan struct{})
	lost := make(chan error, 1)
	go e.heartbeat(ctx, l, stop, lost)
	defer func() {
		close(stop)
		// 释放租约，让等待中的实例尽快接管。
		_ = l.release(context.Background())
	}()

	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	select {
	case err := <-done:
		return err
	case err := <-lost:
		<-done // 等待 fn 退出，避免并发写
		return err
	}
}

// checkPlan 校验待执行迁移的兼容性；除非 AllowDestructive，否则拒绝破坏性变更。
// 注意：只对本次真正要执行的迁移调用，已落库的历史版本不参与判断。
func (e *Engine) checkPlan(migrations []Migration, dir Direction) error {
	if dir != Up || e.cfg.AllowDestructive {
		return nil
	}
	plan := ValidatePlan(migrations)
	if !plan.Compatible() {
		return &CompatibilityError{Violations: plan.Violations}
	}
	return nil
}

// pending 过滤出尚未应用的迁移（保持版本升序）。
func (e *Engine) pending(ctx context.Context, sorted []Migration) ([]Migration, error) {
	var out []Migration
	for _, m := range sorted {
		rec, err := e.recordOf(ctx, m.Version)
		if err != nil {
			return nil, err
		}
		if rec != nil && rec.State == "applied" {
			continue // 已应用，跳过
		}
		out = append(out, m)
	}
	return out, nil
}

// Migrate 按版本顺序应用所有未执行的迁移。
// 已应用的版本自动跳过；上次失败的版本在事务保证下安全重试。
// 兼容性校验只针对本次真正要执行的迁移：历史中已落库的破坏性版本
// 不会反复拦截后续常规兼容的迁移。
//
// 返回的 Outcome 区分两种正常结果：OutcomeExecuted（本次由我执行完成）
// 与 OutcomeYielded（已有其他实例在执行，按让出策略正常返回）。
// 等待策略下超时仍未拿到执行权时返回 ErrLeaseBusy。
func (e *Engine) Migrate(ctx context.Context, migrations []Migration) (Outcome, error) {
	sorted := sortMigrations(migrations)
	err := e.withLease(ctx, func(ctx context.Context) error {
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
		return nil
	})
	return outcomeOf(err)
}

// RollbackTo 按逆序回滚所有版本号大于 target 的已应用迁移。
func (e *Engine) RollbackTo(ctx context.Context, migrations []Migration, target uint) error {
	byVersion := map[uint]Migration{}
	for _, m := range migrations {
		byVersion[m.Version] = m
	}
	return e.withLease(ctx, func(ctx context.Context) error {
		applied, err := e.Applied(ctx)
		if err != nil {
			return err
		}
		// 逆序回滚
		for i := len(applied) - 1; i >= 0; i-- {
			rec := applied[i]
			if rec.Version <= target {
				break
			}
			m, ok := byVersion[rec.Version]
			if !ok {
				return fmt.Errorf("migrate: 缺少版本 %d 的回滚定义: %w", rec.Version, ErrNoMigration)
			}
			if err := e.applyOne(ctx, m, Down); err != nil {
				return err
			}
		}
		return nil
	})
}

// applyOne 在单个事务中执行一个迁移及其簿记，保证要么全部生效、
// 要么完全回滚，不会留下无法判断状态的中间态。
func (e *Engine) applyOne(ctx context.Context, m Migration, dir Direction) error {
	stmts := m.Up
	if dir == Down {
		stmts = m.Down
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
			e.markFailed(m, dir, err)
			return fmt.Errorf("migrate: 版本 %d (%s) %s 失败: %w", m.Version, m.Name, dir, err)
		}
	}
	if dir == Up {
		_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, state, applied_at)
			VALUES (?, ?, 'applied', ?)
			ON CONFLICT(version) DO UPDATE SET state='applied', applied_at=excluded.applied_at`,
			m.Version, m.Name, time.Now().UnixNano())
	} else {
		_, err = tx.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = ?`, m.Version)
	}
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// markFailed 在迁移失败后记录 failed 状态（独立事务），便于运维观测；
// 由于迁移本身在事务中已回滚，重跑时会安全地重试该版本。
func (e *Engine) markFailed(m Migration, dir Direction, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = e.db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, state, applied_at)
		VALUES (?, ?, 'failed', ?)
		ON CONFLICT(version) DO UPDATE SET state='failed', applied_at=excluded.applied_at`,
		m.Version, m.Name, time.Now().UnixNano())
}

func (e *Engine) recordOf(ctx context.Context, version uint) (*AppliedRecord, error) {
	row := e.db.QueryRowContext(ctx,
		`SELECT version, name, state, applied_at FROM schema_migrations WHERE version = ?`, version)
	var rec AppliedRecord
	var state string
	var at int64
	err := row.Scan(&rec.Version, &rec.Name, &state, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec.State = state
	rec.AppliedAt = at
	return &rec, nil
}

// Applied 返回当前已应用（state='applied'）的迁移记录，按版本升序。
func (e *Engine) Applied(ctx context.Context) ([]AppliedRecord, error) {
	if err := e.ensureTables(ctx); err != nil {
		return nil, err
	}
	rows, err := e.db.QueryContext(ctx,
		`SELECT version, name, state, applied_at FROM schema_migrations WHERE state='applied' ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppliedRecord
	for rows.Next() {
		var rec AppliedRecord
		if err := rows.Scan(&rec.Version, &rec.Name, &rec.State, &rec.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func sortMigrations(ms []Migration) []Migration {
	out := make([]Migration, len(ms))
	copy(out, ms)
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}
