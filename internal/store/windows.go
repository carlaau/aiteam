package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // §9.4 D4：Windows 无系统 tzdata 时随二进制嵌入 IANA 库兜底（标准库，无第三方）
)

// InWindow 报告 now 是否落在 [start,end] 时间窗内（技术设计 §6.2 伪码原样）。
// now/start/end 均为 "HH:MM" 等宽字符串；start<=end 为同日闭区间；start>end 为跨午夜区间。
// 注：技术设计 §6.2 注释称 start==end=全天，伪码字面实际行为=仅相等点命中
// （start<=now<=end）；本实现取伪码字面，行为已由测试固定（裁定呈总控中）。
func InWindow(now, start, end string) bool {
	if start <= end { // 同日区间（start==end 语义见 doc 注）
		return start <= now && now <= end
	}
	// 跨午夜区间（如 23:00–09:00 = [23:00,24:00) ∪ [00:00,09:00]），闭区间含端点
	return now >= start || now <= end
}

// ===== B4-4 续段：stage_windows 覆盖式写 =====

// ErrDuplicateStage 同一提交内重复 stage 预校验哨兵错误（§2.2 #21；schema
// UNIQUE(project_id,column_id,stage) 仅作兜底）。handler 层据 errors.Is 映射 400。
var ErrDuplicateStage = errors.New("store: 同一提交内重复 stage")

// WindowInput 一次提交中的单个阶段时间窗（detail 快照 JSON tag 同 #21 请求字段名）。
type WindowInput struct {
	Stage   string `json:"stage"`   // 阶段名（如 S4；格式校验归端点批——store 层只防重复）
	From    string `json:"from"`    // 开始 "HH:MM"（格式校验归端点批）
	To      string `json:"to"`      // 结束 "HH:MM"（start>end=跨午夜，合法不拒——AC3.3）
	Enabled int    `json:"enabled"` // 1=启用；0=停用（停用行照常写入保留）
}

// SetWindows 覆盖式写阶段时间窗（§2.2 #21 数据面）：一次提交该 (project,column) 域
// 全部窗配置，**缺席=删除**。签名微调注：createdBy 相比 plan 的 (project,column,windows,now)
// 形态追加操作者身份入参——审计问责面（b4-spec：操作者身份入审计）与资源侧对齐。
//
// 边界注记：
//   - columnId=0 表示项目级默认窗（schema 语义），与栏目级各自成域互不覆盖；
//   - start>end（跨午夜）合法不拒；HH:MM 格式与 stage 格式校验归端点批（B4-T2），
//     store 层不做——本层唯一预校验=同一提交内重复 stage（拒绝发生在任何写之前，
//     原库数据零触碰；UNIQUE 约束仅作并发兜底）；
//   - windows 为空=清空该域全部窗（DELETE 照常执行，跳过 INSERT）。
//
// 单事务三语句：DELETE 该域全行 + 一条多 VALUES 批量 INSERT（一次绑定提交，禁逐条
// 独立事务）+ INSERT audit_log（action=window.set，detail=提交快照 JSON）。
// 时间一律取注入时钟 now（服务端时间），不读墙钟。
func (s *Store) SetWindows(projectID, columnID int64, windows []WindowInput, createdBy int64, now string) error {
	// 预校验：同一提交内重复 stage（内存判重，先于一切写——保证原数据不被破坏）。
	seen := make(map[string]struct{}, len(windows))
	for _, w := range windows {
		if _, dup := seen[w.Stage]; dup {
			return ErrDuplicateStage
		}
		seen[w.Stage] = struct{}{}
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("store: 开启时间窗事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 覆盖式：DELETE 该 (project,column) 域全行（缺席=删除的语义核）。
	if _, err := tx.Exec(
		`DELETE FROM stage_windows WHERE project_id = ? AND column_id = ?`, projectID, columnID,
	); err != nil {
		return fmt.Errorf("store: 删除原时间窗失败: %w", err)
	}

	// 批量 INSERT：一条多 VALUES 语句一次绑定提交（占位符为 '?' 字面量拼接，无注入面）。
	// 上限实测：每窗 8 占位符，SQLite MAX_VARIABLE_NUMBER=32766 → 单批上限 4095 窗；
	// 现实单域窗数个位~十位不可能撞；超限（4096 窗）报 too many SQL variables 且事务
	// 整体回滚、原数据保留——不设分批（YAGNI）。
	if len(windows) > 0 {
		var sb strings.Builder
		sb.WriteString(`INSERT INTO stage_windows (project_id, column_id, stage, start_time, end_time, enabled, created_at, updated_at) VALUES `)
		args := make([]any, 0, len(windows)*8)
		for i, w := range windows {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`(?,?,?,?,?,?,?,?)`)
			args = append(args, projectID, columnID, w.Stage, w.From, w.To, w.Enabled, now, now)
		}
		if _, err := tx.Exec(sb.String(), args...); err != nil {
			return fmt.Errorf("store: 批量写入时间窗失败: %w", err)
		}
	}

	// INSERT audit_log（action=window.set，detail=提交快照）。
	detail, err := json.Marshal(map[string]any{
		"project_id": projectID, "column_id": columnID, "windows": windows,
	})
	if err != nil {
		return fmt.Errorf("store: 组装时间窗快照失败: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO audit_log (project_id, column_id, session_id, action, detail, created_at)
		 VALUES (?, ?, ?, 'window.set', ?, ?)`,
		projectID, columnID, createdBy, string(detail), now,
	); err != nil {
		return fmt.Errorf("store: 写时间窗审计失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交时间窗事务失败: %w", err)
	}
	return nil
}

// ===== B4-6b 续段：#21 修订行 GET 读侧（窗配置原始行，管理面全量）=====

// ListWindows 读当前域窗配置原始行（技术设计 #21 修订行：同 path 加 GET，
// B4-6b）：columnID=0 即项目级域（对齐 SetWindows 的域语义）。返回原始行含
// enabled=0 停用行（管理面全量，不做 #22 判定态过滤——判定态归 ListWindowStatus）。
// 行结构复用 WindowInput（与 #21 items 形态同构，handler 直映射）。
// 按 stage 升序稳定排序（域内 stage UNIQUE，全序确定）；空域返回非 nil 空切片。
func (s *Store) ListWindows(projectID, columnID int64) ([]WindowInput, error) {
	rows, err := s.DB.Query(
		`SELECT stage, start_time, end_time, enabled FROM stage_windows
		 WHERE project_id = ? AND column_id = ? ORDER BY stage`, projectID, columnID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询时间窗配置失败: %w", err)
	}
	defer rows.Close()
	out := make([]WindowInput, 0) // 非 nil 保证：空域序列化为 []
	for rows.Next() {
		var w WindowInput
		if err := rows.Scan(&w.Stage, &w.From, &w.To, &w.Enabled); err != nil {
			return nil, fmt.Errorf("store: 扫描时间窗配置失败: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历时间窗配置失败: %w", err)
	}
	return out, nil
}

// ===== B4-5 续段：#22 「当前允许阶段」聚合读侧 + NowInTz =====

// NowInTz 服务端判窗取钟（§9.4 D4 / §6.1 时间基准）：返回时区 tz 下当前时刻的
// "HH:MM" 等宽文本。tz 取 "Local"（服务进程时区，config 默认）或 IANA 名
// （如 Asia/Shanghai）；空串按 LoadLocation 语义回落 UTC。解析失败返回 error
// 由调用方决策（handler 映射 500，本函数不静默兜底）。
// 包级变量=测试注入点（handler 端到端测 as_of 敏感状态翻转；仅限串行用例替换，
// 与 types.NowUTC 注入模式同构），生产代码勿在运行期改写。
var NowInTz = func(tz string) (string, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return "", fmt.Errorf("store: 加载时区 %q 失败: %w", tz, err)
	}
	return time.Now().In(loc).Format("15:04"), nil
}

// WindowSpan 时间窗快照（#22 entries.window 字段面：{start,end}；nil=null）。
type WindowSpan struct {
	Start string // "HH:MM"
	End   string // "HH:MM"（start>end=跨午夜原样透出，AC3.3）
}

// WindowStatusEntry 「当前允许阶段」聚合条目（§6.3 输出面；project/column 出 code
// 不出 id，与 #20 条目人读对齐口径一致）。
type WindowStatusEntry struct {
	ProjectCode string
	ColumnCode  string
	Stage       string      // "S0".."S7" 枚举 ∪ 配置行枚举外阶段（并集防漏报）
	Status      string      // allowed | waiting
	Window      *WindowSpan // nil=未配置/停用（无窗可判）
}

// enumeratedStages §6.3 第 3 步的阶段枚举全集（S0..S7；与配置行 stage 并集防漏报）。
var enumeratedStages = []string{"S0", "S1", "S2", "S3", "S4", "S5", "S6", "S7"}

// winRowAgg 聚合内存视图中的窗行（SELECT 原始值快照）。
type winRowAgg struct {
	Stage   string
	Start   string
	End     string
	Enabled int
}

// ListWindowStatus 「当前允许阶段」聚合（§6.3 伪码逐步对拍，B4-5 #22 数据面）：
//
//	输入 as_of 为判窗 "HH:MM"（时钟全注入：handler 经 NowInTz 产生后传入，
//	本函数不读钟）；projectCode 非空按项目 code 过滤，"" =全部项目。
//	①两条整体 SELECT 预拉输入面（栏目清单 + 相关窗行各一条，禁逐域循环查库），
//	②内存 map[(column_id,stage)] 栏目级行覆盖项目级行，
//	③阶段全集 = S0..S7 ∪ 该域可见窗行 stage（per-域并集：枚举外阶段不跨域串台），
//	④每 (栏目,阶段) 判定：无窗或 enabled=0 → allowed+nil（AC3.4）；有窗且
//	  InWindow → allowed+window；否则 waiting+window（AC3.2 窗外挂起非报错）。
//
// 输出按 project code → column code → stage 升序（稳定确定性，测试可对拍）。
// 读口宽容：projectCode 查无 = 空集（对齐 #20 口径，非错误）；archived 域不过滤
// （对齐 #13「archived 域不拒」口径）。
func (s *Store) ListWindowStatus(asOf, projectCode string) ([]WindowStatusEntry, error) {
	// 输入面①：栏目清单（join projects 取双侧 code；projectCode 过滤一条 WHERE）。
	colQuery := `SELECT c.id, c.code, p.id, p.code FROM columns c JOIN projects p ON p.id = c.project_id`
	colArgs := []any{}
	if projectCode != "" {
		colQuery += ` WHERE p.code = ?`
		colArgs = append(colArgs, projectCode)
	}
	colQuery += ` ORDER BY p.code, c.code`

	type colRef struct {
		id       int64
		code     string
		projID   int64
		projCode string
	}
	cols := []colRef{}
	rows, err := s.DB.Query(colQuery, colArgs...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询栏目清单失败: %w", err)
	}
	for rows.Next() {
		var c colRef
		if err := rows.Scan(&c.id, &c.code, &c.projID, &c.projCode); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: 扫描栏目清单失败: %w", err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("store: 遍历栏目清单失败: %w", err)
	}
	_ = rows.Close()

	// 输入面②：相关窗行一条 SELECT（栏目级 + 项目级 column_id=0 全量）。
	winQuery := `SELECT sw.project_id, sw.column_id, sw.stage, sw.start_time, sw.end_time, sw.enabled
FROM stage_windows sw JOIN projects p ON p.id = sw.project_id`
	winArgs := []any{}
	if projectCode != "" {
		winQuery += ` WHERE p.code = ?`
		winArgs = append(winArgs, projectCode)
	}
	rows, err = s.DB.Query(winQuery, winArgs...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询时间窗失败: %w", err)
	}
	defer rows.Close()

	// ②内存 map：栏目级/项目级分表存，查询时栏目级优先（显式优先级，免同 map
	// 后写覆盖对插入序的隐式依赖）。
	colWins := map[int64]map[string]winRowAgg{}
	projWins := map[int64]map[string]winRowAgg{}
	for rows.Next() {
		var projectID, columnID int64
		var w winRowAgg
		if err := rows.Scan(&projectID, &columnID, &w.Stage, &w.Start, &w.End, &w.Enabled); err != nil {
			return nil, fmt.Errorf("store: 扫描时间窗失败: %w", err)
		}
		if columnID == 0 { // 项目级默认窗（schema 语义）
			if projWins[projectID] == nil {
				projWins[projectID] = map[string]winRowAgg{}
			}
			projWins[projectID][w.Stage] = w
		} else {
			if colWins[columnID] == nil {
				colWins[columnID] = map[string]winRowAgg{}
			}
			colWins[columnID][w.Stage] = w
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历时间窗失败: %w", err)
	}

	// ③④组装：每栏目 × per-域阶段全集（S0..S7 ∪ 栏目级 ∪ 项目级），stage 内升序。
	entries := make([]WindowStatusEntry, 0, len(cols)*len(enumeratedStages))
	for _, c := range cols {
		stageSet := map[string]struct{}{}
		for _, st := range enumeratedStages {
			stageSet[st] = struct{}{}
		}
		for st := range colWins[c.id] {
			stageSet[st] = struct{}{}
		}
		for st := range projWins[c.projID] {
			stageSet[st] = struct{}{}
		}
		stages := make([]string, 0, len(stageSet))
		for st := range stageSet {
			stages = append(stages, st)
		}
		slices.Sort(stages)

		for _, st := range stages {
			// 栏目级优先，缺席回落项目级（§6.3 第 2 步覆盖语义）。
			w, ok := colWins[c.id][st]
			if !ok {
				w, ok = projWins[c.projID][st]
			}
			e := WindowStatusEntry{ProjectCode: c.projCode, ColumnCode: c.code, Stage: st}
			if !ok || w.Enabled == 0 { // 无窗或停用：视同未配置不受限（AC3.4）
				e.Status = "allowed"
			} else if InWindow(asOf, w.Start, w.End) {
				e.Status = "allowed"
				e.Window = &WindowSpan{Start: w.Start, End: w.End}
			} else { // 窗外挂起非报错（AC3.2）
				e.Status = "waiting"
				e.Window = &WindowSpan{Start: w.Start, End: w.End}
			}
			entries = append(entries, e)
		}
	}
	return entries, nil // cols 为空（含 projectCode 查无）时零长非 nil
}
