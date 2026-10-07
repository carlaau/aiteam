// Package store 实现 SQLite 访问层（打开/迁移/一表一文件+聚合）。
// 约定：CRUD SQL 集中在本包文件内，handler/上层不散写 SQL。
package store
