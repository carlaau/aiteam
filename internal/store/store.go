// Package store 实现 SQLite 访问层（打开/迁移/一表一文件+聚合）。
package store

import (
	"database/sql"
	_ "embed"
	"fmt"

	// SQLite 驱动（纯 Go 实现，init 注册 "sqlite" 驱动名）。
	_ "modernc.org/sqlite"
)

// dbTypeSQLite 一期唯一支持的数据库类型（技术设计 §3.1 第 7 条 db.type 扩展位）。
const dbTypeSQLite = "sqlite"

// schemaSQL B0 冻结基线 DDL 全文（§3.2 十表 + §3.3 两触发器 + §3.4 全索引）。
//
//go:embed schema.sql
var schemaSQL string

// migration 单条迁移：version 写入 PRAGMA user_version，sql 为该版本 DDL 全文。
// 迁移数组 append-only：已发布条目不得修改，新版本递增版本号追加。
type migration struct {
	version int
	sql     string
}

// migrations 迁移数组（§3.1 第 6 条）：首次启动按序执行，AC16.1 零前置脚本自动建全表。
// append-only：顺序=合并序（v2=b8-W3 先于 b5 的后续扩列——b5 落地时追加其后）。
var migrations = []migration{
	{1, schemaSQL}, // B0 冻结基线：10 表 + 2 触发器 + 全索引
	// b8-W3 命中留痕列（b8-spec §二.3）：哨兵 poll 非空命中时回写该会话行
	// （「在挂≠在干活」服务端可见——命中即注销哨兵行，故留痕落会话行不落哨兵行）；
	// 缺省空串=从未命中（未命中会话该键为 ''）。
	{2, `ALTER TABLE sessions ADD COLUMN sentinel_last_hit_at TEXT NOT NULL DEFAULT ''`},
	// b5-W1 跨项目审计列（FR9/技术设计 §3.2）：''=项目内投递（存量行零迁移成本）；
	// 跨项目投递=发送方项目 code（AC9.3 审计断言面，b5-spec §1.1-W1①——DDL 与技术
	// 设计 §3.2 原文逐字一致，含终结分号）。
	{3, `ALTER TABLE messages ADD COLUMN from_project TEXT NOT NULL DEFAULT '';`},
}

// SchemaVersion 当前 schema 最新版本号（=migrations 末条 version，版本连续 1..N）。
// 跨包锚面：backup 产物自校验测试据 user_version 锚定本值——新迁移追加后锚自跟，
// 免逐批改字面量（同包测试直用 len(migrations)，见 store_test.go AC16.1）。
func SchemaVersion() int { return migrations[len(migrations)-1].version }

// Store SQLite 存储句柄。后续批 CRUD 直接消费。
type Store struct {
	// DB 底层连接池，归属 Store：外部可直接发查询，但勿 Close、勿改池参数
	// （生命周期与池参数由 Open/Close 管理）。
	DB *sql.DB
}

// Open 打开（必要时创建）path 处的 SQLite 库：执行连接初始化 PRAGMA 并跑迁移。
// 一期固定 sqlite 类型；带 db.type 分派用 OpenWithType。
func Open(path string) (*Store, error) {
	return OpenWithType(dbTypeSQLite, path)
}

// OpenWithType 按配置 db.type 分派打开逻辑（§3.1 第 7 条 switch 扩展位：
// 一期仅 "sqlite"，其他值启动报错；不做全量 repository 接口抽象，YAGNI）。
func OpenWithType(dbType, path string) (*Store, error) {
	switch dbType {
	case dbTypeSQLite:
		return openSQLite(path)
	default:
		return nil, fmt.Errorf("store: 不支持的 db.type %q（一期仅支持 %q）", dbType, dbTypeSQLite)
	}
}

// openSQLite 打开 SQLite 库并完成连接初始化 + 迁移。
// 注意：DSN 直接拼接 path，若路径含 '?'/'#' 等 URI 保留字符需先行转义（当前部署不涉及）。
func openSQLite(path string) (*Store, error) {
	// 连接初始化 PRAGMA（§3.1 第 5 条）经 modernc/sqlite 的 DSN _pragma 参数下发：
	// 每条新连接建立时自动执行，连接池重建连接后不丢失（Exec 方式只覆盖当次连接，
	// 重建后 foreign_keys 等会静默回默认值）。journal_mode=WAL：AC16.2 kill -9 自恢复；
	// synchronous=NORMAL：WAL 下已提交事务不丢，性能合理；foreign_keys=ON：外键约束生效；
	// busy_timeout=5000：多连接/多进程并发写排队等待。
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: 打开 sqlite 库 %q 失败: %w", path, err)
	}
	// 单连接池：写路径串行化（§3.1 第 8 条），连接数=1 使连接级 PRAGMA 状态全局稳定。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrate 按 migrations 顺序执行未应用的迁移：每条一个事务（DDL+user_version 同事务原子生效），
// 已应用（version ≤ 当前 user_version）的跳过，重开幂等。
func (s *Store) migrate() error {
	var current int
	if err := s.DB.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: 读取 user_version 失败: %w", err)
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := s.DB.Begin()
		if err != nil {
			return fmt.Errorf("store: 开启迁移 v%d 事务失败: %w", m.version, err)
		}
		if _, err := tx.Exec(m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: 执行迁移 v%d 失败: %w", m.version, err)
		}
		// PRAGMA 不支持参数绑定；版本号为内置常量，无注入面。
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", m.version)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: 写 user_version=%d 失败: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: 提交迁移 v%d 失败: %w", m.version, err)
		}
	}
	return nil
}

// Close 关闭底层连接池。
func (s *Store) Close() error {
	if s == nil || s.DB == nil {
		return nil
	}
	return s.DB.Close()
}
