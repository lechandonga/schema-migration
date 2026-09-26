# schema-migration

支持在线滚动迁移与兼容性校验的数据库模式迁移引擎。使用本地嵌入式
SQLite（`modernc.org/sqlite`，纯 Go 实现）模拟真实环境中的表结构变更与
多实例并发执行。

## 功能概览

- **按版本顺序执行**：每个迁移包含 `Up`（升级）与 `Down`（回滚）定义，
  已执行的版本自动跳过，回滚按逆序进行并恢复到指定目标版本。
- **分阶段安全上线**：一个版本可拆成若干 `Stage` 分多次部署推进，
  每个阶段独立事务提交并单独簿记；中断后续跑不重复已完成阶段，
  回退按阶段逆序进行。
- **兼容性校验**：`ValidatePlan` 识别删列、删表、改类型、加非空约束、
  重命名列等破坏性变更；`Engine.Migrate` 默认拒绝本次待执行计划中
  含破坏性变更的部分，返回可区分的 `*CompatibilityError`
  （含每处违规的类别、原因与分阶段路径）。校验只覆盖尚未应用的版本，
  历史已应用的破坏性版本不会反复挡住后续常规迁移。
- **多实例编排**：基于数据库表的租约保证同一时刻只有一个实例执行迁移。
  支持两种争用策略：等待（超时后收到 `ErrLeaseBusy`）与让出（立即返回
  `ResultYielded`）；持有者崩溃后租约自动过期，其他实例可接管。
- **可恢复执行**：每个迁移（或阶段）与其簿记在单个事务中提交，中断后
  重跑会从中断处继续，已应用部分不会重复应用，也不会留下无法判断状态
  的中间态。
- **可配置策略**：租约 TTL、心跳、获取超时、重试间隔、语句超时、
  争用策略均可配置。

## 快速开始

```go
db, _ := sql.Open("sqlite", "file:app.db?_pragma=busy_timeout(5000)")
eng := migrate.New(db, migrate.DefaultConfig(), "host-1/pid-1234")

migrations := []migrate.Migration{
    {Version: 1, Name: "create_users",
        Up:   []string{"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)"},
        Down: []string{"DROP TABLE users"}},
}
res, err := eng.MigrateWithResult(ctx, migrations)
// err 可能为 *migrate.CompatibilityError（破坏性变更被拒绝）
// 或 migrate.ErrLeaseBusy（等待执行权超时）；
// res 区分 migrate.ResultExecuted（本次由我执行）与
// migrate.ResultYielded（已有其他实例在执行，本次让出）。
_, _ = res, err

// 回滚到版本 0（全部回滚）
_ = eng.RollbackTo(ctx, migrations, 0)
```

## 迁移规则

1. 迁移按 `Version` 升序应用；`RollbackTo(target)` 按降序回滚所有
   `version > target` 的已应用迁移。
2. 每个迁移的所有语句连同版本簿记在同一个事务中执行：要么全部生效，
   要么整体回滚。失败的版本会被标记为 `failed`，修复定义后重跑即可
   从该版本安全继续，之前的版本不会重复执行。
3. 版本簿记存于 `schema_migrations` 表，阶段簿记存于
   `schema_migration_stages` 表，租约存于 `schema_migration_lock`
   表，均自动创建。

## 分阶段安全上线

一次破坏性变更可以拆成若干阶段，分多次部署分别推进。在 `Migration`
中设置 `Stages`（此时 `Up`/`Down` 被忽略）：

```go
{Version: 12, Name: "drop_legacy_column", Stages: []migrate.Stage{
    // 阶段 1（expand）：只做兼容变更，新旧应用版本都可用。
    {Name: "expand",
        Up:   []string{"ALTER TABLE users ADD COLUMN full_name TEXT"},
        Down: []string{"ALTER TABLE users DROP COLUMN full_name"}},
    // 阶段 2（backfill）：回填数据，可配合新应用版本双写。
    {Name: "backfill",
        Up:   []string{"UPDATE users SET full_name = name WHERE full_name IS NULL"},
        Down: []string{"UPDATE users SET full_name = NULL"}},
    // 阶段 3（contract）：确认全部实例升级后再执行破坏性变更。
    {Name: "contract",
        Up:   []string{"ALTER TABLE users DROP COLUMN name"},
        Down: []string{"ALTER TABLE users ADD COLUMN name TEXT"}},
}}
```

语义与保证：

- **每次只推进若干阶段**：每个阶段独立事务提交并写入
  `schema_migration_stages` 簿记；阶段之间可以发布新应用版本、
  观察一段时间甚至暂停（下次部署再继续）。
- **断点续跑**：进程中断或某阶段失败后重新发起 `Migrate`，已完成
  阶段不会重复执行，从失败/中断阶段继续；失败阶段在事务中整体回滚，
  不会留下说不清进行到哪一步的状态。可用 `AppliedStages(ctx, version)`
  查询当前进度。
- **有序回退**：`RollbackTo` 对分阶段版本按阶段逆序回退；中断在版本
  中途（部分阶段完成、版本未整体应用）的迁移同样会被有序回退。
- **旧版本可用性**：每个阶段结束时数据库对尚未升级的旧版本实例仍可用，
  这一点由阶段定义保证（expand/contract 模式：破坏性变更只放在最后的
  contract 阶段，且届时全部实例已升级），引擎负责执行、簿记与恢复。
- 分阶段版本的兼容性校验同样只针对尚未执行的部分；进入 contract
  阶段时按需开启 `AllowDestructive`。

## 兼容性校验的覆盖范围

`Migrate` 的兼容性判断只针对**本次真正待执行的迁移**（未应用及上次
失败的版本），在取得执行权后、执行前进行：

- 历史里已应用的破坏性版本不参与判断，不会让后续只是常规兼容的迁移
  被反复拒绝；
- 本次待执行的版本中如含破坏性变更，仍会被拒绝并返回
  `*CompatibilityError`，可通过 `Violations[i].Kind` 区分是哪一类
  破坏性变更（`drop_column`/`drop_table`/`alter_column_type`/
  `add_not_null`/`rename_column`）；没有 `AllowDestructive = true`
  的明确许可，涉及破坏性变更的版本不会被默默执行。

## 多实例编排与争用策略

`Config.ContentionPolicy` 控制发现执行权被其他实例持有时的行为，
配合 `MigrateWithResult` 的返回值，上层可区分三种结果：

| 结果 | 含义 | 表现 |
|---|---|---|
| 本次由我执行完成 | 取得租约并执行完毕 | `ResultExecuted`, `err == nil` |
| 已有其他实例在执行 | 让出策略下首次获取失败 | `ResultYielded`, `err == nil`（正常返回，非失败） |
| 等待超时没拿到执行权 | 等待策略下超过 `AcquireTimeout` | `err` 为 `ErrLeaseBusy` |

- `ContentionWait`（默认）：在 `AcquireTimeout` 内按 `RetryInterval`
  重试，超时返回 `ErrLeaseBusy`。
- `ContentionYield`：首次获取失败即以 `(ResultYielded, nil)` 正常返回，
  由调用方决定后续怎么做（稍后再试或放弃本次）。注意 SQLite 单写者
  模型下，获取租约的那一次写可能短暂阻塞在持有者的写事务上
  （上限为连接的 `busy_timeout`），让出指的是不等待租约本身。
- 持有执行权的实例异常退出（不释放、不心跳）后，租约在 `LeaseTTL`
  到期自动失效，其他实例（无论哪种策略）下次尝试时即可接手。

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
| `ContentionPolicy` | 执行权被占用时的策略：`ContentionWait` 等待 / `ContentionYield` 让出 | 默认 `ContentionWait`；由调度器统一触发迁移、不希望实例间互相等待时用 `ContentionYield` |

长时间未完成的迁移不会永久阻塞其他实例：若持有者存活，心跳会续期直到
语句超时；若持有者崩溃，租约在 `LeaseTTL` 后自动失效。

## 验证方法

```sh
go build ./...   # 编译
go vet ./...     # 静态检查
go test ./...    # 全部测试
```

测试覆盖：

- `validate_test.go`：各类破坏性变更的识别、可区分拒绝原因、兼容性变更
  放行；历史破坏性版本已应用后追加兼容版本可正常执行、待执行的破坏性
  版本仍被明确拒绝；
- `engine_test.go`：按序执行与跳过已应用版本、逆序回滚到目标版本、
  失败不留中间态且可从中断处恢复；
- `stages_test.go`：分阶段迁移的逐阶段簿记、中途中断后续跑不重复已
  完成阶段、回退按阶段逆序、部分完成进度的有序回退；
- `concurrency_test.go`：租约互斥、崩溃后租约自动失效并被接管、
  双实例并发迁移恰好执行一次、等待策略超时返回 `ErrLeaseBusy`、
  让出策略立即返回 `ResultYielded` 且不执行、失效后让出策略实例
  自动接手、并发让出。
