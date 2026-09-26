# schema-migration

支持在线滚动迁移、分阶段安全上线与多实例编排的数据库模式迁移引擎。
使用本地嵌入式 SQLite（`modernc.org/sqlite`，纯 Go 实现）模拟真实环境
中的表结构变更与多实例并发执行。

## 功能概览

- **按版本顺序执行**：每个迁移包含 `Up`（升级）与 `Down`（回滚）定义，
  已执行的版本自动跳过，回滚按逆序进行并恢复到指定目标版本。
- **分阶段安全上线**：一次破坏性变更可拆成若干 `Stage` 分多次部署推进，
  每次只推进一个阶段，阶段之间可发布新应用版本、观察或暂停；引擎持久
  记录进度，中断后续跑不重复执行，支持有序回退。
- **聚焦本次执行的兼容性校验**：只对本次真正要执行的迁移做破坏性变更
  检查；历史中已落库的破坏性版本不会反复拦截后续兼容迁移。默认拒绝
  破坏性变更，返回可区分具体类别的 `*CompatibilityError`。
- **多实例编排**：基于数据库表的租约保证同一时刻只有一个实例执行迁移。
  冲突策略可配置：等待（超时返回 `ErrLeaseBusy`）或让出（立即正常返回
  `OutcomeYielded`）；持有者崩溃后租约自动过期，其他实例可接管。
- **可恢复执行**：每个迁移与其簿记在单个事务中提交，中断后重跑会从中断
  版本继续，已应用部分不会重复应用，也不会留下无法判断状态的中间态。
- **可配置策略**：租约 TTL、心跳、获取超时、重试间隔、语句超时、冲突
  策略均可配置。

## 快速开始

```go
db, _ := sql.Open("sqlite", "file:app.db?_pragma=busy_timeout(5000)")
eng := migrate.New(db, migrate.DefaultConfig(), "host-1/pid-1234")

migrations := []migrate.Migration{
    {Version: 1, Name: "create_users",
        Up:   []string{"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)"},
        Down: []string{"DROP TABLE users"}},
}
outcome, err := eng.Migrate(ctx, migrations)
// err 可能为 *migrate.CompatibilityError（破坏性变更被拒绝）
// 或 migrate.ErrLeaseBusy（等待执行权超时）
// err == nil 时 outcome 区分：
//   migrate.OutcomeExecuted —— 本次由我执行完成
//   migrate.OutcomeYielded  —— 已有其他实例在执行，本次让出（让出策略下）
_ = outcome

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

## 兼容性校验：覆盖范围

滚动升级期间新旧版本实例并存，直接执行破坏性变更会让旧实例报错。
校验**只针对本次真正要执行的迁移**（未落库的待执行版本）：

- 历史里已经应用过的破坏性版本（例如上一轮收缩阶段已放行执行的
  `DROP COLUMN`）不会反复拦截后续常规兼容的迁移；
- 待执行版本中只要含破坏性变更且未显式放行，整个调用被拒绝，
  该版本不会被默默执行；
- 返回的 `*CompatibilityError` 可通过 `Kinds()` / `HasKind(...)`
  区分被拦下的变更类别，`Violations` 含每处的原因与分阶段路径。

识别的破坏性变更类别：

| 类别 (`ViolationKind`) | 示例 | 风险 |
|---|---|---|
| `drop_column` | `ALTER TABLE t DROP COLUMN c` | 旧实例仍读写该列 |
| `drop_table` | `DROP TABLE t` | 旧实例仍访问该表 |
| `alter_column_type` | `ALTER TABLE t ALTER COLUMN c TYPE BIGINT` | 旧实例按旧类型读写 |
| `add_not_null` | `ADD COLUMN c TEXT NOT NULL`（无默认值） | 旧实例插入不提供该列 |
| `rename_column` | `RENAME COLUMN a TO b` | 旧实例按旧名访问 |

## 分阶段安全上线（expand/contract）

把一次破坏性变更拆成若干 `Stage`，每个阶段对应一次部署，随应用版本
逐步推进：

```go
stages := []migrate.Stage{
    {ID: 1, Name: "expand", Migrations: []migrate.Migration{
        // 只做兼容变更：新增可空列、双写回填；发布后旧实例仍可用
        {Version: 10, Name: "add_name_new",
            Up:   []string{"ALTER TABLE users ADD COLUMN name_new TEXT"},
            Down: []string{"ALTER TABLE users DROP COLUMN name_new"}},
    }},
    {ID: 2, Name: "contract", Migrations: []migrate.Migration{
        // 全部实例升级后再收缩：破坏性变更，需显式放行
        {Version: 11, Name: "drop_name",
            Up:   []string{"ALTER TABLE users DROP COLUMN name"},
            Down: []string{"ALTER TABLE users ADD COLUMN name TEXT"}},
    }},
}

// 每次部署只推进一个阶段；阶段之间可观察、暂停
if _, err := eng.ApplyStage(ctx, stages[0]); err != nil { /* ... */ }
// ... 发布新应用版本，等待全部实例滚动升级 ...
cfg.AllowDestructive = true // 收缩阶段显式放行
if _, err := migrate.New(db, cfg, holder).ApplyStage(ctx, stages[1]); err != nil { /* ... */ }

// 查询进度 / 有序回退（只能回退最近完成的阶段）
progress, _ := eng.Stages(ctx)
_, _ = eng.RollbackStage(ctx, stages[1])
```

规则与保证：

1. 阶段按 `ID` 递增逐一推进（首个阶段可为任意起始 ID），跳过中间
   阶段或回退非最近阶段会返回 `ErrStageOrder`，避免留下说不清进度
   的状态。
2. 每个阶段结束时，该阶段内的迁移对尚未升级的旧版本实例保持可用
   （扩展阶段只含兼容变更；收缩阶段安排在全部实例升级之后）。
3. 进度持久化在 `schema_migration_stages`：进程中断或某阶段失败后
   重新发起，已完成的阶段与已应用的迁移都不会重复执行，续跑从
   中断处继续并补全簿记。
4. `RollbackStage` 按逆序回滚该阶段已应用的迁移并移除阶段簿记，
   只能回退最近完成的阶段，保证回退有序。

## 多实例并发策略

`Config.Contention` 决定执行权已被其他实例持有时的行为：

| 策略 | 行为 | 适用场景 |
|---|---|---|
| `ContentionWait`（默认） | 在 `AcquireTimeout` 内按 `RetryInterval` 重试；超时返回 `ErrLeaseBusy` | 部署流水线中希望随先行者完成后顺带确认本实例状态 |
| `ContentionYield` | 首次获取失败立即让出，正常返回 `OutcomeYielded`，不执行任何迁移 | 多实例同时启动时只希望一个实例执行，其余快速继续启动 |

上层可区分三种结果（`err == nil` 时看 `Outcome`）：

1. `OutcomeExecuted`：本次由我执行完成（含没有待执行迁移的空跑）；
2. `OutcomeYielded`：已有其他实例在执行，本次让出，由调用方决定
   后续（重试、等待或忽略）；
3. `ErrLeaseBusy`：等待策略下超时仍未拿到执行权。

持有者异常退出（不释放、不心跳）后，租约在 `LeaseTTL` 到期自动失效，
其他实例（无论哪种策略）重试即可自动接手。

## 配置参数与推荐取值

| 参数 | 含义 | 推荐值 |
|---|---|---|
| `LeaseTTL` | 租约有效期；持有者崩溃后其他实例需等待该时长才能接管 | 30s（不宜过小，须大于心跳间隔的 3 倍以上） |
| `HeartbeatInterval` | 执行期间租约续期间隔 | 5s（LeaseTTL 的 1/6 ~ 1/3） |
| `AcquireTimeout` | 等待执行权的最长时间，超时返回 `ErrLeaseBusy`（仅等待策略） | 2min（按最长迁移耗时上调） |
| `RetryInterval` | 获取租约失败后的重试间隔 | 500ms |
| `StatementTimeout` | 单条迁移语句的执行超时，防止长事务永久阻塞 | 5min（大表回填按数据量上调） |
| `Contention` | 执行权冲突策略：`ContentionWait` / `ContentionYield` | 默认等待；多实例同时启动且只需一个执行时用让出 |
| `AllowDestructive` | 放行含破坏性变更的待执行迁移 | 默认 `false`，仅在收缩阶段显式开启 |

长时间未完成的迁移不会永久阻塞其他实例：若持有者存活，心跳会续期直到
语句超时；若持有者崩溃，租约在 `LeaseTTL` 后自动失效。

## 验证方法

```sh
go build ./...   # 编译
go vet ./...     # 静态检查
go test ./...    # 全部测试
CGO_ENABLED=1 go test -race ./...  # 竞态检测（可选，较慢）
```

测试覆盖：

- `validate_test.go`：各类破坏性变更的识别、可区分拒绝原因、兼容性变更放行；
- `engine_test.go`：按序执行与跳过已应用版本、逆序回滚到目标版本、
  失败不留中间态且可从中断处恢复、历史破坏性版本不拦截后续兼容迁移、
  待执行破坏性版本仍按类别拒绝；
- `stage_test.go`：分阶段逐段推进与进度查询、跳过阶段被拒绝、中断后
  续跑不重复执行、已完成阶段幂等跳过、有序回退；
- `concurrency_test.go`：租约互斥、崩溃后租约自动失效并被接管、
  双实例并发迁移恰好执行一次、等待超时返回 `ErrLeaseBusy`、让出策略
  立即返回 `OutcomeYielded` 且让出后不执行、失效后重试自动接手。
