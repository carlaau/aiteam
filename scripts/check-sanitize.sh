#!/usr/bin/env bash
# =============================================================================
# check-sanitize.sh —— 全仓脱敏自查机械闸（AC20.2 机制面 / NFR4 / b7a-5）
# =============================================================================
# ★ 口径（本仓已开源发布，v0.1.0）：
#   - WHITELIST_PREFIXES 白名单已清空：常规口径即全量扫描，任何命中 = 阻塞；
#   - --strict 语义保留作等价兜底（白名单空后与常规口径同效，EXEMPT_FILES 仍豁免）。
#
# 用法: scripts/check-sanitize.sh [目录] [--strict] [--publish]
#                                   [--require-calibration] | [--selftest]
#   目录      扫描根，缺省=当前目录（建议在仓根跑）
#   --strict  忽略开发期白名单全量扫描（S7 发布收口用；EXEMPT_FILES 仍豁免，
#             理由见下——规范条文自展示属永久豁免口径）
#   --publish 发布态终扫（发布收口=用 dev 带校准副本扫导出目录）：蕴含
#             --strict+校准文件必需+「内部项目名」规则全范围扫描（不限
#             _template//skills/ 前缀——导出内任何文件携带项目名字面即被抓，
#             含导出自带的本脚本副本）
#   --require-calibration
#             校准文件缺失/为空即 FAIL（exit 4）——dev 闸防「内部项目名」
#             规则静默失效（本地校准文件不入库，误删无 git 提示）
#   --selftest 只跑规则正则双向回归自测（正例必命中/反例必不命中）即退出；
#             默认路径主扫描前也会先跑同一自测——正则与用例失同步即阻塞
# 退出码: 0=PASS | 1=有命中 | 2=参数错误 | 3=规则自测失败 | 4=校准文件缺失
#
# 规则集（命中输出 文件:行号: 内容（规则名）；同一处可被多条规则各报一次，
# 宁多报不漏报）:
#   私有IP        完整点分四段，三私网段（含 \b 边界词符；四段版本号如
#                 v2.10.3.1 属已知误报——脱敏场景宁误报不漏报）
#   私有IP示意    掩码示意写法（192.168.x.x / 10.*.*.* 类）——文档高频脱敏
#                 漏网形态，与完整四段分开报规则名
#   盘符路径      X:\ 或 X:/ 形态（前邻字符非 ASCII 字母且非 %——% 前缀豁除
#                 Go 格式串组合如 %q:\n 字面（文档引用格式动词实证误伤），
#                 真盘符路径不存在 %X:\ 形态；放过 https:// 协议；
#                 LC_ALL=C 下中文紧贴前邻也可命中）
#   用户目录      C:\Users / C:/Users 形态（用户名泄漏）
#   内部项目名    docs/planning/_template/ 与 skills/ 下已知内部项目代号字面
#                 零容忍（--publish 下全范围）；pattern 值=本地校准文件
#                 sanitize-patterns.local（脚本同目录，gitignored 不入库——
#                 每行一条 ERE 片段按 | 拼接、# 注释行跳过，轮换=编辑该文件）；
#                 文件缺失/为空=规则跳过（PASS 行注明，公开仓开箱即用）；
#                 实际项目名不写入本注释，注释自身脱敏=不展示被扫描对象
#   开发期引用    _template/ 下 mailbox/signals 信箱过渡物字面零容忍
#   待回填越界    {{待回填：…}} 双口径：s0-s7-stages.md 仅许 S5/S6/S7 节内
#                 （严格节内管辖）；其他 _template 文件的标记行须自带阶段范围
#                 说明（行文本含 S5~S7 / S5-S7 / S5～S7 字样，防 S0~S4 模板
#                 乱塞）。
#   未登记占位符  _template/ 下 {{大写下划线}} 实例须对得上 _template/README.md
#                 §三保留字登记表（含 <n> 系列展开；未登记=违规）
#   真实人名不查（无法枚举，靠 IP/盘符/用户目录规则兜底）；栏目号无法机械
#   枚举，由人审承载——均为 _template/README.md §五既定口径。
#   mirror 审计不扫: .aiteam/mirror- 前缀=send 镜像审计行（运行态数据面，
#   gitignored 不入库）——逐字审计消息正文必带各类字面，结构性跳过不扫描。
#
# 自身脱敏口径: 本脚本正文零内网 IP 完整字面（正则中的网段模式为技术必需）；
# 注释示例一律用文档保留段 192.0.2.x（TEST-NET-1）。
# =============================================================================
set -u
export LC_ALL=C   # 字节语义：\b 边界对中文紧贴写法也生效、grep 按字节匹配网段

# ---------- 开发期白名单（开源仓常态=已清空即全量扫描；机制保留供豁免复用） --
WHITELIST_PREFIXES=()
# ---------- 强制扫描前缀（优先于白名单；模板区=开源发布面必须严查） ----------
FORCE_SCAN_PREFIXES=(
  "docs/planning/_template/"   # 模板区随仓交付给外部用户，任何硬编码零容忍。
)
# ---------- 永久豁免文件（--strict 亦豁免）：规范条文自展示口径 ---------------
# ① docs/planning/_template/README.md —— §五禁残留清单条文行含 IP/盘符判据形态
#    示例、{{待回填}} 形态引用与 mailbox/signals 禁则自述，按其 §五豁免口径
#    「本 README 条文中的展示不算命中」：对其豁免 私有IP/私有IP示意/盘符路径/
#    用户目录/待回填越界/开发期引用 六规则；但 内部项目名/未登记占位符 规则对
#    其仍生效（它自身即登记表，不该藏脏字面）。
# ② scripts/check-sanitize.sh —— 脱敏脚本自身注释即规则条文展示（含判据形态
#    示例字样），与 ① 同一自展示性质；绝对路径 self-exclude 只挡正在跑的一
#    份，标准落位（scripts/check-sanitize.sh）下的其他检出版本同此豁免，防在
#    主仓根跑时把另一份脚本当普通文件扫出。
SPEC_README="docs/planning/_template/README.md"
SELF_SCRIPT="scripts/check-sanitize.sh"
EXEMPT_FILES=("$SPEC_README" "$SELF_SCRIPT")

usage() {
  # 打印头注释块（自第 2 行起逐行打印 # 行，遇首个非注释代码行即止——对行号漂移免疫）
  awk 'NR<2{next} /^#/{print; next} {exit}' "$0" | sed 's/^# \{0,1\}//'
}
die2() { echo "参数错误: $1" >&2; echo "用法: $0 [目录] [--strict]" >&2; exit 2; }

selftest_drive() { # 盘符规则双向回归（正则演进须同步用例）：0=全过 1=有失
  local ere='(^|[^A-Za-z%])[A-Za-z]:[\\/]' td="" f bad=0
  td="$(mktemp -d)" || return 1
  # 正例（必须命中——真盘符路径三形态：反斜杠/正斜杠/中文紧贴）
  printf 'build at D:\\develop\\go\\proj\n'   >"$td/pos-1.txt"
  printf 'url file C:/Users/demo/x\n'         >"$td/pos-2.txt"
  printf 'manual at F:\\doc\\a.md\n'          >"$td/pos-3.txt"
  # 反例（必须不命中——Go 格式串组合 %q:\n / %d:\n / %v:/path 与协议头）
  printf 'go fmt verb %%q:\\n literal\n'      >"$td/neg-1.txt"
  printf 'verbs %%d:\\n %%v:/path mixed\n'    >"$td/neg-2.txt"
  printf 'see https://example.com/p\n'        >"$td/neg-3.txt"
  for f in "$td"/pos-*.txt; do
    grep -qE -- "$ere" "$f" || { echo "selftest 正例未命中: $(basename "$f")"; bad=1; }
  done
  for f in "$td"/neg-*.txt; do
    grep -qE -- "$ere" "$f" && { echo "selftest 反例误命中: $(basename "$f")"; bad=1; }
  done
  rm -rf "$td"
  return "$bad"
}

TARGET=""; STRICT=0; SELFTEST=0; PUBLISH=0; REQUIRE_CALIB=0
for arg in "$@"; do
  case "$arg" in
    --strict) STRICT=1 ;;
    --publish) PUBLISH=1 ;;
    --require-calibration) REQUIRE_CALIB=1 ;;
    --selftest) SELFTEST=1 ;;
    -h|--help) usage; exit 0 ;;
    -*) die2 "未知参数 $arg" ;;
    *) [ -n "$TARGET" ] && die2 "目录参数只能给一个"; TARGET="$arg" ;;
  esac
done
[ -z "$TARGET" ] && TARGET="."
[ -d "$TARGET" ] || die2 "目录不存在: $TARGET"
TARGET="$(cd "$TARGET" && pwd -P)"
SCRIPT_SELF="$(cd "$(dirname "$0")" && pwd -P)/$(basename "$0")"

# 规则自测闸：默认路径每次先跑（正则与用例失同步即阻塞，回归不被跳过）
if ! selftest_drive; then
  echo "盘符规则自测 FAIL —— 正则与用例失同步，先修再扫" >&2
  exit 3
fi
[ "$SELFTEST" -eq 1 ] && { echo "selftest PASS（盘符规则正反用例全过）"; exit 0; }

# 发布态蕴含：strict（全量口径）+校准文件必需（发布终扫无项目名校准=假闸）
[ "$PUBLISH" -eq 1 ] && { STRICT=1; REQUIRE_CALIB=1; }

# ---------- 内部项目名校准文件（pattern 值=本地敏感配置不入库；轮换=编辑文件） --
CALIB_FILE="$(cd "$(dirname "$0")" && pwd -P)/sanitize-patterns.local"
CALIB_PATTERNS=""
[ -f "$CALIB_FILE" ] && CALIB_PATTERNS="$(grep -vE '^[[:space:]]*(#|$)' "$CALIB_FILE" | paste -sd'|' -)"
if [ -z "$CALIB_PATTERNS" ] && [ "$REQUIRE_CALIB" -eq 1 ]; then
  echo "FAIL: 内部项目名校准文件缺失或为空: $CALIB_FILE（--require-calibration 要求存在）" >&2
  exit 4
fi

is_binary_ext() {
  case "${1##*.}" in
    png|jpg|jpeg|gif|webp|bmp|ico|svgz|zip|gz|tgz|bz2|xz|7z|tar|exe|dll|so|dylib|bin|msi|apk \
    |woff|woff2|ttf|eot|otf|mp3|mp4|avi|mov|webm|wav|pdf|doc|docx|xls|xlsx|ppt|pptx|jar|class \
    |pyc|pyo|iso|img|dmg|deb|rpm|db|sqlite|woff) return 0 ;;
    *) return 1 ;;
  esac
}

match_prefix() { # $1=相对路径 $2=数组名（nameref）
  local -n ref="$2"; local p
  for p in "${ref[@]}"; do
    [[ "$1" == "$p"* ]] && return 0
  done
  return 1
}

match_exact() { # $1=相对路径 $2=数组名（nameref）
  local -n ref="$2"; local p
  for p in "${ref[@]}"; do
    [ "$1" = "$p" ] && return 0
  done
  return 1
}

HITS=0; SCANNED=0; EXEMPTED=0

run_grep() { # $1=规则名 $2=ERE；对当前 $f 逐行报命中
  local name="$1" ere="$2" line
  while IFS= read -r line; do
    echo "$rel:$line（$name）"
    HITS=$((HITS+1))
  done < <(grep -nE -- "$ere" "$f" 2>/dev/null)
}

# 未登记占位符的登记表 regex（从规范 README §三提取一次；<n> 系列展开）
SPEC_ABS="$TARGET/$SPEC_README"
REG_ALT=""
if [ -f "$SPEC_ABS" ]; then
  REG_ALT="$(grep -oE '\{\{[A-Z0-9_]+(<n>)?[A-Z0-9_]*\}\}' "$SPEC_ABS" | sort -u \
    | sed -e 's/[{}]/\\&/g' -e 's/<n>/[A-Z0-9_]+/g' | paste -sd'|' -)"
fi

while IFS= read -r -d '' f; do
  [ "$f" = "$SCRIPT_SELF" ] && continue          # 本脚本自身（正则字面系技术必需）
  [ "$f" = "$CALIB_FILE" ] && continue           # 校准文件自身（内容即敏感 pattern，不入库）
  rel="${f#"$TARGET"/}"
  is_binary_ext "$rel" && continue               # 二进制后缀不扫
  case "$rel" in .aiteam/mirror*) continue ;; esac
  # mirror 审计=运行态数据面不扫：send 镜像逐字审计消息正文，正文自由文本必带
  # 各类字面（如消息原文引用的盘符），扫描必每轮红——豁免的是数据面非交付面。

  # ---- 豁免/白名单/强制扫描判定 ----
  partial_exempt=0; skip=0
  if match_exact "$rel" EXEMPT_FILES; then
    partial_exempt=1; EXEMPTED=$((EXEMPTED+1))   # 部分豁免：条文自展示规则跳过
  elif match_prefix "$rel" FORCE_SCAN_PREFIXES; then
    :                                            # 强制扫描，优先于白名单
  elif [ "$STRICT" -eq 0 ] && match_prefix "$rel" WHITELIST_PREFIXES; then
    skip=1; EXEMPTED=$((EXEMPTED+1))
  elif [ "$STRICT" -eq 1 ] && match_prefix "$rel" WHITELIST_PREFIXES; then
    :                                            # strict：白名单失效，全量扫描
  fi
  [ "$skip" -eq 1 ] && continue
  SCANNED=$((SCANNED+1))

  in_template=0; in_skills=0
  case "$rel" in docs/planning/_template/*) in_template=1 ;; esac
  case "$rel" in skills/*) in_skills=1 ;; esac

  # ---- 通用四规则（豁免文件跳过——规范条文自展示） ----
  if [ "$partial_exempt" -eq 0 ]; then
    # 10 段独占网段一节、需三段尾；192.168 / 172.16~31 网段占两节、需两段尾
    run_grep "私有IP"     '\b(192\.168\.[0-9]+\.[0-9]+|10\.[0-9]+\.[0-9]+\.[0-9]+|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]+\.[0-9]+)\b'
    run_grep "私有IP示意" '\b(192\.168\.[0-9x*]+\.[0-9x*]+|10\.[0-9x*]+\.[0-9x*]+\.[0-9x*]+|172\.(1[6-9]|2[0-9]|3[01])\.[0-9x*]+\.[0-9x*]+)'
    run_grep "盘符路径"   '(^|[^A-Za-z%])[A-Za-z]:[\\/]'
    run_grep "用户目录"   '[Cc]:[\\/][Uu]sers'
  fi

  # ---- 内部项目名零容忍：默认 _template/ 或 skills/ 前缀（skills/ 随仓交付
  #      同属开源发布面，S7 --strict 终审兜底不留盲区）；--publish 发布态全
  #      范围（导出自带的本脚本副本亦是被扫对象——扫描器自身泄漏类自此可被
  #      机械抓住）。pattern 值=本地校准文件（不入库——见头部口径），缺失=
  #      规则整体跳过。
  if [ -n "$CALIB_PATTERNS" ] && { [ "$in_template" -eq 1 ] || [ "$in_skills" -eq 1 ] || [ "$PUBLISH" -eq 1 ]; }; then
    run_grep "内部项目名" "$CALIB_PATTERNS"
  fi

  # ---- _template/ 专用规则 ----
  if [ "$in_template" -eq 1 ]; then
    if [ "$rel" != "$SPEC_README" ]; then
      run_grep "开发期引用" 'mailbox|signals'   # README §五.6 条文自述豁免
    fi
    if [ "$rel" != "$SPEC_README" ]; then
      # 待回填越界（双口径，主会话裁定）：
      #   s0-s7-stages.md —— 严格节内管辖：{{待回填：…}} 仅许 ## S[567] 标题节内；
      #   其他 _template 文件 —— 标记行须自带阶段范围说明（行文本含 S5~S7 /
      #     S5-S7 / S5～S7 变体字样），无说明即越界，防 S0~S4 模板乱塞。
      if [ "$(basename "$rel")" = "s0-s7-stages.md" ]; then
        while IFS= read -r line; do
          echo "$rel:$line（待回填越界）"; HITS=$((HITS+1))
        done < <(awk '/^## /{sec=$0} /\{\{待回填/ && sec !~ /^## S[567]/ {print FNR":"$0}' "$f")
      else
        while IFS= read -r row; do
          echo "$rel:$row（待回填越界）"; HITS=$((HITS+1))
        done < <(grep -nE '\{\{待回填' "$f" | grep -vE 'S5(~|-|～)S7')
      fi
      # 未登记占位符：实例须对上规范 README §三保留字登记表
      if [ -n "$REG_ALT" ]; then
        while IFS= read -r inst; do
          [ -z "$inst" ] && continue
          # 系列字面归一化：{{FOO_<n>}}→{{FOO_1}}，<n> 归一为合法占位字符后
          # 再对登记表系列 regex 试匹配一次（字面原样试一次+归一后试一次）
          norm="${inst//<n>/1}"
          if ! printf '%s\n' "$inst" | grep -qE "^(${REG_ALT})\$" \
             && ! printf '%s\n' "$norm" | grep -qE "^(${REG_ALT})\$"; then
            while IFS= read -r ln; do
              echo "$rel:$ln: $inst（未登记占位符）"; HITS=$((HITS+1))
            done < <(grep -nF -- "$inst" "$f" | cut -d: -f1)
          fi
        done < <(grep -hoE '\{\{[A-Z0-9_]+(<n>)?[A-Z0-9_]*\}\}' "$f" | sort -u)
      fi
    fi
  fi
# 排除目录：.git / node_modules / dist 构建物；.worktrees 与 worktrees 为
# worktree 隔离开发区镜像（非仓工作区本体，与本仓 scripts/check.sh「排除
# .worktrees」同一口径）——否则在主仓根跑会把 worktree 副本整棵重复扫一遍
done < <(find "$TARGET" \( -name .git -o -name node_modules -o -name dist -o -name .worktrees -o -name worktrees \) -prune -o -type f -print0)

CALIB_NOTE=""
[ -z "$CALIB_PATTERNS" ] && CALIB_NOTE="；内部项目名规则未配置=跳过（校准文件 sanitize-patterns.local 缺失或为空，应置于脚本同目录）"

if [ "$HITS" -gt 0 ]; then
  echo "共命中 $HITS 处 —— 脱敏自查 FAIL"
  exit 1
fi
echo "PASS（扫描 $SCANNED 个文件，白名单豁免 $EXEMPTED 个$CALIB_NOTE）"
exit 0
