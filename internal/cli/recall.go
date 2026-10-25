package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// 本文件为 recall 命令实现（b5-T3，b5-spec §二.R4，M-36 未消费窗口消息撤回）：
// POST /api/v1/messages/{seq}/recall（b5-T2 端点 35）。仅发送方本人、仅
// direct/chat 两 kind、仅未消费窗口可撤（bus/receipt/block 服务端 409 拒——
// R1 排除面，CLI 不做本地预判、服务端校验链为唯一权威，错误文案经 APIError
// 零改动透传）。成功输出 `OK recalled seq=<n>`（b5-spec R4 输出形状）；404
// message_not_found/409 家族（not_sender/kind_not_recallable/block_not_recallable/
// already_consumed）服务端文案透传（客户端 Do 非 2xx 统一 APIError，fail 退 4）。

// recallData 端点响应 data（b5-spec R3 恰两键）：幂等重撤同形 200（重复撤回=
// 幂等成功非报错，b5-spec 裁量②）。CLI 只消费成功事实，结构体保留字段面备
// 文档对拍（ack.go receiptData 同款）。
type recallData struct {
	Seq    int64  `json:"seq"`
	Status string `json:"status"`
}

// RunRecall `aiteam recall` 一级动词入口（main.go 分发，b5-spec R4）。
func RunRecall(args []string) int {
	return runRecall(context.Background(), args, "")
}

// runRecall recall 命令主链（serverOverride 供测试注入 httptest 地址，B1 命令
// 同款）。--seq 必填（正整数）；--reason 可选（TrimSpace 空白=未填，与端点
// 「缺体/空体/空串均视为未填」口径一致——未填走零体请求，服务端跳过体解码）。
func runRecall(ctx context.Context, args []string, serverOverride string) int {
	fs := newFlagSet("aiteam recall",
		"用法: aiteam recall --seq <n> [--reason <text>] "+identityUsage+
			"（未消费窗口内撤回本人发出的 direct/chat 消息；bus/receipt/block 不可撤）")
	var id identityFlags
	var seqFlag, reasonFlag string
	registerCommonFlags(fs, &id)
	fs.StringVar(&seqFlag, "seq", "", "要撤回的消息序号（正整数，必填）")
	fs.StringVar(&reasonFlag, "reason", "", "撤回原因（可选，审计留痕）")
	if proceed, exit := parseFlags(fs, args); !proceed {
		return exit
	}
	ident, err := validateIdentity(id)
	if err != nil {
		return fail(err)
	}
	// 本地校验（§4.1 退 2 不发请求）：--seq 必填正整数（parseNonNegFlag 后再加
	// 正数闸——recall 无「0=省略」语义，seq=0 服务端亦必 400 param_invalid，
	// 本地拦截退 2 是干净失败）。
	seq, err := parseNonNegFlag("--seq", seqFlag)
	if err != nil {
		return fail(err)
	}
	if seq <= 0 {
		return fail(fmt.Errorf("%w: 缺少必填参数 --seq <n>（要撤回的消息序号，正整数）", ErrUsage))
	}
	c, err := buildClient(id, ident, serverOverride)
	if err != nil {
		return fail(err)
	}

	// reason 可选体：未填（TrimSpace 空白）传 nil=零体请求（客户端 Do nil body
	// 不设 Content-Type，服务端 ContentLength=0 跳过体解码）；填了 TrimSpace 后
	// 作 reason（端点侧同样 TrimSpace 归一，两端同口径防漂移）。
	var payload any
	if reason := strings.TrimSpace(reasonFlag); reason != "" {
		payload = map[string]string{"reason": reason}
	}
	var out recallData
	if err := c.Do(ctx, http.MethodPost,
		"/api/v1/messages/"+strconv.FormatInt(seq, 10)+"/recall", payload, &out); err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stdout, "OK recalled seq=%d\n", out.Seq)
	return 0
}
