package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"

	"aiteam/internal/client"
	"aiteam/internal/mirror"
	"aiteam/internal/store"
	"aiteam/internal/types"
)

// 本文件为五核心 send 命令实现（B2-6，技术设计 §4.2/§1.4/§8、b2-spec §二 CLI
// send 行）：目标三形态 → client POST #9 → 成功 201 后镜像 append（§8.3 先服务
// 端后镜像）→ 按结果定退出码（0/5；3/4 由 mapExitCode 既有分流；本地校验 2）。
// mirror 包为纯文件操作（Append/Entry/ManualLineError），本文件只消费其导出面。
// b5-W3 增量（FR9 跨项目定向）：--to-project flag（仅与 --to-role/--to-session
// 组合）→ target JSON 增 project 键；404 project_not_found 特判冻结文案；镜像行
// additive 第七字段 to_project。项目内路径行为逐字节不变（AC9.4）。

// sendMessageData 端点 #9 响应 data（§2.2 #9 恰四字段：seq/created_at/level/kind）
// ——--json 输出直接序列化本结构，与 #9 响应同构（§4.2）。
type sendMessageData struct {
	Seq       int64  `json:"seq"`
	CreatedAt string `json:"created_at"`
	Level     string `json:"level"`
	Kind      string `json:"kind"`
}

// sendLevels level 白名单（§4.2 --level 枚举）：直接引用 store 的 level 常量
// （DDL CHECK 枚举同源），与服务端 msgLevels 编译期同源可见，服务端加级改
// store 常量即自动同步：CLI 本地前置拦截省一次必拒往返；服务端白名单仍是
// 权威防线（绕过 CLI 的直连调用方）。
var sendLevels = []string{store.MessageLevelNormal, store.MessageLevelImportant, store.MessageLevelBlock}

// maxSendBodyBytes 正文上限 256KB（§2.2 #9「body(必填,≤256KB)」，与服务端
// maxMessageBodyBytes 同值同源口径，服务端调整须同步）：客户端预校验省 256KB+
// 的无谓上传；服务端 413 body_too_large 保留为权威防线。
const maxSendBodyBytes = 256 << 10

// RunSend `aiteam send` 一级动词入口（main.go 分发，§4.2 五核心）。
func RunSend(args []string) int {
	return runSend(context.Background(), args, "")
}

// runSend send 命令主链（serverOverride 供测试注入 httptest 地址，B1 命令同款）。
// 时序（§8.3 冻结原文）：本地校验 → POST #9 → 成功 201 → mirror.Append → 退 0；
// ManualLineError → 退 5+stderr 补行（消息已入库但镜像未写，AC13.1 判据=send 返回
// 0 ⇔ 镜像新增行，故镜像失败绝不能返回 0）；服务端拒绝 → 退 4 且镜像文件绝不
// 触碰（Append 未被调用即天然满足）；服务不可达 → 退 3。
func runSend(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam send",
		"用法: aiteam send (--to-role <角色> | --bus | --to-session <会话名>) "+
			"[--to-project <项目code>] --body <text|-> "+
			"[--level normal|important|block] [--no-mirror] [--json] [--mirror-root <路径>] "+identityUsage)
	var id identityFlags
	var toRole, toSession, toProject, level, bodyFlag, mirrorRoot string
	var bus, noMirror, asJSON bool
	registerCommonFlags(fs, &id)
	fs.StringVar(&toRole, "to-role", "", "定向：发到 --column 栏目的该角色（三选一）")
	fs.BoolVar(&bus, "bus", false, "总线模式：全栏目总控可见（三选一）")
	fs.StringVar(&toSession, "to-session", "", "会话对话：发到该会话的对话流（三选一）")
	fs.StringVar(&toProject, "to-project", "", "跨项目：发到该项目的目标会话/角色（仅与 --to-role/--to-session 组合）")
	fs.StringVar(&level, "level", "normal", "消息分级 normal|important|block")
	fs.StringVar(&bodyFlag, "body", "", "正文 ≤256KB；值为 - 或缺省且 stdin 非终端时自动读 stdin")
	fs.BoolVar(&noMirror, "no-mirror", false, "跳过镜像双写（仅故障演练/测试用；默认必须双写）")
	fs.BoolVar(&asJSON, "json", false, "机器可读 JSON 输出（与 #9 响应同构）")
	fs.StringVar(&mirrorRoot, "mirror-root", "", "镜像根目录覆盖（缺省=当前工作目录，B2-T1）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}

	// ① 本地校验全部前置（§4.1「CLI 本地校验缺漏=退出 2 不发请求」）：
	//    身份四参 → 目标三选一 → level 白名单 → body 读取与非空/上限。
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	target, err := resolveSendTarget(toRole, bus, toSession, toProject)
	if err != nil {
		return fail(err)
	}
	if !slices.Contains(sendLevels, level) {
		return fail(fmt.Errorf("%w: --level 须为 normal/important/block，得到 %q", ErrUsage, level))
	}
	body, err := readSendBody(bodyFlag)
	if err != nil {
		return fail(err)
	}

	// 镜像根解析前置（B2-T1 推荐口径：镜像根默认=当前工作目录，--mirror-root
	// 显式覆盖）。仅默认双写且未显式覆盖时才调 Getwd。之所以前移到本地校验
	// 段：若推迟到 POST 成功之后，Getwd 失败会落在「消息已入库但无 seq 可供
	// 补行」的半成功态，用户照 stderr 重试即产生服务端重复消息；前移后 POST
	// 成功路径不含任何可失败的环境探测，此处失败时消息尚未发送，修复工作
	// 目录后重试安全。
	root := mirrorRoot
	if !noMirror && root == "" {
		if root, err = os.Getwd(); err != nil {
			// Getwd 失败属运行环境异常（非参数错、非服务端错）→ 未知内部错误退 1。
			return fail(fmt.Errorf("定位镜像根目录失败（消息未发送，修复工作目录后重试）: %w", err))
		}
	}

	// ② POST #9：发送方身份取自身份四头（§2.2 #9，心跳中间件隐式注册+刷心跳），
	//    请求体=target 三形态+level+body。
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}
	payload := map[string]any{
		"target": target.requestBody(ident),
		"level":  level,
		"body":   body,
	}
	var resp sendMessageData
	if err := c.Do(ctx, http.MethodPost, "/api/v1/messages", payload, &resp); err != nil {
		// 跨项目 404 project_not_found 特判（b5-W3，§4.2 冻结文案）：该码有两个
		// 发射源——目标域（跨项目解析「项目不存在: <目标code>」）与身份域（心跳
		// 中间件「项目未登记: <发送方project>」，发送方自身项目未登记）。带
		// --to-project 时身份域坏项目同样命中该码，故按冻结文案前缀判别归因：
		// 身份域回落 fail(err) 透传（真实病因=自身项目拼错，不被「目标项目不
		// 存在」误报吞掉，AC9.2 指明真实缺失层级）；目标域照旧特判输出冻结文案
		//「目标项目不存在: X」。退出码仍为 4——APIError 既有域，零新增（§4.4）。
		var ae *client.APIError
		if target.project != "" && errors.As(err, &ae) && ae.Code == types.CodeProjectNotFound &&
			!strings.HasPrefix(ae.Message, "项目未登记") {
			fmt.Fprintf(os.Stderr, "ERROR: 目标项目不存在: %s\n", target.project)
			return 4
		}
		return fail(err) // UnreachableError→3 / APIError→4（mapExitCode 既有分流）
	}

	// ③ 镜像 append（§8.3 第 2 步；--no-mirror 显式跳过双写）。seq/created_at/
	//    level 以服务端 201 返回值为权威（AC5.4）；sender_label 与服务端四头拼法
	//    同构（session@column）；目标摘要按 §8.2 三形态示例。
	if !noMirror {
		// 服务端 201 但 data 残缺的防御：seq/created_at 是镜像行权威字段
		// （AC5.4），缺任一则不得静默写畸形镜像行后退 0。此时消息已入库而
		// Append 未执行，退 5 语义（已入库、镜像未写）成立。
		if resp.Seq <= 0 || resp.CreatedAt == "" {
			fmt.Fprintf(os.Stderr, "ERROR: 服务端响应缺权威字段（seq/created_at），消息已入库 seq=%d 但镜像未写\n", resp.Seq)
			return 5
		}
		// b5-W3（AC3）：跨项目行追加第七字段 to_project（mirror additive）。显式
		// 指定发送方自身项目（SP1=服务端按项目内路径处理、from_project 不落）时
		// 不落第七字段——项目内行保持六字段，与 from_project 语义同构。
		toProj := target.project
		if toProj == ident.Project {
			toProj = ""
		}
		err := mirror.Append(root, ident.Project, ident.Column, mirror.Entry{
			Seq:         resp.Seq,
			CreatedAt:   resp.CreatedAt,
			Level:       resp.Level,
			Target:      target.mirrorLabel(ident),
			SenderLabel: ident.Session + "@" + ident.Column,
			Body:        body,
			ToProject:   toProj,
		})
		if err != nil {
			// §8.3 第 4 步：stderr 两行=失败原因+完整可粘贴补行（不自动重试 send，
			// 会造成服务端重复消息；补行人工判断后粘贴）。§1.4 失败分支与 §8.3
			// 为同义冻结文案，取 §8.3 两行式（信息更全）。ManualLineError.Error()
			// 已整体含补行，此处 errors.As 结构化拆分以精确控制两行文案。
			var mle *mirror.ManualLineError
			if errors.As(err, &mle) {
				fmt.Fprintf(os.Stderr, "ERROR: 消息已入库 seq=%d 但镜像写入失败: %v\n手工补行: %s\n",
					resp.Seq, mle.Err, mle.Line)
				return 5
			}
			return fail(err) // Append 契约外错误类型兜底（退 1）
		}
	}

	// ④ 输出（§4.2）：人类可读 `OK seq=<n> time=<服务端时间>`；--json 与 #9 响应同构。
	if asJSON {
		b, err := json.Marshal(resp)
		if err != nil {
			return fail(fmt.Errorf("JSON 序列化失败: %w", err))
		}
		fmt.Fprintln(os.Stdout, string(b))
	} else {
		fmt.Fprintf(os.Stdout, "OK seq=%d time=%s\n", resp.Seq, resp.CreatedAt)
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// 目标三选一（§4.2：--to-role / --bus / --to-session 缺或多 → 本地退出 2 不发请求）
// + 跨项目 --to-project 组合校验（b5-W3/A6：仅与 --to-role/--to-session 组合合法）
// ─────────────────────────────────────────────────────────────────────────────

// sendTarget 目标三形态解析产物（kind 枚举=§2.2 #9 target.kind）。
type sendTarget struct {
	kind    string // direct / bus / chat
	role    string // direct：目标角色
	session string // chat：目标会话名
	project string // 跨项目：目标项目 code（--to-project，b5-W3）；空=项目内（空白串视同缺省 SP2）
}

// resolveSendTarget 目标三选一本地校验：三者恰选其一，缺选/多选均按用法错误退 2
// （§4.2「三选一」+ b2-plan「目标缺或多选→本地退出 2 不发请求」）。b5-W3 增量：
// toProject TrimSpace 后非空且与 --bus 同给 → 用法错误退 2 不发请求（A6「--bus
// 拒绝」——bus=发送方栏目广播，无目标项目域可锚）；校验点放在三选一计数之后，
// 使 --to-role+--bus 等既有组合仍先落既有互斥文案（行为不变，AC9.4）。
func resolveSendTarget(toRole string, bus bool, toSession, toProject string) (sendTarget, error) {
	role, sess, proj := strings.TrimSpace(toRole), strings.TrimSpace(toSession), strings.TrimSpace(toProject)
	chosen := 0
	for _, set := range []bool{role != "", bus, sess != ""} {
		if set {
			chosen++
		}
	}
	const choices = "--to-role <角色> / --bus / --to-session <会话名>"
	if chosen == 0 {
		return sendTarget{}, fmt.Errorf("%w: 缺少目标参数：%s 三者恰选其一", ErrUsage, choices)
	}
	if chosen > 1 {
		return sendTarget{}, fmt.Errorf("%w: 目标参数互斥：%s 至多一个（得到 %d 个）", ErrUsage, choices, chosen)
	}
	if proj != "" && bus {
		return sendTarget{}, fmt.Errorf("%w: --to-project 仅与 --to-role/--to-session 组合（--bus 是发送方栏目广播，不支持跨项目）", ErrUsage)
	}
	switch {
	case role != "":
		return sendTarget{kind: store.MessageKindDirect, role: role, project: proj}, nil
	case bus:
		return sendTarget{kind: store.MessageKindBus}, nil
	default:
		return sendTarget{kind: store.MessageKindChat, session: sess, project: proj}, nil
	}
}

// requestBody 组装 #9 请求 target（§2.2 #9 三形态原文）：direct 的 target.column
// 取发送方 --column（§4.2「--to-role：定向：发到 --column 栏目的该角色」——CLI
// flag 面无独立 --to-column，即同栏目定向）；bus 无目标参数；chat 带目标会话名。
// b5-W3：toProject 非空（跨项目）时增 project 键（§2.3；显式=发送方自身时服务端
// 按项目内路径处理 SP1，键无害透传）。
func (t sendTarget) requestBody(ident client.Identity) map[string]any {
	m := map[string]any{"kind": t.kind}
	if t.project != "" {
		m["project"] = t.project
	}
	switch t.kind {
	case store.MessageKindDirect:
		m["column"] = ident.Column
		m["role"] = t.role
	case store.MessageKindChat:
		m["session"] = t.session
	}
	return m
}

// mirrorLabel 镜像行目标摘要（§8.2 行格式第 4 字段三形态示例）：
// direct=proj-a/05/executor、bus=BUS、chat=session:<会话名>。b5-W3/SP3：direct
// 跨项目摘要=目标项目 code 前缀（真实落点语义——显式指定自身项目时前缀与
// ident.Project 同值，字符串形态不变）；bus/chat 摘要不带项目域（chat 定点到
// 会话名、bus 无目标域），维持既有形态。
func (t sendTarget) mirrorLabel(ident client.Identity) string {
	switch t.kind {
	case store.MessageKindDirect:
		proj := ident.Project
		if t.project != "" {
			proj = t.project
		}
		return proj + "/" + ident.Column + "/" + t.role
	case store.MessageKindBus:
		return "BUS"
	default:
		return "session:" + t.session
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 正文读取（§4.1 正文输入行：--body "..." 或 stdin 管道）
// ─────────────────────────────────────────────────────────────────────────────

// readSendBody 正文读取与非空/上限校验：--body <text> 直用；--body - 显式读
// stdin；缺省时 stdin 非 TTY 自动读（长文本管道友好）、TTY 则缺参退 2。
//
// 本地校验裁量（§4.7 vs §2.2 #9）：退出码 2 定义=「参数/用法错误（本地校验）」，
// body 缺失/空/超限均为客户端可完全判定的参数面错误，本地拦截省一次必拒网络
// 往返（空判定 TrimSpace 与服务端 body_empty 口径一致、上限按字节同 256KB）；
// 服务端 body_empty 400 / body_too_large 413 保留为权威防线。
func readSendBody(bodyFlag string) (string, error) {
	if bodyFlag == "" && stdinIsTerminal() {
		return "", fmt.Errorf("%w: 缺少 --body（正文必填；或 --body - 自 stdin 管道读取）", ErrUsage)
	}
	var body string
	if bodyFlag == "" || bodyFlag == "-" {
		// 多读 1 字节判超限（对齐 client.Do 响应侧同款防线）：LimitReader 截断
		// 与恰满上限在截断视角下不可区分，读到 max+1 即可触发下方上限判定，
		// 无需把超长管道输入完整拉进内存。
		b, err := io.ReadAll(io.LimitReader(os.Stdin, maxSendBodyBytes+1))
		if err != nil {
			return "", fmt.Errorf("读取 stdin 失败: %w", err)
		}
		body = string(b)
	} else {
		body = bodyFlag
	}
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("%w: 消息正文为空（--body 必填，≤256KB）", ErrUsage)
	}
	if len(body) > maxSendBodyBytes {
		// 不展示实际字节数：stdin 路径经 LimitReader 截断后 len(body) 至多
		// max+1，并非真实长度，展示反成误导。
		return "", fmt.Errorf("%w: 消息正文超过 256KB 上限", ErrUsage)
	}
	return body, nil
}

// stdinIsTerminal stdin 是否字符设备（终端）。Stat 失败保守按非终端处理——后续
// ReadAll 空读自然落「正文为空」退 2，不会挂死等待。
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
