#!/bin/bash
# Install mibot-lite as a systemd service for an already configured account.
#
# One binary, one unit. Updating is `.update`, which replaces the binary in
# place and restarts this same unit, so there is no second unit and no timer.
set -euo pipefail
umask 077

if [[ "${1:-}" == "--help" ]]; then
  printf '%s\n' 'Usage: bash scripts/install-service.sh --binary PATH [--root DIRECTORY]' \
    'Requires Linux/systemd, root, and a configured account (config.json) under the root.' \
    'Defaults the root to the current directory.' \
    'Copies the binary into the root, installs mibot-lite.service, starts and enables it.'
  exit 0
fi

binary=
root=$(pwd -P)
while [[ $# -gt 0 ]]; do
  case "$1" in
    --root|--binary)
      [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || { echo "Missing value for $1" >&2; exit 2; }
      case "$1" in
        --root) root="$2" ;;
        --binary) binary="$2" ;;
      esac
      shift 2 ;;
    *) echo "Unsupported argument: $1" >&2; exit 2 ;;
  esac
done

[[ $(uname -s) == Linux && $EUID == 0 ]] || { echo "Run on Linux as root" >&2; exit 1; }
[[ -n "$binary" ]] || { echo "Pass the built binary with --binary PATH" >&2; exit 1; }
[[ -f "$binary" && -x "$binary" ]] || { echo "Not an executable file: $binary" >&2; exit 1; }
binary=$(cd -- "$(dirname -- "$binary")" && printf '%s/%s' "$(pwd -P)" "$(basename -- "$binary")")
root=$(cd -- "$root" && pwd -P)
# systemd reads '%' as a specifier, so a path that cannot appear verbatim in
# a unit is rejected here rather than producing a unit that means something
# else.
[[ "$root" =~ ^/[A-Za-z0-9._/-]*$ ]] || { echo "Deployment path must be absolute and free of unit metacharacters: $root" >&2; exit 1; }
[[ -f "$root/config.json" ]] || { echo "Log in first: $root/config.json is missing" >&2; exit 1; }

for executable in /usr/bin/systemctl /usr/bin/systemd-analyze; do
  [[ -x "$executable" ]] || { echo "Missing executable: $executable" >&2; exit 1; }
done

state=$(/usr/bin/systemctl show mibot-lite -p ActiveState --value)
case "$state" in inactive|failed|"") ;; *) echo "mibot-lite is $state; stop it before installing over it" >&2; exit 1 ;; esac

# The new binary reads the deployment before anything is installed: one that
# cannot read this account never becomes the service.
"$binary" --check --root "$root"

installed="$root/mibot-lite"
unit=/etc/systemd/system/mibot-lite.service
template=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../deploy" && pwd -P)/mibot-lite.service
[[ -f "$template" ]] || { echo "Missing unit template: $template" >&2; exit 1; }

install -m 755 "$binary" "$installed"
# systemd-analyze reads the file name as the unit name, so the rendered
# copy has to be called mibot-lite.service; a bare mktemp name is rejected
# with "Failed to prepare filename".
staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT
rendered="$staging/mibot-lite.service"
sed -e "s|@ROOT@|$root|g" -e "s|@BINARY@|$installed|g" "$template" > "$rendered"
if grep -q '@[A-Z][A-Z]*@' "$rendered"; then
  echo "Unit template has unsubstituted placeholders" >&2; exit 1
fi
/usr/bin/systemd-analyze verify "$rendered"
install -m 644 "$rendered" "$unit"

/usr/bin/systemctl daemon-reload
/usr/bin/systemctl reset-failed mibot-lite 2>/dev/null || true
since=$(date '+%Y-%m-%d %H:%M:%S')
/usr/bin/systemctl enable --now mibot-lite

# Not `journalctl | grep -q`: grep leaves on the first match, journalctl
# dies of SIGPIPE, and `set -o pipefail` turns that into a failed check
# over a service that started perfectly well.
for _ in $(seq 30); do
  log=$(/usr/bin/journalctl -u mibot-lite --since "$since" --no-pager -o cat 2>/dev/null || true)
  case $log in
    *msg=runtime.ready*)
      printf '%s\n' 'MiBot Lite is ready and enabled at boot.' 'Verify .help and .ping in Telegram.' 'Logs: journalctl -u mibot-lite -f'
      exit 0
      ;;
  esac
  /usr/bin/systemctl is-active --quiet mibot-lite || break
  sleep 2
done
echo "Startup did not reach runtime.ready; inspect: journalctl -u mibot-lite -n 50" >&2
exit 1
