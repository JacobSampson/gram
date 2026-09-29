#!/usr/bin/env bash
# Build frozen OLD and current NEW tunnel binaries and run the black-box
# agent/gateway compatibility matrix. See tunnel/compatibility/README.md.
#
# Usage: tunnel/compatibility/run.sh [--old-ref REF] [--main-ref REF]
#                                    [--image REF|--no-image] [--main-image REF|--no-main-image]
#                                    [--work DIR] [--build-only] [-- HARNESS_FLAGS...]
set -euo pipefail

repo_root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
# old: the 0.1.0 baseline, which joins non-root paths beneath the pinned path.
# main: pre-feature main at tunnel 0.1.1, which keeps OAuth back-channel paths.
old_ref=ee14f2af29
main_ref=ecf54b81f4
image=ghcr.io/speakeasy-api/gram-tunnel-agent:0.1.0
main_image=ghcr.io/speakeasy-api/gram-tunnel-agent:0.1.1
work=${COMPAT_WORKDIR:-${TMPDIR:-/tmp}/gram-tunnel-compat}
build_only=false
harness_args=()

while [ $# -gt 0 ]; do
    case "$1" in
    --old-ref) old_ref=$2; shift 2 ;;
    --main-ref) main_ref=$2; shift 2 ;;
    --image) image=$2; shift 2 ;;
    --no-image) image=""; main_image=""; shift ;;
    --main-image) main_image=$2; shift 2 ;;
    --no-main-image) main_image=""; shift ;;
    --work) work=$2; shift 2 ;;
    --build-only) build_only=true; shift ;;
    --) shift; harness_args=("$@"); break ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

# Resolve the pinned Go toolchain once; the old snapshot has no mise config.
go_bin=$(cd "$repo_root" && mise which go)
export GOWORK=off GOFLAGS=-mod=readonly GOTOOLCHAIN=local
# Never remove caller-selected paths. Each build owns a fresh private child.
mkdir -p "$work"
work=$(cd "$work" && pwd -P)
work=$(mktemp -d "$work/run.XXXXXXXX")
bin_dir="$work/bin"
stamp=$(date -u +%Y%m%dT%H%M%SZ)
out_dir="$work/runs/$stamp"
mkdir -p "$bin_dir/new" "$out_dir"

sha() { shasum -a 256 "$1" | cut -d' ' -f1; }

# build_frozen NAME REF exports REF's tunnel source with git archive (never the
# working tree), adds the baseline-API testgateway driver as the only extra
# file, builds the agent and gateway into $bin_dir/NAME and prints provenance.
build_frozen() {
    local name=$1 ref=$2 commit src
    if ! commit=$(git -C "$repo_root" rev-parse --verify "$ref^{commit}" 2>/dev/null); then
        echo "Missing frozen revision $ref. Fetch full history (git fetch --unshallow origin for a shallow checkout), then rerun." >&2
        exit 1
    fi
    src="$work/$name-src"
    mkdir -p "$src" "$bin_dir/$name"
    echo "==> exporting frozen $name tunnel source at $commit" >&2
    # Only go.mod/go.sum and tunnel/ are needed, matching tunnel/Dockerfile.
    git -C "$repo_root" archive "$commit" go.mod go.sum tunnel | tar -x -C "$src"
    if [ -e "$src/tunnel/compatibility" ]; then
        echo "$name ref already contains tunnel/compatibility; refusing to overwrite it" >&2
        exit 1
    fi
    mkdir -p "$src/tunnel/compatibility/testgateway"
    cp "$repo_root/tunnel/compatibility/testgateway/main.go" \
        "$repo_root/tunnel/compatibility/testgateway/gateway_old.go" \
        "$src/tunnel/compatibility/testgateway/"
    echo "==> building frozen $name agent and gateway driver" >&2
    (cd "$src" && CGO_ENABLED=0 "$go_bin" build -trimpath -o "$bin_dir/$name/tunnel-agent" ./tunnel/cmd/tunnel-agent) >&2
    (cd "$src" && CGO_ENABLED=0 "$go_bin" build -trimpath -o "$bin_dir/$name/testgateway" ./tunnel/compatibility/testgateway) >&2
    printf '{"ref": "%s", "commit": "%s", "source": "git archive (go.mod go.sum tunnel) + testgateway driver", "agent_sha256": "%s", "gateway_sha256": "%s"}' \
        "$ref" "$commit" "$(sha "$bin_dir/$name/tunnel-agent")" "$(sha "$bin_dir/$name/testgateway")"
}
old_json=$(build_frozen old "$old_ref")
main_json=$(build_frozen main "$main_ref")

echo "==> building NEW agent and gateway driver from the working tree"
(cd "$repo_root" && CGO_ENABLED=0 "$go_bin" build -trimpath -o "$bin_dir/new/tunnel-agent" ./tunnel/cmd/tunnel-agent)
(cd "$repo_root" && CGO_ENABLED=0 "$go_bin" build -trimpath -tags compatnew -o "$bin_dir/new/testgateway" ./tunnel/compatibility/testgateway)
(cd "$repo_root" && CGO_ENABLED=0 "$go_bin" build -trimpath -o "$bin_dir/new/faultagent" ./tunnel/compatibility/faultagent)
(cd "$repo_root" && "$go_bin" build -o "$bin_dir/harness" ./tunnel/compatibility/harness)

# pull_image REF pulls a published agent image and prints its provenance.
pull_image() {
    local ref=$1
    if [ -z "$ref" ]; then
        echo null
        return
    fi
    echo "==> pulling $ref" >&2
    if docker pull --quiet "$ref" >/dev/null; then
        docker image inspect "$ref" --format '{"ref":{{json .RepoTags}},"repo_digests":{{json .RepoDigests}},"image_id":{{json .Id}},"architecture":{{json .Architecture}},"revision_label":{{json (index .Config.Labels "org.opencontainers.image.revision")}},"version_label":{{json (index .Config.Labels "org.opencontainers.image.version")}}}'
    else
        echo "cannot pull $ref; its agent kind will fail" >&2
        echo null
    fi
}
image_json=$(pull_image "$image")
main_image_json=$(pull_image "$main_image")

new_head=$(git -C "$repo_root" rev-parse HEAD)
dirty_tunnel=$(git -C "$repo_root" status --porcelain -- tunnel go.mod go.sum | grep -v ' tunnel/compatibility/' || true)
dirty_hash=$( (git -C "$repo_root" diff HEAD -- tunnel go.mod go.sum ':!tunnel/compatibility'; \
    git -C "$repo_root" ls-files --others --exclude-standard -- tunnel ':!tunnel/compatibility' | sort | while read -r f; do cat "$repo_root/$f"; done) | shasum -a 256 | cut -d' ' -f1)
cat >"$out_dir/provenance.json" <<EOF
{
  "go": "$("$go_bin" version)",
  "old": $old_json,
  "main": $main_json,
  "new": {"head": "$new_head", "worktree_dirty": $([ -n "$dirty_tunnel" ] && echo true || echo false), "tunnel_diff_sha256": "$dirty_hash",
          "agent_sha256": "$(sha "$bin_dir/new/tunnel-agent")", "gateway_sha256": "$(sha "$bin_dir/new/testgateway")"},
  "image": $image_json,
  "main_image": $main_image_json
}
EOF
cat "$out_dir/provenance.json"

if [ "$build_only" = true ]; then
    exit 0
fi

image_flag=()
if [ -n "$image" ]; then
    image_flag+=(-image "$image")
fi
if [ -n "$main_image" ]; then
    image_flag+=(-main-image "$main_image")
fi
exec "$bin_dir/harness" -bin-dir "$bin_dir" -out "$out_dir" -provenance "$out_dir/provenance.json" \
    ${image_flag[@]+"${image_flag[@]}"} ${harness_args[@]+"${harness_args[@]}"}
