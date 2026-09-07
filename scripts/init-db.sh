#!/bin/bash
set -e

# 初始化 PostgreSQL 数据库和表结构

echo "==> 检查 PostgreSQL 连接"

# 使用 Go 程序来执行 SQL
cat > /tmp/init_db.go << 'EOF'
package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	
	_ "github.com/lib/pq"
)

func main() {
	dsn := "postgres://fluxkeys:fluxkeys@localhost:5432/postgres?sslmode=disable"
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal("连接失败:", err)
	}
	defer db.Close()
	
	// 创建数据库
	_, err = db.Exec("CREATE DATABASE fluxkeys")
	if err != nil {
		// 可能已存在
		fmt.Println("数据库可能已存在，继续...")
	} else {
		fmt.Println("✓ 数据库创建成功")
	}
	
	// 连接到 fluxkeys 数据库
	dsn = "postgres://fluxkeys:fluxkeys@localhost:5432/fluxkeys?sslmode=disable"
	db2, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal("连接 fluxkeys 数据库失败:", err)
	}
	defer db2.Close()
	
	// 读取并执行 schema
	schema, err := os.ReadFile("internal/store/schema.sql")
	if err != nil {
		log.Fatal("读取 schema 失败:", err)
	}
	
	_, err = db2.Exec(string(schema))
	if err != nil {
		log.Fatal("执行 schema 失败:", err)
	}
	
	fmt.Println("✓ 表结构创建成功")
}
EOF

cd /Users/betterme/PycharmProjects/AI/fluxkeys
go run /tmp/init_db.go

echo "✅ 数据库初始化完成"
