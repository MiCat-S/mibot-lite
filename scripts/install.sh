#!/bin/bash
# One-command install for MiBot Lite.
#
#   bash <(curl -fsSL https://raw.githubusercontent.com/MiCat-S/mibot-lite/main/scripts/install.sh)
#
# Use the process substitution form, not `curl … | bash`: signing in asks
# for a phone number and a code, and a piped script has no keyboard. The
# installer opens /dev/tty when it can and says what to run when it cannot.
#
# It downloads the released binary for this machine, checks it against the
# release's own checksums, signs the account in if the directory has no
# session yet, installs the systemd unit and starts the service. Running it
# again upgrades the binary in place and leaves the account alone.
set -euo pipefail
umask 077

REPO=${MIBOT_REPO:-MiCat-S/mibot-lite}
ROOT=${MIBOT_ROOT:-/root/mibot-lite}
SERVICE=mibot-lite.service
UNIT=/etc/systemd/system/$SERVICE
WITH_SERVICE=1

for argument in "$@"; do
  case "$argument" in
    --root) shift; ROOT=${1:?--root needs a directory}; shift || true ;;
    --root=*) ROOT=${argument#*=} ;;
    --repo=*) REPO=${argument#*=} ;;
    # Everything except the systemd unit: for trying the installer out
    # without touching a running deployment.
    --no-service) WITH_SERVICE=0 ;;
    --help|-h)
      printf '%s\n' \
        'Usage: install.sh [--root DIR] [--repo OWNER/NAME] [--no-service]' \
        '' \
        'Downloads the latest release for this machine, verifies its SHA-256,' \
        'signs in when needed, and installs the service. Safe to re-run: it' \
        'upgrades the binary and leaves config.json and data/ untouched.'
      exit 0 ;;
  esac
done

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mError:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || die "this installer targets Linux; build from source for $(uname -s)"
[ "${EUID:-$(id -u)}" = 0 ] || die "run as root (the service and its unit are installed system-wide)"
for tool in curl install systemctl; do
  command -v "$tool" > /dev/null || die "missing required command: $tool"
done
command -v sha256sum > /dev/null || command -v shasum > /dev/null || die "need sha256sum or shasum to verify the download"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "no released build for $(uname -m); build from source" ;;
esac
ASSET="mibot-lite-linux-$ARCH"

# A service running on *this* directory must not have its binary swapped
# underneath it mid-request, so it is stopped first and started again at
# the end. A service running on some other directory is none of this
# install's business: asking systemd which root the unit actually points
# at is what keeps `--root somewhere-else` from taking down a live bot,
# which is exactly what an earlier version of this script did.
OWNS_SERVICE=0
if [ "$WITH_SERVICE" = 1 ] && systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
  RUNNING_ROOT=$(systemctl show "$SERVICE" -p WorkingDirectory --value 2>/dev/null || true)
  if [ "$RUNNING_ROOT" = "$ROOT" ]; then
    OWNS_SERVICE=1
  else
    say "A mibot-lite service is running on ${RUNNING_ROOT:-another directory}; leaving it alone"
  fi
fi

say "Fetching the latest release of $REPO"
RELEASE=$(mktemp) && trap 'rm -rf "$RELEASE" "${WORK:-}"' EXIT
curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" -o "$RELEASE" \
  || die "could not read the release list; check the network and that $REPO has a release"

url_of() { sed -n 's/.*"browser_download_url": *"\([^"]*\/'"$1"'\)".*/\1/p' "$RELEASE" | head -1; }
TAG=$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$RELEASE" | head -1)
BINARY_URL=$(url_of "$ASSET")
SUMS_URL=$(url_of checksums.txt)
[ -n "$BINARY_URL" ] || die "release ${TAG:-?} has no $ASSET build"
[ -n "$SUMS_URL" ] || die "release ${TAG:-?} has no checksums.txt; refusing to install an unverified binary"
say "Release $TAG, $ASSET"

WORK=$(mktemp -d)
curl -fsSL "$BINARY_URL" -o "$WORK/$ASSET" || die "download failed"
curl -fsSL "$SUMS_URL" -o "$WORK/checksums.txt" || die "could not download checksums.txt"

WANT=$(awk -v name="$ASSET" '$2 == name || $2 == "*" name {print $1}' "$WORK/checksums.txt" | head -1)
[ ${#WANT} = 64 ] || die "checksums.txt has no entry for $ASSET"
if command -v sha256sum > /dev/null; then
  GOT=$(sha256sum "$WORK/$ASSET" | awk '{print $1}')
else
  GOT=$(shasum -a 256 "$WORK/$ASSET" | awk '{print $1}')
fi
[ "$GOT" = "$WANT" ] || die "SHA-256 mismatch: the download does not match the release"
say "SHA-256 verified"

mkdir -p "$ROOT"
chmod 700 "$ROOT"

# Sign in before anything is installed: a deployment with no account is
# not worth starting, and the prompts need a keyboard.
if ! grep -q '"session"' "$ROOT/config.json" 2>/dev/null; then
  say "This directory has no account yet; signing in"
  echo "   You will need the api_id and api_hash from https://my.telegram.org,"
  echo "   your phone number, and the code Telegram sends you."
  chmod 755 "$WORK/$ASSET"
  if [ -t 0 ]; then
    "$WORK/$ASSET" --login --root "$ROOT"
  elif [ -r /dev/tty ]; then
    "$WORK/$ASSET" --login --root "$ROOT" < /dev/tty
  else
    install -m 755 "$WORK/$ASSET" "$ROOT/mibot-lite"
    die "no terminal to ask for the code on. The binary is installed; run:
    $ROOT/mibot-lite --login --root $ROOT
  then run this installer again."
  fi
fi

if [ "$OWNS_SERVICE" = 1 ]; then
  say "Stopping the running service to replace its binary"
  systemctl stop "$SERVICE"
fi
install -m 755 "$WORK/$ASSET" "$ROOT/mibot-lite"

say "Checking that this build can read $ROOT"
"$ROOT/mibot-lite" --check --root "$ROOT" || die "the new binary cannot read this deployment; nothing was started"

if [ "$WITH_SERVICE" = 0 ]; then
  say "Installed to $ROOT/mibot-lite (service untouched)"
  exit 0
fi

# Installing over a unit that points somewhere else would repoint the
# service at this directory without saying so.
if [ -f "$UNIT" ]; then
  EXISTING_ROOT=$(systemctl show "$SERVICE" -p WorkingDirectory --value 2>/dev/null || true)
  if [ -n "$EXISTING_ROOT" ] && [ "$EXISTING_ROOT" != "$ROOT" ]; then
    die "$SERVICE already points at $EXISTING_ROOT.
  Installing here would repoint it at $ROOT and orphan that deployment.
  Use --root $EXISTING_ROOT to upgrade it, or remove the unit first."
  fi
fi

# systemd-analyze reads the file name as the unit name, so the rendered
# copy has to carry it.
[ "${ROOT#/}" != "$ROOT" ] || die "the deployment path must be absolute: $ROOT"
case "$ROOT" in *[!A-Za-z0-9._/-]*) die "deployment path must be free of unit metacharacters: $ROOT" ;; esac
STAGING=$(mktemp -d)
curl -fsSL "https://raw.githubusercontent.com/$REPO/$TAG/deploy/mibot-lite.service" -o "$STAGING/template" \
  || die "could not download the service template"
sed -e "s|@ROOT@|$ROOT|g" -e "s|@BINARY@|$ROOT/mibot-lite|g" "$STAGING/template" > "$STAGING/$SERVICE"
grep -q '@[A-Z][A-Z]*@' "$STAGING/$SERVICE" && die "service template has unsubstituted placeholders"
command -v systemd-analyze > /dev/null && systemd-analyze verify "$STAGING/$SERVICE"
install -m 644 "$STAGING/$SERVICE" "$UNIT"
rm -rf "$STAGING"

systemctl daemon-reload
systemctl reset-failed mibot-lite 2> /dev/null || true
systemctl enable --now mibot-lite

for _ in $(seq 30); do
  if journalctl -u mibot-lite -n 50 --no-pager -o cat 2> /dev/null | grep -q 'msg=runtime.ready'; then
    say "MiBot Lite $TAG is running and enabled at boot"
    printf '%s\n' \
      "   Try .help and .ping in Telegram." \
      "   Logs:    journalctl -u mibot-lite -f" \
      "   Upgrade: re-run this installer, or .update run in Telegram"
    exit 0
  fi
  systemctl is-active --quiet mibot-lite || break
  sleep 2
done
die "the service did not report runtime.ready; inspect: journalctl -u mibot-lite -n 50"
