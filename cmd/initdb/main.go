package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()

	// 尝试多种常见的 PostgreSQL 配置
	dsns := []string{
		"postgres://postgres:@localhost:5432/postgres?sslmode=disable",
		"postgres://betterme:@localhost:5432/postgres?sslmode=disable",
		"postgres://:@localhost:5432/postgres?sslmode=disable",
		"postgres://fluxkeys:fluxkeys@localhost:5432/postgres?sslmode=disable",
	}

	var conn *pgx.Conn
	var err error
	var successDSN string
	for _, dsn := range dsns {
		conn, err = pgx.Connect(ctx, dsn)
		if err == nil {
			fmt.Printf("✓ 使用配置连接成功: %s\n", dsn)
			successDSN = dsn
			break
		}
		fmt.Printf("尝试失败: %s\n", dsn)
	}
	if conn == nil {
		log.Fatal("无法连接到 PostgreSQL，请确保 PostgreSQL 正在运行")
	}
	defer func() { _ = conn.Close(ctx) }() // 工具命令，关闭失败无处理价值

	// 创建数据库
	_, err = conn.Exec(ctx, "CREATE DATABASE fluxkeys")
	if err != nil {
		// 可能已存在
		fmt.Println("数据库可能已存在:", err)
	} else {
		fmt.Println("✓ 数据库创建成功")
	}
	_ = conn.Close(ctx)

	// 连接到 fluxkeys 数据库，使用之前成功的连接配置但替换数据库名
	fluxkeysDSN := successDSN[:len(successDSN)-len("postgres?sslmode=disable")] + "fluxkeys?sslmode=disable"
	conn2, err := pgx.Connect(ctx, fluxkeysDSN)
	if err != nil {
		log.Fatal("连接 fluxkeys 数据库失败:", err)
	}
	defer func() { _ = conn2.Close(ctx) }()

	// 读取并执行 schema
	schema, err := os.ReadFile("internal/store/schema.sql")
	if err != nil {
		log.Fatal("读取 schema 失败:", err)
	}

	_, err = conn2.Exec(ctx, string(schema))
	if err != nil {
		log.Fatal("执行 schema 失败:", err)
	}

	fmt.Println("✓ 表结构创建成功")
}
