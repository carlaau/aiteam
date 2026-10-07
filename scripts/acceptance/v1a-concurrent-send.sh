#!/usr/bin/env bash
# =============================================================================
# aiteam 验收脚本 v1a-concurrent-send.sh —— V1a 重号=0 回归（b7b-2，真二进制）。
#
# 判据（PRD §4 V1a 原文 + b7b-spec §一/§三，AC5.2 复验）：
#   「回归脚本：多轮『20 进程并发 send → 全量拉取比对』
#     PASS = 每轮序号两两唯一、消息数=发送数」
#   即：默认 3 轮（$1 可调）× 每轮 20 后台并发 send（同栏目混合角色）→
#   逐个 wait 收退出码全 0 → history --json 全量拉取 → seq 抽取 →
#   唯一性（sort+uniq -d 为空）+ COUNT=20 断言；任一失败=FAIL 并列明细
#   （哪个 seq 重复/计数差多少）。
#
# 用法：
#   bash scripts/acceptance/v1a-concurrent-send.sh            # 默认 3 轮验收（退出 0=PASS）
#   bash scripts/acceptance/v1a-concurrent-send.sh 5          # 自定轮数
#   bash scripts/acceptance/v1a-concurrent-send.sh selftest   # 断言器破坏演练（防假绿）：
#                                                             # 造重复 seq/缺条/坏退出码喂真断言器，
#                                                             # 断言器必须 FAIL——FAIL 不到场=断言器失效
#
# 夹具（每轮脚本独立 serve+独立临时库，不碰共享面）：
#   - ait_seed_domain seed 项目/栏目（心跳存在性短路挡死 CLI 空库首登记，
#     已知死锁（首栏目登记命令面不可达）——seed 直插库，见 common.sh ait_seed_domain 头注）
#   - direct 目标格子预注册恰 1 活会话（agent_conflict 防线：目标格子多活
#     会话=409，实测形态；接收者唯一即规避）+ 发送方角色个体化
#     （controller / executor_w<轮>-<i>），混合 direct/bus 目标与三级 level
#   - 20 进程共享同一 --mirror-root 镜像仓（试点真实形态：同仓库多会话并发
#     双写；V1a 只断言退出码与 seq，镜像行完整性归 V1b）
# =============================================================================
cd "$(dirname "$0")/../.." || exit 1
AIT_REPO_ROOT="$PWD" # ait_seed_domain 的 go -C 依赖解析根（module root）

# shellcheck source=common.sh
source scripts/acceptance/common.sh

# 脚本级临时目录（并发输出/拉取缓存）；EXIT trap 先清自己再调 ait_cleanup
# （common.sh 头注约定：宿主自定义 trap 须在其清理逻辑中调用 ait_cleanup）。
V1A_TMP=""
v1a_cleanup() {
  [ -n "$V1A_TMP" ] && rm -rf "$V1A_TMP" 2>/dev/null
  ait_cleanup
}
trap 'v1a_cleanup' EXIT
trap 'exit 130' INT TERM # 信号转正常退出→触发 EXIT trap 统一清理

# 夹具常量：项目/栏目/接收者格子（唯一活会话）/history 拉取身份
V1A_PROJECT="v1a-p"
V1A_COLUMN="01"
V1A_RECV_ROLE="executor_recv"     # direct 目标格子角色（恰 1 活会话）
V1A_AUDIT_SESSION="v1a-audit"     # history 拉取用会话（零消息杂音，GET 不写 messages）
V1A_AUDIT_ROLE="controller"
V1A_CONCURRENCY=20                # PRD 判据钉死 20 进程/轮
V1A_JOBS_OUT=""                   # 并发 job 输出根目录

# ---- 断言器（主流程与 selftest 破坏演练共用同一实现——演练喂真断言器防假绿）----

# v1a_assert_exit_codes <标签> <退出码串（空格分隔）> → 0=全 0
# PRD 判据「逐个 wait 收退出码全 0」；FAIL 明细列出第几个 job 非零。
v1a_assert_exit_codes() {
  local label="$1" rcs="$2" rc i=0 bad=""
  for rc in $rcs; do
    i=$((i + 1))
    if [ "$rc" -ne 0 ]; then bad+="job#$i(退出$rc) "; fi
  done
  if [ -n "$bad" ]; then
    echo "  明细[$label]: 非零退出码 → $bad" >&2
    return 1
  fi
  return 0
}

# v1a_extract_seqs <history --json 输出> → stdout 每行一个 seq（升序去稳由调用方 sort）
# --json 为结构化面（send --json 与 #13 同构），seq 键仅 messages[].seq 一处，
# 无行首 [N] 文本格式的 body 假阳风险——b7b-plan「实测后选可靠者」取 --json。
v1a_extract_seqs() {
  grep -o '"seq":[0-9]\+' <<<"$1" | grep -o '[0-9]\+'
}

# v1a_assert_round <标签> <history --json 输出> <期望条数> → 0=唯一且计数相符
# PRD 判据「每轮序号两两唯一、消息数=发送数」；FAIL 明细列出重复 seq 与计数差。
v1a_assert_round() {
  local label="$1" json="$2" want="$3"
  local seqs dup count
  seqs="$(v1a_extract_seqs "$json" | sort -n)"
  count="$(printf '%s' "$seqs" | grep -c . )"         # 空输入 grep -c 退 1 但输出 0，够用
  dup="$(printf '%s\n' "$seqs" | sort -n | uniq -d)"
  if [ "$count" -ne "$want" ]; then
    echo "  明细[$label]: COUNT=$count 期望=$want（缺 $((want - count)) 条）" >&2
    return 1
  fi
  if [ -n "$dup" ]; then
    echo "  明细[$label]: 重复 seq → $(printf '%s' "$dup" | tr '\n' ' ')" >&2
    return 1
  fi
  return 0
}

# ---- 断言器破坏演练（selftest 模式，防假绿）----------------------------------
# 造坏夹具直接喂上方真断言器：断言器必须返回失败，不失败=断言器失效（退出 1）。
# 反向各跑一条好夹具（断言器必须放行）防「恒失败假红」。
v1a_destructive_selftest() {
  echo "==> 断言器破坏演练（坏夹具必须 FAIL / 好夹具必须 PASS）"
  local dup_json missing_json ok_json out=0
  # 1) 重复 seq：21 个 seq 含一对重复 → 退出码断言器不设防，round 断言器必须抓
  dup_json='{"messages":[{"seq":1},{"seq":2},{"seq":3},{"seq":3}]}'
  if v1a_assert_round "演练-重复seq" "$dup_json" 4; then
    fail "破坏演练：重复 seq 夹具未被断言器抓住（假绿）"
    out=1
  else
    pass "破坏演练：重复 seq 夹具被断言器 FAIL（断言器有效）"
  fi
  # 2) 缺条：期望 20 实给 4 条唯一 → COUNT 断言必须抓
  missing_json='{"messages":[{"seq":10},{"seq":11},{"seq":12},{"seq":13}]}'
  if v1a_assert_round "演练-缺条" "$missing_json" 20; then
    fail "破坏演练：缺条夹具未被断言器抓住（假绿）"
    out=1
  else
    pass "破坏演练：缺条夹具被断言器 FAIL（COUNT=4 期望 20）"
  fi
  # 3) 坏退出码：job#3 非零 → 退出码断言器必须抓
  if v1a_assert_exit_codes "演练-退出码" "0 0 5 0 0"; then
    fail "破坏演练：非零退出码夹具未被断言器抓住（假绿）"
    out=1
  else
    pass "破坏演练：非零退出码（job#3 退 5）被断言器 FAIL"
  fi
  # 4) 好夹具反向健全：唯一 4 条喂期望 4 必须放行；全 0 退出码必须放行
  ok_json='{"messages":[{"seq":1},{"seq":2},{"seq":3},{"seq":4}]}'
  if v1a_assert_round "演练-好夹具" "$ok_json" 4 && v1a_assert_exit_codes "演练-好退出码" "0 0 0"; then
    pass "破坏演练：好夹具被断言器放行（无恒失败假红）"
  else
    fail "破坏演练：好夹具被断言器误杀（恒失败假红）"
    out=1
  fi
  return $out
}

# ---- 主验收流程 --------------------------------------------------------------

v1a_run() {
  local rounds="${1:-3}"
  case "$rounds" in
    ''|*[!0-9]*) echo "FAIL: 轮数须为正整数，得到 $rounds" >&2; return 2 ;;
  esac
  if [ "$rounds" -lt 1 ]; then
    echo "FAIL: 轮数须 ≥1，得到 $rounds" >&2
    return 2
  fi
  echo "==> 起服务（真二进制 serve + 独立临时库）"
  start_server || { fail "start_server 起服务失败"; return 1; }
  local db
  db="$(ait_db_path_from_config "$AIT_TMPDIR/config.json")" || { fail "解析 db.path 失败"; return 1; }
  echo "==> seed 夹具域 $V1A_PROJECT/$V1A_COLUMN（引导死锁 workaround）"
  ait_seed_domain "$db" "$V1A_PROJECT" "$V1A_COLUMN" || { fail "seed 夹具域失败"; return 1; }

  V1A_TMP="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-v1a.XXXXXX")" \
    || { fail "mktemp -d 创建脚本临时目录失败"; return 1; }
  V1A_JOBS_OUT="$V1A_TMP/jobs"
  mkdir -p "$V1A_JOBS_OUT" || { fail "创建并发输出目录失败"; return 1; }
  local mirror_root="$V1A_TMP/repo"

  # 预注册 direct 目标格子（恰 1 活会话，规避 agent_conflict 409）与 chat 无关
  echo "==> 预注册 direct 接收者格子（$V1A_RECV_ROLE，恰 1 活会话）"
  if ai_cli "$AIT_URL" "$V1A_PROJECT" "$V1A_COLUMN" recv-pre "$V1A_RECV_ROLE" poll --limit 1 >/dev/null 2>&1; then
    pass "接收者格子预注册（poll 心跳隐式注册）"
  else
    fail "接收者格子预注册失败"
    return 1
  fi

  local round prev_max=0 bad=0
  for round in $(seq 1 "$rounds"); do
    echo "==> 第 $round/$rounds 轮：$V1A_CONCURRENCY 进程并发 send（同栏目混合角色/目标/级别）"
    local pids=() i role level tgt
    for i in $(seq 1 "$V1A_CONCURRENCY"); do
      # 混合角色：controller / executor_<个体化>（个体化防目标格子多活会话 409，
      # 亦覆盖 executor_ 前缀多执行者形态）；混合目标：direct/bus 交替；
      # 混合级别：normal/important/block 按 (i/3+轮)%3 与角色正交轮转（同余
      # 绑定会让角色与级别恒耦合）。
      case $((i % 3)) in
        0) role="controller" ;;
        *) role="executor_r${round}j${i}" ;;
      esac
      case $(((i / 3 + round) % 3)) in
        0) level="normal" ;;
        1) level="important" ;;
        2) level="block" ;;
      esac
      if [ $((i % 2)) -eq 0 ]; then tgt="--bus"; else tgt="--to-role $V1A_RECV_ROLE"; fi
      # shellcheck disable=SC2086 —— tgt 双形态按词展开是本行语义
      ai_send "$AIT_URL" "$V1A_PROJECT" "$V1A_COLUMN" "r${round}j${i}" "$role" \
        $tgt --level "$level" --body "v1a-r${round}-j${i}" --json \
        --mirror-root "$mirror_root" \
        > "$V1A_JOBS_OUT/out.$i" 2> "$V1A_JOBS_OUT/err.$i" &
      pids+=($!)
    done
    # 逐个 wait 收退出码（PRD 判据：全 0）
    local rcs="" pid
    for pid in "${pids[@]}"; do
      wait "$pid"
      rcs+="$? "
    done
    if v1a_assert_exit_codes "第${round}轮退出码" "${rcs% }"; then
      pass "第 $round 轮：20 并发 send 退出码全 0"
    else
      fail "第 $round 轮：并发 send 存在非零退出码"
      bad=1
    fi
    # 全量拉取本轮消息（独立库无外来写入源）。b7b-2 实测发现：store 权威语义
    # 是 seq >= SinceSeq（含起点，internal/store/messages.go MessageFilter），
    # 而 CLI --since-seq 帮助文本写「seq > 该值」——文案与实现相悖（文档漂移，
    # 已报备不在本脚本任务内改 Go）。脚本按权威语义适配：传 prev_max+1 等价
    # 「只取上一轮 max 之后的新增」，首轮 0+1=1（seq 全局自增自 1 起，等价不过滤）。
    local json
    if ! json="$(ai_cli "$AIT_URL" "$V1A_PROJECT" "$V1A_COLUMN" "$V1A_AUDIT_SESSION" "$V1A_AUDIT_ROLE" \
      history --json --order asc --since-seq "$((prev_max + 1))" --limit 500)"; then
      fail "第 $round 轮：history 拉取失败"
      bad=1
      continue
    fi
    printf '%s' "$json" > "$V1A_TMP/history-r$round.json"
    if v1a_assert_round "第${round}轮seq" "$json" "$V1A_CONCURRENCY"; then
      pass "第 $round 轮：seq 两两唯一且 COUNT=$V1A_CONCURRENCY"
    else
      fail "第 $round 轮：seq 唯一性/计数断言失败（明细见上）"
      bad=1
    fi
    prev_max="$(v1a_extract_seqs "$json" | sort -n | tail -1)"
    [ -n "$prev_max" ] || prev_max=0
  done

  echo "==> 收尾"
  stop_server
  if [ "$bad" -eq 0 ]; then
    pass "V1a 汇总：$rounds 轮 × $V1A_CONCURRENCY 并发全部唯一且计数相符（重号=0）"
  else
    fail "V1a 汇总：存在失败轮（重号/缺条/非零退出），明细见上"
  fi
  return $bad
}

# ---- 入口分流：selftest=断言器破坏演练；其余=真验收 --------------------------
if [ "${1:-}" = "selftest" ]; then
  v1a_destructive_selftest
  summary # FAIL>0 时自身 exit 1
  exit 0  # 全过显式退出（防落穿主验收流程）
fi

v1a_run "${1:-3}"
summary
