#!/bin/bash
# Provision Bazel module dependencies that cannot be downloaded from
# within the Claude Code on the web sandbox (see session-start.sh for
# the egress rules). For every module in MODULE.bazel.lock whose source
# archive is neither on the bazel-mirror GCS bucket nor directly
# fetchable, this script recreates the module from a git clone of the
# exact tag/commit, reapplying whatever the registry would have layered
# on top (registry MODULE.bazel, overlay files, patches, strip_prefix),
# and emits --override_module flags into ~/.bazelrc-overrides.
#
# A handful of modules additionally download prebuilt binaries from
# GitHub releases at fetch time. Those are substituted with locally
# built equivalents (yq via `go install`, uutils coreutils via `cargo
# install`, the system bsdtar, Bootstrap's dist repackaged from npm,
# cpp_jsonnet from a git clone) by patching the local module clones.
#
# Idempotent: existing clones, binaries, and patches are left alone.
set -uo pipefail

log() { echo "provision-bazel-deps: $*" >&2; }

CLAUDE_PROJECT_DIR="${CLAUDE_PROJECT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
DEPS="$HOME/deps"
BCR=https://raw.githubusercontent.com/bazelbuild/bazel-central-registry/main
LOCK="$CLAUDE_PROJECT_DIR/MODULE.bazel.lock"
OUT="$HOME/.bazelrc-overrides"

mkdir -p "$DEPS" "$DEPS/archives" "$DEPS/bin"

# --- locally built binary substitutes ---------------------------------
# yq (needed by yq.bzl; the prebuilt binary is a GitHub release asset).
if [ ! -x "$DEPS/bin/yq" ] && command -v go >/dev/null 2>&1; then
  log "building yq from source (proxy.golang.org)"
  GOBIN="$DEPS/bin" go install github.com/mikefarah/yq/v4@v4.45.2 >&2 \
    || log "WARNING: yq build failed"
fi

# uutils coreutils (needed by bazel_lib and aspect_bazel_lib). The
# "unix" feature set is required: the default set lacks chmod, which
# rules_js runs when extracting npm packages.
if [ ! -x "$DEPS/cargo/bin/coreutils" ] && command -v cargo >/dev/null 2>&1; then
  log "building uutils coreutils from source (index.crates.io, ~2min)"
  cargo install coreutils --version 0.5.0 --features unix --root "$DEPS/cargo" >&2 \
    || log "WARNING: coreutils build failed"
fi

# bsdtar (needed by tar.bzl; the prebuilt binary is a GitHub release
# asset).
if ! command -v bsdtar >/dev/null 2>&1; then
  log "installing libarchive-tools (bsdtar)"
  DEBIAN_FRONTEND=noninteractive apt-get update -qq >&2 2>/dev/null
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libarchive-tools >&2 \
    || log "WARNING: bsdtar install failed"
fi

# Bootstrap dist (embedded by bb-storage's pkg/otel; the dist zip is a
# GitHub release asset). The same files ship in the npm package.
if [ ! -f "$DEPS/archives/bootstrap-dist/css/bootstrap.min.css" ]; then
  log "repackaging bootstrap 5.1.0 dist from npm"
  (
    cd "$DEPS/archives" &&
      curl -sfL https://registry.npmjs.org/bootstrap/-/bootstrap-5.1.0.tgz -o bootstrap-npm.tgz &&
      tar xzf bootstrap-npm.tgz &&
      mkdir -p bootstrap-dist/css bootstrap-dist/js &&
      cp package/dist/css/bootstrap.min.css bootstrap-dist/css/ &&
      cp package/dist/js/bootstrap.min.js bootstrap-dist/js/ &&
      rm -rf package bootstrap-npm.tgz
  ) || log "WARNING: bootstrap repackaging failed"
fi

# --- module overrides from the lockfile --------------------------------
: > "$OUT"
overridden=""
mods=$(grep -o '"[^"]*source.json"' "$LOCK" | sed 's/"//g;s|.*/modules/||;s|/source.json||' | sort -u)
for mv in $mods; do
  m=${mv%/*}
  v=${mv#*/}
  src=$(curl -sf "$BCR/modules/$m/$v/source.json") || { log "SKIP $mv (no source.json)"; continue; }
  url=$(echo "$src" | python3 -c "import json,sys; print(json.load(sys.stdin).get('url',''))")
  [ -z "$url" ] && continue

  dest="$DEPS/$m"
  if [ ! -d "$dest" ]; then
    # Fetchable via the mirror or directly? Then no override is needed.
    code=$(curl -s -o /dev/null -w "%{http_code}" -r 0-0 --max-time 20 "https://storage.googleapis.com/bazel-mirror/${url#https://}")
    case "$code" in 200 | 206) continue ;; esac
    code=$(curl -s -o /dev/null -w "%{http_code}" -r 0-0 --max-time 20 "$url")
    case "$code" in 200 | 206) continue ;; esac

    # Derive the git repo and ref from the archive URL.
    case "$url" in
      https://github.com/*/releases/download/*)
        repo=$(echo "$url" | sed -E 's|https://github.com/([^/]+/[^/]+)/releases/download/.*|\1|')
        ref=$(echo "$url" | sed -E 's|.*/releases/download/([^/]+)/.*|\1|')
        ;;
      https://github.com/*/archive/refs/tags/*)
        repo=$(echo "$url" | sed -E 's|https://github.com/([^/]+/[^/]+)/archive/refs/tags/.*|\1|')
        ref=$(echo "$url" | sed -E 's|.*/archive/refs/tags/(.*)\.(tar\.gz\|zip)|\1|')
        ;;
      https://github.com/*/archive/*)
        repo=$(echo "$url" | sed -E 's|https://github.com/([^/]+/[^/]+)/archive/.*|\1|')
        ref=$(echo "$url" | sed -E 's|.*/archive/(.*)\.(tar\.gz\|zip)|\1|')
        ;;
      *)
        log "MANUAL $mv ($url is unfetchable and not on GitHub)"
        continue
        ;;
    esac

    log "cloning $mv from $repo@$ref"
    if ! git clone --quiet --depth 1 --branch "$ref" "https://github.com/$repo.git" "$dest" 2>/dev/null; then
      # The ref may be a bare commit hash.
      git init -q "$dest" &&
        git -C "$dest" remote add origin "https://github.com/$repo.git" &&
        git -C "$dest" fetch -q --depth 1 origin "$ref" &&
        git -C "$dest" checkout -q FETCH_HEAD ||
        {
          log "FAIL clone $mv ($repo@$ref)"
          rm -rf "$dest"
          continue
        }
    fi

    # Apply the registry's patches.
    strip=$(echo "$src" | python3 -c "import json,sys; print(json.load(sys.stdin).get('patch_strip',0))")
    for p in $(echo "$src" | python3 -c "import json,sys; [print(k) for k in json.load(sys.stdin).get('patches',{})]"); do
      curl -sf "$BCR/modules/$m/$v/patches/$p" | patch -p"$strip" -d "$dest" -s --no-backup-if-mismatch ||
        log "WARNING: $m: patch $p did not apply cleanly"
    done
  fi

  # The module root within the clone: release archives are usually the
  # repo root (strip_prefix "<repo>-<version>"), but some modules live
  # in a subdirectory of a monorepo.
  sp=$(echo "$src" | python3 -c "import json,sys; print(json.load(sys.stdin).get('strip_prefix',''))")
  root="$dest"
  if [ -n "$sp" ] && [ -d "$dest/$sp" ]; then
    root="$dest/$sp"
  else
    rest=${sp#*/}
    if [ "$rest" != "$sp" ] && [ -n "$rest" ] && [ -d "$dest/$rest" ]; then
      root="$dest/$rest"
    fi
  fi

  # For registry archives, Bazel overlays the registry's MODULE.bazel
  # and overlay files onto the sources; do the same here.
  curl -sf "$BCR/modules/$m/$v/MODULE.bazel" -o "$root/MODULE.bazel" ||
    log "WARNING: $m: could not fetch registry MODULE.bazel"
  for f in $(echo "$src" | python3 -c "import json,sys; [print(k) for k in json.load(sys.stdin).get('overlay',{})]"); do
    mkdir -p "$root/$(dirname "$f")"
    curl -sf "$BCR/modules/$m/$v/overlay/$f" -o "$root/$f" ||
      log "WARNING: $m: could not fetch overlay $f"
  done

  echo "common --override_module=$m=$root" >> "$OUT"
  overridden="$overridden $m"
done

has_override() { case " $overridden " in *" $1 "*) return 0 ;; *) return 1 ;; esac }

# --- fix-ups for overridden modules ------------------------------------
# Git trees are not always identical to the packaged release archives,
# and some modules download prebuilt binaries at fetch time. Patch the
# local clones accordingly. Every patch is guarded so reruns are no-ops
# and pattern drift after a version bump degrades to a warning.
pyreplace() { # file, old, new, marker
  python3 - "$1" "$2" "$3" "$4" <<'EOF'
import sys
path, old, new, marker = sys.argv[1:5]
src = open(path).read()
if marker in src:
    sys.exit(0)  # already patched
if old not in src:
    print(f"WARNING: pattern not found in {path}; module version may have changed", file=sys.stderr)
    sys.exit(1)
open(path, "w").write(src.replace(old, new))
EOF
}

# rules_kotlin: the source tree's kotlin/internal package loads release
# packaging code that needs rules_pkg, which is only a dev_dependency
# and thus unavailable in non-root modules.
if has_override rules_kotlin && ! grep -q '"rules_pkg"' "$DEPS/rules_kotlin/MODULE.bazel"; then
  printf '\nbazel_dep(name = "rules_pkg", version = "1.0.1")\n' >> "$DEPS/rules_kotlin/MODULE.bazel"
  log "rules_kotlin: added rules_pkg dependency"
fi

# aspect_bazel_lib: without its prebuilt tool binaries (GitHub release
# assets), the source toolchains are used, which need rules_go to be a
# regular dependency.
if has_override aspect_bazel_lib; then
  pyreplace "$DEPS/aspect_bazel_lib/MODULE.bazel" '    repo_name = "io_bazel_rules_go",
    dev_dependency = True,
)' '    repo_name = "io_bazel_rules_go",
)' 'repo_name = "io_bazel_rules_go",
)' && log "aspect_bazel_lib: rules_go made a regular dependency"
fi

# bazel_lib + aspect_bazel_lib: substitute the coreutils prebuilt binary.
for m in bazel_lib aspect_bazel_lib; do
  f="$DEPS/$m/lib/private/coreutils_toolchain.bzl"
  if has_override "$m" && [ -f "$f" ]; then
    pyreplace "$f" '    rctx.download_and_extract(
        url = url,
        stripPrefix = filename.replace(".zip", "").replace(".tar.gz", ""),
        integrity = COREUTILS_VERSIONS[rctx.attr.version][platform]["sha256"],
    )' "    if platform == \"linux_amd64\":
        # Sandbox lacks egress to GitHub releases; use a locally built
        # uutils coreutils.
        rctx.symlink(\"$DEPS/cargo/bin/coreutils\", \"coreutils\")
    else:
        rctx.download_and_extract(
            url = url,
            stripPrefix = filename.replace(\".zip\", \"\").replace(\".tar.gz\", \"\"),
            integrity = COREUTILS_VERSIONS[rctx.attr.version][platform][\"sha256\"],
        )" "cargo/bin/coreutils" && log "$m: coreutils substituted"
  fi
done

# yq.bzl: substitute the yq prebuilt binary.
f="$DEPS/yq.bzl/yq/toolchain/platforms.bzl"
if has_override yq.bzl && [ -f "$f" ]; then
  pyreplace "$f" '    rctx.download(
        url = url,
        output = "yq.exe" if is_windows else "yq",
        executable = True,
        integrity = YQ_VERSIONS[rctx.attr.version][release_platform],
    )' "    if release_platform == \"linux_amd64\":
        # Sandbox lacks egress to GitHub releases; use a locally built yq.
        rctx.symlink(\"$DEPS/bin/yq\", \"yq\")
    else:
        rctx.download(
            url = url,
            output = \"yq.exe\" if is_windows else \"yq\",
            executable = True,
            integrity = YQ_VERSIONS[rctx.attr.version][release_platform],
        )" "deps/bin/yq" && log "yq.bzl: yq substituted"
fi

# tar.bzl: substitute the bsdtar prebuilt binary.
f="$DEPS/tar.bzl/tar/toolchain/platforms.bzl"
if has_override tar.bzl && [ -f "$f" ]; then
  pyreplace "$f" '    rctx.download(
        url = url,
        output = binary,
        executable = True,
        sha256 = sha256,
    )' '    if rctx.attr.platform == "linux_amd64":
        # Sandbox lacks egress to GitHub releases; use the system bsdtar.
        rctx.symlink("/usr/bin/bsdtar", binary)
    else:
        rctx.download(
            url = url,
            output = binary,
            executable = True,
            sha256 = sha256,
        )' "usr/bin/bsdtar" && log "tar.bzl: bsdtar substituted"
fi

# jsonnet_go: its cpp_jsonnet http_archive points at a GitHub release
# asset. Clone the pinned commit and use a local_repository instead.
f="$DEPS/jsonnet_go/MODULE.bazel"
if has_override jsonnet_go && [ -f "$f" ] && ! grep -q "jsonnet-src" "$f"; then
  githash=$(sed -n 's/^CPP_JSONNET_GITHASH = "\(.*\)"$/\1/p' "$f")
  if [ -n "$githash" ] && [ ! -d "$DEPS/archives/jsonnet-src" ]; then
    git init -q "$DEPS/archives/jsonnet-src" &&
      git -C "$DEPS/archives/jsonnet-src" remote add origin https://github.com/google/jsonnet.git &&
      git -C "$DEPS/archives/jsonnet-src" fetch -q --depth 1 origin "$githash" &&
      git -C "$DEPS/archives/jsonnet-src" checkout -q FETCH_HEAD ||
      log "WARNING: cpp_jsonnet clone failed"
  fi
  if [ -d "$DEPS/archives/jsonnet-src" ]; then
    pyreplace "$f" 'http_archive(
    name = "cpp_jsonnet",
    sha256 = CPP_JSONNET_SHA256,
    strip_prefix = CPP_JSONNET_STRIP_PREFIX,
    urls = [CPP_JSONNET_URL],
)' "local_repository = use_repo_rule(\"@bazel_tools//tools/build_defs/repo:local.bzl\", \"local_repository\")

local_repository(
    name = \"cpp_jsonnet\",
    path = \"$DEPS/archives/jsonnet-src\",
)" "jsonnet-src" && log "jsonnet_go: cpp_jsonnet localized"
  fi
fi

# --- bb-storage: bootstrap dist is a GitHub release asset --------------
# bb-storage is a git_override in MODULE.bazel (git fetches work), but
# its pkg/otel embeds Bootstrap via an unfetchable http_archive. Clone
# the pinned commit, point the archive at the npm-repackaged dist, and
# override the module.
bbs_commit=$(python3 - "$CLAUDE_PROJECT_DIR/MODULE.bazel" <<'EOF'
import re, sys
src = open(sys.argv[1]).read()
m = re.search(r'git_override\(\s*module_name = "com_github_buildbarn_bb_storage",\s*commit = "([0-9a-f]+)"', src)
print(m.group(1) if m else "")
EOF
)
bbs="$DEPS/com_github_buildbarn_bb_storage"
if [ -n "$bbs_commit" ]; then
  if [ ! -d "$bbs" ]; then
    log "cloning bb-storage@$bbs_commit"
    git init -q "$bbs" &&
      git -C "$bbs" remote add origin https://github.com/buildbarn/bb-storage.git &&
      git -C "$bbs" fetch -q --depth 1 origin "$bbs_commit" &&
      git -C "$bbs" checkout -q FETCH_HEAD ||
      { log "WARNING: bb-storage clone failed"; rm -rf "$bbs"; }
  fi
  if [ -d "$bbs" ]; then
    pyreplace "$bbs/MODULE.bazel" 'http_archive(
    name = "com_github_twbs_bootstrap",
    build_file_content = """exports_files(["css/bootstrap.min.css", "js/bootstrap.min.js"])""",
    sha256 = "395342b2974e3350560e65752d36aab6573652b11cc6cb5ef79a2e5e83ad64b1",
    strip_prefix = "bootstrap-5.1.0-dist",
    urls = ["https://github.com/twbs/bootstrap/releases/download/v5.1.0/bootstrap-5.1.0-dist.zip"],
)' "new_local_repository = use_repo_rule(\"@bazel_tools//tools/build_defs/repo:local.bzl\", \"new_local_repository\")

new_local_repository(
    name = \"com_github_twbs_bootstrap\",
    build_file_content = \"\"\"exports_files([\"css/bootstrap.min.css\", \"js/bootstrap.min.js\"])\"\"\",
    path = \"$DEPS/archives/bootstrap-dist\",
)" "bootstrap-dist" && log "bb-storage: bootstrap localized"
    echo "common --override_module=com_github_buildbarn_bb_storage=$bbs" >> "$OUT"
  fi
else
  log "WARNING: could not determine bb-storage commit from MODULE.bazel"
fi

log "$(grep -c override_module "$OUT" 2>/dev/null || echo 0) module overrides written to $OUT"
