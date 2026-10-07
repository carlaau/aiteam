package cli

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"aiteam/internal/config"
	"aiteam/internal/store"
)

// RunBackup 是 `aiteam backup --out <file> [--db <path>] [--config <path>]` 子命令入口
// （技术设计 §12 D6、NFR3）：基于 SQLite VACUUM INTO 的在线热备——WAL 模式下
// VACUUM INTO 产出执行时刻的一致快照且读写不互斥，serve 运行中可安全执行（D6 裁定）。
//
// db 路径三级解析（#18 裁定）：--db 显式 > --config 读 db.path > 无参时探测 CWD
// 的 aiteam-config.json（备份对象=正在服务的库，同源原则；序 9 部署结构拍平起
// serve 改 exe 目录基准，backup 属人跑的维护命令保持 CWD 语义不变——服务化部署
// 下配置在 exe 目录时无参 backup 探测不到，需 --config/--db 指路）；CWD 无配置
// 文件走内建默认 db/aiteam.db（相对值按维护命令惯例相对 CWD 解析）。探测命中自定义
// db.path 时在 OK 行以 source=<路径> 明示实际源库（隐式解析必须可观测，防备错库不知情）。
// 显式给的 --config 文件不存在直接报 ErrUsage，绝不静默回落默认——备错库比不备更糟。
//
// 退出码语义（§4.7，不新增退出码位）：flag 解析失败/缺 --out/显式 --config 文件不存在/
// --out 父目录不存在等「路径解析等本地参数错」→ ErrUsage（main 层退 2）；源库不存在
// （开库失败族）/目标已存在/VACUUM 执行失败（磁盘满等）/自校验不一致等运行期失败 →
// 普通 error（退 1）。
func RunBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	outFlag := fs.String("out", "", "备份产物文件路径（必填；已存在则拒绝覆盖——B6-T2）")
	dbFlag := fs.String("db", "", "源库文件路径（优先级高于 --config 与默认值）")
	configFlag := fs.String("config", "", "配置文件路径（取其 db.path 作源库路径）")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "用法: aiteam backup --out <路径> [--db <路径>] [--config <路径>]")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// -h/--help：flag 包已输出用法，按 CLI 惯例视作成功（退出码 0，同 serve）
			return nil
		}
		// flag 包已自行输出错误与用法（ContinueOnError），此处仅打标退出码语义
		return fmt.Errorf("%w: %v", ErrUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: 无法识别的位置参数 %q", ErrUsage, fs.Args())
	}
	if *outFlag == "" {
		return fmt.Errorf("%w: 缺少必填参数 --out", ErrUsage)
	}

	// --out 静态可判的参数问题先拒：父目录不存在时 VACUUM INTO 必败，提前给出
	// 明确中文报错优于 SQLite 底层英文报错（本地参数错→ErrUsage）。
	if dir := filepath.Dir(*outFlag); dir != "" {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return fmt.Errorf("%w: 备份目标父目录不存在或不是目录: %s", ErrUsage, dir)
		}
	}

	srcPath, cwdSourcePath, err := resolveBackupDBPath(*dbFlag, *configFlag)
	if err != nil {
		return err
	}

	// 源库存在性预检：store.Open 对不存在的路径会静默创建空库（迁移建全表），
	// 不预检会把「路径打错」静默变成「备份出一个空库假产物」——比失败更糟
	//（运行期失败→普通 error 退 1，属开库失败族）。
	if _, err := os.Stat(srcPath); err != nil {
		return fmt.Errorf("源库不存在: %s", srcPath)
	}

	// --out 等于源库路径的自我覆盖场景在此天然拦截：源库文件存在 ⇒ 目标「已存在」。
	if fi, err := os.Stat(*outFlag); err == nil {
		// B6-T2（裁定照推荐）：目标已存在→原样报错退出，不静默覆盖——静默覆盖备份
		// =数据事故，删旧重备由使用者显式操作（不加 --force，YAGNI）。预检与 VACUUM
		// INTO 执行的间隙若被第三方建出文件，SQLite 层对已存在目标自身报错兜底，
		// 构成双保险，任何路径都不会覆盖既有文件。
		// 目录与文件分开报：若 --out 是同名目录仍提示「删除该文件」，使用者照做
		// 可能误删整棵目录树（Windows rmdir /s 不可逆），文案必须与实际形态匹配。
		if fi.IsDir() {
			return fmt.Errorf("备份目标路径已存在同名目录，拒绝覆盖: %s（请另选 --out 路径）", *outFlag)
		}
		return fmt.Errorf("备份目标已存在，拒绝覆盖: %s（如需重备请先删除该文件）", *outFlag)
	}

	// 独立打开源库：store.Open 即「连接初始化 PRAGMA 同链」的实现（journal_mode(WAL)/
	// synchronous(NORMAL)/foreign_keys(1)/busy_timeout(5000)，§3.1 第 5 条），勿另写 DSN。
	// 对已迁移库 migrate 幂等跳过，无写入副作用。
	src, err := store.Open(srcPath)
	if err != nil {
		return fmt.Errorf("打开源库失败: %w", err)
	}
	defer src.Close()

	// 单条 VACUUM INTO：WAL 下产出执行时刻的一致快照（读写不互斥，serve 运行中
	// 安全——D6）。目标路径经参数绑定下发（实测 modernc.org/sqlite 支持 VACUUM INTO ?
	// 绑定），无拼接即无引号转义面，含空格/特殊字符路径天然兼容。
	if _, err := src.DB.Exec("VACUUM INTO ?", *outFlag); err != nil {
		return fmt.Errorf("执行 VACUUM INTO 备份失败（目标 %s）: %w", *outFlag, err)
	}

	// 产物自校验（NFR3）：打开备份文件比对 user_version+表清单+各表行数与源库一致。
	tables, err := validateBackup(src.DB, *outFlag)
	if err != nil {
		// 失败产物不残留：残留会让下一次重试撞上 B6-T2「目标已存在」拒覆盖，使用者
		// 得先手工定位删除；删除对象是本次刚生成且校验未通过的产物，非既有文件。
		_ = os.Remove(*outFlag)
		return fmt.Errorf("备份自校验失败（产物已删除）: %w", err)
	}

	// 成功输出（§D6 行为规格 + #18 裁定）：OK backup=<path> tables=<N>；CWD 探测
	// 命中且 db.path 非内建默认时追加 source=<db.path>——无参备份是隐式解析，使用者
	// 必须能看出备的是哪个库；默认值不提示（备的就是明面默认库，无误导面）。
	okLine := fmt.Sprintf("OK backup=%s tables=%d", *outFlag, tables)
	if cwdSourcePath != "" && cwdSourcePath != config.Default().DB.Path {
		okLine += " source=" + cwdSourcePath
	}
	fmt.Println(okLine)
	return nil
}

// resolveBackupDBPath 三级解析源库路径（§12 D6 + #18 CWD 探测裁定）：--db 显式 >
// --config 读 db.path > 无参时探测 CWD 的 aiteam-config.json（维护命令 CWD 语义，
// 序 9 起 serve 已改 exe 目录基准），兜底内建默认值（config.Default 单一来源，
// 勿写字面量副本）。
//
// 第二返回值 cwdSourcePath 仅在 CWD 探测命中时携带实际 db.path（其余来源为空串），
// 供 RunBackup 在 OK 行明示实际源库——探测是隐式行为，必须可观测（#18 裁定）。
func resolveBackupDBPath(dbFlag, configFlag string) (srcPath, cwdSourcePath string, err error) {
	if dbFlag != "" {
		return dbFlag, "", nil
	}
	if configFlag != "" {
		path, err := loadConfigDBPath(configFlag)
		return path, "", err
	}
	// 无参 CWD 探测（#18 裁定；维护命令保持 CWD 语义，serve 序 9 起已改 exe 目录
	// 基准）：aiteam-config.json 存在于 CWD 则加载（db.type 校验同显式 --config
	// 路径），不存在保持内建默认——零配置部署（未写配置文件）语义不变。探测=
	// 备份对象与正在服务的库同源（serve 配置文件同放 CWD 时）。
	if _, err := os.Stat(defaultConfigFile); err == nil {
		path, err := loadConfigDBPath(defaultConfigFile)
		return path, path, err
	}
	cfg, err := config.Load("")
	if err != nil {
		return "", "", err // 不可达（空路径恒返回内建默认），防御性保留
	}
	return cfg.DB.Path, "", nil
}

// loadConfigDBPath 从指定配置文件读取 db.path 作源库路径：文件不存在报 ErrUsage
// （显式参数指向的本地文件缺失=本地参数错，绝不静默回落默认）；内容非法（JSON 坏
// 等）与 serve 同口径归运行期错误（退 1）；db.type 一期仅支持 sqlite（字面量与
// config 包私有常量 dbTypeSQLite 同源，后者不导出，不为此扩公共面）。
func loadConfigDBPath(configPath string) (string, error) {
	if _, err := os.Stat(configPath); err != nil {
		return "", fmt.Errorf("%w: 配置文件不存在: %s", ErrUsage, configPath)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return "", fmt.Errorf("加载配置文件 %s 失败: %w", configPath, err)
	}
	if cfg.DB.Type != "sqlite" {
		return "", fmt.Errorf("backup 仅支持 db.type=sqlite，当前为 %q", cfg.DB.Type)
	}
	return cfg.DB.Path, nil
}

// validateBackup 产物自校验（NFR3）：独立打开备份文件（裸开只读，不触发迁移），
// 与源库比对——用户表清单一致+user_version 一致+各表行数一致，任一不符即失败。
// 返回用户表数量（供 OK 行 tables=N 输出）。
//
// 往返账如实总账（审查修正：b6-spec §七「固定 ≤8」只预估了 backup 语句本体，
// 未含产物比对必须的源/备两侧查询形态）：连接级 PRAGMA 4 条（经 store.Open 的
// DSN _pragma）+ migrate 读 user_version 1 条 + VACUUM INTO 1 条 + 本函数自校验
// 6 条（表清单×2+user_version×2+行数聚合×2），合计 12 条。全程固定、无循环、
// 不随表数/行数增长，base.md §4.9 纪律满足；spec 预估与实际的差额已报总控回写
// spec（以本注释为准）。
// 行数聚合成单条查询：逐表一条会随表数线性增长，违反批量化纪律。
// 校验窗口内源库被 serve 并发写入会保守报「行数不一致」失败——失败侧倾向符合
// 「不一致=失败」规格，重跑即可；VACUUM INTO 快照本身始终一致完整。
func validateBackup(src *sql.DB, backupPath string) (int, error) {
	bk, err := sql.Open("sqlite", backupPath)
	if err != nil {
		return 0, fmt.Errorf("打开备份文件失败: %w", err)
	}
	defer bk.Close()

	srcTables, err := listTables(src)
	if err != nil {
		return 0, fmt.Errorf("读取源库表清单失败: %w", err)
	}
	bkTables, err := listTables(bk)
	if err != nil {
		return 0, fmt.Errorf("读取备份文件表清单失败: %w", err)
	}
	if !slices.Equal(srcTables, bkTables) {
		return 0, fmt.Errorf("表清单不一致: 源=%v 备份=%v", srcTables, bkTables)
	}

	var srcVer, bkVer int
	if err := src.QueryRow("PRAGMA user_version").Scan(&srcVer); err != nil {
		return 0, fmt.Errorf("读取源库 user_version 失败: %w", err)
	}
	if err := bk.QueryRow("PRAGMA user_version").Scan(&bkVer); err != nil {
		return 0, fmt.Errorf("读取备份文件 user_version 失败: %w", err)
	}
	if srcVer != bkVer {
		return 0, fmt.Errorf("user_version 不一致: 源=%d 备份=%d", srcVer, bkVer)
	}

	if len(srcTables) > 0 {
		cols := make([]string, len(srcTables))
		for i, tbl := range srcTables {
			// 表名取自 sqlite_master（非用户输入），仅做标识符引号包裹防表名内
			// 特殊字符破坏语句，无注入面。
			cols[i] = fmt.Sprintf(`(SELECT COUNT(*) FROM "%s")`, strings.ReplaceAll(tbl, `"`, `""`))
		}
		query := "SELECT " + strings.Join(cols, ", ")
		srcCounts := make([]int, len(srcTables))
		bkCounts := make([]int, len(srcTables))
		if err := src.QueryRow(query).Scan(scanTargets(srcCounts)...); err != nil {
			return 0, fmt.Errorf("统计源库各表行数失败: %w", err)
		}
		if err := bk.QueryRow(query).Scan(scanTargets(bkCounts)...); err != nil {
			return 0, fmt.Errorf("统计备份各表行数失败: %w", err)
		}
		for i, tbl := range srcTables {
			if srcCounts[i] != bkCounts[i] {
				return 0, fmt.Errorf("表 %s 行数不一致: 源=%d 备份=%d", tbl, srcCounts[i], bkCounts[i])
			}
		}
	}
	return len(srcTables), nil
}

// listTables 返回用户表名清单（过滤 sqlite_% 内部对象），升序稳定便于比对。
func listTables(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Sort(tables)
	return tables, nil
}

// scanTargets 生成 Scan 用的 []any（指向 ints 各元素，供单行聚合查询逐列接收）。
func scanTargets(ints []int) []any {
	args := make([]any, len(ints))
	for i := range ints {
		args[i] = &ints[i]
	}
	return args
}
