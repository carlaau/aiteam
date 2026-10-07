// handler_status.go —— B3-5 #17 status 端点（§2.2 #17 / §2.3 响应结构；b3-plan B3-5）。
//
// 本 handler 纯装配透传：聚合判定全在 store.BuildOverview（§3.7 单一实现，含 B4 窗
// 压缩视图与 FR23 progress 段——B3-5 接线）；mode=column 与 overview 同函数带过滤参数
// （b3-spec §七「column 同聚合+过滤参数，不另起炉灶」）。错误面：param_invalid 400
// （mode 非法 / mode=column 缺 project 或 column 参数）/ column_not_found 404 /
// internal_error 500 兜底（时区配置坏值、聚合查询失败）。
package server

import (
	"fmt"
	"net/http"
	"time"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// parseStaleAfterSec config progress.stale_after 时长串→秒（信箱#17③：B8 呈裁②——
// 解析职责落 B3-5，store 收 int 秒）。语法=Go time.ParseDuration 全集（"60m"/"90s"/
// "1h30m" 复合均可）；非法串、缺单位、非正值、亚秒截断为 0 → 一律回退默认 3600
// （§9.3 服务配置坏值不放大到端点行为——运行期兜底口径，与启动期 Validate fail-fast
// 分层；阈值偏离默认只影响 stale 标记灵敏度，不产生错误响应）。
func parseStaleAfterSec(raw string) int {
	if raw == "" {
		return store.DefaultProgressStaleAfterSec
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return store.DefaultProgressStaleAfterSec
	}
	secs := int(d.Seconds())
	if secs <= 0 {
		return store.DefaultProgressStaleAfterSec // 亚秒配置（如 500ms）截断为 0 → 回退
	}
	return secs
}

// handleStatus GET /api/v1/status（§2.2 #17：CLI status --global/--project 与看板首屏
// 同一数据源，AC14.1 同源；AC11.4 <5s 由 §3.7 往返基线保证——TestLatency 端到端对拍）。
//
// query 参数面：
//   - mode：缺省或 "overview"=全局全量；"column"=单栏目模式（同结构限定单栏目输出，
//     须带 project+column 双参数——ColumnScope 定位面）；其他值 400 param_invalid
//     （防拼错静默回 overview 的「看似成功实未生效」）；
//   - all：仅 overview 模式有语义——all=1 时 OverviewOpts.IncludeArchived=true（含
//     archived 项目，总控 #20 裁定消 TODO(B3-7-all) 占位）。严格 == "1"（其他值忽略
//     ——宽松容错不引入误语义：all=0/true 等回落缺省面而非报错，与 400 的 mode 分寸
//     有意区分——mode 错=定位歧义必须拦截，all 值错=扩面开关未生效不产生定位歧义）；
//     mode=column 时忽略（单栏目=精确定位单栏目，all 无扩面语义——裁量声明，不 400）；
//   - project/column：mode=column 时必带；栏目（或项目）未登记 → 404 column_not_found
//     （ResolveColumn 项目缺失归一口径，§2.2 #18 先例——#17 特有错误语义单一出口）。
//
// 时钟双注入（时钟纪律）：now=types.NowUTC()（RFC3339——失联与 progress stale 共用
// 同一判定时刻）；as_of=store.NowInTz(cfg.Timezone)（判窗 HH:MM，§9.4 D4——与 #22
// windows/now 同源钟面；时区解析失败 500 不静默兜底改钟面）。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := store.OverviewOpts{
		SessionTimeoutSec:     int64(s.cfg.Session.HeartbeatTimeoutSec),
		SentinelTimeoutSec:    int64(s.cfg.Watch.SentinelTimeoutSec),
		ProgressStaleAfterSec: parseStaleAfterSec(s.cfg.Progress.StaleAfter),
	}
	switch mode := q.Get("mode"); mode {
	case "", "overview":
		// 全局全量（缺省=overview——CLI status --global 与看板首屏直连形态）；
		// all=1 扩面含 archived 项目（总控 #20 裁定，消 TODO(B3-7-all) 占位）。
		opts.IncludeArchived = q.Get("all") == "1"
	case "column":
		projectCode, columnCode := q.Get("project"), q.Get("column")
		if projectCode == "" || columnCode == "" {
			types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
				"mode=column 须同时带 project 与 column 参数")
			return
		}
		if _, err := s.st.ResolveColumn(projectCode, columnCode); err != nil {
			s.writeStoreError(w, err) // ErrColumnNotFound→404 column_not_found（项目缺失归一）
			return
		}
		opts.Column = &store.ColumnScope{ProjectCode: projectCode, ColumnCode: columnCode}
	default:
		types.WriteError(w, http.StatusBadRequest, types.CodeParamInvalid,
			fmt.Sprintf("mode %q 须为 overview 或 column", q.Get("mode")))
		return
	}

	// 判窗钟面（#22 同源）：NowInTz 为包级注入点（测试替换即生效）。
	asOf, err := store.NowInTz(s.cfg.Timezone)
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError,
			fmt.Sprintf("服务端时区配置 %q 不可用", s.cfg.Timezone))
		return
	}
	opts.WindowAsOf = asOf

	ov, err := s.st.BuildOverview(types.NowUTC(), opts)
	if err != nil {
		types.WriteError(w, http.StatusInternalServerError, codeInternalError, "status 聚合查询失败")
		return
	}
	types.WriteData(w, http.StatusOK, ov)
}
