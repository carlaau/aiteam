package store

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// ===== B3-1 哨兵域 store 测试（规格：技术设计 §3.2 表 4 / §3.6 哨兵 SQL / §7.3；b3-plan B3-1 + B3-T2）=====
//
// 时钟纪律：零真实墙钟——now 全部字符串注入（固定基准 tsOffset 推算），实现侧不调 time.Now()。

// sentinelBase 活性/心跳用例的固定基准时刻（UTC；推算偏移全部经 tsOffset，避免手算跨日错误）。
var sentinelBase = time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)

// tsOffset 基准时刻加 offsetSecs 秒后的 UTC ISO8601 串（RFC3339 → 2006-01-02T15:04:05Z）。
func tsOffset(offsetSecs int64) string {
	return sentinelBase.Add(time.Duration(offsetSecs) * time.Second).UTC().Format(time.RFC3339)
}

// countSentinels 统计 sentinels 行数（行数不增/删除生效断言用）。
func countSentinels(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sentinels`).Scan(&n); err != nil {
		t.Fatalf("统计 sentinels 失败: %v", err)
	}
	return n
}

// sentinelTimes 查指定哨兵行的 started_at / last_ping_at（时间列刷新断言用）。
func sentinelTimes(t *testing.T, s *Store, id int64) (startedAt, lastPingAt string) {
	t.Helper()
	err := s.DB.QueryRow(`SELECT started_at, last_ping_at FROM sentinels WHERE id = ?`, id).
		Scan(&startedAt, &lastPingAt)
	if err != nil {
		t.Fatalf("查询哨兵 %d 时间列失败: %v", id, err)
	}
	return startedAt, lastPingAt
}

// TestRegisterSentinel B3-T2 upsert 口径：首插返回新 id；同 (session,column,role) 重复注册
// 返回同 id + started_at/last_ping_at 双双刷新（kill 重启即复活）+ 行数不增；
// 不同 role 各占一行（UNIQUE 三元组维度分离）。
func TestRegisterSentinel(t *testing.T) {
	s := openTemp(t)
	projectID, columnID, sessionID := fixtureObservation(t, s)

	// 首次注册：新 id。
	id1, err := s.RegisterSentinel(sessionID, columnID, "executor", tsOffset(0))
	if err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}
	if id1 <= 0 {
		t.Fatalf("首次注册 id = %d，期望 > 0", id1)
	}
	gotStarted, gotPing := sentinelTimes(t, s, id1)
	if gotStarted != tsOffset(0) || gotPing != tsOffset(0) {
		t.Errorf("首插时间列 = (%s, %s)，期望均为 %s", gotStarted, gotPing, tsOffset(0))
	}

	// 重复注册（时钟推进 10s）：同 id + 两时间列刷新 + 行数不增。
	id2, err := s.RegisterSentinel(sessionID, columnID, "executor", tsOffset(10))
	if err != nil {
		t.Fatalf("重复注册失败: %v", err)
	}
	if id2 != id1 {
		t.Errorf("重复注册 id = %d，期望复用同 id %d（B3-T2）", id2, id1)
	}
	if n := countSentinels(t, s); n != 1 {
		t.Errorf("重复注册后行数 = %d，期望不增 = 1", n)
	}
	gotStarted, gotPing = sentinelTimes(t, s, id1)
	if gotStarted != tsOffset(10) || gotPing != tsOffset(10) {
		t.Errorf("重复注册后时间列 = (%s, %s)，期望双双刷新为 %s（B3-T2）", gotStarted, gotPing, tsOffset(10))
	}

	// 不同 role：新行新 id（UNIQUE(session_id,column_id,role) 按 role 维度分离）。
	id3, err := s.RegisterSentinel(sessionID, columnID, "controller", tsOffset(20))
	if err != nil {
		t.Fatalf("不同 role 注册失败: %v", err)
	}
	if id3 == id1 {
		t.Errorf("不同 role 应新开一行，实际复用 id %d", id3)
	}
	if n := countSentinels(t, s); n != 2 {
		t.Errorf("两 role 注册后行数 = %d，期望 2", n)
	}

	// 归属断言：project_id 自 sessions 带出（非零且与夹具一致），column_id 即目标信箱栏目。
	list, err := s.ListSentinelsWithLiveness(tsOffset(20), 15)
	if err != nil {
		t.Fatalf("查询哨兵清单失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("哨兵清单行数 = %d，期望 2", len(list))
	}
	for _, st := range list {
		if st.ProjectID != projectID {
			t.Errorf("哨兵 %d project_id = %d，期望自 sessions 带出 %d", st.ID, st.ProjectID, projectID)
		}
		if st.SessionID != sessionID || st.ColumnID != columnID {
			t.Errorf("哨兵 %d 归属 = (session %d, column %d)，期望 (%d, %d)",
				st.ID, st.SessionID, st.ColumnID, sessionID, columnID)
		}
	}
}

// TestRegisterSentinelSessionMissing 防御面：session 不存在时 INSERT...SELECT 空集 →
// 插 0 行返回错误（非 ErrSentinelNotFound——那是行 id 面口径）。正常路径由 B1 中间件
// 前置 upsert 保证 session 存在，此分支仅供直调防御。
func TestRegisterSentinelSessionMissing(t *testing.T) {
	s := openTemp(t)
	_, columnID, _ := fixtureObservation(t, s)

	if _, err := s.RegisterSentinel(99999, columnID, "executor", tsOffset(0)); err == nil {
		t.Fatal("session 不存在应报错，实际成功")
	} else if errors.Is(err, ErrSentinelNotFound) {
		t.Errorf("session 缺失防御错误不应混用 ErrSentinelNotFound（行 id 面口径）: %v", err)
	}
	if n := countSentinels(t, s); n != 0 {
		t.Errorf("失败注册不应落行，实际行数 = %d", n)
	}
}

// TestPingSentinel ping 只刷 last_ping_at 不动 started_at；不存在 id → ErrSentinelNotFound。
func TestPingSentinel(t *testing.T) {
	s := openTemp(t)
	_, columnID, sessionID := fixtureObservation(t, s)

	id, err := s.RegisterSentinel(sessionID, columnID, "executor", tsOffset(0))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// ping（时钟推进 5s）：last_ping_at 前进，started_at 保留首插时刻。
	if err := s.PingSentinel(id, tsOffset(5)); err != nil {
		t.Fatalf("ping 失败: %v", err)
	}
	gotStarted, gotPing := sentinelTimes(t, s, id)
	if gotPing != tsOffset(5) {
		t.Errorf("ping 后 last_ping_at = %s，期望 %s（注入时钟前进）", gotPing, tsOffset(5))
	}
	if gotStarted != tsOffset(0) {
		t.Errorf("ping 后 started_at = %s，期望不动 = %s", gotStarted, tsOffset(0))
	}

	// 不存在 id → ErrSentinelNotFound（handler 层 errors.Is 映射 404 sentinel_not_found）。
	if err := s.PingSentinel(99999, tsOffset(6)); !errors.Is(err, ErrSentinelNotFound) {
		t.Errorf("ping 不存在哨兵应返回 ErrSentinelNotFound，实际: %v", err)
	}
}

// TestDeleteSentinel 删除生效；重复删除幂等 not_found 口径（b3-spec #16「幂等 404」）。
func TestDeleteSentinel(t *testing.T) {
	s := openTemp(t)
	_, columnID, sessionID := fixtureObservation(t, s)

	id, err := s.RegisterSentinel(sessionID, columnID, "executor", tsOffset(0))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	if err := s.DeleteSentinel(id); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if n := countSentinels(t, s); n != 0 {
		t.Errorf("删除后行数 = %d，期望 0", n)
	}

	// 重复删除：返回 ErrSentinelNotFound（幂等——效果已达成，handler 映射 404）。
	if err := s.DeleteSentinel(id); !errors.Is(err, ErrSentinelNotFound) {
		t.Errorf("重复删除应返回 ErrSentinelNotFound（幂等 not_found 口径），实际: %v", err)
	}
}

// TestSentinelLiveness 活性判定（§7.3：now - last_ping_at > timeout → dead）：
// timeout=15s 绑定参数注入（禁 SQL 内 'now' 字面量——时钟全注入纪律）。
// 混合活性：16s 前→dead、14s 前→alive、60s 前→dead、恰等 15s→alive（`>` 严格大于边界固化）。
// 夹具经直插构造：不同 role 亦可经 RegisterSentinel 公开 API 造多行，直插为隔离被测面——
// List 活性判定测试不依赖 Register 正确性（role 各异仅为互不撞 UNIQUE）。
func TestSentinelLiveness(t *testing.T) {
	s := openTemp(t)
	projectID, columnID, sessionID := fixtureObservation(t, s)

	cases := []struct {
		name    string
		role    string
		pingAgo int64 // 相对基准时刻的 last_ping_at 偏移（负=过去）
		alive   bool
	}{
		{"16s前_dead", "watch-16", -16, false},
		{"14s前_alive", "watch-14", -14, true},
		{"60s前_dead", "watch-60", -60, false},
		{"恰等15s_alive", "watch-15", -15, true}, // now-last_ping=15 不大于 15 → alive（§7.3 `>` 口径）
	}
	for _, c := range cases {
		if _, err := s.DB.Exec(
			`INSERT INTO sentinels (session_id, project_id, column_id, role, started_at, last_ping_at)
			 SELECT ?, project_id, ?, ?, ?, ? FROM sessions WHERE id = ?`,
			sessionID, columnID, c.role, tsOffset(c.pingAgo), tsOffset(c.pingAgo), sessionID,
		); err != nil {
			t.Fatalf("直插夹具 %s 失败: %v", c.name, err)
		}
	}

	list, err := s.ListSentinelsWithLiveness(tsOffset(0), 15)
	if err != nil {
		t.Fatalf("活性查询失败: %v", err)
	}
	if len(list) != len(cases) {
		t.Fatalf("活性清单行数 = %d，期望 %d", len(list), len(cases))
	}

	gotAlive := make(map[string]bool, len(list)) // role → alive
	for _, st := range list {
		if st.ProjectID != projectID || st.SessionID != sessionID || st.ColumnID != columnID {
			t.Errorf("哨兵 %d（role=%s）归属 = (proj %d, session %d, column %d)，期望 (%d, %d, %d)",
				st.ID, st.Role, st.ProjectID, st.SessionID, st.ColumnID, projectID, sessionID, columnID)
		}
		gotAlive[st.Role] = st.Alive
	}
	for _, c := range cases {
		if got, ok := gotAlive[c.role]; !ok {
			t.Errorf("清单缺 role=%s", c.role)
		} else if got != c.alive {
			t.Errorf("%s（last_ping_at=%s）alive = %v，期望 %v", c.name, tsOffset(c.pingAgo), got, c.alive)
		}
	}
}

// TestUniqueConstraint 绕过 API 直插同 (session,column,role) 两行 → UNIQUE 约束冲突
// （schema 层机械保证「同会话同信箱单哨兵」，RegisterSentinel upsert 正常路径绕不开它）。
func TestUniqueConstraint(t *testing.T) {
	s := openTemp(t)
	_, columnID, sessionID := fixtureObservation(t, s)

	insert := func() error {
		_, err := s.DB.Exec(
			`INSERT INTO sentinels (session_id, project_id, column_id, role, started_at, last_ping_at)
			 SELECT ?, project_id, ?, 'executor', ?, ? FROM sessions WHERE id = ?`,
			sessionID, columnID, tsOffset(0), tsOffset(0), sessionID,
		)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("首条直插失败: %v", err)
	}
	err := insert()
	if err == nil {
		t.Fatal("同 (session,column,role) 第二条直插应触发 UNIQUE 冲突，实际成功")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("冲突报错应含 UNIQUE 字样，实际: %v", err)
	}
}

// ---- TestPollHitsKindTargetRole：poll 命中探测扩列锚（b7-W1）----

// TestPollHitsKindTargetRole b7-spec §二.5 store 层测试面：pollHitsSQL 三分支
// （direct/bus/chat+receipt）命中各 kind 返回的 Kind/TargetRole 值正确——谓词零改动
// 纯扩列，命中集语义与既有锚（server TestPollHits）一致：direct→target_role=目标角色；
// bus/chat/receipt→target_role 空串（DDL 缺省）；json tag kind/target_role 随结构带出
// （AC3 #15 hits 元素 additive 的数据面基础）。
func TestPollHitsKindTargetRole(t *testing.T) {
	s := openTemp(t)
	const t0 = "2026-10-02T08:00:00Z"
	pid := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, t0)
	c1 := seedColumn(t, s, pid, "01", "栏目01", ColumnStatusActive, t0)
	sa := seedSession(t, s, pid, c1, "executor-A", "executor", t0, t0)
	sc := seedSession(t, s, pid, c1, "controller", "controller", t0, t0)

	// 位点三行（信箱 executor-A/controller + 对话流），初值 0 锁定行存在性。
	for _, c := range []string{"executor-A", "controller", DialogConsumer(sa)} {
		if _, err := s.DB.Exec(
			`INSERT INTO ack_positions (project_id, column_id, consumer, position, updated_at)
			 VALUES (?, ?, ?, 0, ?)`, pid, c1, c, t0,
		); err != nil {
			t.Fatalf("插入位点夹具 %s 失败: %v", c, err)
		}
	}

	// 消息面：d1(direct→executor-A) b1(bus) ch1(chat→sa) rc1(receipt→sa)——四 kind 各一。
	d1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindDirect, TargetRole: "executor-A", SenderSessionID: 0, SenderLabel: "system", Level: "important", Body: "d1"}, t0)
	b1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindBus, SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "b1"}, t0)
	ch1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindChat, TargetSessionID: sa, SenderSessionID: 0, SenderLabel: "board-user:张三", Level: "normal", Body: "ch1"}, t0)
	rc1 := seedMessageFull(t, s, MessageInput{ProjectID: pid, ColumnID: c1, Kind: MessageKindReceipt, TargetSessionID: sa, SenderSessionID: 0, SenderLabel: "system", Level: "normal", Body: "rc1"}, t0)

	// executor-A 视角：direct(d1) + chat(ch1) + receipt(rc1) 三分支命中，逐条核扩列值；
	// bus(b1) 非 controller 不命中。
	got, err := s.PollHits(pid, c1, "executor-A", sa)
	if err != nil {
		t.Fatalf("PollHits(executor-A) 失败: %v", err)
	}
	want := []Hit{
		{Seq: d1, Level: "important", Kind: "direct", TargetRole: "executor-A"},
		{Seq: ch1, Level: "normal", Kind: "chat", TargetRole: ""},
		{Seq: rc1, Level: "normal", Kind: "receipt", TargetRole: ""},
	}
	if !slices.Equal(got, want) {
		t.Errorf("executor-A hits = %+v，期望 %+v（三 kind 扩列值/升序/bus 排除）", got, want)
	}

	// controller 视角：bus 分支命中（b1，target_role=''），chat 分支只按目标会话——
	// sc 无目标会话消息不命中。
	got, err = s.PollHits(pid, c1, "controller", sc)
	if err != nil {
		t.Fatalf("PollHits(controller) 失败: %v", err)
	}
	want = []Hit{{Seq: b1, Level: "normal", Kind: "bus", TargetRole: ""}}
	if !slices.Equal(got, want) {
		t.Errorf("controller hits = %+v，期望 %+v（bus 扩列值）", got, want)
	}

	// json tag 锚：kind/target_role 两键随结构带出（服务端 []store.Hit 直透的序列化面）。
	data, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("marshal Hit 失败: %v", err)
	}
	for _, key := range []string{`"kind":"bus"`, `"target_role":""`, `"seq":`, `"level":`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("Hit JSON 缺 %s 键值: %s", key, data)
		}
	}
}

// ===== b8-W2/W3 store 测试：TakeoverSentinel 接管事务 / FindAliveSentinel 持有者诊断
// 单查 / MarkSentinelLastHit 命中留痕 =====

// TestTakeoverSentinel b8-spec W2 store 层锚：同事务 DELETE 旧行+INSERT 新行——
// 返回新 id（≠旧 id，AUTOINCREMENT 不复用）、行数不增、新行时间列=注入 now、旧 id 查无；
// 无旧行坐标=普通插入（DELETE 0 行不碍 INSERT）；session 不存在防御错误不落行。
func TestTakeoverSentinel(t *testing.T) {
	s := openTemp(t)
	_, columnID, sessionID := fixtureObservation(t, s)

	oldID, err := s.RegisterSentinel(sessionID, columnID, "executor", tsOffset(0))
	if err != nil {
		t.Fatalf("注册占位哨兵失败: %v", err)
	}

	// 接管：旧删新插，新 id 新行。
	newID, err := s.TakeoverSentinel(sessionID, columnID, "executor", tsOffset(5))
	if err != nil {
		t.Fatalf("接管失败: %v", err)
	}
	if newID <= 0 || newID == oldID {
		t.Errorf("接管 id = %d，期望新 id（≠旧 id %d）", newID, oldID)
	}
	if n := countSentinels(t, s); n != 1 {
		t.Errorf("接管后行数 = %d，期望 1（旧删新插）", n)
	}
	gotStarted, gotPing := sentinelTimes(t, s, newID)
	if gotStarted != tsOffset(5) || gotPing != tsOffset(5) {
		t.Errorf("新行时间列 = (%s, %s)，期望均为 %s", gotStarted, gotPing, tsOffset(5))
	}
	if _, _, err := sentinelTimesErr(s, oldID); err == nil {
		t.Errorf("旧 id %d 应已删除，实际仍可查到", oldID)
	}

	// 无旧行坐标（不同 role）=普通插入：DELETE 0 行不影响 INSERT。
	plainID, err := s.TakeoverSentinel(sessionID, columnID, "controller", tsOffset(10))
	if err != nil {
		t.Fatalf("无旧行接管失败: %v", err)
	}
	if plainID <= 0 {
		t.Errorf("无旧行接管 id = %d，期望正数（普通插入）", plainID)
	}
	if n := countSentinels(t, s); n != 2 {
		t.Errorf("无旧行接管后行数 = %d，期望 2", n)
	}

	// session 不存在防御：不落行返回错误（INSERT...SELECT 空集同 RegisterSentinel 口径）。
	if _, err := s.TakeoverSentinel(99999, columnID, "executor", tsOffset(15)); err == nil {
		t.Fatal("session 不存在应报错，实际成功")
	}
}

// sentinelTimesErr 查哨兵时间列的错误透传变体（旧 id 删除后查无断言用）。
func sentinelTimesErr(s *Store, id int64) (startedAt, lastPingAt string, err error) {
	err = s.DB.QueryRow(`SELECT started_at, last_ping_at FROM sentinels WHERE id = ?`, id).
		Scan(&startedAt, &lastPingAt)
	return
}

// TestFindAliveSentinelHolderDiagnostics b8-W2 持有者诊断数据面（质量审查修 1 单查化）：
// 命中单语句带出 id+持有者三列（session 名/last_ping_at/age_sec——strftime 与活性
// 判定同源算式），消 FindAlive 命中与二次取持有者之间「持有者恰退出→ErrNoRows→500」
// 竞态窗；未命中（无行/死行）alive=false 零值；恰等边界=alive（`<=` 谓词口径）。
func TestFindAliveSentinelHolderDiagnostics(t *testing.T) {
	s := openTemp(t)
	_, columnID, sessionID := fixtureObservation(t, s)

	id, err := s.RegisterSentinel(sessionID, columnID, "executor", tsOffset(0))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	h, alive, err := s.FindAliveSentinel(sessionID, columnID, "executor", tsOffset(7), 15)
	if err != nil || !alive {
		t.Fatalf("活哨兵查询 = (%+v, %v, err=%v)，期望命中", h, alive, err)
	}
	if h.ID != id {
		t.Errorf("holder.ID = %d，期望既有行 id %d", h.ID, id)
	}
	if h.Session != "watch-a" {
		t.Errorf("holder.Session = %q，期望 watch-a", h.Session)
	}
	if h.LastPingAt != tsOffset(0) {
		t.Errorf("holder.LastPingAt = %q，期望 %s", h.LastPingAt, tsOffset(0))
	}
	if h.AgeSec != 7 {
		t.Errorf("holder.AgeSec = %d，期望 7", h.AgeSec)
	}

	// 恰等边界=alive：age=timeout 用 `<=` 谓词（§7.3 活性口径同源）。
	if _, alive, err := s.FindAliveSentinel(sessionID, columnID, "executor", tsOffset(15), 15); err != nil || !alive {
		t.Errorf("恰等边界 age=timeout = (%v)，期望 alive", alive)
	}
	// 死行（age>timeout）→ 未命中面（复活路径交 RegisterSentinel upsert）。
	if h, alive, err := s.FindAliveSentinel(sessionID, columnID, "executor", tsOffset(30), 15); err != nil || alive {
		t.Fatalf("死行查询 = (%+v, %v)，期望未命中", h, alive)
	}
	// 无行坐标 → 未命中零值（Holder 零值面锚——调用方 alive 判 false 不消费）。
	if h, alive, err := s.FindAliveSentinel(sessionID, columnID, "controller", tsOffset(7), 15); err != nil || alive || h.ID != 0 || h.Session != "" {
		t.Fatalf("无行查询 = (%+v, %v)，期望零值未命中", h, alive)
	}
}

// TestMarkSentinelLastHit b8-W3 命中留痕数据面：新会话列缺省空串（迁移 DEFAULT 面——
// v2 追加迁移生效锚）；标记后列值=注入时刻；未知会话报错 0 行（降级面的错误来源）。
func TestMarkSentinelLastHit(t *testing.T) {
	s := openTemp(t)
	_, _, sessionID := fixtureObservation(t, s)

	var def string
	if err := s.DB.QueryRow(`SELECT sentinel_last_hit_at FROM sessions WHERE id = ?`, sessionID).Scan(&def); err != nil {
		t.Fatalf("查询留痕列失败（迁移 v2 未生效？）: %v", err)
	}
	if def != "" {
		t.Errorf("新会话 sentinel_last_hit_at = %q，期望缺省空串", def)
	}

	if err := s.MarkSentinelLastHit(sessionID, tsOffset(30)); err != nil {
		t.Fatalf("标记命中失败: %v", err)
	}
	var got string
	if err := s.DB.QueryRow(`SELECT sentinel_last_hit_at FROM sessions WHERE id = ?`, sessionID).Scan(&got); err != nil {
		t.Fatalf("复查留痕列失败: %v", err)
	}
	if got != tsOffset(30) {
		t.Errorf("sentinel_last_hit_at = %q，期望 %s", got, tsOffset(30))
	}

	if err := s.MarkSentinelLastHit(99999, tsOffset(31)); err == nil {
		t.Error("未知会话应报错（0 行 UPDATE），实际成功")
	}
}
