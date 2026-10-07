package cli

import (
	"database/sql"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aiteam/internal/store"
)

// fixtureTime 夹具行统一时间戳：schema 各时间列 NOT NULL 且无 DEFAULT，须显式给值。
const fixtureTime = "2026-10-02T00:00:00Z"

// seedFixture 向 db 插入跨表夹具：projects→columns→sessions→messages(3)→
// message_receipts→ack_positions→audit_log(2)，满足 FK/CHECK/NOT NULL 约束。
// 返回各表插入行数（期望基准；未列出的表期望 0 行）。messages 有 append-only
// 触发器但仅禁 UPDATE/DELETE，INSERT 不受限。
func seedFixture(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	mustExec := func(desc string, err error) {
		if err != nil {
			t.Fatalf("插入夹具 %s 失败: %v", desc, err)
		}
	}
	_, err := db.Exec(
		`INSERT INTO projects(code,name,created_at,updated_at) VALUES('proj-b6','备份夹具项目',?,?)`,
		fixtureTime, fixtureTime)
	mustExec("projects", err)
	_, err = db.Exec(
		`INSERT INTO columns(project_id,code,name,created_at,updated_at) VALUES(1,'05','备份夹具栏目',?,?)`,
		fixtureTime, fixtureTime)
	mustExec("columns", err)
	_, err = db.Exec(
		`INSERT INTO sessions(project_id,column_id,name,role,last_seen_at,created_at) VALUES(1,1,'controller','controller',?,?)`,
		fixtureTime, fixtureTime)
	mustExec("sessions", err)
	for range 3 {
		_, err = db.Exec(
			`INSERT INTO messages(project_id,column_id,kind,level,body,created_at) VALUES(1,1,'bus','normal','备份夹具消息',?)`,
			fixtureTime)
		mustExec("messages", err)
	}
	_, err = db.Exec(
		`INSERT INTO message_receipts(message_seq,receipt_session_id,created_at) VALUES(1,1,?)`,
		fixtureTime)
	mustExec("message_receipts", err)
	_, err = db.Exec(
		`INSERT INTO ack_positions(project_id,column_id,consumer,position,updated_at) VALUES(1,1,'controller',2,?)`,
		fixtureTime)
	mustExec("ack_positions", err)
	for range 2 {
		_, err = db.Exec(
			`INSERT INTO audit_log(action,detail,created_at) VALUES('session_register','{}',?)`,
			fixtureTime)
		mustExec("audit_log", err)
	}
	return map[string]int{
		"projects": 1, "columns": 1, "sessions": 1, "messages": 3,
		"message_receipts": 1, "ack_positions": 1, "audit_log": 2,
	}
}

// seedDBFile 在 path 处经 store.Open 建库、灌夹具后关闭（一次性造库，返回期望行数）。
func seedDBFile(t *testing.T, path string) map[string]int {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("造源库 %s 失败: %v", path, err)
	}
	want := seedFixture(t, s.DB)
	if err := s.Close(); err != nil {
		t.Fatalf("关闭源库 %s 失败: %v", path, err)
	}
	return want
}

// countAllTables 裸开 path 处库（不触发迁移，保证「产物独立可开」判据纯净），
// 逐表 COUNT(*)。表名来自 wantServeTables 常量清单，拼接无注入面。
func countAllTables(t *testing.T, path string, tables []string) map[string]int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开 %s 统计行数失败: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	counts := map[string]int{}
	for _, tbl := range tables {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Fatalf("统计 %s 表 %s 行数失败: %v", path, tbl, err)
		}
		counts[tbl] = n
	}
	return counts
}

// assertFixtureCounts 断言 path 处库各表行数与期望一致（want 未列出的表期望 0）。
func assertFixtureCounts(t *testing.T, path string, want map[string]int) {
	t.Helper()
	got := countAllTables(t, path, wantServeTables)
	for _, tbl := range wantServeTables {
		if got[tbl] != want[tbl] {
			t.Errorf("库 %s 表 %s 行数 = %d，期望 %d", path, tbl, got[tbl], want[tbl])
		}
	}
}

// userVersionOf 裸开 path 处库读取 PRAGMA user_version（=store.SchemaVersion()
// 迁移末版——b8-W3 v2 起，锚导出面自跟免逐批改字面量）。
func userVersionOf(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开 %s 读取 user_version 失败: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	var uv int
	if err := db.QueryRow("PRAGMA user_version").Scan(&uv); err != nil {
		t.Fatalf("读取 %s user_version 失败: %v", path, err)
	}
	return uv
}

// captureStdout 捕获 fn 执行期间进程 stdout（断言 OK 行输出格式用）。
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建 stdout 捕获管道失败: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	fnErr := fn()
	os.Stdout = old
	_ = w.Close()
	data, _ := io.ReadAll(r)
	_ = r.Close()
	return string(data), fnErr
}

// TestBackupConsistency D6 产物判据：跨表夹具源库→backup→产物独立裸开校验——
// 十表齐+各表行数与源一致+user_version 同。两个附加规格一并覆盖：
//  1. WAL 未 checkpoint 场景：夹具写入后源库连接不 Close 直接备（数据在 -wal 中
//     未合并回主库），VACUUM INTO 快照必须数据全在；
//  2. 含空格产物路径：目标目录与文件名均含空格，参数绑定路径天然兼容。
func TestBackupConsistency(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "src.db")
	outPath := filepath.Join(dir, "back up dir", "backup file.db")
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		t.Fatalf("建含空格产物目录失败: %v", err)
	}

	src, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("打开源库失败: %v", err)
	}
	// 测试收尾才关源库：下方 RunBackup 执行期间源库连接未 Close——
	// WAL 未 checkpoint 热备场景成立；defer 保证失败路径不泄漏句柄
	defer func() { _ = src.Close() }()
	want := seedFixture(t, src.DB)

	out, runErr := captureStdout(t, func() error {
		return RunBackup([]string{"--db", dbPath, "--out", outPath})
	})
	if runErr != nil {
		t.Fatalf("backup 失败: %v", runErr)
	}

	// 成功输出格式（§D6 行为规格）：OK backup=<path> tables=11
	if !strings.HasPrefix(out, "OK backup=") || !strings.Contains(out, "tables=11") {
		t.Errorf("stdout = %q，期望前缀 OK backup= 且含 tables=11", out)
	}

	// 产物独立可开：十表齐（表名单 wantServeTables 与 schema.sql 同源）
	assertTablesComplete(t, outPath)
	if uv := userVersionOf(t, outPath); uv != store.SchemaVersion() {
		t.Errorf("产物 user_version = %d，期望 = store.SchemaVersion() %d", uv, store.SchemaVersion())
	}
	assertFixtureCounts(t, outPath, want)
}

// TestBackupTargetExists B6-T2 裁定（照推荐：原样报错退出不覆盖——静默覆盖备份=
// 数据事故）：目标已存在→报错，且源/目标内容均不变。子场景预置形态各异的既有
// 目标（普通文本 / 另一 SQLite 库 / 同名目录），断言均不被篡改。
func TestBackupTargetExists(t *testing.T) {
	t.Run("目标为普通文本文件", func(t *testing.T) {
		dir := t.TempDir()
		srcPath := filepath.Join(dir, "src.db")
		want := seedDBFile(t, srcPath)
		outPath := filepath.Join(dir, "existing.txt")
		existing := []byte("不可覆盖的既有文件内容")
		if err := os.WriteFile(outPath, existing, 0o644); err != nil {
			t.Fatalf("预置既有目标文件失败: %v", err)
		}

		if err := RunBackup([]string{"--db", srcPath, "--out", outPath}); err == nil {
			t.Fatal("目标已存在应报错拒绝（B6-T2），实际 backup 成功")
		}

		got, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("读取既有目标文件失败: %v", err)
		}
		if string(got) != string(existing) {
			t.Errorf("既有目标文件被覆盖: %q", got)
		}
		assertFixtureCounts(t, srcPath, want) // 源内容不变
	})

	t.Run("目标为另一SQLite库", func(t *testing.T) {
		dir := t.TempDir()
		srcPath := filepath.Join(dir, "src.db")
		want := seedDBFile(t, srcPath)
		outPath := filepath.Join(dir, "target.db")
		// 预置内容与源库不同的既有库（code 可区分，覆盖即现形）
		tgt, err := store.Open(outPath)
		if err != nil {
			t.Fatalf("预置既有目标库失败: %v", err)
		}
		if _, err := tgt.DB.Exec(
			`INSERT INTO projects(code,name,created_at,updated_at) VALUES('proj-target','既有目标库',?,?)`,
			fixtureTime, fixtureTime); err != nil {
			t.Fatalf("预置既有目标库数据失败: %v", err)
		}
		if err := tgt.Close(); err != nil {
			t.Fatalf("关闭既有目标库失败: %v", err)
		}

		if err := RunBackup([]string{"--db", srcPath, "--out", outPath}); err == nil {
			t.Fatal("目标已存在应报错拒绝（B6-T2），实际 backup 成功")
		}

		// 目标库未被覆盖：projects.code 仍为预置值（若被 VACUUM INTO 覆盖则变 proj-b6）
		db, err := sql.Open("sqlite", outPath)
		if err != nil {
			t.Fatalf("打开既有目标库失败: %v", err)
		}
		var code string
		if err := db.QueryRow(`SELECT code FROM projects`).Scan(&code); err != nil {
			t.Fatalf("读取既有目标库数据失败: %v", err)
		}
		_ = db.Close()
		if code != "proj-target" {
			t.Errorf("既有目标库被覆盖，projects.code = %q，期望 proj-target", code)
		}
		assertFixtureCounts(t, srcPath, want) // 源内容不变
	})

	t.Run("目标为已存在目录", func(t *testing.T) {
		dir := t.TempDir()
		srcPath := filepath.Join(dir, "src.db")
		seedDBFile(t, srcPath)
		outPath := filepath.Join(dir, "existing-dir")
		if err := os.MkdirAll(outPath, 0o755); err != nil {
			t.Fatalf("预置既有目录失败: %v", err)
		}

		err := RunBackup([]string{"--db", srcPath, "--out", outPath})
		if err == nil {
			t.Fatal("目标为已存在目录应报错拒绝，实际 backup 成功")
		}
		// 文案必须指明是目录：笼统的「删除该文件」会诱导使用者误删整棵目录树
		//（审查 Minor）
		if !strings.Contains(err.Error(), "目录") {
			t.Errorf("错误信息 %q 应明确指出目标是目录", err.Error())
		}
		// 目录本体未被破坏：仍可写入探针文件
		probe := filepath.Join(outPath, "probe.txt")
		if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
			t.Errorf("既有目录疑似被破坏（探针文件写入失败）: %v", err)
		}
	})
}

// TestBackupWhileServing D6 热备判据：in-process serve 运行中（连接持有+WAL 未收尾）
// 执行 backup，产物完整可查。
//
// 写负载说明：send CLI 属 B2 批尚未合并——用裸 SQL INSERT 直插 messages 表等价表达，
// 依据：send 的服务端落库路径即 INSERT INTO messages（技术设计 FR5，服务端生成 seq），
// 此处只需制造真实写负载验证 VACUUM INTO 快照语义，无需经 HTTP 全链。
// 顺序为「写满→停写→backup」而非边写边备：实现的自校验按规格「产物与源实时比对，
// 不一致=失败」，若写与备重叠则校验窗口内源库持续增长必然保守报失败——停写后 backup
// 且 serve/写连接全程不关，已完整覆盖「活跃 WAL 库在线热备」本体。
func TestBackupWhileServing(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hot.db")
	cfgPath := writeTempConfig(t, dbPath)

	ready, errs, _, cancel := startServeForTest(t, []string{"--config", cfgPath})
	addr := waitReady(t, ready, errs)
	assertPingOK(t, addr) // serve 确在运行

	// 独立写负载连接（等价性见函数注释）
	src, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开写负载连接失败: %v", err)
	}
	defer func() { _ = src.Close() }()
	want := seedFixture(t, src) // messages 的 FK 父行等夹具

	// 定时写 goroutine：约 2ms 一行，收 stop 后回报累计写入行数。
	// stopWrite 用 sync.OnceFunc 包裹、正常路径与 t.Cleanup 均调：任一步 t.Fatalf
	// 提前 Goexit 时 Cleanup 兜底关停写 goroutine——否则 2ms 周期的无限写会一直
	// 打到测试收尾之后（审查 Minor：失败路径 goroutine 泄漏）。
	stop := make(chan struct{})
	stopWrite := sync.OnceFunc(func() { close(stop) })
	t.Cleanup(stopWrite)
	written := make(chan int, 1)
	go func() {
		n := 0
		for {
			select {
			case <-stop:
				written <- n
				return
			case <-time.After(2 * time.Millisecond):
				if _, err := src.Exec(
					`INSERT INTO messages(project_id,column_id,kind,level,body,created_at) VALUES(1,1,'bus','normal','热备写负载',?)`,
					fixtureTime); err != nil {
					t.Errorf("写负载插入失败: %v", err)
					written <- n
					return
				}
				n++
			}
		}
	}()

	// 等写负载积累 ≥5 行（快照有实质增量可验）
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := src.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
			t.Fatalf("查询写负载进度失败: %v", err)
		}
		if n >= 5 {
			break
		}
		if time.Now().After(deadline) {
			stopWrite()
			t.Fatal("写负载 5s 内未积累到 5 行")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stopWrite()
	w := <-written
	expect := maps.Clone(want)
	expect["messages"] += w

	// backup：serve 与写负载连接全程未关（活跃库在线热备）
	outPath := filepath.Join(dir, "hot-backup.db")
	if err := RunBackup([]string{"--config", cfgPath, "--out", outPath}); err != nil {
		t.Fatalf("serve 运行中 backup 失败: %v", err)
	}

	// 产物完整可查：十表齐+行数与源一致（此刻无并发写，快照应与源完全一致）
	assertTablesComplete(t, outPath)
	assertFixtureCounts(t, outPath, expect)
	if uv := userVersionOf(t, outPath); uv != store.SchemaVersion() {
		t.Errorf("产物 user_version = %d，期望 = store.SchemaVersion() %d", uv, store.SchemaVersion())
	}
	_ = cancel // serve 停机由 startServeForTest 的 Cleanup 兜底
}

// TestBackupPathResolution 三级 db 路径解析（§12 D6：--db > --config 读 db.path >
// 默认 db/aiteam.db——内建默认值与 config.Default() 单一来源，序 9 拍平改值；
// backup 维护命令保持相对值按 CWD 解析）各一例；外加三个失败分支：显式 --config 文件
// 不存在（ErrUsage——与 serve 静默默认的语义区分）、--out 父目录不存在（ErrUsage，
// 本地参数错）、源库不存在（不预检会被 store.Open 静默建空库假备份）。
func TestBackupPathResolution(t *testing.T) {
	t.Run("db显式", func(t *testing.T) {
		dir := t.TempDir()
		srcPath := filepath.Join(dir, "explicit.db")
		want := seedDBFile(t, srcPath)
		outPath := filepath.Join(dir, "out.db")
		if err := RunBackup([]string{"--db", srcPath, "--out", outPath}); err != nil {
			t.Fatalf("backup 失败: %v", err)
		}
		assertFixtureCounts(t, outPath, want)
	})

	t.Run("config读db.path", func(t *testing.T) {
		dir := t.TempDir()
		srcPath := filepath.Join(dir, "from-config", "cfg.db")
		if err := os.MkdirAll(filepath.Dir(srcPath), 0o755); err != nil {
			t.Fatalf("建 db 目录失败: %v", err)
		}
		want := seedDBFile(t, srcPath)
		cfgPath := writeTempConfig(t, srcPath)
		outPath := filepath.Join(dir, "out.db")
		if err := RunBackup([]string{"--config", cfgPath, "--out", outPath}); err != nil {
			t.Fatalf("backup 失败: %v", err)
		}
		assertFixtureCounts(t, outPath, want)
	})

	t.Run("默认db/aiteam.db", func(t *testing.T) {
		dir := t.TempDir()
		chdirForTest(t, dir) // CWD 无 aiteam-config.json → 应落内建默认 db.path=db/aiteam.db
		if err := os.MkdirAll("db", 0o755); err != nil {
			t.Fatalf("建默认库目录失败: %v", err)
		}
		want := seedDBFile(t, filepath.Join("db", "aiteam.db")) // 相对 CWD（维护命令语义）
		outPath := filepath.Join(dir, "out.db")
		if err := RunBackup([]string{"--out", outPath}); err != nil {
			t.Fatalf("backup 失败: %v", err)
		}
		assertFixtureCounts(t, outPath, want)
	})

	t.Run("config文件不存在报ErrUsage", func(t *testing.T) {
		dir := t.TempDir()
		outPath := filepath.Join(dir, "out.db")
		err := RunBackup([]string{"--config", filepath.Join(dir, "no-such.json"), "--out", outPath})
		if !errors.Is(err, ErrUsage) {
			t.Errorf("错误 = %v，期望 ErrUsage（显式参数指向的本地文件不存在=本地参数错）", err)
		}
		if _, statErr := os.Stat(outPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("拒绝路径不应产出备份文件（stat err=%v）", statErr)
		}
	})

	t.Run("out父目录不存在报ErrUsage", func(t *testing.T) {
		dir := t.TempDir()
		srcPath := filepath.Join(dir, "src.db")
		seedDBFile(t, srcPath)
		err := RunBackup([]string{"--db", srcPath, "--out", filepath.Join(dir, "no-such-dir", "out.db")})
		if !errors.Is(err, ErrUsage) {
			t.Errorf("错误 = %v，期望 ErrUsage（输出路径非法=本地参数错）", err)
		}
	})

	t.Run("源库不存在报错", func(t *testing.T) {
		err := RunBackup([]string{
			"--db", filepath.Join(t.TempDir(), "nope.db"),
			"--out", filepath.Join(t.TempDir(), "out.db"),
		})
		if err == nil {
			t.Fatal("源库不存在应报错（不预检会被 store.Open 静默创建空库，备份出假产物）")
		}
		// 钉死退出码口径：源库不存在属开库失败族=退 1（普通 error），
		// 不得被标为 ErrUsage 退 2（审查 Minor：防两分类将来互相串位）
		if errors.Is(err, ErrUsage) {
			t.Errorf("源库不存在应归运行期错误（退 1），实际被标记为 ErrUsage（退 2）: %v", err)
		}
	})
}

// TestBackupCWDProbe #18 裁定（b7b-5 走查实证缺陷修复）：无 --db/--config 时
// backup 探测 CWD 的 aiteam-config.json（备份对象=正在服务的库，同源原则；序 9
// 拍平起 serve 改 exe 目录基准，backup 维护命令保持 CWD 语义）。实证缺陷原貌：
// serve 用自定义 db.path=custom.db 服务时，无参 backup 静默备份内建默认库
// （空库 tables=11 假绿），恢复四步把空库拷回=现场清零实害。
func TestBackupCWDProbe(t *testing.T) {
	t.Run("CWD配置含自定义db.path备对库且明示source", func(t *testing.T) {
		dir := t.TempDir()
		chdirForTest(t, dir)
		// CWD 部署现场：aiteam-config.json 指向 custom.db（相对路径，相对 CWD 解析
		// 与 serve 同款），custom.db 含跨表夹具数据
		cfgContent := `{"db": {"path": "custom.db"}}`
		if err := os.WriteFile(filepath.Join(dir, "aiteam-config.json"), []byte(cfgContent), 0o644); err != nil {
			t.Fatalf("写 CWD 配置文件失败: %v", err)
		}
		want := seedDBFile(t, "custom.db") // 相对 CWD
		// 干扰项：内建默认路径 db/aiteam.db 预置一张 11 表空库（复刻实证缺陷现场）——
		// 若实现退回旧行为备了它，tables=11 照样成立（假绿）但夹具行数全 0，行数断言必炸
		if err := os.MkdirAll("db", 0o755); err != nil {
			t.Fatalf("建默认库目录失败: %v", err)
		}
		empty, err := store.Open(filepath.Join("db", "aiteam.db"))
		if err != nil {
			t.Fatalf("预置空默认库失败: %v", err)
		}
		if err := empty.Close(); err != nil {
			t.Fatalf("关闭空默认库失败: %v", err)
		}

		outPath := filepath.Join(dir, "out.db")
		out, runErr := captureStdout(t, func() error {
			return RunBackup([]string{"--out", outPath}) // 无 --db/--config：CWD 探测
		})
		if runErr != nil {
			t.Fatalf("backup 失败: %v", runErr)
		}

		// 备对库：产物各表行数=custom.db 夹具（非空库 0 行）
		assertFixtureCounts(t, outPath, want)
		// 明示实际源库（#18 裁定：探测=隐式行为，必须可观测防备错库不知情）
		if !strings.Contains(out, "source=custom.db") {
			t.Errorf("stdout = %q，期望含 source=custom.db（探测命中自定义 db.path 须明示实际源库）", out)
		}
		if !strings.HasPrefix(out, "OK backup=") || !strings.Contains(out, "tables=11") {
			t.Errorf("stdout = %q，期望前缀 OK backup= 且含 tables=11（既有输出契约不被破坏）", out)
		}
	})

	t.Run("CWD裸目录走内建默认且不提示source", func(t *testing.T) {
		dir := t.TempDir()
		chdirForTest(t, dir) // 裸目录：无 aiteam-config.json → 内建默认 db/aiteam.db
		if err := os.MkdirAll("db", 0o755); err != nil {
			t.Fatalf("建默认库目录失败: %v", err)
		}
		want := seedDBFile(t, filepath.Join("db", "aiteam.db")) // 相对 CWD（维护命令语义）
		outPath := filepath.Join(dir, "out.db")

		out, runErr := captureStdout(t, func() error {
			return RunBackup([]string{"--out", outPath}) // 无 --db/--config：零配置部署语义不变
		})
		if runErr != nil {
			t.Fatalf("backup 失败: %v", runErr)
		}

		assertFixtureCounts(t, outPath, want)
		// 默认库无误导面，OK 行不追加 source=（既有输出形态保持，消费方零感知）
		if strings.Contains(out, "source=") {
			t.Errorf("stdout = %q，裸目录走内建默认不应追加 source= 提示", out)
		}
	})

	// 快审采纳（排他断言）：显式 --db / --config 是使用者的主动精确指路，OK 行
	// 不追加 source=——该提示专属「无参 CWD 探测命中自定义 db.path」场景。钉死
	// 排他面，防将来追加条件被改坏后显式场景静默混入提示（假绿不易察觉）。
	t.Run("显式db与config不提示source", func(t *testing.T) {
		dir := t.TempDir()
		srcPath := filepath.Join(dir, "explicit-src.db")
		seedDBFile(t, srcPath)
		cfgPath := writeTempConfig(t, srcPath)

		for _, args := range [][]string{
			{"--db", srcPath, "--out", filepath.Join(dir, "out-flag.db")},
			{"--config", cfgPath, "--out", filepath.Join(dir, "out-cfg.db")},
		} {
			out, runErr := captureStdout(t, func() error {
				return RunBackup(args)
			})
			if runErr != nil {
				t.Fatalf("backup(%v) 失败: %v", args, runErr)
			}
			if strings.Contains(out, "source=") {
				t.Errorf("显式参数场景 stdout = %q，不应追加 source= 提示（仅无参 CWD 探测命中自定义 db.path 时明示）", out)
			}
		}
	})
}
