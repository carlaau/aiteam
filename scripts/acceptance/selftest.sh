#!/usr/bin/env bash
# =============================================================================
# aiteam 验收脚手自测 selftest.sh —— 验证 common.sh 脚手本身+版本注入双形态
# （b7b-1 完成判据：common.sh 自测过；版本注入双形态断言过）。
#
# 判据（b7b-plan b7b-1 原文）：
#   [1] 起停干净：起→ping 就绪→停→无残留进程与临时目录
#   [2] 端口冲突：2a 前置探测生效（被占口跳过换口）+2b 探测失效→serve 真以被占口
#       启动→bind 失败即死→死亡检测分支换口兜底（审查 Important 2 真测）
#   [3] CLI 封装透传：--server 显式+身份四参+--token 可选（fake bin 参数逐项断言，
#       含 ai_send 快捷封装）
#   [4] 版本注入双形态：VER=v9.9.9-test 构建→`aiteam version`+#30 输出注入值；
#       裸 go build→输出 dev 兜底
#   [5] 追杀表回归：起两实例不 stop→cleanup→目录/进程全消失
#
# 用法：bash scripts/acceptance/selftest.sh（双平台：Git Bash / Linux bash）
# =============================================================================
cd "$(dirname "$0")/../.." || exit 1

# shellcheck source=common.sh
source scripts/acceptance/common.sh

echo "==> [1/5] 起停干净（起→ping 就绪→停→无残留进程与临时目录）"
if start_server; then
  if curl -fsS -o /dev/null "$AIT_URL/api/v1/ping"; then
    pass "ping 就绪（$AIT_URL）"
  else
    fail "start_server 返回成功但 ping 不通（$AIT_URL）"
  fi
  pid1="$AIT_PID" tmp1="$AIT_TMPDIR"
  stop_server
  if kill -0 "$pid1" 2>/dev/null; then
    fail "stop_server 后进程仍存活 pid=$pid1"
  else
    pass "stop_server 后进程消失 pid=$pid1"
  fi
  if [ -d "$tmp1" ]; then
    fail "stop_server 后临时目录残留 $tmp1"
  else
    pass "stop_server 后临时目录已删 $tmp1"
  fi
else
  fail "start_server 起服务失败"
fi

echo "==> [2/5] 端口冲突（2a 前置探测生效 / 2b 探测失效→bind 失败死亡检测换口）"
if start_server; then
  pid_a="$AIT_PID" tmp_a="$AIT_TMPDIR" url_a="$AIT_URL" port_a="${AIT_URL##*:}"
  # 2a 前置探测生效路径：hint 指向被占口→探测跳过→换随机口就绪
  if start_server "$port_a"; then
    port_b="${AIT_URL##*:}"
    if [ "$port_b" != "$port_a" ] && curl -fsS -o /dev/null "$AIT_URL/api/v1/ping"; then
      pass "2a 前置探测：被占口 $port_a 跳过，B 就绪于 $port_b"
    else
      fail "2a 端口冲突重试异常：port_b=$port_b port_a=$port_a"
    fi
    if kill -0 "$pid_a" 2>/dev/null; then
      pass "2a 占位实例 A 未被重试波及（pid=$pid_a 仍活）"
    else
      fail "2a 占位实例 A 意外死亡 pid=$pid_a"
    fi
    stop_server # 停 B（当前实例）
  else
    fail "2a hint 指向被占端口时 start_server 未成功换口"
    stop_server # 尽力收 A
  fi
  # 2b 死亡检测路径（审查 Important 2）：override ait_port_taken 恒报「未占用」+
  # AITEAM_BIN 换 flaky wrapper（listen 端口==被占口时按真实报错文案模拟 bind 失败
  # 即死，其余调用透传真二进制）。端口被占行为不确定——bind 失败即死（审查反证
  # 形态）与双 bind 成功（本机实测形态）均存在，wrapper 把前者钉成确定形态，
  # 确定性覆盖「serve 死→死亡检测→换口恢复」分支。
  eval "_real_ait_port_taken() $(declare -f ait_port_taken | tail -n +2)"
  ait_port_taken() { return 1; }
  flaky_dir="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-flaky.XXXXXX")" \
    || { echo "FAIL: mktemp -d 创建 flaky 目录失败" >&2; exit 1; }
  REAL_BIN="$AIT_BIN" # 节1 已默认构建的真二进制
  export REAL_BIN BAD_PORT="$port_a"
  cat > "$flaky_dir/flakybin" <<'EOF'
#!/usr/bin/env bash
# flaky wrapper：--config 的 listen 端口==BAD_PORT 时按真实 serve 报错文案模拟
# bind 失败即死（exit 1），其余调用透传 $REAL_BIN。
cfg=""; prev=""
for a in "$@"; do [ "$prev" = "--config" ] && cfg="$a"; prev="$a"; done
port="$(sed -n 's/.*127\.0\.0\.1:\([0-9]*\).*/\1/p' "$cfg")"
if [ "$port" = "$BAD_PORT" ]; then
  echo "aiteam serve: 监听 127.0.0.1:$port 失败: listen tcp 127.0.0.1:$port: bind: Only one usage of each socket address (protocol/network address/port may be specified)." >&2
  exit 1
fi
exec "$REAL_BIN" "$@"
EOF
  chmod +x "$flaky_dir/flakybin"
  AITEAM_BIN="$flaky_dir/flakybin" # 显式赋值（Minor 6 同口径）
  errlog="$(mktemp "${TMPDIR:-/tmp}/aiteam-errlog.XXXXXX")" \
    || { echo "FAIL: mktemp 创建 errlog 失败" >&2; exit 1; }
  if start_server "$port_a" 2> "$errlog"; then
    port_c="${AIT_URL##*:}"
    if [ "$port_c" != "$port_a" ]; then
      pass "2b 死亡检测：bind 失败即死后换口就绪（$port_a→$port_c）"
    else
      fail "2b 未换口：port_c=$port_c 与被占口相同"
    fi
    if grep -q "WARN: serve 未就绪即退出" "$errlog" && grep -q "监听.*失败" "$errlog"; then
      pass "2b serve.log bind 失败痕迹在案（WARN+监听失败经 stderr 捕获）"
    else
      fail "2b 死亡检测痕迹缺失：$(head -3 "$errlog" | tr '\n' ' ')"
    fi
    stop_server
  else
    fail "2b 探测失效场景 start_server 未通过死亡检测换口恢复"
  fi
  AIT_BIN="$REAL_BIN" # 恢复缓存指向真二进制（防 wrapper 带病复用污染后续节）
  unset AITEAM_BIN REAL_BIN BAD_PORT
  rm -rf "$flaky_dir"
  unset -f ait_port_taken # 恢复真探测函数（先删假再从备份重建）
  eval "ait_port_taken() $(declare -f _real_ait_port_taken | tail -n +2)"
  unset -f _real_ait_port_taken
  rm -f "$errlog"
  # 白盒切回 A 实例收尾（selftest 允许白盒：验证 A 也能完整干净停掉）
  AIT_PID="$pid_a" AIT_TMPDIR="$tmp_a" AIT_URL="$url_a"
  stop_server
else
  fail "端口冲突用例：占位实例 A 起服务失败"
fi

echo "==> [3/5] CLI 封装透传（--server 显式+身份四参+--token 可选+ai_send 快捷）"
fake_dir="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-fake.XXXXXX")" \
  || { echo "FAIL: mktemp -d 创建 fake 目录失败" >&2; exit 1; }
FAKE="$fake_dir/fakebin"
export FAKE_ARGS="$fake_dir/args.txt"
printf '#!/usr/bin/env bash\nprintf '"'"'%%s\\n'"'"' "$@" > "$FAKE_ARGS"\n' > "$FAKE"
chmod +x "$FAKE"
AITEAM_BIN="$FAKE" # 显式赋值（审查 Minor 6：不用前缀赋值，消 bash 版本语义差异）
AITEAM_TOKEN="t-abc"
ai_cli http://127.0.0.1:41000 proj-x 05 sess-1 foreman send --body hello
# 参数序=子命令 + 连接/身份组（--server/身份四参/--token）+ 透传参数
want=$'send\n--server\nhttp://127.0.0.1:41000\n--project\nproj-x\n--column\n05\n--session\nsess-1\n--role\nforeman\n--token\nt-abc\n--body\nhello'
if [ "$(cat "$FAKE_ARGS")" = "$want" ]; then
  pass "ai_cli 参数展开逐项精确（子命令/身份四参/--server/--token/透传）"
else
  fail "ai_cli 参数展开不符：got=$(cat "$FAKE_ARGS" | tr '\n' ' ')"
fi
ai_send http://127.0.0.1:41000 proj-x 05 sess-1 foreman --body hi --level block
want_send=$'send\n--server\nhttp://127.0.0.1:41000\n--project\nproj-x\n--column\n05\n--session\nsess-1\n--role\nforeman\n--token\nt-abc\n--body\nhi\n--level\nblock'
if [ "$(cat "$FAKE_ARGS")" = "$want_send" ]; then
  pass "ai_send 透传精确（子命令 send 注入+剩余参数原样，b7b-2 高频依赖）"
else
  fail "ai_send 透传不符：got=$(cat "$FAKE_ARGS" | tr '\n' ' ')"
fi
unset AITEAM_TOKEN
ai_cli http://127.0.0.1:41000 proj-x 05 sess-1 foreman poll
if grep -qx -- "--server" "$FAKE_ARGS" && ! grep -qx -- "--token" "$FAKE_ARGS"; then
  pass "AITEAM_TOKEN 未设时不追加 --token（可选语义）"
else
  fail "--token 可选语义异常：args=$(tr '\n' ' ' < "$FAKE_ARGS")"
fi
unset AITEAM_BIN # 对称 unset（审查 Minor 6）
rm -rf "$fake_dir"

echo "==> [4/5] 版本注入双形态（注入=v9.9.9-test / 裸构建=dev）"
ver_log="$(mktemp "${TMPDIR:-/tmp}/aiteam-buildlog.XXXXXX")" \
  || { echo "FAIL: mktemp 创建构建日志失败" >&2; exit 1; }
if VER=v9.9.9-test bash scripts/build.sh > "$ver_log" 2>&1; then
  # 本机只可执行宿主平台产物（windows exe 判据原文口径；Linux 侧等价换 linux 产物）。
  # 判宿主用 $OSTYPE 非 go env GOOS（审查 Minor 7：GOOS 常驻交叉环境会误判）
  case "$OSTYPE" in
    msys*|cygwin*) INJ_BIN="dist/aiteam-windows-amd64.exe" ;;
    *) INJ_BIN="dist/aiteam-linux-amd64" ;;
  esac
  if out="$("$INJ_BIN" version)" && [ "$out" = "aiteam version v9.9.9-test" ]; then
    pass "注入构建 version 输出：$out"
  else
    fail "注入构建 version 输出不符：got=${out:-<空>}"
  fi
  AITEAM_BIN="$INJ_BIN" # 显式赋值（审查 Minor 6）
  if start_server; then
    v30="$(curl -fsS "$AIT_URL/api/v1/version" 2>/dev/null || true)"
    if grep -q "v9.9.9-test" <<<"$v30"; then
      pass "#30 /api/v1/version 输出注入值：$v30"
    else
      fail "#30 未输出注入值：got=${v30:-<空>}"
    fi
    stop_server
  else
    fail "注入产物起 serve 失败（#30 断言跳过）"
  fi
  unset AITEAM_BIN
  # dist 收尾（审查 Important 1）：自测构建已用 v9.9.9-test 覆盖 dist/ 发布产物，
  # rm 测试产物防误用；dist/ 不入 git，发布前重跑 scripts/build.sh 即恢复
  rm -f dist/aiteam-windows-amd64.exe dist/aiteam-linux-amd64
  echo "提示: dist 产物已被自测覆盖，已清除；发布前重跑 scripts/build.sh" >&2
else
  fail "VER=v9.9.9-test bash scripts/build.sh 失败，日志："
  cat "$ver_log" >&2
fi
rm -f "$ver_log"

plain_dir="$(mktemp -d "${TMPDIR:-/tmp}/aiteam-plain.XXXXXX")" \
  || { echo "FAIL: mktemp -d 创建裸构建目录失败" >&2; exit 1; }
plain_ext=""
case "$OSTYPE" in msys*|cygwin*) plain_ext=".exe" ;; esac # 宿主平台后缀（Minor 7 同口径）
if go build -o "$plain_dir/aiteam$plain_ext" ./cmd/aiteam; then
  out2="$("$plain_dir/aiteam$plain_ext" version)"
  if [ "$out2" = "aiteam version dev" ]; then
    pass "裸 go build version 输出 dev 兜底：$out2"
  else
    fail "裸 go build version 输出不符：got=$out2"
  fi
else
  fail "裸 go build ./cmd/aiteam 失败"
fi
rm -rf "$plain_dir"

echo "==> [5/5] 追杀表回归（起两实例不 stop→cleanup→目录/进程全消失）"
if start_server && start_server; then
  n=${#AIT_ALL_PIDS[@]}
  p1="${AIT_ALL_PIDS[n - 2]}" d1="${AIT_ALL_TMPDIRS[n - 2]}"
  p2="${AIT_ALL_PIDS[n - 1]}" d2="${AIT_ALL_TMPDIRS[n - 1]}"
  ait_cleanup # 显式调（EXIT trap 二次触发时追杀表已清，幂等空转）
  # Windows 进程死亡/文件句柄释放异步，等稳再断言目录
  ait_wait_gone "$p1" 10 || true
  ait_wait_gone "$p2" 10 || true
  if [ ! -d "$d1" ] && [ ! -d "$d2" ]; then
    pass "追杀表兜底：两实例临时目录全消失"
  else
    fail "追杀表兜底目录残留：d1=$d1 d2=$d2"
  fi
  if kill -0 "$p1" 2>/dev/null || kill -0 "$p2" 2>/dev/null; then
    fail "追杀表兜底进程残留：p1=$p1 p2=$p2"
  else
    pass "追杀表兜底：两实例进程全消失"
  fi
else
  fail "追杀表用例：start_server 失败"
fi

summary
