#!/bin/sh
# Create and remove repository scratch worktrees without changing this checkout.
set -eu

usage() {
    cat >&2 <<'EOF'
usage:
  scripts/worktree.sh add <name> [<base-ref>]
  scripts/worktree.sh remove <name>
  scripts/worktree.sh list
EOF
    exit 2
}

[ "$#" -gt 0 ] || usage
command=$1
shift
case "$command" in
    add) [ "$#" -ge 1 ] && [ "$#" -le 2 ] || usage ;;
    remove) [ "$#" -eq 1 ] || usage ;;
    list) [ "$#" -eq 0 ] || usage ;;
    *) usage ;;
esac

repo_root=$(git rev-parse --path-format=absolute --show-toplevel 2>/dev/null) || {
    echo "error: run in a repository worktree" >&2
    exit 1
}
repo_root=$(cd "$repo_root" && pwd -P)
scratch_root=${WORKTREE_ROOT:-$HOME/workspace/agent-message-queue-wt}
case "$scratch_root" in
    /*) ;;
    *) echo "error: WORKTREE_ROOT must be an absolute path" >&2; exit 1 ;;
esac

# Canonicalize an existing root, or create its closest existing parent first.
# Untrusting an unresolved source path avoids writing through a lexical /tmp alias.
requested_root=$scratch_root
if ! scratch_root=$(cd "$scratch_root" 2>/dev/null && pwd -P); then
    scratch_root=$requested_root
    [ "$command" = add ] || { echo "error: worktree root is unavailable: $scratch_root" >&2; exit 1; }
    parent=$scratch_root
    while [ ! -d "$parent" ]; do
        [ "$parent" != / ] || {
            echo "error: cannot resolve worktree root parent" >&2
            exit 1
        }
        parent=$(dirname "$parent")
    done
    parent=$(cd "$parent" && pwd -P)
    case "$parent" in
        /tmp|/tmp/*|/private/tmp|/private/tmp/*)
            echo "error: worktree root cannot be under /tmp or /private/tmp" >&2
            exit 1
            ;;
    esac
    mkdir -p "$scratch_root"
    scratch_root=$(cd "$scratch_root" && pwd -P)
fi
case "$scratch_root" in
    /tmp|/tmp/*|/private/tmp|/private/tmp/*)
        echo "error: worktree root cannot be under /tmp or /private/tmp" >&2
        exit 1
        ;;
esac

case "$command" in
    add|remove)
        case "$1" in
            ""|*/*|*\*)
                echo "error: name must be nonempty and contain no path separator" >&2
                exit 2
                ;;
        esac
        path="$scratch_root/$1"
        ;;
esac

case "$command" in
    add)
        base_ref=${2:-origin/main}
        [ ! -e "$path" ] || {
            echo "error: worktree path already exists: $path" >&2
            exit 1
        }
        # Fetch only the default; an explicit base is used as named.
        if [ "$#" -eq 1 ]; then
            git -C "$repo_root" fetch --no-tags origin main
        fi
        git -C "$repo_root" rev-parse --verify "$base_ref^{commit}" >/dev/null || {
            echo "error: unknown base ref: $base_ref" >&2
            exit 1
        }
        git -C "$repo_root" worktree add --detach "$path" "$base_ref"
        base=$(git -C "$path" rev-parse HEAD) || {
            echo "error: cannot resolve new worktree HEAD: $path" >&2
            exit 1
        }
        printf 'worktree=%s\nbase=%s\n' "$path" "$base"
        ;;
    remove)
        [ -d "$path" ] || {
            echo "error: worktree path does not exist: $path" >&2
            exit 1
        }
        top=$(git -C "$path" rev-parse --path-format=absolute --show-toplevel 2>/dev/null)
        top=$(cd "$top" && pwd -P)
        [ "$top" = "$path" ] || {
            echo "error: not a git worktree: $path" >&2
            exit 1
        }
        # No force path or branch deletion; git preserves modified/untracked work.
        git -C "$repo_root" worktree remove "$path"
        printf 'removed=%s\n' "$path"
        ;;
    list)
        git -C "$repo_root" worktree list --porcelain | while IFS= read -r line; do
            case "$line" in
                worktree\ *)
                    printf '%s\n' "$line"
                    ;;
                HEAD\ *)
                    printf '%s\n' "$line"
                    head=${line#HEAD }
                    if ! git -C "$repo_root" rev-parse --verify "refs/remotes/origin/main^{commit}" >/dev/null 2>&1 ||
                        ! git -C "$repo_root" cat-file -e "$head^{commit}" 2>/dev/null; then
                        status=origin-main-or-head-unavailable
                    else
                        git -C "$repo_root" merge-base --is-ancestor "$head" refs/remotes/origin/main 2>/dev/null
                        rc=$?
                        case "$rc" in
                            0) status=ancestor-of-origin/main ;;
                            1) status=not-ancestor-of-origin/main ;;
                            *) status=origin-main-or-head-unavailable ;;
                        esac
                    fi
                    printf '  %s\n' "$status"
                    ;;
                *)
                    printf '%s\n' "$line"
                    ;;
            esac
        done
        ;;
esac
