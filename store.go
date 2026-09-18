package migrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrNotFound 表示引用的表或列不存在。
var ErrNotFound = errors.New("not found")

// ErrAlreadyExists 表示表或列已存在。
var ErrAlreadyExists = errors.New("already exists")

// ErrInvalidChange 表示变更定义不适用于当前模式。
var ErrInvalidChange = errors.New("invalid schema change")

// Column 描述一列的定义。
type Column struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Nullable   bool   `json:"nullable"`
	HasDefault bool   `json:"has_default"`
	Default    any    `json:"default,omitempty"`
}

// Table 描述一张表及其列（列按声明顺序保存）。
type Table struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
}

// Schema 是某一时刻的完整模式快照。
type Schema struct {
	Tables []Table `json:"tables"`
}

// FaultHook 允许测试向特定变更注入瞬时或永久故障。
// 返回 nil 表示放行；返回错误将使该次变更应用失败。
type FaultHook func(change Change, attempt int) error

// StepJournal 记录一个迁移步骤中已持久化的变更，用于崩溃恢复。
// 每个下标对应的变更一旦记录，即表示该变更已与日志原子提交。
type StepJournal struct {
	Version   Version   `json:"version"`
	Direction Direction `json:"direction"`
	// Applied 已成功应用并持久化的变更下标（按顺序）。
	Applied []int `json:"applied"`
	// Total 该步骤变更总数。
	Total int `json:"total"`
}

const (
	stateFile = "schema_state.json"
	leaseFile = "migration_lease.json"
	lockFile  = ".dir.lock"
)

// persistedState 是状态文件的持久化结构。
type persistedState struct {
	Schema  Schema       `json:"schema"`
	Applied []Version    `json:"applied"`
	Journal *StepJournal `json:"journal,omitempty"`
	// FenceEpoch 记录历史发放过的最大租约 token，保证跨释放/接管单调递增。
	FenceEpoch int64 `json:"fence_epoch"`
}

// Store 是基于本地 JSON 文件 + 文件锁模拟的嵌入式数据库与迁移元数据存储。
// 所有写入均以原子替换持久化：先写临时文件再 rename，崩溃后要么是旧状态、
// 要么是完整的新状态，不会出现无法判断的半截文件。
type Store struct {
	dir string
	mu  sync.RWMutex
	dmu *dirMutex
	// Fault 为可选的故障注入钩子（测试用）。
	Fault FaultHook
}

var (
	openStoresMu sync.Mutex
	openStores   = map[string]*Store{}
)

// OpenStore 打开（不存在则初始化）目录下的模拟数据库。
// 同一目录在同一进程内返回同一个实例，以便多个“实例”并发时共享互斥。
func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("store dir must not be empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	openStoresMu.Lock()
	defer openStoresMu.Unlock()
	if s, ok := openStores[abs]; ok {
		return s, nil
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: abs, dmu: getDirMutex(abs)}
	if err := s.ensureInitialized(); err != nil {
		return nil, err
	}
	openStores[abs] = s
	return s, nil
}

func (s *Store) ensureInitialized() error {
	path := filepath.Join(s.dir, stateFile)
	if _, err := os.Stat(path); err == nil {
		// 校验文件可解析，避免沿用损坏状态。
		var st persistedState
		if err := readJSON(path, &st); err != nil {
			return fmt.Errorf("state file corrupt: %w", err)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.writeState(&persistedState{Applied: []Version{}})
}

// Close 释放底层文件资源（本实现为内存对象，无额外句柄）。
func (s *Store) Close() error { return nil }

func (s *Store) statePath() string { return filepath.Join(s.dir, stateFile) }

func (s *Store) loadState() (*persistedState, error) {
	var st persistedState
	if err := readJSON(s.statePath(), &st); err != nil {
		return nil, err
	}
	if st.Applied == nil {
		st.Applied = []Version{}
	}
	return &st, nil
}

func (s *Store) writeState(st *persistedState) error {
	return writeJSONAtomic(s.statePath(), st)
}

// withState 在目录互斥保护下读取状态执行 fn；fn 返回的新状态仅在 ok=true 时持久化。
func (s *Store) withState(fn func(st *persistedState) (ok bool, err error)) error {
	s.dmu.Lock()
	defer s.dmu.Unlock()
	st, err := s.loadState()
	if err != nil {
		return err
	}
	ok, err := fn(st)
	if err != nil {
		return err
	}
	if ok {
		return s.writeState(st)
	}
	return nil
}

// CurrentSchema 返回当前模式的深拷贝。
func (s *Store) CurrentSchema() Schema {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out Schema
	err := s.withState(func(st *persistedState) (bool, error) {
		out = deepCopySchema(st.Schema)
		return false, nil
	})
	if err != nil {
		return Schema{}
	}
	return out
}

// ApplyChange 针对模拟数据库原子地应用单个模式变更，
// 变更非法（如表不存在）时返回错误且不改变状态。
// 注意：该方法不更新版本/日志，仅供需要直接操作模式的场景使用；
// 迁移执行应通过 ApplyChangeAndJournal，以保证模式变更与进度日志原子提交。
func (s *Store) ApplyChange(ctx context.Context, ch Change) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withState(func(st *persistedState) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := applyChangeToSchema(&st.Schema, ch); err != nil {
			return false, err
		}
		return true, nil
	})
}

// ApplyChangeAndJournal 在同一原子提交中：应用变更并把该变更下标记入步骤日志。
// 二者要么同时生效、要么同时不生效，崩溃恢复时不会出现“改了却没记账”或反之。
func (s *Store) ApplyChangeAndJournal(ch Change, index int, journal *StepJournal) error {
	return s.ApplyChangeAndJournalCtx(context.Background(), ch, index, journal)
}

// ApplyChangeAndJournalCtx 是 ApplyChangeAndJournal 的带上下文版本，
// 在实际修改前检查 ctx（用于 OperationTimeout / 取消）。
func (s *Store) ApplyChangeAndJournalCtx(ctx context.Context, ch Change, index int, journal *StepJournal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withState(func(st *persistedState) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := applyChangeToSchema(&st.Schema, ch); err != nil {
			return false, err
		}
		jc := *journal
		jc.Applied = append(append([]int(nil), journal.Applied...), index)
		st.Journal = &jc
		return true, nil
	})
}

// CompleteStep 原子地结束一个迁移步骤：
// Up 成功后登记版本；Down 成功后移除版本；并清除步骤日志。
// 已应用的版本不会被重复登记。
func (s *Store) CompleteStep(j StepJournal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withState(func(st *persistedState) (bool, error) {
		switch j.Direction {
		case Up:
			if !containsVersion(st.Applied, j.Version) {
				st.Applied = append(st.Applied, j.Version)
			}
		case Down:
			st.Applied = removeVersion(st.Applied, j.Version)
		default:
			return false, fmt.Errorf("%w: unknown direction %d", ErrInvalidChange, j.Direction)
		}
		st.Journal = nil
		return true, nil
	})
}

// AppliedVersions 返回已成功应用的版本，按应用顺序排列。
func (s *Store) AppliedVersions() []Version {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Version
	_ = s.withState(func(st *persistedState) (bool, error) {
		out = append([]Version(nil), st.Applied...)
		return false, nil
	})
	return out
}

// IsApplied 报告某版本是否已应用。
func (s *Store) IsApplied(v Version) bool {
	return containsVersion(s.AppliedVersions(), v)
}

// MarkApplied 将版本登记为已应用（已存在则不重复登记）。
func (s *Store) MarkApplied(v Version) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withState(func(st *persistedState) (bool, error) {
		if !containsVersion(st.Applied, v) {
			st.Applied = append(st.Applied, v)
		}
		return true, nil
	})
}

// MarkRolledBack 将版本从已应用列表移除。
func (s *Store) MarkRolledBack(v Version) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withState(func(st *persistedState) (bool, error) {
		st.Applied = removeVersion(st.Applied, v)
		return true, nil
	})
}

// LoadJournal 读取未完成步骤的日志；不存在时返回 (nil, nil)。
func (s *Store) LoadJournal() (*StepJournal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out *StepJournal
	err := s.withState(func(st *persistedState) (bool, error) {
		out = st.Journal
		if out != nil {
			cp := *out
			cp.Applied = append([]int(nil), out.Applied...)
			out = &cp
		}
		return false, nil
	})
	return out, err
}

// SaveJournal 持久化步骤进度（通常由 ApplyChangeAndJournal 顺带完成，
// 单独提供用于测试或初始化日志）。
func (s *Store) SaveJournal(j *StepJournal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withState(func(st *persistedState) (bool, error) {
		cp := *j
		cp.Applied = append([]int(nil), j.Applied...)
		st.Journal = &cp
		return true, nil
	})
}

// ClearJournal 在步骤完整结束后清除日志。
func (s *Store) ClearJournal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withState(func(st *persistedState) (bool, error) {
		st.Journal = nil
		return true, nil
	})
}

// CurrentFenceEpoch 返回历史发放过的最大租约 token。
func (s *Store) CurrentFenceEpoch() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var epoch int64
	_ = s.withState(func(st *persistedState) (bool, error) {
		epoch = st.FenceEpoch
		return false, nil
	})
	return epoch
}

// BumpFenceEpoch 在不少于 minToken 的前提下分配并持久化下一个单调 token。
func (s *Store) BumpFenceEpoch(minToken int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dmu.Lock()
	defer s.dmu.Unlock()
	return s.bumpEpochLocked(minToken)
}

// bumpEpochLocked 假定调用方已持有目录锁。
func (s *Store) bumpEpochLocked(minToken int64) (int64, error) {
	st, err := s.loadState()
	if err != nil {
		return 0, err
	}
	if minToken > st.FenceEpoch {
		st.FenceEpoch = minToken
	}
	st.FenceEpoch++
	if err := s.writeState(st); err != nil {
		return 0, err
	}
	return st.FenceEpoch, nil
}

// ---- 纯函数式的模式变更模拟 ----

func applyChangeToSchema(sc *Schema, ch Change) error {
	switch ch.Kind {
	case NoOp:
		return nil
	case CreateTable:
		if findTable(sc, ch.Table) != nil {
			return fmt.Errorf("%w: table %q already exists", ErrAlreadyExists, ch.Table)
		}
		cols := append([]Column(nil), ch.Columns...)
		for _, c := range cols {
			if c.Name == "" {
				return fmt.Errorf("%w: column without name in create table %q", ErrInvalidChange, ch.Table)
			}
		}
		sc.Tables = append(sc.Tables, Table{Name: ch.Table, Columns: cols})
	case DropTable:
		t := findTable(sc, ch.Table)
		if t == nil {
			return fmt.Errorf("%w: table %q", ErrNotFound, ch.Table)
		}
		removeTable(sc, ch.Table)
	case AddColumn:
		t := mustFindTable(sc, ch.Table)
		if t == nil {
			return fmt.Errorf("%w: table %q", ErrNotFound, ch.Table)
		}
		if findColumn(t, ch.Column) != nil {
			return fmt.Errorf("%w: column %q.%q", ErrAlreadyExists, ch.Table, ch.Column)
		}
		t.Columns = append(t.Columns, columnFromChange(ch))
	case DropColumn:
		t := mustFindTable(sc, ch.Table)
		if t == nil {
			return fmt.Errorf("%w: table %q", ErrNotFound, ch.Table)
		}
		if findColumn(t, ch.Column) == nil {
			return fmt.Errorf("%w: column %q.%q", ErrNotFound, ch.Table, ch.Column)
		}
		t.Columns = removeColumn(t.Columns, ch.Column)
	case RenameColumn:
		t := mustFindTable(sc, ch.Table)
		if t == nil {
			return fmt.Errorf("%w: table %q", ErrNotFound, ch.Table)
		}
		c := findColumn(t, ch.Column)
		if c == nil {
			return fmt.Errorf("%w: column %q.%q", ErrNotFound, ch.Table, ch.Column)
		}
		if findColumn(t, ch.RenameTo) != nil {
			return fmt.Errorf("%w: column %q.%q", ErrAlreadyExists, ch.Table, ch.RenameTo)
		}
		c.Name = ch.RenameTo
	case AlterColumn:
		t := mustFindTable(sc, ch.Table)
		if t == nil {
			return fmt.Errorf("%w: table %q", ErrNotFound, ch.Table)
		}
		c := findColumn(t, ch.Column)
		if c == nil {
			return fmt.Errorf("%w: column %q.%q", ErrNotFound, ch.Table, ch.Column)
		}
		if ch.Type != "" {
			c.Type = ch.Type
		}
		// HasNullable 语义通过 SetNullable 表达；AlterColumn 仅改类型。
	case SetNullable:
		t := mustFindTable(sc, ch.Table)
		if t == nil {
			return fmt.Errorf("%w: table %q", ErrNotFound, ch.Table)
		}
		c := findColumn(t, ch.Column)
		if c == nil {
			return fmt.Errorf("%w: column %q.%q", ErrNotFound, ch.Table, ch.Column)
		}
		// 加非空约束要求列有默认值（模拟真实数据库对存量行的要求）。
		if !ch.Nullable && !c.HasDefault && !ch.HasDefault {
			return fmt.Errorf("%w: cannot set NOT NULL on %q.%q without default",
				ErrInvalidChange, ch.Table, ch.Column)
		}
		c.Nullable = ch.Nullable
		if ch.HasDefault {
			c.HasDefault = true
			c.Default = ch.Default
		}
	case AddIndex:
		// 模拟实现不单独保存索引，仅校验表存在。
		t := mustFindTable(sc, ch.Table)
		if t == nil {
			return fmt.Errorf("%w: table %q", ErrNotFound, ch.Table)
		}
	default:
		return fmt.Errorf("%w: unknown change kind %q", ErrInvalidChange, ch.Kind)
	}
	return nil
}

func columnFromChange(ch Change) Column {
	return Column{
		Name:       ch.Column,
		Type:       ch.Type,
		Nullable:   ch.Nullable,
		HasDefault: ch.HasDefault,
		Default:    ch.Default,
	}
}

func mustFindTable(sc *Schema, name string) *Table {
	return findTable(sc, name)
}

func findTable(sc *Schema, name string) *Table {
	for i := range sc.Tables {
		if sc.Tables[i].Name == name {
			return &sc.Tables[i]
		}
	}
	return nil
}

func removeTable(sc *Schema, name string) {
	for i := range sc.Tables {
		if sc.Tables[i].Name == name {
			sc.Tables = append(sc.Tables[:i], sc.Tables[i+1:]...)
			return
		}
	}
}

func findColumn(t *Table, name string) *Column {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return &t.Columns[i]
		}
	}
	return nil
}

func removeColumn(cols []Column, name string) []Column {
	for i := range cols {
		if cols[i].Name == name {
			return append(cols[:i], cols[i+1:]...)
		}
	}
	return cols
}

func containsVersion(vs []Version, v Version) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

func removeVersion(vs []Version, v Version) []Version {
	out := vs[:0]
	for _, x := range vs {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func deepCopySchema(sc Schema) Schema {
	out := Schema{Tables: make([]Table, 0, len(sc.Tables))}
	for _, t := range sc.Tables {
		nt := Table{Name: t.Name, Columns: make([]Column, len(t.Columns))}
		copy(nt.Columns, t.Columns)
		out.Tables = append(out.Tables, nt)
	}
	return out
}

// ---- JSON 原子读写 ----

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("empty file: %s", path)
	}
	return unmarshalJSON(data, v)
}

func unmarshalJSON(data []byte, v any) error { return json.Unmarshal(data, v) }

// writeJSONAtomic 先写临时文件、fsync 后 rename，保证原子可见。
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
