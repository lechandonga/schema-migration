# 测试与验证方法

## 运行

```bash
go test ./...                       # 全部用例
go test -race ./...                 # 竞态检测（需 CGO_ENABLED=1）
go test -v -run TestConcurrent ./...  # 只跑并发相关
go vet ./...
go run ./cmd/migrator-demo -dir /tmp/demo   # 端到端手动演示
```

## 故障注入机制

- `Store.Fault`：存储级钩子，对每个变更调用，返回错误即视为该次应用失败。
- `Engine.SetFaultHook`：实例私有钩子，优先级高于 `Store.Fault`，可让不同实例表现不同。
- `Engine.SetHeartbeatStopper`：返回一个停止信号通道；信号触发后该实例**停止续租**，
  用来精确模拟“持有者进程崩溃”（工作线程可同时阻塞在某个变更上）。

钩子签名：`func(change Change, attempt int) error`，`attempt` 从 1 开始，可据此实现
“前两次失败、第三次成功”的瞬时故障，或“永久失败”。

## 覆盖场景与对应用例

### 基础正确性

| 用例 | 验证点 |
| --- | --- |
| `TestStoreBasicChanges` | 建表/加列/删列语义、重复表、无默认值加 NOT NULL 被拒 |
| `TestStoreJournalAtomicity` | 变更与日志原子提交、步骤完成登记版本、重开持久化 |
| `TestMigrateHappyPath` | 版本顺序执行、重跑为 no-op 且不重复 |
| `TestMigrateToSpecificVersion` | 升级到指定目标版本 |
| `TestRollbackReverseOrder` | 严格逆序回滚、目标版本保留、回滚到空库 |

### 破坏性变更拒绝与分阶段路径

| 用例 | 验证点 |
| --- | --- |
| `TestCheckCompatibility_AdditiveIsOK` | 纯新增/放宽约束判为兼容 |
| `TestCheckCompatibility_DropColumnStaged` | 删列拒绝（`drop_column`）+ 两阶段 |
| `TestCheckCompatibility_RenameStaged` | 重命名拒绝 + expand/cutover/contract 三阶段顺序 |
| `TestCheckCompatibility_AlterTypeStaged` | 改类型拒绝 + 影子列三阶段 |
| `TestCheckCompatibility_NotNullStaged` | 无默认值加非空拒绝 + 两阶段 |
| `TestCheckCompatibility_DropTableNoSafePath` | 删表拒绝且**不提供**安全路径 |
| `TestMigrateRejectsDestructive` | `errors.As`/`errors.Is` 可区分拒绝原因，被拒迁移不落库 |
| `TestMigrateStagedDropColumn` | 分阶段计划真实执行成功、阶段版本登记、旧列最终删除 |

### 并发互斥与崩溃接管

| 用例 | 验证点 |
| --- | --- |
| `TestLeaseMutexAndTakeover` | 互斥获取、续租、过期接管、fencing token 单调、旧持有者续租失败 |
| `TestConcurrent_SkipPolicy` | 并发下恰一个执行，另一个 `skipped`，最终版本完整 |
| `TestConcurrent_WaitPolicyConverges` | 三个实例并发，等待者全部收敛，且只有一个真正执行者 |
| `TestConcurrent_CrashTakeover` | 持有者停止心跳“崩溃”，等待者在租约到期后接管并独立完成；表只创建一次 |

### 中断恢复与重试

| 用例 | 验证点 |
| --- | --- |
| `TestResumeAfterCrash` | 中途崩溃后重启：只续跑未提交变更、已提交变更不重复、`Recovered=true`、最终一致 |
| `TestResumeIdempotentChangeTracking` | 成功路径下每个变更恰好执行一次 |
| `TestRetryTransientFailure` | 瞬时失败按指数退避重试，第三次成功 |
| `TestRetryExhaustedLeavesJournal` | 重试耗尽保留已提交前缀日志、版本不误标为已应用 |
| `TestRollbackResumeAfterCrash` | 回滚中断后重启，从 Down 序列断点继续，最终到空库并清日志 |

## 如何手动验证一个新场景

1. 用 `t.TempDir()` 打开 `Store`，注册迁移定义；
2. 需要并发时，对同一目录 `OpenStore` 多次（同进程返回同一实例）并创建多个 `Engine`；
3. 用通道/计数器在 `SetFaultHook` 中制造你关心的失败时序；
4. 断言三类“事实”：
   - `store.AppliedVersions()`（版本集合与顺序）；
   - `store.CurrentSchema()`（实际表/列结构，验证没有重复 create 等）；
   - `store.LoadJournal()`（中断现场的已提交前缀，成功后应为空）。
