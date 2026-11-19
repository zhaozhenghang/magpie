#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 3 || ! "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ || ! "$2" =~ ^[1-9][0-9]*$ ]]; then
  echo "Usage: build-release.sh <upstream-version> <fork-revision> <output-directory>" >&2
  exit 2
fi

[[ "$(uname -s)/$(uname -m)" == Darwin/arm64 ]] || { echo "Build on an ARM64 Mac." >&2; exit 2; }
source_repo=$(git rev-parse --show-toplevel)
cd "$source_repo"
upstream="$1"
version="$upstream-weekly.$2"
mkdir -p "$3"
output=$(cd "$3" && pwd -P)
base=$(cat contrib/weekly-balance/upstream-base)
git merge-base --is-ancestor "$base" HEAD
[[ -z "$(git rev-list --merges "$base"..HEAD)" ]]
patches=()
while IFS= read -r patch; do patches+=("$patch"); done < <(git rev-list --reverse "$base"..HEAD)
[[ ${#patches[@]} -gt 0 ]]

temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/magpie-build.XXXXXXXX")
checkout="$temp_dir/source"
upstream_ref="refs/weekly-balance/builds/$(basename "$temp_dir")"
cleanup() {
  git -C "$source_repo" worktree remove --force "$checkout" 2>/dev/null || true
  git -C "$source_repo" update-ref -d "$upstream_ref"
  rm -rf "$temp_dir"
}
trap cleanup EXIT
tag="v$upstream"
official=https://github.com/yetone/magpie.git
git fetch --no-tags --no-write-fetch-head "$official" "refs/tags/$tag:$upstream_ref"
upstream_commit=$(git rev-parse "$upstream_ref^{commit}")
git worktree add -q --detach "$checkout" "$upstream_commit"
git -C "$checkout" -c user.name='Magpie Weekly Balance' -c user.email='magpie-weekly-balance@users.noreply.github.com' \
  cherry-pick "${patches[@]}"
cd "$checkout"

export GOPATH=$(go env GOPATH) GOMODCACHE=$(go env GOMODCACHE) GOCACHE=$(go env GOCACHE)
export HOME="$temp_dir/home" USERPROFILE="$temp_dir/home"
mkdir -m 700 "$HOME"
go test -tags nogui -count=1 -run '^Test(RoutedAllowanceQuotaWindows|StrategyQuotaWindows|RemoteMagpiesWeighedByTheirCards|WeeklyBalancePluginOnRemoteGroup|WindowCapsHoldTheAccount|RemoteMagpieQuotaRestsTheModel|StrategyFailsOpen|StrategiesServedWhileOn)$' \
  ./internal/provider ./internal/middleware ./internal/gateway
node --test contrib/weekly-balance/weekly-balance.test.mjs
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s contrib/weekly-balance -p test_update.py

make app VERSION="$version"
plist="magpie.app/Contents/Info.plist"
/usr/libexec/PlistBuddy -c 'Set :CFBundleIdentifier io.github.zhaozhenghang.magpie.weekly-balance' "$plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $upstream" -c "Set :CFBundleVersion $upstream" \
  -c "Add :MagpieWeeklyBalanceVersion string $version" "$plist"
/usr/libexec/PlistBuddy -c 'Delete :CFBundleURLTypes' "$plist" 2>/dev/null || true
codesign --force --deep --sign - magpie.app
codesign --verify --deep --strict magpie.app
[[ "$(magpie.app/Contents/MacOS/magpie --version)" == "magpie $version" ]]
[[ "$(lipo -archs magpie.app/Contents/MacOS/magpie)" == "arm64" ]]
ditto -c -k --keepParent magpie.app "$output/magpie-darwin-arm64.zip"
cp contrib/weekly-balance/weekly-balance.gateway.js "$output/"
printf '%s\n' "$upstream_commit" > "$output/upstream-commit.txt"
