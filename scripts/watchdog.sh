#!/usr/bin/env bash
# =============================================================================
# watchdog.sh —— aiteam 系统健康巡检（约定层兜底；全系统一份，总控侧挂）
# =============================================================================
# 值守哲学：aiteam 流转纯事件驱动（哨兵=传呼机，消息必达），本脚本不参与
#   调度、不唤醒会话——只做进程级健康检查：
#   ① 服务可达性（status 不可达=服务死）
#   ② 会话心跳失联（alive:false；失联≠死亡，报给主控人判）
# 异常动作为 send 一条 important 给 controller（经消息链唤醒，人在回路），
# 同一异常只报一次（恢复健康后清去重，再异常可再报）；全部健康则静默循环。
#
# 自身心跳与巡检节拍：每轮开头的 status 调用同时刷新本脚本会话心跳
#   （last_seen）。**「看板 watchdog 会话常绿=脚本活着」的语义成立条件=
#   巡检间隔 < 服务端心跳失联阈值（heartbeat_timeout_sec，默认 900s）**——
#   间隔超过阈值会让 last_seen 老化标红（sleep 后半段必红，实战反馈实证）。
#   故默认 5m（阈值/3 余量）；启动自检若发现间隔 ≥ 阈值/2 打 stderr 警告
#   （仍按用户参数运行，不擅自改值）。脚本死了=心跳停刷看板标红（失联红=
#   脚本死，终极看门狗=用户看板一眼）。
#
# 用法: scripts/watchdog.sh [间隔]      间隔为 sleep 时长字面（默认 5m，如 1m/15m）
# 身份: 默认 --project aiteam --column 05 --session watchdog --role executor_watchdog
#       （role 走 executor_<标识> 形态占一格；跨项目用 WD_PROJECT/WD_COLUMN/
#        WD_SESSION/WD_ROLE 环境变量覆盖；报警目标固定 controller）
# CLI 探测: AITEAM_BIN 环境变量 → ~/.aiteam/aiteam(.exe) → PATH
# 注: 判定用 grep 匹配 status --json 紧凑输出的 "alive":false 字面（同版本
#   二进制字段顺序稳定；升级若变更输出结构需同步本脚本判定行）。
# =============================================================================
set -u

INTERVAL="${1:-5m}"
if command -v "${AITEAM_BIN:-}" >/dev/null 2>&1; then
  BIN="$AITEAM_BIN"
elif [ -x "$HOME/.aiteam/aiteam.exe" ]; then
  BIN="$HOME/.aiteam/aiteam.exe"
elif [ -x "$HOME/.aiteam/aiteam" ]; then
  BIN="$HOME/.aiteam/aiteam"
else
  BIN="aiteam"
fi

WD_PROJECT="${WD_PROJECT:-aiteam}"
WD_COLUMN="${WD_COLUMN:-05}"
WD_SESSION="${WD_SESSION:-watchdog}"
WD_ROLE="${WD_ROLE:-executor_watchdog}"
ID_ARGS=(--project "$WD_PROJECT" --column "$WD_COLUMN" --session "$WD_SESSION" --role "$WD_ROLE")

# 时长字面（Ns/Nm/Nh，与 aiteam watch --max-wait 同款）→ 秒；解析失败回 0
dur2sec() {
  local s="${1%?}" u="${1: -1}"
  case "$u" in
    s) echo "$s" ;;
    m) echo $((s * 60)) ;;
    h) echo $((s * 3600)) ;;
    *) echo 0 ;;
  esac
}

report() { # $1=报警正文；发送失败仅 stderr 提示（需人工关注）
  "$BIN" send "${ID_ARGS[@]}" --to-role controller --level important \
    --body "watchdog 巡检报告：$1" >/dev/null 2>&1 \
    || echo "watchdog: 报警发送失败（服务不可达？）: $1" >&2
}

echo "watchdog 启动：间隔=$INTERVAL 身份=$WD_SESSION@$WD_ROLE（常绿条件=间隔<心跳阈值，见头注）"
declare -A REPORTED=()   # 已报异常签名（去重；恢复健康即清）

# 启动自检：间隔过大会让自身心跳老化标红（常绿语义破坏）——阈值取自服务端
# config，间隔 ≥ 阈值/2 即警告（留半阈值作心跳老化余量）；服务不可达时跳过
# 自检（首轮巡检的 service-unreachable 报警会覆盖此场景）。
INTERVAL_SEC="$(dur2sec "$INTERVAL")"
BOOT_JSON="$("$BIN" status --global "${ID_ARGS[@]}" --json 2>/dev/null)"
if [ -n "$BOOT_JSON" ] && [ "$INTERVAL_SEC" -gt 0 ]; then
  HB="$(printf '%s' "$BOOT_JSON" | grep -o '"heartbeat_timeout_sec":[0-9]*' | head -1 | grep -o '[0-9]*$')"
  if [ -n "$HB" ] && [ "$INTERVAL_SEC" -ge $((HB / 2)) ]; then
    echo "watchdog 警告：间隔 ${INTERVAL}（${INTERVAL_SEC}s）≥ 心跳阈值 ${HB}s 的一半——sleep 后半段看板将标红（常绿语义破坏），建议 ≤$((HB / 3))s" >&2
  fi
fi

while true; do
  sleep "$INTERVAL"

  out="$("$BIN" status --global "${ID_ARGS[@]}" --json 2>/dev/null)"
  rc=$?
  if [ "$rc" -ne 0 ] || [ -z "$out" ]; then
    sig="service-unreachable"
    if [ -z "${REPORTED[$sig]:-}" ]; then
      report "服务不可达（status 退出码 $rc）——请核实 serve 进程"
      REPORTED[$sig]=1
    fi
    continue
  fi

  lost="$(printf '%s' "$out" | grep -o '"alive":false' | wc -l)"
  if [ "$lost" -gt 0 ]; then
    sig="sessions-lost"
    if [ -z "${REPORTED[$sig]:-}" ]; then
      report "$lost 个会话心跳失联（阈值=服务端 heartbeat_timeout_sec；失联≠死亡，请 status --global 核实是待命闲置还是窗口已死）"
      REPORTED[$sig]=1
    fi
  else
    REPORTED[sessions-lost]=""
    REPORTED[service-unreachable]=""
  fi
done
