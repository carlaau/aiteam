XXX#!/usr/bin/env bash
# setup-skills.sh —— 方法论技能包手动安装（类 Unix symlink 版；init 默认装 .agents/skills/
# 之后的定位=手动链接/重装工具：把仓内技能逐个链接到目标项目的 .claude/skills/）。
# 源在 internal/assets/skills/（embed 同源——单二进制用户走 aiteam init 即可，无需本脚本）。
# 技能是各契约/启动指令中「以 Skill 工具加载 XX」的实体；不装则客户端 Skill 工具无法发现。
#
# 用法: bash setup-skills.sh [--target <目标项目根>]
#   （本仓 sh 按惯例不带可执行位，用 bash 显式调用；已 chmod +x 时亦可 ./setup-skills.sh）
#   --target 缺省 = 当前工作目录（在哪个项目根下跑就装到哪）
#   源目录自动定位本脚本所在仓的 skills/，与执行时 cwd 无关
#
# 幂等语义（重复跑零副作用）:
#   目标已是指向源的正确链接 → 跳过
#   链接存在但指向错         → 删旧建新（修复）
#   同路径已是普通目录/文件   → 该技能报错（先备份再删除该目录后重跑），其余技能继续，最终退出码非零
#
# 退出码: 0=全部成功（含跳过/修复）  1=有报错（源缺失/源零技能/目标冲突）  2=参数错误
#
# Windows Git Bash: MSYS 的 ln -s 默认退化为拷贝（产出非链接），本脚本自动回滚改用 NTFS
#   junction 兜底（与 setup-skills.ps1 产物一致，无需管理员权限）；原生 Linux/macOS 一律 ln -s。
# 其他客户端: zcode 等客户端把同样的 junction/symlink 建到其自己的技能目录即可（目录路径见各自文档）。
# 非技能机制客户端: SKILL.md 本质是 markdown 指令文件，没有 Skill 工具的客户端可把其内容当 prompt 模板手动粘贴使用。
set -u

# ---- 参数解析 ----
TARGET=""
while [ $# -gt 0 ]; do
  case "$1" in
    --target)
      [ $# -ge 2 ] || { echo "[报错] --target 缺参数（用法: $0 [--target <目标项目根>]）"; exit 2; }
      TARGET="$2"; shift 2 ;;
    --target=*)
      TARGET="${1#--target=}"; shift ;;
    -h|--help)
      echo "用法: $0 [--target <目标项目根>]"
      echo "  把本仓 skills/ 逐技能链接到 <目标项目根>/.claude/skills/（缺省安装到当前工作目录）"
      echo "  幂等：正确链接跳过 / 指错修复 / 普通目录报错（先备份再删除后重跑）"
      exit 0 ;;
    *)
      echo "[报错] 未知参数: $1（用法: $0 [--target <目标项目根>]）"; exit 2 ;;
  esac
done

# ---- 源定位（脚本所在仓的 internal/assets/skills/，与 cwd 无关）----
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 1
SRC="$(cd "$SCRIPT_DIR/../internal/assets/skills" 2>/dev/null && pwd)"
if [ -z "$SRC" ]; then
  echo "[报错] 源目录不存在: $SCRIPT_DIR/../internal/assets/skills"
  exit 1
fi

# ---- 目标解析（缺省 cwd）----
TARGET="${TARGET:-$PWD}"
if [ ! -d "$TARGET" ]; then
  echo "[报错] 目标项目根不存在: $TARGET"
  exit 1
fi
TARGET="$(cd "$TARGET" && pwd)"
SKILLS_DIR="$TARGET/.claude/skills"

# ---- Windows 判定（MSYS junction 兜底 + 路径大小写不敏感比较）----
IS_WIN=0
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*|*_NT-*) IS_WIN=1 ;; esac

# 建链接: create_link <dest> <src>，0=成功
# 先 ln -s 并自检产出确为链接；MSYS 下 ln -s 退化为拷贝 → 回滚后仅 Windows 改用 junction
create_link() {
  _dest="$1"; _src="$2"
  if ln -s "$_src" "$_dest" 2>/dev/null && [ -L "$_dest" ]; then
    return 0
  fi
  rm -rf "$_dest" 2>/dev/null
  if [ "$IS_WIN" = "1" ]; then
    cmd //c mklink //J "$(cygpath -w "$_dest")" "$(cygpath -w "$_src")" >/dev/null 2>&1 &&
      [ -L "$_dest" ] && return 0
  fi
  return 1
}

# 链接指向比较: link_targets_equal <readlink结果> <期望源路径>（去尾斜杠；Windows 不区分大小写）
link_targets_equal() {
  _lt="${1%/}"; _want="${2%/}"
  if [ "$IS_WIN" = "1" ]; then
    [ "$(printf '%s' "$_lt" | tr 'A-Z' 'a-z')" = "$(printf '%s' "$_want" | tr 'A-Z' 'a-z')" ]
  else
    [ "$_lt" = "$_want" ]
  fi
}

# ---- 技能枚举（含 SKILL.md 的子目录；源零技能=报错）----
shopt -s nullglob
_count=0
for _d in "$SRC"/*/; do
  [ -f "${_d}SKILL.md" ] && _count=$((_count + 1))
done
if [ "$_count" -eq 0 ]; then
  echo "[报错] 源 skills/ 下没有任何含 SKILL.md 的技能目录: $SRC"
  exit 1
fi

echo "源: $SRC"
echo "目标: $SKILLS_DIR"

installed=0; skipped=0; fixed=0; failed=0
for d in "$SRC"/*/; do
  name="$(basename "$d")"
  [ -f "${d}SKILL.md" ] || continue
  src="${d%/}"  # 去尾斜杠：链接目标不带斜杠，避免 MSYS 显示/比较歧义
  dest="$SKILLS_DIR/$name"
  if [ -L "$dest" ]; then
    # 链接已存在（含悬空链接，-L 不跟随目标）→ 按指向分流
    lt="$(readlink "$dest")"
    if [ -n "$lt" ] && link_targets_equal "$lt" "$src"; then
      echo "[跳过] $name"
      skipped=$((skipped + 1))
    else
      rm -f "$dest"  # rm 对链接只删链接本身，不碰目标内容
      if create_link "$dest" "$src"; then
        echo "[修复] $name"
        fixed=$((fixed + 1))
      else
        echo "[报错] $name —— 重建链接失败"
        failed=$((failed + 1))
      fi
    fi
  elif [ -e "$dest" ]; then
    echo "[报错] $name —— 目标已存在且不是链接: $dest（先备份再删除该目录，然后重跑本脚本）"
    failed=$((failed + 1))
  else
    # 安装
    [ -d "$SKILLS_DIR" ] || mkdir -p "$SKILLS_DIR"
    if create_link "$dest" "$src"; then
      echo "[安装] $name"
      installed=$((installed + 1))
    else
      echo "[报错] $name —— 建链接失败（ln -s 与 junction 兜底均未成功）"
      failed=$((failed + 1))
    fi
  fi
done
shopt -u nullglob

echo "汇总：安装 $installed / 跳过 $skipped / 修复 $fixed / 报错 $failed"
[ "$failed" -eq 0 ] || exit 1
exit 0
