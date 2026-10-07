#!/usr/bin/env bash
# =============================================================================
# aiteam 验收脚本 v3-status-recovery.sh —— V3 status 恢复面（b7b-4，真二进制）。
#
# 判据（PRD §4 V3 + b7b-plan b7b-4 任务书原文）：零上下文一眼恢复问卷——
#   「status --global 单命令输出恢复问卷四要素，命令到断言完成 ≤30s」
#   四问卷要素（照技术设计 §4.2 输出样例锚点 grep）：
#     ① 项目/栏目状态行（<proj>  <col>  <role>  pending=N  pos=N  sentinel=<态>）
#     ② pending（位点差：有积压 vs 已消费清零并存）
#     ③ block 未回执清单（`block 未回执: N 条` + `  #<seq> <target> <- <sender>`)
#     ④ 时间窗标记（S4:(ALLOWED|WAITING)(23:00-09:00)——跨午夜 start>end 原样；
#        无窗栏目尾列 `-`＝不受限 AC3.4）
#
# 造现场（2 项目 3 栏目 + block 未回执 2 条 + 跨午夜窗 + 多角色位点差）：
#   v3-pa/01  executor_work   ← 3 normal   → pending=3 pos=0（积压态）
#   v3-pa/01  executor_blk_a  ← 1 block    → 未回执（清单要素①条）
#   v3-pa/02  executor_done   ← 2 normal → poll+ack → pending=0 pos=6（清零态，
#                             pos=信箱已消费最大 seq 非 条数——实测裁定见消费段注）
#   v3-pa/02  executor_blk_b  ← 1 block    → 未回执（清单要素②条）
#   v3-pb/01  executor_plan   ← 1 normal   → pending=1 pos=0（跨项目第二域）
#   跨午夜窗：window set --stage S4 --from 23:00 --to 09:00（v3-pa/01 栏目域，
#   start>end 跨午夜合法 AC3.3；判定态随运行时刻 ALLOWED/WAITING 二态正则断言）
#   role 全部守规约闸（2026-10-03 增补令：合法=controller/executor/executor_<标识>，
#   v2 脚本实测 salesperson=400 invalid_role）。
#
# 零上下文子 shell（b7b-T2 四条口径）：
#   ① env -u AITEAM_SERVER ② env -u AITEAM_TOKEN ③ cd 干净临时目录（斩仓库级
#   ./.aiteam/cli.json 查找链——cli.go clientRepoConfigPath=进程 CWD 下
#   ./.aiteam/cli.json）④ --server 显式传参（flag 五级链最高优先级胜出）。
#   ——模拟「新会话裸环境一眼恢复」：不带任何前次会话遗留上下文。
#
# 计时（AC11.4）：status 单命令 <5s（t0→t1）；命令到断言完成 ≤30s（t0→t2）。
# date +%s%N 毫秒差值（busybox date 无 %N 时退化 SECONDS*1000 秒级兜底）。
#
# 用法：
#   bash scripts/acceptance/v3-status-recovery.sh          # 真验收（退出 0=PASS）
#   bash scripts/acceptance/v3-status-recovery.sh selftest # 断言器破坏演练（防假绿）：
#                                                          # 独立现场只发 1 条 block
#                                                          # 喂「期望 2 条」真断言器
#                                                          # 必须 FAIL（=少造一个
#                                                          # block 逃不过断言）+
#                                                          # pending=99 坏期望必须
#                                                          # FAIL + 好夹具必须放行
#
# status 查询者身份：v3-status/controller（v3-pa/01 域）——controller role 未被
# 任何 direct 目标格子占用（格子全 executor_*），四头心跳 upsert 不触发格子冲突；
# 查询者自身 upsert 出现的信箱行（pending=0 pos=0）非问卷要素不断言。
# 夹具：独立 serve+独立临时库（不碰共享面）；ait_seed_domain 幂等双插
# （ON CONFLICT DO NOTHING）同项目跨栏目多次调用安全。
# =============================================================================
cd "$(dirname "$0")/../.." || exit 1
AIT_REPO_ROOT="$PWD" # ait_seed_domain 的 go -C 依赖解析根（module root）

# shellcheck source=common.sh
source scripts/acceptance/common.sh

# 脚本级临时目录+零上下文干净目录；EXIT trap 先清自己再调 ait_cleanup
# （common.sh 头注约定：宿主自定义 trap 须在其清理逻辑中调用 ait_cleanup）。
V3_TMP=""
V3_CLEAN="" # 零上下文子 shell 工作目录（主流程/selftest 各一，b7b-4 审查 F1：
            # 此前只清 V3_TMP，clean 目录 mktemp 泄漏——首跑实测 /tmp 残留 9 个）
v3_cleanup() {
  [ -n "$V3_CLEAN" ] && rm -rf "$V3_CLEAN" 2>/dev/null
  [ -n "$V3_TMP" ] && rm -rf "$V3_TMP" 2>/dev/null
  ait_cleanup
}
trap 'v3_cleanup' EXIT
trap 'exit 130' INT TERM # 信号转正常退出→触发 EXIT trap 统一清理

# 夹具常量：两项目三栏目 + 格子角色（规约闸合法面）。
V3_PA="v3-pa"
V3_PB="v3-pb"

# ---- 计时 --------------------------------------------------------------------

# v3_now_ms 当前毫秒（GNU date %N；busybox 退化 SECONDS 秒级——验收宿主双兼容）。
v3_now_ms() {
  local n
  n="$(date +%s%N)"
  case "$n" in
    *N*) echo $((SECONDS * 1000)) ;;
    *) echo $((n / 1000000)) ;;
  esac
}

# ---- 零上下文子 shell（b7b-T2 四条口径封装，主流程与 selftest 共用）-----------
# v3_zero_ctx_status <输出文件> <proj> <col> —— 环境变量卸载+干净 cwd+--server
# 显式跑 status --global；输出落文件，退出码回传。
v3_zero_ctx_status() {
  local out="$1" proj="$2" col="$3"
  (
    cd "${V3_CLEAN:?零上下文工作目录未初始化}" ||
      exit 3 # 干净目录不存在=本地失败（防落穿仓库 CWD 假装零上下文）
    env -u AITEAM_SERVER -u AITEAM_TOKEN "$AIT_BIN" status --global \
      --server "$AIT_URL" --project "$proj" --column "$col" \
      --session v3-status --role controller
  ) > "$out" 2> "$out.err"
}

# ---- 断言器（主流程与 selftest 破坏演练共用同一实现——演练喂真断言器防假绿）----

# v3_assert_mailbox <输出文件> <proj> <col> <role> <pending> <pos> → 0=存在该
# 信箱状态行（§4.2 形态两空格分隔；sentinel 列词法断言 dead 追 * 异常标记）。
v3_assert_mailbox() {
  local out="$1" proj="$2" col="$3" role="$4" pend="$5" pos="$6"
  if grep -Eq "^$proj  $col  $role  pending=$pend  pos=$pos  sentinel=[a-z]+\*?  " "$out"; then
    return 0
  fi
  echo "  明细[信箱行]: 未命中 $proj/$col/$role pending=$pend pos=$pos，实际：" >&2
  grep -E "^$proj  $col  " "$out" | sed 's/^/    /' >&2
  return 1
}

# v3_assert_window_mark <输出文件> → 0=存在跨午夜窗标记列（判定态 ALLOWED/WAITING
# 随运行时刻二选一；start>end=23:00-09:00 原样透出 AC3.3）。
v3_assert_window_mark() {
  local out="$1"
  if grep -Eq 'S4:(ALLOWED|WAITING)\(23:00-09:00\)' "$out"; then
    return 0
  fi
  echo "  明细[窗标记]: 未命中 S4:(ALLOWED|WAITING)(23:00-09:00)，实际窗列：" >&2
  grep -E 'S4:' "$out" | sed 's/^/    /' >&2
  return 1
}

# v3_assert_window_off <输出文件> <proj> <col> <role> → 0=该信箱行尾列恰 `-`
# （未配置窗=不受限 AC3.4——与带窗行同表并存，恢复问卷「是否受窗限制」两态齐备）。
v3_assert_window_off() {
  local out="$1" proj="$2" col="$3" role="$4"
  if grep -Eq "^$proj  $col  $role  pending=[0-9]+  pos=[0-9]+  sentinel=[a-z]+\*?  -$" "$out"; then
    return 0
  fi
  echo "  明细[无窗行]: 未命中 $proj/$col/$role 行尾 -（实际：" >&2
  grep -E "^$proj  $col  " "$out" | sed 's/^/    /' >&2
  return 1
}

# v3_assert_blocks <输出文件> <期望条数> [<seq>:<target>...] → 0=标题行计数相符
# 且逐条清单行命中（`  #<seq> <target> <- <sender> (<created_at>)` §4.2 形态；
# sender 列=发送者 sender_label=<session>@<column> 冗余显示不作断言锚）。
v3_assert_blocks() {
  local out="$1" want="$2"; shift 2
  local n pair seq tgt
  n="$(sed -n 's/^block 未回执: \([0-9]\+\) 条$/\1/p' "$out")"
  if [ "$n" != "$want" ]; then
    echo "  明细[block计数]: 输出=${n:-<无标题行>} 期望=$want，实际段：" >&2
    grep -A3 '^block 未回执' "$out" | sed 's/^/    /' >&2
    return 1
  fi
  for pair in "$@"; do
    seq="${pair%%:*}"
    tgt="${pair#*:}"
    if ! grep -q "^  #$seq $tgt <- " "$out"; then
      echo "  明细[block清单]: 未命中 #$seq $tgt，实际段：" >&2
      grep -A3 '^block 未回执' "$out" | sed 's/^/    /' >&2
      return 1
    fi
  done
  return 0
}

# v3_assert_sections <输出文件> → 0=头行+sessions 行+resources 行在位
# （§4.2 输出样例锚点：恢复问卷「谁在线/资源占用」两段）。
v3_assert_sections() {
  local out="$1"
  grep -q '^aiteam status ' "$out" || { echo "  明细[头行]: 缺 aiteam status <时刻>" >&2; return 1; }
  grep -q '^sessions: ' "$out" || { echo "  明细[sessions]: 缺 sessions: 行" >&2; return 1; }
  grep -q '^resources in_use: ' "$out" || { echo "  明细[resources]: 缺 resources in_use: 行" >&2; return 1; }
  return 0
}

# ---- 断言器破坏演练（selftest 模式，防假绿）----------------------------------
# 真现场造「少一个 block」喂真断言器：独立 serve 只发 1 条 block，期望 2 必须抓
# （任务书演练口径：少造一个 block 未回执→断言 FAIL）；pending 坏期望必须抓；
# 好期望放行（防恒失败假红）。
v3_destructive_selftest() {
  echo "==> 断言器破坏演练（真现场少造一个 block，坏期望必须 FAIL / 好期望必须 PASS）"
  start_server >/dev/null || { fail "演练 start_server 失败"; return 1; }
  local db
  db="$(ait_db_path_from_config "$AIT_TMPDIR/config.json")" || { fail "解析 db.path 失败"; return 1; }
  ait_seed_domain "$db" v3-st 01 >/dev/null || { fail "演练 seed 失败"; return 1; }
  ai_cli "$AIT_URL" v3-st 01 st-x executor_x poll --limit 1 >/dev/null 2>&1 \
    || { fail "演练格子预注册失败"; return 1; }
  local blk_seq mirror_root="$V3_TMP/mirror-self"
  mkdir -p "$mirror_root" || { fail "演练创建镜像目录失败"; return 1; }
  blk_seq="$(ai_send "$AIT_URL" v3-st 01 st-snd executor_s1 --to-role executor_x \
    --level block --body 演练阻断条 --json --mirror-root "$mirror_root" | grep -o '"seq":[0-9]\+' | grep -o '[0-9]\+')"
  [ -n "$blk_seq" ] || { fail "演练 block 发送失败"; return 1; }
  V3_CLEAN="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-v3-clean-self.XXXXXX")"
  local out="$V3_TMP/self-status.out"
  v3_zero_ctx_status "$out" v3-st 01
  local bad=0
  # 1) 现场只 1 条 block，喂期望 2 → 必须抓（少造一个 block 逃不过断言器）
  if v3_assert_blocks "$out" 2 "$blk_seq:v3-st/01/executor_x"; then
    fail "破坏演练：单 block 现场未被「期望2」断言器抓住（假绿）"
    bad=1
  else
    pass "破坏演练：单 block 现场喂期望 2 被断言器 FAIL（少造一个 block 即 FAIL）"
  fi
  # 2) 同现场喂期望 1 → 放行（断言器对正确期望不误杀）
  if v3_assert_blocks "$out" 1 "$blk_seq:v3-st/01/executor_x"; then
    pass "破坏演练：单 block 现场喂期望 1 放行（无恒失败假红）"
  else
    fail "破坏演练：正确期望被误杀（恒失败假红）"
    bad=1
  fi
  # 3) pending 坏期望 → 必须抓
  if v3_assert_mailbox "$out" v3-st 01 executor_x 99 0; then
    fail "破坏演练：pending=99 坏期望未被断言器抓住（假绿）"
    bad=1
  else
    pass "破坏演练：pending=99 坏期望被断言器 FAIL"
  fi
  # 4) pending 好期望放行（executor_x 收 1 条 block 未消费 pending=1 pos=0）
  if v3_assert_mailbox "$out" v3-st 01 executor_x 1 0; then
    pass "破坏演练：pending=1 好期望放行"
  else
    fail "破坏演练：pending 好期望被误杀（恒失败假红）"
    bad=1
  fi
  stop_server
  return $bad
}

# ---- 主验收流程 --------------------------------------------------------------

v3_run() {
  echo "==> 起服务（真二进制 serve + 独立临时库）"
  start_server >/dev/null || { fail "start_server 起服务失败"; return 1; }
  local db
  db="$(ait_db_path_from_config "$AIT_TMPDIR/config.json")" || { fail "解析 db.path 失败"; return 1; }

  echo "==> seed 两项目三栏目（v3-pa/01 v3-pa/02 v3-pb/01，ait_seed_domain 幂等）"
  ait_seed_domain "$db" "$V3_PA" 01 >/dev/null || { fail "seed v3-pa/01 失败"; return 1; }
  ait_seed_domain "$db" "$V3_PA" 02 >/dev/null || { fail "seed v3-pa/02 失败"; return 1; }
  ait_seed_domain "$db" "$V3_PB" 01 >/dev/null || { fail "seed v3-pb/01 失败"; return 1; }

  echo "==> 造现场：格子预注册+direct 发送+消费+block 未回执+跨午夜窗"
  # 镜像仓落脚本临时目录（v1a 先例同款）——send 缺省 --mirror-root=进程 CWD 下
  # .aiteam/，不显式指定会把 mirror-<proj>-<col>.md 写进仓库工作区（首跑实测残留）。
  local mirror_root="$V3_TMP/mirror"
  mkdir -p "$mirror_root" || { fail "创建镜像目录失败"; return 1; }
  # 格子预注册（poll 心跳隐式注册，恰 1 活会话/格子）
  local g
  for g in "executor_work" "executor_blk_a"; do
    ai_cli "$AIT_URL" "$V3_PA" 01 "recv-$g" "$g" poll --limit 1 >/dev/null 2>&1 \
      || { fail "格子预注册失败 v3-pa/01/$g"; return 1; }
  done
  for g in "executor_done" "executor_blk_b"; do
    ai_cli "$AIT_URL" "$V3_PA" 02 "recv-$g" "$g" poll --limit 1 >/dev/null 2>&1 \
      || { fail "格子预注册失败 v3-pa/02/$g"; return 1; }
  done
  ai_cli "$AIT_URL" "$V3_PB" 01 "recv-executor_plan" "executor_plan" poll --limit 1 >/dev/null 2>&1 \
    || { fail "格子预注册失败 v3-pb/01/executor_plan"; return 1; }
  pass "5 格子预注册（poll 隐式注册，恰 1 活会话/格子）"

  # direct 发送（发送者会话个体化 v3-send-N/executor_sN；--json 收 seq）
  local seqs_work="" seqs_done="" seqs_plan="" blk_a_seq blk_b_seq s
  for s in 1 2 3; do
    seqs_work+="$(ai_send "$AIT_URL" "$V3_PA" 01 "v3-send-$s" "executor_s$s" \
      --to-role executor_work --level normal --body "v3积压条$s" --json --mirror-root "$mirror_root" \
      | grep -o '"seq":[0-9]\+' | grep -o '[0-9]\+') "
  done
  blk_a_seq="$(ai_send "$AIT_URL" "$V3_PA" 01 v3-send-4 executor_s4 \
    --to-role executor_blk_a --level block --body "v3阻断请先回执" --json --mirror-root "$mirror_root" \
    | grep -o '"seq":[0-9]\+' | grep -o '[0-9]\+')"
  seqs_done+="$(ai_send "$AIT_URL" "$V3_PA" 02 v3-send-5 executor_s5 \
    --to-role executor_done --level normal --body "v3待消费条A" --json --mirror-root "$mirror_root" \
    | grep -o '"seq":[0-9]\+' | grep -o '[0-9]\+') "
  seqs_done+="$(ai_send "$AIT_URL" "$V3_PA" 02 v3-send-6 executor_s6 \
    --to-role executor_done --level normal --body "v3待消费条B" --json --mirror-root "$mirror_root" \
    | grep -o '"seq":[0-9]\+' | grep -o '[0-9]\+') "
  blk_b_seq="$(ai_send "$AIT_URL" "$V3_PA" 02 v3-send-7 executor_s7 \
    --to-role executor_blk_b --level block --body "v3阻断二请先回执" --json --mirror-root "$mirror_root" \
    | grep -o '"seq":[0-9]\+' | grep -o '[0-9]\+')"
  seqs_plan+="$(ai_send "$AIT_URL" "$V3_PB" 01 v3-send-8 executor_s8 \
    --to-role executor_plan --level normal --body "v3跨项目条" --json --mirror-root "$mirror_root" \
    | grep -o '"seq":[0-9]\+' | grep -o '[0-9]\+') "
  if [ -n "${seqs_work// }" ] && [ -n "$blk_a_seq" ] && [ -n "${seqs_done// }" ] \
    && [ -n "$blk_b_seq" ] && [ -n "${seqs_plan// }" ]; then
    pass "8 条发送落位（3 积压+1 blockA+2 待消费+1 blockB+1 跨项目；发送者会话个体化）"
  else
    fail "发送存在失败：work=[$seqs_work] blk_a=[$blk_a_seq] done=[$seqs_done] blk_b=[$blk_b_seq] plan=[$seqs_plan]"
    return 1
  fi

  # 位点差：executor_done 拉取+ack 确认 → pos=6 pending=0（与 executor_work 积压
  # 并存）。消费两段式：poll 只拉取不动位点（实测 poll 摘要 mailbox_pos 恒 0），
  # 位点推进归 ack #11（--seq 省略=推进到当前各自最大可见，ack.go）；pos 语义=
  # 该信箱已消费最大 seq（全局 seq 维度，实测该格子 seq5/6 两条 → pos=6 非 2）。
  ai_cli "$AIT_URL" "$V3_PA" 02 recv-executor_done executor_done poll --limit 10 >/dev/null 2>&1 \
    || { fail "executor_done 拉取失败"; return 1; }
  ai_cli "$AIT_URL" "$V3_PA" 02 recv-executor_done executor_done ack >/dev/null 2>&1 \
    || { fail "executor_done ack 失败"; return 1; }
  pass "位点差构造：executor_done poll+ack 消费 2 条（pos=6 pending=0）vs executor_work 积压（pos=0 pending=3）"

  # 跨午夜窗（start>end 合法 AC3.3）：v3-pa/01 栏目域 S4=23:00~09:00。
  # 组级命令（window <动词>）动词必须是第一参数（RunWindowGroup args[0] 分发），
  # ai_cli 的「子命令在前+身份 flag 随后」展开会把 --server 误当动词（实测
  # 「未知 window 动词 --server」退 2）——组级不走 ai_cli，直接拼命令
  # （动词置首+flag 随后；v1c 的 watch 一级命令直拼同款先例）。
  if "$AIT_BIN" window set --server "$AIT_URL" \
    --project "$V3_PA" --column 01 --session v3-win --role executor_win \
    --stage S4 --from 23:00 --to 09:00 >/dev/null 2>&1; then
    pass "跨午夜窗落位：v3-pa/01 S4=23:00-09:00（#21 覆盖式写）"
  else
    fail "window set 跨午夜窗失败"
    return 1
  fi

  echo "==> 零上下文子 shell 跑 status --global（b7b-T2 四条口径）+ 计时"
  V3_CLEAN="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-v3-clean.XXXXXX")"
  local t0 t1 t2 status_ms total_ms out="$V3_TMP/status.out"
  t0="$(v3_now_ms)"
  v3_zero_ctx_status "$out" "$V3_PA" 01
  local rc=$?
  t1="$(v3_now_ms)"
  if [ "$rc" -ne 0 ]; then
    echo "  status 非零退出 rc=$rc，stderr：" >&2
    sed 's/^/    /' "$out.err" >&2
    fail "status --global 零上下文执行失败"
    return 1
  fi
  echo "---- status --global 实测输出 ----"
  sed 's/^/    /' "$out"
  echo "----------------------------------"

  echo "==> 断言四问卷要素（§4.2 输出样例锚点）"
  local bad=0
  # 要素①：项目/栏目状态行（跨两项目三栏目）
  if v3_assert_mailbox "$out" "$V3_PA" 01 executor_work 3 0 \
    && v3_assert_mailbox "$out" "$V3_PA" 02 executor_done 0 6 \
    && v3_assert_mailbox "$out" "$V3_PB" 01 executor_plan 1 0; then
    pass "要素①②项目/栏目状态行+pending 位点差：积压 3/0、清零 0/6、跨项目 1/0 三态并存"
  else
    fail "要素①②状态行/pending 断言失败（明细见上）"
    bad=1
  fi
  # 要素③：block 未回执清单（2 条，跨两栏目 target 精确）
  if v3_assert_blocks "$out" 2 \
    "$blk_a_seq:$V3_PA/01/executor_blk_a" \
    "$blk_b_seq:$V3_PA/02/executor_blk_b"; then
    pass "要素③block 未回执清单：2 条全命中（#$blk_a_seq v3-pa/01/executor_blk_a + #$blk_b_seq v3-pa/02/executor_blk_b）"
  else
    fail "要素③block 未回执清单断言失败（明细见上）"
    bad=1
  fi
  # 要素④：时间窗标记（跨午夜二态判定+无窗栏目尾列 -）
  if v3_assert_window_mark "$out" \
    && v3_assert_window_off "$out" "$V3_PA" 02 executor_done \
    && v3_assert_window_off "$out" "$V3_PB" 01 executor_plan; then
    pass "要素④时间窗标记：S4:(ALLOWED|WAITING)(23:00-09:00) 跨午夜原样 + 无窗栏目尾列 - 两态齐备"
  else
    fail "要素④时间窗标记断言失败（明细见上）"
    bad=1
  fi
  # 样例其余段：头行+sessions+resources
  if v3_assert_sections "$out"; then
    pass "问卷其余段：头行+sessions 行+resources in_use 行在位"
  else
    fail "问卷其余段断言失败（明细见上）"
    bad=1
  fi

  # 计时判据（AC11.4 前半）：status 单命令 <5s；命令到断言完成 ≤30s
  t2="$(v3_now_ms)"
  status_ms=$((t1 - t0))
  total_ms=$((t2 - t0))
  echo "==> 计时：status 单命令=${status_ms}ms（判据 <5000ms）；命令到断言完成=${total_ms}ms（判据 ≤30000ms）"
  if [ "$status_ms" -lt 5000 ]; then
    pass "status 单命令 ${status_ms}ms < 5000ms（AC11.4 前半）"
  else
    fail "status 单命令 ${status_ms}ms ≥ 5000ms 超判据"
    bad=1
  fi
  if [ "$total_ms" -le 30000 ]; then
    pass "命令到断言完成 ${total_ms}ms ≤ 30000ms（V3 时限判据）"
  else
    fail "命令到断言完成 ${total_ms}ms > 30000ms 超判据"
    bad=1
  fi

  echo "==> 收尾"
  stop_server
  if [ "$bad" -eq 0 ]; then
    pass "V3 汇总：零上下文 status --global 四问卷要素全命中（${status_ms}ms/${total_ms}ms 双时限达标）"
  else
    fail "V3 汇总：存在失败断言（明细见上）"
  fi
  return $bad
}

# ---- 入口分流：selftest=断言器破坏演练；其余=真验收 --------------------------
V3_TMP="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-v3.XXXXXX")" \
  || { echo "FAIL: mktemp -d 创建脚本临时目录失败" >&2; exit 1; }
if [ "${1:-}" = "selftest" ]; then
  v3_destructive_selftest
  summary # FAIL>0 时自身 exit 1
  exit 0  # 全过显式退出（防落穿主验收流程）
fi

v3_run
summary
