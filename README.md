# schema-migration

支持**在线滚动迁移**与**兼容性校验**的数据库模式迁移引擎。使用本地嵌入式存储
（JSON 文件 + 文件锁）模拟真实环境中的表结构变更与多实例并发执行，无需任何外部数据库。

- 按语义化版本顺序解析并执行迁移；每个迁移同时声明升级（Up）与回滚（Down）。
- 已执行版本不重复执行；回滚严格按逆序进行，可恢复到指定目标版本。
- 迁移计划的兼容性校验：识别删列、改类型、加非空约束、重命名列、删表等破坏性变更，
  在“直接执行会导致旧版本实例不可用”时以**可区分的拒绝原因**拒绝，并为可分阶段完成的
  变更生成 **expand / cutover / contract** 的安全上线路径。
- 多实例并发编排：同一时刻只有一个实例执行迁移，其余实例等待或跳过；
  持有者崩溃后租约自动失效，其他实例到期即可接管（fencing token 防止“假死”持有者复活误写）。
- 可恢复执行：每个变更与其进度日志**原子提交**，中断重跑从中断处继续，
  已应用部分绝不重复，失败时不留下无法判断状态的中间态。
- 可配置超时、租约与重试策略，避免长时间未完成的迁移永久阻塞其他实例。

## 安装与快速上手

```go
import migrator "github.com/lechandonga/schema-migration"

store, _ := migrator.OpenStore("./data")
defer store.Close()

migrations := []migrator.Migration{
    {
        Version: "1.0.0",
        Up: []migrator.Change{
            {Kind: migrator.CreateTable, Table: "users", Columns: []migrator.Column{
                {Name: "id", Type: "bigint", Nullable: false, HasDefault: true, Default: 0},
            }},
            {Kind: migrator.AddColumn, Table: "users", Column: "email", Type: "varchar(255)", Nullable: true},
        },
        Down: []migrator.Change{
            {Kind: migrator.DropColumn, Table: "users", Column: "email"},
            {Kind: migrator.DropTable, Table: "users"},
        },
    },
}

eng := migrator.NewEngine(store, migrations, "instance-1", migrator.DefaultConfig())

res, err := eng.Migrate(ctx, migrator.Latest)          // 升级到最高版本
res, err = eng.Migrate(ctx, "1.0.0")                  // 升级到指定版本
res, err = eng.Rollback(ctx, "1.0.0")                 // 逆序回滚到指定版本（该版本保留）
res, err = eng.Rollback(ctx, "")                      // 回滚到空库
```

破坏性变更会被拒绝，并给出原因与分阶段建议：

```go
_, err = eng.Migrate(ctx, "2.0.0")
var rj *migrator.RejectedPlanError
if errors.As(err, &rj) {
    for _, v := range rj.Violations {
        // v.Reason 可区分：drop_column / rename_column / alter_column_type /
        // add_not_null_constraint / drop_table
    }
    // rj.Staged 为每个破坏性迁移给出 expand/cutover/contract 阶段
}

// 直接按分阶段路径执行（阶段之间通常需要配合灰度发布，见 docs/safe-rollout.md）
res, staged, err := eng.MigrateStaged(ctx, "2.0.0")
```

可运行的完整示例：

```bash
go run ./cmd/migrator-demo -dir ./demo-data
```

## 变更类型与兼容性规则

| 变更 | Kind | 滚动兼容 | 说明 |
| --- | --- | --- | --- |
| 建表 | `create_table` | ✅ | 纯新增 |
| 删表 | `drop_table` | ❌ 无安全在线路径 | 旧实例整体失败 |
| 加可空列 | `add_column`（nullable） | ✅ | 纯新增 |
| 加非空列 | `add_column`（not null） | ⚠️ 需默认值 | 无默认值按“加非空约束”处理 |
| 删列 | `drop_column` | ❌ 可分阶段 | expand 停止使用 → contract 删除 |
| 重命名列 | `rename_column` | ❌ 可分阶段 | expand 影子列双写 → cutover 切换 → contract 删旧列 |
| 改列类型 | `alter_column` | ❌ 可分阶段 | expand 新类型影子列双写 → cutover 回填切换 → contract 替换 |
| 加非空约束 | `set_nullable(false)` 无默认值 | ❌ 可分阶段 | expand 加默认值并回填 → contract 再加 NOT NULL |
| 放宽可空 | `set_nullable(true)` | ✅ | 放宽约束对旧实例安全 |
| 加索引 | `add_index` | ✅ | 模拟实现仅校验表存在 |

判定原则：滚动发布期间旧版本实例仍会读写“升级后”的模式，因此凡移除/重命名旧实例
依赖的结构、收紧约束或改变数据类型，均判定为不兼容。详见 [`docs/design.md`](docs/design.md)
与 [`docs/safe-rollout.md`](docs/safe-rollout.md)。

## 配置

见 [`docs/config.md`](docs/config.md)，包含所有参数的含义、推荐取值与取舍。

## 测试与验证

```bash
go test ./...            # 全部用例
go test -race ./...      # 竞态检测（需 CGO_ENABLED=1）
go vet ./...
```

覆盖场景：破坏性变更拒绝与分阶段路径、并发互斥（等待/跳过）、崩溃后租约接管、
中断恢复（升级/回滚）、重试耗尽后的一致性、逆序回滚等。验证方法详见
[`docs/testing.md`](docs/testing.md)。

## 目录结构

```
types.go      核心领域类型（版本、变更、迁移、计划）
semver.go     语义化版本比较
config.go     超时/租约/重试配置与推荐值
store.go      嵌入式模拟数据库 + 元数据 + 原子持久化 + 步骤日志
dirmutex.go   进程内目录互斥
flock*.go     跨进程文件锁（Unix flock，Windows 退化）
lock.go       租约管理器（获取/续租/过期接管/释放，fencing token）
plan.go       升级/回滚计划构造
compat.go     兼容性校验与 expand/contract 阶段生成
engine.go     引擎编排（租约竞争、心跳、恢复、重试、超时）
cmd/          可运行示例
docs/         设计、安全上线流程、配置与测试文档
```
