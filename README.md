# schema-migration

支持在线滚动迁移与兼容性校验的数据库模式迁移引擎。使用本地嵌入式
SQLite（`modernc.org/sqlite`，纯 Go 实现）模拟真实环境中的表结构变更与
多实例并发执行。

## 功能概览

- **按版本顺序执行**：每个迁移包含 `Up`（升级）与 `Down`（回滚）定义，
  已执行的版本自动跳过，回滚按逆序进行并恢复到指定目标版本。
- **兼容性校验**：`ValidatePlan` 识别删列、删表、改类型、加非空约束、
  重命名列等破坏性变更；`Engine.Migrate` 默认拒绝含破坏性变更的计划，
  返回可区分的 `*CompatibilityError`（含每处违规的类别、原因与分阶段路径）。
- **多实例编排**：基于数据库表的租约保证同一时刻只有一个实例执行迁移，
  其余实例等待或在超时后收到 `ErrLeaseBusy`；持有者崩溃后租约自动过期，
  其他实例可接管。
- **可恢复执行**：每个迁移与其簿记在单个事务中提交，中断后重跑会从中断
  版本继续，已应用部分不会重复应用，也不会留下无法判断状态的中间态。
- **可配置策略**：租约 TTL、心跳、获取超时、重试间隔、语句超时均可配置。

## 快速开始

```go
db, _ := sql.Open("sqlite", "file:app.db?_pragma=busy_timeout(5000)")
eng := migrate.New(db, migrate.DefaultConfig(), "host-1/pid-1234")

migrations := []migrate.Migration{
    {Version: 1, Name: "create_users",
        Up:   []string{"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)"},
        Down: []string{"DROP TABLE users"}},
}
if err := eng.Migrate(ctx, migrations); err != nil {
    // 可能为 *migrate.CompatibilityError（破坏性变更被拒绝）
    // 或 migrate.ErrLeaseBusy（等待执行权超时）
}
// 回滚到版本 0（全部回滚）
_ = eng.RollbackTo(ctx, migrations, 0)
```

## 迁移规则

1. 迁移按 `Version` 升序应用；`RollbackTo(target)` 按降序回滚所有
   `version > target` 的已应用迁移。
2. 每个迁移的所有语句连同版本簿记在同一个事务中执行：要么全部生效，
   要么整体回滚。失败的版本会被标记为 `failed`，修复定义后重跑即可
   从该版本安全继续，之前的版本不会重复执行。
3. 版本簿记存于 `schema_migrations` 表，租约存于 `schema_migration_lock`
   表，均自动创建。

## 兼容性校验与安全上线流程

滚动升级期间新旧版本实例并存，直接执行破坏性变更会让旧实例报错。
引擎识别以下破坏性变更并给出分阶段路径（`Violation.StagedPath`）：

| 类别 (`ViolationKind`) | 示例 | 风险 |
|---|---|---|
| `drop_column` | `ALTER TABLE t DROP COLUMN c` | 旧实例仍读写该列 |
| `drop_table` | `DROP TABLE t` | 旧实例仍访问该表 |
| `alter_column_type` | `ALTER TABLE t ALTER COLUMN c TYPE BIGINT` | 旧实例按旧类型读写 |
| `add_not_null` | `ADD COLUMN c TEXT NOT NULL`（无默认值） | 旧实例插入不提供该列 |
| `rename_column` | `RENAME COLUMN a TO b` | 旧实例按旧名访问 |

通用的分阶段（expand/contract）上线流程：

1. **扩展阶段**：只做兼容性变更（新增可空列/新表），发布双写新旧结构的
   应用版本，等待全部实例完成滚动升级。
2. **回填阶段**：回填历史数据并校验一致性。
3. **收缩阶段**：全部实例切换到新结构后，在独立迁移中执行破坏性变更，
   并以 `Config.AllowDestructive = true` 显式放行。

## 配置参数与推荐取值

| 参数 | 含义 | 推荐值 |
|---|---|---|
| `LeaseTTL` | 租约有效期；持有者崩溃后其他实例需等待该时长才能接管 | 30s（不宜过小，须大于心跳间隔的 3 倍以上） |
| `HeartbeatInterval` | 执行期间租约续期间隔 | 5s（LeaseTTL 的 1/6 ~ 1/3） |
| `AcquireTimeout` | 等待执行权的最长时间，超时返回 `ErrLeaseBusy` | 2min（按最长迁移耗时上调） |
| `RetryInterval` | 获取租约失败后的重试间隔 | 500ms |
| `StatementTimeout` | 单条迁移语句的执行超时，防止长事务永久阻塞 | 5min（大表回填按数据量上调） |
| `AllowDestructive` | 放行含破坏性变更的计划 | 默认 `false`，仅在收缩阶段显式开启 |

长时间未完成的迁移不会永久阻塞其他实例：若持有者存活，心跳会续期直到
语句超时；若持有者崩溃，租约在 `LeaseTTL` 后自动失效。

## 验证方法

```sh
go build ./...   # 编译
go vet ./...     # 静态检查
go test ./...    # 全部测试
```

测试覆盖：

- `validate_test.go`：各类破坏性变更的识别、可区分拒绝原因、兼容性变更放行；
- `engine_test.go`：按序执行与跳过已应用版本、逆序回滚到目标版本、
  失败不留中间态且可从中断处恢复；
- `concurrency_test.go`：租约互斥、崩溃后租约自动失效并被接管、
  双实例并发迁移恰好执行一次、获取超时返回 `ErrLeaseBusy`。
