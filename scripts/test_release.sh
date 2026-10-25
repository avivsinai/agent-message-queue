#!/usr/bin/env bash
set -euo pipefail

# Happy path: the operator requests an exact version and receives the PR URL
# only after that dispatch completes successfully. No GitHub writes are made.
ROOT="$(git rev-parse --show-toplevel)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/bin"
cat > "$scratch/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'repo view --json nameWithOwner --jq .nameWithOwner') echo example/amq ;;
  'run list --repo example/amq --workflow release-please.yml --limit 1 --json databaseId --jq .[0].databaseId // 0') echo 12 ;;
  'workflow run release-please.yml --repo example/amq --ref main -f version=9.9.9')
    echo dispatched > "$DISPATCH_LOG" ;;
  'run list --repo example/amq --workflow release-please.yml --event workflow_dispatch --limit 100 --json databaseId,displayTitle')
    printf '%s\n' '[{"databaseId":13,"displayTitle":"Release request 9.9.9"}]' ;;
  'run view 13 --repo example/amq --json status,conclusion,url')
    printf '%s\n' '{"status":"completed","conclusion":"success","url":"https://example.test/run/13"}' ;;
  'api --paginate --slurp repos/example/amq/pulls?state=open&base=main&head=example%3Arelease-please--branches--main&per_page=100')
    test -f "$DISPATCH_LOG"
    printf '%s\n' '[[{"state":"open","title":"chore(release): v9.9.9","html_url":"https://example.test/pull/7","base":{"ref":"main"},"head":{"ref":"release-please--branches--main","repo":{"full_name":"example/amq"}}}]]' ;;
  *) echo "Unexpected gh invocation: $*" >&2; exit 1 ;;
esac
GH
chmod +x "$scratch/bin/gh"
actual="$(PATH="$scratch/bin:$PATH" DISPATCH_LOG="$scratch/dispatched" \
  bash "$ROOT/scripts/release.sh" 9.9.9)"
test "$actual" = https://example.test/pull/7
test "$(cat "$scratch/dispatched")" = dispatched
echo 'release request test ok'
