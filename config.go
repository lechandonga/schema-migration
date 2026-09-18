package migrator

import "time"

// WaitPolicy 决定多实例争抢执行权时，未抢到租约的实例如何处理。
type WaitPolicy int

const (
	// Wait 表示等待持有租约的实例完成后返回其结果。
	Wait WaitPolicy = iota
	// Skip 表示若已有其他实例在迁移，则立即跳过并返回 Skipped。
	Skip
)

// RetryPolicy 描述单个变更应用失败后的重试策略。
type RetryPolicy struct {
	// MaxAttempts 最大尝试次数（含首次），<=0 表示不重试。
	MaxAttempts    int           `json:"max_attempts"`
	InitialBackoff time.Duration `json:"initial_backoff"`
	MaxBackoff     time.Duration `json:"max_backoff"`
}

// Config 是迁移引擎的可配置参数。
type Config struct {
	// LeaseDuration 租约时长：持有者需在此周期内续租，否则租约失效。
	LeaseDuration time.Duration `json:"lease_duration"`
	// HeartbeatInterval 心跳（续租）间隔，应显著小于 LeaseDuration。
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`
	// OperationTimeout 单个变更应用的超时时间。
	OperationTimeout time.Duration `json:"operation_timeout"`
	// AcquisitionTimeout 等待获取租约的最长时间。
	AcquisitionTimeout time.Duration `json:"acquisition_timeout"`
	// AcquisitionRetry 租约竞争失败后的重试间隔。
	AcquisitionRetry time.Duration `json:"acquisition_retry"`
	// WaitPolicy 未抢到租约时的策略。
	WaitPolicy WaitPolicy `json:"wait_policy"`
	// Retry 变更应用失败的重试策略。
	Retry RetryPolicy `json:"retry"`
}

// DefaultConfig 返回推荐的默认配置。
//
// 推荐取值（适用于秒级完成的常规在线迁移）：
//   - LeaseDuration=30s：实例崩溃后至多 30s 租约即可被接管；
//   - HeartbeatInterval=10s：约为租约时长的 1/3，允许 2 次续租丢失；
//   - OperationTimeout=10s：单个变更的执行上限；
//   - AcquisitionTimeout=5m：等待其他实例完成迁移的上限；
//   - AcquisitionRetry=500ms：租约竞争的轮询间隔；
//   - Retry：最多 3 次，指数退避 200ms 起、上限 5s。
func DefaultConfig() Config {
	return Config{
		LeaseDuration:      30 * time.Second,
		HeartbeatInterval:  10 * time.Second,
		OperationTimeout:   10 * time.Second,
		AcquisitionTimeout: 5 * time.Minute,
		AcquisitionRetry:   500 * time.Millisecond,
		WaitPolicy:         Wait,
		Retry: RetryPolicy{
			MaxAttempts:    3,
			InitialBackoff: 200 * time.Millisecond,
			MaxBackoff:     5 * time.Second,
		},
	}
}

// FastConfig 返回大幅缩短时长的配置，仅供单元测试与演示使用，
// 使租约接管、心跳与等待都在毫秒级完成。
func FastConfig() Config {
	c := DefaultConfig()
	c.LeaseDuration = 200 * time.Millisecond
	c.HeartbeatInterval = 50 * time.Millisecond
	c.OperationTimeout = 2 * time.Second
	c.AcquisitionTimeout = 5 * time.Second
	c.AcquisitionRetry = 10 * time.Millisecond
	c.Retry = RetryPolicy{MaxAttempts: 1}
	return c
}
