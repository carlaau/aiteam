#!/usr/bin/env bash
# =============================================================================
# aiteam 验收脚手 common.sh —— b7b-2/3/4 验收脚本共用的起停/CLI/断言库。
#
# 用法：验收脚本头部 `source "$(dirname "$0")/common.sh"`，随后：
#   start_server            # 起真二进制 serve（go build 产物，非 go run——验收对象即发布物）
#   ...curl "$AIT_URL/api/v1/ping"... / ai_send "$AIT_URL" P C S R --body hi
#   stop_server             # 停 serve+等进程消失+删临时目录
#   pass "判据描述" / fail "判据描述" / summary   # 计数汇总：FAIL>0 退出 1
#
# 退出清理：source 即挂 EXIT/INT/TERM trap（ait_cleanup）——任何退出路径无残留
# 进程/临时目录；宿主如需自定义 trap，须在其清理逻辑中调用 ait_cleanup。
#
# 环境变量：
#   AITEAM_BIN     被测二进制路径覆盖（默认当前平台 go build ./cmd/aiteam 产物，构建一次缓存复用）
#   AITEAM_TOKEN   设置时 CLI 封装自动追加 --token 显式传参（不依赖 cli.json——脚本环境自洽）
#   AIT_START_RETRIES / AIT_START_TIMEOUT / AIT_STOP_TIMEOUT  起停参数（默认 5 次 / 20s / 10s）
#
# 判据（b7b-plan b7b-1）：起停干净 + 端口无冲突。
# 风格对齐 scripts/check.sh（set -u / ==> 分节 / FAIL>&2 / OK）；双平台兼容
# （Git Bash + Linux bash：路径 /、kill、$RANDOM）。
# =============================================================================
set -u

# ---- 全局状态（AIT_ 前缀）---------------------------------------------------
AIT_WORKDIR=""   # 脚手工作区：默认构建产物缓存（首次需要时 mktemp -d 创建）
AIT_TMPDIR=""    # 当前 server 实例临时目录（config/db/serve.log），stop 时删除
AIT_BIN=""       # 被测二进制绝对路径（ait_ensure_bin 解析）
AIT_PID=""       # 当前 serve 进程 pid（多实例并发允许：AIT_* 永指最近一次 start 的实例，
                 # 历史实例 pid/目录已入追杀表由 cleanup 兜底；重启场景=stop→start）
AIT_URL=""       # 当前 server 基地址，如 http://127.0.0.1:41234
AIT_ALL_PIDS=()    # 本会话 start 过的全部 pid（cleanup 兜底追杀，防验收脚本漏 stop）
AIT_ALL_TMPDIRS=() # 对应临时目录（cleanup 兜底删除）

# ---- pass/fail 计数器（判据断言用）------------------------------------------
# 串行约束：断言须主 shell 串行调用，勿后台化/子 shell 内调用——bash 变量自增
# 非原子（并发调用丢计数）+子 shell 内计数改动不回传父进程。
AIT_PASS=0
AIT_FAIL=0
AIT_FAIL_LOG=""  # fail 即时记录明细（汇总时 stderr 再打一遍，防刷屏丢失）

pass() { AIT_PASS=$((AIT_PASS + 1)); echo "PASS: $*"; }
fail() {
  AIT_FAIL=$((AIT_FAIL + 1))
  AIT_FAIL_LOG+="FAIL[$AIT_FAIL]: $*"$'\n'
  echo "FAIL: $*" >&2
}
# summary 打印 PASS N / FAIL M；FAIL>0 时 stderr 重放明细并 exit 1（验收脚本尾部必调）。
summary() {
  echo "==> 汇总: PASS $AIT_PASS / FAIL $AIT_FAIL"
  if [ "$AIT_FAIL" -gt 0 ]; then
    printf '%s' "$AIT_FAIL_LOG" >&2
    exit 1
  fi
}

# ---- 内部工具 ---------------------------------------------------------------
# ait_wait_gone 轮询等进程消失（Git Bash/Linux kill -0 兼容）；超时返回 1。
ait_wait_gone() {
  local pid="$1" timeout="${2:-10}"
  local deadline=$((SECONDS + timeout))
  while kill -0 "$pid" 2>/dev/null; do
    [ "$SECONDS" -ge "$deadline" ] && return 1
    sleep 0.2
  done
  return 0
}

# ait_ensure_bin 解析被测二进制：AITEAM_BIN 覆盖 > 已构建缓存 > 现场构建（真二进制）。
# AITEAM_BIN 相对路径归一为绝对路径：serve 进程以临时目录为 CWD 启动，
# 相对路径会在 cd 后失效（selftest 实测坑）；归一后存在性 fail-fast（审查 Minor 3）。
# 宿主平台判 $OSTYPE 非 go env GOOS：GOOS 是工具链目标（常驻交叉环境会误判），
# OSTYPE 反映脚本宿主 shell（msys*/cygwin*=Windows 系需 .exe 后缀）。
ait_ensure_bin() {
  if [ -n "${AITEAM_BIN:-}" ]; then
    case "$AITEAM_BIN" in
      /*) AIT_BIN="$AITEAM_BIN" ;;
      *) AIT_BIN="$PWD/$AITEAM_BIN" ;;
    esac
    if [ ! -x "$AIT_BIN" ]; then
      echo "FAIL: AITEAM_BIN 指向的二进制不存在/不可执行: $AIT_BIN" >&2
      return 1
    fi
    return 0
  fi
  if [ -n "$AIT_BIN" ] && [ -s "$AIT_BIN" ]; then return 0; fi
  if [ -n "$AIT_WORKDIR" ]; then :
  else
    AIT_WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-acc-work.XXXXXX")" \
      || { echo "FAIL: mktemp -d 创建脚手工作区失败" >&2; return 1; }
  fi
  local ext=""
  case "$OSTYPE" in msys*|cygwin*) ext=".exe" ;; esac
  AIT_BIN="$AIT_WORKDIR/aiteam$ext"
  echo "==> 构建被测二进制: go build -o $AIT_BIN ./cmd/aiteam"
  go build -o "$AIT_BIN" ./cmd/aiteam || { echo "FAIL: go build ./cmd/aiteam 失败" >&2; return 1; }
}

# ait_port_taken 探测 127.0.0.1:$1 是否已被监听（bash /dev/tcp connect 探测，双平台）。
# 起前探测=省一次失败启动+兜住端口行为不确定性（bind 失败即死或双 bind 均兜住）；
# 端口被占的实测形态为 bind 失败即死（listen tcp: Only one usage of each socket
# address 退出 1）。
ait_port_taken() {
  ( exec 3<>"/dev/tcp/127.0.0.1/$1" ) 2>/dev/null && return 0
  return 1
}

# ait_server_ready 就绪判据（双条件 AND）：curl ping 通 且 本实例 serve.log 出现
# slog 启动行「aiteam 服务已启动」。仅 curl 会误判同端口占位实例（selftest 2b 实测：
# 本实例 bind 失败即死但占位者同端口服务时 ping 必通）；serve.log 启动行标识服务
# 归属，两条件合取才认定本次实例真正就绪。
ait_server_ready() {
  curl -fsS -o /dev/null "$AIT_URL/api/v1/ping" 2>/dev/null || return 1
  grep -q "aiteam 服务已启动" "$AIT_TMPDIR/serve.log" 2>/dev/null
}

# ---- start_server [port_hint] -----------------------------------------------
# 起真二进制 serve：临时目录（mktemp -d）+随机端口（$RANDOM 高位段 40000~54999——
# 避开常见服务端口与动态段上部）+占用前置探测（ait_port_taken，见上）+临时 config
# （serve 以临时目录为 CWD、config 内 db.path 用相对文件名——规避 Git Bash→原生
# 进程的路径转换坑）+ curl 轮询 /api/v1/ping 就绪等待。
# 端口冲突策略（双保险）：起前 /dev/tcp 探测=省一次失败启动+兜住端口行为不确定性
# （bind 失败即死或双 bind 均兜住）；起后死亡检测=bind 失败路径的必需兜底
# （bind 失败即死→打印 serve.log→换端口重试，耗尽 AIT_START_RETRIES 报错；
# 兼兜二进制坏等启动失败）。就绪判据用 ait_server_ready 双条件（curl+本实例
# serve.log 启动行）——同端口占位实例存活时仅 curl 会误判就绪（2b 实测）。
# port_hint 仅自测用（首选拿被占端口验证重试）。
# 成功：AIT_PID/AIT_URL/AIT_TMPDIR 就绪；失败：return 1（实例状态已清理）。
start_server() {
  ait_ensure_bin || return 1
  local hint="${1:-}"
  local attempt=0
  while [ "$attempt" -lt "${AIT_START_RETRIES:-5}" ]; do
    attempt=$((attempt + 1))
    local port
    if [ -n "$hint" ] && [ "$attempt" -eq 1 ]; then port="$hint"; else port=$((RANDOM % 15000 + 40000)); fi
    if ait_port_taken "$port"; then
      echo "WARN: 端口 $port 已被占用，跳过（第 $attempt 次）" >&2
      hint=""
      continue
    fi
    AIT_TMPDIR="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-acc.XXXXXX")" \
      || { echo "FAIL: mktemp -d 创建实例目录失败" >&2; return 1; }
    printf '{"listen":"127.0.0.1:%s","db":{"type":"sqlite","path":"aiteam-acc.db"}}\n' "$port" \
      > "$AIT_TMPDIR/config.json"
    # subshell 内 exec：pid 不变，$! 即 serve 进程；serve.log 留实例目录供失败诊断
    ( cd "$AIT_TMPDIR" && exec "$AIT_BIN" serve --config config.json ) > "$AIT_TMPDIR/serve.log" 2>&1 &
    AIT_PID=$!
    AIT_URL="http://127.0.0.1:$port"
    local deadline=$((SECONDS + ${AIT_START_TIMEOUT:-20}))
    while ! ait_server_ready; do
      # 死亡/误判判定（二选一即死）：
      #   a) 进程不在（bind 失败即死等启动失败；kill -0 在 MSYS pid 复用下有小概率假阳）；
      #   b) ping 通但本实例 serve.log 无启动行=同端口占位者在服务、本实例已死
      #      （不依赖 kill -0，规避 pid 复用假阳——selftest 2b 实测形态）。
      if ! kill -0 "$AIT_PID" 2>/dev/null \
        || { curl -fsS -o /dev/null "$AIT_URL/api/v1/ping" 2>/dev/null \
          && ! grep -q "aiteam 服务已启动" "$AIT_TMPDIR/serve.log" 2>/dev/null; }; then
        echo "WARN: serve 未就绪即退出（port=$port 第 $attempt 次），serve.log:" >&2
        sed 's/^/    /' "$AIT_TMPDIR/serve.log" >&2
        break # 端口冲突/启动失败→换端口重试
      fi
      if [ "$SECONDS" -ge "$deadline" ]; then
        echo "FAIL: ping 就绪等待超时（url=$AIT_URL）" >&2
        stop_server
        return 1
      fi
      sleep 0.2
    done
    if ait_server_ready; then
      AIT_ALL_PIDS+=("$AIT_PID")
      AIT_ALL_TMPDIRS+=("$AIT_TMPDIR")
      echo "OK: serve 就绪 $AIT_URL (pid=$AIT_PID)"
      return 0
    fi
    stop_server # 清理本次失败实例（杀残留进程+删临时目录）再重试
    hint=""     # hint 只对第一次尝试生效
  done
  echo "FAIL: start_server 重试 ${AIT_START_RETRIES:-5} 次耗尽（端口/启动持续失败）" >&2
  return 1
}

# ---- stop_server -------------------------------------------------------------
# 停 serve 并收干净：kill（优雅路径）→等进程消失（Windows 文件句柄释放需进程真死——
# B6-4 冒烟竞态坑：先删目录会撞 WAL 文件占用）→超时 kill -9 强杀→再等→删临时目录。
stop_server() {
  [ -n "$AIT_PID" ] || return 0
  local pid="$AIT_PID"
  kill "$pid" 2>/dev/null || true
  if ! ait_wait_gone "$pid" "${AIT_STOP_TIMEOUT:-10}"; then
    kill -9 "$pid" 2>/dev/null || true
    ait_wait_gone "$pid" 5 || echo "WARN: 进程 $pid 强杀后仍未见消失" >&2
  fi
  AIT_PID=""
  if [ -n "$AIT_TMPDIR" ]; then
    rm -rf "$AIT_TMPDIR" 2>/dev/null || echo "WARN: 临时目录删除失败 $AIT_TMPDIR" >&2
    AIT_TMPDIR=""
  fi
  AIT_URL=""
  # 中性表述（审查 Minor 8）：本函数在重试清理/兜底等失败语境也被调用，打 OK 有误导
  echo "serve 已停(pid=$pid)"
}

# ---- trap 清理：任何退出路径无残留进程/临时目录 --------------------------------
# 当前实例走 stop_server 完整收尾；追杀表兜底（含已 stop 的死 pid/已删目录，均幂等无害）。
ait_cleanup() {
  stop_server
  local pid dir
  for pid in "${AIT_ALL_PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
  for dir in "${AIT_ALL_TMPDIRS[@]}"; do rm -rf "$dir" 2>/dev/null || true; done
  [ -n "$AIT_WORKDIR" ] && rm -rf "$AIT_WORKDIR" 2>/dev/null || true
  AIT_WORKDIR=""
  AIT_ALL_PIDS=()   # 清追杀表：EXIT trap 二次触发（显式调 cleanup 后退出）时空转幂等
  AIT_ALL_TMPDIRS=()
  return 0
}
trap 'ait_cleanup' EXIT
trap 'exit 130' INT TERM # 信号转正常退出→触发 EXIT trap 统一清理

# ---- CLI 封装（身份四参 + --server 显式传参，脚本环境自洽不依赖 cli.json）-------
# ai_cli <server> <project> <column> <session> <role> <子命令> [参数...]
#   展开为: "$AIT_BIN" <子命令> --server S --project P --column C --session S --role R [参数...]
#   AITEAM_TOKEN 非空时追加 --token "$AITEAM_TOKEN"（显式传参优先级与 CLI 内建查找链一致）。
#   其余参数原样透传（--body/--json/--level/--to-role/... 由验收脚本按需追加）。
ai_cli() {
  if [ "$#" -lt 6 ]; then
    echo "FAIL: ai_cli 用法: ai_cli <server> <project> <column> <session> <role> <子命令> [参数...]" >&2
    return 2
  fi
  local srv="$1" proj="$2" col="$3" sess="$4" role="$5" sub="$6"
  shift 6
  ait_ensure_bin || return 1
  local args=(--server "$srv" --project "$proj" --column "$col" --session "$sess" --role "$role")
  [ -n "${AITEAM_TOKEN:-}" ] && args+=(--token "$AITEAM_TOKEN")
  "$AIT_BIN" "$sub" "${args[@]}" "$@"
}

# ai_send <server> <project> <column> <session> <role> [send 参数...] —— send 快捷封装（b7b-2 高频），
# 其余命令直接 ai_cli <...> poll|history|ack|status|watch [参数...]。
ai_send() {
  if [ "$#" -lt 5 ]; then
    echo "FAIL: ai_send 用法: ai_send <server> <project> <column> <session> <role> [send 参数...]" >&2
    return 2
  fi
  local srv="$1" proj="$2" col="$3" sess="$4" role="$5"
  shift 5
  ai_cli "$srv" "$proj" "$col" "$sess" "$role" send "$@"
}

# ---- ait_seed_domain <db路径> <项目code> <栏目code> --------------------------
# 验收夹具引导：向临时库 seed 项目+栏目各一行（b7b-2 引入，b7b-3/4 复用）。
#
# 为什么不走 CLI：心跳中间件存在性短路（middleware.go ②）挡死空库首栏目——
# project register 的身份四头指向未登记域 → 404 project_not_found；column
# register 更是已知死锁（首栏目登记命令面不可达，详见 README 撞名坑提示框）。唯二
# 豁免=POST /api/v1/projects 缺头（系统动作，测试夹具不用 curl 裸调兜）。
# 故 seed 直接写库：与 startRegTestServer（internal/cli/register_test.go）
# 的 seed 引导域夹具同语义，行面与 schema 一致（projects/columns 各恰一行，
# status=active、时间戳=UTC ISO8601）。serve 侧存在性短路每次请求实时查库，
# seed 后无需重启即生效；busy_timeout=5000（store openSQLite PRAGMA）兜住
# 与 serve 的多进程写排队。
#
# 实现形态：go run 单文件（mktemp 临时目录，用完即删，仓库零残留）+ modernc.org/sqlite
# 纯 Go 驱动（go.mod 既有依赖，零新增）；go -C <module root> run
# <module 外文件>——依赖按 -C 后的 CWD（module root）解析（Git Bash 实测），
# 因此调用方须先把 AIT_REPO_ROOT 置为 aiteam module 根（v1a/v1b 脚本头部
# cd repo root 后已满足；go build 也在此目录跑，同一上下文）。
ait_seed_domain() {
  if [ "$#" -lt 3 ]; then
    echo "FAIL: ait_seed_domain 用法: ait_seed_domain <db路径> <项目code> <栏目code>" >&2
    return 2
  fi
  local db="$1" proj="$2" col="$3"
  local seeddir
  seeddir="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-acc-seed.XXXXXX")" \
    || { echo "FAIL: mktemp -d 创建 seed 目录失败" >&2; return 1; }
  cat > "$seeddir/seed.go" <<'EOF'
package main

// aiteam 验收夹具引导（common.sh ait_seed_domain 运行时生成物，勿手改）：
// 向 sqlite 库幂等 seed 项目+栏目各一行。裸 SQL 直插（module 外文件不可导
// internal/store），行面照 store/schema.sql 的 projects/columns 定义。
import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath, proj, col := os.Args[1], os.Args[2], os.Args[3]
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed: 打开库失败:", err)
		os.Exit(1)
	}
	defer db.Close()
	now := "strftime('%Y-%m-%dT%H:%M:%SZ','now')"
	if _, err := db.Exec(`INSERT INTO projects (code, name, status, heartbeat_timeout_sec, created_at, updated_at)
		VALUES (?, 'acc-seed-project', 'active', 900, ` + now + `, ` + now + `)
		ON CONFLICT(code) DO NOTHING`, proj); err != nil {
		fmt.Fprintln(os.Stderr, "seed: 插入项目失败:", err)
		os.Exit(1)
	}
	if _, err := db.Exec(`INSERT INTO columns (project_id, code, name, status, created_at, updated_at)
		SELECT id, ?, 'acc-seed-column', 'active', ` + now + `, ` + now + ` FROM projects WHERE code = ?
		ON CONFLICT(project_id, code) DO NOTHING`, col, proj); err != nil {
		fmt.Fprintln(os.Stderr, "seed: 插入栏目失败:", err)
		os.Exit(1)
	}
	fmt.Println("seeded", proj, col)
}
EOF
  go -C "${AIT_REPO_ROOT:-$PWD}" run "$seeddir/seed.go" "$db" "$proj" "$col"
  local rc=$?
  rm -rf "$seeddir"
  [ "$rc" -eq 0 ] || echo "FAIL: ait_seed_domain seed 失败（db=$db proj=$proj col=$col）" >&2
  return $rc
}

# ait_db_path_from_config <config.json> —— 从 start_server 写的 config 抽 db.path
# （相对路径按其所在目录补全为绝对——seed/巡检要直接 open 该文件）。
ait_db_path_from_config() {
  local cfg="$1" p
  p="$(sed -n 's/.*"path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$cfg")"
  [ -n "$p" ] || return 1
  case "$p" in
    /*|[A-Za-z]:*) printf '%s\n' "$p" ;;
    *) printf '%s\n' "$(dirname "$cfg")/$p" ;;
  esac
}
