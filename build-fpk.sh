#!/usr/bin/env bash
#
# 打包飞牛 fnOS 应用（.fpk）
#
# 用法：
#   ./build-fpk.sh            # 产出 x86 包（amd64）
#   ./build-fpk.sh arm64      # 产出 ARM 包
#
# 产物：项目根目录下的 Autodocker.fpk
#
# 做三件事：交叉编译 Linux 二进制 → 规整文件（行尾 / 平台声明）→ 调 fnpack 打包。
# 图标不在这里生成，它已经在 fnos/ 里了（改图标请重跑 tools/make-icon 那套 HTML 截图流程）。

set -euo pipefail

cd "$(dirname "$0")"

ARCH="${1:-amd64}"
case "$ARCH" in
amd64 | x86 | x86_64)
    GOARCH_VAL=amd64
    PLATFORM_VAL=x86
    ;;
arm64 | arm | aarch64)
    GOARCH_VAL=arm64
    PLATFORM_VAL=arm
    ;;
*)
    echo "不支持的架构：$ARCH（可用 amd64 / arm64）" >&2
    exit 1
    ;;
esac

# 找不到 go / fnpack 时回落到本机已知位置，方便在开发机上直接跑
if ! command -v go >/dev/null 2>&1; then
    if [ -x /c/Users/akira/.workbuddy/binaries/go/go/bin/go ]; then
        PATH="/c/Users/akira/.workbuddy/binaries/go/go/bin:$PATH"
        export GOPATH="${GOPATH:-/c/Users/akira/.workbuddy/binaries/go/gopath}"
    fi
fi
export GOFLAGS="${GOFLAGS:--mod=mod}"

FNPACK="${FNPACK:-}"
if [ -z "$FNPACK" ]; then
    if command -v fnpack >/dev/null 2>&1; then
        FNPACK=fnpack
    else
        FNPACK=/c/Users/akira/.workbuddy/binaries/fnpack/fnpack.exe
    fi
fi

echo "==> 编译 linux/${GOARCH_VAL}"
mkdir -p fnos/app/bin

# 版本号从 manifest 取，编进二进制里给「自检更新」用。
# 两处各写一份迟早会写成不一样 —— 就会出现「应用中心显示 1.5.0、程序自己认为 1.4.0」。
VERSION="$(sed -n 's/^version[[:space:]]*=[[:space:]]*//p' fnos/manifest | head -n 1 | tr -d '[:space:]')"
[ -z "$VERSION" ] && VERSION=dev
echo "    版本 ${VERSION}"

# 必须先删掉旧产物，这是个很坑的地方：
# go build -o 在「目标文件已存在且 build ID 相同」时会直接跳过写入。一旦产物被
# 外部工具改坏（文件被截断，尾部没了但开头的 build ID 还在），Go 仍认为它是最新的，
# 坏文件就会一直留着，后面每一步都「成功」但包里装的是跑不起来的二进制。
rm -f fnos/app/bin/autodocker
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH_VAL" \
    go build -trimpath \
    -ldflags="-s -w -X github.com/haihaipypy/autodocker/internal/selfcheck.Version=${VERSION}" \
    -o fnos/app/bin/autodocker ./cmd/server

echo "==> 校验二进制完整性"
go run ./tools/elfcheck fnos/app/bin/autodocker

echo "==> 校验前端脚本语法"
NODE_BIN="$(command -v node 2>/dev/null || true)"
[ -z "$NODE_BIN" ] && NODE_BIN=/c/Users/akira/.workbuddy/binaries/node/versions/22.22.2-6/node.exe
if [ -x "$NODE_BIN" ]; then
    "$NODE_BIN" --check web/app.js && echo "  app.js 语法正常"
fi

echo "==> 同步 manifest 平台声明为 ${PLATFORM_VAL}"
sed -i.bak "s/^platform .*$/platform              = ${PLATFORM_VAL}/" fnos/manifest
rm -f fnos/manifest.bak

echo "==> 规整文件格式（统一 LF 行尾，避免 Linux 上脚本报 bad interpreter）"
# 只处理文本文件。绝不能把编译产物一起喂给 sed —— ELF 里若出现 0D 0A 字节对，
# 会被当成行尾 CR 删掉，二进制当场损坏。
# 另外这里刻意不用 python：Windows 上 python 可能是应用商店的桩程序，执行会挂住。
fixed=0
for f in fnos/cmd/* fnos/app/bin/*.sh fnos/manifest fnos/config/* fnos/wizard/* fnos/app/ui/config; do
    [ -f "$f" ] || continue
    if grep -q $'\r' "$f" 2>/dev/null; then
        sed -i 's/\r$//' "$f"
        fixed=$((fixed + 1))
        echo "  LF -> $f"
    fi
done
echo "  共修正 ${fixed} 个文件"

echo "==> 打包"
rm -f ./*.fpk
"$FNPACK" build --directory fnos

echo
echo "==> 产物"
ls -la ./*.fpk
