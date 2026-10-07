# setup-skills.ps1 —— 方法论技能包安装（Windows junction 版）
# 把仓内技能（internal/assets/skills/）逐个链接到目标项目的 .claude/skills/（每技能一条 junction；init 默认装 .agents/skills/ 后=手动工具）。
# 技能是各契约/启动指令中「以 Skill 工具加载 XX」的实体；不装则客户端 Skill 工具无法发现。
#
# 用法: powershell -File scripts/setup-skills.ps1 [-Target <目标项目根>]
#   -Target 缺省 = 当前工作目录（在哪个项目根下跑就装到哪）
#   源目录自动定位本脚本所在仓的 internal/assets/skills/，与执行时 cwd 无关
#
# 幂等语义（重复跑零副作用）:
#   目标已是指向源的正确链接 → 跳过
#   链接存在但指向错         → 删旧建新（修复）
#   同路径已是普通目录/文件   → 该技能报错（先备份再删除该目录后重跑），其余技能继续，最终退出码非零
#
# 退出码: 0=全部成功（含跳过/修复）  1=有报错（源缺失/源零技能/目标冲突）
#
# 其他客户端: zcode 等客户端把同样的 junction/symlink 建到其自己的技能目录即可（目录路径见各自文档）。
# 非技能机制客户端: SKILL.md 本质是 markdown 指令文件，没有 Skill 工具的客户端可把其内容当 prompt 模板手动粘贴使用。
param(
    [string]$Target = (Get-Location).Path
)

$ErrorActionPreference = 'Stop'

# ---- 源定位（脚本所在仓的 internal/assets/skills/，与 cwd 无关）----
$repoRoot  = Split-Path -Parent $PSScriptRoot
$sourceDir = Join-Path $repoRoot 'internal/assets/skills'
if (-not (Test-Path -LiteralPath $sourceDir)) {
    Write-Host "[报错] 源目录不存在: $sourceDir"
    exit 1
}
$sourceFull = (Resolve-Path -LiteralPath $sourceDir).Path.TrimEnd('\', '/')

# ---- 目标解析（缺省 cwd）----
if (-not (Test-Path -LiteralPath $Target)) {
    Write-Host "[报错] 目标项目根不存在: $Target"
    exit 1
}
$targetRoot = (Resolve-Path -LiteralPath $Target).Path
$skillsDir  = Join-Path $targetRoot '.claude\skills'

# ---- 技能枚举（含 SKILL.md 的子目录；源零技能=报错）----
$skills = @(Get-ChildItem -LiteralPath $sourceDir -Directory |
    Where-Object { Test-Path -LiteralPath (Join-Path $_.FullName 'SKILL.md') } |
    Sort-Object Name)
if ($skills.Count -eq 0) {
    Write-Host "[报错] 源 skills/ 下没有任何含 SKILL.md 的技能目录: $sourceDir"
    exit 1
}

# 取目标条目：悬空 reparse point 用 Test-Path 会漏判（返回 false），父目录枚举兜底
function Get-DestItem([string]$Path) {
    if (Test-Path -LiteralPath $Path) {
        return Get-Item -LiteralPath $Path -Force
    }
    $parent = Split-Path -Parent $Path
    $leaf   = Split-Path -Leaf $Path
    if (Test-Path -LiteralPath $parent) {
        $hit = @(Get-ChildItem -LiteralPath $parent -Force -ErrorAction SilentlyContinue |
            Where-Object { $_.Name -ieq $leaf })
        if ($hit.Count -gt 0) { return $hit[0] }
    }
    return $null
}

Write-Host "源: $sourceFull"
Write-Host "目标: $skillsDir"

$installed = 0; $skipped = 0; $fixed = 0; $failed = 0

foreach ($s in $skills) {
    $name = $s.Name
    $dest = Join-Path $skillsDir $name
    try {
        $item = Get-DestItem $dest
        if ($null -eq $item) {
            # 安装
            if (-not (Test-Path -LiteralPath $skillsDir)) {
                New-Item -ItemType Directory -Path $skillsDir -Force | Out-Null
            }
            New-Item -ItemType Junction -Path $dest -Target $s.FullName | Out-Null
            Write-Host "[安装] $name"
            $installed++
        }
        elseif ($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) {
            # 链接已存在（含悬空链接）→ 按指向分流（期望指向=该技能自己的源目录）
            $lt = $item.Target
            if ($lt -is [array]) { $lt = $lt[0] }
            $want = $s.FullName.TrimEnd('\', '/')
            if ($lt -and $lt.TrimEnd('\', '/') -ieq $want) {
                Write-Host "[跳过] $name"
                $skipped++
            } else {
                # 修复：Directory.Delete 对 reparse point 只删链接本身，不碰目标内容
                [System.IO.Directory]::Delete($item.FullName)
                New-Item -ItemType Junction -Path $dest -Target $s.FullName | Out-Null
                Write-Host "[修复] $name"
                $fixed++
            }
        }
        else {
            Write-Host "[报错] $name —— 目标已存在且不是链接: $dest（先备份再删除该目录，然后重跑本脚本）"
            $failed++
        }
    } catch {
        Write-Host "[报错] $name —— $($_.Exception.Message)"
        $failed++
    }
}

Write-Host "汇总：安装 $installed / 跳过 $skipped / 修复 $fixed / 报错 $failed"
if ($failed -gt 0) { exit 1 }
exit 0
