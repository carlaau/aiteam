#!/usr/bin/env bash
# =============================================================================
# aiteam AC16.2 验收：serve 进程 kill -9 非正常终止 → 同 db.path 重启 →
# 数据零丢失 + WAL 自愈无报错。
#
# 判据（b7b-plan b7b-3 AC16.2）：
#   [1] 自举夹具+send N=9 条：项目走 #1（唯一豁免身份四头的系统动作端点，空库
#       自举引导）；栏目走外部 SQLite 连接 seed——HTTP #5 建栏目走完整心跳链，
#       身份四头必须指向已登记栏目（空库无一栏目时 404 column_not_found，实测
#       复现；B4 集成测试同困境走 seedDomain 直插库，非本验收修复范围）。
#       send direct/bus/chat 三形态 × normal/important/block 三级正交轮换
#       （形态=(i-1)/3、级别=(i-1)%3 独立取值，N=9 恰 3×3 九组合全覆盖；旧版
#       i%3 与 (i-1)%3 相位差恒 1 的同余绑定仅覆盖 3/9 组合，审查采纳修正），
#       默认双写镜像，--json 逐一取回 seq
#   [2] kill -9 serve 进程（真 kill 注入非 mock）：注入前进程存活 → kill -9 →
#       进程消失。平台实证（Git Bash / MSYS2）：$! 得到的 MSYS pid 由 runtime
#       映射到原生 Windows 进程，kill -9 走 TerminateProcess 强杀生效
#       （实测进程消失、无 taskkill 必要——若未来 MSYS 语义变化致强杀无效，
#       兜底方案是 taskkill //F //PID，本脚本未用到）
#   [3] 同 db.path 重启：复用同一实例目录与同一 config.json（db.path 相对文件
#       名不变），同端口 bind 成功、ping 通 + serve.log 启动行双条件就绪
#   [4] 数据零丢失：history 全量拉取（--limit 500 --order asc）——COUNT=N 且
#       seq 逐一在行首、body 逐一完整可查（WAL 未 checkpoint 数据重启后全在：
#       实测主库 4KB 空壳+WAL 全量，kill -9 后数据存活全靠 WAL 自愈回放）
#   [5] WAL 自愈无报错：重启后 serve.log 无报错痕迹（slog ERROR/WARN 级别行、
#       corrupt/malformed/locked/disk I/O 关键词零匹配）
#   [6] 断言器自证（破坏夹具演练）：再 kill -9 → 外部连接删 messages 一行
#       （append-only 触发器 messages_no_delete 禁 DELETE，演练先 DROP 后
#       DELETE 再按 schema.sql 原文还原触发器——sqlite3 CLI 本机不可用，
#       夹具用 go run 单文件微工具经 modernc.org/sqlite 直连；删一行比删整库
#       更严：验证逐 seq 断言粒度而非仅总量）→ 重启 → 同一个 check_history_full
#       断言器必须检测到缺失；若删行后仍报全量在 = 断言器盲区，fail
#
# 用法：bash scripts/acceptance/ac16-2-kill9-recovery.sh（双平台：Git Bash / Linux bash）
# 依赖：common.sh 脚手（起停/CLI 封装/断言计数/EXIT 清理全复用）；go run 的
#       module 上下文取脚本 cwd（仓库根）解析 modernc.org/sqlite 依赖
# =============================================================================
cd "$(dirname "$0")/../.." || exit 1

# shellcheck source=common.sh
source scripts/acceptance/common.sh

# ---- 用例身份（全新库自建，无预置依赖）--------------------------------------
PROJ="ac16-2-proj"
COL="a1"
SESS="ac16-2-sess"
ROLE="controller"        # 发送方角色（合法枚举 controller/executor/executor_*）
DIRECT_ROLE="executor"   # direct 形态目标角色
N=9                      # 发送条数（AC 要求 6~10；9=3 形态×3 级别九组合全覆盖）

# 夹具目录：send 镜像根（默认双写保真实链路，不污染仓库工作区）+ seed 微工具源
FIXTURE_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/ac16-2-fixture.XXXXXX")" \
  || { echo "FAIL: mktemp -d 创建夹具目录失败" >&2; exit 1; }
MIRROR_ROOT="$FIXTURE_ROOT/mirror"
mkdir -p "$MIRROR_ROOT" || exit 1
cleanup_all() {
  rm -rf "$FIXTURE_ROOT" 2>/dev/null || true
  ait_cleanup
}
trap 'cleanup_all' EXIT
trap 'exit 130' INT TERM # 信号转正常退出→触发 EXIT trap 统一清理

SEQS=()   # 各条消息服务端 seq（send --json 提取）
BODIES=() # 各条消息正文（history 输出 body 摘要逐条核对用）

# ---- seed 工具源：外部 SQLite 连接的直连夹具（seed 插栏目 / delrow 删一行）----
# cwd 在仓库根时 go run 解析 modernc.org/sqlite（项目 go.mod 既有依赖）；
# 不 import aiteam/internal（internal 跨目录禁入，且本工具面只需裸 SQL）。
cat > "$FIXTURE_ROOT/seedmain.go" <<'EOF'
package main

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	mode := os.Args[1]
	dsn := os.Args[2] + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	switch mode {
	case "seed": // seed <db> <project> <column>：只插栏目（项目走 HTTP #1 已建，防 UNIQUE 冲突）
		proj, col := os.Args[3], os.Args[4]
		if _, err = db.Exec(`INSERT INTO columns (project_id, code, name, status, created_at, updated_at)
			 VALUES ((SELECT id FROM projects WHERE code = ?), ?, ?, 'active', ?, ?)`,
			proj, col, col, now, now); err != nil {
			fail(err)
		}
	case "delrow": // delrow <db> <seq>：DROP 禁删触发器→删该行→按 schema.sql 原文还原触发器
		seq := os.Args[3]
		for _, stmt := range []string{
			`DROP TRIGGER messages_no_delete`,
			`DELETE FROM messages WHERE seq = ` + seq,
			`CREATE TRIGGER messages_no_delete BEFORE DELETE ON messages
			 BEGIN
			   SELECT RAISE(ABORT, 'messages is append-only: DELETE forbidden');
			 END`,
		} {
			if _, err = db.Exec(stmt); err != nil {
				fail(err)
			}
		}
	default:
		fmt.Fprintln(os.Stderr, "未知模式:", mode)
		os.Exit(2)
	}
	fmt.Println(mode, "OK")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
EOF

seed_fixture() { go run "$FIXTURE_ROOT/seedmain.go" seed "$@"; }
delrow_fixture() { go run "$FIXTURE_ROOT/seedmain.go" delrow "$@"; }

# ---- 前置失败快捷退出（trap 收尾，exit 1 由 summary 语义对齐）----------------
die() { fail "$*"; exit 1; }

# ---- restart_same_dir：同实例目录同 config.json 重启（同 db.path）-----------
# 复用 AIT_TMPDIR（不重建目录→db/-wal 文件原样留存）与其中 config.json（listen
# 端口/db.path 均不变）；serve.log 覆盖重写（就绪判定 grep 到的启动行即新实例
# 的）。kill -9 后端口释放即时（监听 socket 随进程死释放），仍给 3 次重试兜
# Windows 句柄释放的异步窗口。成功：AIT_PID/AIT_ALL_PIDS 更新；失败：return 1。
restart_same_dir() {
  local attempt=0
  while [ "$attempt" -lt 3 ]; do
    attempt=$((attempt + 1))
    ( cd "$AIT_TMPDIR" && exec "$AIT_BIN" serve --config config.json ) > "$AIT_TMPDIR/serve.log" 2>&1 &
    AIT_PID=$!
    AIT_ALL_PIDS+=("$AIT_PID") # 追杀表兜底（cleanup 收所有起过的实例）
    local deadline=$((SECONDS + ${AIT_START_TIMEOUT:-20}))
    while ! ait_server_ready; do
      if ! kill -0 "$AIT_PID" 2>/dev/null; then break; fi # bind 失败即死等路径
      if [ "$SECONDS" -ge "$deadline" ]; then break; fi
      sleep 0.2
    done
    if ait_server_ready; then
      echo "OK: 同 db.path 重启就绪 $AIT_URL (pid=$AIT_PID, 第 $attempt 次尝试)"
      return 0
    fi
    kill -9 "$AIT_PID" 2>/dev/null || true # 清掉半死实例再重试（防双实例抢端口）
    ait_wait_gone "$AIT_PID" 5 || true
    sleep 1
  done
  echo "FAIL: 同 db.path 重启 3 次未就绪，serve.log:" >&2
  sed 's/^/    /' "$AIT_TMPDIR/serve.log" >&2
  return 1
}

# ---- check_history_full：断言器（数据零丢失检测，判据 [4]，主节+演练节共用）----
# history 全量拉取 → COUNT=N 且 seq 逐一在行首且 body 逐一完整 → 返回 0；任一
# 不满足返回 1 并把缺失明细打到 stderr（诊断不刷屏）。演练节复用本函数：删行
# 后它必须返回 1（检测到缺失）——两节同器双向，即为断言器自证。
check_history_full() {
  local hist count s b
  hist="$(ai_cli "$AIT_URL" "$PROJ" "$COL" "$SESS" "$ROLE" history --limit 500 --order asc 2>/dev/null)" \
    || { echo "  检测: history 命令失败" >&2; return 1; }
  count="$(grep -c '^\[' <<<"$hist" || true)"
  if [ "$count" != "$N" ]; then
    echo "  检测: COUNT=$count 期望 $N" >&2
    return 1
  fi
  for s in "${SEQS[@]}"; do
    if ! grep -q "^\[$s\] " <<<"$hist"; then
      echo "  检测: seq=$s 缺失" >&2
      return 1
    fi
  done
  for b in "${BODIES[@]}"; do
    if ! grep -qF -- "$b" <<<"$hist"; then
      echo "  检测: body 缺失「$b」" >&2
      return 1
    fi
  done
  return 0
}

# =============================================================================
echo "==> [1/4] 自举夹具：起 serve + 建项目（#1 豁免端点）+ seed 栏目（外部连接）+ send $N 条"
start_server || die "start_server 起服务失败"
tmpdir="$AIT_TMPDIR" url="$AIT_URL" # 全程同一实例目录（同 db.path 载体），restart 不换

if curl -fsS -o /dev/null -X POST "$url/api/v1/projects" \
  -H 'Content-Type: application/json' \
  -d "{\"code\":\"$PROJ\",\"name\":\"AC16.2 kill -9 自愈验收项目\"}"; then
  pass "项目预登记 $PROJ（#1 系统动作豁免端点）"
else
  die "POST /api/v1/projects 失败"
fi
# 栏目 HTTP #5 走不通（判据注释 [1]：身份四头须挂已登记栏目，空库死结）——
# 外部连接 seed（serve 运行中，WAL 多连接并发合法；serve 无应用层缓存，插行即刻可见）
if seed_fixture "$tmpdir/aiteam-acc.db" "$PROJ" "$COL" > /dev/null; then
  pass "栏目 seed $PROJ/$COL（外部 SQLite 连接直插，绕 #5 空库死结）"
else
  die "seed 栏目失败（外部连接写库被拒）"
fi

for ((i = 1; i <= N; i++)); do
  # 形态×级别正交轮换：kind=(i-1)/3 与 level=(i-1)%3 独立取值，N=9 恰 3×3
  # 笛卡尔积全覆盖（direct×3 级 / bus×3 级 / chat×3 级），无同余相位绑定。
  case $(((i - 1) / 3)) in
    0) target=(--to-role "$DIRECT_ROLE") kind="direct" ;;
    1) target=(--bus) kind="bus" ;;
    2) target=(--to-session "$SESS") kind="chat" ;; # 自身会话（此前 send 已用心跳注册）
  esac
  case $(((i - 1) % 3)) in
    0) level="normal" ;;
    1) level="important" ;;
    2) level="block" ;;
  esac
  body="ac16-2 第${i}条 ${kind} 消息"
  out="$(ai_send "$url" "$PROJ" "$COL" "$SESS" "$ROLE" "${target[@]}" \
    --level "$level" --body "$body" --mirror-root "$MIRROR_ROOT" --json)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    die "send 第 $i 条（$kind/$level）退出码 $rc 非 0：$out"
  fi
  seq="$(sed -n 's/.*"seq":\([0-9][0-9]*\).*/\1/p' <<<"$out")"
  if [ -z "$seq" ] || [ "$seq" = "0" ]; then
    die "send 第 $i 条 --json 输出无有效 seq：$out"
  fi
  SEQS+=("$seq")
  BODIES+=("$body")
  pass "send[$i] $kind/$level seq=$seq"
done

# =============================================================================
echo "==> [2/4] kill -9 注入（真 kill 非 mock）"
victim="$AIT_PID"
if kill -0 "$victim" 2>/dev/null; then
  pass "注入前 serve 进程存活 pid=$victim"
else
  die "注入前 serve 进程已意外死亡 pid=$victim"
fi
kill -9 "$victim" 2>/dev/null || true
wait "$victim" 2>/dev/null || true # reap 后台作业，抑制 bash「Killed」通知
if ait_wait_gone "$victim" 10; then
  pass "kill -9 后 serve 进程消失 pid=$victim"
else
  die "kill -9 后 serve 进程 10s 仍存活（MSYS 强杀失效，需 taskkill //F 兜底）pid=$victim"
fi
if [ -f "$tmpdir/aiteam-acc.db-wal" ]; then
  pass "kill -9 后 WAL 文件留存（非正常终止未 checkpoint，数据自愈载体在）: $(ls "$tmpdir" | tr '\n' ' ')"
else
  echo "WARN: 实例目录未见 -wal 文件（数据可能已 checkpoint 进主库，自愈路径退化但判据不变）" >&2
fi

# =============================================================================
echo "==> [3/4] 同 db.path 重启 + 数据零丢失 + WAL 自愈无报错"
AIT_TMPDIR="$tmpdir" # restart_same_dir 复用同一实例目录（db/-wal 原样留存）
restart_same_dir || die "同 db.path 重启失败"
if [ "$AIT_TMPDIR" = "$tmpdir" ] && [ "$AIT_PID" != "$victim" ]; then
  pass "重启在原实例目录（db.path 未变），新进程 pid=$AIT_PID ≠ 被杀 $victim"
else
  fail "重启实例目录/进程不符：tmpdir=$AIT_TMPDIR 期望 $tmpdir"
fi
if check_history_full; then
  pass "数据零丢失：history COUNT=$N 且 seq 逐一在且 body 逐一可查（WAL 自愈回放）"
else
  fail "kill -9 重启后数据丢失（check_history_full 检测明细见上）"
fi
if grep -Eqi '\b(ERROR|WARN)\b|corrupt|malform|locked|disk i/o' "$tmpdir/serve.log"; then
  fail "重启 serve.log 有 WAL/数据库报错痕迹："
  sed 's/^/    /' "$tmpdir/serve.log" >&2
else
  pass "WAL 自愈无报错：重启 serve.log 无 ERROR/WARN 级别行与 corrupt/locked 等关键词"
fi

# =============================================================================
echo "==> [4/4] 断言器自证：破坏夹具演练（再 kill -9 → 删 messages 一行 → 重启 → 须报缺失）"
victim2="$AIT_PID"
kill -9 "$victim2" 2>/dev/null || true
wait "$victim2" 2>/dev/null || true # reap + 等句柄释放（Windows 外部连接防占用）
ait_wait_gone "$victim2" 10 || die "演练 kill -9 后进程仍存活 pid=$victim2"
# 删首条消息一行（验证逐 seq 粒度）：微工具 DROP 禁删触发器→DELETE→还原触发器。
# 顺带佐证 kill -9 后数据仍完整可外部读改（WAL recovery 对外部连接同样生效）。
if delrow_fixture "$tmpdir/aiteam-acc.db" "${SEQS[0]}" > /dev/null; then
  pass "破坏夹具：messages seq=${SEQS[0]} 一行已删（触发器已还原）"
else
  die "删行失败（破坏夹具未生效）"
fi
restart_same_dir || die "删行后重启失败"
if check_history_full; then
  fail "断言器盲区：删行后 history 仍报全量 $N 条在（演练未生效，判据 [4] 不可信）"
else
  pass "断言器自证：删行后 check_history_full 正确检测到数据缺失（检测明细见上）"
fi

summary
