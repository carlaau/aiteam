package server

import (
	"net/http"
	"strings"
	"testing"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// ---- b2-T7（M-43②，seq=869 建议 2）direct agent_conflict 自指提示三锚 ----
//
// 病灶：direct 发送方声明 --column 恰为目标栏时，心跳中间件本请求刚把发送方
// upsert 进目标格 → 409 agent_conflict 名单含发送方自己，用户看到名单里有自己
// 却不知何故。修复方向（绿笔落定）：名单含发送方自己（按会话 id 精确判定——
// 发送方 id=sessionIDFromCtx；名字相同但 id 不同的行不触发）时，既有主体文案
// 追加一句「；名单中 <发送方名> 是你本次 --column 声明注册进来的；若本意是
// 跨栏目投递请改用 --bus（或 --to-column）」，插入位置=教学语之后、
// agentConflictClearing 段之前；名单不含发送方时文案逐字节不变。
//
// 红笔三锚（本文件只断言红，不含任何实现）：
//   - 锚 1 SelfHint：发送方与目标格同栏同 role → 名单含自己 → message 须含
//     自指提示片段（修复前缺片段=断言红）；
//   - 锚 2 NoSelfNegativeAnchor：发送方栏≠目标栏（发送方行不在目标格）→
//     名单不含自己 → message 既有主体逐字节不变且不含自指片段（修复前后均绿）；
//   - 锚 3 CrossProjectSameNameNoHint：跨项目目标格内有同名行（id≠发送方 id）
//     → 不触发自指提示（逼绿笔用会话 id 判定而非名字判定，修复前后均绿）。
//
// 夹具套路照既有 agent_conflict 族（TestSendDirectAgentConflictDoubleAlive/
// ClearingHint/CrossProjectDegenerate）：newTestEnv + seedProjectColumn 直插 +
// injectFixedClock 双同源（seed 与判定同一固定钟，last_seen=now 恒活）+ 身份
// 四头经心跳中间件 upsert。

// TestSendDirectAgentConflictSelfHint 锚 1（修复前红）：目标 (02,executor) 格
// 预 seed 1 个活会话 executor-B；发送方身份四头=(p-a, 02, executor-A, executor)
// 与目标格同栏同 role——心跳中间件本请求把 executor-A upsert 进目标格（last_seen
// =判定时钟恒活）→ 格内 2 活 → 409 agent_conflict，名单含发送方自己。断言
// message 含自指提示片段「是你本次 --column 声明注册进来的」且含发送方名
// executor-A；既有主体锚（格子+数量）不受追加影响。修复前 message 无片段 → 红。
func TestSendDirectAgentConflictSelfHint(t *testing.T) {
	h, st := newTestEnv(t)
	projectID, columnID := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "02", store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	if _, err := st.UpsertSession(projectID, columnID, "executor-B", "executor"); err != nil {
		t.Fatalf("预注册 executor-B 失败: %v", err)
	}
	selfCell := `{"target":{"kind":"direct","column":"02","role":"executor"},"level":"normal","body":"自格定向"}`

	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages", selfCell,
		authProj, "02", "executor-A", "executor")
	wantErrBody(t, rr, http.StatusConflict, types.CodeAgentConflict)
	msg := errMsg(t, rr)
	for _, want := range []string{
		"目标 02/executor 格子存在 2 个活会话", // 既有主体锚（追加不重写）
		"是你本次 --column 声明注册进来的",      // 自指提示片段（b2-T7 定稿锚；修复前缺=红）
		// 绿笔全句锚（b2-T7 裁量追加，留痕）：定稿整句逐字节锁定（拼接位置=主体
		// 「--bus 广播」之后、清障段之前），防文案漂移。
		"；名单中 executor-A 是你本次 --column 声明注册进来的；若本意是跨栏目投递请改用 --bus（或 --to-column）",
		"executor-A", // 名单含发送方自己（病灶现场）
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息 %q 不含 %q（b2-T7 自指提示：名单含发送方自己时须教学）", msg, want)
		}
	}
	if got := countTable(t, st, "messages"); got != 0 {
		t.Errorf("拒投后 messages 行数 = %d，期望 0（409 拒投不落库）", got)
	}
}

// TestSendDirectAgentConflictNoSelfNegativeAnchor 锚 2（负面锚，修复前后均绿）：
// 发送方栏 c01≠目标栏 c02（发送方行不在目标格），目标格 seed 两活会话
// executor-B/executor-C（均非发送方名）→ 409 名单不含自己 → message 既有主体
// 整段逐字节不变（照 TestSendDirectAgentConflictClearingHint 主体锚定法）且不含
// 自指提示片段——「名单不含发送方时文案逐字节不变」的负面锚，绿笔若无条件
// 追加即红。
func TestSendDirectAgentConflictNoSelfNegativeAnchor(t *testing.T) {
	h, st := newTestEnv(t)
	projID, _ := seedProjectColumn(t, st, "p-a", store.ProjectStatusActive, "01", store.ColumnStatusActive)
	col02 := seedColumnOnly(t, st, "p-a", "02", store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	for _, name := range []string{"executor-B", "executor-C"} {
		if _, err := st.UpsertSession(projID, col02, name, "executor"); err != nil {
			t.Fatalf("预注册 %s 失败: %v", name, err)
		}
	}

	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","column":"02","role":"executor"},"level":"normal","body":"跨栏定向"}`,
		authProj, "01", "executor-A", "executor")
	wantErrBody(t, rr, http.StatusConflict, types.CodeAgentConflict)
	msg := errMsg(t, rr)
	// 既有主体整段逐字节锚（名单序=store s.id 稳定序：B 先 C 后）。
	want := "目标 02/executor 格子存在 2 个活会话（executor-B、executor-C），请个体化命名 executor_<标识> 或改用 --bus 广播"
	if !strings.Contains(msg, want) {
		t.Errorf("错误信息 %q 不含既有主体整段 %q（b2-T7：名单不含自己时文案逐字节不变）", msg, want)
	}
	if strings.Contains(msg, "是你本次") {
		t.Errorf("错误信息 %q 含自指提示片段（负面锚：发送方行不在目标格，名单不含自己，不得追加）", msg)
	}
	if got := countTable(t, st, "messages"); got != 0 {
		t.Errorf("拒投后 messages 行数 = %d，期望 0（409 拒投不落库）", got)
	}
}

// TestSendDirectAgentConflictCrossProjectSameNameNoHint 锚 3（方向约束锚，修复
// 前后均绿）：跨项目 direct（target.project=p-b）打 B 域 (02,executor) 双活格，
// 格内 executor-A 与发送方同名但属 P2 域内独立行（sessions id≠发送方 id）→
// 自指提示不得触发（跨项目同名不同行≠发送方自己）。若绿笔按名字判定则本锚红
// ——逼会话 id 精确判定。主体锚照 TestSendCrossProjectDegenerate 跨项目锚定法。
func TestSendDirectAgentConflictCrossProjectSameNameNoHint(t *testing.T) {
	h, st := newTestEnv(t)
	seedProjectColumn(t, st, authProj, store.ProjectStatusActive, "01", store.ColumnStatusActive)
	projB, colB02 := seedProjectColumn(t, st, "p-b", store.ProjectStatusActive, "02", store.ColumnStatusActive)
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	for _, name := range []string{"executor-A", "executor-B"} {
		if _, err := st.UpsertSession(projB, colB02, name, "executor"); err != nil {
			t.Fatalf("预注册 B 域 %s 失败: %v", name, err)
		}
	}

	rr := doAuthedReq(t, h, http.MethodPost, "/api/v1/messages",
		`{"target":{"kind":"direct","project":"p-b","column":"02","role":"executor"},"level":"normal","body":"跨项目定向"}`,
		authProj, "01", "executor-A", "executor")
	wantErrBody(t, rr, http.StatusConflict, types.CodeAgentConflict)
	msg := errMsg(t, rr)
	for _, want := range []string{
		"目标项目 p-b",    // 跨项目前缀锚（目标域指明）
		"02/executor", // 目标格子锚
		"executor-A",  // 陷阱武装证明：目标格同名行确在名单内
		"请个体化命名",      // 教学语主体锚
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息 %q 不含 %q（跨项目 agent_conflict 主体锚）", msg, want)
		}
	}
	if strings.Contains(msg, "是你本次") {
		t.Errorf("错误信息 %q 含自指提示片段（方向约束锚：目标格同名行属目标项目域，会话 id≠发送方 id，不得按名字判定触发提示）", msg)
	}
	if got := countTable(t, st, "messages"); got != 0 {
		t.Errorf("拒投后 messages 行数 = %d，期望 0（409 拒投不落库）", got)
	}
}
