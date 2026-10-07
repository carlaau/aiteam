package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aiteam/internal/types"
)

// wantTables §3.2 十一表清单（TestMigrateCreatesAll / TestReopenIdempotent 逐名断言）；
// 第 11 表 progress_reports 系 d722b93 全局变更窗（FR23 进度上报）增补。
var wantTables = []string{
	"projects", "columns", "sessions", "sentinels", "messages",
	"message_receipts", "ack_positions", "resources", "stage_windows", "audit_log",
	"progress_reports",
}

// wantTriggers §3.3 append-only 触发器清单。
var wantTriggers = []string{"messages_no_update", "messages_no_delete"}

// wantIndexes §3.4 索引清单（逐名断言；UNIQUE 约束自动生成的 sqlite_autoindex_* 不在内）。
var wantIndexes = []string{
	"idx_messages_direct", "idx_messages_bus", "idx_messages_chat",
	"idx_messages_sender", "idx_messages_bus_recent",
	"idx_sessions_last_seen", "idx_sentinels_ping",
	"idx_resources_conflict", "idx_audit_project",
	"idx_progress_session_time", "idx_progress_created",
}

// openTemp 在临时目录打开一个真文件 SQLite 库（WAL 语义需真文件，禁 :memory:），
// 测试结束自动 Close。
func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "aiteam.db"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// injectFixedClock 注入固定服务端时钟（types.NowUTC 包级注入点），测试结束自动
// 还原。与 server 包哨兵域同名 helper 统一（B3-8 收敛清单追加项①：withClock 旧名
// 删除，全仓单一时钟注入入口）。勿与 t.Parallel 同用——NowUTC 是包级全局，并行
// 测试会互相污染时钟。
func injectFixedClock(t *testing.T, at string) {
	t.Helper()
	orig := types.NowUTC
	types.NowUTC = func() string { return at }
	t.Cleanup(func() { types.NowUTC = orig })
}

// seedProject 直插一行 projects 夹具，返回自增 id（不依赖 handler/CRUD 方法）。
func seedProject(t *testing.T, s *Store, code, name, status string, hb int, createdAt string) int64 {
	t.Helper()
	res, err := s.DB.Exec(
		`INSERT INTO projects (code, name, status, heartbeat_timeout_sec, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		code, name, status, hb, createdAt, createdAt,
	)
	if err != nil {
		t.Fatalf("插入夹具项目 %q 失败: %v", code, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取夹具项目 %q 自增 id 失败: %v", code, err)
	}
	return id
}

// fixtureObservation 直插观测域夹具链（projects/columns/sessions 各一行——sentinels 表
// 三重 FK 的依赖面），返回三元 id；时间列统一 t0 注入。复用 B0 既有直插夹具模式。
// 共享区（B3-1 审查登记项）：B3-3 status 聚合等多观测域测试文件复用，同包零 import 负担；
// 时间基准 tsOffset 定义于 sentinels_test.go（同包共享）。
func fixtureObservation(t *testing.T, s *Store) (projectID, columnID, sessionID int64) {
	t.Helper()
	t0 := tsOffset(0)
	mustLastInsertID := func(what, query string, args ...any) int64 {
		t.Helper()
		res, err := s.DB.Exec(query, args...)
		if err != nil {
			t.Fatalf("插入 %s 夹具失败: %v", what, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("读取 %s 自增 id 失败: %v", what, err)
		}
		return id
	}
	projectID = mustLastInsertID("projects",
		`INSERT INTO projects (code, name, created_at, updated_at) VALUES ('proj-a', '项目A', ?, ?)`, t0, t0)
	columnID = mustLastInsertID("columns",
		`INSERT INTO columns (project_id, code, name, created_at, updated_at) VALUES (?, '05', '栏目05', ?, ?)`,
		projectID, t0, t0)
	sessionID = mustLastInsertID("sessions",
		`INSERT INTO sessions (project_id, column_id, name, role, last_seen_at, created_at) VALUES (?, ?, 'watch-a', 'executor', ?, ?)`,
		projectID, columnID, t0, t0)
	return projectID, columnID, sessionID
}

// masterNames 查 sqlite_master 中指定 type 的对象名集合（过滤 sqlite_% 内部对象）。
func masterNames(t *testing.T, db *sql.DB, typ string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = ? AND name NOT LIKE 'sqlite_%'`, typ)
	if err != nil {
		t.Fatalf("查询 sqlite_master type=%s 失败: %v", typ, err)
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("扫描对象名失败: %v", err)
		}
		names[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历对象名失败: %v", err)
	}
	return names
}

// userVersion 读取 PRAGMA user_version。
func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("读取 user_version 失败: %v", err)
	}
	return v
}

// assertAllIn 断言 got 包含 want 中每一个名字。
func assertAllIn(t *testing.T, what string, want []string, got map[string]bool) {
	t.Helper()
	for _, name := range want {
		if !got[name] {
			t.Errorf("%s 缺少 %q", what, name)
		}
	}
}

// TestMigrateCreatesAll AC16.1：Open 自动建库建表——10 表 + 2 触发器 + §3.4 全索引 + user_version=迁移数组长度。
func TestMigrateCreatesAll(t *testing.T) {
	s := openTemp(t)

	tables := masterNames(t, s.DB, "table")
	assertAllIn(t, "表", wantTables, tables)
	if len(tables) != len(wantTables) {
		t.Errorf("表数量 = %d，期望 %d（多余对象: %v）", len(tables), len(wantTables), tables)
	}

	triggers := masterNames(t, s.DB, "trigger")
	assertAllIn(t, "触发器", wantTriggers, triggers)
	if len(triggers) != len(wantTriggers) {
		t.Errorf("触发器数量 = %d，期望 %d", len(triggers), len(wantTriggers))
	}

	indexes := masterNames(t, s.DB, "index")
	assertAllIn(t, "索引", wantIndexes, indexes)
	if len(indexes) != len(wantIndexes) {
		t.Errorf("索引数量 = %d，期望 %d（实际: %v）", len(indexes), len(wantIndexes), indexes)
	}

	if v := userVersion(t, s.DB); v != len(migrations) {
		t.Errorf("user_version = %d，期望 = 迁移数组长度 %d", v, len(migrations))
	}
}

// TestReopenIdempotent 关库重开：不重复建、user_version 不变、无错误。
func TestReopenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aiteam.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("首次 Open 失败: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("首次 Close 失败: %v", err)
	}

	s2, err := Open(path) // 重开：迁移应全部跳过
	if err != nil {
		t.Fatalf("重开 Open 失败: %v", err)
	}
	defer func() { _ = s2.Close() }()

	if v := userVersion(t, s2.DB); v != len(migrations) {
		t.Errorf("重开后 user_version = %d，期望不变 = %d", v, len(migrations))
	}
	tables := masterNames(t, s2.DB, "table")
	if len(tables) != len(wantTables) {
		t.Errorf("重开后表数量 = %d，期望 %d（重复建表或多余对象: %v）", len(tables), len(wantTables), tables)
	}
	assertAllIn(t, "重开后表", wantTables, tables)
}

// TestMigrationsV3FromProject AC6：v3 追加于 v2 后，from_project 列就位且存量零回填。
// 断言面：①版本 1..N 连续+migrations[2] 固定下标锚 v3；②新库 InsertMessage 后
// messages 行 from_project=空串（另正向探针断言显式落值）；③DDL 与技术设计 §3.2
// 原文逐字一致。
// 另覆盖硬约束「存量 v2 库重开后 user_version=3 幂等」：手工造 v2 停版库
// （v1 基线+v2 扩列、user_version=2、含既有 messages 行）重开——v3 自动补列，
// 既有行零回填（DEFAULT 空串语义）、再次重开版本不动（幂等）。
// 措辞注：注释避用成对单引号字面——gofmt 1.19+ doc comment 会将其转智能引号
// （b8 4bfdccf 先例解法=改「空串」措辞）。
func TestMigrationsV3FromProject(t *testing.T) {
	// ① 迁移数组结构面：版本 1..N 连续递增（append-only 合并序，编号正确性由连续性
	// 覆盖；v3 身份由下方 migrations[2] 固定下标锚承担——不设「末位==3」末位锚，
	// v4 落地时会假红误导）。
	for i, m := range migrations {
		if m.version != i+1 {
			t.Errorf("migrations[%d].version = %d，期望 %d（版本须从 1 起连续递增）", i, m.version, i+1)
		}
	}
	// ③ DDL 逐字对拍：技术设计 §3.2 原文（含语句终结分号），防后续维护漂改。
	// migrations[2]=v3 固定下标（数组 append-only 下标恒定，追加新版本不改此锚）。
	const wantV3DDL = `ALTER TABLE messages ADD COLUMN from_project TEXT NOT NULL DEFAULT '';`
	if got := migrations[2].sql; got != wantV3DDL {
		t.Errorf("migrations[2]（v3）DDL 与技术设计 §3.2 原文不一致\n got: %q\nwant: %q", got, wantV3DDL)
	}

	// ② 新库列就位+零值落行：PRAGMA table_info 断言 from_project 列存在且
	// NOT NULL DEFAULT ''；InsertMessage 未设 FromProject（零值）→ 行落 ''。
	s := openTemp(t)
	projectID, columnID, _ := fixtureObservation(t, s)

	var (
		found   bool
		notNull int
		dflt    sql.NullString // table_info.dflt_value：无默认列=NULL，故 NullString
	)
	rows, err := s.DB.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		t.Fatalf("查询 table_info(messages) 失败: %v", err)
	}
	for rows.Next() {
		var cid, nn, pk int
		var name, colType string
		var d sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &nn, &d, &pk); err != nil {
			t.Fatalf("扫描 table_info 行失败: %v", err)
		}
		if name == "from_project" {
			found, notNull, dflt = true, nn, d
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 table_info 失败: %v", err)
	}
	if !found {
		t.Fatal("messages 缺少 from_project 列（v3 迁移未生效）")
	}
	if notNull != 1 {
		t.Errorf("from_project notnull = %d，期望 1（NOT NULL）", notNull)
	}
	if !dflt.Valid || dflt.String != "''" {
		t.Errorf("from_project dflt_value = %v，期望 \"''\"（DEFAULT ''）", dflt)
	}

	if _, err := s.InsertMessage(MessageInput{
		ProjectID: projectID,
		ColumnID:  columnID,
		Kind:      MessageKindBus,
		Level:     MessageLevelNormal,
		Body:      "b5 v3 零值探针",
	}); err != nil {
		t.Fatalf("InsertMessage 失败: %v", err)
	}
	var fromProject string
	if err := s.DB.QueryRow(`SELECT from_project FROM messages LIMIT 1`).Scan(&fromProject); err != nil {
		t.Fatalf("查询新插入行 from_project 失败: %v", err)
	}
	if fromProject != "" {
		t.Errorf("新插入消息 from_project = %q，期望 \"\"（FromProject 零值=项目内投递语义）", fromProject)
	}

	// 正向落值断言（质量审建议③）：显式 FromProject 精确落列——零值探针对
	// from_project 与其他空串参数（target_role 等）在 INSERT 三面错位互换不可
	// 分辨，正向探针按 seq 精确回读堵该缝。
	positive, err := s.InsertMessage(MessageInput{
		ProjectID:   projectID,
		ColumnID:    columnID,
		Kind:        MessageKindBus,
		Level:       MessageLevelNormal,
		Body:        "b5 v3 正向落值探针",
		FromProject: "proj-x",
	})
	if err != nil {
		t.Fatalf("InsertMessage（FromProject=proj-x）失败: %v", err)
	}
	var positiveFrom string
	if err := s.DB.QueryRow(`SELECT from_project FROM messages WHERE seq = ?`, positive.Seq).Scan(&positiveFrom); err != nil {
		t.Fatalf("查询正向探针行 from_project 失败: %v", err)
	}
	if positiveFrom != "proj-x" {
		t.Errorf("正向探针 from_project = %q，期望 \"proj-x\"（显式落值精确进列）", positiveFrom)
	}

	// 存量 v2 库重开面：裸开连接手跑 v1/v2 两条迁移并停版 user_version=2，插入一条
	// v2 时代消息行——模拟 b5 发布前的生产库现场。裸连接未开 foreign_keys，直插
	// messages 免父行夹具；v3 的 ADD COLUMN 常量默认值不触发 FK 重校验，重开
	// （foreign_keys=ON）后读面不受影响。
	v2Path := filepath.Join(t.TempDir(), "stock-v2.db")
	db, err := sql.Open("sqlite", "file:"+v2Path)
	if err != nil {
		t.Fatalf("裸开存量 v2 库失败: %v", err)
	}
	for _, m := range migrations[:2] { // v1 基线 + v2（b8 sentinel_last_hit_at），止步于 2
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("手跑迁移 v%d 失败: %v", m.version, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=2`); err != nil {
		t.Fatalf("停版 user_version=2 失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO messages (project_id, column_id, kind, level, body, created_at)
		VALUES (1, 1, 'bus', 'normal', 'v2 时代存量行', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("插入 v2 时代存量行失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭存量 v2 库失败: %v", err)
	}

	// 重开：migrate 按 user_version=2 跳过 v1/v2、只补 v3——版本止于 3 且既有行零回填。
	s2, err := Open(v2Path)
	if err != nil {
		t.Fatalf("存量 v2 库重开失败: %v", err)
	}
	if v := userVersion(t, s2.DB); v != 3 {
		t.Errorf("存量 v2 库重开后 user_version = %d，期望 3（v3 自动补列）", v)
	}
	var stock string
	if err := s2.DB.QueryRow(`SELECT from_project FROM messages LIMIT 1`).Scan(&stock); err != nil {
		t.Fatalf("查询存量行 from_project 失败: %v", err)
	}
	if stock != "" {
		t.Errorf("存量 v2 行 from_project = %q，期望 \"\"（DEFAULT '' 零回填）", stock)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("重开后 Close 失败: %v", err)
	}

	// 二次重开幂等：user_version 不动（无重复迁移/无报错）。
	s3, err := Open(v2Path)
	if err != nil {
		t.Fatalf("二次重开失败: %v", err)
	}
	defer func() { _ = s3.Close() }()
	if v := userVersion(t, s3.DB); v != 3 {
		t.Errorf("二次重开后 user_version = %d，期望 3（幂等）", v)
	}
}

// TestPragmas 连接初始化 PRAGMA（§3.1 第 5 条）：WAL / 外键 / busy 超时 / synchronous。
// 实现经 DSN _pragma 参数下发，本测试即为该机制的回归验证（连接级持久生效）。
func TestPragmas(t *testing.T) {
	s := openTemp(t)

	var mode string
	if err := s.DB.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("读 journal_mode 失败: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q，期望 \"wal\"", mode)
	}

	var syncMode int
	if err := s.DB.QueryRow("PRAGMA synchronous").Scan(&syncMode); err != nil {
		t.Fatalf("读 synchronous 失败: %v", err)
	}
	if syncMode != 1 { // NORMAL=1
		t.Errorf("synchronous = %d，期望 1（NORMAL）", syncMode)
	}

	var fk int
	if err := s.DB.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("读 foreign_keys 失败: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d，期望 1", fk)
	}

	var busy int
	if err := s.DB.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("读 busy_timeout 失败: %v", err)
	}
	if busy != 5000 {
		t.Errorf("busy_timeout = %d，期望 5000", busy)
	}
}

// TestAppendOnlyTriggers NFR3：messages 禁改删——UPDATE/DELETE 均被触发器 RAISE ABORT，
// 报错信息含 append-only 字样（§3.3 文案）。
func TestAppendOnlyTriggers(t *testing.T) {
	s := openTemp(t)
	fixtures := []string{
		`INSERT INTO projects (code, name, created_at, updated_at)
		 VALUES ('proj-a', '项目A', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO columns (project_id, code, name, created_at, updated_at)
		 VALUES (1, '05', '栏目05', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO messages (project_id, column_id, kind, level, body, created_at)
		 VALUES (1, 1, 'bus', 'normal', 'hello', '2026-01-01T00:00:00Z')`,
	}
	for _, q := range fixtures {
		if _, err := s.DB.Exec(q); err != nil {
			t.Fatalf("插入夹具失败: %v\nSQL: %s", err, q)
		}
	}

	if _, err := s.DB.Exec(`UPDATE messages SET body = 'x' WHERE seq = 1`); err == nil {
		t.Fatal("UPDATE messages 应被触发器拒绝，实际成功")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE 报错信息应含 append-only（§3.3 RAISE 文案），实际: %v", err)
	}

	if _, err := s.DB.Exec(`DELETE FROM messages WHERE seq = 1`); err == nil {
		t.Fatal("DELETE FROM messages 应被触发器拒绝，实际成功")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("DELETE 报错信息应含 append-only（§3.3 RAISE 文案），实际: %v", err)
	}

	// RAISE(ABORT) 回滚语句：该行必须原样还在。
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		t.Fatalf("统计 messages 失败: %v", err)
	}
	if n != 1 {
		t.Errorf("被拒改删后 messages 行数 = %d，期望 1", n)
	}
}

// TestCloseReopenDataIntact Close/重开数据完整。
// 机制归因：modernc/sqlite 最后一条连接 Close 时自动 checkpoint（WAL 内容合入主库并删
// -wal/-shm），重开读的是已落盘主库——本测试验证的是关闭路径数据不丢，而非 WAL 崩溃恢复；
// 真实崩溃恢复机制见 TestWALCrashRecovery。
func TestCloseReopenDataIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aiteam.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("首次 Open 失败: %v", err)
	}
	for i := range 5 {
		_, err := s1.DB.Exec(
			`INSERT INTO projects (code, name, created_at, updated_at) VALUES (?, '', ?, ?)`,
			fmt.Sprintf("proj-%d", i+1), "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z",
		)
		if err != nil {
			t.Fatalf("插入第 %d 行失败: %v", i+1, err)
		}
	}
	// Close 幂等，cleanup 二次 Close 无害。
	if err := s1.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开 Open 失败: %v", err)
	}
	defer func() { _ = s2.Close() }()

	var n int
	if err := s2.DB.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n); err != nil {
		t.Fatalf("统计 projects 失败: %v", err)
	}
	if n != 5 {
		t.Errorf("重开后 projects 行数 = %d，期望 5", n)
	}
}

// TestWALCrashRecovery AC16.2 真实机制场景：数据已提交但连接未 Close（无 checkpoint），
// 将主库 + -wal（如存在）文件对复制到同目录新路径，模拟进程被 kill -9 后残留的文件现场；
// Open 副本时由 SQLite 的 WAL recovery 从 wal 帧恢复全部数据。
func TestWALCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aiteam.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = s.Close() }() // 仅测试收尾；断言发生在此之前，连接全程持有

	for i := range 5 {
		_, err := s.DB.Exec(
			`INSERT INTO projects (code, name, created_at, updated_at) VALUES (?, '', ?, ?)`,
			fmt.Sprintf("proj-%d", i+1), "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z",
		)
		if err != nil {
			t.Fatalf("插入第 %d 行失败: %v", i+1, err)
		}
	}

	// 不 Close、不 checkpoint：直接复制文件对（-shm 是临时索引不复制，重开时自动重建）。
	copyFile := func(src, dst string) {
		t.Helper()
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", src, err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatalf("写入 %s 失败: %v", dst, err)
		}
	}
	copyPath := filepath.Join(dir, "crash-copy.db")
	copyFile(path, copyPath)
	if _, err := os.Stat(path + "-wal"); err == nil {
		copyFile(path+"-wal", copyPath+"-wal")
	} else if !os.IsNotExist(err) {
		t.Fatalf("探测 -wal 文件失败: %v", err)
	}
	// -wal 不存在 = 写入已被 checkpoint 进主库，场景退化为整库复制——断言依然成立。

	s2, err := Open(copyPath)
	if err != nil {
		t.Fatalf("Open 副本失败: %v", err)
	}
	defer func() { _ = s2.Close() }()

	var n int
	if err := s2.DB.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n); err != nil {
		t.Fatalf("统计副本 projects 失败: %v", err)
	}
	if n != 5 {
		t.Errorf("副本 projects 行数 = %d，期望 5（WAL 恢复失败）", n)
	}
}

// TestConcurrentWrites §3.1 第 3/8 条：20 并发写全成功、COUNT=20、id 两两不同。
// 机制归因：database/sql 单连接池在池层面排队串行化写访问；busy_timeout 为多连接/
// 多进程场景兜底，本测试未触达 SQLITE_BUSY 路径。AUTOINCREMENT 保证序号不复用。
func TestConcurrentWrites(t *testing.T) {
	s := openTemp(t)

	const n = 20
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs = make([]error, 0, n)
	)
	for i := range n {
		wg.Add(1)
		go func() { // go1.22 循环变量每迭代语义，闭包直捕 i
			defer wg.Done()
			_, err := s.DB.Exec(
				`INSERT INTO projects (code, name, created_at, updated_at) VALUES (?, '', ?, ?)`,
				fmt.Sprintf("proj-%02d", i), "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z",
			)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("goroutine %d: %w", i, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("并发写存在失败（%d/%d）: %v", len(errs), n, errs)
	}

	rows, err := s.DB.Query(`SELECT id FROM projects ORDER BY id`)
	if err != nil {
		t.Fatalf("查询 id 失败: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("扫描 id 失败: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 id 失败: %v", err)
	}

	if len(ids) != n {
		t.Errorf("projects 行数 = %d，期望 %d", len(ids), n)
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			t.Errorf("id %d 重复（序号应两两不同）", id)
		}
		seen[id] = true
	}
}

// countRows 统计表行数（跨文件共享 helper：projects/columns 的 WithAudit 原子性
// 断言消费——回滚后表行数须为 0）。
func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("统计 %s 失败: %v", table, err)
	}
	return n
}
