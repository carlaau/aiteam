package store

import (
	"errors"
	"strings"
	"testing"
)

// seedProgress 直插一行 progress_reports 夹具，返回自增 id（不依赖 CRUD 方法
// ——List/Latest/Stale 用例的隔离夹具面，被测查询不依赖被测写入）。
// testStatus 空串传 NULL（DDL CHECK 枚举不含空串，空=未提供的落库形态）。
func seedProgress(t *testing.T, s *Store, sessionID int64, batch, task, commitHash, branch, testStatus, summary, createdAt string) int64 {
	t.Helper()
	var statusArg any
	if testStatus != "" {
		statusArg = testStatus
	}
	res, err := s.DB.Exec(
		`INSERT INTO progress_reports (session_id, batch, task, commit_hash, branch, test_status, summary, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, nullableText(batch), nullableText(task), nullableText(commitHash),
		nullableText(branch), statusArg, nullableText(summary), createdAt,
	)
	if err != nil {
		t.Fatalf("插入夹具进度上报（session=%d）失败: %v", sessionID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取夹具进度上报自增 id 失败: %v", err)
	}
	return id
}

// progressFixture 两项目×各一栏目一会话的标准夹具：返回 p-a/executor-A 与
// p-b/executor-B 的会话 id（progress 域各用例共用）。
func progressFixture(t *testing.T, s *Store) (sidA, sidB int64) {
	t.Helper()
	pidA := seedProject(t, s, "p-a", "项目A", ProjectStatusActive, 900, "2026-01-01T00:00:00Z")
	cidA := seedColumn(t, s, pidA, "05", "栏目05", ColumnStatusActive, "2026-01-01T00:00:00Z")
	sidA = seedSession(t, s, pidA, cidA, "executor-A", "executor", "2026-01-01T08:00:00Z", "2026-01-01T08:00:00Z")
	pidB := seedProject(t, s, "p-b", "项目B", ProjectStatusActive, 900, "2026-01-01T00:01:00Z")
	cidB := seedColumn(t, s, pidB, "06", "栏目06", ColumnStatusActive, "2026-01-01T00:01:00Z")
	sidB = seedSession(t, s, pidB, cidB, "executor-B", "executor", "2026-01-01T08:00:00Z", "2026-01-01T08:00:00Z")
	return sidA, sidB
}

// ---- InsertProgress / ListProgress ----

// TestProgressInsertList 插入+过滤+分页倒序（b8-plan B8-1 用例面）：
//   - InsertProgress 返回库中真实行（RETURNING：自增 id+注入时钟，#31 响应
//     {id,created_at} 数据面）；全字段与「可选字段缺省」两形态 round-trip；
//   - ListProgress：project 过滤（projectID 0=全部，#28 QueryAudit 同款弱约定）/
//     session 按名过滤（跨项目同名生效）/组合过滤/limit 截取与 clamp；
//   - 倒序=created_at DESC,id DESC（同秒多报按 id 倒序稳定，idx_progress_created 支撑）；
//   - 空结果非 nil 空切片（JSON null 防线，仓内 list 惯例）。
func TestProgressInsertList(t *testing.T) {
	s := openTemp(t)
	sidA, sidB := progressFixture(t, s)

	// 插入三条互异时刻：全字段 / 全可选缺省 / 跨项目会话。
	injectFixedClock(t, "2026-01-01T08:00:00Z")
	full, err := s.InsertProgress(ProgressReport{
		SessionID:  sidA,
		Batch:      "B8",
		Task:       "B8-1",
		CommitHash: "abc1234",
		Branch:     "feat/b8",
		TestStatus: TestStatusPass,
		Summary:    "store 层完成",
	})
	if err != nil {
		t.Fatalf("InsertProgress(全字段) 失败: %v", err)
	}
	if full.ID <= 0 {
		t.Errorf("返回 id = %d，期望 > 0（RETURNING 自增主键）", full.ID)
	}
	if full.CreatedAt != "2026-01-01T08:00:00Z" {
		t.Errorf("返回 created_at = %q，期望注入时钟（服务端生成，AC5.4）", full.CreatedAt)
	}
	if full.SessionID != sidA || full.Batch != "B8" || full.Task != "B8-1" ||
		full.CommitHash != "abc1234" || full.Branch != "feat/b8" ||
		full.TestStatus != TestStatusPass || full.Summary != "store 层完成" {
		t.Errorf("全字段 round-trip 不符: %+v", full)
	}
	if full.Stale != ProgressStaleNone {
		t.Errorf("InsertProgress 返回值 Stale = %q，期望恒空串（stale 只在读路径计算）", full.Stale)
	}
	if full.SessionName != "" {
		t.Errorf("InsertProgress 返回值 SessionName = %q，期望空串（写路径不 JOIN 装配）", full.SessionName)
	}

	injectFixedClock(t, "2026-01-01T08:00:01Z")
	bare, err := s.InsertProgress(ProgressReport{SessionID: sidA})
	if err != nil {
		t.Fatalf("InsertProgress(全可选缺省) 失败: %v", err)
	}
	if bare.Batch != "" || bare.Task != "" || bare.CommitHash != "" || bare.Branch != "" ||
		bare.TestStatus != "" || bare.Summary != "" {
		t.Errorf("缺省可选字段 round-trip = %+v，期望全空串（空串落 NULL，读回空串）", bare)
	}
	if bare.CreatedAt != "2026-01-01T08:00:01Z" {
		t.Errorf("缺省形态 created_at = %q，期望注入时钟", bare.CreatedAt)
	}

	injectFixedClock(t, "2026-01-01T08:00:02Z")
	other, err := s.InsertProgress(ProgressReport{SessionID: sidB, Batch: "B8", Task: "B8-2"})
	if err != nil {
		t.Fatalf("InsertProgress(项目B) 失败: %v", err)
	}

	// 同秒 tie-break：与 other 同 created_at，倒序仅 id DESC 可稳定决序。
	tie, err := s.InsertProgress(ProgressReport{SessionID: sidB, Batch: "B8", Task: "tie"})
	if err != nil {
		t.Fatalf("InsertProgress(同秒) 失败: %v", err)
	}

	if n := countRows(t, s, "progress_reports"); n != 4 {
		t.Fatalf("progress_reports 行数 = %d，期望 4", n)
	}

	// 全部列表（limit=0 → 默认 100 全取）：created_at DESC,id DESC → [tie,other,bare,full]。
	all, err := s.ListProgress(0, "", 0)
	if err != nil {
		t.Fatalf("ListProgress(全部) 失败: %v", err)
	}
	wantIDs := []int64{tie.ID, other.ID, bare.ID, full.ID}
	if len(all) != len(wantIDs) {
		t.Fatalf("全部列表长度 = %d，期望 %d", len(all), len(wantIDs))
	}
	for i, want := range wantIDs {
		if all[i].ID != want {
			t.Errorf("全部列表[%d].id = %d，期望 %d（created_at DESC,id DESC 倒序）", i, all[i].ID, want)
		}
	}
	if all[0].SessionName != "executor-B" {
		t.Errorf("列表[0].SessionName = %q，期望 JOIN 装配 executor-B（#32 响应 session 字段）", all[0].SessionName)
	}
	if all[3].Task != "B8-1" {
		t.Errorf("列表[3].task = %q，期望 B8-1（对应首插 full 行）", all[3].Task)
	}

	// project 过滤（projectID 精确匹配）：仅项目 A 的两行，倒序不变。
	gotA, err := s.ListProgress(pidAOf(t, s, "p-a"), "", 0)
	if err != nil {
		t.Fatalf("ListProgress(p-a) 失败: %v", err)
	}
	if len(gotA) != 2 || gotA[0].ID != bare.ID || gotA[1].ID != full.ID {
		t.Errorf("p-a 过滤 = %+v，期望 [bare(%d), full(%d)]", gotA, bare.ID, full.ID)
	}

	// session 按名过滤：跨项目同名会话一并命中（p-b 补一个同名 executor-A）。
	pidB := projectIDByCode(t, s, "p-b")
	cidBX := columnIDByCode(t, s, "p-b", "06")
	sidBX := seedSession(t, s, pidB, cidBX, "executor-A", "executor", "2026-01-01T08:00:00Z", "2026-01-01T08:00:00Z")
	injectFixedClock(t, "2026-01-01T08:00:03Z")
	twin, err := s.InsertProgress(ProgressReport{SessionID: sidBX, Batch: "B8", Task: "twin"})
	if err != nil {
		t.Fatalf("InsertProgress(同名会话) 失败: %v", err)
	}
	gotByName, err := s.ListProgress(0, "executor-A", 0)
	if err != nil {
		t.Fatalf("ListProgress(session=executor-A) 失败: %v", err)
	}
	if len(gotByName) != 3 || gotByName[0].ID != twin.ID || gotByName[1].ID != bare.ID || gotByName[2].ID != full.ID {
		t.Errorf("session 过滤 = %+v，期望跨项目同名 3 条倒序 [twin,bare,full]", gotByName)
	}

	// project+session 组合：仅 p-b 的 executor-A 一条。
	gotBoth, err := s.ListProgress(pidB, "executor-A", 0)
	if err != nil {
		t.Fatalf("ListProgress(组合) 失败: %v", err)
	}
	if len(gotBoth) != 1 || gotBoth[0].ID != twin.ID {
		t.Errorf("组合过滤 = %+v，期望恰 twin(%d) 一条", gotBoth, twin.ID)
	}

	// limit 截取：最新 2 条。
	gotLimit, err := s.ListProgress(0, "", 2)
	if err != nil {
		t.Fatalf("ListProgress(limit=2) 失败: %v", err)
	}
	if len(gotLimit) != 2 || gotLimit[0].ID != twin.ID || gotLimit[1].ID != tie.ID {
		t.Errorf("limit=2 = %+v，期望最新两条 [twin,tie]", gotLimit)
	}

	// limit clamp（#32「默认 100/max 1000」数据面收敛，QueryAudit 同款）：超界不炸全取、
	// ≤0 兜底默认全取。
	gotClamp, err := s.ListProgress(0, "", 99999)
	if err != nil {
		t.Fatalf("ListProgress(limit 超界) 失败: %v", err)
	}
	if len(gotClamp) != 5 {
		t.Errorf("limit=99999 取回 %d 条，期望 5（clamp 到 1000=全量）", len(gotClamp))
	}
	gotDefault, err := s.ListProgress(0, "", -1)
	if err != nil {
		t.Fatalf("ListProgress(limit≤0) 失败: %v", err)
	}
	if len(gotDefault) != 5 {
		t.Errorf("limit=-1 取回 %d 条，期望 5（兜底默认 100=全量）", len(gotDefault))
	}

	// project 不存在 id → 空结果非 nil（#28 同款：存在性 404 主责在 handler 换装层，
	// store 数据面查无即空，不报错）。
	gotNone, err := s.ListProgress(99999, "", 0)
	if err != nil {
		t.Fatalf("ListProgress(不存在项目) 失败: %v", err)
	}
	if gotNone == nil || len(gotNone) != 0 {
		t.Errorf("不存在项目过滤 = %v（len=%d），期望非 nil 空切片", gotNone, len(gotNone))
	}
}

// pidAOf / projectIDByCode / columnIDByCode：测试内按 code 换装 id 的便捷读法
// （直查表，不依赖被测方法）。
func pidAOf(t *testing.T, s *Store, code string) int64 {
	t.Helper()
	return projectIDByCode(t, s, code)
}

func projectIDByCode(t *testing.T, s *Store, code string) int64 {
	t.Helper()
	var id int64
	if err := s.DB.QueryRow(`SELECT id FROM projects WHERE code = ?`, code).Scan(&id); err != nil {
		t.Fatalf("定位项目 %q 失败: %v", code, err)
	}
	return id
}

func columnIDByCode(t *testing.T, s *Store, projectCode, colCode string) int64 {
	t.Helper()
	var id int64
	if err := s.DB.QueryRow(
		`SELECT c.id FROM columns c JOIN projects p ON p.id = c.project_id
		 WHERE p.code = ? AND c.code = ?`, projectCode, colCode,
	).Scan(&id); err != nil {
		t.Fatalf("定位栏目 %s/%s 失败: %v", projectCode, colCode, err)
	}
	return id
}

// ---- LatestProgressBySession ----

// TestLatestBySession 多会话各取最近一条（b8-plan B8-1 用例面；#17 聚合与看板
// 消费面）：某会话多条只留最新（同秒 tie 由 id DESC 决序）；输出按 created_at DESC,
// id DESC（最新活动会话在前）；stale 字段按注入 now+阈值正常档为空串；空库非 nil。
func TestLatestBySession(t *testing.T) {
	s := openTemp(t)

	// 空库：非 nil 空切片。
	empty, err := s.LatestProgressBySession("2026-01-01T09:00:00Z", 3600)
	if err != nil {
		t.Fatalf("空库 LatestProgressBySession 失败: %v", err)
	}
	if empty == nil {
		t.Error("空库 LatestProgressBySession 返回 nil，期望非 nil 空切片（JSON null 防线）")
	}

	sidA, sidB := progressFixture(t, s)
	// p-b 补第三会话，构成三会话样本：A1×3 条、A2×1 条、B1×2 条。
	cidBX := columnIDByCode(t, s, "p-b", "06")
	sidB2 := seedSession(t, s, projectIDByCode(t, s, "p-b"), cidBX, "watcher-B", "watcher", "2026-01-01T08:00:00Z", "2026-01-01T08:00:00Z")

	// 夹具（直插，隔离被测查询与被测写入）：
	//   A1(executor-A)：08:00 / 08:01 / 08:02 三条 → 留 08:02；
	//   B1(executor-B)：07:59 / 08:04 两条 → 留 08:04（跨「早于」与「晚于」A1 的样本）；
	//   A2(watcher-B)：08:03 一条。
	seedProgress(t, s, sidA, "B8", "a1-old", "", "", TestStatusPass, "", "2026-01-01T08:00:00Z")
	midA1 := seedProgress(t, s, sidA, "B8", "a1-mid", "", "", TestStatusFail, "", "2026-01-01T08:01:00Z")
	latestA1 := seedProgress(t, s, sidA, "B8", "a1-new", "dead11", "feat/x", TestStatusPass, "最新", "2026-01-01T08:02:00Z")
	seedProgress(t, s, sidB, "B8", "b1-old", "", "", "", "", "2026-01-01T07:59:00Z")
	latestB1 := seedProgress(t, s, sidB, "B8", "b1-new", "", "", TestStatusUnknown, "", "2026-01-01T08:04:00Z")
	latestB2 := seedProgress(t, s, sidB2, "", "", "", "", "", "", "2026-01-01T08:03:00Z")

	got, err := s.LatestProgressBySession("2026-01-01T09:00:00Z", 3600)
	if err != nil {
		t.Fatalf("LatestProgressBySession 失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Latest 长度 = %d，期望 3（每会话恰一条）", len(got))
	}
	// 输出倒序：08:04(B1) → 08:03(A2) → 08:02(A1)。
	wantOrder := []struct {
		id    int64
		sess  string
		task  string
		stale string
	}{
		{latestB1, "executor-B", "b1-new", ProgressStaleNone}, // 距 now 56m < 60m 正常
		{latestB2, "watcher-B", "", ProgressStaleNone},        // 57m 正常
		{latestA1, "executor-A", "a1-new", ProgressStaleNone}, // 58m 正常
	}
	for i, w := range wantOrder {
		if got[i].ID != w.id {
			t.Errorf("Latest[%d].id = %d，期望 %d（该会话最新行）", i, got[i].ID, w.id)
		}
		if got[i].SessionName != w.sess {
			t.Errorf("Latest[%d].SessionName = %q，期望 %q", i, got[i].SessionName, w.sess)
		}
		if got[i].Task != w.task {
			t.Errorf("Latest[%d].task = %q，期望 %q（取该会话最新条的载荷）", i, got[i].Task, w.task)
		}
		if got[i].Stale != w.stale {
			t.Errorf("Latest[%d].Stale = %q，期望 %q", i, got[i].Stale, w.stale)
		}
	}
	// midA1 不在结果中（被 08:02 覆盖）。
	for _, row := range got {
		if row.ID == midA1 {
			t.Error("会话 executor-A 的中间行（08:01）混入 Latest，期望只留最新一条")
		}
	}
}

// ---- stale 判定（B8-T1 推荐口径：查询侧/Go 侧时间比较；严格大于——阈值整点归正常档） ----

// TestStaleFlags 注入时钟三态边界（b8-plan B8-1 用例面，AC23.3 数据面）：
// 阈值 3600s（60m，progress.stale_after 默认）下——
//   - 30m → ""（正常）；60m 整 → ""（严格大于，§7.2 失联判定同款边界口径）；
//   - 61m → "stale"（黄）；120m 整 → "stale"（2× 边界归黄）；
//   - 121m → "dead"（红）；
//   - 未来时刻（now < created_at，时钟回拨容忍）→ ""（负差值恒不超阈值，
//     TestGetSession 回拨口径同款）。
//
// 判定不依赖 time.Now 内部调用——now 全参数注入（GetSession 的 now 同形态）。
func TestStaleFlags(t *testing.T) {
	s := openTemp(t)
	sidA, sidB := progressFixture(t, s)
	cidBX := columnIDByCode(t, s, "p-b", "06")
	pidB := projectIDByCode(t, s, "p-b")
	sidB2 := seedSession(t, s, pidB, cidBX, "watcher-B", "watcher", "2026-01-01T08:00:00Z", "2026-01-01T08:00:00Z")
	sidB3 := seedSession(t, s, pidB, cidBX, "foreman-B", "foreman", "2026-01-01T08:00:00Z", "2026-01-01T08:00:00Z")
	sidB4 := seedSession(t, s, pidB, cidBX, "controller-B", "controller", "2026-01-01T08:00:00Z", "2026-01-01T08:00:00Z")
	sidB5 := seedSession(t, s, pidB, cidBX, "sales-B", "salesperson", "2026-01-01T08:00:00Z", "2026-01-01T08:00:00Z")

	// 基准 now=T0=2026-01-01T12:00:00Z，六会话各一条不同时刻的夹具。
	const now = "2026-01-01T12:00:00Z"
	fixtures := []struct {
		name string
		sid  int64
		at   string // created_at
		want string
	}{
		{"30m 正常", sidA, isoTime(now, -30*60), ProgressStaleNone},
		{"60m 整点边界（严格大于→正常档）", sidB, isoTime(now, -60*60), ProgressStaleNone},
		{"61m 黄", sidB2, isoTime(now, -61*60), ProgressStaleYellow},
		{"120m 整点边界（2× 归黄档）", sidB3, isoTime(now, -120*60), ProgressStaleYellow},
		{"121m 红", sidB4, isoTime(now, -121*60), ProgressStaleRed},
		{"未来时刻（回拨容忍→正常档）", sidB5, isoTime(now, 10*60), ProgressStaleNone},
	}
	for _, f := range fixtures {
		seedProgress(t, s, f.sid, "B8", f.name, "", "", "", "", f.at)
	}

	got, err := s.LatestProgressBySession(now, 3600)
	if err != nil {
		t.Fatalf("LatestProgressBySession(注入 now) 失败: %v", err)
	}
	if len(got) != len(fixtures) {
		t.Fatalf("Latest 长度 = %d，期望 %d", len(got), len(fixtures))
	}
	bySession := map[int64]string{}
	for _, row := range got {
		bySession[row.SessionID] = row.Stale
	}
	for _, f := range fixtures {
		if bySession[f.sid] != f.want {
			t.Errorf("%s：session=%d Stale = %q，期望 %q", f.name, f.sid, bySession[f.sid], f.want)
		}
	}
}

// ---- 约束与兜底 ----

// TestTestStatusConstraint test_status CHECK 反例（b8-plan B8-1 用例面）：非法值
// 插入触发 DDL CHECK 约束错误并透出为 error（数据面故障，非参数哨兵）；合法三值
// pass/fail/unknown 与空串（未提供→落 NULL）均成功。
func TestTestStatusConstraint(t *testing.T) {
	s := openTemp(t)
	sidA, _ := progressFixture(t, s)

	// CHECK 反例：非法枚举值被 DDL 拒绝，错误透出（含「插入进度」上下文包装），
	// 且不落数据面。
	bad, err := s.InsertProgress(ProgressReport{SessionID: sidA, TestStatus: "bogus"})
	if err == nil {
		t.Fatal("test_status=非枚举值 应被 CHECK 约束拒绝，实际成功")
	}
	if bad.ID != 0 {
		t.Errorf("被拒插入返回 id = %d，期望 0（错误返回零值行）", bad.ID)
	}
	if !strings.Contains(err.Error(), "插入进度") {
		t.Errorf("CHECK 错误缺「插入进度」上下文包装: %v", err)
	}
	if n := countRows(t, s, "progress_reports"); n != 0 {
		t.Errorf("CHECK 拒绝后 progress_reports 行数 = %d，期望 0", n)
	}

	// 合法三值 + 空串（未提供）全部成功。
	for _, tc := range []struct{ status, want string }{
		{TestStatusPass, TestStatusPass},
		{TestStatusFail, TestStatusFail},
		{TestStatusUnknown, TestStatusUnknown},
		{"", ""}, // 未提供 → NULL 落库，读回空串
	} {
		got, err := s.InsertProgress(ProgressReport{SessionID: sidA, TestStatus: tc.status})
		if err != nil {
			t.Fatalf("test_status=%q 插入失败: %v", tc.status, err)
		}
		if got.TestStatus != tc.want {
			t.Errorf("test_status=%q 读回 %q，期望 %q", tc.status, got.TestStatus, tc.want)
		}
	}
}

// TestProgressInsertForeignKey FK 兜底路径（B1 口径：会话存在性校验+隐式注册
// 主责在 B1-4 中间件——GetProjectColumnForSession 404 + UpsertSession 注册，
// store 层数据面兜底）：session_id 不存在时 foreign_keys=ON 拒绝插入，错误经
// 「store: 插入进度 …」上下文包装返回，事务不落数据面。与 TestUpsertSessionForeignKey
// 同构钉底（错误可辨识性：非参数哨兵 ErrSessionInvalid）。
func TestProgressInsertForeignKey(t *testing.T) {
	s := openTemp(t)

	_, err := s.InsertProgress(ProgressReport{SessionID: 9999, Batch: "B8", Task: "orphan"})
	if err == nil {
		t.Fatal("session_id 不存在应 FK 拒绝，实际成功")
	}
	if errors.Is(err, ErrSessionInvalid) {
		t.Errorf("FK 错误误判为参数哨兵 ErrSessionInvalid: %v", err)
	}
	if !strings.Contains(err.Error(), "插入进度") {
		t.Errorf("FK 错误缺「插入进度」上下文包装: %v", err)
	}
	if n := countRows(t, s, "progress_reports"); n != 0 {
		t.Errorf("FK 拒绝后 progress_reports 行数 = %d，期望 0", n)
	}
}
