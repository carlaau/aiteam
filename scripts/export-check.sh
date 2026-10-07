#!/usr/bin/env bash
# =============================================================================
# export-check.sh —— 发布快照形态机械校验闸（导出白名单闸）
# =============================================================================
# 用途: 「开发版→开源快照导出」的发布前机械对拍——禁入面（FORBIDDEN，有残留
#   即违规）与必在面（REQUIRED，有缺失即违规）双清单对拍，任一不满足即 FAIL
#   卡发布。白名单边界：实况件（红绿灯/维护台账/迭代实录等运行态）不进公开仓；
#   接入配置（.aiteam/cli.json+onboarding.md）与 CLAUDE.md 属项目身份两仓共享必在。
# 职责分离: 内容级私有字面扫描归 scripts/check-sanitize.sh --publish 承担；
#   本脚本只管结构（存在/缺失/非空），不做任何内容级扫描。
# fork 语境注记: 校验对象=发布快照（单提交形态）。fork 开发者跑过 `aiteam init`
#   后，planning 实况骨架（project-status/maintenance/iterations）会被播种到
#   工作区，届时禁入面必然命中、本脚本对该目录不再适用——属预期行为非缺陷。
# 环境: bash ≥ 4.4 + GNU findutils（Git Bash/Linux 均满足；find -print -quit
#   为 GNU 扩展，非空校验依赖它）
#
# 用法: scripts/export-check.sh [目录]
#   目录      校验根，缺省=当前目录（建议对快照目录根跑）
# 退出码: 0=PASS | 1=有违规 | 2=参数错误
# =============================================================================
set -u
export LC_ALL=C   # 字节语义：路径判定与 find 行为跨语环境稳定一致

# ---------- 禁入面（FORBIDDEN：存在即违规，目录/文件都算） -------------------
# 口径: 仅查 TARGET 顶层，嵌套产物由 .gitignore 与导出流程保证——勿误当全深度闸。
#   .aiteam 不整目录禁入（cli.json+onboarding.md 接入配置已入发布面），仅
#   mirror- 前缀运行态审计另设特检（见下方 mirror 段）。
FORBIDDEN_PATHS=(
  "docs/planning/project-status.md"   # 红绿灯实况=本仓运行态，不进公开快照
  "docs/planning/maintenance.md"      # 维护台账实况（运行态记录）
  "docs/planning/iterations"          # 开发过程实录层（批次/决策账本）
  "dist"                              # 本地构建产物（gitignore，不入快照）
  "node_modules"                      # 前端构建产物
  ".worktrees"                        # worktree 隔离区镜像
  "worktrees"                         # worktree 隔离区镜像（无点变体）
)

# ---------- 必在面（REQUIRED：缺失即违规） -----------------------------------
REQUIRED_PATHS=(
  "LICENSE"                           # MIT 协议文本，开源发布法定件
  "README.md"                         # 项目门面说明
  "AGENTS.md"                         # AI/人接手动线（第一读序+通讯接入区块）
  ".gitattributes"                    # 行尾/eol 策略（全仓 LF 口径载体）
  ".gitignore"                        # 构建产物/本地配置忽略策略
  "go.mod"                            # Go 模块定义（单二进制构建入口依赖）
  "go.sum"                            # 依赖校验和（可复现构建）
  "templates.go"                      # 方法论模板 embed 载体（模块根包）
  "cmd/aiteam/main.go"                # 入口 main
  "internal/server"                   # HTTP 服务+看板路由
  "internal/store"                    # SQLite 存储层
  "internal/cli"                      # CLI 命令族
  "internal/client"                   # HTTP 客户端封装
  "internal/config"                   # 配置加载
  "internal/mirror"                   # send 镜像审计双写
  "internal/types"                    # 请求/响应结构体
  "internal/assets"                   # embed 发布资产
  "internal/assets/skills"            # 方法论技能包 embed 资产
  "scripts/check.sh"                  # 提交前双闸之一（五节检查）
  "scripts/check-sanitize.sh"         # 提交前双闸之二（脱敏红线）
  "scripts/build.sh"                  # 构建脚本
  "scripts/export-sync.sh"            # dev→快照机械同步闸（随发布面交付）
  "docs/planning/_template"           # 方法论模板包（目录且非空，另加非空校验）
  ".agents/skills"                    # 技能包（目录且非空，另加非空校验）
  ".agents/skills/LICENSES.md"        # 技能包许可证清单（随技能包交付）
  "CLAUDE.md"                         # Claude Code 客户端指引（通讯接入区块，两仓同文）
  ".aiteam/cli.json"                  # 多会话通讯连接配置（服务地址/令牌，项目身份）
  ".aiteam/onboarding.md"             # 多会话通讯上手启动指令（AI 会话开工第一读）
  "scripts/export-check.sh"           # 本脚本自身随发布面交付（快照里也应有）
)

usage() {
  # 打印头注释块（自第 2 行起逐行打印 # 行，遇首个非注释代码行即止——对行号漂移免疫）
  awk 'NR<2{next} /^#/{print; next} {exit}' "$0" | sed 's/^# \{0,1\}//'
}
die2() { echo "参数错误: $1" >&2; echo "用法: $0 [目录]" >&2; exit 2; }

dir_has_file() { # $1=绝对路径；「非空」语义=目录下（递归）存在至少 1 个常规文件（find -type f），
                 # 仅含空子目录/其他条目不算——防把 -type d 口径误读成「有条目即非空」
  [ -n "$(find "$1" -type f -print -quit 2>/dev/null)" ]
}

TARGET=""
for arg in "$@"; do
  case "$arg" in
    -h|--help) usage; exit 0 ;;
    -*) die2 "未知参数 $arg" ;;
    *) [ -n "$TARGET" ] && die2 "目录参数只能给一个"; TARGET="$arg" ;;
  esac
done
[ -z "$TARGET" ] && TARGET="."
[ -d "$TARGET" ] || die2 "目录不存在: $TARGET"
TARGET="$(cd "$TARGET" && pwd -P)"

VIOLATIONS=0

# ---- 禁入面：存在即违规（目录/文件都算） ------------------------------------
for p in "${FORBIDDEN_PATHS[@]}"; do
  if [ -e "$TARGET/$p" ]; then
    echo "禁入残留: $p"
    VIOLATIONS=$((VIOLATIONS+1))
  fi
done

# ---- 禁入面补充：.aiteam/mirror- 前缀运行态审计（命中即违规） ----------------
# send 镜像审计行=本地运行态不入库；.aiteam 其余两件（cli.json/onboarding.md）
# 已入发布面必在面，故只对 mirror- 前缀特检（路径前缀匹配，不限深度）。
while IFS= read -r f; do
  echo "禁入残留: ${f#"$TARGET"/}"
  VIOLATIONS=$((VIOLATIONS+1))
done < <(find "$TARGET/.aiteam" -path "$TARGET/.aiteam/mirror-*" -print 2>/dev/null)

# ---- 必在面：缺失即违规；_template 与 .agents/skills 另加非空校验 -----------
for p in "${REQUIRED_PATHS[@]}"; do
  abs="$TARGET/$p"
  if [ ! -e "$abs" ]; then
    echo "缺失: $p"
    VIOLATIONS=$((VIOLATIONS+1))
    continue
  fi
  # 非空特检分发：新增「目录且非空」项须同步本 case 与 REQUIRED 清单注释，
  # 防静默降级为只查存在
  case "$p" in
    docs/planning/_template|.agents/skills)
      if [ ! -d "$abs" ]; then
        echo "非目录: $p"
        VIOLATIONS=$((VIOLATIONS+1))
      elif ! dir_has_file "$abs"; then
        echo "空目录: $p"
        VIOLATIONS=$((VIOLATIONS+1))
      fi
      ;;
  esac
done

if [ "$VIOLATIONS" -gt 0 ]; then
  echo "共 $VIOLATIONS 处违规 —— export-check FAIL"
  exit 1
fi
echo "export-check PASS（禁入面干净，必在面 ${#REQUIRED_PATHS[@]} 项齐备）"
exit 0
