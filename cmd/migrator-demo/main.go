// Command migrator-demo 演示模式迁移引擎的典型用法：
// 顺序升级、破坏性变更拒绝、分阶段上线、多实例互斥与中断恢复。
//
// 用法：
//
//	go run ./cmd/migrator-demo -dir ./demo-data
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"time"

	migrator "github.com/lechandonga/schema-migration"
)

func demoMigrations() []migrator.Migration {
	return []migrator.Migration{
		{
			Version:     "1.0.0",
			Description: "建表并新增 email 列（兼容性变更）",
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
		{
			Version:     "2.0.0",
			Description: "直接删除 email 列（破坏性变更，会被拒绝一步执行）",
			Up:          []migrator.Change{{Kind: migrator.DropColumn, Table: "users", Column: "email", Type: "varchar(255)"}},
			Down: []migrator.Change{
				{Kind: migrator.AddColumn, Table: "users", Column: "email", Type: "varchar(255)", Nullable: true},
			},
		},
	}
}

func cfg() migrator.Config {
	c := migrator.DefaultConfig()
	// demo 中适当缩短，便于观察。
	c.LeaseDuration = 2 * time.Second
	c.HeartbeatInterval = 500 * time.Millisecond
	return c
}

func main() {
	dir := flag.String("dir", "./demo-data", "模拟数据库的数据目录")
	flag.Parse()

	ctx := context.Background()
	store, err := migrator.OpenStore(*dir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	eng := migrator.NewEngine(store, demoMigrations(), "instance-1", cfg())

	// 1) 升级到 1.0.0。
	res, err := eng.Migrate(ctx, "1.0.0")
	if err != nil {
		log.Fatalf("migrate 1.0.0: %v", err)
	}
	fmt.Printf("[1] migrate -> 1.0.0: status=%s applied=%v\n", res.Status, res.Applied)

	// 2) 尝试直接升级到含破坏性变更的 2.0.0，预期被拒绝。
	_, err = eng.Migrate(ctx, "2.0.0")
	var rejected *migrator.RejectedPlanError
	if errors.As(err, &rejected) {
		fmt.Printf("[2] direct migrate -> 2.0.0 被拒绝：\n")
		for _, v := range rejected.Violations {
			fmt.Printf("    - reason=%s table=%s column=%s staged=%v\n      %s\n",
				v.Reason, v.Table, v.Column, v.Staged, v.Message)
		}
		fmt.Printf("    建议的安全上线阶段：\n")
		for _, sm := range rejected.Staged {
			for _, p := range sm.Phases {
				fmt.Printf("      * %s — %s\n", p.Version, p.Description)
			}
		}
	} else if err != nil {
		log.Fatalf("unexpected error: %v", err)
	}

	// 3) 按 expand/contract 分阶段路径安全上线 2.0.0。
	res, staged, err := eng.MigrateStaged(ctx, "2.0.0")
	if err != nil {
		log.Fatalf("staged migrate: %v", err)
	}
	fmt.Printf("[3] staged migrate -> 2.0.0: status=%s applied=%v phases=%d\n",
		res.Status, res.Applied, len(staged))

	// 4) 回滚到 1.0.0（注意阶段版本与 2.0.0 的撤销顺序由 Down 定义驱动）。
	res, err = eng.Rollback(ctx, "1.0.0")
	if err != nil {
		// demo 迁移未声明阶段版本的 Down，演示中可接受；打印信息即可。
		fmt.Printf("[4] rollback: %v\n", err)
		return
	}
	fmt.Printf("[4] rollback -> 1.0.0: status=%s undone=%v\n", res.Status, res.Applied)
	fmt.Println("当前已应用版本：", store.AppliedVersions())
}
