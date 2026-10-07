package mirror

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// HeaderComment 镜像文件首次创建时写入的头注释行（技术设计 §8.2）。
const HeaderComment = "# aiteam mirror (append-only, do not edit)"

// Entry 单条镜像行的字段载荷（§8.2 行格式六字段+b5-W3 additive 第七字段）。
// Seq/CreatedAt 以服务端返回值为权威（AC5.4），CLI 在收到 #9 响应后组装；
// Body 为正文原文，写入前仅做单行化，不截断、不做其他任何清洗。
type Entry struct {
	Seq         int64  // 服务端分配的全局递增序号
	CreatedAt   string // 服务端时间，ISO8601 UTC，如 2026-10-02T13:04:05Z
	Level       string // 消息级别：normal / important / block
	Target      string // 目标摘要：角色形态 proj-a/05/executor、总线 BUS、会话 session:<id>
	SenderLabel string // 发送方标签，如 controller-A@05
	Body        string // 正文原文
	ToProject   string // 跨项目目标项目 code（b5-W3 additive）：空=项目内六字段行；非空=行尾追加第七字段 to_project（既有行格式零破坏）
}

// ManualLineError 镜像写入失败错误（§8.3）：携带完整可粘贴的补行材料，
// CLI 层据此退出 5 并向 stderr 输出「手工补行: <Line>」（errors.As 取
// Line/Err 可结构化拆分，直接 err.Error() 亦可整体输出）。
type ManualLineError struct {
	Line string // 完整补行内容（不含结尾换行），可直接粘贴进镜像文件
	Err  error  // 底层文件操作错误
}

func (e *ManualLineError) Error() string {
	return fmt.Sprintf("镜像写入失败: %v；手工补行: %s", e.Err, e.Line)
}

func (e *ManualLineError) Unwrap() error { return e.Err }

// fileName 镜像文件名：mirror-<项目code>-<栏目code>.md（§8.1）。
// 项目 code 前缀防同一仓库服务多项目时跨项目串文件。
func fileName(projectCode, columnCode string) string {
	return "mirror-" + projectCode + "-" + columnCode + ".md"
}

// RenderLine 渲染一行镜像记录（不含结尾换行）：六字段以 " | "（空格+管道+空格）
// 分隔，body 经 SanitizeBody 单行化（§8.2）。纯函数无 IO，便于单测逐字段断言。
// b5-W3 additive：ToProject 非空（跨项目消息）时行尾追加第七字段 to_project——
// 项目内行（ToProject 空）保持六字段不追加，既有行格式零破坏（AC9.4）。
func RenderLine(e Entry) string {
	parts := []string{
		strconv.FormatInt(e.Seq, 10),
		e.CreatedAt,
		e.Level,
		e.Target,
		e.SenderLabel,
		SanitizeBody(e.Body),
	}
	if e.ToProject != "" {
		parts = append(parts, e.ToProject)
	}
	return strings.Join(parts, " | ")
}

// SanitizeBody body 单行化（§8.2）：换行替换为字面 \n、制表符替换为字面 \t，
// 保证「一行=一条消息」（diff/grep 友好）。CRLF 先归一为 LF、孤立 CR 视同换行，
// 避免行内残留回车破坏单行性。不截断、不做其他任何变换（审计完整性）。
func SanitizeBody(body string) string {
	s := strings.ReplaceAll(body, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}

// Append 向 <root>/.aiteam/mirror-<项目code>-<栏目code>.md 追加一行（§8.3
// 写入时序第 2 步）。root 为发送方 git 仓库根；.aiteam 目录缺失时自动创建；
// 文件首次创建（打开后 size=0）时随同写入头注释行。写入为
// O_APPEND|O_CREATE|O_WRONLY 打开后的单次 Write（§8.4 并发口径：低频场景
// 追加原子性足够，不引入文件锁）。
// 边界契约：本函数只碰文件系统——零网络依赖、不自动 commit、不碰 .gitignore
// （§8.1，服务端零参与 AC13.3）；Seq=0/空 Body 等字段不校验（纯文件操作，
// 组装正确性由 CLI 层负责）。任何失败返回 *ManualLineError，携带完整补行材料。
func Append(root, projectCode, columnCode string, e Entry) error {
	line := RenderLine(e)
	if err := appendFile(filepath.Join(root, ".aiteam", fileName(projectCode, columnCode)), line); err != nil {
		return &ManualLineError{Line: line, Err: err}
	}
	return nil
}

// appendFile 打开路径追加一行；文件 size=0（含新建）时头注释与消息行拼在
// 同一次 Write 写入，保持「单次 Write」口径（§8.4）。
func appendFile(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建镜像目录失败: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("打开镜像文件失败: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("查询镜像文件状态失败: %w", err)
	}
	data := line + "\n"
	if info.Size() == 0 {
		data = HeaderComment + "\n" + data
	}
	if _, err := f.Write([]byte(data)); err != nil {
		_ = f.Close()
		return fmt.Errorf("写入镜像失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭镜像文件失败: %w", err)
	}
	return nil
}
