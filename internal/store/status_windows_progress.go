// status_windows_progress.go —— status 聚合的 windows 压缩视图与 progress 段装配
// （B3-5）：windows 消费 B4 产物 ListWindowStatus（禁自含复刻——信箱#11 分层），
// progress 消费 B8 产物 LatestProgressBySession（FR23 增补，b3-spec 行 137，
// AC23.3 端点面归 B3-5）。
//
// 独立文件依据 base.md §4.8 功能域分文件：status.go 已近行数软上限，B3-5 装配
// 增量（往返 +3：窗聚合两查+进度一查）不进主文件，BuildOverview 内两处调用点接线。
//
// 往返纪律：两段装配均为一次全量查询 + 内存按归属装配（禁逐栏目/逐会话循环查——
// §3.7 尾注；生效范围外的条目经归属索引自然落空，不另起过滤查询）。
package store

// DefaultProgressStaleAfterSec progress.stale_after 默认 60m=3600 秒（§9.3）。
// handler 层 config 坏值回退同值（parseStaleAfterSec）——单一阈值来源勿另立。
const DefaultProgressStaleAfterSec = 3600

// WindowPhaseView §2.3 windows 压缩视图条目（key=stage）：ColumnOverview.Windows
// 的值面，status/start/end 三键恒齐（只列已配置启用阶段——未配置/停用无窗可判
// 不列，AC3.4 视同未配置不受限）。
type WindowPhaseView struct {
	Status string `json:"status"` // allowed | waiting（ListWindowStatus 判定态透传）
	Start  string `json:"start"`  // 窗启 "HH:MM"（start>end=跨午夜原样透出，AC3.3）
	End    string `json:"end"`    // 窗止 "HH:MM"
}

// SessionProgress §2.3 sessions[].progress 段（FR23 增补，PRD AC23.3）：每会话
// 最近一次进度上报摘要 + stale 三态标记（值取 ProgressReport 对应字段，stale 直接
// 消费 store 三态 ProgressStaleNone/Yellow/Red——""=正常/stale=黄/dead=红）。
// SessionEntry.Progress 为指针 + omitempty：无上报记录的会话不出该键。
type SessionProgress struct {
	CommitHash string `json:"commit_hash"` // 可空串（post-commit hook 自动层可缺省）
	Batch      string `json:"batch"`
	Task       string `json:"task"`
	CreatedAt  string `json:"created_at"` // 最近上报时刻（ISO8601 UTC）
	Stale      string `json:"stale"`      // ""=正常 / "stale"=黄 / "dead"=红
}

// scopeProjectCode 单栏目模式的项目过滤参数透传（ColumnScope→ListWindowStatus
// 的 projectCode 形参；nil=全局无过滤）。
func scopeProjectCode(scope *ColumnScope) string {
	if scope == nil {
		return ""
	}
	return scope.ProjectCode
}

// resolveStaleAfterSec 进度 stale 阈值兜底：opts 零值=默认 3600（handler 已解析
// config 时长串传入，0 表示调用方未指定——与 SessionTimeoutSec 同款零值兜底口径）。
func resolveStaleAfterSec(v int) int {
	if v == 0 {
		return DefaultProgressStaleAfterSec
	}
	return v
}

// assembleWindows 把 ListWindowStatus 聚合条目转 §2.3 windows 压缩视图挂到对应
// 栏目（零额外查询——条目已在内存）：
//   - 只列已配置且启用（entry.Window != nil）的阶段，未配置/停用阶段不列（压缩
//     视图语义，value 恒三键齐）；
//   - 超出生效范围（单栏目模式外的栏目）的条目经归属索引落空。
//
// Windows map 由 BuildOverview 往返 2 构造点初始化（恒非 nil），此处直接写键。
func assembleWindows(projects []ProjectOverview, entries []WindowStatusEntry) {
	type colRef struct {
		p, c int // 项目/栏目切片下标
	}
	idx := make(map[[2]string]colRef, len(projects))
	for pi := range projects {
		for ci := range projects[pi].Columns {
			idx[[2]string{projects[pi].Code, projects[pi].Columns[ci].Code}] = colRef{pi, ci}
		}
	}
	for _, e := range entries {
		ref, ok := idx[[2]string{e.ProjectCode, e.ColumnCode}]
		if !ok || e.Window == nil {
			continue // 生效范围外栏目 / 未配置或停用（无窗可判不进压缩视图）
		}
		projects[ref.p].Columns[ref.c].Windows[e.Stage] = WindowPhaseView{
			Status: e.Status,
			Start:  e.Window.Start,
			End:    e.Window.End,
		}
	}
}

// assembleProgress 把每会话最新进度（LatestProgressBySession 全量，stale 三态已由
// store 算好）按 SessionID 挂到聚合视图会话行（零额外查询；生效范围外会话经
// sessionByID 落空；同会话至多一条——窗口函数 rn=1 语义由查询侧保证）。
func assembleProgress(projects []ProjectOverview, sessionByID map[int64]sessionRef, reports []ProgressReport) {
	for _, p := range reports {
		ref, ok := sessionByID[p.SessionID]
		if !ok {
			continue
		}
		projects[ref.projIdx].Sessions[ref.index].Progress = &SessionProgress{
			CommitHash: p.CommitHash,
			Batch:      p.Batch,
			Task:       p.Task,
			CreatedAt:  p.CreatedAt,
			Stale:      p.Stale,
		}
	}
}
