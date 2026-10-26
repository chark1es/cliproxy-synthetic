#!/usr/bin/env bash
# Sanity-check a built plugin: it must export the entry point the host looks
# up, and must not need a newer glibc than Debian 12 (2.36) provides.
#
# usage: scripts/verify.sh path/to/synthetic.so
set -euo pipefail

so="${1:?usage: verify.sh path/to/synthetic.so}"
max_glibc="2.36"

nm -D "$so" | grep -q ' T cliproxy_plugin_init$' || {
  echo "FAIL: $so does not export cliproxy_plugin_init" >&2
  exit 1
}

need="$(objdump -T "$so" | grep -o 'GLIBC_[0-9.]*' | sed 's/GLIBC_//' | sort -V | tail -1)"
if [ -n "$need" ] && [ "$(printf '%s\n%s\n' "$need" "$max_glibc" | sort -V | tail -1)" != "$max_glibc" ]; then
  echo "FAIL: $so needs glibc $need, newer than $max_glibc" >&2
  exit 1
fi
echo "ok: $so exports cliproxy_plugin_init, needs glibc ${need:-none} (max $max_glibc)"
