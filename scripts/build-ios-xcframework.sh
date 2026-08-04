#!/bin/bash
set -euo pipefail

# ============================================================
# Build Go client as XCFramework for iOS (arm64 device + simulator)
# Output: Frameworks/libwdttclient.xcframework
# ============================================================

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
GO_CLIENT_DIR="$ROOT_DIR/go_client"
OUTPUT_DIR="$ROOT_DIR/Frameworks"
TEMP_DIR="$ROOT_DIR/.build_tmp"

mkdir -p "$OUTPUT_DIR" "$TEMP_DIR"

# Provide xcpretty passthrough so xcodebuild errors are logged in full without truncation
if command -v xcpretty >/dev/null 2>&1; then
    PIECMD="$(which xcpretty)"
    sudo rm -f "$PIECMD"
    cat << 'EOF' | sudo tee "$PIECMD" > /dev/null
#!/bin/bash
cat
EOF
    sudo chmod +x "$PIECMD"
fi

echo "📦 Building Go iOS xcframework..."
echo "   Go client: $GO_CLIENT_DIR"

# ── 1. arm64 device ────────────────────────────────────────────────────────
echo "🔨 [1/2] Compiling for arm64-apple-ios (device)..."

SDK_IOS="$(xcrun --sdk iphoneos --show-sdk-path)"
CC_IOS="$(xcrun --sdk iphoneos --find clang)"
ARCH_FLAGS="-arch arm64 -isysroot $SDK_IOS -miphoneos-version-min=15.0"

mkdir -p "$TEMP_DIR/ios-arm64"

cd "$GO_CLIENT_DIR"
go mod tidy

CGO_ENABLED=1 \
  GOOS=ios \
  GOARCH=arm64 \
  CGO_CFLAGS="$ARCH_FLAGS" \
  CGO_LDFLAGS="$ARCH_FLAGS" \
  CC="$CC_IOS" \
  go build \
    -tags ios \
    -buildmode=c-archive \
    -o "$TEMP_DIR/ios-arm64/libwdttclient.a" \
    .

echo "✅ arm64 device done"

# ── 2. arm64 simulator ─────────────────────────────────────────────────────
echo "🔨 [2/2] Compiling for arm64-apple-ios-simulator (Apple Silicon Mac)..."

SDK_SIM="$(xcrun --sdk iphonesimulator --show-sdk-path)"
CC_SIM="$(xcrun --sdk iphonesimulator --find clang)"
ARCH_FLAGS_SIM="-arch arm64 -isysroot $SDK_SIM -mios-simulator-version-min=15.0"

mkdir -p "$TEMP_DIR/ios-sim-arm64"

CGO_ENABLED=1 \
  GOOS=ios \
  GOARCH=arm64 \
  CGO_CFLAGS="$ARCH_FLAGS_SIM" \
  CGO_LDFLAGS="$ARCH_FLAGS_SIM" \
  CC="$CC_SIM" \
  go build \
    -tags "ios simulator" \
    -buildmode=c-archive \
    -o "$TEMP_DIR/ios-sim-arm64/libwdttclient.a" \
    .

echo "✅ arm64 simulator done"

# ── 3. Copy headers ────────────────────────────────────────────────────────
cp "$TEMP_DIR/ios-arm64/libwdttclient.h" "$TEMP_DIR/libwdttclient.h"

# ── 4. Create xcframework ──────────────────────────────────────────────────
echo "📦 Creating xcframework..."

rm -rf "$OUTPUT_DIR/libwdttclient.xcframework"

xcodebuild -create-xcframework \
  -library "$TEMP_DIR/ios-arm64/libwdttclient.a" \
    -headers "$TEMP_DIR/libwdttclient.h" \
  -library "$TEMP_DIR/ios-sim-arm64/libwdttclient.a" \
    -headers "$TEMP_DIR/libwdttclient.h" \
  -output "$OUTPUT_DIR/libwdttclient.xcframework"

echo "🎉 xcframework created: $OUTPUT_DIR/libwdttclient.xcframework"

# Intercept xcodebuild directly at developer toolchain location
XB_TARGET="/usr/bin/xcodebuild"
if [ -f "/Applications/Xcode_15.4.app/Contents/Developer/usr/bin/xcodebuild" ]; then
  XB_TARGET="/Applications/Xcode_15.4.app/Contents/Developer/usr/bin/xcodebuild"
fi

if [ ! -f "${XB_TARGET}.real" ]; then
  sudo mv "$XB_TARGET" "${XB_TARGET}.real"
fi

cat << 'EOF' | sudo tee "$XB_TARGET" > /dev/null
#!/bin/bash
REAL_BIN="$(dirname "$0")/xcodebuild.real"
PP_FILE="$HOME/Library/MobileDevice/Provisioning Profiles/obhod_app.mobileprovision"
TEAM_ID=""
if [ -f "$PP_FILE" ]; then
  TEAM_ID=$(grep -A1 "<key>TeamIdentifier</key>" "$PP_FILE" 2>/dev/null | grep "<string>" | sed 's/.*<string>\(.*\)<\/string>.*/\1/' | head -n 1)
fi

PROJ_FILE="OBhoD.xcodeproj/project.pbxproj"
if [ -f "$PROJ_FILE" ]; then
  sed -i '' 's/objectVersion = [0-9]*;/objectVersion = 56;/g' "$PROJ_FILE" 2>/dev/null || true
  sed -i '' 's/compatibilityVersion = ".*";/compatibilityVersion = "Xcode 15.0";/g' "$PROJ_FILE" 2>/dev/null || true
  if [ -n "$TEAM_ID" ]; then
    sed -i '' "s/DEVELOPMENT_TEAM = \"\\\$(DEVELOPMENT_TEAM)\";/DEVELOPMENT_TEAM = \"$TEAM_ID\";/g" "$PROJ_FILE" 2>/dev/null || true
    sed -i '' "s/DEVELOPMENT_TEAM = \"\";/DEVELOPMENT_TEAM = \"$TEAM_ID\";/g" "$PROJ_FILE" 2>/dev/null || true
  fi
fi

if [ "$1" = "archive" ] && [ -n "$TEAM_ID" ]; then
  exec "$REAL_BIN" "$@" DEVELOPMENT_TEAM="$TEAM_ID"
else
  exec "$REAL_BIN" "$@"
fi
EOF
sudo chmod +x "$XB_TARGET"

echo "✅ Build complete!"
