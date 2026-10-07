#!/usr/bin/env bash
# aiteam 一键检查五节：gofmt → go vet → go build → go build GOOS=linux → go test。
# 任一节失败退出 1；全部通过输出 ALL GREEN。
set -u
cd "$(dirname "$0")/.." || exit 1

# gofmt 节：无条件严格（S7-3 2026-10-04 恢复——.gitattributes eol=lf 全仓生效+工作区已刷 LF，
# autocrlf=true 不再假红；SKIP 口径退役。.worktrees 排除随 worktree 清场一并退役）。
echo "==> [1/5] gofmt"
if out=$(find . -name '*.go' -not -path './.git/*' -print0 | xargs -0 gofmt -l 2>/dev/null); [ -n "$out" ]; then
  echo "FAIL: 以下文件未通过 gofmt:" >&2
  printf '%s\n' "$out" >&2
  exit 1
fi
echo "OK"

echo "==> [2/5] go vet ./..."
go vet ./... || { echo "FAIL: go vet" >&2; exit 1; }
echo "OK"

echo "==> [3/5] go build ./..."
go build ./... || { echo "FAIL: go build" >&2; exit 1; }
echo "OK"

# 交叉编译步（B6 抓虫后增）：文件名 _windows/_linux 等后缀=隐式 GOOS 构建约束，
# 本机 build 不触发——GOOS=linux 全量 build 防「整文件被排除」类缺陷回归
# （B4 域 handler_windows.go 事故：linux 下三 handler undefined，2026-10-03 C 抓修）。
echo "==> [4/5] go build GOOS=linux ./...（交叉编译防回归）"
GOOS=linux go build ./... || { echo "FAIL: GOOS=linux build" >&2; exit 1; }
echo "OK"

echo "==> [5/5] go test ./..."
go test ./... || { echo "FAIL: go test" >&2; exit 1; }
echo "OK"

echo "ALL GREEN"
