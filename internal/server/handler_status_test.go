// handler_status_test.go —— B3-5 #17 status 端点 handler 测试（§2.2 #17 / §2.3 响应结构；
// b3-plan B3-5：TestOverview / TestColumnMode / TestLatency / TestProjectVisible，
// 附 AC23.3 progress 段与 stale_after 解析单测）。
//
// 时钟纪律：types.NowUTC（RFC3339，失联/progress stale 判定）与 store.NowInTz（判窗
// HH:MM）双注入点固定，零真实墙钟。夹具：项目/栏目/会话/位点/消息/哨兵直插（复用
// handlers_misc_test.go 与 handler_sentinels_test.go 基座 helper），窗/进度经 store
// 公开 API（SetWindows/InsertProgress）——隔离被测面只经 HTTP。
package server

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"aiteam/internal/config"
	"aiteam/internal/store"
	"aiteam/internal/types"
)

// status 域测试固定时刻：T0=progress 上报时刻；Red=T0+7400s（>2×3600 红档）兼作
// 查询时钟——会话 last_seen_at 相对 Red 构造 alive 态（差 10s/20s ≤ 默认 900）。
const (
	statusT0  = "2026-10-02T13:00:00Z"
	statusRed = "2026-10-02T15:03:20Z" // +7400s
)

// seedColumnRow 直插栏目行（同项目第二栏目起经此入口——seedProjectColumn 每次新建
// 项目，同项目多栏目须复用 project id）。
func seedColumnRow(t *testing.T, st *store.Store, projectID int64, code string) int64 {
	t.Helper()
	const ts = "2026-01-01T00:00:00Z"
	res, err := st.DB.Exec(
		`INSERT INTO columns (project_id, code, name, status, created_at, updated_at)
		 VALUES (?, ?, ?, 'active', ?, ?)`, projectID, code, "栏目"+code, ts, ts,
	)
	if err != nil {
		t.Fatalf("插入栏目夹具 %q 失败: %v", code, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取栏目夹具 %q 自增 id 失败: %v", code, err)
	}
	return id
}

// seedStatusFixtures #17 测试共享夹具：proj-a{05,06}+proj-b{05} 三栏目两项目；
// executor-B@05（progress 上报者+alive 哨兵挂载）、controller-A@06（无 progress 对照）；
// 05 栏目 executor 信箱位点=m1（pending=1）、latest=m2；block 未回执一条；
// S4 跨午夜窗（as_of=10:00 → waiting）/S5 同日窗内（allowed）/S6 停用（压缩视图不列）；
// in_use 资源一条。返回 executor-B 会话 id 与两条 direct 消息 seq 供断言。
func seedStatusFixtures(t *testing.T, st *store.Store) (execSess, m1, m2 int64) {
	t.Helper()
	pidA, cid05 := seedProjectColumn(t, st, "proj-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	cid06 := seedColumnRow(t, st, pidA, "06")
	seedProjectColumn(t, st, "proj-b", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	// 会话 last_seen_at 相对查询时钟 statusRed 构造（alive 态）。
	execSess = seedSessionRow(t, st, pidA, cid05, "executor-B", "executor",
		"2026-10-02T15:03:10Z", "2026-10-02T12:00:00Z")
	seedSessionRow(t, st, pidA, cid06, "controller-A", "controller",
		"2026-10-02T15:03:00Z", "2026-10-02T12:00:00Z")

	// progress 上报于 statusT0：查询时钟=statusRed → 差 7400s > 2×3600 → 红档 stale。
	injectFixedClock(t, statusT0)
	if _, err := st.InsertProgress(store.ProgressReport{
		SessionID: execSess, Batch: "B3", Task: "B3-5", CommitHash: "abc1234",
	}); err != nil {
		t.Fatalf("上报进度夹具失败: %v", err)
	}

	// 05 executor 信箱：位点=m1 → pending=1（m2 未消费）；latest=m2（含已消费）。
	m1 = insertMessage(t, st, cid05, "direct", "executor", 0, execSess, "normal")
	m2 = insertMessage(t, st, cid05, "direct", "executor", 0, execSess, "normal")
	insertPosition(t, st, cid05, "executor", m1)
	// block 未回执一条（controller 信箱出现面 + block_unreceipted 清单断言面）。
	insertMessage(t, st, cid05, "direct", "controller", 0, execSess, "block")

	// alive 哨兵挂 executor-B（last_ping 距查询时钟 10s ≤ 15 → alive）。
	seedSentinel(t, st, execSess, cid05, "executor", "2026-10-02T15:03:10Z")

	// 阶段窗：S4 跨午夜（as_of=10:00 窗外 → waiting）、S5 同日窗内（allowed）、
	// S6 停用（enabled=0 → 压缩视图不列）。
	if err := st.SetWindows(pidA, cid05, []store.WindowInput{
		{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1},
		{Stage: "S5", From: "08:00", To: "12:00", Enabled: 1},
		{Stage: "S6", From: "08:00", To: "12:00", Enabled: 0},
	}, execSess, statusT0); err != nil {
		t.Fatalf("配置时间窗夹具失败: %v", err)
	}

	// in_use 资源一条（resources_summary 断言面）。
	if _, err := st.RegisterResource(pidA, cid05, "port", "8080", "", execSess, statusT0); err != nil {
		t.Fatalf("登记资源夹具失败: %v", err)
	}
	return execSess, m1, m2
}

// TestOverview §2.3 响应结构逐字段（mode=overview 全局）：顶层五键（含 B5-6 增补
// config 回显段）、项目/栏目/信箱/windows 压缩视图/会话+progress 段/block 未回执/
// 资源摘要；红档 stale 与无上报会话 progress 键省略（omitempty）双分支。
func TestOverview(t *testing.T) {
	h, st := newResTestServer(t)
	_, m1, m2 := seedStatusFixtures(t, st)

	// 查询时钟=红档时刻；判窗 as_of=10:00（S4 waiting / S5 allowed）。
	injectFixedClock(t, statusRed)
	injectFixedHHMM(t, "10:00")

	rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/status", "",
		"proj-a", "05", "executor-B", "executor")
	data := wantData(t, rr, http.StatusOK)

	// 顶层五键（B5-6 起 +config 回显段）+ generated_at=注入时钟。
	wantKeys(t, data, "generated_at", "projects", "block_unreceipted", "resources_summary", "config")
	if data["generated_at"] != statusRed {
		t.Errorf("generated_at = %v, want 注入时钟 %s", data["generated_at"], statusRed)
	}

	// 项目层：proj-a + proj-b（ORDER BY code）。
	projects := decodeRows(t, data["projects"], "data.projects")
	if len(projects) != 2 || projects[0]["code"] != "proj-a" || projects[1]["code"] != "proj-b" {
		t.Fatalf("projects = %v, want [proj-a proj-b]", projects)
	}
	pa := projects[0]
	if pa["name"] != "项目proj-a" || pa["status"] != "active" {
		t.Errorf("proj-a 字段 = (%v, %v), want (项目proj-a, active)", pa["name"], pa["status"])
	}

	// 栏目层：05 + 06（ORDER BY project_id, code）。
	cols := decodeRows(t, pa["columns"], "proj-a.columns")
	if len(cols) != 2 || cols[0]["code"] != "05" || cols[1]["code"] != "06" {
		t.Fatalf("proj-a columns = %v, want [05 06]", cols)
	}
	col05 := cols[0]
	wantKeys(t, col05, "code", "status", "mailboxes", "windows")

	// 信箱层：executor（pending/position/latest/sentinel 四面）。
	mailboxes := decodeRows(t, col05["mailboxes"], "05.mailboxes")
	var mb map[string]any
	for _, row := range mailboxes {
		if row["role"] == "executor" {
			mb = row
		}
	}
	if mb == nil {
		t.Fatalf("05 缺 executor 信箱: %v", mailboxes)
	}
	wantKeys(t, mb, "role", "pending", "position", "latest", "sentinel")
	if mb["pending"] != float64(1) {
		t.Errorf("executor pending = %v, want 1（位点 %d 之后 1 条）", mb["pending"], m1)
	}
	if mb["position"] != float64(m1) {
		t.Errorf("executor position = %v, want %d", mb["position"], m1)
	}
	latest, ok := mb["latest"].(map[string]any)
	if !ok {
		t.Fatalf("executor latest 非对象: %v", mb["latest"])
	}
	if latest["seq"] != float64(m2) || latest["level"] != "normal" {
		t.Errorf("executor latest = %v, want {seq:%d, normal}", latest, m2)
	}
	if _, has := latest["created_at"]; !has {
		t.Errorf("latest 缺 created_at 键（§2.3 三键面）: %v", latest)
	}
	if mb["sentinel"] != "alive" {
		t.Errorf("executor sentinel = %v, want alive", mb["sentinel"])
	}

	// windows 压缩视图：只列已配置启用阶段；S4 跨午夜窗外=waiting、S5 窗内=allowed。
	wins, ok := col05["windows"].(map[string]any)
	if !ok {
		t.Fatalf("05.windows 非对象: %v", col05["windows"])
	}
	if len(wins) != 2 {
		t.Fatalf("05.windows = %v, want 恰 {S4,S5}（S6 停用不列）", wins)
	}
	s4, ok := wins["S4"].(map[string]any)
	if !ok || s4["status"] != "waiting" || s4["start"] != "23:00" || s4["end"] != "09:00" {
		t.Errorf("windows.S4 = %v, want {waiting,23:00,09:00}（as_of=10:00 跨午夜窗外）", wins["S4"])
	}
	s5, ok := wins["S5"].(map[string]any)
	if !ok || s5["status"] != "allowed" || s5["start"] != "08:00" || s5["end"] != "12:00" {
		t.Errorf("windows.S5 = %v, want {allowed,08:00,12:00}（as_of=10:00 窗内）", wins["S5"])
	}
	// 06 栏目未配置窗：windows 空对象（键恒在，非 null）。
	wins06, ok := cols[1]["windows"].(map[string]any)
	if !ok || len(wins06) != 0 {
		t.Errorf("06.windows = %v, want 空对象 {}", cols[1]["windows"])
	}

	// 会话层：§2.3 字段面 + FR23 progress 段（红档 stale）+ #27 内嵌 sentinels。
	sessions := decodeRows(t, pa["sessions"], "proj-a.sessions")
	if len(sessions) != 2 {
		t.Fatalf("proj-a sessions = %d 行, want 2", len(sessions))
	}
	se0 := sessions[0] // querySessions ORDER BY s.id：executor-B 先插
	wantKeys(t, se0, "name", "column", "role", "last_seen_at", "alive", "lost_for_sec")
	if se0["name"] != "executor-B" || se0["column"] != "05" || se0["role"] != "executor" {
		t.Errorf("sessions[0] = %v, want executor-B/05/executor", se0)
	}
	if se0["alive"] != true {
		t.Errorf("sessions[0].alive = %v, want true（last_seen 距查询时钟 10s）", se0["alive"])
	}
	prog, ok := se0["progress"].(map[string]any)
	if !ok {
		t.Fatalf("executor-B 缺 progress 段（AC23.3）: %v", se0)
	}
	wantKeys(t, prog, "commit_hash", "batch", "task", "created_at", "stale")
	if prog["commit_hash"] != "abc1234" || prog["batch"] != "B3" || prog["task"] != "B3-5" ||
		prog["created_at"] != statusT0 {
		t.Errorf("progress 值面 = %v, want {abc1234, B3, B3-5, %s}", prog, statusT0)
	}
	if prog["stale"] != "dead" {
		t.Errorf("progress.stale = %v, want dead（%s→%s 差 7400s > 2×3600 红档）",
			prog["stale"], statusT0, statusRed)
	}
	// sentinels 内嵌同源（#27 口径字段面 id/role/alive）。
	sen := decodeRows(t, se0["sentinels"], "executor-B.sentinels")
	if len(sen) != 1 || sen[0]["role"] != "executor" || sen[0]["alive"] != true {
		t.Errorf("executor-B sentinels = %v, want [{id, executor, alive:true}]", sen)
	}
	// controller-A 无上报：progress 键省略（omitempty——无上报记录不出键）。
	se1 := sessions[1]
	if se1["name"] != "controller-A" {
		t.Fatalf("sessions[1] = %v, want controller-A", se1)
	}
	if _, has := se1["progress"]; has {
		t.Errorf("controller-A 不应含 progress 键（无上报 omitempty）: %v", se1)
	}

	// block 未回执清单：恰 block 一条，target="proj-a/05/controller"。
	blocks := decodeRows(t, data["block_unreceipted"], "block_unreceipted")
	if len(blocks) != 1 {
		t.Fatalf("block_unreceipted = %d 行, want 1", len(blocks))
	}
	b := blocks[0]
	wantKeys(t, b, "seq", "level", "target", "sender", "created_at")
	if b["level"] != "block" || b["target"] != "proj-a/05/controller" {
		t.Errorf("block 条目 = %v, want level=block target=proj-a/05/controller", b)
	}

	// resources_summary：in_use 1 条 port。
	res, ok := data["resources_summary"].(map[string]any)
	if !ok {
		t.Fatalf("resources_summary 非对象: %v", data["resources_summary"])
	}
	if res["in_use_count"] != float64(1) {
		t.Errorf("in_use_count = %v, want 1", res["in_use_count"])
	}
	byType, ok := res["by_type"].(map[string]any)
	if !ok || byType["port"] != float64(1) {
		t.Errorf("by_type = %v, want {port:1}", res["by_type"])
	}

	// config 回显段（B5-6 增补，看板配置展示区数据源）：三键恒在，值=注入 opts 的
	// 生效阈值（newTestEnv 用 config.Default()——心跳 900 / 哨兵 15；stale_after
	// "60m" 经 parseStaleAfterSec → 3600）。JSON 加键对既有消费者向后兼容。
	cfgSeg, ok := data["config"].(map[string]any)
	if !ok {
		t.Fatalf("config 段缺失或非对象: %v", data["config"])
	}
	wantKeys(t, cfgSeg, "heartbeat_timeout_sec", "sentinel_timeout_sec", "progress_stale_after_sec")
	if cfgSeg["heartbeat_timeout_sec"] != float64(900) ||
		cfgSeg["sentinel_timeout_sec"] != float64(15) ||
		cfgSeg["progress_stale_after_sec"] != float64(3600) {
		t.Errorf("config = %v, want {heartbeat:900, sentinel:15, stale:3600}（config.Default 注入值）", cfgSeg)
	}
}

// TestOverviewIncludeArchived all=1 扩面（#17 all 参数，总控 #20 裁定）：缺省面
// 不含 archived 项目（冻结口径不变）；all=1 含 archived 项目（status 原样透出、
// 其下栏目/会话照常聚合）；非 "1" 值忽略（严格 == "1" 口径——宽松容错不引入误
// 语义）；mode=column 时 all 忽略（单栏目=精确定位单栏目，all 无扩面语义——
// handler 裁量声明）。
func TestOverviewIncludeArchived(t *testing.T) {
	h, st := newResTestServer(t)
	seedStatusFixtures(t, st)
	// archived 项目夹具：proj-old + active 栏目 01 + 会话（扩面在列三面断言）。
	pidOld, cidOld := seedProjectColumn(t, st, "proj-old", store.ProjectStatusArchived, "01", store.ColumnStatusActive)
	seedSessionRow(t, st, pidOld, cidOld, "zombie-Z", "executor",
		"2026-10-02T15:03:00Z", "2026-10-02T12:00:00Z")
	injectFixedClock(t, statusRed)
	injectFixedHHMM(t, "10:00")

	projectsOf := func(query string) []map[string]any {
		t.Helper()
		rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/status"+query, "",
			"proj-a", "05", "executor-B", "executor")
		return decodeRows(t, wantData(t, rr, http.StatusOK)["projects"], "data.projects")
	}

	// 缺省（无 all）：仅 active 项目（proj-a/proj-b）——冻结口径不变。
	if got := projectsOf(""); len(got) != 2 {
		t.Fatalf("缺省 projects = %v，want 2 个 active（archived 不在列）", got)
	}

	// all=1：含 archived 项目（status 原样、栏目/会话照常聚合）。
	got := projectsOf("?all=1")
	if len(got) != 3 {
		t.Fatalf("all=1 projects = %v，want 3（proj-old 在列）", got)
	}
	po := got[2] // ORDER BY code：proj-a < proj-b < proj-old（'b'<'o'）
	if po["code"] != "proj-old" || po["status"] != "archived" {
		t.Fatalf("projects[2] = %v，want proj-old/archived", po)
	}
	if cols := decodeRows(t, po["columns"], "proj-old.columns"); len(cols) != 1 || cols[0]["code"] != "01" {
		t.Errorf("proj-old columns = %v，want 仅 01（active 栏目照常聚合）", cols)
	}
	if sess := decodeRows(t, po["sessions"], "proj-old.sessions"); len(sess) != 1 || sess[0]["name"] != "zombie-Z" {
		t.Errorf("proj-old sessions = %v，want zombie-Z（archived 项目会话随扩面在列）", sess)
	}

	// 非 "1" 值忽略：all=true / all=0 均回落缺省面（严格 == "1"）。
	for _, bad := range []string{"?all=true", "?all=0"} {
		if got := projectsOf(bad); len(got) != 2 {
			t.Errorf("%s projects = %v，want 2（非 \"1\" 值忽略）", bad, got)
		}
	}

	// mode=column 时 all 忽略：单栏目输出不含 proj-old（all 对 column 无扩面语义）。
	rr := doReqWithHeaders(t, h, http.MethodGet,
		"/api/v1/status?mode=column&project=proj-a&column=05&all=1", nil,
		sentinelHeads("proj-a", "05", "executor-B", "executor"))
	data := wantData(t, rr, http.StatusOK)
	cols := decodeRows(t, data["projects"], "data.projects")
	if len(cols) != 1 || cols[0]["code"] != "proj-a" {
		t.Errorf("mode=column&all=1 projects = %v，want 仅 proj-a（all 在 column 模式忽略）", cols)
	}
}

// TestColumnMode mode=column 单栏目模式：同结构限定单栏目输出；缺 column/project
// 参数 → 400 param_invalid；栏目/项目未登记 → 404 column_not_found；非法 mode → 400。
func TestColumnMode(t *testing.T) {
	h, st := newResTestServer(t)
	seedStatusFixtures(t, st)
	injectFixedClock(t, statusRed)
	injectFixedHHMM(t, "10:00")
	heads := sentinelHeads("proj-a", "05", "executor-B", "executor")

	// mode=column 限定单栏目：同结构仅 proj-a/05（windows 段同源在列）。
	rr := doReqWithHeaders(t, h, http.MethodGet,
		"/api/v1/status?mode=column&project=proj-a&column=05", nil, heads)
	data := wantData(t, rr, http.StatusOK)
	projects := decodeRows(t, data["projects"], "data.projects")
	if len(projects) != 1 || projects[0]["code"] != "proj-a" {
		t.Fatalf("column 模式 projects = %v, want 仅 proj-a", projects)
	}
	cols := decodeRows(t, projects[0]["columns"], "columns")
	if len(cols) != 1 || cols[0]["code"] != "05" {
		t.Fatalf("column 模式 columns = %v, want 仅 05", cols)
	}
	if _, ok := cols[0]["windows"].(map[string]any); !ok {
		t.Errorf("column 模式 windows 段缺失（同结构输出）: %v", cols[0])
	}
	sessions := decodeRows(t, projects[0]["sessions"], "sessions")
	if len(sessions) != 1 || sessions[0]["name"] != "executor-B" {
		t.Errorf("column 模式 sessions = %v, want 仅 executor-B（06 的会话不混入）", sessions)
	}

	// 缺 column 参数 → 400 param_invalid（b3-plan B3-5 任务书口径）。
	rr = doReqWithHeaders(t, h, http.MethodGet, "/api/v1/status?mode=column&project=proj-a", nil, heads)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	// 缺 project 参数 → 400 param_invalid（单栏目定位双参数缺一不可）。
	rr = doReqWithHeaders(t, h, http.MethodGet, "/api/v1/status?mode=column&column=05", nil, heads)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
	// 未登记栏目 → 404 column_not_found（§2.2 #17 特有错误）。
	rr = doReqWithHeaders(t, h, http.MethodGet,
		"/api/v1/status?mode=column&project=proj-a&column=99", nil, heads)
	wantErrBody(t, rr, http.StatusNotFound, codeColumnNotFound)
	// 未登记项目 → 同 404 column_not_found（ResolveColumn 项目缺失归一口径，#18 先例）。
	rr = doReqWithHeaders(t, h, http.MethodGet,
		"/api/v1/status?mode=column&project=nope&column=05", nil, heads)
	wantErrBody(t, rr, http.StatusNotFound, codeColumnNotFound)
	// 非法 mode → 400 param_invalid（防拼错静默回 overview 的「看似成功实未生效」）。
	rr = doReqWithHeaders(t, h, http.MethodGet, "/api/v1/status?mode=bogus", nil, heads)
	wantErrBody(t, rr, http.StatusBadRequest, types.CodeParamInvalid)
}

// TestLatency AC11.4：多项目多栏目夹具（4×5=20 栏目量级）下聚合全程 <5s——
// 由 §3.7 往返基线不随栏目数增长保证（store 侧机械闸 TestRoundtripBudget 对拍）。
func TestLatency(t *testing.T) {
	h, st := newResTestServer(t)
	const ts = "2026-10-02T12:00:00Z"
	for i := range 4 {
		var pid int64
		if err := st.DB.QueryRow(
			`INSERT INTO projects (code, name, status, created_at, updated_at)
			 VALUES (?, ?, 'active', ?, ?) RETURNING id`,
			fmt.Sprintf("proj-%d", i), "项目"+fmt.Sprint(i), ts, ts).Scan(&pid); err != nil {
			t.Fatalf("插入项目夹具 proj-%d 失败: %v", i, err)
		}
		for j := range 5 {
			var cid int64
			if err := st.DB.QueryRow(
				`INSERT INTO columns (project_id, code, name, status, created_at, updated_at)
				 VALUES (?, ?, ?, 'active', ?, ?) RETURNING id`,
				pid, fmt.Sprintf("c%d", j), "栏目"+fmt.Sprint(j), ts, ts).Scan(&cid); err != nil {
				t.Fatalf("插入栏目夹具 proj-%d/c%d 失败: %v", i, j, err)
			}
			if _, err := st.DB.Exec(
				`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at)
				 VALUES (?, ?, ?, 'executor', ?, ?)`,
				pid, cid, fmt.Sprintf("sess-%d-%d", i, j), ts, ts); err != nil {
				t.Fatalf("插入会话夹具 proj-%d/c%d 失败: %v", i, j, err)
			}
			if _, err := st.DB.Exec(
				`INSERT INTO ack_positions (project_id, column_id, consumer, position, updated_at)
				 VALUES (?, ?, 'controller', 0, ?)`, pid, cid, ts); err != nil {
				t.Fatalf("插入位点夹具 proj-%d/c%d 失败: %v", i, j, err)
			}
		}
	}

	injectFixedClock(t, statusRed)
	injectFixedHHMM(t, "10:00")

	start := time.Now()
	rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/status", "",
		"proj-0", "c0", "sess-0-0", "executor")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body: %s）", rr.Code, rr.Body.String())
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("status 聚合耗时 %v ≥ 5s（AC11.4）", elapsed)
	}
	// 聚合完整性顺带核验：4 项目全量在列。
	data := wantData(t, rr, http.StatusOK)
	if projects := decodeRows(t, data["projects"], "projects"); len(projects) != 4 {
		t.Errorf("projects = %d 行, want 4（多项目全量聚合）", len(projects))
	}
}

// TestProjectVisible AC1.1 端到端：#1 登记项目后 overview 立即可见（登记→观测闭环，
// b3-plan B3-5 任务书第 4 用例）。身份域与被观测实体解耦：#1 走系统动作豁免路径
// （无身份头——登记自举），观测请求身份用预置夹具域（proj-a/05）。
func TestProjectVisible(t *testing.T) {
	h, st := newResTestServer(t)
	seedProjectColumn(t, st, "proj-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)
	injectFixedClock(t, statusT0)
	injectFixedHHMM(t, "10:00")

	// #1 登记新项目（系统动作豁免：不带身份头）。
	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/projects",
		`{"code":"new-proj","name":"新项目"}`)
	wantData(t, rr, http.StatusCreated)

	// overview 立即可见：new-proj 与夹具域 proj-a 同列（AC1.1 端到端）。
	rr = doAuthedReq(t, h, http.MethodGet, "/api/v1/status", "",
		"proj-a", "05", "watch-a", "executor")
	data := wantData(t, rr, http.StatusOK)
	projects := decodeRows(t, data["projects"], "projects")
	if len(projects) != 2 {
		t.Fatalf("登记后 overview projects = %d 行, want 2（proj-a+new-proj）", len(projects))
	}
	if projects[0]["code"] != "new-proj" || projects[0]["status"] != "active" ||
		projects[1]["code"] != "proj-a" {
		t.Errorf("projects = (%v, %v), want [new-proj active, proj-a]（ORDER BY code）",
			projects[0], projects[1])
	}
}

// TestOverviewConfigEcho config 回显跟随注入 opts（B5-6）：自定义 cfg 三阈值
// （1200/30/"90s"→90）注入后 #17 config 段须逐键回显——锚定回显源=handler 注入
// opts 的生效阈值，非硬编码默认值（TestOverview 的 900/15/3600 为 Default 形态
// 恰好同值，防呆补此变体）。
func TestOverviewConfigEcho(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开临时 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.Session.HeartbeatTimeoutSec = 1200
	cfg.Watch.SentinelTimeoutSec = 30
	cfg.Progress.StaleAfter = "90s"
	h := NewServer(st, cfg, Version)
	seedProjectColumn(t, st, "proj-a", store.ProjectStatusActive, "05", store.ColumnStatusActive)

	injectFixedClock(t, statusT0)
	injectFixedHHMM(t, "10:00")
	rr := doAuthedReq(t, h, http.MethodGet, "/api/v1/status", "",
		"proj-a", "05", "executor-B", "executor")
	cfgSeg, ok := wantData(t, rr, http.StatusOK)["config"].(map[string]any)
	if !ok {
		t.Fatalf("config 段缺失或非对象: %v", cfgSeg)
	}
	if cfgSeg["heartbeat_timeout_sec"] != float64(1200) ||
		cfgSeg["sentinel_timeout_sec"] != float64(30) ||
		cfgSeg["progress_stale_after_sec"] != float64(90) {
		t.Errorf("config = %v, want {heartbeat:1200, sentinel:30, stale:90}（自定义 cfg 注入值）", cfgSeg)
	}
}

// TestParseStaleAfterSec config progress.stale_after 时长串→秒解析（信箱#17③：
// B8 呈裁②——解析职责落 B3-5，store 收 int 秒）；非法/非正值回退默认 3600
// （§9.3 服务配置坏值不放大到端点行为口径）。
func TestParseStaleAfterSec(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", store.DefaultProgressStaleAfterSec}, // 配置缺省 → 默认
		{"60m", 3600},                            // 默认形态（config.Default）
		{"90s", 90},                              // 秒形态
		{"1h", 3600},                             // 小时形态
		{"1h30m", 5400},                          // 复合时长
		{"bogus", store.DefaultProgressStaleAfterSec}, // 非法串回退
		{"60", store.DefaultProgressStaleAfterSec},    // 缺单位（ParseDuration 拒绝）回退
		{"0s", store.DefaultProgressStaleAfterSec},    // 零值（非正）回退
		{"-5m", store.DefaultProgressStaleAfterSec},   // 负值回退
		{"500ms", store.DefaultProgressStaleAfterSec}, // 亚秒截断为 0 → 回退
	}
	for _, c := range cases {
		if got := parseStaleAfterSec(c.raw); got != c.want {
			t.Errorf("parseStaleAfterSec(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}
