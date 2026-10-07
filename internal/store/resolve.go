// resolve.go —— 身份/栏目/项目解析 helper（B4-5 自 resources.go 抽出为共享文件：
// 窗域端点（B4-5）与资源端点（B4-2）跨域消费同一批解析面，B4-2 质量审查裁定动作）。
// 各函数的功能语义注释随函数走，此处只记归属变更。
//
// B1 合并收敛说明（rebase 裁定）：三哨兵（ErrProjectNotFound/ErrColumnNotFound/
// ErrSessionNotFound）定义删除，统一消费 B1 在 projects.go/columns.go/sessions.go
// 的同语义哨兵（errors.Is 判定消费方零改动）。三 Resolve 函数保留独立实现：
// ResolveColumn 将项目缺失归一为栏目 404（端点冻结语义——技术设计 #18/#21 特有
// 错误仅 column_not_found，项目存在性由会话心跳中间件前置保证），与中间件
// GetProjectColumnForSession 的 project/column 分级 404 语义不同层，非双胞胎；
// ResolveSession 三元组防歧义查询 B1 无等价物（UpsertSession 为写路径）。
package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// ResolveProjectID 按项目 code 解析项目 id（#20 project query 过滤前置）。
func (s *Store) ResolveProjectID(projectCode string) (int64, error) {
	var id int64
	err := s.DB.QueryRow(`SELECT id FROM projects WHERE code = ?`, projectCode).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrProjectNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: 解析项目失败: %w", err)
	}
	return id, nil
}

// ColumnRef 解析后的栏目归属（#18 登记 store 参数面；具名结构体防双 int64 参数换位）。
type ColumnRef struct {
	ProjectID int64
	ColumnID  int64
}

// ResolveColumn 按项目 code+栏目 code 解析栏目（§2.2 #18：四头 project 域+请求体 column）。
// 项目不存在或栏目不在该项目域内（UNIQUE(project_id, code) 无命中）→ ErrColumnNotFound。
func (s *Store) ResolveColumn(projectCode, columnCode string) (ColumnRef, error) {
	var ref ColumnRef
	err := s.DB.QueryRow(
		`SELECT c.project_id, c.id FROM columns c JOIN projects p ON p.id = c.project_id
		 WHERE p.code = ? AND c.code = ?`, projectCode, columnCode,
	).Scan(&ref.ProjectID, &ref.ColumnID)
	if errors.Is(err, sql.ErrNoRows) {
		return ColumnRef{}, ErrColumnNotFound
	}
	if err != nil {
		return ColumnRef{}, fmt.Errorf("store: 解析栏目失败: %w", err)
	}
	return ref, nil
}

// ResolveSession 按项目/栏目/会话名三元组解析会话 id（#18/#19 created_by 前置）。
// 三元组对齐 sessions UNIQUE (project_id, column_id, name)：同名会话可跨栏目多行
// （总控多栏目=多实体行），仅按 name 查询会歧义，故限定三头域精确命中——与 B1
// 心跳 upsert 键同构，B1 在位时（先 upsert 后进 handler）必命中。
func (s *Store) ResolveSession(projectCode, columnCode, sessionName string) (int64, error) {
	var id int64
	err := s.DB.QueryRow(
		`SELECT se.id FROM sessions se
		 JOIN projects p ON p.id = se.project_id
		 JOIN columns c ON c.id = se.column_id
		 WHERE p.code = ? AND c.code = ? AND se.name = ?`,
		projectCode, columnCode, sessionName,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrSessionNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: 解析会话失败: %w", err)
	}
	return id, nil
}
