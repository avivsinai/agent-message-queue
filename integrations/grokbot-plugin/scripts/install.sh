#!/usr/bin/env bash
set -euo pipefail

dest="${AMQ_BIN:-$HOME/.amq/bin}"
manifest="${AMQ_RELEASE_MANIFEST:-}"
if [[ -z "$manifest" ]]; then
  echo "AMQ_RELEASE_MANIFEST is required (sha256  name per line)" >&2
  exit 2
fi
if [[ ! -f "$manifest" || -L "$manifest" ]]; then
  echo "release manifest must be a regular file" >&2
  exit 2
fi
umask 077
mkdir -p "$dest"
while read -r sum name; do
  [[ -z "${sum:-}" || -z "${name:-}" ]] && continue
  src="${AMQ_RELEASE_DIR:-}/$name"
  if [[ ! -f "$src" || -L "$src" ]]; then
    echo "missing release file $name" >&2
    exit 1
  fi
  got="$(sha256sum "$src" | awk '{print $1}')"
  if [[ "$got" != "$sum" ]]; then
    echo "sha256 mismatch for $name" >&2
    exit 1
  fi
  case "$name" in
    amq|amq-bridge|dumbpipe) ;;
    *)
      echo "unexpected binary $name" >&2
      exit 1
      ;;
  esac
  cp "$src" "$dest/$name"
  chmod 0755 "$dest/$name"
done <"$manifest"
echo "installed into $dest"
