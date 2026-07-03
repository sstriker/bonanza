#!/bin/bash
# SessionStart hook — provision Bazel 9 for Claude Code on the web.
#
# The web container has no bazel, and its egress proxy only permits a
# subset of the hosts a bonanza build needs:
#   - releases.bazel.build and *.bazel.build are blocked entirely.
#   - github.com is repo-scoped: git smart-HTTP works for any public
#     repo, but codeload archives and /releases/download/ assets only
#     work for repos that were explicitly added to the session.
#   - dl.google.com (Go SDK) is blocked.
#   - registry.npmjs.org, pypi.org, proxy.golang.org, index.crates.io,
#     nodejs.org, raw.githubusercontent.com, and
#     storage.googleapis.com are reachable.
#
# This hook provisions:
#   - bazelisk (npm) + bazel from the GCS bucket backing
#     releases.bazel.build, pinned by .bazelversion.
#   - ~/.bazelrc: BCR registry via its GitHub mirror, a downloader
#     config rewriting blocked hosts to the bazel-mirror GCS bucket
#     (with fallback to the original URL), shared disk/repository
#     caches, and module overrides for archives that cannot be
#     downloaded at all (see provision-bazel-deps.sh).
#   - a host Go SDK for rules_go: MODULE.bazel is locally switched from
#     go_sdk.from_file() to go_sdk.host(), with the modification hidden
#     from git via skip-worktree. Undo with:
#       git update-index --no-skip-worktree MODULE.bazel
#       git checkout -- MODULE.bazel
#
# Web-only, idempotent, synchronous, non-interactive. Individual steps
# degrade gracefully; a failure is logged instead of aborting the hook.
set -uo pipefail

if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

log() { echo "session-start: $*" >&2; }

CLAUDE_PROJECT_DIR="${CLAUDE_PROJECT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

# --- bazelisk: fetch bazel from GCS instead of releases.bazel.build ---
# The GCS bucket "bazel" backs releases.bazel.build and serves the same
# binaries, but under <version>/release/... instead of bazelisk's
# default layout, hence BAZELISK_FORMAT_URL rather than
# BAZELISK_BASE_URL.
BAZELISK_FORMAT_URL="https://storage.googleapis.com/bazel/%v/release/bazel-%v-%o-%m"
export BAZELISK_FORMAT_URL
echo "export BAZELISK_FORMAT_URL=\"$BAZELISK_FORMAT_URL\"" > /etc/profile.d/bazelisk-format-url.sh 2>/dev/null \
  || log "note: could not write /etc/profile.d/bazelisk-format-url.sh"
if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
  echo "export BAZELISK_FORMAT_URL=\"$BAZELISK_FORMAT_URL\"" >> "$CLAUDE_ENV_FILE"
fi

if command -v bazelisk >/dev/null 2>&1; then
  log "bazelisk already present; skipping"
elif command -v npm >/dev/null 2>&1; then
  # GitHub release downloads are blocked, but the npm package bundles
  # the bazelisk binary.
  if npm install -g @bazel/bazelisk >&2; then
    log "bazelisk installed via npm"
  else
    log "WARNING: bazelisk install failed"
  fi
else
  log "WARNING: no npm; cannot install bazelisk"
fi

# --- downloader config: reroute blocked hosts ------------------------
# Each blocked host is first rewritten to a mirror; an identity rewrite
# keeps the original URL as a fallback for archives the mirror lacks
# (e.g. repos added to the session via add_repo).
bzl_downloader_cfg="$HOME/.cache/bazel-mirror-downloader.cfg"
mkdir -p "$(dirname "$bzl_downloader_cfg")" "$HOME/.cache/bazel-disk" "$HOME/.cache/bazel-repos" 2>/dev/null
cat > "$bzl_downloader_cfg" <<'EOF'
rewrite ^github.com/(.*) storage.googleapis.com/bazel-mirror/github.com/$1
rewrite ^github.com/(.*) github.com/$1
rewrite ^codeload.github.com/(.*) storage.googleapis.com/bazel-mirror/codeload.github.com/$1
rewrite ^codeload.github.com/(.*) codeload.github.com/$1
rewrite ^releases.bazel.build/(.*) storage.googleapis.com/bazel/$1
rewrite ^mirror.bazel.build/(.*) storage.googleapis.com/bazel-mirror/$1
rewrite ^bcr.bazel.build/(.*) raw.githubusercontent.com/bazelbuild/bazel-central-registry/main/$1
EOF

# --- host Go SDK for rules_go -----------------------------------------
# go_sdk.from_file() downloads the SDK from dl.google.com, which is
# blocked. Resolve the go.mod-pinned toolchain through the Go module
# proxy instead (GOTOOLCHAIN fetches it from proxy.golang.org, which is
# reachable) and hand it to rules_go via go_sdk.host() + GOROOT.
goroot=""
if command -v go >/dev/null 2>&1; then
  goroot=$(cd "$CLAUDE_PROJECT_DIR" && go env GOROOT 2>/dev/null)
  if [ -n "$goroot" ]; then
    log "Go SDK for rules_go: $goroot"
  else
    log "WARNING: could not resolve GOROOT; bazel builds will fail to fetch the Go SDK"
  fi
fi
if [ -n "$goroot" ] && grep -q 'go_sdk.from_file(go_mod = "//:go.mod")' "$CLAUDE_PROJECT_DIR/MODULE.bazel"; then
  sed -i 's|go_sdk.from_file(go_mod = "//:go.mod")|go_sdk.host()  # LOCAL SANDBOX EDIT, DO NOT COMMIT (dl.google.com is blocked)|' \
    "$CLAUDE_PROJECT_DIR/MODULE.bazel"
  git -C "$CLAUDE_PROJECT_DIR" update-index --skip-worktree MODULE.bazel 2>/dev/null
  log "MODULE.bazel: switched to go_sdk.host() (hidden from git via skip-worktree)"
fi

# --- ~/.bazelrc managed block -----------------------------------------
bzl_rc="$HOME/.bazelrc"
bzl_rc_tmp=$(mktemp "$HOME/.bazelrc.XXXXXX" 2>/dev/null)
if [ -n "$bzl_rc_tmp" ]; then
  if [ -f "$bzl_rc" ]; then
    sed '/# >>> bonanza-egress >>>/,/# <<< bonanza-egress <<</d' "$bzl_rc" > "$bzl_rc_tmp"
  fi
  { [ -s "$bzl_rc_tmp" ] && [ -n "$(tail -c1 "$bzl_rc_tmp" 2>/dev/null)" ] && printf '\n' >> "$bzl_rc_tmp"; } || true
  cat >> "$bzl_rc_tmp" <<EOF
# >>> bonanza-egress >>>
common --registry=https://raw.githubusercontent.com/bazelbuild/bazel-central-registry/main
common --downloader_config=$bzl_downloader_cfg
common --disk_cache=$HOME/.cache/bazel-disk
common --repository_cache=$HOME/.cache/bazel-repos
# Local module overrides for archives that cannot be downloaded here.
try-import $HOME/.bazelrc-overrides
# Overridden modules are git trees, not release archives, so resolution
# from the registry does not line up with the checked-in lockfile.
common --lockfile_mode=off
# The prebuilt protoc is a GitHub release asset (blocked); build protoc
# from source instead, and don't fail on its slightly different version
# stamp.
common --no@protobuf//bazel/toolchains:prefer_prebuilt_protoc
common --@protobuf//bazel/flags:allow_nonstandard_protoc
${goroot:+common --repo_env=GOROOT=$goroot}
# <<< bonanza-egress <<<
EOF
  mv -f "$bzl_rc_tmp" "$bzl_rc" || { rm -f "$bzl_rc_tmp"; log "WARNING: could not update $bzl_rc"; }
  log "bazel egress configured in $bzl_rc"
fi

# --- module dependency overrides (heavy, idempotent) -------------------
if [ -x "$CLAUDE_PROJECT_DIR/.claude/hooks/provision-bazel-deps.sh" ]; then
  "$CLAUDE_PROJECT_DIR/.claude/hooks/provision-bazel-deps.sh" \
    || log "WARNING: provision-bazel-deps.sh reported failures (see above)"
else
  log "WARNING: provision-bazel-deps.sh not found; bazel builds will fail on unfetchable modules"
fi

# --- prefetch bazel itself ---------------------------------------------
if command -v bazelisk >/dev/null 2>&1; then
  if (cd "$CLAUDE_PROJECT_DIR" && bazelisk version >/dev/null 2>&1); then
    log "bazel prefetched: $(cd "$CLAUDE_PROJECT_DIR" && bazelisk version 2>/dev/null | grep 'Build label' || true)"
  else
    log "WARNING: bazel prefetch failed (deferred to first use)"
  fi
fi

log "provisioning done"
