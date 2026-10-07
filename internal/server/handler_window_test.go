// handler_window_test.go —— B4-5 时间窗域两端点契约测试（#21 覆盖式 PUT/#22 权威查询口）。
// 形态与夹具循 handler_resources_test.go（httptest+临时真库+身份头字面量+注入时钟）；
// as_of 经 store.NowInTz 包级注入点固定（仅串行用例，与 types.NowUTC 注入模式同构）。
package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aiteam/internal/store"
)

// injectFixedHHMM 将 store.NowInTz 包级注入点替换为固定 "HH:MM"（save/restore 挂
// Cleanup）。仅限串行用例：包级变量替换与并发读有竞态面（同 injectFixedClock 纪律）。
func injectFixedHHMM(t *testing.T, hhmm string) {
	t.Helper()
	orig := store.NowInTz
	store.NowInTz = func(string) (string, error) { return hhmm, nil }
	t.Cleanup(func() { store.NowInTz = orig })
}

// seedWin 经 store 直设某 (project,column) 域窗（#22 用例数据准备，绕开 #21 HTTP
// 链路；createdBy 夹具会话身份，审计快照随写落行）。
func seedWin(t *testing.T, st *store.Store, projectID, columnID int64, windows []store.WindowInput) {
	t.Helper()
	if err := st.SetWindows(projectID, columnID, windows, 0, apiResNow); err != nil {
		t.Fatalf("准备窗配置失败: %v", err)
	}
}

// winRowAPI server 侧库内窗行断言视图（覆盖式生效断言用）。
type winRowAPI struct {
	ColumnID int64
	Stage    string
	Start    string
	End      string
	Enabled  int
}

// queryWinRowsAt 查指定 (project,column) 域全部窗行（ORDER BY stage=断言稳定序）。
func queryWinRowsAt(t *testing.T, st *store.Store, projectID, columnID int64) []winRowAPI {
	t.Helper()
	rows, err := st.DB.Query(
		`SELECT column_id, stage, start_time, end_time, enabled FROM stage_windows
		 WHERE project_id = ? AND column_id = ? ORDER BY stage`, projectID, columnID)
	if err != nil {
		t.Fatalf("查询 stage_windows 失败: %v", err)
	}
	defer rows.Close()
	var out []winRowAPI
	for rows.Next() {
		var r winRowAPI
		if err := rows.Scan(&r.ColumnID, &r.Stage, &r.Start, &r.End, &r.Enabled); err != nil {
			t.Fatalf("扫描 stage_windows 失败: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 stage_windows 失败: %v", err)
	}
	return out
}

// winEntries 解析 #22 响应 data.entries 为条目映射切片（空数组与非数组在此区分）。
func winEntries(t *testing.T, rr *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	m := decodeMap(t, rr)
	data, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 data 对象: %s", rr.Body.String())
	}
	items, ok := data["entries"].([]any) // null 反序列化后断言失败，锚定「空数组非 null」口径
	if !ok {
		t.Fatalf("data.entries 非数组（null 亦不合格）: %s", rr.Body.String())
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		mItem, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("data.entries 条目非对象: %v", it)
		}
		out = append(out, mItem)
	}
	return out
}

// findWinEntry 按 (project,column,stage) 定位条目（不存在即 Fatal）。
func findWinEntry(t *testing.T, entries []map[string]any, project, column, stage string) map[string]any {
	t.Helper()
	for _, e := range entries {
		if e["project"] == project && e["column"] == column && e["stage"] == stage {
			return e
		}
	}
	t.Fatalf("entries 缺 (%s/%s/%s) 条目: %v", project, column, stage, entries)
	return nil
}

// TestSetWindows §2.2 #21 PUT /api/v1/projects/{code}/columns/{col}/windows：
// 覆盖式生效（缺席=删除）/stage 与 HH:MM 校验（B4-T2 ^S\d+$；信箱#8 两位等宽）/
// 跨午夜合法（AC3.3）/scope=project 落 column 维度 0/域 404/审计快照/错误序锚定。
func TestSetWindows(t *testing.T) {
	putTarget := func(code, col string) string {
		return fmt.Sprintf("/api/v1/projects/%s/columns/%s/windows", code, col)
	}

	t.Run("PUT覆盖式生效_缺席行消失_响应回读", func(t *testing.T) {
		injectFixedClock(t, apiResNow)
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		seedWin(t, st, fx.projA, fx.colA05, []store.WindowInput{ // 先直插 3 窗
			{Stage: "S1", From: "08:00", To: "09:00", Enabled: 1},
			{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1},
			{Stage: "S5", From: "13:00", To: "14:00", Enabled: 1},
		})

		// PUT 2 窗：S4 留任改值 + S10（两位数阶段，B4-T2 口径收）+ enabled=false。
		body := []byte(`{"items":[{"stage":"S4","start":"20:00","end":"07:00","enabled":true},{"stage":"S10","start":"09:00","end":"18:00","enabled":false}]}`)
		rr := doReqHeaders(t, h, http.MethodPut, putTarget("proj-a", "05"), body, resIdentity())

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		m := decodeMap(t, rr)
		data, ok := m["data"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺 data 对象: %s", rr.Body.String())
		}
		items, ok := data["items"].([]any)
		if !ok || len(items) != 2 {
			t.Fatalf("data.items = %v, want 2 条回读", data["items"])
		}
		i0, _ := items[0].(map[string]any)
		if i0["stage"] != "S4" || i0["start"] != "20:00" || i0["end"] != "07:00" || i0["enabled"] != true {
			t.Errorf("items[0] 回读不符: %v", i0)
		}
		i1, _ := items[1].(map[string]any)
		if i1["stage"] != "S10" || i1["enabled"] != false {
			t.Errorf("items[1] 回读不符: %v", i1)
		}

		// 落库断言：S1/S5 缺席即删；S4 改值生效；S10 enabled=0 保留。
		rows := queryWinRowsAt(t, st, fx.projA, fx.colA05)
		if len(rows) != 2 || rows[0].Stage != "S10" || rows[1].Stage != "S4" {
			t.Fatalf("覆盖后行集 = %+v, want [S10 S4]（S1/S5 缺席即删）", rows)
		}
		if rows[1].Start != "20:00" || rows[1].End != "07:00" || rows[1].Enabled != 1 {
			t.Errorf("S4 落库值 = %+v, want 20:00-07:00 enabled=1", rows[1])
		}
		if rows[0].Enabled != 0 {
			t.Errorf("S10 enabled = %d, want 0", rows[0].Enabled)
		}
	})

	t.Run("空items清空该域_响应空数组", func(t *testing.T) {
		// 契约面：空 items=清空该域全部窗（store.TestSetWindowsEmptyClears 的 HTTP 面）。
		injectFixedClock(t, apiResNow)
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		seedWin(t, st, fx.projA, fx.colA05, []store.WindowInput{
			{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1},
		})

		rr := doReqHeaders(t, h, http.MethodPut, putTarget("proj-a", "05"), []byte(`{"items":[]}`), resIdentity())

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		if rows := queryWinRowsAt(t, st, fx.projA, fx.colA05); len(rows) != 0 {
			t.Errorf("空 items 后栏目级行数 = %d, want 0（域已清空）", len(rows))
		}
		m := decodeMap(t, rr)
		items, ok := m["data"].(map[string]any)["items"].([]any)
		if !ok || len(items) != 0 {
			t.Errorf("空 items 响应 data.items 应为空数组: %s", rr.Body.String())
		}
	})

	t.Run("start大于end跨午夜接受_AC3.3", func(t *testing.T) {
		injectFixedClock(t, apiResNow)
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)

		body := []byte(`{"items":[{"stage":"S4","start":"23:00","end":"09:00","enabled":true}]}`)
		rr := doReqHeaders(t, h, http.MethodPut, putTarget("proj-a", "05"), body, resIdentity())

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（跨午夜合法，AC3.3；body: %s）", rr.Code, rr.Body.String())
		}
		rows := queryWinRowsAt(t, st, fx.projA, fx.colA05)
		if len(rows) != 1 || rows[0].Start != "23:00" || rows[0].End != "09:00" {
			t.Errorf("跨午夜窗落库 = %+v, want 23:00-09:00", rows)
		}
	})

	t.Run("scope=project_落column维度0_col不参与定位", func(t *testing.T) {
		// 口径锚定：scope=project 写项目级默认窗（column_id=0），路径 {col} 不参与
		// 定位（即便填不存在值也成功）——项目级域与栏目级域互不覆盖（TestProjectScope）。
		injectFixedClock(t, apiResNow)
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)

		body := []byte(`{"items":[{"stage":"S4","start":"22:00","end":"06:00","enabled":true}]}`)
		rr := doReqHeaders(t, h, http.MethodPut,
			putTarget("proj-a", "no-such")+"?scope=project", body, resIdentity())

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		rows := queryWinRowsAt(t, st, fx.projA, 0)
		if len(rows) != 1 || rows[0].Stage != "S4" || rows[0].ColumnID != 0 {
			t.Errorf("项目级窗落库 = %+v, want column 维度 0 的 S4", rows)
		}
		if rows := queryWinRowsAt(t, st, fx.projA, fx.colA05); len(rows) != 0 {
			t.Errorf("scope=project 不应写栏目级域，栏目级行 = %+v", rows)
		}
	})

	t.Run("stage非法_400", func(t *testing.T) {
		for _, stage := range []string{"X4", "S", ""} {
			h, st := newResTestServer(t)
			seedResAPIFixtures(t, st)

			body := []byte(fmt.Sprintf(`{"items":[{"stage":%q,"start":"09:00","end":"18:00","enabled":true}]}`, stage))
			rr := doReqHeaders(t, h, http.MethodPut, putTarget("proj-a", "05"), body, resIdentity())

			wantErrBody(t, rr, http.StatusBadRequest, "stage_invalid")
		}
	})

	t.Run("time格式非法_400", func(t *testing.T) {
		// 信箱#8 裁定：HH:MM 两位等宽（小时 00-23、分钟 00-59）——反例逐个必拒，
		// start/end 两字段同口径（四反例×双字段）。
		badVals := []string{"9:00", "25:00", "23:5", "0900"}
		for _, field := range []string{"start", "end"} {
			for _, bad := range badVals {
				start, end := "09:00", "18:00"
				if field == "start" {
					start = bad
				} else {
					end = bad
				}
				h, st := newResTestServer(t)
				seedResAPIFixtures(t, st)

				body := []byte(fmt.Sprintf(`{"items":[{"stage":"S4","start":%q,"end":%q,"enabled":true}]}`, start, end))
				rr := doReqHeaders(t, h, http.MethodPut, putTarget("proj-a", "05"), body, resIdentity())

				wantErrBody(t, rr, http.StatusBadRequest, "time_format_invalid")
			}
		}
	})

	t.Run("校验序锚定_stage先于time_双条件同坏", func(t *testing.T) {
		// 口径锚定：单 item 内 stage 校验先于 time——stage 与 time 同时非法时报
		// stage_invalid（校验序见 handleSetWindows doc 注）。
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		body := []byte(`{"items":[{"stage":"X4","start":"9:00","end":"18:00","enabled":true}]}`)
		rr := doReqHeaders(t, h, http.MethodPut, putTarget("proj-a", "05"), body, resIdentity())

		wantErrBody(t, rr, http.StatusBadRequest, "stage_invalid")
	})

	t.Run("校验序锚定_数组序_首item坏time先报", func(t *testing.T) {
		// 口径锚定：跨 item 按数组序——items[0] 坏 time + items[1] 坏 stage，
		// 报 time_format_invalid（首个违规即返，不收集全量）。
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		body := []byte(`{"items":[{"stage":"S4","start":"9:00","end":"18:00","enabled":true},{"stage":"X4","start":"09:00","end":"18:00","enabled":true}]}`)
		rr := doReqHeaders(t, h, http.MethodPut, putTarget("proj-a", "05"), body, resIdentity())

		wantErrBody(t, rr, http.StatusBadRequest, "time_format_invalid")
	})

	t.Run("同提交重复stage_400", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		body := []byte(`{"items":[{"stage":"S4","start":"09:00","end":"10:00","enabled":true},{"stage":"S4","start":"11:00","end":"12:00","enabled":true}]}`)
		rr := doReqHeaders(t, h, http.MethodPut, putTarget("proj-a", "05"), body, resIdentity())

		wantErrBody(t, rr, http.StatusBadRequest, "param_invalid")
	})

	t.Run("栏目不存在_404", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		body := []byte(`{"items":[{"stage":"S4","start":"09:00","end":"18:00","enabled":true}]}`)
		rr := doReqHeaders(t, h, http.MethodPut, putTarget("proj-a", "99"), body, resIdentity())

		wantErrBody(t, rr, http.StatusNotFound, "column_not_found")
	})

	t.Run("col非数字_404口径对齐19", func(t *testing.T) {
		// 口径锚定：{col} 是 TEXT code，非数字值照 code 精确查询——查无即
		// column_not_found 404（对齐 #19「解析不了=不存在」口径）。
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		body := []byte(`{"items":[{"stage":"S4","start":"09:00","end":"18:00","enabled":true}]}`)
		rr := doReqHeaders(t, h, http.MethodPut, putTarget("proj-a", "abc"), body, resIdentity())

		wantErrBody(t, rr, http.StatusNotFound, "column_not_found")
	})

	t.Run("项目不存在_404", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		body := []byte(`{"items":[{"stage":"S4","start":"09:00","end":"18:00","enabled":true}]}`)
		rr := doReqHeaders(t, h, http.MethodPut, putTarget("no-such", "05"), body, resIdentity())

		wantErrBody(t, rr, http.StatusNotFound, "project_not_found")
	})

	t.Run("审计window.set_含快照与会话身份", func(t *testing.T) {
		injectFixedClock(t, apiResNow)
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)

		body := []byte(`{"items":[{"stage":"S4","start":"23:00","end":"09:00","enabled":true}]}`)
		rr := doReqHeaders(t, h, http.MethodPut, putTarget("proj-a", "05"), body, resIdentity())

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		auditSess, auditAt, detail := queryAudit(t, st, "window.set")
		if auditSess != fx.sessA {
			t.Errorf("audit.session_id = %d, want 会话 id %d", auditSess, fx.sessA)
		}
		if auditAt != apiResNow {
			t.Errorf("audit.created_at = %q, want 注入时钟 %q", auditAt, apiResNow)
		}
		for _, want := range []string{"S4", "23:00", "09:00"} {
			if !strings.Contains(detail, want) {
				t.Errorf("audit.detail 缺提交快照 %q: %s", want, detail)
			}
		}
	})

	t.Run("会话头缺失_400", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		headers := resIdentity()
		delete(headers, "X-Aiteam-Session")
		body := []byte(`{"items":[{"stage":"S4","start":"09:00","end":"18:00","enabled":true}]}`)
		rr := doReqHeaders(t, h, http.MethodPut, putTarget("proj-a", "05"), body, headers)

		wantErrBody(t, rr, http.StatusBadRequest, "missing_header")
	})

	t.Run("坏JSON体_400", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		rr := doReqHeaders(t, h, http.MethodPut, putTarget("proj-a", "05"), []byte(`{"items":`), resIdentity())

		wantErrBody(t, rr, http.StatusBadRequest, "bad_json")
	})
}

// TestWindowsNow §2.2 #22 GET /api/v1/windows/now（权威查询口）：entries 结构逐字段 /
// 未配置 allowed+null（AC3.4）/ 窗外 waiting 非报错（AC3.2）/ 窗内 allowed（AC3.1）/
// as_of 注入四时刻翻转（§6.2 对拍表跨午夜正反）/ project 过滤 / 栏目级覆盖项目级 /
// enabled=0 行视同未配置。
func TestWindowsNow(t *testing.T) {
	t.Run("entries结构逐字段_未配置allowed_windowNull_AC3.4", func(t *testing.T) {
		injectFixedHHMM(t, "23:30")
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		seedWin(t, st, fx.projA, fx.colA05, []store.WindowInput{
			{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1},
		})

		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/windows/now", nil, resIdentity())
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		m := decodeMap(t, rr)
		data := m["data"].(map[string]any)
		if got := data["as_of"]; got != "23:30" {
			t.Errorf("data.as_of = %v, want 注入值 23:30", got)
		}
		entries := winEntries(t, rr)

		// 配置窗条目：窗内 allowed + window 快照逐字段。
		e := findWinEntry(t, entries, "proj-a", "05", "S4")
		if e["status"] != "allowed" {
			t.Errorf("23:30∈[23:00,09:00] 应 allowed，实际 %v", e)
		}
		win, ok := e["window"].(map[string]any)
		if !ok || win["start"] != "23:00" || win["end"] != "09:00" {
			t.Errorf("window 快照不符: %v", e["window"])
		}

		// 未配置阶段：allowed + window=null（AC3.4；响应恒含 window 字段，值为 null）。
		e = findWinEntry(t, entries, "proj-a", "05", "S0")
		if e["status"] != "allowed" || e["window"] != nil {
			t.Errorf("未配置阶段应 allowed+window=null，实际 %v", e)
		}
		if !strings.Contains(rr.Body.String(), `"window":null`) {
			t.Errorf("响应应显式含 window:null 字段（非省略）: %s", rr.Body.String())
		}

		// 另一项目无任何配置：S0 亦 allowed+null（权威口全量出枚举）。
		e = findWinEntry(t, entries, "proj-b", "05", "S0")
		if e["status"] != "allowed" || e["window"] != nil {
			t.Errorf("未配置项目条目应 allowed+null，实际 %v", e)
		}
	})

	t.Run("窗外waiting非报错_AC3.2", func(t *testing.T) {
		injectFixedHHMM(t, "10:00")
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		seedWin(t, st, fx.projA, fx.colA05, []store.WindowInput{
			{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1},
		})

		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/windows/now", nil, resIdentity())
		if rr.Code != http.StatusOK { // 窗外挂起是正常态：200 非错误响应
			t.Fatalf("status = %d, want 200（AC3.2 窗外非报错）", rr.Code)
		}
		e := findWinEntry(t, winEntries(t, rr), "proj-a", "05", "S4")
		if e["status"] != "waiting" {
			t.Errorf("10:00∉[23:00,09:00] 应 waiting，实际 %v", e)
		}
		win, ok := e["window"].(map[string]any)
		if !ok || win["start"] != "23:00" || win["end"] != "09:00" {
			t.Errorf("waiting 条目应带 window 快照: %v", e["window"])
		}
	})

	t.Run("as_of注入四时刻翻转同窗状态", func(t *testing.T) {
		// §6.2 对拍表跨午夜正反四组（经 HTTP 端到端，as_of 全注入非墙钟）。
		for _, tt := range []struct {
			name     string
			from, to string
			asOf     string
			want     string
		}{
			{name: "跨午夜_窗内白天_13:00-01:00@15:00", from: "13:00", to: "01:00", asOf: "15:00", want: "allowed"},
			{name: "跨午夜_窗外_15:00-05:00@10:00", from: "15:00", to: "05:00", asOf: "10:00", want: "waiting"},
			{name: "跨午夜_端点in_23:00-09:00@23:00", from: "23:00", to: "09:00", asOf: "23:00", want: "allowed"},
			{name: "同日_窗外_09:00-18:00@07:00", from: "09:00", to: "18:00", asOf: "07:00", want: "waiting"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				injectFixedHHMM(t, tt.asOf)
				h, st := newResTestServer(t)
				fx := seedResAPIFixtures(t, st)
				seedWin(t, st, fx.projA, fx.colA05, []store.WindowInput{
					{Stage: "S4", From: tt.from, To: tt.to, Enabled: 1},
				})

				rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/windows/now", nil, resIdentity())
				if rr.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
				}
				m := decodeMap(t, rr)
				if got := m["data"].(map[string]any)["as_of"]; got != tt.asOf {
					t.Errorf("as_of = %v, want 注入值 %q", got, tt.asOf)
				}
				e := findWinEntry(t, winEntries(t, rr), "proj-a", "05", "S4")
				if e["status"] != tt.want {
					t.Errorf("as_of=%s 窗 %s-%s 状态 = %v, want %s", tt.asOf, tt.from, tt.to, e["status"], tt.want)
				}
			})
		}
	})

	t.Run("project过滤_省略全部", func(t *testing.T) {
		injectFixedHHMM(t, "12:00")
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		seedWin(t, st, fx.projA, fx.colA05, []store.WindowInput{
			{Stage: "S4", From: "09:00", To: "18:00", Enabled: 1},
		})

		// 省略 project=全部项目（proj-a 与 proj-b 条目都在）。
		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/windows/now", nil, resIdentity())
		entries := winEntries(t, rr)
		projs := map[string]bool{}
		for _, e := range entries {
			projs[e["project"].(string)] = true
		}
		if !projs["proj-a"] || !projs["proj-b"] {
			t.Errorf("省略 project 应含全部项目，实际 %v", projs)
		}

		// project=proj-a 过滤：仅 proj-a 条目。
		rr = doReqHeaders(t, h, http.MethodGet, "/api/v1/windows/now?project=proj-a", nil, resIdentity())
		for _, e := range winEntries(t, rr) {
			if e["project"] != "proj-a" {
				t.Errorf("project=proj-a 过滤串入其他项目: %v", e)
			}
		}
	})

	t.Run("栏目级覆盖项目级", func(t *testing.T) {
		injectFixedHHMM(t, "12:00")
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		seedWin(t, st, fx.projA, 0, []store.WindowInput{ // 项目级默认窗（column 维度 0）
			{Stage: "S4", From: "09:00", To: "18:00", Enabled: 1},
		})
		seedWin(t, st, fx.projA, fx.colA05, []store.WindowInput{ // 栏目级覆盖
			{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1},
		})

		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/windows/now?project=proj-a", nil, resIdentity())
		// as_of=12:00：栏目级窗(23:00-09:00)外 → waiting（若误用项目级窗会判 allowed）。
		e := findWinEntry(t, winEntries(t, rr), "proj-a", "05", "S4")
		if e["status"] != "waiting" {
			t.Errorf("栏目级应覆盖项目级（12:00 按栏目级窗判 waiting），实际 %v", e)
		}
		win := e["window"].(map[string]any)
		if win["start"] != "23:00" {
			t.Errorf("生效窗应为栏目级 23:00 窗，实际 %v", win)
		}
	})

	t.Run("enabled0行视同未配置_allowed_windowNull", func(t *testing.T) {
		injectFixedHHMM(t, "01:30") // 停用窗的窗内时刻：仍应 allowed（视同未配置）
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		seedWin(t, st, fx.projA, fx.colA05, []store.WindowInput{
			{Stage: "S5", From: "01:00", To: "02:00", Enabled: 0},
		})

		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/windows/now?project=proj-a", nil, resIdentity())
		e := findWinEntry(t, winEntries(t, rr), "proj-a", "05", "S5")
		if e["status"] != "allowed" || e["window"] != nil {
			t.Errorf("enabled=0 应视同未配置 allowed+null，实际 %v", e)
		}
	})

	t.Run("栏目级enabled0覆盖项目级1_不回落", func(t *testing.T) {
		// B4-5 质量审查遗留组合面：栏目级行存在但 enabled=0 时，聚合须「视同未配置
		// allowed+window=null」，不得回落项目级窗（§6.3 第 2 步覆盖语义含停用行——
		// 停用是显式状态，回落会把项目级窗意外套到该栏目上）。
		injectFixedHHMM(t, "12:00")
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		seedWin(t, st, fx.projA, 0, []store.WindowInput{ // 项目级启用窗：12:00 窗内
			{Stage: "S4", From: "09:00", To: "18:00", Enabled: 1},
		})
		seedWin(t, st, fx.projA, fx.colA05, []store.WindowInput{ // 栏目级停用窗
			{Stage: "S4", From: "01:00", To: "02:00", Enabled: 0},
		})

		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/windows/now?project=proj-a", nil, resIdentity())
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		// 双条件精确锚定：若误回落项目级 → allowed+window={09:00,18:00}（window 非 null
		// 被抓）；若误把停用行当启用 → waiting（status 被抓）。
		e := findWinEntry(t, winEntries(t, rr), "proj-a", "05", "S4")
		if e["status"] != "allowed" || e["window"] != nil {
			t.Errorf("栏目级 enabled=0 应 allowed+window=null（不回落项目级），实际 %v", e)
		}
	})
}

// winItems 解析 #21 响应 data.items 为条目映射切片（空数组与非数组在此区分，
// 镜像 winEntries——两方法响应 data 同构 `data:{items:[...]}`）。
func winItems(t *testing.T, rr *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	m := decodeMap(t, rr)
	data, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 data 对象: %s", rr.Body.String())
	}
	items, ok := data["items"].([]any) // null 反序列化后断言失败，锚定「空数组非 null」口径
	if !ok {
		t.Fatalf("data.items 非数组（null 亦不合格）: %s", rr.Body.String())
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		mItem, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("data.items 条目非对象: %v", it)
		}
		out = append(out, mItem)
	}
	return out
}

// TestGetWindows §2.2 #21 修订行（B4-6b）GET /api/v1/projects/{code}/columns/{col}/windows：
// 同 path 加 GET 读当前域窗配置原始行（管理面全量：含 enabled=false 停用行，不做
// #22 判定态过滤）/scope=project 同参（{col} 不参与定位）/两级 404 错误码同 PUT/
// 空域 items:[] 非 null/同 pattern 双方法 405 兜底 Allow 扩容。
func TestGetWindows(t *testing.T) {
	getTarget := func(code, col string) string {
		return fmt.Sprintf("/api/v1/projects/%s/columns/%s/windows", code, col)
	}

	t.Run("栏目级全量原始行_含停用", func(t *testing.T) {
		injectFixedClock(t, apiResNow)
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		seedWin(t, st, fx.projA, fx.colA05, []store.WindowInput{
			{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1}, // 跨午夜
			{Stage: "S5", From: "01:00", To: "02:00", Enabled: 0}, // 停用行
		})

		rr := doReqHeaders(t, h, http.MethodGet, getTarget("proj-a", "05"), nil, resIdentity())
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		items := winItems(t, rr)
		if len(items) != 2 {
			t.Fatalf("data.items 数 = %d, want 2（管理面全量，停用行不过滤）", len(items))
		}
		// stage 升序：S4 在前。逐字段与 PUT 响应 items 同构（stage/start/end/enabled）。
		i0 := items[0]
		if i0["stage"] != "S4" || i0["start"] != "23:00" || i0["end"] != "09:00" || i0["enabled"] != true {
			t.Errorf("items[0] = %v, want S4 23:00-09:00 enabled=true", i0)
		}
		i1 := items[1]
		if i1["stage"] != "S5" || i1["start"] != "01:00" || i1["end"] != "02:00" || i1["enabled"] != false {
			t.Errorf("items[1] = %v, want S5 停用行原样 enabled=false", i1)
		}
	})

	t.Run("项目级scope_同参_col不参与定位", func(t *testing.T) {
		// scope 定位口径与 PUT 同参对齐：?scope=project 读项目级域（column 维度 0），
		// {col} 不参与定位（填不存在值亦 200）；栏目级域隔离不可见。
		h, st := newResTestServer(t)
		fx := seedResAPIFixtures(t, st)
		seedWin(t, st, fx.projA, 0, []store.WindowInput{
			{Stage: "S4", From: "22:00", To: "06:00", Enabled: 1},
		})
		seedWin(t, st, fx.projA, fx.colA05, []store.WindowInput{ // 隔离对照
			{Stage: "S3", From: "09:00", To: "18:00", Enabled: 1},
		})

		rr := doReqHeaders(t, h, http.MethodGet, getTarget("proj-a", "no-such")+"?scope=project", nil, resIdentity())
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（{col} 不参与定位；body: %s）", rr.Code, rr.Body.String())
		}
		items := winItems(t, rr)
		if len(items) != 1 || items[0]["stage"] != "S4" || items[0]["start"] != "22:00" {
			t.Errorf("项目级 items = %v, want 仅项目级 S4=22:00-06:00（栏目级 S3 不串入）", items)
		}
	})

	t.Run("未登记栏目_404", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		rr := doReqHeaders(t, h, http.MethodGet, getTarget("proj-a", "99"), nil, resIdentity())

		wantErrBody(t, rr, http.StatusNotFound, "column_not_found")
	})

	t.Run("未登记项目_404", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		rr := doReqHeaders(t, h, http.MethodGet, getTarget("no-such", "05"), nil, resIdentity())

		wantErrBody(t, rr, http.StatusNotFound, "project_not_found")
	})

	t.Run("空域_items空数组非null", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st) // proj-a/05 无任何窗配置

		rr := doReqHeaders(t, h, http.MethodGet, getTarget("proj-a", "05"), nil, resIdentity())
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		if items := winItems(t, rr); len(items) != 0 { // winItems 内断言数组类型（null 不合格）
			t.Errorf("空域 items 数 = %d, want 0", len(items))
		}
		if !strings.Contains(rr.Body.String(), `"items":[]`) {
			t.Errorf("空域响应应显式含 items:[] 非 null: %s", rr.Body.String())
		}
	})

	t.Run("POST_405_Allow扩容", func(t *testing.T) {
		// 同 pattern 双方法注册（PUT+GET）后 405 兜底 Allow 自动扩容（server.handle
		// methodsByPath 机制——冻结面行为锚）。
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		rr := doReqHeaders(t, h, http.MethodPost, getTarget("proj-a", "05"), nil, resIdentity())

		wantErrBody(t, rr, http.StatusMethodNotAllowed, "method_not_allowed")
		allow := rr.Header().Get("Allow")
		if !strings.Contains(allow, "PUT") || !strings.Contains(allow, "GET") {
			t.Errorf("405 Allow 头 = %q, want 含 PUT 与 GET（双方法扩容）", allow)
		}
	})
}

// TestSingleSource 单一实现判据（§6.3「单一实现三处复用」）：#22 HTTP 响应 entries
// 与 store.ListWindowStatus 同 as_of 同过滤逐条同构（handler 零自有判定逻辑，纯映射）。
//
// 注：§6.3 同源的第三处消费——#17 status 内嵌每栏目 windows 压缩视图——属 B3 批次，
// 其与 ListWindowStatus 的联动一致断言待 B3 关批后补跑（总控 #9 指令，非本批范围）。
func TestSingleSource(t *testing.T) {
	injectFixedHHMM(t, "12:00")
	h, st := newResTestServer(t)
	fx := seedResAPIFixtures(t, st)
	seedWin(t, st, fx.projA, fx.colA05, []store.WindowInput{
		{Stage: "S4", From: "23:00", To: "09:00", Enabled: 1},
		{Stage: "S5", From: "01:00", To: "02:00", Enabled: 0}, // 停用行走 allowed+null 分支
	})

	rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/windows/now", nil, resIdentity())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
	}
	apiEntries := winEntries(t, rr)
	storeEntries, err := st.ListWindowStatus("12:00", "")
	if err != nil {
		t.Fatalf("ListWindowStatus 意外失败: %v", err)
	}

	if len(apiEntries) != len(storeEntries) {
		t.Fatalf("条目数不一致: HTTP %d vs store %d", len(apiEntries), len(storeEntries))
	}
	for i, se := range storeEntries { // 两侧同为 (project,column,stage) 排序输出，按下标逐条对拍
		ae := apiEntries[i]
		if ae["project"] != se.ProjectCode || ae["column"] != se.ColumnCode || ae["stage"] != se.Stage {
			t.Fatalf("#%d 域不一致: HTTP %v vs store %s/%s/%s", i, ae, se.ProjectCode, se.ColumnCode, se.Stage)
		}
		if ae["status"] != se.Status {
			t.Errorf("#%d(%s) 状态不一致: HTTP %v vs store %s", i, se.Stage, ae["status"], se.Status)
		}
		if se.Window == nil {
			if ae["window"] != nil {
				t.Errorf("#%d(%s) window 不一致: HTTP %v vs store nil", i, se.Stage, ae["window"])
			}
			continue
		}
		win, ok := ae["window"].(map[string]any)
		if !ok || win["start"] != se.Window.Start || win["end"] != se.Window.End {
			t.Errorf("#%d(%s) window 不一致: HTTP %v vs store %+v", i, se.Stage, ae["window"], se.Window)
		}
	}
}

// ===== B4-7 收口补测：B4-5 质量审查遗留——校验函数表驱动直接单测 =====
// HTTP 契约面（TestSetWindows 的 stage_invalid/time_format_invalid）已覆盖 400 映射，
// 此处下沉到函数级逐例固化（未导出函数同包直调），补齐正例边界与 HTTP 反例矩阵
// 之外的全部裁决点。

// TestValidHHMM validHHMM 纯函数表驱动（信箱#8 两位等宽口径 10 例固化）：
// 正例双边界 + 反例（小时/分钟越界、非等宽、缺冒号）逐个必拒。
func TestValidHHMM(t *testing.T) {
	for _, tt := range []struct {
		in   string // 待校验串
		want bool   // 期望：true=合法
	}{
		{"24:00", false}, // 小时越界（24>23）
		{"00:60", false}, // 分钟越界（60>59）
		{"23:59", true},  // 双上边界
		{"00:00", true},  // 双下边界
		{"9:00", false},  // 小时一位（非等宽）
		{"25:00", false}, // 小时越界
		{"23:5", false},  // 分钟一位（非等宽）
		{"0900", false},  // 缺冒号
		{"60:00", false}, // 小时两位仍越界
		{"00:99", false}, // 分钟两位仍越界
	} {
		t.Run(tt.in, func(t *testing.T) {
			if got := validHHMM(tt.in); got != tt.want {
				t.Errorf("validHHMM(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestValidStageFormat validStageFormat 纯函数表驱动（B4-T2 ^S\d+$ 口径 8 例固化）：
// S0/S99/S007 等多位数收（不硬限 S0~S7）；小写/缺数字/尾缀/负号/异首字母拒。
func TestValidStageFormat(t *testing.T) {
	for _, tt := range []struct {
		in   string // 待校验串
		want bool   // 期望：true=合法
	}{
		{"S0", true},   // 单位数下界
		{"S99", true},  // 两位数收（B4-T2：S9/S10 等收）
		{"S007", true}, // 三位数收（\d+ 不限位数）
		{"s4", false},  // 小写 s
		{"S", false},   // 缺数字
		{"S4x", false}, // 数字后带尾缀
		{"S-4", false}, // 带负号
		{"X4", false},  // 首字母非 S
	} {
		t.Run(tt.in, func(t *testing.T) {
			if got := validStageFormat(tt.in); got != tt.want {
				t.Errorf("validStageFormat(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
