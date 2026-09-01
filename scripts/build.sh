#!/bin/bash
# build.sh - 编译所有 CPA 插件
# 用法: ./scripts/build.sh [linux|darwin|windows] [amd64|arm64]
# 默认: 当前平台

set -e

TARGET_OS=${1:-$(go env GOOS)}
TARGET_ARCH=${2:-$(go env GOARCH)}
ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PLUGINS_DIR="$ROOT_DIR/plugins"

echo "=== cpa-multi-plugins build ==="
echo "Target: $TARGET_OS/$TARGET_ARCH"
echo "Plugins dir: $PLUGINS_DIR"
echo ""

# 插件清单（v0.12.0 收敛为 3 个统一插件）
PLUGINS=(
  "workbuddy"
  "qoder"
  "trae"
)

# 输出目录
OUT_DIR="$ROOT_DIR/dist/$TARGET_OS-$TARGET_ARCH"
mkdir -p "$OUT_DIR"

# 平台后缀
EXT="so"
if [ "$TARGET_OS" = "darwin" ]; then
  EXT="dylib"
elif [ "$TARGET_OS" = "windows" ]; then
  EXT="dll"
fi

# 设置 cross-compile 环境
export CGO_ENABLED=1
export GOOS=$TARGET_OS
export GOARCH=$TARGET_ARCH

# 跨平台编译需要对应 C 编译器
CC="gcc"
if [ "$TARGET_OS" = "darwin" ]; then
  CC="${CC:-clang}"
elif [ "$TARGET_OS" = "windows" ]; then
  CC="${CC:-x86_64-w64-mingw32-gcc}"
fi
export CC

SUCCESS=0
FAILED=0
FAILED_LIST=()

for plugin in "${PLUGINS[@]}"; do
  echo "--- Building $plugin ---"
  cd "$PLUGINS_DIR/$plugin"
  OUT_FILE="$OUT_DIR/${plugin}.${EXT}"
  if CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o "$OUT_FILE" . 2>&1 | tail -5; then
    if [ -f "$OUT_FILE" ]; then
      SIZE=$(du -h "$OUT_FILE" | cut -f1)
      echo "  ✅ $plugin → $OUT_FILE ($SIZE)"
      SUCCESS=$((SUCCESS+1))
    else
      echo "  ❌ $plugin: build succeeded but no output file"
      FAILED=$((FAILED+1))
      FAILED_LIST+=("$plugin")
    fi
  else
    echo "  ❌ $plugin: build failed"
    FAILED=$((FAILED+1))
    FAILED_LIST+=("$plugin")
  fi
  echo ""
done

echo "=== Summary ==="
echo "Success: $SUCCESS / ${#PLUGINS[@]}"
if [ $FAILED -gt 0 ]; then
  echo "Failed: $FAILED (${FAILED_LIST[*]})"
  exit 1
fi
echo "All plugins built. Output in: $OUT_DIR"
