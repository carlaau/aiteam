package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"aiteam/internal/config"
	"aiteam/internal/server"
	"aiteam/internal/store"
)

// ErrUsage 各子命令共享的用法错误哨兵（serve/init/backup…）：子命令层标记
// flag 解析/参数校验失败，main 层据此以退出码 2 退出（§4.7：参数/用法错误=2，
// 运行期故障=1）。
var ErrUsage = errors.New("参数/用法错误")

// shutdownGracePeriod 优雅停机收敛上限（§9.5）：到时即强制收敛，不是死等。
const shutdownGracePeriod = 10 * time.Second

// defaultConfigFile 默认配置文件名（§4.6）：serve 未显式 --config 时在 exe 所在
// 目录（部署根）探测/首跑生成（序 9 部署结构拍平：CWD 基准退役——服务化部署
// NSSM/sc/systemd 常见 cwd=System32，会把库文件写进系统目录）；backup 等 CLI
// 维护命令仍按 CWD 探测同款文件名（本批边界：维护命令 CWD 语义不动）。
const defaultConfigFile = "aiteam-config.json"

// exeDir 返回服务端 exe 所在目录（部署根锚点，序 9 部署结构拍平 W1）。包级变量
// 以便测试换桩：go test 下 os.Executable() 返回测试二进制的临时构建路径，直接
// 探测会落 go-build 临时目录——测试注入 t.TempDir 才能隔离断言（W4 可注入点）。
var exeDir = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

// RunServe 是 `aiteam serve [--config <path>]` 子命令入口：SIGINT/SIGTERM 经
// signal.NotifyContext 转成 ctx 取消（§9.5），与测试注入的 cancel 同构。
func RunServe(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, args, nil)
}

// serve 装配并启动服务主链：解析（config 三态+db 基准 exe 目录，序 9 部署结构
// 拍平 W1/W2）→Load→Validate→Open→NewServer→Listen→Serve，阻塞至 ctx 取消
// （优雅停机）或服务异常退出。
//
// ready 非 nil 时在监听就绪后被调用、参数为实际监听地址——listen :0 随机端口
// 场景下这是测试拿到真实地址的唯一同步点（Listen 后从 net.Listener.Addr() 取）。
func serve(ctx context.Context, args []string, ready func(addr string)) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "配置文件路径（缺省探测 exe 同目录 aiteam-config.json，无则首跑自动生成；生成失败用内建默认）")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "用法: aiteam serve [--config <路径>]")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// -h/--help：flag 包已输出用法，按 CLI 惯例视作成功（退出码 0）
			return nil
		}
		// flag 包已自行输出错误与用法（ContinueOnError），此处仅打标退出码语义
		return fmt.Errorf("%w: %v", ErrUsage, err)
	}
	// 位置参数拒绝：`aiteam serve myconfig.json` 忘打 --config 时曾被静默忽略、
	// 按默认配置启动（审查 Important 1）——多余位置参数一律报用法错误（§4.7 退 2）。
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: 无法识别的位置参数 %q", ErrUsage, fs.Args())
	}

	// 解析基准（序 9 部署结构拍平 W1）：exe 所在目录=部署根。服务化部署（NSSM/sc/
	// systemd 常见 cwd=System32）曾把库文件写进系统目录——serve 链自此以 exe 目录为
	// 唯一基准：cwd 彻底出局（零 os.Getwd、零 CWD 相对解析、无兜底回落，防双语义
	// 并存）。exe 目录解析失败属理论边缘：部署根退化为空串，db 相对路径将按 CWD
	// 解析——部署语义退化，须在 Warn 日志明示（回落内建默认继续，不阻启动）。
	deployRoot, err := exeDir()
	if err != nil {
		slog.Warn("exe 目录解析失败，config/db 相对路径将按 CWD 解析（部署语义退化），回落内建默认继续启动", "err", err)
	}

	// 配置解析链（W1/W2）：显式 --config > exe 目录 aiteam-config.json > 内建默认。
	cfgPath := *configPath
	switch {
	case cfgPath != "":
		// 显式指定：不做探测、不生成——文件不存在/不可访问直接拒启（显式路径拼错
		// 时静默回落默认=配置改动静默失效，比拒启糟；Load 对缺失路径的静默默认
		// 语义不动， serve 层前置 Stat 把关）。
		if _, serr := os.Stat(cfgPath); serr != nil {
			return fmt.Errorf("%w: 配置文件不存在: %s（%v）", ErrUsage, cfgPath, serr)
		}
	case deployRoot != "":
		// 未显式：探测 exe 目录（部署根）下的默认配置文件。
		candidate := filepath.Join(deployRoot, defaultConfigFile)
		if _, serr := os.Stat(candidate); serr == nil {
			cfgPath = candidate // 已存在：加载，不覆盖（幂等）
		} else if werr := os.WriteFile(candidate, []byte(configExampleTemplate), 0o644); werr == nil {
			// 首跑自生成（W2）：与 config init 同一模板（单一来源），拷贝即跑
			slog.Info("已生成默认配置（_doc 说明可删，改后重启生效）", "config", candidate)
			cfgPath = candidate
		} else {
			// 生成失败（如只读目录）：告警后回落内建默认，不阻启动
			slog.Warn("默认配置生成失败，回落内建默认继续启动", "config", candidate, "err", werr)
		}
	}

	// 装配链 §4.6：加载配置→校验拒启
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("配置校验失败: %w", err)
	}

	// db 路径解析（W1）：绝对路径照用；相对值相对 exe 目录（部署根）解析——默认值
	// db/aiteam.db 落 exe 目录 db/ 子目录，不再相对 CWD；父目录自动创建（失败拒启）。
	dbPath := cfg.DB.Path
	if !filepath.IsAbs(dbPath) {
		dbPath = filepath.Join(deployRoot, dbPath)
	}
	dbDir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return fmt.Errorf("创建数据库目录 %s 失败: %w", dbDir, err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}

	handler := server.NewServer(st, cfg, server.Version)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		_ = st.Close()
		return fmt.Errorf("监听 %s 失败: %w", cfg.Listen, err)
	}

	srv := &http.Server{Handler: handler}
	if ready != nil {
		ready(ln.Addr().String())
	}
	// 启动横幅：db/config 一律打印已解析的绝对路径——部署根语义下落点一眼可辨
	// （库文件写进系统目录事故的防线之二：看日志即知落点异常）；解析失败回落原值
	// 不阻塞启动。生成失败回落内建默认时 config 显示 <内建默认>。
	dbAbs := dbPath
	if abs, err := filepath.Abs(dbAbs); err == nil {
		dbAbs = abs
	}
	cfgSrc := cfgPath
	if cfgSrc == "" {
		cfgSrc = "<内建默认>"
	} else if abs, err := filepath.Abs(cfgSrc); err == nil {
		cfgSrc = abs
	}
	slog.Info("aiteam 服务已启动", "listen", ln.Addr().String(), "db", dbAbs, "config", cfgSrc, "version", server.Version)

	serveErr := make(chan error, 1) // 带缓冲：停机路径不读它，Serve goroutine 不泄漏
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr: // Serve 先于停机信号退出（如监听被系统回收）
		_ = st.Close()
		// 防御分支（审查 Minor 3）：当前结构下 ErrServerClosed 只在 Shutdown 后
		// 由 Serve 返回，走 ctx.Done 分支收尾不会到此；保留以对冲未来重构
		// 改变收尾顺序。
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("服务异常退出: %w", err)
	case <-ctx.Done():
		return shutdown(srv, st)
	}
}

// shutdown 优雅停机（§9.5）：Shutdown 等 in-flight 请求收敛、上限 10s；
// 超时也继续关库退出——10s 是收敛上限不是死等。
func shutdown(srv *http.Server, st *store.Store) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = st.Close()
		slog.Warn("aiteam 服务停机超时/失败，已强制收敛", "err", err)
		return fmt.Errorf("优雅停机未在 %s 内收敛: %w", shutdownGracePeriod, err)
	}
	if err := st.Close(); err != nil {
		return fmt.Errorf("关闭数据库失败: %w", err)
	}
	slog.Info("aiteam 服务已优雅停机")
	return nil
}
