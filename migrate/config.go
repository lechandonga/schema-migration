package migrate

import "time"

// Config 控制迁移引擎的超时、租约与重试策略。
type Config struct {
	// LeaseTTL 为执行租约的有效期；持有者崩溃后租约在该时长后自动失效。
	LeaseTTL time.Duration
	// HeartbeatInterval 为租约续期心跳间隔，应显著小于 LeaseTTL。
	HeartbeatInterval time.Duration
	// AcquireTimeout 为等待获取执行权的最长时间，超时后返回 ErrLeaseBusy。
	AcquireTimeout time.Duration
	// RetryInterval 为获取租约失败后的重试间隔。
	RetryInterval time.Duration
	// StatementTimeout 为单条迁移语句的执行超时。
	StatementTimeout time.Duration
	// AllowDestructive 允许执行包含破坏性变更的迁移计划。
	// 仅在已按分阶段路径完成前置阶段后开启。
	AllowDestructive bool
}

// DefaultConfig 返回推荐配置。
func DefaultConfig() Config {
	return Config{
		LeaseTTL:          30 * time.Second,
		HeartbeatInterval: 5 * time.Second,
		AcquireTimeout:    2 * time.Minute,
		RetryInterval:     500 * time.Millisecond,
		StatementTimeout:  5 * time.Minute,
	}
}
