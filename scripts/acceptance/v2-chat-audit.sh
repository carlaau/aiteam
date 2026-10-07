#!/usr/bin/env bash
# =============================================================================
# aiteam 验收脚本 v2-chat-audit.sh —— V2 看板对话审计（b7b-4，真二进制）。
#
# 判据（PRD §4 V2 + b7b-plan b7b-4 任务书原文）：
#   「看板发起的对话消息可导出清单并人工归类审计；技术信号类=0=PASS」
#   职责切分（任务书钉死）：归类语义人工填表，脚本只导出+计数——
#     ① curl POST #24 构造看板对话（数条决策类示例，含 from 有/无两形态）
#     ② history --kind chat 导出 board 发起 chat 清单
#     ③ 生成/合并 v2-classifications.csv（列：seq,created_at,body,归类,备注）
#     ④ 汇总计数输出归类分布，断言技术信号类=0（PASS 主判据）
#
# 用法：
#   bash scripts/acceptance/v2-chat-audit.sh          # 真验收（退出 0=PASS）
#   bash scripts/acceptance/v2-chat-audit.sh selftest # 断言器破坏演练（防假绿）：
#                                                     # 技术信号类行/未知枚举行/
#                                                     # agent 混入 chat 行喂真
#                                                     # 断言器，必须 FAIL——
#                                                     # FAIL 不到场=断言器失效
#
# #24 端点契约（B5-1 已实现；2026-10-03 裁定 id 定位；本分支实测）：
#   POST /api/v1/board/messages  body={"session":<数字id>,"body":"...","from":"..."}
#   —— session 传数字 id 非 name（#24 恒无四头无项目上下文，name 跨栏目不唯一）；
#   恒免身份四头；响应 201 {"data":{"seq":N}}；服务端转 important 级 chat 落库
#   sender_label=board-user[:from]（from 缺省=board-user）。
#   会话 id 获取路径（实测后选可靠者）：GET /api/v1/sessions?project=<code>（#27，
#   B5-1 起可选豁免无四头放行）→ 响应 data.sessions[] 键序 id,project,column,name,
#   role,...（encoding/json 按 struct 字段序输出）→ 按 project+column+name 精确
#   串匹配提取目标会话数字 id。
#
# 「board-user 前缀过滤」的实测语义裁定（b7b-4 勘察结论）：
#   #13 响应 sender 三件 {session,role,column} 由 sender_session_id JOIN 会话装配
#   （handler_messages.go），board 消息 sender_session_id=0 → JOIN miss 三件空串，
#   CLI 文本渲染 `-`（poll.go senderLabel「系统/看板/回告语义行三件空串以 - 占位」
#   ——看板语义行即此形态）。sender_label 原文不入 #13 响应面，「board-user 前缀」
#   字面 grep 不可达。故导出过滤判据物化为两条合取（比前缀 grep 更强）：
#     a) chat 行 sender 三件恒空串（agent 会话名混入 chat 流=FAIL）；
#     b) 导出行 seq 全集与 #24 响应 seq 全集对拍一致（chat 全集恰为 board 发起）。
#
# csv 归类枚举：决策类 | 技术信号类 | 其他 | 待人工归类（实测导出行预填「待人工
# 归类」——归类语义人工填表；枚举外值计坏行=FAIL，防人工误填静默漏计）。
# 语料约束（脚本自造 body 全部满足）：无逗号/引号/换行——csv 零转义直拼。
#
# 夹具：独立 serve+独立临时库（不碰共享面）；ait_seed_domain seed 引导域
# （心跳存在性短路挡死 CLI 空库首登记，见 common.sh 头注）；目标会话 board-target
# 预注册恰 1 活会话（#24 target_session_id 定位目标）。
# =============================================================================
cd "$(dirname "$0")/../.." || exit 1
AIT_REPO_ROOT="$PWD" # ait_seed_domain 的 go -C 依赖解析根（module root）

# shellcheck source=common.sh
source scripts/acceptance/common.sh

# 脚本级临时目录（#24 响应体/导出缓存/合并 csv）；EXIT trap 先清自己再调 ait_cleanup
# （common.sh 头注约定：宿主自定义 trap 须在其清理逻辑中调用 ait_cleanup）。
V2_TMP=""
v2_cleanup() {
  [ -n "$V2_TMP" ] && rm -rf "$V2_TMP" 2>/dev/null
  ait_cleanup
}
trap 'v2_cleanup' EXIT
trap 'exit 130' INT TERM # 信号转正常退出→触发 EXIT trap 统一清理

# 夹具常量：项目/栏目/目标会话（board 对话目标，恰 1 活会话）/审计身份（history
# 拉取——GET 查询但四头心跳仍会 upsert 本会话行，与断言无涉）。role 守规约闸
# （2026-10-03 增补令：合法=controller / executor / executor_<标识>，实测 400
# invalid_role 反例=salesperson）。
V2_PROJECT="v2-p"
V2_COLUMN="01"
V2_TARGET_NAME="board-target"
V2_TARGET_ROLE="executor"
V2_AUDIT_SESSION="v2-audit"
V2_AUDIT_ROLE="controller"
V2_CHAT_N=4 # 决策类示例条数

# ---- 断言器（主流程与 selftest 破坏演练共用同一实现——演练喂真断言器防假绿）----

# v2_split_messages <history --json 输出> → stdout 每行一条 message JSON 片段。
# 先剥包裹键 {"messages":[ 与尾 ]}，再按数组元素间 `,{"seq"` 拆行（encoding/json
# 紧凑序列化；historyMessageData 键序 seq,kind,level,body,sender,receipt,
# created_at 由 struct 序保证稳定）。首条消息不拆行会随包裹前缀被 grep 丢弃
# （首跑实测丢 1 条，期望 4 导出 3——前缀剥离即修复）。
v2_split_messages() {
  sed 's/^{"messages":\[//; s/\]}$//; s/},{"seq"/}\n{"seq"/g' <<<"$1" | grep '^{"seq"'
}

# v2_assert_chat_rows <行文件> <期望条数> <期望seq串(空格分隔升序)> → 0=四判据全过：
#   行数相符 + seq 全集一致 + sender 三件空串（board 语义行，agent 混入=FAIL）+
#   level 恒 important（#24 恒 important 契约）。FAIL 打明细到 stderr。
v2_assert_chat_rows() {
  local rows="$1" want_n="$2" want_seqs="$3" n bad_from bad_lvl
  n="$(grep -c . "$rows")"
  if [ "$n" -ne "$want_n" ]; then
    echo "  明细[chat行数]: 导出=$n 期望=$want_n" >&2
    return 1
  fi
  # seq 全集对拍（board 发起 chat 清单语义闭合：导出 seq 全集=#24 响应 seq 全集）
  local got_seqs
  got_seqs="$(grep -o '"seq":[0-9]\+' "$rows" | grep -o '[0-9]\+' | sort -n | tr '\n' ' ')"
  if [ "${got_seqs% }" != "$want_seqs" ]; then
    echo "  明细[seq对拍]: 导出=[$got_seqs] 期望=[$want_seqs]" >&2
    return 1
  fi
  # sender 三件空串（board-user 前缀的 #13 面物化形态；非空=agent 会话名混入 chat 流）
  bad_from="$(grep -v '"sender":{"session":"","role":"","column":""}' "$rows")"
  if [ -n "$bad_from" ]; then
    echo "  明细[sender非空]: agent 会话混入 chat 流 → $bad_from" >&2
    return 1
  fi
  # level 恒 important（#24 契约）
  bad_lvl="$(grep -v '"level":"important"' "$rows")"
  if [ -n "$bad_lvl" ]; then
    echo "  明细[level]: 存在非 important chat 行 → $bad_lvl" >&2
    return 1
  fi
  return 0
}

# v2_count_classes <csv文件> → 计数入全局 V2_CNT_*（决策/技术信号/其他/待人工/坏行
# +V2_BAD_ROWS 明细）。表头行（第4列=「归类」）跳过；seq=0 行=示例预填行照计
# （示例基线参与分布统计——任务书语义：示例数据预填「决策类」入仓内 csv，
# 实测行写临时目录合并计数）。
v2_count_classes() {
  V2_CNT_DEC=0; V2_CNT_SIG=0; V2_CNT_OTH=0; V2_CNT_MAN=0; V2_CNT_BAD=0
  V2_BAD_ROWS=""
  local f1 f2 f3 cls rest
  # `|| [ -n "$cls" ]` 尾行兜底（b7b-4 审查 S1）：read 在末行无换行符时返回
  # 非零但变量已填充——不加兜底该行整行静默漏计（审计计数最忌讳）。空行
  # （read 返回非零且 cls 空）不误计。
  while IFS=, read -r f1 f2 f3 cls rest || [ -n "$cls" ]; do
    [ "$cls" = "归类" ] && continue # 表头
    case "$cls" in
      决策类) V2_CNT_DEC=$((V2_CNT_DEC + 1)) ;;
      技术信号类) V2_CNT_SIG=$((V2_CNT_SIG + 1)) ;;
      其他) V2_CNT_OTH=$((V2_CNT_OTH + 1)) ;;
      待人工归类) V2_CNT_MAN=$((V2_CNT_MAN + 1)) ;;
      *) V2_CNT_BAD=$((V2_CNT_BAD + 1)); V2_BAD_ROWS+="[$cls] " ;;
    esac
  done < "$1"
}

# v2_assert_distribution <csv文件> → 0=技术信号类=0 且无坏枚举行。
# V2 主判据（PRD §4：技术信号类=0=PASS）；坏枚举行=人工误填静默漏计的防线。
# 打印归类分布汇总（审计产物面）。
v2_assert_distribution() {
  local csv="$1"
  v2_count_classes "$csv"
  echo "==> v2 归类分布汇总（仓内示例预填 + 实测导出合并）：$csv"
  echo "    决策类: $V2_CNT_DEC"
  echo "    技术信号类: $V2_CNT_SIG"
  echo "    其他: $V2_CNT_OTH"
  echo "    待人工归类: $V2_CNT_MAN"
  if [ "$V2_CNT_BAD" -gt 0 ]; then
    echo "  明细[坏枚举行]: $V2_BAD_ROWS（枚举=决策类/技术信号类/其他/待人工归类）" >&2
    return 1
  fi
  if [ "$V2_CNT_SIG" -ne 0 ]; then
    echo "  明细[主判据]: 技术信号类=$V2_CNT_SIG ≠ 0" >&2
    return 1
  fi
  return 0
}

# ---- 断言器破坏演练（selftest 模式，防假绿）----------------------------------
# 造坏夹具直接喂上方真断言器：断言器必须返回失败，不失败=断言器失效（退出 1）。
# 反向各跑一条好夹具（断言器必须放行）防「恒失败假红」。
v2_destructive_selftest() {
  echo "==> 断言器破坏演练（坏夹具必须 FAIL / 好夹具必须 PASS）"
  local out=0 good bad unk rows_ok rows_bad
  good='0,2026-10-02T00:00:00Z,示例一,决策类,演练
1,2026-10-02T00:00:01Z,示例二,待人工归类,演练'
  # 1) 技术信号类夹具：1 行技术信号类 → 主判据必须抓（v2 塞一条技术信号类→FAIL）
  bad="0,2026-10-02T00:00:00Z,示例,决策类,演练
1,2026-10-02T00:00:01Z,报错堆栈贴进对话,技术信号类,演练"
  if v2_assert_distribution <(printf '%s\n' "$bad"); then
    fail "破坏演练：技术信号类夹具未被断言器抓住（假绿）"
    out=1
  else
    pass "破坏演练：技术信号类=1 被断言器 FAIL（主判据有效）"
  fi
  # 1b) 尾行无换行兜底（b7b-4 审查 S1 修复防假绿）：无尾换行的技术信号类行——
  #     read 若漏计末行则技术信号类=0 断言器放行（=兜底失效当场被抓）；
  #     正确计数后主判据必须 FAIL。
  local nonl
  nonl="$(printf '%s' "0,2026-10-02T00:00:00Z,无尾换行信号行,技术信号类,演练")"
  if v2_assert_distribution <(printf '%s' "$nonl"); then
    fail "破坏演练：无尾换行技术信号类行被漏计（S1 尾行兜底失效，假绿）"
    out=1
  else
    pass "破坏演练：无尾换行技术信号类行被正确计数并 FAIL（S1 尾行兜底有效）"
  fi
  # 1c) 反向健全：无尾换行的决策类行必须被正确计数（决策类=1）放行（防兜底误伤）。
  nonl="$(printf '%s' "0,2026-10-02T00:00:00Z,无尾换行决策行,决策类,演练")"
  if v2_assert_distribution <(printf '%s' "$nonl") && [ "$V2_CNT_DEC" -eq 1 ]; then
    pass "破坏演练：无尾换行决策类行被正确计数放行（决策类=1，无误伤）"
  else
    fail "破坏演练：无尾换行决策类行计数错误（漏计或兜底误伤）"
    out=1
  fi
  # 2) 未知枚举行：归类列填枚举外值 → 坏行防线必须抓
  unk="0,2026-10-02T00:00:00Z,示例,随便填的,演练"
  if v2_assert_distribution <(printf '%s\n' "$unk"); then
    fail "破坏演练：未知枚举行未被断言器抓住（假绿）"
    out=1
  else
    pass "破坏演练：未知枚举行被断言器 FAIL（坏行防线有效）"
  fi
  # 3) 好夹具反向健全：决策类+待人工归类混合必须放行（无恒失败假红）
  if v2_assert_distribution <(printf '%s\n' "$good"); then
    pass "破坏演练：好夹具（决策类+待人工）被断言器放行"
  else
    fail "破坏演练：好夹具被断言器误杀（恒失败假红）"
    out=1
  fi
  # 4) sender 混入：agent 会话名非空的 chat 行 → sender 断言必须抓
  rows_ok='{"seq":1,"kind":"chat","level":"important","body":"a","sender":{"session":"","role":"","column":""},"receipt":null,"created_at":"2026-10-02T00:00:00Z"}'
  rows_bad='{"seq":2,"kind":"chat","level":"important","body":"b","sender":{"session":"agent-x","role":"worker","column":"01"},"receipt":null,"created_at":"2026-10-02T00:00:01Z"}'
  printf '%s\n%s\n' "$rows_ok" "$rows_bad" > "$V2_TMP/selftest-rows.txt"
  if v2_assert_chat_rows "$V2_TMP/selftest-rows.txt" 2 "1 2"; then
    fail "破坏演练：agent 混入 chat 行未被断言器抓住（假绿）"
    out=1
  else
    pass "破坏演练：agent 混入 chat 行被断言器 FAIL（sender 防线有效）"
  fi
  # 5) 好行反向健全：全空 sender 两行必须放行
  printf '%s\n%s\n' "$rows_ok" "$rows_ok" > "$V2_TMP/selftest-rows-ok.txt"
  if v2_assert_chat_rows "$V2_TMP/selftest-rows-ok.txt" 2 "1 1"; then
    pass "破坏演练：好 chat 行被断言器放行"
  else
    fail "破坏演练：好 chat 行被断言器误杀（恒失败假红）"
    out=1
  fi
  return $out
}

# ---- 主验收流程 --------------------------------------------------------------

v2_run() {
  echo "==> 起服务（真二进制 serve + 独立临时库）"
  start_server || { fail "start_server 起服务失败"; return 1; }
  local db
  db="$(ait_db_path_from_config "$AIT_TMPDIR/config.json")" || { fail "解析 db.path 失败"; return 1; }
  echo "==> seed 夹具域 $V2_PROJECT/$V2_COLUMN（引导死锁 workaround）"
  ait_seed_domain "$db" "$V2_PROJECT" "$V2_COLUMN" || { fail "seed 夹具域失败"; return 1; }

  V2_TMP="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-v2.XXXXXX")" \
    || { fail "mktemp -d 创建脚本临时目录失败"; return 1; }

  echo "==> 预注册看板对话目标会话（$V2_TARGET_NAME/$V2_TARGET_ROLE，恰 1 活会话）"
  if ai_cli "$AIT_URL" "$V2_PROJECT" "$V2_COLUMN" "$V2_TARGET_NAME" "$V2_TARGET_ROLE" \
    poll --limit 1 >/dev/null 2>&1; then
    pass "目标会话预注册（poll 心跳隐式注册）"
  else
    fail "目标会话预注册失败"
    return 1
  fi

  echo "==> #27 取目标会话数字 id（2026-10-03 裁定：#24 按 id 定位禁按 name）"
  local sess_json target_id
  if ! sess_json="$(curl -fsS "$AIT_URL/api/v1/sessions?project=$V2_PROJECT")"; then
    fail "#27 sessions 列表拉取失败（豁免面裸调）"
    return 1
  fi
  # data.sessions[] 键序 id,project,column,name,role,...（encoding/json struct 序）
  target_id="$(grep -o "\"id\":[0-9]\+,\"project\":\"$V2_PROJECT\",\"column\":\"$V2_COLUMN\",\"name\":\"$V2_TARGET_NAME\"" \
    <<<"$sess_json" | grep -o '[0-9]\+' | head -1)"
  if [ -n "$target_id" ]; then
    pass "#27 裸调提取目标会话 id=$target_id（#27 可选豁免面顺带验证）"
  else
    local sess_head
    sess_head="$(printf '%s' "$sess_json" | head -c 400)"
    fail "#27 未提取到目标会话 id（响应头 400 字节：$sess_head）"
    return 1
  fi

  echo "==> curl POST #24 构造看板对话（$V2_CHAT_N 条决策类示例，from 有/无两形态）"
  local bodies=() froms=()
  bodies[0]="请把登录页主色改为深蓝色并在本周五前交付"; froms[0]="王经理"
  bodies[1]="验收以PRD第四章为准请先走完评审再合入";   froms[1]="李总监"
  bodies[2]="预算上限调整为十二万本月内锁定供应商";     froms[2]="王经理"
  bodies[3]="下周一十点开对齐会请提前准备进度材料";     froms[3]="" # from 缺省→board-user 形态
  local i code seq got_seqs="" body payload
  for i in $(seq 0 $((V2_CHAT_N - 1))); do
    body="${bodies[$i]}"
    if [ -n "${froms[$i]}" ]; then
      payload=$(printf '{"session":%s,"body":"%s","from":"%s"}' "$target_id" "$body" "${froms[$i]}")
    else
      payload=$(printf '{"session":%s,"body":"%s"}' "$target_id" "$body")
    fi
    code="$(curl -s -o "$V2_TMP/post-$i.json" -w '%{http_code}' -X POST \
      -H 'Content-Type: application/json' -d "$payload" "$AIT_URL/api/v1/board/messages")"
    if [ "$code" = "201" ]; then
      seq="$(grep -o '"seq":[0-9]\+' "$V2_TMP/post-$i.json" | grep -o '[0-9]\+')"
      if [ -n "$seq" ]; then
        got_seqs+="$seq "
        pass "#24 第$((i + 1))条 201 seq=$seq（from=${froms[$i]:-<缺省>}）"
      else
        fail "#24 第$((i + 1))条 201 但响应无 seq（$(head -c 200 "$V2_TMP/post-$i.json")）"
      fi
    else
      fail "#24 第$((i + 1))条 HTTP $code（期望 201；$(head -c 200 "$V2_TMP/post-$i.json")）"
    fi
  done
  local want_seqs
  want_seqs="$(printf '%s' "$got_seqs" | tr ' ' '\n' | grep -c .)"
  if [ "$want_seqs" -eq "$V2_CHAT_N" ]; then
    pass "#24 全部 $V2_CHAT_N 条构造成功（seq 全集：$(printf '%s' "$got_seqs" | sed 's/ $//')）"
  else
    fail "#24 构造条数不足：成功=$want_seqs 期望=$V2_CHAT_N"
  fi
  want_seqs="$(printf '%s' "$got_seqs" | tr ' ' '\n' | sort -n | tr '\n' ' ')"
  want_seqs="${want_seqs% }"

  echo "==> history --kind chat 导出 board 发起 chat 清单（--json 结构化面）"
  local json
  if ! json="$(ai_cli "$AIT_URL" "$V2_PROJECT" "$V2_COLUMN" "$V2_AUDIT_SESSION" "$V2_AUDIT_ROLE" \
    history --kind chat --json --order asc --limit 500)"; then
    fail "history --kind chat 拉取失败"
    return 1
  fi
  printf '%s' "$json" > "$V2_TMP/chat.json"
  v2_split_messages "$json" > "$V2_TMP/chat-rows.txt"
  if v2_assert_chat_rows "$V2_TMP/chat-rows.txt" "$V2_CHAT_N" "$want_seqs"; then
    pass "chat 导出四判据：行数=$V2_CHAT_N + seq 全集与 #24 对拍一致 + sender 三件空串（board 语义行）+ level 恒 important"
  else
    fail "chat 导出断言失败（明细见上）"
  fi

  echo "==> 生成实测导出 csv（归类语义人工填表——实测行预填「待人工归类」）"
  # 任务书语义：示例数据预填「决策类」入仓内 csv；脚本运行时导出实测行到临时
  # 目录，合并计数。实测行归类列=待人工归类（脚本不越权替人归类）。
  local seq_out created_at body_out
  : > "$V2_TMP/export.csv"
  while IFS= read -r row; do
    seq_out="$(grep -o '"seq":[0-9]\+' <<<"$row" | grep -o '[0-9]\+')"
    created_at="$(grep -o '"created_at":"[^"]*"' <<<"$row" | sed 's/"created_at":"//; s/"$//')"
    body_out="$(sed -n 's/.*"body":"\([^"]*\)".*/\1/p' <<<"$row")"
    printf '%s,%s,%s,待人工归类,b7b-4 实测导出（归类待人工填表）\n' \
      "$seq_out" "$created_at" "$body_out" >> "$V2_TMP/export.csv"
  done < "$V2_TMP/chat-rows.txt"
  if [ "$(grep -c . "$V2_TMP/export.csv")" -eq "$V2_CHAT_N" ]; then
    pass "实测导出 csv 落盘 $V2_TMP/export.csv（$V2_CHAT_N 行）"
  else
    fail "实测导出 csv 行数不符（$(grep -c . "$V2_TMP/export.csv") ≠ $V2_CHAT_N）"
  fi

  echo "==> 合并仓内示例 csv + 实测导出 csv → 计数归类分布"
  local repo_csv="scripts/acceptance/v2-classifications.csv" merged="$V2_TMP/merged.csv"
  if [ ! -f "$repo_csv" ]; then
    fail "仓内归类表缺失：$repo_csv"
    return 1
  fi
  cat "$repo_csv" "$V2_TMP/export.csv" > "$merged"
  if v2_assert_distribution "$merged"; then
    pass "V2 主判据：技术信号类=0（决策类 $V2_CNT_DEC + 其他 $V2_CNT_OTH + 待人工 $V2_CNT_MAN，零技术信号）"
  else
    fail "V2 主判据失败：技术信号类=$V2_CNT_SIG / 坏枚举=$V2_CNT_BAD（明细见上）"
  fi

  echo "==> 收尾"
  stop_server
  return 0
}

# ---- 入口分流：selftest=断言器破坏演练；其余=真验收 --------------------------
if [ "${1:-}" = "selftest" ]; then
  V2_TMP="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-v2-self.XXXXXX")" # 演练行文件落点
  v2_destructive_selftest
  summary # FAIL>0 时自身 exit 1
  exit 0  # 全过显式退出（防落穿主验收流程）
fi

v2_run
summary
