#!/usr/bin/env bash
# Build everything from one place.
#
#   scripts/build.sh server      cross-compile atn for every platform -> dist/bin/<platform>-<arch>/ (+ dist/release/atn-<platform>-<arch>)
#   scripts/build.sh dev         build atn for this machine only + copy assets into both clients
#   scripts/build.sh vscode      server + package the VS Code extension (.vsix, all platforms)
#   scripts/build.sh vscode-targets  server + one platform-specific .vsix per target (marketplace)
#   scripts/build.sh intellij    server + build the IntelliJ plugin zip
#   scripts/build.sh all         everything
#   scripts/build.sh test        run server, VS Code and IntelliJ tests
#
# Platform names follow Node/VS Code: darwin-arm64, darwin-x64, linux-x64,
# linux-arm64, win32-x64, win32-arm64. The IntelliJ plugin uses the same names.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="$(cat "$ROOT/VERSION")"
DIST="$ROOT/dist"
TARGETS=(darwin-arm64 darwin-x64 linux-x64 linux-arm64 win32-x64 win32-arm64)

goos()   { case "$1" in darwin-*) echo darwin;; linux-*) echo linux;; win32-*) echo windows;; esac; }
goarch() { case "$1" in *-arm64) echo arm64;; *-x64) echo amd64;; esac; }

build_one() {
  local t="$1" exe=atn
  [[ "$t" == win32-* ]] && exe=atn.exe
  mkdir -p "$DIST/bin/$t"
  (cd "$ROOT/server" && CGO_ENABLED=0 GOOS="$(goos "$t")" GOARCH="$(goarch "$t")" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$DIST/bin/$t/$exe" ./cmd/atn)
  echo "  built $t"
}

host_target() {
  local os arch
  case "$(uname -s)" in Darwin) os=darwin;; Linux) os=linux;; *) os=win32;; esac
  case "$(uname -m)" in arm64|aarch64) arch=arm64;; *) arch=x64;; esac
  echo "$os-$arch"
}

server() {
  echo "▶ server $VERSION"
  rm -rf "$DIST/bin" "$DIST/release"
  mkdir -p "$DIST/release"
  for t in "${TARGETS[@]}"; do
    build_one "$t"
    # flat copies for a GitHub release (CLI users / CI): atn-linux-x64, atn-win32-x64.exe, ...
    if [[ "$t" == win32-* ]]; then cp "$DIST/bin/$t/atn.exe" "$DIST/release/atn-$t.exe"; else cp "$DIST/bin/$t/atn" "$DIST/release/atn-$t"; fi
  done
}

sync_versions() {
  # One version for everything: VERSION file -> package.json + gradle.properties
  (cd "$ROOT/clients/vscode" && npm version "$VERSION" --no-git-tag-version --allow-same-version >/dev/null)
  sed -i.bak "s/^pluginVersion=.*/pluginVersion=$VERSION/" "$ROOT/clients/intellij/gradle.properties" && rm -f "$ROOT/clients/intellij/gradle.properties.bak"
}

# README images are served from GitHub (vsce rewrites relative links); keep in sync with the repo URL.
VSCE_IMAGES="--baseImagesUrl https://github.com/qSharpy/azure-templates-navigator/raw/HEAD/clients/vscode --baseContentUrl https://github.com/qSharpy/azure-templates-navigator/blob/HEAD/clients/vscode"

vscode_assets() { (cd "$ROOT/clients/vscode" && [ -d node_modules ] || npm ci --silent; node scripts/copy-assets.js "$@"); }

vscode() {
  echo "▶ vscode (universal)"
  sync_versions
  vscode_assets
  (cd "$ROOT/clients/vscode" && npx vsce package $VSCE_IMAGES -o "$DIST/azure-templates-navigator-$VERSION.vsix")
}

vscode_targets() {
  sync_versions
  for t in "${TARGETS[@]}"; do
    echo "▶ vscode $t"
    vscode_assets --target "$t"
    (cd "$ROOT/clients/vscode" && npx vsce package $VSCE_IMAGES --target "$t" -o "$DIST/azure-templates-navigator-$t-$VERSION.vsix")
  done
}

intellij() {
  echo "▶ intellij"
  sync_versions
  (cd "$ROOT/clients/intellij" && ./gradlew buildPlugin --quiet)
  cp "$ROOT"/clients/intellij/build/distributions/*.zip "$DIST/"
}

dev() {
  local t; t="$(host_target)"
  echo "▶ dev build for $t"
  rm -rf "$DIST/bin" && build_one "$t"
  vscode_assets
}

tests() {
  echo "▶ server tests";  (cd "$ROOT/server" && go vet ./... && go test ./...)
  echo "▶ vscode tests";  (cd "$ROOT/clients/vscode" && npm test)
  echo "▶ intellij tests"; (cd "$ROOT/clients/intellij" && ./gradlew test --quiet)
}

case "${1:-all}" in
  server) server ;;
  dev) dev ;;
  vscode) server; vscode ;;
  vscode-targets) server; vscode_targets ;;
  intellij) server; intellij ;;
  all) server; vscode; intellij ;;
  test) tests ;;
  *) echo "unknown target: $1"; exit 2 ;;
esac
