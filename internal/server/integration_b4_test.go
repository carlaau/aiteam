// integration_b4_test.go —— B4-7 集成验收：AC4.x/AC3.x HTTP 端到端全链+软闸双层断言。
//
// 形态：in-process httptest+临时真库，夹具/helper 全复用契约测试文件
// （newResTestServer/seedResAPIFixtures/resIdentity/doReqHeaders/injectFixedClock/
// injectFixedHHMM/queryAudit/decodeResources/apiResValues/assertResValues/winEntries/
// findWinEntry），零复制。与契约测试分工：契约测试以 store 直插夹具聚焦单端点行为；
// 本文件写读全走 HTTP 端点互编成链（#18→#20→#19→#21→#22），并承载 B1/B2/B3 依赖面
// 的待补跑标记（信箱#9 口径——skip 不伪造通过）。
package server

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"aiteam/internal/store"
)

// TestIntegrationResourcesAC4 AC4.1~4.3 HTTP 端到端全链（同一临时库内子测试按序
// 递进成链）：登记→list 可见→同值 409 指明冲突对象→跨项目 D3 全局拒→跨 type 隔离
// →release→默认不见/历史可见→同值再登记→设窗→三 action 审计核验。
// 串行+注入时钟（审计服务端时间断言依赖）；并发面独立成 Test（时钟纪律），见下。
func TestIntegrationResourcesAC4(t *testing.T) {
	injectFixedClock(t, apiResNow)
	h, st := newResTestServer(t)
	fx := seedResAPIFixtures(t, st)

	var firstID int64 // 链上首个在用资源 id（后续子测试的冲突对象/释放目标）

	t.Run("登记port_201_AC4.1", func(t *testing.T) {
		body := []byte(`{"column":"05","type":"port","value":"8080","note":"网关端口"}`)
		rr := doReqHeaders(t, h, http.MethodPost, "/api/v1/resources", body, resIdentity())

		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201（body: %s）", rr.Code, rr.Body.String())
		}
		data, ok := decodeMap(t, rr)["data"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺 data 对象: %s", rr.Body.String())
		}
		if got := data["value"]; got != "8080" {
			t.Errorf("data.value = %v, want 8080", got)
		}
		firstID = int64(data["id"].(float64))
		if firstID <= 0 {
			t.Fatalf("data.id = %v, want 正数", data["id"])
		}
	})

	t.Run("list按project_type过滤可见_AC4.1", func(t *testing.T) {
		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/resources?project=proj-a&type=port", nil, resIdentity())
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		assertResValues(t, "proj-a port 清单", apiResValues(t, rr), "8080")

		// 条目要素齐（AC4.1：项目+栏目+类型+值+用途说明）
		items := decodeResources(t, rr)
		if len(items) != 1 {
			t.Fatalf("清单条目数 = %d, want 1", len(items))
		}
		it := items[0]
		if it["project"] != "proj-a" || it["column"] != "05" || it["type"] != "port" || it["note"] != "网关端口" {
			t.Errorf("清单条目要素不符: %v", it)
		}
		if it["status"] != "in_use" {
			t.Errorf("清单条目 status = %v, want in_use", it["status"])
		}
	})

	t.Run("同值再登记_409_message指明冲突对象_AC4.2", func(t *testing.T) {
		body := []byte(`{"column":"05","type":"port","value":"8080"}`)
		rr := doReqHeaders(t, h, http.MethodPost, "/api/v1/resources", body, resIdentity())

		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409（body: %s）", rr.Code, rr.Body.String())
		}
		errObj, ok := decodeMap(t, rr)["error"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺 error 对象: %s", rr.Body.String())
		}
		if got := errObj["code"]; got != "resource_conflict" {
			t.Errorf("error.code = %v, want resource_conflict", got)
		}
		// 冲突对象三要素：项目 code/栏目 code/资源 id（AC4.2 信息面）
		wantMsg := fmt.Sprintf("与 proj-a/05 在用的 resource#%d 冲突", firstID)
		if got := errObj["message"]; got != wantMsg {
			t.Errorf("error.message = %q, want %q", got, wantMsg)
		}
	})

	t.Run("跨项目同值仍拒_D3全局_AC4.2", func(t *testing.T) {
		// D3=全局拒：proj-b/05 登记 proj-a 在用的 8080 亦 409，且 message 指明
		// 冲突对象在 proj-a（跨项目场景照样指明——D3 的 message 面）。
		seedSessionAt(t, st, fx.projB, fx.colB05, "operator-b", "controller")
		headers := map[string]string{
			"X-Aiteam-Project": "proj-b",
			"X-Aiteam-Column":  "05",
			"X-Aiteam-Session": "operator-b",
			"X-Aiteam-Role":    "controller",
		}
		body := []byte(`{"column":"05","type":"port","value":"8080"}`)
		rr := doReqHeaders(t, h, http.MethodPost, "/api/v1/resources", body, headers)

		wantErrBody(t, rr, http.StatusConflict, "resource_conflict")
		wantMsg := fmt.Sprintf("与 proj-a/05 在用的 resource#%d 冲突", firstID)
		if got := decodeMap(t, rr)["error"].(map[string]any)["message"]; got != wantMsg {
			t.Errorf("跨项目冲突 message = %q, want %q", got, wantMsg)
		}
	})

	t.Run("跨type同数值不互斥_spec三5", func(t *testing.T) {
		// spec §三.5：rtype 在冲突判定 SQL 内隔离——port 8080 在用 ≠
		// account_range "acct:8080-8080" 冲突。注意 account_range 的裸 "8080"
		// 是缺段非法（value_invalid），段形态才合法，故用全段值对撞。
		body := []byte(`{"column":"05","type":"account_range","value":"acct:8080-8080"}`)
		rr := doReqHeaders(t, h, http.MethodPost, "/api/v1/resources", body, resIdentity())

		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201（跨 type 同数值不应互斥；body: %s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("release_默认不见_历史可见_同值再登记_AC4.3", func(t *testing.T) {
		// release → 200 released + released_at=注入时钟（服务端时间）
		rr := doReqHeaders(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/resources/%d", firstID), nil, resIdentity())
		if rr.Code != http.StatusOK {
			t.Fatalf("release status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		data, ok := decodeMap(t, rr)["data"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺 data 对象: %s", rr.Body.String())
		}
		if data["status"] != "released" || data["released_at"] != apiResNow {
			t.Errorf("release 响应 = %v, want released+released_at=%s", data, apiResNow)
		}

		// 默认清单：release 后不再计在用（AC4.3；本库 proj-a port 在用此时为空——
		// acct 条目经 type=port 过滤排除）
		rr = doReqHeaders(t, h, http.MethodGet, "/api/v1/resources?project=proj-a&type=port", nil, resIdentity())
		if got := decodeResources(t, rr); len(got) != 0 {
			t.Errorf("release 后默认清单条目 = %v, want 空", got)
		}

		// 历史 --all 语义：include_released=true 保留可查（AC4.3）
		rr = doReqHeaders(t, h, http.MethodGet, "/api/v1/resources?project=proj-a&type=port&include_released=true", nil, resIdentity())
		assertResValues(t, "含历史清单", apiResValues(t, rr), "8080")
		items := decodeResources(t, rr)
		if len(items) != 1 {
			t.Fatalf("含历史清单条目数 = %d, want 1", len(items))
		}
		if items[0]["status"] != "released" || items[0]["released_at"] != apiResNow {
			t.Errorf("历史条目 = %v, want released+released_at=%s", items[0], apiResNow)
		}

		// 同值再登记 → 201（released 行不参与冲突判定；历史行保留=两行同值并存）
		body := []byte(`{"column":"05","type":"port","value":"8080","note":"复用再登记"}`)
		rr = doReqHeaders(t, h, http.MethodPost, "/api/v1/resources", body, resIdentity())
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201（AC4.3 释放后同值可再登记；body: %s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("设窗全链_落window_set审计", func(t *testing.T) {
		// PUT #21 端到端设窗（跨午夜窗）：为审计核验落 window.set，兼验写口全链。
		body := []byte(`{"items":[{"stage":"S4","start":"23:00","end":"09:00","enabled":true}]}`)
		rr := doReqHeaders(t, h, http.MethodPut, "/api/v1/projects/proj-a/columns/05/windows", body, resIdentity())
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("审计三action落行_会话身份与服务端时间", func(t *testing.T) {
		// #28 audit 端点属 B1 尚未合并（注册面无 audit 路由）——审计核验走 store 层
		// 直查（queryAudit 同契约测试口径）；#28 端到端待 B1 合并后补跑。
		sess, at, detail := queryAudit(t, st, "resource.register")
		if sess != fx.sessA {
			t.Errorf("register audit.session_id = %d, want 会话 id %d", sess, fx.sessA)
		}
		if at != apiResNow {
			t.Errorf("register audit.created_at = %q, want 注入时钟 %q", at, apiResNow)
		}
		if !strings.Contains(detail, "8080") {
			t.Errorf("register audit.detail 缺登记 value 快照: %s", detail)
		}

		sess, at, _ = queryAudit(t, st, "resource.release")
		if sess != fx.sessA {
			t.Errorf("release audit.session_id = %d, want 会话 id %d", sess, fx.sessA)
		}
		if at != apiResNow {
			t.Errorf("release audit.created_at = %q, want 注入时钟 %q", at, apiResNow)
		}

		sess, at, detail = queryAudit(t, st, "window.set")
		if sess != fx.sessA {
			t.Errorf("window.set audit.session_id = %d, want 会话 id %d", sess, fx.sessA)
		}
		if at != apiResNow {
			t.Errorf("window.set audit.created_at = %q, want 注入时钟 %q", at, apiResNow)
		}
		for _, want := range []string{"S4", "23:00", "09:00"} {
			if !strings.Contains(detail, want) {
				t.Errorf("window.set audit.detail 缺提交快照 %q: %s", want, detail)
			}
		}
	})
}

// TestIntegrationConcurrentRegisterAC4 AC4.2 并发面（集成链一环）：20 goroutine
// 并发同值 register → 恰 1×201+19×409，库内恰 1 行在用+1 条登记审计（并发无重复
// 落行/落审计）。独立成 Test：不注入时钟（包级替换与并发读有竞态面——
// TestConcurrentRegister 同纪律）。
func TestIntegrationConcurrentRegisterAC4(t *testing.T) {
	h, st := newResTestServer(t)
	fx := seedResAPIFixtures(t, st)

	const n = 20
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := []byte(`{"column":"05","type":"port","value":"8888","note":"并发集成登记"}`)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/resources", bytes.NewReader(body))
			req.Header.Set("X-Aiteam-Project", "proj-a")
			req.Header.Set("X-Aiteam-Column", "05")
			req.Header.Set("X-Aiteam-Session", "controller-a")
			req.Header.Set("X-Aiteam-Role", "controller")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			codes <- rr.Code
		}()
	}
	wg.Wait()
	close(codes)

	created, conflict := 0, 0
	for c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("并发登记出现意外状态码 %d", c)
		}
	}
	if created != 1 || conflict != n-1 {
		t.Errorf("并发登记结果 = %d×201 + %d×409, want 恰 1×201 + %d×409", created, conflict, n-1)
	}

	// 库内恰一行在用资源（并发无重复落行）
	var cnt int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM resources WHERE value = '8888' AND status = 'in_use'`).Scan(&cnt); err != nil {
		t.Fatalf("统计资源行失败: %v", err)
	}
	if cnt != 1 {
		t.Errorf("value=8888 在用行数 = %d, want 1", cnt)
	}

	// 审计恰一条（并发无重复落审计），操作会话身份正确
	var auditSess int64
	var auditCnt int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'resource.register' AND detail LIKE '%8888%'`).Scan(&auditCnt); err != nil {
		t.Fatalf("统计登记审计失败: %v", err)
	}
	if auditCnt != 1 {
		t.Fatalf("value=8888 登记审计条数 = %d, want 1", auditCnt)
	}
	if err := st.DB.QueryRow(`SELECT session_id FROM audit_log WHERE action = 'resource.register' AND detail LIKE '%8888%'`).Scan(&auditSess); err != nil {
		t.Fatalf("查询登记审计失败: %v", err)
	}
	if auditSess != fx.sessA {
		t.Errorf("并发登记 audit.session_id = %d, want 会话 id %d", auditSess, fx.sessA)
	}
}

// TestIntegrationWindowsAC3 AC3.1~3.4 HTTP #21→#22 端到端全链：窗配置经 PUT 写入
// （契约测试以 seedWin 直插，本文件写读全走端点），as_of 全注入非墙钟（§四测试策略）。
func TestIntegrationWindowsAC3(t *testing.T) {
	// putWindows PUT #21 设窗（断言 200，返回 recorder 供回读断言）。
	putWindows := func(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
		t.Helper()
		rr := doReqHeaders(t, h, http.MethodPut, "/api/v1/projects/proj-a/columns/05/windows", []byte(body), resIdentity())
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT 窗 status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		return rr
	}
	// nowStatus GET #22 查询（断言 200+as_of=注入值），返回 (proj,column,stage) 条目状态与窗快照。
	nowStatus := func(t *testing.T, h http.Handler, asOf, project, column, stage string) (string, map[string]any) {
		t.Helper()
		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/windows/now?project="+project, nil, resIdentity())
		if rr.Code != http.StatusOK {
			t.Fatalf("GET now status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		m := decodeMap(t, rr)
		if got := m["data"].(map[string]any)["as_of"]; got != asOf {
			t.Errorf("as_of = %v, want 注入值 %q", got, asOf)
		}
		e := findWinEntry(t, winEntries(t, rr), project, column, stage)
		win, _ := e["window"].(map[string]any) // window=null 时断言失败得 nil，交调用方判
		status, _ := e["status"].(string)
		return status, win
	}

	t.Run("对拍表全四行_端到端_AC3.1_3.2_3.3", func(t *testing.T) {
		// §6.2 对拍表全四行逐条（跨午夜正反+端点闭区间），每行独立临时库走
		// PUT→GET 全链（as_of 注入构造，非墙钟）。
		for _, tt := range []struct {
			name     string
			from, to string
			asOf     string
			want     string
		}{
			{name: "跨午夜_含当前_13:00-01:00@15:00", from: "13:00", to: "01:00", asOf: "15:00", want: "allowed"},
			{name: "跨午夜_不含_15:00-05:00@10:00", from: "15:00", to: "05:00", asOf: "10:00", want: "waiting"},
			{name: "跨午夜_端点in_23:00-09:00@23:00", from: "23:00", to: "09:00", asOf: "23:00", want: "allowed"},
			{name: "同日_不含_09:00-18:00@07:00", from: "09:00", to: "18:00", asOf: "07:00", want: "waiting"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				injectFixedHHMM(t, tt.asOf)
				h, st := newResTestServer(t)
				seedResAPIFixtures(t, st)

				putWindows(t, h, fmt.Sprintf(`{"items":[{"stage":"S4","start":%q,"end":%q,"enabled":true}]}`, tt.from, tt.to))
				status, win := nowStatus(t, h, tt.asOf, "proj-a", "05", "S4")
				if status != tt.want {
					t.Errorf("as_of=%s 窗 %s-%s 状态 = %q, want %q", tt.asOf, tt.from, tt.to, status, tt.want)
				}
				// 配置且启用窗的条目两态均带快照（window 快照=PUT 原值）
				if win == nil || win["start"] != tt.from || win["end"] != tt.to {
					t.Errorf("window 快照 = %v, want %s-%s", win, tt.from, tt.to)
				}
			})
		}
	})

	t.Run("未配置阶段默认allowed_windowNull_AC3.4", func(t *testing.T) {
		injectFixedHHMM(t, "12:00")
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st) // 有项目有栏目零窗

		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/windows/now?project=proj-a", nil, resIdentity())
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body: %s）", rr.Code, rr.Body.String())
		}
		entries := winEntries(t, rr)
		// 本独立库恰一项目一栏目：entries 恰为 S0..S7 枚举全集，全部 allowed+null
		if len(entries) != 8 {
			t.Fatalf("entries 条目数 = %d, want 8（S0..S7 全集）", len(entries))
		}
		for _, e := range entries {
			if e["status"] != "allowed" || e["window"] != nil {
				t.Errorf("未配置阶段 %v 应 allowed+window=null（AC3.4）", e)
			}
		}
	})

	t.Run("窗外waiting_200非报错_AC3.2", func(t *testing.T) {
		// 窗外挂起是正常态：HTTP 200 非错误响应（状态码面强调，状态值由对拍表行 4 覆盖）
		injectFixedHHMM(t, "07:00")
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		putWindows(t, h, `{"items":[{"stage":"S4","start":"09:00","end":"18:00","enabled":true}]}`)
		rr := doReqHeaders(t, h, http.MethodGet, "/api/v1/windows/now?project=proj-a", nil, resIdentity())
		if rr.Code != http.StatusOK {
			t.Fatalf("AC3.2 窗外须 200 非报错, got %d（body: %s）", rr.Code, rr.Body.String())
		}
		e := findWinEntry(t, winEntries(t, rr), "proj-a", "05", "S4")
		if e["status"] != "waiting" {
			t.Errorf("07:00∉[09:00,18:00] 应 waiting，实际 %v", e)
		}
	})

	t.Run("跨午夜start大于end_PUT接受_AC3.3", func(t *testing.T) {
		h, st := newResTestServer(t)
		seedResAPIFixtures(t, st)

		// PUT 接受 start>end（跨午夜合法，写口不比大小），响应回读原样
		rr := putWindows(t, h, `{"items":[{"stage":"S4","start":"23:00","end":"09:00","enabled":true}]}`)
		items, ok := decodeMap(t, rr)["data"].(map[string]any)["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("PUT 响应 items 不符: %s", rr.Body.String())
		}
		i0, _ := items[0].(map[string]any)
		if i0["start"] != "23:00" || i0["end"] != "09:00" {
			t.Errorf("跨午夜窗回读 = %v, want 23:00-09:00 原样", i0)
		}

		// 判定面三时刻：前段窗内/后段窗内（跨午夜续段）/窗外
		for _, tt := range []struct{ asOf, want string }{
			{"23:30", "allowed"}, // 前段 [23:00,24:00)
			{"08:00", "allowed"}, // 后段 [00:00,09:00]
			{"12:00", "waiting"}, // 窗外
		} {
			t.Run(tt.asOf, func(t *testing.T) {
				injectFixedHHMM(t, tt.asOf)
				status, win := nowStatus(t, h, tt.asOf, "proj-a", "05", "S4")
				if status != tt.want {
					t.Errorf("as_of=%s 状态 = %q, want %q", tt.asOf, status, tt.want)
				}
				if win == nil {
					t.Errorf("配置且启用窗的条目 window 快照不应为 nil")
				}
			})
		}
	})
}

// TestSoftGateAC3_5 AC3.5 软闸双层：功能面（窗外五命令照常执行——B2/B3 未合并，
// 标记性跳过不伪造通过）+ 架构静态断言（注册面 pattern 字面无条件闸+B4 域 handler
// 零 InWindow 引用+通讯 handler 文件条件断言——B2/B3 合并 rebase 后自动生效）。
func TestSoftGateAC3_5(t *testing.T) {
	t.Run("功能面_窗外五命令照常执行", func(t *testing.T) {
		// AC3.5 功能面依赖 B2（send/poll/ack）与 B3（status/watch）命令族——两批均
		// 未合并，无法在本分支端到端执行；不伪造通过，标待补跑（信箱#9 口径）。
		// 补跑清单（B2/B3 关批后本用例改写为真断言）：
		//   1. 设全窗外窗（如 S0..S7 全配 00:00-00:01 且 as_of 远离该一分钟）；
		//   2. send/poll/ack（B2 命令族，in-process 全链）三命令照常成功；
		//   3. status/watch（B3 命令族）照常成功，窗外仅 waiting 标识不报错。
		t.Skip("待 B2/B3 关批后补跑——信箱#9 口径")
	})

	t.Run("架构面_注册面pattern字面断言_无条件闸", func(t *testing.T) {
		// #22/#21 两 pattern 经 s.handle 注册，注册行零 InWindow 引用（注册面
		// 无条件闸包裹）——软闸「查询展示路径唯一挂窗」的注册面静态锚。
		// 行级字面扫描（注册行是单行格式化代码；形态变更即 fail 提醒人工复核）。
		// B4-6b：#21 修订行冻结面扩为同 path 双方法（PUT 写+GET 读原始行），
		// 闸面同步锁双注册行形态。
		dir := testDir(t)
		lines := strings.Split(readSrc(t, dir, "server.go"), "\n")
		for _, tt := range []struct {
			pattern string
			methods []string // 该 pattern 注册面应有的全部方法行（缺一即 fail）
		}{
			{"/api/v1/windows/now", []string{"http.MethodGet"}},
			{"/api/v1/projects/{code}/columns/{col}/windows", []string{"http.MethodPut", "http.MethodGet"}},
		} {
			found := map[string]bool{}
			for _, line := range lines {
				if !strings.Contains(line, tt.pattern) || !strings.Contains(line, "s.handle(") {
					continue // 只看注册行（注释/别处出现不参与判定）
				}
				for _, m := range tt.methods {
					if strings.Contains(line, "s.handle("+m) {
						found[m] = true
					}
				}
				if strings.Contains(line, "InWindow") {
					t.Errorf("注册行含 InWindow（注册面被闸包裹）: %s", line)
				}
			}
			for _, m := range tt.methods {
				if !found[m] {
					t.Errorf("server.go 注册面缺 pattern %s 的 %s 注册行", tt.pattern, m)
				}
			}
		}
	})

	t.Run("架构面_B4域生产文件零InWindow引用_单一实现在store", func(t *testing.T) {
		// 判窗算法单一实现在 store（§6.3）：server 包现有生产文件零 InWindow 标识符
		// =窗口判定不出现在任何 handler（含注册面）——B4 侧架构判据静态化（永久生效）。
		dir := testDir(t)
		for _, f := range []string{"server.go", "handler_resources.go", "handler_window.go", "handlers_misc.go"} {
			if n := fileRefsIdent(t, filepath.Join(dir, f), "InWindow"); n > 0 {
				t.Errorf("%s 含 InWindow 标识符引用 %d 处（判窗须单一实现在 store.ListWindowStatus）", f, n)
			}
		}
	})

	t.Run("架构面_通讯handler零InWindow引用_条件断言", func(t *testing.T) {
		// B2/B3 通讯 handler 三文件（messages=send/poll/ack；sentinels；status 通讯
		// 路径）：尚不存在=B2/B3 未合并→skip；存在（rebase 后）→零 InWindow 引用
		// 自动生效（软闸红线：判定只许出现在 #22/#17 查询展示路径）。AST 级标识符
		// 扫描（go/parser 标准库）：注释/字符串字面量提及不计，仅真代码引用触发。
		dir := testDir(t)
		missing := 0
		for _, f := range []string{"handler_messages.go", "handler_sentinels.go", "handler_status.go"} {
			path := filepath.Join(dir, f)
			_, err := os.Stat(path)
			if errors.Is(err, os.ErrNotExist) {
				missing++
				continue
			}
			if err != nil {
				t.Fatalf("探测 %s 失败: %v", f, err)
			}
			if n := fileRefsIdent(t, path, "InWindow"); n > 0 {
				t.Errorf("通讯 handler %s 含 InWindow 标识符引用 %d 处（AC3.5 架构面违规：软闸红线）", f, n)
			}
		}
		if missing == 3 {
			t.Skip("B2/B3 未合并：通讯 handler 三文件均未落地——合并 rebase 后本断言自动生效")
		}
	})
}

// TestStatusLinkageAC11 AC11.1 联动复验：登记 2 资源+设 1 窗后 #17 status 的
// resources_summary（port=N account=N）与 windows 段应由真 CRUD 数据驱动。
// #17 status 端点与命令族属 B3（未合并）——不伪造通过，标待补跑；B3 以夹具直插
// 主验 AC11.1，本批真数据联动复验在其关批后进行（信箱#9 口径）。
func TestStatusLinkageAC11(t *testing.T) {
	t.Skip("待 B3 关批后补跑——#17 status 属 B3：登记 2 资源+设 1 窗后复验 resources_summary 与 windows 段真数据（AC11.1）")
}

// testDir 定位本包源码目录：以 runtime.Caller 锚定编译期源路径（测试二进制 CWD
// 理论上即包目录，此处免 CWD 依赖误判）。
func testDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 无法定位源码路径")
	}
	return filepath.Dir(file)
}

// readSrc 读包内源文件全文（注册面行级字面断言用）。
func readSrc(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("读源文件 %s 失败: %v", name, err)
	}
	return string(b)
}

// fileRefsIdent 解析 Go 源文件（go/parser 标准库，零第三方依赖），统计标识符 name
// 的 AST 级出现次数：注释与字符串字面量中的提及不计（strings 保守扫描会误报
// 「注释里写了 InWindow 三个字」的合规文件），仅真代码引用（含 store.InWindow 型
// 选择器）触发。
func fileRefsIdent(t *testing.T, path, name string) int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok && id.Name == name {
			n++
		}
		return true
	})
	return n
}

// seedSessionAt 补插一行会话（集成链跨项目身份需要——夹具函数固定集之外的增量，
// 语义与 seedResAPIFixtures 不同不算复制）。
func seedSessionAt(t *testing.T, st *store.Store, projectID, columnID int64, name, role string) {
	t.Helper()
	const ts = "2026-01-01T00:00:00Z"
	if _, err := st.DB.Exec(
		`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, columnID, name, role, ts, ts,
	); err != nil {
		t.Fatalf("插入会话夹具失败: %v", err)
	}
}
