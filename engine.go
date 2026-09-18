package migrator

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrPlanRejected 表示计划因破坏性变更被拒绝直接执行。
var ErrPlanRejected = errors.New("migration plan rejected due to incompatible changes")

// ErrUnknownVersion 表示目标版本没有对应的迁移定义。
var ErrUnknownVersion = errors.New("unknown migration version")

// ErrMissingMigration 表示某个已应用版本在当前实例上缺少定义，无法继续。
var ErrMissingMigration = errors.New("missing migration definition")

// ErrInvalidStep 表示发现与当前步骤不匹配的未完成日志（状态异常，拒绝盲覆盖）。
var ErrInvalidStep = errors.New("invalid migration step state")

// Status 表示一次迁移请求的结果状态。
type Status string

const (
	// StatusApplied 表示本实例执行并完成了迁移（含等待后由他人完成）。
	StatusApplied Status = "applied"
	// StatusSkipped 表示其他实例正在迁移，本实例按 Skip 策略跳过。
	StatusSkipped Status = "skipped"
	// StatusNoop 表示目标版本与当前一致，无需迁移。
	StatusNoop Status = "noop"
)

// Result 是一次迁移或回滚请求的结果。
type Result struct {
	Status      Status    `json:"status"`
	FromVersion Version   `json:"from_version"`
	ToVersion   Version   `json:"to_version"`
	Owner       string    `json:"owner,omitempty"`
	Applied     []Version `json:"applied,omitempty"`
	// Recovered 为 true 表示本次运行恢复了上次崩溃遗留的未完成步骤。
	Recovered bool `json:"recovered,omitempty"`
}

// RejectedPlanError 包装兼容性校验拒绝详情。
type RejectedPlanError struct {
	Violations []Violation
	Staged     []StagedMigration
}

func (e *RejectedPlanError) Error() string {
	return fmt.Sprintf("%s: %d destructive change(s)", ErrPlanRejected, len(e.Violations))
}

func (e *RejectedPlanError) Unwrap() error { return ErrPlanRejected }

// Engine 是模式迁移引擎，编排计划、租约、执行与恢复。
type Engine struct {
	store      *Store
	lease      *LeaseManager
	migrations map[Version]*Migration
	cfg        Config
	owner      string
	// fault 为引擎实例私有的故障注入钩子；为空时回退到 store.Fault。
	fault FaultHook
	// heartbeatStopper 返回一个通道：通道关闭后立即停止心跳（用于模拟崩溃）。
	heartbeatStopper func() <-chan struct{}
}

// NewEngine 创建引擎；migrations 为所有已知迁移定义。
func NewEngine(store *Store, migrations []Migration, owner string, cfg Config) *Engine {
	e := &Engine{
		store:      store,
		migrations: map[Version]*Migration{},
		cfg:        cfg,
		owner:      owner,
	}
	e.lease = NewLeaseManager(store, owner, cfg)
	e.RegisterMigrations(migrations)
	return e
}

// SetFaultHook 为该实例设置私有的故障注入钩子（测试用）。
func (e *Engine) SetFaultHook(h FaultHook) { e.fault = h }

// SetHeartbeatStopper 设置一个返回停止信号的钩子；信号触发后该实例停止续租，
// 使租约按 LeaseDuration 到期，从而模拟持有者崩溃后被接管。
func (e *Engine) SetHeartbeatStopper(fn func() <-chan struct{}) { e.heartbeatStopper = fn }

// hookForChange 返回当前生效的故障钩子。
func (e *Engine) hookForChange() FaultHook {
	if e.fault != nil {
		return e.fault
	}
	return e.store.Fault
}

// RegisterMigrations 追加注册迁移定义（主要用于测试）。
func (e *Engine) RegisterMigrations(ms []Migration) {
	for i := range ms {
		m := ms[i]
		e.migrations[m.Version] = &m
	}
}

// Owner 返回引擎实例标识。
func (e *Engine) Owner() string { return e.owner }

// Migrate 按版本顺序将模式升级到 target。
// 已应用版本不重复执行；存在破坏性变更且会影响旧实例时返回
// *RejectedPlanError（可通过 errors.As 取出拒绝原因与分阶段路径）；
// 多实例并发时仅一个实例执行，其余按 WaitPolicy 等待或跳过。
// 若上次执行在某版本中途崩溃，本次先从中断处恢复再继续，已应用变更不重复。
func (e *Engine) Migrate(ctx context.Context, target Version) (Result, error) {
	from := highestVersion(e.store.AppliedVersions())

	plan, err := PlanUp(e.store.AppliedVersions(), e.migrations, target)
	if err != nil {
		return Result{FromVersion: from}, err
	}
	if len(plan.Steps) == 0 {
		// 可能存在中断的日志但所有版本已登记（极端边界），清理孤儿日志。
		if j, _ := e.store.LoadJournal(); j != nil {
			_ = e.store.ClearJournal()
		}
		return Result{Status: StatusNoop, FromVersion: from, ToVersion: plan.Target}, nil
	}

	// 兼容性校验在争抢租约之前进行：破坏性计划直接拒绝并给出可区分原因。
	if rep := CheckCompatibility(plan); !rep.Compatible {
		return Result{FromVersion: from, ToVersion: plan.Target},
			&RejectedPlanError{Violations: rep.Violations, Staged: rep.Staged}
	}

	return e.runWithLease(ctx, plan, from)
}

// Rollback 按应用逆序回滚，直到当前版本到达 target（target 保持已应用）。
// 回滚同样受租约保护并支持中断恢复；回滚不做“破坏性”校验（Down 本就是撤销）。
func (e *Engine) Rollback(ctx context.Context, target Version) (Result, error) {
	from := highestVersion(e.store.AppliedVersions())
	plan, err := PlanDown(e.store.AppliedVersions(), e.migrations, target)
	if err != nil {
		return Result{FromVersion: from}, err
	}
	if len(plan.Steps) == 0 {
		if j, _ := e.store.LoadJournal(); j != nil {
			_ = e.store.ClearJournal()
		}
		return Result{Status: StatusNoop, FromVersion: from, ToVersion: target}, nil
	}
	return e.runWithLease(ctx, plan, from)
}

// MigrateStaged 按 expand/contract 分阶段路径升级到 target。
// 它先做兼容性校验；若无破坏性变更则等价于普通 Migrate。
// 若存在破坏性变更，则用 StagedMigration 序列展开计划后执行。
// 注意：expand 与 contract 阶段之间通常需要灰度发布与切流，
// 调用方可通过 target 指向某个阶段版本来分步上线。
func (e *Engine) MigrateStaged(ctx context.Context, target Version) (Result, []StagedMigration, error) {
	from := highestVersion(e.store.AppliedVersions())
	plan, err := PlanUp(e.store.AppliedVersions(), e.migrations, target)
	if err != nil {
		return Result{FromVersion: from}, nil, err
	}
	rep := CheckCompatibility(plan)
	if rep.Compatible {
		if len(plan.Steps) == 0 {
			return Result{Status: StatusNoop, FromVersion: from, ToVersion: plan.Target}, nil, nil
		}
		res, err := e.runWithLease(ctx, plan, from)
		return res, nil, err
	}
	// 注册生成的阶段迁移定义，供执行与崩溃恢复查找；随后直接执行展开计划，
	// 因为阶段版本（"x+expand"）与基础版本在语义化比较中相等，不再走 PlanUp。
	for _, sm := range rep.Staged {
		e.RegisterMigrations(sm.Phases)
	}
	expanded := ExpandStagedPlan(plan, rep.Staged)
	if len(expanded.Steps) == 0 {
		return Result{Status: StatusNoop, FromVersion: from, ToVersion: plan.Target}, rep.Staged, nil
	}
	res, err := e.runWithLease(ctx, expanded, from)
	if err != nil {
		return res, rep.Staged, err
	}
	return res, rep.Staged, nil
}

// runWithLease 负责租约竞争：持有者执行计划，其余实例等待或跳过。
func (e *Engine) runWithLease(ctx context.Context, plan Plan, from Version) (Result, error) {
	deadline := time.Now().Add(e.cfg.AcquisitionTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return Result{FromVersion: from}, err
		}
		l, err := e.lease.Acquire(ctx)
		if err == nil {
			return e.executeAsOwner(ctx, plan, from, l.FenceToken)
		}
		if !errors.Is(err, ErrLeaseHeld) {
			return Result{FromVersion: from}, err
		}
		// 被其他活跃实例抢占。
		switch e.cfg.WaitPolicy {
		case Wait:
			// 轮询：租约释放说明对方完成；对方崩溃则租约到期后由我们接管执行。
			if time.Now().After(deadline) {
				return Result{FromVersion: from, Status: StatusSkipped},
					fmt.Errorf("acquire migration lease timed out after %s", e.cfg.AcquisitionTimeout)
			}
			if err := sleep(ctx, e.cfg.AcquisitionRetry); err != nil {
				return Result{FromVersion: from}, err
			}
			// 若对方已完成全部步骤，则无需再执行。
			if cur := e.store.AppliedVersions(); planAllDone(plan, cur) {
				return Result{
					Status: StatusApplied, FromVersion: from, ToVersion: plan.Target,
					Applied: planVersionList(plan),
				}, nil
			}
		default: // Skip
			return Result{Status: StatusSkipped, FromVersion: from, ToVersion: plan.Target}, nil
		}
	}
}

// executeAsOwner 由持有租约的实例执行整个计划，包含心跳续租与逐版本恢复。
func (e *Engine) executeAsOwner(ctx context.Context, plan Plan, from Version, token int64) (Result, error) {
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	hbErr := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		e.heartbeat(heartbeatCtx, token, hbErr)
	}()
	// leaseLost 在整个执行期（含恢复）只上报一次。
	leaseLost := func() error {
		select {
		case err := <-hbErr:
			if err != nil {
				return err
			}
			return ErrLeaseLost
		default:
			return nil
		}
	}

	res := Result{Status: StatusApplied, FromVersion: from, ToVersion: plan.Target, Owner: e.owner}

	// 若存在上次崩溃遗留的步骤日志，先恢复其所属版本的执行。
	recovered, err := e.recoverPending(ctx, token, hbErr)
	if err != nil {
		e.safeRelease(token)
		return res, err
	}
	res.Recovered = recovered

	for _, step := range plan.Steps {
		if err := leaseLost(); err != nil {
			e.safeRelease(token)
			return res, err
		}
		// 版本可能已在恢复或前序执行中登记，严格保证不重复。
		if step.Direction == Up && e.store.IsApplied(step.Migration.Version) {
			continue
		}
		// 回滚时若版本已不在 applied（例如恢复路径完成了一个 Down 步骤），也跳过。
		if step.Direction == Down && !e.store.IsApplied(step.Migration.Version) {
			continue
		}
		if err := e.runStep(ctx, step, token, hbErr); err != nil {
			// 崩溃恢复的关键约定：失败时保留日志，状态停留在“已记录的前缀”，
			// 不会出现无法判断的中间态；下次运行从日志续跑。
			e.safeRelease(token)
			return res, err
		}
		res.Applied = append(res.Applied, step.Migration.Version)
	}

	stopHeartbeat()
	<-heartbeatDone
	if err := e.lease.Release(token); err != nil && !errors.Is(err, ErrLeaseLost) {
		return res, err
	}
	return res, nil
}

// heartbeat 周期性续租；一旦发现租约丢失或收到停止信号则停止。
func (e *Engine) heartbeat(ctx context.Context, token int64, hbErr chan<- error) {
	interval := e.cfg.HeartbeatInterval
	if interval <= 0 {
		interval = e.cfg.LeaseDuration / 3
	}
	var stopSig <-chan struct{}
	if e.heartbeatStopper != nil {
		stopSig = e.heartbeatStopper()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopSig:
			// 模拟崩溃：停止续租但不报错（工作线程可能仍僵在某一步）。
			return
		case <-t.C:
			if err := e.lease.Renew(token); err != nil {
				select {
				case hbErr <- err:
				default:
				}
				return
			}
		}
	}
}

// runStep 执行单个迁移步骤（一个版本的 Up 或 Down 变更序列）。
// 幂等保证：开始时若无日志则建立日志；每个变更成功后与日志原子提交；
// 已有日志时跳过其记录过的下标，确保中断重跑不重复应用。
func (e *Engine) runStep(ctx context.Context, step PlannedMigration, token int64, hbErr <-chan error) error {
	changes := step.Migration.Up
	if step.Direction == Down {
		changes = step.Migration.Down
	}
	j, err := e.store.LoadJournal()
	if err != nil {
		return err
	}
	if j != nil && (j.Version != step.Migration.Version || j.Direction != step.Direction) {
		// 存在属于其他步骤的未完成日志：绝不能覆盖（否则丢失崩溃现场）。
		// 正常流程会先经 recoverPending 消费它；走到这里说明状态异常，直接报错。
		return fmt.Errorf("%w: pending journal for %s/%d while running %s/%d",
			ErrInvalidStep, j.Version, j.Direction, step.Migration.Version, step.Direction)
	}
	if j == nil {
		// 全新步骤（正常情况下旧日志已在恢复路径被消费）。
		j = &StepJournal{Version: step.Migration.Version, Direction: step.Direction, Total: len(changes)}
		if err := e.store.SaveJournal(j); err != nil {
			return err
		}
	}

	done := make(map[int]bool, len(j.Applied))
	for _, idx := range j.Applied {
		done[idx] = true
	}

	for i, ch := range changes {
		if done[i] {
			continue // 崩溃前已提交，绝不重复应用
		}
		if err := e.applyWithRetry(ctx, ch, token, hbErr); err != nil {
			return err
		}
		// 真正的模式变更受 OperationTimeout 约束；与日志原子提交。
		opCtx := ctx
		cancel := func() {}
		if e.cfg.OperationTimeout > 0 {
			opCtx, cancel = context.WithTimeout(ctx, e.cfg.OperationTimeout)
		}
		err := e.store.ApplyChangeAndJournalCtx(opCtx, ch, i, j)
		cancel()
		if err != nil {
			return err
		}
		j.Applied = append(j.Applied, i)
	}
	// 全部变更提交后，原子登记/移除版本并清除日志。
	return e.store.CompleteStep(StepJournal{
		Version:   step.Migration.Version,
		Direction: step.Direction,
	})
}

// applyWithRetry 应用单个变更，处理超时、心跳丢失与可重试错误退避。
func (e *Engine) applyWithRetry(ctx context.Context, ch Change, token int64, hbErr <-chan error) error {
	maxAttempts := e.cfg.Retry.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	backoff := e.cfg.Retry.InitialBackoff
	if backoff <= 0 {
		backoff = time.Millisecond
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// 续租失败意味着执行权可能已被接管，立即停手，避免双写。
		select {
		case err := <-hbErr:
			if err != nil {
				return err
			}
			return ErrLeaseLost
		default:
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = nil
		if hook := e.hookForChange(); hook != nil {
			if err := hook(ch, attempt); err != nil {
				lastErr = err
			}
		}
		if lastErr == nil {
			return nil
		}
		if attempt == maxAttempts {
			break
		}
		if err := sleep(ctx, backoff); err != nil {
			return err
		}
		backoff *= 2
		if e.cfg.Retry.MaxBackoff > 0 && backoff > e.cfg.Retry.MaxBackoff {
			backoff = e.cfg.Retry.MaxBackoff
		}
	}
	return fmt.Errorf("applying %s on %s failed after %d attempts: %w",
		ch.Kind, ch.Table, maxAttempts, lastErr)
}

// recoverPending 检查并恢复上次崩溃遗留的未完成步骤。
// 若日志属于计划之外的版本（如升级中断后直接调用回滚），仍能安全续跑该步骤。
func (e *Engine) recoverPending(ctx context.Context, token int64, hbErr chan error) (bool, error) {
	j, err := e.store.LoadJournal()
	if err != nil {
		return false, err
	}
	if j == nil {
		return false, nil
	}
	m, ok := e.migrations[j.Version]
	if !ok {
		return false, fmt.Errorf("%w: cannot resume %s", ErrMissingMigration, j.Version)
	}
	// 恢复期间沿用主心跳通道，受同一租约保护；runStep 依据日志跳过已提交下标。
	if err := e.runStep(ctx, PlannedMigration{Migration: m, Direction: j.Direction}, token, hbErr); err != nil {
		return true, err
	}
	return true, nil
}

func (e *Engine) safeRelease(token int64) { _ = e.lease.Release(token) }

// ExpandStagedPlan 用 CheckCompatibility 产出的分阶段迁移替换计划中的破坏性步骤。
// 调用方应在实际需要按 expand/contract 上线时使用；阶段之间的灰度/切流由部署流程控制。
// 阶段版本（如 "2.0.0+expand"）同样登记在已应用列表中，保证不重复执行。
func ExpandStagedPlan(plan Plan, staged []StagedMigration) Plan {
	bySource := map[Version][]Migration{}
	for _, sm := range staged {
		bySource[sm.Source] = sm.Phases
	}
	out := Plan{Current: plan.Current, Target: plan.Target}
	for _, step := range plan.Steps {
		if step.Direction == Up {
			if phases, ok := bySource[step.Migration.Version]; ok {
				for i := range phases {
					p := phases[i]
					out.Steps = append(out.Steps, PlannedMigration{Migration: &p, Direction: Up})
				}
				continue
			}
		}
		out.Steps = append(out.Steps, step)
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func planAllDone(plan Plan, applied []Version) bool {
	for _, s := range plan.Steps {
		if s.Direction == Up {
			if !containsVersion(applied, s.Migration.Version) {
				return false
			}
		} else {
			if containsVersion(applied, s.Migration.Version) {
				return false
			}
		}
	}
	return true
}

func planVersionList(plan Plan) []Version {
	out := make([]Version, 0, len(plan.Steps))
	for _, s := range plan.Steps {
		out = append(out, s.Migration.Version)
	}
	return out
}
