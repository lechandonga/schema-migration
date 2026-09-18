# 配置说明

所有参数集中在 `migrator.Config` 中，可用 `migrator.DefaultConfig()` 获取推荐值，
用 `migrator.FastConfig()` 获取仅供单元测试/演示使用的毫秒级配置。

```go
type Config struct {
    LeaseDuration      time.Duration // 租约时长
    HeartbeatInterval  time.Duration // 心跳（续租）间隔
    OperationTimeout   time.Duration // 单个变更的执行超时
    AcquisitionTimeout time.Duration // 等待获取租约的最长时间
    AcquisitionRetry   time.Duration // 租约竞争失败后的重试/轮询间隔
    WaitPolicy         WaitPolicy    // 未抢到租约时：Wait 或 Skip
    Retry              RetryPolicy   // 变更应用失败的重试策略
}
```

## 参数详解与推荐取值

| 参数 | 推荐值 | 说明 | 调小的影响 | 调大的影响 |
| --- | --- | --- | --- | --- |
| `LeaseDuration` | **30s** | 租约有效期。持有者必须在此周期内续租，否则租约失效可被接管。它决定**崩溃恢复时间上界**：持有者死亡后，其他实例最多等待约一个租约周期即可接管。 | 接管更快，但心跳抖动/GC/网络抖动更容易误判持有者死亡 | 崩溃后接管更慢，迁移窗口拉长 |
| `HeartbeatInterval` | **10s** | 续租间隔，建议约为 `LeaseDuration` 的 1/3，留出至少 2 次续租丢失的容忍度。 | CPU/IO 开销略增 | 租约可能在两次心跳间过期 |
| `OperationTimeout` | **10s** | 单个变更（一次 DDL）的执行上限，超时即判失败并按重试策略处理。 | 慢 DDL 易被误杀 | 卡死的变更会更久地占用租约 |
| `AcquisitionTimeout` | **5m** | 选择 `Wait` 策略时，等待其他实例完成/接管的最长时间；超时返回错误（状态为 `skipped`）。必须显著大于“一次完整迁移的预期耗时”。 | 大迁移期间等待者容易超时报错 | 调用方阻塞更久 |
| `AcquisitionRetry` | **500ms** | 租约竞争的轮询间隔，也是等待者发现“对方已完成”的延迟上界。 | 空转 CPU 增加、锁竞争更激烈 | 发现完成/接管的延迟变大 |
| `WaitPolicy` | **Wait** | `Wait`：等待持有者完成后收敛返回；`Skip`：已有实例在迁移时立即返回 `StatusSkipped`。 | — | — |
| `Retry.MaxAttempts` | **3** | 单个变更的最大尝试次数（含首次）。仅对注入/返回的瞬时错误重试；`<=0` 表示不重试。 | 瞬时故障直接失败 | 永久故障会被重复尝试更久 |
| `Retry.InitialBackoff` | **200ms** | 首次退避时长，之后指数翻倍。 | 重试更激进，可能放大故障期压力 | 恢复变慢 |
| `Retry.MaxBackoff` | **5s** | 退避上限，防止指数增长失控。 | — | 故障恢复更慢 |

## 取值原则

1. **`HeartbeatInterval` ≈ `LeaseDuration / 3`**：容忍偶发的一次续租失败而不丢租约。
2. **`LeaseDuration` 按“可接受的崩溃恢复时间”选择**：30s 适合在线服务；
   对分钟级大迁移可上调到 60–120s，但崩溃接管也随之变慢。
3. **`AcquisitionTimeout` ≥ 全量迁移预估耗时 + 一个租约周期**，否则等待者会在迁移正常进行时超时。
4. **重试只用于幂等的瞬时错误**：本引擎的变更是“先记账后生效”的原子提交，
   未确认生效的变更可以安全重试；确认失败会保留在步骤日志中，由下次运行恢复。
5. 单测/演示用 `FastConfig()`（租约 200ms、心跳 50ms），**不要用于生产**。

## Wait 与 Skip 的选择

- **Wait（默认，推荐）**：所有实例最终对“当前版本”达成一致，调用方能在返回后确定模式已就绪。
  适合启动时阻塞等待迁移完成的场景。
- **Skip**：后启动的实例发现已有实例负责迁移即立即返回，适合“宁可不迁移也不要排队阻塞”的场景；
  调用方需自行处理 `StatusSkipped`（稍后重试或只读运行）。

无论哪种策略，真正执行迁移的实例**全局只有一个**（由租约保证）。
