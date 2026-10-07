#!/usr/bin/env bash
# aiteam 交叉编译发布脚本：产出五平台单二进制（windows/amd64 + linux amd64/arm64
# + darwin amd64/arm64，v0.2.0 发布链 Q3 扩平台）。
# 规格依据：FR16 / AC16.3——纯 Go（modernc.org/sqlite 零 cgo）天然可交叉编译。
# 范围边界：不产压缩包/release note（S7 发布收口定）；checksum（SHA256SUMS）随本
# 脚本产出（S7 发布收口启用）。版本注入（b7b-1 口径升级）：VER 默认 git describe
# （tag 仓发布构建得 v0.2.0 形态），无 tag 回落 v0.1.0-dev.<yyyyMMddHHmm>（开发仓
# 日常构建），可环境变量覆盖。
set -eu
cd "$(dirname "$0")/.." || exit 1

VER="${VER:-$(git describe --tags --always --dirty 2>/dev/null || echo "v0.1.0-dev.$(date +%Y%m%d%H%M)")}"
LDFLAGS="-X aiteam/internal/server.Version=$VER"

echo "==> 版本注入: $VER（aiteam/internal/server.Version）"

echo "==> mkdir -p dist"
mkdir -p dist || { echo "FAIL: 创建 dist 目录失败" >&2; exit 1; }

build_one() {
  local goos="$1" goarch="$2" out="$3"
  echo "==> GOOS=$goos GOARCH=$goarch go build"
  GOOS="$goos" GOARCH="$goarch" go build -ldflags "$LDFLAGS" -o "dist/$out" ./cmd/aiteam \
    || { echo "FAIL: $goos/$goarch 编译失败" >&2; exit 1; }
  case "$out" in
    *.exe) ;;
    *)
      # 执行位声明：部署侧 `./aiteam-linux-amd64` 直接运行依赖 755。Linux 环境
      # 跑本脚本时 go build 本就产出 755，此步幂等；Windows/Git Bash 产出侧因
      # noacl 挂载为 no-op（实测 mount 全 noacl，chmod 退 0 位不变）。
      chmod +x "dist/$out" || { echo "FAIL: 补执行位失败 dist/$out" >&2; exit 1; }
      ;;
  esac
  echo "OK"
}

build_one windows amd64 aiteam-windows-amd64.exe
build_one linux   amd64 aiteam-linux-amd64
build_one linux   arm64 aiteam-linux-arm64
build_one darwin  amd64 aiteam-darwin-amd64
build_one darwin  arm64 aiteam-darwin-arm64

echo "==> 产物清单"
for f in dist/aiteam-windows-amd64.exe dist/aiteam-linux-amd64 dist/aiteam-linux-arm64 \
         dist/aiteam-darwin-amd64 dist/aiteam-darwin-arm64; do
  ls -lh "$f" || { echo "FAIL: 产物缺失 $f" >&2; exit 1; }
done

echo "==> SHA256SUMS"
cd dist && sha256sum aiteam-windows-amd64.exe aiteam-linux-amd64 aiteam-linux-arm64 \
  aiteam-darwin-amd64 aiteam-darwin-arm64 > SHA256SUMS && cat SHA256SUMS
cd ..
echo "BUILD DONE version=$VER"
