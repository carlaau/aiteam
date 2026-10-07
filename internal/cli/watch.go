package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"aiteam/internal/client"
	"aiteam/internal/store"
	"aiteam/internal/types"
)

// watch.go —— `aiteam watch` 哨兵值守（B3-6）：注册哨兵 → poll 空轮睡/命中输出
// → DELETE 注销退出。规格：技术设计 §2.2 #14/#15/#16、§7.3 哨兵活性、D2 裁定
// （watch 5s/失活 15s）、B3-T3 裁定（超时/信号先 DELETE 再退 6）；b3-plan B3-6。
//
// 结构裁定：循环体抽 runWatch(deps) 纯逻辑（poll/del/sleep/now 全注入——测试零
// 真实 sleep），命令壳 runWatchCmd 只做 flag 解析→身份校验→注册→装配依赖→收尾
// 退出码。服务端失活兜底（kill -9 死行）无需 CLI 处理（§7.3 服务端阈值清理）。

// watchHit poll 命中摘要（§2.2 #15 hits 元素；b7-W1 additive 扩列
// seq/level/kind/target_role 四键——tag 对齐服务端 handler_sentinels.go store.Hit
// 字段名，--json 保结构值直出，target 渲染归文本行见 watchHitTarget）。
type watchHit struct {
	Seq        int64  `json:"seq"`
	Level      string `json:"level"`
	Kind       string `json:"kind"`
	TargetRole string `json:"target_role"`
}

// watchDeps runWatch 循环依赖集：poll/del/sleep/now/runHook 全函数注入——纯逻辑
// 零真实 sleep、零墙钟、零真实子进程，测试注入桩记录调用并编排每轮行为。
type watchDeps struct {
	poll     func(ctx context.Context) ([]watchHit, error)                 // 一轮命中探测（注册后的 #15）
	del      func(ctx context.Context) error                               // 注销哨兵（#16）
	sleep    func(ctx context.Context, d time.Duration) error              // 空轮间隔；ctx 取消时返回非 nil
	now      func() time.Time                                              // 注入时钟（--max-wait 判定）
	interval time.Duration                                                 // 空轮睡间隔（生效值=服务端回显 interval_sec）
	maxWait  time.Duration                                                 // 总等待上限（0=无限等）
	asJSON   bool                                                          // --json：命中输出 JSON 摘要
	out      io.Writer                                                     // 命中输出面（生产=os.Stdout）
	onHit    string                                                        // b8-W1 --on-hit 命令行（空串=零钩子零开销）
	runHook  func(ctx context.Context, cmdLine string, env []string) error // b8-W1 钩子执行器（默认 watchHookRunner；测试桩零真实子进程）
	errOut   io.Writer                                                     // b8-W1 钩子提示/透传面（生产=os.Stderr）
}

// errWatchHookTimeout 钩子超时强杀标记错误（b8-W1）：watchHookRunner 的分类面——
// 超时 context 到点强杀后包装底层 exec 错误，runHitHook 经 errors.Is 识别后走
// 超时提示模板（区别于普通失败的提示文案）。文本=短英文标记（质量审查修 2：中文
// 语义归模板承载，防 sentinel 文本与模板逐字重复叠印）。
var errWatchHookTimeout = errors.New("hook timeout killed")

// runWatch 哨兵轮询循环纯逻辑（b3-spec B3-6 结构裁定）：
//   - hits 非空 → 逐 hit 输出 → [b8-W1] --on-hit 钩子（失败/超时只落 stderr 提示，
//     不改退出码；钩子窗内收 SIGINT 仍 return 0——钩子属尽力交付面；执行点=report
//     后 finish 前——HIT 行先出，AI 侧宿主唤醒不被钩子耽误）→ DELETE → 退 0
//     （正常值守命中路径；DELETE 走 finish 同一收尾——poll 响应已收与 SIGINT
//     cancel 之间存在竞态窗，hit 分支特判「ctx 完好则直接用原 ctx」救不了这个窗，
//     统一收尾即消除特例）；
//   - --max-wait 到时（注入时钟判定总等待 ≥max-wait）→ 收尾退 6；
//   - ctx 取消（SIGINT/SIGTERM 经命令壳 signal.Notify→cancel 注入）→ 收尾退 6；
//   - poll 出错且 ctx 已取消 → 收尾路径（取消期的 HTTP 错误是收尾信号的面影，
//     不是网络故障）；poll 出错且 ctx 完好 → fail 透传（不可达退 3 / 404
//     sentinel_not_found 等服务端拒绝退 4）。
//
// 收尾路径（B3-T3）：尽力 DELETE 后退 6（finish——独立 ctx，见其注）。
func runWatch(ctx context.Context, d watchDeps) int {
	start := d.now()
	for {
		if ctx.Err() != nil {
			d.finish(ctx)
			return 6
		}
		hits, err := d.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				d.finish(ctx)
				return 6
			}
			return fail(err)
		}
		if len(hits) > 0 {
			d.report(hits)
			d.runHitHook(ctx, hits)
			d.finish(ctx)
			return 0
		}
		if d.maxWait > 0 && d.now().Sub(start) >= d.maxWait {
			d.finish(ctx)
			return 6
		}
		if d.sleep(ctx, d.interval) != nil {
			d.finish(ctx)
			return 6
		}
	}
}

// watchHookTimeout 命中钩子执行上限（b8-W1：10s 超时强杀——推送类钩子允许完整
// 交付但不允许无限挂死 watch）。
const watchHookTimeout = 10 * time.Second

// watchHookTimeoutIOGrace 强杀后的 I/O 管道收尾宽限（WaitDelay）：windows 下孙进程
// 继承管道写端可在 cmd.exe 被杀后拖住 Wait 直至其自然退出——WaitDelay 到点强制关
// 管道返回，10s 强杀承诺不被拖穿（unix 侧 sh -c 单命令 exec 优化一般无此窗，兜底
// 同值无害）。
const watchHookTimeoutIOGrace = 500 * time.Millisecond

// watchHookEnvKeys 钩子注入键集合（b8-W1，质量审查修 1）：父环境底座过滤面。
var watchHookEnvKeys = map[string]bool{
	"AITEAM_HITS_JSON": true,
	"AITEAM_HIT_SEQ":   true,
	"AITEAM_HIT_LEVEL": true,
}

// environWithoutHookKeys 剔除 os.Environ 底座中的三个注入键：同名键一律让位注入值
// （unix execve 后 getenv 首匹配——不过滤则嵌套钩子/用户手 export 的陈旧 AITEAM_
// 键遮蔽本次命中数据，静默错报；语义=注入必生效）。就地过滤复用底座切片。
func environWithoutHookKeys() []string {
	base := os.Environ()
	kept := base[:0]
	for _, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok && watchHookEnvKeys[k] {
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}

// watchHookRunner runHook 默认实现（真子进程路径，b8-W1 spec §二.1）：cmdLine 经
// sh -c（unix）/cmd /c（windows）按 runtime.GOOS 执行；timeout 到点 CommandContext
// 强杀；钩子自身 stdout/stderr 透传到 errW（watch 的 stderr——stdout 管道消费者
// 零污染，b7 同款哲学）；env 追加注入当前环境之上（os.Environ 底座先剔除三个注入
// 键防同名遮蔽，env 空时 child 走缺省全量继承不触碰）。timeout 参数化：生产传
// watchHookTimeout，测试注入短值验强杀路径（AC2 不真等 10s）。
//
// 错误分类：超时强杀 → errWatchHookTimeout 可 Is 链（hctx 到点判定，与子进程自身
// 报错文本解耦）；其余 exec 错误原样透传（exit 非 0 / shell 未找到等）。
func watchHookRunner(errW io.Writer, timeout time.Duration) func(ctx context.Context, cmdLine string, env []string) error {
	return func(ctx context.Context, cmdLine string, env []string) error {
		// 交付类尽力面同 finish() 口径：剥离取消链——信号/收尾场景钩子仍完整交付
		// （推送可达优先），timeout 上限兜底挂死。
		hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(hctx, "cmd", "/c", cmdLine)
		} else {
			cmd = exec.CommandContext(hctx, "sh", "-c", cmdLine)
		}
		cmd.Stdout = errW
		cmd.Stderr = errW
		cmd.WaitDelay = watchHookTimeoutIOGrace
		if len(env) > 0 {
			cmd.Env = append(environWithoutHookKeys(), env...)
		}
		if err := cmd.Run(); err != nil {
			if errors.Is(hctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%w: %v", errWatchHookTimeout, err)
			}
			return err
		}
		return nil
	}
}

// watchHookEnv 命中钩子环境变量注入面（b8-W1）：AITEAM_HITS_JSON=全量命中 JSON
// 数组（watchHit 直接 marshal——b7 的 kind/target_role 键格式零变化）；
// AITEAM_HIT_SEQ / AITEAM_HIT_LEVEL=首条便捷键（本函数仅 hits 非空路径进入，
// [0] 取值安全）。marshal 失败分支实际不可达（字段全为 int/string），守恒编译器
// 边界——HITS_JSON 缺席但便捷键仍在。
func watchHookEnv(hits []watchHit) []string {
	env := []string{
		"AITEAM_HIT_SEQ=" + strconv.FormatInt(hits[0].Seq, 10),
		"AITEAM_HIT_LEVEL=" + hits[0].Level,
	}
	if b, err := json.Marshal(hits); err == nil {
		env = append(env, "AITEAM_HITS_JSON="+string(b))
	}
	return env
}

// runHitHook 命中钩子执行面（b8-W1）：onHit 空串=零钩子零开销（不组 env 不起
// 子进程，既有 v0.1 语义零变化）；失败/超时走 errOut 提示但不改 watch 退出码
// （推送属尽力面，值守命中语义零变化）。提示与钩子自身输出同落 errOut（生产=
// os.Stderr——stdout 管道消费者零污染，b7 同款哲学）。
func (d watchDeps) runHitHook(ctx context.Context, hits []watchHit) {
	if d.onHit == "" || d.runHook == nil {
		return
	}
	err := d.runHook(ctx, d.onHit, watchHookEnv(hits))
	if err == nil {
		return
	}
	w := d.errOut
	if w == nil {
		w = os.Stderr
	}
	if errors.Is(err, errWatchHookTimeout) {
		fmt.Fprintf(w, "命中钩子执行超时已强杀: %v\n", err)
		return
	}
	fmt.Fprintf(w, "命中钩子执行失败: %v\n", err)
}

// finish 收尾注销（B3-T3「尽力 DELETE 后退 6」的尽力二字所在）：DELETE 换独立
// ctx——信号/取消场景传入 ctx 已取消，http.NewRequestWithContext 下注销请求 100%
// 发不出去，收尾沦为零操作（审查修复 1，审查员实测已取消 ctx 的 DELETE 零到达）。
// context.WithoutCancel 剥离取消链（Go 1.21+），2s 超时兜底防服务端黑洞挂死收尾；
// ctx 完好的调用方（命中路径 / --max-wait 路径）也统一走此收尾（一致性，不为
// 「ctx 还活着」开特例——WithoutCancel 对完好 ctx 无副作用）。DELETE 失败不改变
// 退出码——注销属效果达成类尽力面，服务端失活阈值兜底死行。
func (d watchDeps) finish(ctx context.Context) {
	delCtx, cancelDel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancelDel()
	_ = d.del(delCtx)
}

// watchHitTarget b7-W3 target 渲染规则（json 侧保结构值 target_role，渲染归文本
// 行，b7-spec §二.4）：direct→目标角色值；chat/receipt→字面「会话对话流」；
// bus→字面「栏目广播仅总控」；其余（理论不可达——DDL CHECK 限四枚举）以 - 占位
// 保持行形状（senderLabel 占位惯例）。kind 分支用 store.MessageKind* 常量
// （T2 审查修 4：对齐 send.go/history.go 的 store 常量消费惯例——编译期拦拼写错）。
func watchHitTarget(h watchHit) string {
	switch h.Kind {
	case store.MessageKindDirect:
		return h.TargetRole
	case store.MessageKindChat, store.MessageKindReceipt:
		return "会话对话流"
	case store.MessageKindBus:
		return "栏目广播仅总控"
	default:
		return "-"
	}
}

// report 命中输出：默认逐 hit 一行 `HIT seq=<n> level=<l> kind=<k> target=<t>`
// （b7-W3 扩列——kind 标注+target 渲染规则见 watchHitTarget，实战「哨兵响但不知
// 为何响」的自解释面）；--json 输出单行 JSON 摘要 {"hits":[{seq,level,kind,
// target_role}]}（watchHit 自带 json tag 直接 marshal，不另立同构类型）。
func (d watchDeps) report(hits []watchHit) {
	if d.asJSON {
		b, err := json.Marshal(struct {
			Hits []watchHit `json:"hits"`
		}{Hits: hits})
		if err != nil {
			return // 字段全为 int/string 实际不可达；守恒编译器边界
		}
		fmt.Fprintln(d.out, string(b))
		return
	}
	for _, h := range hits {
		fmt.Fprintf(d.out, "HIT seq=%d level=%s kind=%s target=%s\n", h.Seq, h.Level, h.Kind, watchHitTarget(h))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 响应结构（对齐 §2.2 #14/#15 端点 data 字段面）
// ─────────────────────────────────────────────────────────────────────────────

// sentinelRegisterResp POST /api/v1/sentinels 响应 data（#14：interval_sec=生效值回显）。
// Holder 为 b8-W2① 幂等命中（200）持有者诊断——旧服务端无此键=nil（CLI 回落既有
// 提示行），additive 向后兼容。
type sentinelRegisterResp struct {
	SentinelID  int64                `json:"sentinel_id"`
	IntervalSec int64                `json:"interval_sec"`
	Holder      *watchSentinelHolder `json:"holder,omitempty"`
}

// watchSentinelHolder #14 幂等命中持有者诊断（b8-W2①）：session=持有者会话名、
// age_sec=服务端算好的 ping 年龄（CLI 从响应值渲染不自行取时钟——口径与看板同源）。
type watchSentinelHolder struct {
	Session    string `json:"session"`
	LastPingAt string `json:"last_ping_at"`
	AgeSec     int64  `json:"age_sec"`
}

// sentinelPollResp POST .../poll 响应 data（#15：hits 空=未命中继续睡）。
type sentinelPollResp struct {
	Hits []watchHit `json:"hits"`
	Now  string     `json:"now"`
}

// RunWatch `aiteam watch` 一级命令入口（main.go 挂载；无二级动词）。
func RunWatch(args []string) int {
	return runWatchCmd(context.Background(), args, "", nil)
}

// runWatchCmd watch 命令壳：flag 解析（身份四参+--interval/--max-wait/--on-hit/
// --json+公共连接面）→ 本地校验 → 注册哨兵 → 装配依赖 → runWatch。sigCh 非 nil 时
// 为测试注入的信号通道（nil=生产路径自建 signal.Notify，Windows 兼容走标准 API）。
func runWatchCmd(ctx context.Context, args []string, serverOverride string, sigCh <-chan os.Signal) int {
	fs := newFlagSet("aiteam watch",
		"用法: aiteam watch [--interval <时长>] [--max-wait <时长>] [--on-hit <命令行>] [--force] [--json] "+identityUsage)
	var id identityFlags
	var interval, maxWait time.Duration
	var asJSON bool
	var onHit string
	var force bool
	registerCommonFlags(fs, &id)
	fs.DurationVar(&interval, "interval", 5*time.Second, "轮询间隔时长（默认 5s，须满足 3×interval ≤ 服务端哨兵失活阈值）")
	fs.DurationVar(&maxWait, "max-wait", 0, "最长等待时长（0=无限等；到时先注销再退 6）")
	fs.BoolVar(&asJSON, "json", false, "命中时输出 JSON 摘要（缺省逐 hit 一行）")
	fs.StringVar(&onHit, "on-hit", "", "命中后执行的钩子命令行（经 sh -c/cmd /c 执行，10s 超时强杀；命中数据经 AITEAM_HITS_JSON/AITEAM_HIT_SEQ/AITEAM_HIT_LEVEL 环境变量透出；钩子输出并入 stderr，失败不影响退出码）")
	fs.BoolVar(&force, "force", false, "显式接管：同坐标已有活哨兵时注销旧行并以新 id 接手值守（用于确认残留孤儿/自己旧进程；无占位时等同普通注册）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}

	// 信号收尾：SIGINT/SIGTERM → cancel ctx（runWatch 经 ctx 取消进入收尾路径）。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if sigCh == nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(ch)
		sigCh = ch
	}
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	// 注册（#14，状态分叉见 registerSentinel）：201=新建 → 进轮询循环；200=幂等
	// 命中活哨兵（信箱 #12）→ 打印提示退 0，不进循环也不 DELETE——哨兵在别人手里
	// 值班，注销是破坏行为（b8-W2①：响应带 holder 时打印持有者诊断+--force 接管
	// 指引，人判依据）。--interval 显式提供时带 body interval_sec（向上取整到
	// 秒——服务端以整秒为粒度，亚秒间隔按 1s 建议；服务端 3×interval>失活阈值时
	// 400 param_invalid 透传退 4）；缺省不带 body 让服务端用默认 interval。
	// --force（b8-W2②）显式接管：body force:true，服务端同坐标旧删新插（新 id）；
	// 无占位时 force no-op（普通注册）。缺省不带 force 键（wire 契约：v0.1 服务端
	// 与旧客户端零感知）。
	intervalSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "interval" {
			intervalSet = true
		}
	})
	var regBody any
	if intervalSet || force {
		m := map[string]any{}
		if intervalSet {
			sec := (interval + time.Second - 1) / time.Second
			m["interval_sec"] = int64(sec)
		}
		if force {
			m["force"] = true
		}
		regBody = m
	}
	reg, created, err := registerSentinel(ctx, c, regBody)
	if err != nil {
		if ctx.Err() != nil {
			// 注册期收到收尾信号：中断的 HTTP 错误是信号面影非网络故障，按 B3-T3
			// 退出语义退 6（不报「服务不可达」误报）。sentinel id 未拿到、注销无从
			// 发起——服务端若已写行由失活阈值兜底（kill -9 同款死行路径）。
			return 6
		}
		return fail(err)
	}
	if !created {
		// b8-W2①：200 幂等命中——响应带 holder 且 age_sec 非负（新服务端）→ 持有者
		// 诊断行（谁在值班、ping 多新鲜）+ 接管指引；负差（服务端钟回拨）回落既有
		// 单行——「最后 ping -3s 前」误导观感，与 CLI status 命中后缀/看板徽标负差
		// 防御同姿态；无 holder（旧服务端）→ 既有单行回落。各形态均退 0 不进循环
		// 不 DELETE（age 取响应值，CLI 不取本地时钟）。
		if reg.Holder != nil && reg.Holder.AgeSec >= 0 {
			fmt.Fprintf(os.Stdout,
				"已有哨兵值班中（持有者=%s，最后 ping %ds 前）——若为残留孤儿可 --force 接管；若你是恢复/接手此会话（压缩续接/窗口重开），直接 --force——旧哨兵的输出通道已随旧宿主失联\n",
				reg.Holder.Session, reg.Holder.AgeSec)
		} else {
			fmt.Fprintln(os.Stdout, "已有哨兵值班中")
		}
		return 0
	}

	// poll 路径（#15）：query 三参传目标信箱坐标（column/role/session——
	// handler_sentinels.go q.Get 参数名照表），项目域走 X-Aiteam-Project 头。
	pollPath := fmt.Sprintf("/api/v1/sentinels/%d/poll?%s", reg.SentinelID, url.Values{
		"column":  {ident.Column},
		"role":    {ident.Role},
		"session": {ident.Session},
	}.Encode())
	delPath := fmt.Sprintf("/api/v1/sentinels/%d", reg.SentinelID)

	// 空轮睡间隔=服务端回显 interval_sec（生效值以服务端回显为准——缺省时为
	// 服务配置值，显式时为请求值；D2 默认 5s）。
	effective := time.Duration(reg.IntervalSec) * time.Second
	if effective <= 0 {
		effective = interval // 回显异常兜底：退回本地 flag 值，不零间隔打转
	}
	deps := watchDeps{
		poll: func(ctx context.Context) ([]watchHit, error) {
			var resp sentinelPollResp
			if err := c.Do(ctx, http.MethodPost, pollPath, nil, &resp); err != nil {
				return nil, err
			}
			return resp.Hits, nil
		},
		del: func(ctx context.Context) error {
			return c.Do(ctx, http.MethodDelete, delPath, nil, nil)
		},
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		now:      time.Now,
		interval: effective,
		maxWait:  maxWait,
		asJSON:   asJSON,
		out:      os.Stdout,
		errOut:   os.Stderr,
	}
	// --on-hit 缺省空串=零钩子零开销（不装执行器；runHitHook 空串短路同防线）。
	// errOut 单点决策（质量审查修 3）：runner 提示/透传面与 deps 提示面同源，
	// 改路由只动 errOut 一处。
	if onHit != "" {
		deps.onHit = onHit
		deps.runHook = watchHookRunner(deps.errOut, watchHookTimeout)
	}
	return runWatch(ctx, deps)
}

// watchRegRespLimit 注册响应读取上限（1MB，对齐服务端请求侧 §2.1 同量级防御；
// 注册响应极小，纯防御面）。
const watchRegRespLimit = 1 << 20

// registerSentinel #14 注册请求并区分幂等（信箱 #12：200=同坐标已有活哨兵，201=
// 新建）。client.Do 把 2xx 一律解码进 out、不回传状态码（client.go 判定面：仅
// <200 || >299 归 APIError），幂等分叉拿不到差异——经 Client 导出字段（HTTP/
// BaseURL/Token/Identity）自发这一个请求：头注入与 {data}/{error} 解析用 types
// 同源常量/包裹（与 client.applyHeaders/decodeAPIError 同型），错误分类构造导出的
// *client.UnreachableError / *client.APIError，fail()/mapExitCode 面不变。poll/
// DELETE 等其余请求仍走 c.Do。created=true 表示 201 新建；false 表示 200 幂等命中。
//
// TODO(B3-8): client 包导出 DoStatus 变体（回传状态码）或 Client.RegisterSentinel
// 业务方法后，registerSentinel 缩回普通调用——自发请求的头注入/包裹解析与 client
// 包同型 ~90%，禁长期双路径（形态同 handler_sentinels.go:26/35 既有标注）。
func registerSentinel(ctx context.Context, c *client.Client, body any) (sentinelRegisterResp, bool, error) {
	var out sentinelRegisterResp
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return out, false, fmt.Errorf("请求体序列化失败: %w", err)
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/sentinels", payload)
	if err != nil {
		return out, false, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Accept", types.ContentTypeJSON)
	if body != nil {
		req.Header.Set("Content-Type", types.ContentTypeJSON)
	}
	// 身份四头 + Bearer：与 client.applyHeaders 同源（空值字段跳过同口径）。
	if c.Identity.Project != "" {
		req.Header.Set(types.HeaderAiteamProject, c.Identity.Project)
	}
	if c.Identity.Column != "" {
		req.Header.Set(types.HeaderAiteamColumn, c.Identity.Column)
	}
	if c.Identity.Session != "" {
		req.Header.Set(types.HeaderAiteamSession, c.Identity.Session)
	}
	if c.Identity.Role != "" {
		req.Header.Set(types.HeaderAiteamRole, c.Identity.Role)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", client.BearerPrefix+c.Token)
	}

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return out, false, &client.UnreachableError{Err: err, ServerAddr: c.BaseURL}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, watchRegRespLimit+1))
	if err != nil {
		return out, false, &client.UnreachableError{Err: err, ServerAddr: c.BaseURL}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, false, decodeWatchRegError(resp.StatusCode, raw)
	}
	if len(raw) > watchRegRespLimit {
		return out, false, fmt.Errorf("响应体超过上限 %d 字节（server=%s）", watchRegRespLimit, c.BaseURL)
	}
	if err := json.Unmarshal(raw, &types.DataResponse{Data: &out}); err != nil {
		return out, false, fmt.Errorf("响应解析失败（HTTP %d）: %w", resp.StatusCode, err)
	}
	return out, resp.StatusCode == http.StatusCreated, nil
}

// decodeWatchRegError 注册请求非 2xx 响应 → *client.APIError：标准 {"error":{code,
// message}} 形状逐字段透传；非标准形状（反代 HTML 等）兜底保留状态码+原始片段。
func decodeWatchRegError(status int, raw []byte) error {
	var er types.ErrorResponse
	if err := json.Unmarshal(raw, &er); err == nil && er.Error.Code != "" {
		return &client.APIError{StatusCode: status, Code: er.Error.Code, Message: er.Error.Message}
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 200 {
		// 截断点回退到 rune 起始字节（client.decodeAPIError 同防线），
		// 防把多字节中文切碎成非法 UTF-8。
		n := 200
		for n > 0 && !utf8.RuneStart(msg[n]) {
			n--
		}
		msg = msg[:n] + "…"
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &client.APIError{StatusCode: status, Message: msg}
}
