package migrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ErrLeaseHeld 表示租约正由其他活跃实例持有，当前无法获取。
var ErrLeaseHeld = errors.New("migration lease is held by another instance")

// ErrLeaseLost 表示续租时发现租约已失效或被接管。
var ErrLeaseLost = errors.New("migration lease has been lost")

// ErrNotLeaseOwner 表示租约不属于当前实例，无权执行该操作。
var ErrNotLeaseOwner = errors.New("not the lease owner")

// Lease 记录迁移执行租约的持久化状态。
type Lease struct {
	Owner      string    `json:"owner"`
	FenceToken int64     `json:"fence_token"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// LeaseManager 负责跨实例的迁移执行权协调。
//
// 互斥规则：
//   - 租约持久化在数据目录的 migration_lease.json，读改写由目录锁串行化，
//     写文件采用原子替换；跨进程再叠加目录 flock 防止并发踩踏；
//   - 租约带过期时间：持有者必须周期性 Renew 续租。持有者崩溃后心跳停止，
//     租约到期即自动失效，其他实例可接管；
//   - 每次接管都使单调递增的 FenceToken +1，防止“假死”后复活的旧持有者
//     继续写入（其续租会因 token 不匹配而收到 ErrLeaseLost）。
type LeaseManager struct {
	dir   string
	store *Store
	owner string
	cfg   Config
}

// NewLeaseManager 创建租约管理器。
func NewLeaseManager(store *Store, owner string, cfg Config) *LeaseManager {
	return &LeaseManager{dir: store.dir, store: store, owner: owner, cfg: cfg}
}

func (m *LeaseManager) path() string { return filepath.Join(m.dir, leaseFile) }

// CurrentLease 返回租约文件当前内容；不存在时返回 (nil, nil)。
func (m *LeaseManager) CurrentLease() (*Lease, error) {
	m.store.dmu.Lock()
	defer m.store.dmu.Unlock()
	return m.readLeaseLocked()
}

func (m *LeaseManager) readLeaseLocked() (*Lease, error) {
	data, err := os.ReadFile(m.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var l Lease
	if err := unmarshalJSON(data, &l); err != nil {
		// 租约文件损坏时按“无租约”处理，避免永久阻塞后续实例。
		return nil, nil
	}
	return &l, nil
}

// Acquire 尝试获取租约；若被活跃持有者占用则返回 ErrLeaseHeld。
// 已过期的租约会被接管并换发新的 FenceToken（单调 +1）。
// 同一实例重复获取自己持有的有效租约等同于续租。
func (m *LeaseManager) Acquire(ctx context.Context) (*Lease, error) {
	// 跨进程串行化对目录/文件的并发探测。
	lf, err := flockDir(m.dir)
	if err != nil {
		return nil, err
	}
	if lf != nil {
		defer func() { _ = lf.Close() }()
		if ok, err := tryFLock(lf); err != nil {
			return nil, err
		} else if !ok {
			return nil, ErrLeaseHeld
		}
	}

	m.store.dmu.Lock()
	defer m.store.dmu.Unlock()

	now := time.Now()
	cur, err := m.readLeaseLocked()
	if err != nil {
		return nil, err
	}
	switch {
	case cur == nil || !cur.ExpiresAt.After(now):
		// 无租约或租约已过期：（接管并）换发新 token。
		// token 取自状态文件中的单调高水位，保证租约释放/接管后永不回退。
		minToken := int64(1)
		if cur != nil && cur.FenceToken+1 > minToken {
			minToken = cur.FenceToken + 1
		}
		token, err := m.store.bumpEpochLocked(minToken)
		if err != nil {
			return nil, err
		}
		l := &Lease{
			Owner:      m.owner,
			FenceToken: token,
			AcquiredAt: now,
			ExpiresAt:  now.Add(m.cfg.LeaseDuration),
		}
		if err := writeJSONAtomic(m.path(), l); err != nil {
			return nil, err
		}
		return l, nil
	case cur.Owner == m.owner:
		// 自己仍持有：幂等续租，token 不变。
		cur.ExpiresAt = now.Add(m.cfg.LeaseDuration)
		if err := writeJSONAtomic(m.path(), cur); err != nil {
			return nil, err
		}
		return cur, nil
	default:
		return nil, ErrLeaseHeld
	}
}

// Renew 续租并校验 FenceToken；租约失效或被接管时返回 ErrLeaseLost。
func (m *LeaseManager) Renew(token int64) error {
	m.store.dmu.Lock()
	defer m.store.dmu.Unlock()
	cur, err := m.readLeaseLocked()
	if err != nil {
		return err
	}
	if cur == nil || cur.Owner != m.owner || cur.FenceToken != token {
		return ErrLeaseLost
	}
	cur.ExpiresAt = time.Now().Add(m.cfg.LeaseDuration)
	return writeJSONAtomic(m.path(), cur)
}

// Release 释放租约（仅持有者本人、且 token 匹配时可释放）。
// 租约已不存在或已过期视为成功；若租约已被别的 token 接管则返回 ErrLeaseLost。
func (m *LeaseManager) Release(token int64) error {
	lf, err := flockDir(m.dir)
	if err != nil {
		return err
	}
	if lf != nil {
		defer func() { _ = lf.Close() }()
		// 释放时不要求拿到排他 flock：我们仍以租约内容为准。
		_ = lf
	}
	m.store.dmu.Lock()
	defer m.store.dmu.Unlock()
	cur, err := m.readLeaseLocked()
	if err != nil {
		return err
	}
	switch {
	case cur == nil:
		return nil
	case cur.Owner == m.owner && cur.FenceToken == token:
		return os.Remove(m.path())
	case cur.Owner == m.owner || !cur.ExpiresAt.After(time.Now()):
		// 自己的旧 token 或租约已过期：接管已发生/将发生，清除即可。
		return os.Remove(m.path())
	default:
		return ErrLeaseLost
	}
}
