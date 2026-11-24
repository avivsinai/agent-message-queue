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

repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || {
    echo "error: run in a repository worktree" >&2
    exit 1
}
scratch_root="${WORKTREE_ROOT:-$HOME/workspace/agent-message-queue-wt}"
case "$scratch_root" in
    /tmp|/tmp/*|/private/tmp|/private/tmp/*)
        echo "error: worktree root cannot be under /tmp or /private/tmp" >&2
        exit 1
        ;;
esac

worktree_path() {
    case "$1" in
        ""|*/*|*\*)
            echo "error: name must be nonempty and contain no path separator" >&2
            exit 2
            ;;
    esac
    printf '%s/%s\n' "$scratch_root" "$1"
}

case "$command" in
    add)
        [ "$#" -ge 1 ] && [ "$#" -le 2 ] || usage
        path=$(worktree_path "$1")
        base_ref=${2:-origin/main}
        [ ! -e "$path" ] || {
            echo "error: worktree path already exists: $path" >&2
            exit 1
        }
        # A caller-provided explicit base is used as named; only the default
        # requires this fetch. A detached HEAD never retargets a branch.
        if [ "$#" -eq 1 ]; then
            git -C "$repo_root" fetch --no-tags origin main
        fi
        git -C "$repo_root" rev-parse --verify "$base_ref^{commit}" >/dev/null || {
            echo "error: unknown base ref: $base_ref" >&2
            exit 1
        }
        mkdir -p "$scratch_root"
        git -C "$repo_root" worktree add --detach "$path" "$base_ref"
        printf 'worktree=%s\nbase=%s\n' "$path" "$(git -C "$path" rev-parse HEAD)"
        ;;
    remove)
        [ "$#" -eq 1 ] || usage
        path=$(worktree_path "$1")
        [ -d "$path" ] || {
            echo "error: worktree path does not exist: $path" >&2
            exit 1
        }
        [ "$(git -C "$path" rev-parse --show-toplevel 2>/dev/null)" = "$path" ] || {
            echo "error: not a git worktree: $path" >&2
            exit 1
        }
        # Git's refusal protects modified and untracked work. There is no
        # force path and no local or remote branch deletion.
        git -C "$repo_root" worktree remove "$path"
        printf 'removed=%s\n' "$path"
        ;;
    list)
        [ "$#" -eq 0 ] || usage
        git -C "$repo_root" worktree list --porcelain | while IFS= read -r line; do
            case "$line" in
                worktree\ *)
                    path=${line#worktree }
                    printf '%s\n' "$line"
                    ;;
                HEAD\ *)
                    head=${line#HEAD }
                    if git -C "$repo_root" merge-base --is-ancestor "$head" origin/main 2>/dev/null; then
                        status=ancestor-of-origin/main
                    else
                        status=not-ancestor-of-origin/main
                    fi
                    printf '%s\n  %s\n' "$line" "$status"
                    ;;
                *)
                    printf '%s\n' "$line"
                    ;;
            esac
        done
        ;;
    *)
        usage
        ;;
esac
