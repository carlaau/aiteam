#!/usr/bin/env bash
# =============================================================================
# aiteam 验收脚本 v1b-mirror-check.sh —— V1b 写而不推=0 抽检（b7b-2，真二进制）。
#
# 判据（PRD §4 V1b 原文 + b7b-spec §一/§三，§8.2 行格式、AC13.1 复验）：
#   「抽检脚本：每条 send 返回 0 后立即读发送方镜像文件比对
#     PASS = send 成功 ⇔ 镜像新增对应行，无一缺失」
#   即：循环 send（--mirror-root 显式指临时仓，B2-T1 口径）→ 捕获输出 seq →
#   立即读镜像文件尾部行 → 六字段逐项比对（seq/created_at/level/目标摘要/
#   sender_label/body 单行化，行格式=§8.2 六字段 " | " 分隔）→
#   send 成功数=镜像新增行数守恒断言。
#
# 用法：
#   bash scripts/acceptance/v1b-mirror-check.sh            # 默认 10 条验收（退出 0=PASS）
#   bash scripts/acceptance/v1b-mirror-check.sh 5          # 自定条数
#   bash scripts/acceptance/v1b-mirror-check.sh selftest   # 断言器破坏演练（防假绿）：
#                                                          # 造假镜像行（字段错/删行）喂真断言器，
#                                                          # 断言器必须 FAIL——FAIL 不到场=断言器失效
#
# 夹具（独立 serve+独立临时库+独立临时镜像仓，不碰共享面）：
#   - ait_seed_domain seed 项目/栏目（引导死锁 workaround，见 common.sh）
#   - chat 目标会话与 direct 接收者格子各预注册 1 活会话（poll 心跳隐式注册；
#     chat 目标不存在=404 target_session_not_found，direct 格子多活会话=409
#     agent_conflict，均实测形态）
#   - 三目标形态 direct/bus/chat × 三级 level 轮转；body 覆盖普通文本/
#   含换行+制表符（单行化面）/含 " | "（镜像拆分器健壮性）
# =============================================================================
cd "$(dirname "$0")/../.." || exit 1
AIT_REPO_ROOT="$PWD" # ait_seed_domain 的 go -C 依赖解析根（module root）

# shellcheck source=common.sh
source scripts/acceptance/common.sh

# 脚本级临时目录；EXIT trap 先清自己再调 ait_cleanup（common.sh 头注约定）
V1B_TMP=""
v1b_cleanup() {
  [ -n "$V1B_TMP" ] && rm -rf "$V1B_TMP" 2>/dev/null
  ait_cleanup
}
trap 'v1b_cleanup' EXIT
trap 'exit 130' INT TERM # 信号转正常退出→触发 EXIT trap 统一清理

# 夹具常量
V1B_PROJECT="v1b-p"
V1B_COLUMN="01"
V1B_RECV_ROLE="executor_recv"  # direct 目标格子角色（恰 1 活会话）
V1B_CHAT_SESSION="v1b-chat-t"  # chat 目标会话名（预注册）
V1B_MIRROR_HEADER="# aiteam mirror (append-only, do not edit)" # §8.2 头注释（mirror.HeaderComment）

# ---- 断言器（主流程与 selftest 破坏演练共用同一实现——演练喂真断言器防假绿）----

# v1b_sanitize_body —— body 单行化期望值构造（与 internal/mirror SanitizeBody 同构）：
# \r\n 先归一 \n、孤立 \r 视同 \n，\n→字面 \n、\t→字面 \t（§8.2 一行=一条消息）。
v1b_sanitize_body() {
  local s="$1"
  s="${s//$'\r\n'/$'\n'}"
  s="${s//$'\r'/$'\n'}"
  s="${s//$'\n'/\\n}"
  s="${s//$'\t'/\\t}"
  printf '%s' "$s"
}

# v1b_split_line <镜像行> → 全局 V1B_F_SEQ/_TS/_LEVEL/_TARGET/_SENDER/_BODY
# 按前 5 个 " | " 定位切分，剩余整体归 body——body 原文可能含 " | "（§8.2
# 不做清洗，只单行化），按固定 6 段 awk -F 切会把 body 拆碎，故前缀定位。
v1b_split_line() {
  local rest="$1" i
  local -a f=()
  for i in 1 2 3 4 5; do
    f+=("${rest%% | *}")
    rest="${rest#* | }"
  done
  V1B_F_SEQ="${f[0]}"
  V1B_F_TS="${f[1]}"
  V1B_F_LEVEL="${f[2]}"
  V1B_F_TARGET="${f[3]}"
  V1B_F_SENDER="${f[4]}"
  V1B_F_BODY="$rest"
}

# v1b_assert_line <标签> <镜像行> <期望seq> <期望created_at> <期望level>
#                 <期望target> <期望sender> <期望body(已单行化)>
# 六字段逐项比对（§8.2），FAIL 明细列出首个不一致字段与两侧值。
v1b_assert_line() {
  local label="$1" line="$2"
  shift 2
  local w_seq="$1" w_ts="$2" w_level="$3" w_target="$4" w_sender="$5" w_body="$6"
  v1b_split_line "$line"
  local -a names=(seq created_at level target sender_label body)
  local -a wants=("$w_seq" "$w_ts" "$w_level" "$w_target" "$w_sender" "$w_body")
  local -a gots=("$V1B_F_SEQ" "$V1B_F_TS" "$V1B_F_LEVEL" "$V1B_F_TARGET" "$V1B_F_SENDER" "$V1B_F_BODY")
  local i
  for i in 0 1 2 3 4 5; do
    if [ "${gots[i]}" != "${wants[i]}" ]; then
      echo "  明细[$label]: 字段 ${names[i]} 不符——期望「${wants[i]}」实得「${gots[i]}」" >&2
      return 1
    fi
  done
  return 0
}

# v1b_assert_conserved <标签> <镜像文件> <期望消息行数> → 0=守恒
# 「send 成功数=镜像新增行数守恒」：总行数-头注释 1 行=消息行数；
# 首行非 §8.2 头注释=文件被外部写坏，直接 FAIL（守恒基准失真）。
v1b_assert_conserved() {
  local label="$1" file="$2" want="$3" head total
  if [ ! -s "$file" ]; then
    echo "  明细[$label]: 镜像文件不存在/为空: $file" >&2
    return 1
  fi
  head="$(head -n 1 "$file")"
  if [ "$head" != "$V1B_MIRROR_HEADER" ]; then
    echo "  明细[$label]: 首行非 §8.2 头注释——期望「$V1B_MIRROR_HEADER」实得「$head」" >&2
    return 1
  fi
  total="$(wc -l < "$file")"
  total=$((total - 1)) # 去头注释
  if [ "$total" -ne "$want" ]; then
    echo "  明细[$label]: 镜像行数=$total 期望=$want（缺 $((want - total)) 行=写而不推嫌疑）" >&2
    return 1
  fi
  return 0
}

# ---- send 输出解析（--json 与 #9 响应同构：seq/created_at/level/kind 恰四键）--
v1b_parse_json_field() { # <json> <键> → stdout 值
  sed -n 's/.*"'"$2"'":"\([^"]*\)".*/\1/p' <<<"$1" | head -n 1
}
v1b_parse_json_num() { # <json> <键> → stdout 数值
  grep -o '"'"$2"'":[0-9]\+' <<<"$1" | grep -o '[0-9]\+' | head -n 1
}

# ---- 断言器破坏演练（selftest 模式，防假绿）----------------------------------
v1b_destructive_selftest() {
  echo "==> 断言器破坏演练（坏夹具必须 FAIL / 好夹具必须 PASS）"
  local out=0 line file
  V1B_TMP="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-v1b-selftest.XXXXXX")" \
    || { echo "FAIL: mktemp -d 创建演练目录失败" >&2; return 1; }
  # 基准行与期望（好夹具）：seq=7、时间、level、target、sender、body（含 " | "）
  line='7 | 2026-10-02T13:04:05Z | important | v1b-p/01/executor_recv | s7@01 | pipe | inside'
  local w_seq=7 w_ts="2026-10-02T13:04:05Z" w_level="important" \
    w_target="v1b-p/01/executor_recv" w_sender="s7@01" w_body="pipe | inside"
  if v1b_assert_line "演练-好行" "$line" "$w_seq" "$w_ts" "$w_level" "$w_target" "$w_sender" "$w_body" \
    && [ "$(v1b_sanitize_body $'a\nb\tc')" = $'a\\nb\\tc' ]; then
    pass "演练-好行：六字段逐项比对放行+body 含 \" | \" 拆分器不误切+单行化口径正确"
  else
    fail "演练-好行：好夹具被断言器误杀（恒失败假红）或单行化口径漂移"
    out=1
  fi
  # 坏夹具六连：每字段各坏一处，断言器必须逐个抓住（只验首个不一致=目标字段，
  # 故每次只坏一个字段、其余保持期望值）
  local -a bad_lines=(
    '8 | 2026-10-02T13:04:05Z | important | v1b-p/01/executor_recv | s7@01 | pipe | inside' # seq 错
    '7 | 2026-10-02T13:04:06Z | important | v1b-p/01/executor_recv | s7@01 | pipe | inside' # created_at 错
    '7 | 2026-10-02T13:04:05Z | normal     | v1b-p/01/executor_recv | s7@01 | pipe | inside' # level 错
    '7 | 2026-10-02T13:04:05Z | important | BUS                       | s7@01 | pipe | inside' # target 错
    '7 | 2026-10-02T13:04:05Z | important | v1b-p/01/executor_recv | s9@01 | pipe | inside' # sender 错
    '7 | 2026-10-02T13:04:05Z | important | v1b-p/01/executor_recv | s7@01 | pipe X inside' # body 错
  )
  local i
  for i in "${!bad_lines[@]}"; do
    if v1b_assert_line "演练-坏行$((i + 1))" "${bad_lines[i]}" "$w_seq" "$w_ts" "$w_level" "$w_target" "$w_sender" "$w_body"; then
      fail "破坏演练：坏行$((i + 1))（第 $((i + 1)) 字段错）未被断言器抓住（假绿）"
      out=1
    else
      pass "破坏演练：坏行$((i + 1)) 被六字段比对 FAIL"
    fi
  done
  # 守恒破坏：构造 3 消息行镜像喂期望 4 → 必须抓；喂期望 3 → 必须放行
  file="$V1B_TMP/selftest-mirror.md"
  { printf '%s\n' "$V1B_MIRROR_HEADER"; printf '1 | t | normal | x | y | b\n'; printf '2 | t | normal | x | y | b\n'; printf '3 | t | normal | x | y | b\n'; } > "$file"
  if v1b_assert_conserved "演练-守恒坏" "$file" 4; then
    fail "破坏演练：删一行镜像（3 行喂期望 4）未被守恒断言抓住（假绿）"
    out=1
  else
    pass "破坏演练：删行镜像（3 行喂期望 4）被守恒断言 FAIL"
  fi
  if v1b_assert_conserved "演练-守恒好" "$file" 3; then
    pass "破坏演练：满行镜像（3 行喂期望 3）守恒放行（无恒失败假红）"
  else
    fail "破坏演练：守恒好夹具被误杀（恒失败假红）"
    out=1
  fi
  return $out
}

# ---- 主验收流程 --------------------------------------------------------------

v1b_run() {
  local count="${1:-10}"
  case "$count" in
    ''|*[!0-9]*) echo "FAIL: 条数须为正整数，得到 $count" >&2; return 2 ;;
  esac
  if [ "$count" -lt 1 ]; then
    echo "FAIL: 条数须 ≥1，得到 $count" >&2
    return 2
  fi
  echo "==> 起服务（真二进制 serve + 独立临时库）"
  start_server || { fail "start_server 起服务失败"; return 1; }
  local db
  db="$(ait_db_path_from_config "$AIT_TMPDIR/config.json")" || { fail "解析 db.path 失败"; return 1; }
  echo "==> seed 夹具域 $V1B_PROJECT/$V1B_COLUMN（引导死锁 workaround）"
  ait_seed_domain "$db" "$V1B_PROJECT" "$V1B_COLUMN" || { fail "seed 夹具域失败"; return 1; }

  V1B_TMP="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-v1b.XXXXXX")" \
    || { fail "mktemp -d 创建脚本临时目录失败"; return 1; }
  local mirror_root="$V1B_TMP/repo" # B2-T1 口径：--mirror-root 显式指临时仓
  mkdir -p "$mirror_root" || { fail "创建临时仓失败"; return 1; }

  # 预注册：chat 目标会话（不存在=404 target_session_not_found）+ direct 接收者格子
  echo "==> 预注册 chat 目标会话与 direct 接收者格子"
  if ai_cli "$AIT_URL" "$V1B_PROJECT" "$V1B_COLUMN" "$V1B_CHAT_SESSION" executor poll --limit 1 >/dev/null 2>&1 \
    && ai_cli "$AIT_URL" "$V1B_PROJECT" "$V1B_COLUMN" recv-pre "$V1B_RECV_ROLE" poll --limit 1 >/dev/null 2>&1; then
    pass "chat 目标会话（$V1B_CHAT_SESSION）与 direct 接收者格子（$V1B_RECV_ROLE）预注册"
  else
    fail "chat 目标会话/接收者格子预注册失败"
    return 1
  fi

  echo "==> 循环 $count 条：send→立即读镜像尾部行→六字段比对→行数守恒递进"
  local i rc out jseq jts jlevel jkind body w_target w_sender w_level mirror_file bad=0 sent=0
  for i in $(seq 1 "$count"); do
    # 目标三形态轮转（§8.2 目标摘要三形态全覆盖）+ 三级 level 轮转 + body 三变体
    case $((i % 3)) in
      0) body="v1b pipe | inside msg $i"; w_target="$V1B_PROJECT/$V1B_COLUMN/$V1B_RECV_ROLE"; jkind="direct" ;;
      1) body=$'v1b multi\nline\tmsg '"$i";     w_target="BUS";                                   jkind="bus" ;;
      2) body="v1b chat msg $i";                 w_target="session:$V1B_CHAT_SESSION";             jkind="chat" ;;
    esac
    # level 轮转取 (i/3)%3 与 kind 的 i%3 正交——10 条内 direct/bus/chat ×
    # normal/important/block 九种组合全覆盖（同余绑定会让形态与级别恒耦合）。
    case $(((i / 3) % 3)) in
      0) w_level="normal" ;;
      1) w_level="important" ;;
      2) w_level="block" ;;
    esac
    local sess="s$i"
    w_sender="$sess@$V1B_COLUMN"
    case $jkind in
      direct) set -- --to-role "$V1B_RECV_ROLE" ;;
      bus)    set -- --bus ;;
      chat)   set -- --to-session "$V1B_CHAT_SESSION" ;;
    esac
    # shellcheck disable=SC2086 —— 目标参数按词展开是本行语义
    out="$(ai_send "$AIT_URL" "$V1B_PROJECT" "$V1B_COLUMN" "$sess" executor "$@" \
      --level "$w_level" --body "$body" --json --mirror-root "$mirror_root")"
    rc=$?
    if [ "$rc" -ne 0 ]; then
      fail "第 $i 条：send 退出码=$rc（期望 0）输出：$out"
      bad=1
      continue
    fi
    sent=$((sent + 1))
    # PRD 判据「每条 send 返回 0 后立即读镜像」——先解析再读文件，同一迭代内完成
    jseq="$(v1b_parse_json_num "$out" seq)"
    jts="$(v1b_parse_json_field "$out" created_at)"
    jlevel="$(v1b_parse_json_field "$out" level)"
    if [ -z "$jseq" ] || [ -z "$jts" ] || [ -z "$jlevel" ]; then
      fail "第 $i 条：send --json 输出残缺（seq/created_at/level 解析空）：$out"
      bad=1
      continue
    fi
    pass "第 $i 条：send 退 0，seq=$jseq（$jkind/$jlevel）"
    mirror_file="$mirror_root/.aiteam/mirror-$V1B_PROJECT-$V1B_COLUMN.md"
    local tail_line
    tail_line="$(tail -n 1 "$mirror_file" 2>/dev/null)"
    if [ -z "$tail_line" ]; then
      fail "第 $i 条：镜像文件缺尾部行（未新增）: $mirror_file"
      bad=1
      continue
    fi
    if v1b_assert_line "第${i}条镜像行" "$tail_line" \
      "$jseq" "$jts" "$jlevel" "$w_target" "$w_sender" "$(v1b_sanitize_body "$body")"; then
      pass "第 $i 条：镜像尾部行六字段逐项相符"
    else
      fail "第 $i 条：镜像尾部行六字段比对失败（明细见上）"
      bad=1
    fi
    if v1b_assert_conserved "第${i}条守恒" "$mirror_file" "$sent"; then
      pass "第 $i 条：成功数=$sent 与镜像行数守恒"
    else
      fail "第 $i 条：镜像行数守恒断言失败（明细见上）"
      bad=1
    fi
  done

  echo "==> 收尾"
  stop_server
  if [ "$bad" -eq 0 ]; then
    pass "V1b 汇总：$count 条 send 全退 0、镜像行逐条六字段相符且全程守恒（写而不推=0）"
  else
    fail "V1b 汇总：存在失败条目（明细见上）"
  fi
  return $bad
}

# ---- 入口分流：selftest=断言器破坏演练；其余=真验收 --------------------------
if [ "${1:-}" = "selftest" ]; then
  v1b_destructive_selftest
  summary # FAIL>0 时自身 exit 1
  exit 0  # 全过显式退出（防落穿主验收流程）
fi

v1b_run "${1:-10}"
summary
