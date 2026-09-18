package migrate

import (
	"context"
	"database/sql"
	"time"
)

// lease 实现基于数据库表的单实例执行租约。
// 租约表只有一行（id=1），持有者为 holder，过期时间为 expires_at。
// 持有者崩溃后，租约在 TTL 到期后自动失效，其他实例可接管。
type lease struct {
	db     *sql.DB
	holder string
	ttl    time.Duration
}

func newLease(db *sql.DB, holder string, ttl time.Duration) *lease {
	return &lease{db: db, holder: holder, ttl: ttl}
}

// ensureTable 创建租约表（幂等）。
func (l *lease) ensureTable(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migration_lock (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		holder TEXT NOT NULL,
		expires_at INTEGER NOT NULL
	)`)
	return err
}

// acquire 尝试获取租约；已持有则续期。返回是否持有。
// 使用单条原子 UPSERT，保证多实例并发时只有一个能成功。
func (l *lease) acquire(ctx context.Context) (bool, error) {
	if err := l.ensureTable(ctx); err != nil {
		return false, err
	}
	now := time.Now()
	expires := now.Add(l.ttl).UnixNano()
	// 仅当记录不存在、已过期或本就由本实例持有时才写入。
	res, err := l.db.ExecContext(ctx, `INSERT INTO schema_migration_lock (id, holder, expires_at)
		VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET holder = excluded.holder, expires_at = excluded.expires_at
		WHERE schema_migration_lock.expires_at < ? OR schema_migration_lock.holder = excluded.holder`,
		l.holder, expires, now.UnixNano())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// release 释放租约；只有持有者本人才能释放。
func (l *lease) release(ctx context.Context) error {
	if err := l.ensureTable(ctx); err != nil {
		return err
	}
	_, err := l.db.ExecContext(ctx,
		`DELETE FROM schema_migration_lock WHERE id = 1 AND holder = ?`, l.holder)
	return err
}
