#!/usr/bin/env bash
# =============================================================================
# aiteam 验收脚本 v1c-sentinel-kill.sh —— V1c 哨兵 kill -9 失活恢复（b7b-3，
# 真二进制自跑，kill 注入真触发不 mock）。
#
# 判据（PRD §4 V1c PASS 判据语义：哨兵进程 kill -9 非正常终止 → 失活阈值后
# status 可见 sentinel=dead/失联标记 → 消息零丢失）：
#   [1] 前置链：起 serve + seed 夹具域（登记）+ 两信箱位点先行落行（poll 空拉——
#       poll 心跳隐式注册会话 v1a 实证 + GetPositions 惰性落位点 COALESCE(MAX(seq),0)
#       =0，positions.go:25；位点先于消息落行是消费者 poll 可回看全部消息的前提）
#       + kill 前预发 N_PRE=3 条 direct 给消费者信箱 executor_2（level 三级轮换）
#   [2] aiteam watch 后台起（executor 信箱哨兵，真二进制非 go run）→ status
#       --global 该信箱 sentinel=alive（中间断言=alive→dead 观测时序前半+哨兵
#       注册成功直接证据；轮询 8s 容忍进程冷启动）
#   [3] kill -9 $!（真 kill 注入非 mock；MSYS pid→runtime 映射原生进程
#       TerminateProcess 强杀有效，AC16.2 实证同款未用 taskkill）→ 进程消失 +
#       watch 输出零 HIT（值守全程空转未被打退——executor 值守箱 kill 前零消息）
#   [4] sleep 20（失活阈值 15s+5s 余量，D2 裁定 config Watch.SentinelTimeoutSec
#       默认 15；活性谓词 now-last_ping_at>15 严格大于恰等=alive，§7.3）→
#       status --global 该信箱 sentinel=dead*（dead 渲染追加星号失联标记，
#       status.go statusSentinelCell §4.2）→ 断言器自证演练 a：dead 语境喂
#       alive 期望，同一断言器必须 FAIL（区分力防恒真假绿）
#   [5] 断言器自证演练 b（复活对照三态闭环，此刻值守箱仍零消息——twin 空转
#       前提，故先于 [6] 补发执行）：对照 watch 同坐标注册 → 死行 upsert 复活
#       （B3-T2 kill 重启即复活同 id）→ status 断言回 sentinel=alive（证 [4] 的
#       dead* 非恒 dead 假判定）→ --max-wait 到时先 DELETE 再退 6（退出码断言）
#       → sentinel=none（alive/dead/none 三态全观测闭环）
#   [6] kill 后补发（消费者箱 N_POST=2 条+值守箱 1 条）→ poll --limit 0 全量
#       拉取断言消息零丢失（含 kill 前后新发）：消费者箱 kill 前预发+kill 后补发
#       seq 全在（主判据——消息在服务端库 watch 只是消费者，kill 不丢数据）+
#       值守箱 poll 拉到 kill 后补发（哨兵死亡不影响投递）+ history 全表对照
#       （COUNT=6 且 6 seq 全含）
#
# 时序设计说明（为什么 kill 前预发不走 watch 值守信箱）：
#   watch 命中即退是正常值守语义（watch.go runWatch：hits 非空 → DELETE → 退 0）；
#   哨兵探测位点缺行 COALESCE 0=全量可见（「新哨兵首探不漏位点行缺失前的历史」
#   防漏报设计，store/sentinels.go pollHitsSQL 注）。若 kill 前给 executor 值守箱
#   发消息，watch 首探必命中 ≤5s 内 HIT 退出，kill 注入落空。故 kill 前预发走
#   executor_2 消费者信箱（direct 探测分支 target_role=executor 不含 executor_2，
#   不在 watch 视野）；值守箱 kill 前保持零消息保注入有效，kill 后（演练 b 的
#   twin 已注销后）再补发验证「哨兵死了消息照常入箱」。
#
# 用法：bash scripts/acceptance/v1c-sentinel-kill.sh（双平台：Git Bash / Linux bash）
# 依赖：common.sh 脚手（起停/seed/CLI 封装/断言计数/EXIT 清理全复用）
# =============================================================================
cd "$(dirname "$0")/../.." || exit 1
AIT_REPO_ROOT="$PWD" # ait_seed_domain 的 go -C 依赖解析根（module root）

# shellcheck source=common.sh
source scripts/acceptance/common.sh

# 脚本级临时目录 + watch 进程追杀表（主 watch 被 kill -9 后无残留，twin 自然退出；
# 追杀兜底中途 fail 退出的无限等 watch 进程）；EXIT trap 先清自己再调 ait_cleanup
# （common.sh 头注约定：宿主自定义 trap 须在其清理逻辑中调用 ait_cleanup）。
V1C_TMP=""
V1C_WATCH_PIDS=()
v1c_cleanup() {
  local p
  for p in "${V1C_WATCH_PIDS[@]:-}"; do
    [ -n "$p" ] && kill -9 "$p" 2>/dev/null
  done
  [ -n "$V1C_TMP" ] && rm -rf "$V1C_TMP" 2>/dev/null
  ait_cleanup
}
trap 'v1c_cleanup' EXIT
trap 'exit 130' INT TERM # 信号转正常退出→触发 EXIT trap 统一清理

# ---- 用例身份（全新库自建，无预置依赖）--------------------------------------
PROJ="v1c-proj"
COL="a1"
WATCH_SESSION="v1c-watch"   # watch 哨兵会话（executor 信箱值守者，恰 1 活会话）
WATCH_ROLE="executor"
CONS_SESSION="v1c-consumer" # 消费者会话（executor_2 信箱唯一活会话，409 防线）
CONS_ROLE="executor_2"
SEND_SESSION="v1c-sender"   # 发送方/观察者会话（send 心跳注册，status 断言复用）
SEND_ROLE="controller"
N_PRE=3                     # kill 前预发条数（normal/important/block 三级轮换）
N_POST=2                    # kill 后补发条数（消费者箱）

# ---- 断言器：check_sentinel <期望 sentinel 列值> ----------------------------
# status --global 目标信箱行第 6 列与期望匹配 → 0。主节（alive/dead*/none 三态
# 断言）与演练 a（dead 语境喂 alive 期望必须 FAIL）共用同一实现——演练喂真断言
# 器防假绿（v1a selftest 同精神，ac16-2 演练节同款「两节同器双向」）。
check_sentinel() {
  local expect="$1" out line got
  out="$(ai_cli "$AIT_URL" "$PROJ" "$COL" "$SEND_SESSION" "$SEND_ROLE" status --global 2>/dev/null)" \
    || { echo "  检测: status --global 命令失败" >&2; return 1; }
  # 信箱行形态（renderGlobalLines 两空格分隔，status.go:233）：
  #   <proj>  <col>  <role>  pending=N  pos=N  sentinel=<三态>  <窗标记|->
  line="$(grep "^${PROJ}  ${COL}  ${WATCH_ROLE}  " <<<"$out" | head -1)"
  [ -z "$line" ] && { echo "  检测: 未找到信箱行 $PROJ/$COL/$WATCH_ROLE" >&2; return 1; }
  got="$(tr -s ' ' '\t' <<<"$line" | cut -f6)" # 第 6 列=sentinel=XXX
  case "$got" in
    "sentinel=$expect") return 0 ;;
    *)
      echo "  检测: got=$got 期望 sentinel=$expect（行: $line）" >&2
      return 1
      ;;
  esac
}

# wait_sentinel <期望态> <超时秒>：轮询观测（容忍进程冷启动抖动），期内观测到=0
wait_sentinel() {
  local expect="$1" timeout="$2"
  local deadline=$((SECONDS + timeout))
  while :; do
    if check_sentinel "$expect"; then return 0; fi
    [ "$SECONDS" -ge "$deadline" ] && return 1
    sleep 1
  done
}

# json_seqs <--json 输出> → stdout 空格分隔的 seq 集合（结构化面抽取无文本格式
# 假阳——v1a v1a_extract_seqs 同款实测写法）
json_seqs() {
  grep -o '"seq":[0-9]\+' <<<"$1" | grep -o '[0-9]\+' | tr '\n' ' '
}

# seqs_have <seq 集合串> <期望 seq...>：逐一包含断言，缺失打到 stderr
# → 0=全含；1=有缺失
seqs_have() {
  local pool="$1" s miss=""
  shift
  for s in "$@"; do
    case "$pool" in *" $s "*) ;; *) miss+="$s " ;; esac
  done
  [ -z "$miss" ] && return 0
  echo "  检测: 缺失 seq → $miss" >&2
  return 1
}

# send_one <目标箱 role> <level> <body> → stdout=seq（send --json 提取）
send_one() {
  local role="$1" level="$2" body="$3" out seq
  out="$(ai_send "$AIT_URL" "$PROJ" "$COL" "$SEND_SESSION" "$SEND_ROLE" \
    --to-role "$role" --level "$level" --body "$body" --json \
    --mirror-root "$V1C_TMP/mirror")" || { echo "  send 退出非 0: $out" >&2; return 1; }
  seq="$(sed -n 's/.*"seq":\([0-9][0-9]*\).*/\1/p' <<<"$out")"
  if [ -n "$seq" ] && [ "$seq" != "0" ]; then
    printf '%s' "$seq"
    return 0
  fi
  echo "  send --json 无有效 seq: $out" >&2
  return 1
}

# 前置失败快捷退出（trap 收尾，exit 1 与 summary 语义对齐）
die() { fail "$*"; exit 1; }

# =============================================================================
echo "==> [1/6] 前置链：起 serve + seed 域 + 两箱位点先行落行 + kill 前预发 $N_PRE 条"
start_server || die "start_server 起服务失败"
db="$(ait_db_path_from_config "$AIT_TMPDIR/config.json")" || die "解析 db.path 失败"
ait_seed_domain "$db" "$PROJ" "$COL" || die "seed 夹具域失败"
V1C_TMP="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-v1c.XXXXXX")" || die "mktemp 创建脚本临时目录失败"
WATCH_OUT="$V1C_TMP/watch.log" # 主 watch 输出（零 HIT 断言载体）
TWIN_OUT="$V1C_TMP/watch-twin.log" # 演练 b 对照 watch 输出
mkdir -p "$V1C_TMP/mirror" || die "创建镜像目录失败"

# 位点先行落行（值=当时全表 MAX(seq)=0）：poll 空拉兼会话注册（v1a 实证）。
# 位点必须先于消息落行——GetPositions 惰性落行取调用时刻 MAX(seq) 不回看，
# 位点落后于消息将把 kill 前消息跳过，「零丢失」无从断言。
ai_cli "$AIT_URL" "$PROJ" "$COL" "$CONS_SESSION" "$CONS_ROLE" poll --limit 1 >/dev/null 2>&1 \
  || die "消费者会话预注册/位点落行失败"
ai_cli "$AIT_URL" "$PROJ" "$COL" "$WATCH_SESSION" "$WATCH_ROLE" poll --limit 1 >/dev/null 2>&1 \
  || die "watch 会话预注册/位点落行失败"
pass "两会话 poll 空拉预注册（心跳注册会话+信箱位点先行落行值 0）"

PRE_SEQS=()
pre_levels=(normal important block)
for ((i = 0; i < N_PRE; i++)); do
  seq="$(send_one "$CONS_ROLE" "${pre_levels[i]}" "v1c-kill前预发第$((i + 1))条")" \
    || die "kill 前预发第 $((i + 1)) 条失败"
  PRE_SEQS+=("$seq")
  pass "kill 前预发[$((i + 1))] level=${pre_levels[i]} seq=$seq"
done

# =============================================================================
echo "==> [2/6] watch 后台起（executor 信箱哨兵）+ alive 中间断言"
# 直接后台起二进制（不经 ai_cli 函数后台化——$! 恒为 aiteam 进程 pid，kill -9
# 注入目标无歧义；身份四参显式传参脚本环境自洽，与 ai_cli 展开同构）
"$AIT_BIN" watch --server "$AIT_URL" --project "$PROJ" --column "$COL" \
  --session "$WATCH_SESSION" --role "$WATCH_ROLE" > "$WATCH_OUT" 2>&1 &
V1C_WATCH_PID=$!
V1C_WATCH_PIDS+=("$V1C_WATCH_PID")
if wait_sentinel "alive" 8; then
  pass "watch 注册成功，status 可见 sentinel=alive pid=$V1C_WATCH_PID (t=$(date +%H:%M:%S))"
else
  die "watch 起后 8s 内未观测到 sentinel=alive（注册失败/未进循环；watch.log: $(tr '\n' ' ' <"$WATCH_OUT" 2>/dev/null)）"
fi

# =============================================================================
echo "==> [3/6] kill -9 注入（真 kill 非 mock，AC16.2 实证同款）"
if kill -0 "$V1C_WATCH_PID" 2>/dev/null; then
  pass "注入前 watch 进程存活 pid=$V1C_WATCH_PID"
else
  die "注入前 watch 进程已意外死亡 pid=$V1C_WATCH_PID"
fi
T_KILL="$(date +%H:%M:%S)"
kill -9 "$V1C_WATCH_PID" 2>/dev/null || true
wait "$V1C_WATCH_PID" 2>/dev/null || true # reap 后台作业，抑制 bash「Killed」通知
if ait_wait_gone "$V1C_WATCH_PID" 10; then
  pass "kill -9 后 watch 进程消失 pid=$V1C_WATCH_PID (t=$T_KILL)"
else
  die "kill -9 后 watch 进程 10s 仍存活（MSYS 强杀失效，需 taskkill //F 兜底）pid=$V1C_WATCH_PID"
fi
if [ ! -s "$WATCH_OUT" ]; then
  pass "watch 输出零 HIT（值守全程空转未被打退，kill 注入有效）"
else
  die "watch 输出非空（值守期被消息打退提前退出，kill 注入无效）: $(tr '\n' ' ' <"$WATCH_OUT")"
fi

# =============================================================================
echo "==> [4/6] sleep 20（失活阈值 15s+5s 余量，D2）→ dead* 失联标记 + 演练 a"
sleep 20
if check_sentinel "dead*"; then
  pass "status --global 该信箱 sentinel=dead*（失活阈值后失联标记可见；t=$(date +%H:%M:%S)，kill 时刻 $T_KILL）"
else
  fail "sleep 20 后未见 sentinel=dead*（检测明细见上）"
fi
# 演练 a：dead 语境喂 alive 期望——同一断言器必须 FAIL，不失败=恒真假绿盲区
if check_sentinel "alive"; then
  fail "断言器盲区：dead 语境下 alive 期望未被抓住（假绿，判据 [4] 不可信）"
else
  pass "断言器自证演练 a：dead 语境喂 alive 期望被 FAIL（区分力在，明细见上）"
fi

# =============================================================================
echo "==> [5/6] 断言器自证演练 b（复活对照三态闭环；此刻值守箱仍零消息=twin 空转前提）"
# 同坐标（column+role）哨兵：主 watch 死行（>15s）→ FindAliveSentinel 无活哨兵 →
# RegisterSentinel upsert 复活同 id（B3-T2）→ last_ping_at 刷新 → alive。
"$AIT_BIN" watch --server "$AIT_URL" --project "$PROJ" --column "$COL" \
  --session "$WATCH_SESSION" --role "$WATCH_ROLE" --max-wait 4s > "$TWIN_OUT" 2>&1 &
V1C_TWIN_PID=$!
V1C_WATCH_PIDS+=("$V1C_TWIN_PID")
if wait_sentinel "alive" 8; then
  pass "复活对照：同坐标 watch 注册复活死行，status 回 sentinel=alive（[4] 的 dead* 非恒 dead 假判定）"
else
  fail "复活对照失败：twin watch 注册后 8s 内未见 sentinel=alive（twin.log: $(tr '\n' ' ' <"$TWIN_OUT" 2>/dev/null)）"
fi
wait "$V1C_TWIN_PID"
twin_rc=$?
if [ "$twin_rc" -eq 6 ]; then
  pass "对照 watch --max-wait 到时注销退出 6（先 DELETE 再退 6 路径，B3-T3）"
else
  fail "对照 watch 退出码 $twin_rc 期望 6（max-wait 注销路径）"
fi
if check_sentinel "none"; then
  pass "注销后 sentinel=none（DELETE 行删；alive/dead/none 三态全观测闭环）"
else
  fail "注销后未见 sentinel=none（检测明细见上）"
fi

# =============================================================================
echo "==> [6/6] kill 后补发 → poll 全量拉取零丢失断言（含 kill 前后新发）"
POST_SEQS=()
post_levels=(normal block)
for ((i = 0; i < N_POST; i++)); do
  seq="$(send_one "$CONS_ROLE" "${post_levels[i]}" "v1c-kill后补发第$((i + 1))条")" \
    || die "kill 后补发第 $((i + 1)) 条失败"
  POST_SEQS+=("$seq")
  pass "kill 后补发[$((i + 1))] level=${post_levels[i]} seq=$seq"
done
BOX_SEQ="$(send_one "$WATCH_ROLE" important "v1c-kill后值守箱补发")" \
  || die "值守箱 kill 后补发失败"
pass "kill 后值守箱补发 level=important seq=$BOX_SEQ（哨兵已死，消息照常入箱）"

# 消费者箱全量拉取（位点先行落行值 0 → kill 前预发+kill 后补发全可见）：
# 零丢失主判据——kill 前后新发消息 seq 全在。
poll_json="$(ai_cli "$AIT_URL" "$PROJ" "$COL" "$CONS_SESSION" "$CONS_ROLE" poll --limit 0 --json)" \
  || die "消费者 poll --limit 0 失败: $poll_json"
poll_pool=" $(json_seqs "$poll_json") "
if [ "$(wc -w <<<"$poll_pool")" -eq $((N_PRE + N_POST)) ]; then
  pass "消费者箱 poll COUNT=$((N_PRE + N_POST))（kill 前 $N_PRE + kill 后 $N_POST，零丢失计数）"
else
  fail "消费者箱 poll COUNT=$(wc -w <<<"$poll_pool") 期望 $((N_PRE + N_POST))"
fi
if seqs_have "$poll_pool" "${PRE_SEQS[@]}" "${POST_SEQS[@]}"; then
  pass "kill 前后新发消息 seq 全在消费者箱 poll 结果（零丢失逐一断言）"
else
  fail "消费者箱 poll 缺失 seq（明细见上）"
fi

# 值守箱拉取：哨兵已死的信箱消息照常可消费
box_json="$(ai_cli "$AIT_URL" "$PROJ" "$COL" "$WATCH_SESSION" "$WATCH_ROLE" poll --limit 0 --json)" \
  || die "值守箱 poll 失败: $box_json"
if seqs_have " $(json_seqs "$box_json") " "$BOX_SEQ"; then
  pass "值守箱 poll 拉到 kill 后补发 seq=$BOX_SEQ（哨兵死亡不影响信箱投递）"
else
  fail "值守箱 poll 未拉到 seq=$BOX_SEQ"
fi

# history 全表对照（无位点过滤的全量表查询）：COUNT=6 且 6 seq 全含
hist_json="$(ai_cli "$AIT_URL" "$PROJ" "$COL" "$SEND_SESSION" "$SEND_ROLE" \
  history --limit 500 --order asc --json)" || die "history 拉取失败: $hist_json"
hist_pool=" $(json_seqs "$hist_json") "
if [ "$(wc -w <<<"$hist_pool")" -eq $((N_PRE + N_POST + 1)) ]; then
  pass "history 全表 COUNT=$((N_PRE + N_POST + 1))（独立库无外来写入源）"
else
  fail "history 全表 COUNT=$(wc -w <<<"$hist_pool") 期望 $((N_PRE + N_POST + 1))"
fi
if seqs_have "$hist_pool" "${PRE_SEQS[@]}" "${POST_SEQS[@]}" "$BOX_SEQ"; then
  pass "history 对照：kill 前后全部 $((N_PRE + N_POST + 1)) 条 seq 全含"
else
  fail "history 缺失 seq（明细见上）"
fi

# =============================================================================
echo "==> 收尾"
stop_server
summary
