package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"aiteam/internal/types"
)

// progress 域 test_status 枚举（DDL CHECK (test_status IN ('pass','fail','unknown'))，
// §3.2 表 11）：列可空（无 NOT NULL），空串语义=未提供（写入侧落 NULL，读回空串）。
const (
	TestStatusPass    = "pass"
	TestStatusFail    = "fail"
	TestStatusUnknown = "unknown"
)

// stale 三态标记（B8-T1 推荐口径=查询侧/Go 侧时间比较，spec §1.1 stale 行）：
// 仅 LatestProgressBySession 填充。判定=严格大于（对齐 §7.2 失联判定边界口径：
// now-created_at 恰 1×阈值整点归正常档、恰 2×整点归黄档）。
const (
	// ProgressStaleNone 正常（含时钟回拨的负差值——GetSession 回拨容忍同款）。
	ProgressStaleNone = ""
	// ProgressStaleYellow 黄：now-created_at > 1×阈值（会话久未上报，疑似停滞）。
	ProgressStaleYellow = "stale"
	// ProgressStaleRed 红：now-created_at > 2×阈值（会话确认失联，巡检升级告警）。
	ProgressStaleRed = "dead"
)

// progress limit 口径（#32「limit 默认 100/max 1000」，与 #28 AuditLimit* 同构）：
// 端点默认值 100 由 handler 传参保证；store 层 ≤0 兜底默认（防调用方漏传）、
// 超界 clamp 到 Max（上限强制收敛在数据面）。
const (
	ProgressLimitDefault = 100
	ProgressLimitMax     = 1000
)

// ProgressReport progress_reports 表一行（§3.2 表 11 字段面）+ 会话名装配列 +
// stale 计算列。
//
// 作为 InsertProgress 入参时只消费 SessionID 与六个业务字段；ID/CreatedAt/
// SessionName/Stale 为输出面（服务端生成/读路径装配），输入值被忽略
// （CreateProject 复用 Project 结构同款模式）。
type ProgressReport struct {
	ID        int64
	SessionID int64 // FK sessions(id)（强关联，foreign_keys=ON 兜底）
	// SessionName JOIN sessions 装配（#32 响应 session 字段面）；InsertProgress
	// 返回值为空串（写路径不 JOIN，UpsertSession 返回值不装配 code 同款惯例）。
	SessionName string
	Batch       string // 可空（批次号，如 B8）；空串落 NULL 读回空串
	Task        string // 可空（任务号，如 B8-1）
	CommitHash  string // 可空（post-commit hook 自动层采 git rev-parse HEAD）
	Branch      string // 可空（hook 自动层采 --abbrev-ref）
	TestStatus  string // pass|fail|unknown|""（可空；空=未提供落 NULL）
	Summary     string // 可空（≤512B 上限校验主责在 #31 handler）
	CreatedAt   string // ISO8601 UTC（服务端 types.NowUTC 注入，AC5.4）
	// Stale 陈旧计算列（查询侧 Go 比较，B8-T1 口径）：仅 LatestProgressBySession
	// 填充；InsertProgress/ListProgress 返回值恒 ProgressStaleNone（#32 字段面
	// 无 stale，历史明细不做陈旧标注）。三态与边界口径见 ProgressStale* 常量注。
	Stale string
}

// progressColumns 表本位列清单（InsertProgress 的 RETURNING 用，与
// scanProgress 的 Scan 顺序一一对应；写路径不 JOIN 装配会话名）。
const progressColumns = `id, session_id, batch, task, commit_hash, branch, test_status, summary, created_at`

// progressJoinColumns SELECT 列清单（ListProgress 与 LatestProgressBySession 共用，
// 与 scanProgressRow 的 Scan 顺序一一对应）：JOIN sessions 装配会话名（#32 响应
// session 字段），内连接必有行——session_id 为 NOT NULL FK 且 foreign_keys=ON，
// 引用完整性由写入侧兜底保证。
const progressJoinColumns = `pr.id, pr.session_id, se.name,
       pr.batch, pr.task, pr.commit_hash, pr.branch, pr.test_status, pr.summary, pr.created_at`

// scanProgress 从一行解出 ProgressReport——表本位列版（InsertProgress RETURNING，
// 9 列无会话名装配，SessionName 恒零值）。
func scanProgress(scan func(dest ...any) error) (ProgressReport, error) {
	var (
		p                                                ProgressReport
		batch, task, commitHash, branch, testSt, summary sql.NullString
	)
	err := scan(&p.ID, &p.SessionID,
		&batch, &task, &commitHash, &branch, &testSt, &summary, &p.CreatedAt)
	if err != nil {
		return ProgressReport{}, err
	}
	p.Batch, p.Task, p.CommitHash, p.Branch, p.TestStatus, p.Summary =
		batch.String, task.String, commitHash.String, branch.String, testSt.String, summary.String
	return p, nil
}

// scanProgressRow 从一行解出 ProgressReport——JOIN 装配版（ListProgress/
// LatestProgressBySession，10 列含会话名；内连接 se.name 恒非 NULL 直扫）。
// 可空 TEXT 列经 sql.NullString 中转（写入侧空串落 NULL——对齐 DDL 可空语义，
// 读侧统一空串）。
func scanProgressRow(scan func(dest ...any) error) (ProgressReport, error) {
	var (
		p                                                ProgressReport
		batch, task, commitHash, branch, testSt, summary sql.NullString
	)
	err := scan(&p.ID, &p.SessionID, &p.SessionName,
		&batch, &task, &commitHash, &branch, &testSt, &summary, &p.CreatedAt)
	if err != nil {
		return ProgressReport{}, err
	}
	p.Batch, p.Task, p.CommitHash, p.Branch, p.TestStatus, p.Summary =
		batch.String, task.String, commitHash.String, branch.String, testSt.String, summary.String
	return p, nil
}

// nullableText 可空文本写入参数：空串转 nil（落 NULL，对齐 DDL 可空列语义——
// test_status 尤其必须如此，空串不在 CHECK 枚举内会被约束拒绝）。
func nullableText(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// progressStaleFlag stale 判定纯函数（B8-T1 查询侧口径）：created_at 距 now 的
// 差值严格大于 1×staleAfterSec → 黄、大于 2× → 红、其余（含负差值回拨容忍）→ 正常。
// 两时刻均为 types.NowUTC 口径的 ISO8601 UTC 文本（time.RFC3339 可解析）；解析
// 失败返回 error（服务端写入的受控格式，坏文本=库被外部改动的 fail-fast，不静默容忍）。
func progressStaleFlag(createdAt, now string, staleAfterSec int) (string, error) {
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return "", fmt.Errorf("store: 进度上报 created_at %q 解析失败: %w", createdAt, err)
	}
	nowT, err := time.Parse(time.RFC3339, now)
	if err != nil {
		return "", fmt.Errorf("store: 判定基准 now %q 解析失败: %w", now, err)
	}
	threshold := time.Duration(staleAfterSec) * time.Second
	d := nowT.Sub(created)
	switch {
	case d > 2*threshold:
		return ProgressStaleRed, nil
	case d > threshold:
		return ProgressStaleYellow, nil
	default:
		return ProgressStaleNone, nil
	}
}

// InsertProgress 写一条进度上报（#31 POST /progress 数据面，FR23）：
//
//   - 会话解析与注册主责在 B1-4 中间件（GetProjectColumnForSession 存在性 404 +
//     UpsertSession 隐式注册/心跳——「上报即心跳」由中间件达成），本函数消费
//     中间件产物 sessionID 直插，业务面恰 1 条 INSERT（b8-spec §七往返账
//     「#31 = 中间件 3+业务 1=4 语句/次」）；sessionID 不存在时 FK 约束兜底报错
//     （TestProgressInsertForeignKey 钉底，UpsertSession 的 project_id FK 兜底同构）；
//   - test_status 非法值不预校验：DDL CHECK 约束拒绝并透出为 error
//     （TestTestStatusConstraint 钉底；合法枚举常量 TestStatus* 供调用方引用）；
//   - 可空文本字段空串落 NULL（nullableText）；
//   - created_at 服务端注入 types.NowUTC（AC5.4 多机时钟免疫）；
//   - 经 RETURNING 返回库中真实行（含自增 id 与注入时间——#31 响应
//     {id,created_at} 数据面；SessionName/Stale 不装配，恒零值）。
func (s *Store) InsertProgress(p ProgressReport) (ProgressReport, error) {
	row := s.DB.QueryRow(
		`INSERT INTO progress_reports
		     (session_id, batch, task, commit_hash, branch, test_status, summary, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 RETURNING `+progressColumns,
		p.SessionID, nullableText(p.Batch), nullableText(p.Task), nullableText(p.CommitHash),
		nullableText(p.Branch), nullableText(p.TestStatus), nullableText(p.Summary), types.NowUTC(),
	)
	got, err := scanProgress(row.Scan)
	if err != nil {
		return ProgressReport{}, fmt.Errorf("store: 插入进度上报（session=%d）失败: %w", p.SessionID, err)
	}
	return got, nil
}

// ListProgress 进度上报历史明细（#32 GET /progress 数据面）：projectID 0=全部
// 项目、sessionName 空=全部会话（#32 query 可选语义；code→id 换装与不存在 404
// 主责在 handler——#28 GetProjectByCode 换装先例，store 数据面查无即空不报错）；
// JOIN sessions 单条 SELECT 完成过滤+会话名装配+分页（b8-spec §七往返账
// 「#32 = 单条 SELECT」）；按 created_at DESC,id DESC 倒序（idx_progress_created
// 支撑，同秒多报按 id 决序）；limit 口径见 ProgressLimit* 常量注；Stale 不计算
// （#32 字段面无 stale）恒 ProgressStaleNone；空结果非 nil 空切片。
func (s *Store) ListProgress(projectID int64, sessionName string, limit int) ([]ProgressReport, error) {
	if limit <= 0 {
		limit = ProgressLimitDefault
	}
	if limit > ProgressLimitMax {
		limit = ProgressLimitMax
	}
	var conds []string
	var args []any
	if projectID != 0 {
		conds = append(conds, "se.project_id = ?")
		args = append(args, projectID)
	}
	if sessionName != "" {
		conds = append(conds, "se.name = ?")
		args = append(args, sessionName)
	}
	// 条件白名单片段拼接，值全走 ? 占位（QueryAudit/UpdateProject 同款惯例）。
	q := `SELECT ` + progressJoinColumns + `
		 FROM progress_reports pr
		 JOIN sessions se ON se.id = pr.session_id`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY pr.created_at DESC, pr.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询进度上报列表失败: %w", err)
	}
	defer rows.Close()
	out := make([]ProgressReport, 0)
	for rows.Next() {
		p, err := scanProgressRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描进度上报行失败: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历进度上报列表失败: %w", err)
	}
	return out, nil
}

// LatestProgressBySession 每会话最近一条进度上报（#17 status 聚合 progress 段与
// 看板消费面，B3-5 将消费；spec §1.1 LatestBySession 行）：
//
// 往返账（b8-spec §七「窗口函数或自 JOIN 单条（禁 N+1）」）：全程恰 1 次 DB 往返
// ——子查询 ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY created_at DESC,
// id DESC) 取 rn=1（同秒 tie 由 id DESC 决序，与 ListProgress 排序键一致），
// 外层 JOIN sessions 装配会话名，禁 Go 侧循环逐会话查询。
//
// now=判定基准时刻（ISO8601 UTC 文本，GetSession 的 now 同形态注入参数——聚合
// 场景多行共享同一 now 保判定一致，不依赖 time.Now 内部调用）；
// staleAfterSec=黄档阈值秒数（config progress.stale_after 换算注入，B8-6 消费方；
// 默认 60m=3600；红档=2× 自动推导，本层不读 config）；stale 三态与边界口径见
// ProgressStale* 常量注。输出按 created_at DESC,id DESC（最新活动会话在前）；
// 空结果非 nil 空切片。
func (s *Store) LatestProgressBySession(now string, staleAfterSec int) ([]ProgressReport, error) {
	// 单条 SQL（往返=1，禁 N+1）：子查询窗口函数取每会话最新行，外层 JOIN 装配会话名。
	q := `SELECT ` + progressJoinColumns + `
		 FROM (
		   SELECT id, session_id, batch, task, commit_hash, branch, test_status, summary, created_at,
		          ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY created_at DESC, id DESC) AS rn
		   FROM progress_reports
		 ) pr
		 JOIN sessions se ON se.id = pr.session_id
		 WHERE pr.rn = 1
		 ORDER BY pr.created_at DESC, pr.id DESC`

	rows, err := s.DB.Query(q)
	if err != nil {
		return nil, fmt.Errorf("store: 查询每会话最新进度失败: %w", err)
	}
	defer rows.Close()
	out := make([]ProgressReport, 0)
	for rows.Next() {
		p, err := scanProgressRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描最新进度行失败: %w", err)
		}
		// stale 判定在 Go 侧逐行完成（B8-T1 查询侧口径；无额外 DB 往返）。
		p.Stale, err = progressStaleFlag(p.CreatedAt, now, staleAfterSec)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历最新进度失败: %w", err)
	}
	return out, nil
}
