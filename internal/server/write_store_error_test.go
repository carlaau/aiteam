package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"aiteam/internal/store"
	"aiteam/internal/types"
)

// writeStoreError 哨兵覆盖清单测试（B2 开工前备忘任务）：防「store 新增哨兵
// 忘更新 writeStoreError 映射 → 静默落 default 分支变 500 internal_error」的
// 用户面回归（default 有 slog 日志但对外是笼统内部错误）。
//
// 清单性质：手工枚举（Go 无包级 var 反射枚举机制）——B2+ 新增 store 哨兵时
// 必须同步更新本清单：产于写路径的哨兵移入 mapped 清单并给出精确 HTTP 映射，
// 本测试即守门（忘映射 → 命中 default 500 → 红）。
//
// Server{} 零值直调即可：writeStoreError 只消费包级 slog/types，不触 st/cfg
// （勿为它开 newTestEnv 真文件库）。
func TestWriteStoreErrorMapsEveryWritePathSentinel(t *testing.T) {
	// ---- 清单一：在映射内的哨兵（产于写路径、会被 writeStoreError 消费）----
	// err 用 fmt.Errorf %w 包装形态=store 实际产出形态（projects.go/columns.go
	// 各方法均带上下文包装），顺带验证 errors.Is 解包链路。
	mapped := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"ErrProjectExists", fmt.Errorf("store: 插入项目 p-x 失败: %w", store.ErrProjectExists),
			http.StatusConflict, types.CodeProjectExists},
		{"ErrColumnExists", fmt.Errorf("store: 项目 p-x 栏目 c-x: %w", store.ErrColumnExists),
			http.StatusConflict, types.CodeColumnExists},
		{"ErrProjectNotFound", fmt.Errorf("store: 项目不存在: %w", store.ErrProjectNotFound),
			http.StatusNotFound, types.CodeProjectNotFound},
		{"ErrColumnNotFound", fmt.Errorf("store: 栏目不存在: %w", store.ErrColumnNotFound),
			http.StatusNotFound, types.CodeColumnNotFound},
		// B2-5：#12 回执校验链首步（GetMessageBySeq 未命中，404 message_not_found
		// ——存在性 404 优先于 not_block_level 409 语义）。
		{"ErrMessageNotFound", fmt.Errorf("store: 查询消息 seq=%d 失败: %w", int64(999), store.ErrMessageNotFound),
			http.StatusNotFound, types.CodeMessageNotFound},
		{"ErrProjectInvalid", fmt.Errorf("store: %w: code", store.ErrProjectInvalid),
			http.StatusBadRequest, types.CodeParamInvalid},
		{"ErrColumnInvalid", fmt.Errorf("store: %w: code", store.ErrColumnInvalid),
			http.StatusBadRequest, types.CodeParamInvalid},
		{"ErrNoFields", fmt.Errorf("store: %w（code=%q）", store.ErrNoFields, "p-x"),
			http.StatusBadRequest, types.CodeParamInvalid},
		{"ErrAuditInvalid", fmt.Errorf("store: %w", store.ErrAuditInvalid),
			http.StatusBadRequest, types.CodeParamInvalid},
		// B2-3：消息域写路径兜底哨兵（InsertMessage kind/level 出枚举或 body 空
		// ——handler 前置校验主责，此处映射兜底场景）。
		{"ErrMessageInvalid", fmt.Errorf("store: %w: level %q", store.ErrMessageInvalid, "urgent"),
			http.StatusBadRequest, types.CodeParamInvalid},
		// B2-4：poll/ack 链路兜底哨兵（ErrInvalidLimit 产于 PollVisible、
		// ErrInvalidSeq 产于 AdvancePositions——handler 前置拦截主责，此处映射
		// 数据面兜底；ErrInvalidOrder 产于 QueryHistory，#13 history B2-5 消费）。
		{"ErrInvalidLimit", fmt.Errorf("store: %w: -1", store.ErrInvalidLimit),
			http.StatusBadRequest, types.CodeParamInvalid},
		{"ErrInvalidSeq", fmt.Errorf("store: %w: -1", store.ErrInvalidSeq),
			http.StatusBadRequest, types.CodeParamInvalid},
		{"ErrInvalidOrder", fmt.Errorf("store: %w: %q", store.ErrInvalidOrder, "sideways"),
			http.StatusBadRequest, types.CodeParamInvalid},
	}

	s := &Server{}
	for _, tc := range mapped {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.writeStoreError(rr, tc.err)
			if rr.Code == http.StatusInternalServerError {
				t.Fatalf("HTTP %d internal_error——哨兵 %s 落 default 兜底（映射缺口：writeStoreError 未覆盖或分支被误删）",
					rr.Code, tc.name)
			}
			if rr.Code != tc.wantStatus {
				t.Fatalf("HTTP %d，期望 %d（%s 专属映射）", rr.Code, tc.wantStatus, tc.name)
			}
			var body types.ErrorResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("错误响应非合法 JSON: %v（body=%q）", err, rr.Body.String())
			}
			if body.Error.Code != tc.wantCode {
				t.Errorf("error.code = %q，期望 %q", body.Error.Code, tc.wantCode)
			}
			if body.Error.Message == "" {
				t.Error("error.message 为空——应透传 store 侧中文文本（AC2.2「错误信息指明」）")
			}
		})
	}

	// 清单完备性自检：若上方表漏登记新哨兵，本测试帮不上忙——防守义务在
	// 新增哨兵的提交者（见文件头注释）。此断言锚定清单规模，规模变化强制
	// 提交者抬头看一眼是否对应新增哨兵。
	if len(mapped) != 13 {
		t.Errorf("映射内哨兵清单 = %d 项，期望 13——数量变化须核对 store 包哨兵全集（B2 新增哨兵须同步补映射+补清单）", len(mapped))
	}

	// ---- 清单二：刻意不在映射内的哨兵（不产于写路径，勿误加进上方表）----
	// 断言落 default（500 兜底）= 把「刻意不映射」固化成机械闸：
	//
	//   ErrSessionInvalid（sessions.go UpsertSession 产出）：唯一消费点在心跳
	//     中间件 ③（middleware.go）——四头非空由 ① 保证、FK 由 ② 保证，理论
	//     不可达，中间件自己 500 兜底，不经 writeStoreError；
	//   ErrSessionNotFound（sessions.go GetSession/GetSessionByName 产出）：
	//     B2 消息域按会话语义给专属 404 错误口（handler_messages.go chat 分支
	//     target_session_not_found、B2-4 poll/ack 消费视角 session_not_found，
	//     handler 内 errors.Is 判定），不走通用映射。B2-5 receipt ⑤ 防御步新增
	//     反例：GetSession 命中本哨兵时流经 writeStoreError 落 default 500
	//     （消息存在而发送方会话缺失=数据完整性异常，理论不可达）——清单二
	//     语义是「无专属错误码映射」，并非「必须 errors.Is 分流」。
	//   ErrReceiptNotFound（receipts.go GetReceiptBySeq/insertReceipt 幂等回读
	//     产出）：B2 整批自审 R2 补登记。生产消费面=幂等回读失败（UNIQUE 冲突
	//     后回读仍无行=数据完整性异常，500 恰当）；GetReceiptBySeq 当前零生产
	//     消费——后续批次消费它且需专属错误口时，移入 mapped 清单并删本条目。
	//
	// 本断言防误加：若有人往 writeStoreError 添了这些哨兵的分支而未修订本
	// 清单，此处即红、强制对齐。反向亦然：B2+ 真让消息域错误流经
	// writeStoreError 时，把哨兵移入上方 mapped 清单并同步删本清单对应条目。
	// （刻意触发 default，会产生两行 slog ERROR 日志，属预期噪音。）
	unmapped := []struct {
		name string
		err  error
	}{
		{"ErrSessionInvalid", store.ErrSessionInvalid},
		{"ErrSessionNotFound", store.ErrSessionNotFound},
		// B2 整批自审 R2：B2-2 新增哨兵按文件头约定补登记（刻意不映射语义见上）。
		{"ErrReceiptNotFound", store.ErrReceiptNotFound},
	}
	for _, tc := range unmapped {
		rr := httptest.NewRecorder()
		s.writeStoreError(rr, tc.err)
		var body types.ErrorResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		if rr.Code != http.StatusInternalServerError || body.Error.Code != types.CodeInternalError {
			t.Errorf("刻意不映射哨兵 %s 命中了 HTTP %d %q——若是新增专属映射，须同步把哨兵移入 mapped 清单并修订清单二注释",
				tc.name, rr.Code, body.Error.Code)
		}
	}
}
