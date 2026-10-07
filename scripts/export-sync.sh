#!/usr/bin/env bash
# =============================================================================
# export-sync.sh —— 开发仓→发布快照机械同步闸（纯拷贝+清单外件人裁）
# =============================================================================
# 用途: 以 dev 仓 git ls-files 为清单（剔除禁入面），纯拷贝同名件到快照（覆盖），
#   消灭两仓同名文件的人工漂移——终态目标=两仓 tracked 同名件逐字一致零特例。
# 职责边界: 只做存在性与内容同步，不做内容级转换（逐字拷贝）；内容级私有字面
#   扫描归 scripts/check-sanitize.sh，快照结构校验归 scripts/export-check.sh
#   （同步后跑它收口）。禁入面（planning 根级实况/渲染件、dev-guide、mirror
#   运行态）不入拷贝清单——与 export-check FORBIDDEN 同源口径，改任一侧须同步
#   另一侧；快照侧若残留禁入件则落清单外报告（exit 1 由人裁），脚本不越权删除。
# 用法: scripts/export-sync.sh <dev根> <快照根>
#   dev根     开发仓根（git 工作树，清单来源）
#   快照根    发布快照仓根（同步目标）
# 退出码: 0=同步完成 | 1=快照侧存在清单外文件（已列报告，由人裁） | 2=参数错误
# =============================================================================
set -u
export LC_ALL=C   # 字节语义：路径比较与排序跨语环境稳定一致

# ---------- 禁入面（不进拷贝清单；与 export-check.sh FORBIDDEN 同源） ----------
# docs/planning/ 采口径式排除：仅 _template/ 豁免，根级实况/渲染件（红绿灯/
#   维护台账/iterations 实录/CONTEXT 等）一律不携带——快照定案=planning 仅 _template。
EXCLUDE_RES=(
  "docs/dev-guide"                    # dev 开发纪律（快照无此层，纪律随 _template/dev-guide-base 播种）
  ".aiteam/mirror-"                   # send 镜像审计行=本地运行态
)
is_excluded() { # $1=相对 dev 根路径；前缀命中即排除（整族靠前缀覆盖）
  local rel="$1" p
  case "$rel" in
    docs/planning/_template|docs/planning/_template/*) return 1 ;;  # 唯一豁免
    docs/planning/*) return 0 ;;
  esac
  for p in "${EXCLUDE_RES[@]}"; do
    case "$rel" in "$p"*) return 0 ;; esac
  done
  return 1
}

usage() {
  # 打印头注释块（自第 2 行起逐行打印 # 行，遇首个非注释代码行即止——对行号漂移免疫）
  awk 'NR<2{next} /^#/{print; next} {exit}' "$0" | sed 's/^# \{0,1\}//'
}
die2() { echo "参数错误: $1" >&2; echo "用法: $0 <dev根> <快照根>" >&2; exit 2; }

[ $# -eq 2 ] || die2 "须恰好两个参数: <dev根> <快照根>"
DEV_ROOT="$1"
SNAP_ROOT="$2"
[ -d "$DEV_ROOT" ] || die2 "dev 根不存在: $DEV_ROOT"
[ -d "$SNAP_ROOT" ] || die2 "快照根不存在: $SNAP_ROOT"
DEV_ROOT="$(cd "$DEV_ROOT" && pwd -P)"
SNAP_ROOT="$(cd "$SNAP_ROOT" && pwd -P)"
git -C "$DEV_ROOT" rev-parse --git-dir >/dev/null 2>&1 || die2 "dev 根不是 git 工作树: $DEV_ROOT"

# ---- 导出清单：dev git ls-files 剔除禁入面 -----------------------------------
declare -A EXPORT_LIST=()
excluded=0
while IFS= read -r f; do
  if is_excluded "$f"; then
    excluded=$((excluded+1))
    continue
  fi
  EXPORT_LIST["$f"]=1
done < <(git -C "$DEV_ROOT" -c core.quotepath=off ls-files)

# ---- 拷贝面：清单内同名件纯拷贝（覆盖），已一致件跳过 ------------------------
changed=0
same=0
while IFS= read -r f; do
  src="$DEV_ROOT/$f"
  dst="$SNAP_ROOT/$f"
  if [ ! -f "$src" ]; then
    echo "警告: 清单内但 dev 工作树缺失（疑删除未提交）: $f" >&2
    continue
  fi
  if [ -f "$dst" ] && cmp -s "$src" "$dst"; then
    same=$((same+1))
    continue
  fi
  mkdir -p "$(dirname "$dst")"
  cp "$src" "$dst"
  changed=$((changed+1))
done < <(printf '%s\n' "${!EXPORT_LIST[@]}" | sort)

# ---- 清单外面：快照侧存在但导出清单没有 → 仅报告，由人裁 --------------------
extra=0
while IFS= read -r f; do
  rel="${f#"$SNAP_ROOT"/}"
  [ -n "${EXPORT_LIST["$rel"]:-}" ] && continue
  echo "清单外文件（不自动删，由人裁）: $rel"
  extra=$((extra+1))
done < <(find "$SNAP_ROOT" -type f -not -path "$SNAP_ROOT/.git/*")

echo "export-sync 完成: 内容变更拷贝 $changed 件，已一致 $same 件，禁入面剔除 $excluded 件，清单外 $extra 件"
if [ "$extra" -gt 0 ]; then
  echo "快照侧存在清单外文件 —— 人裁处理后再跑本脚本复核" >&2
  exit 1
fi
echo "下一步收口: bash scripts/export-check.sh <快照根>（结构闸）+ 双闸（check.sh + check-sanitize.sh）"
exit 0
