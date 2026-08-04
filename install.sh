#!/bin/sh
# goldfinger installer — downloads the right prebuilt release binary, verifies
# its checksum, and drops it on your PATH. No Go required.
#
#   curl -sSfL https://raw.githubusercontent.com/redscaresu/goldfinger/main/install.sh | sh
#
# Knobs (env vars):
#   GOLDFINGER_VERSION  release tag to install (default: latest, e.g. v0.2.0)
#   GOLDFINGER_BIN      install dir (default: /usr/local/bin if writable, else ~/.local/bin)
set -eu

REPO="redscaresu/goldfinger"

# --- resolve os/arch ---------------------------------------------------------
os=$(uname -s)
case "$os" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) echo "install: unsupported OS '$os' (linux/darwin only)" >&2; exit 1 ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) echo "install: unsupported arch '$arch' (amd64/arm64 only)" >&2; exit 1 ;;
esac

binary="goldfinger-${os}-${arch}"

# --- resolve download URL ----------------------------------------------------
version="${GOLDFINGER_VERSION:-latest}"
if [ "$version" = "latest" ]; then
	base="https://github.com/${REPO}/releases/latest/download"
else
	base="https://github.com/${REPO}/releases/download/${version}"
fi

# --- pick an install dir (prefer no-sudo) ------------------------------------
if [ -n "${GOLDFINGER_BIN:-}" ]; then
	dir="$GOLDFINGER_BIN"
elif [ -w /usr/local/bin ]; then
	dir="/usr/local/bin"
else
	dir="${HOME}/.local/bin"
fi
mkdir -p "$dir"

# --- checksum tool -----------------------------------------------------------
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	echo "install: need sha256sum or shasum to verify the download" >&2
	exit 1
fi

# --- download + verify -------------------------------------------------------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "install: downloading ${binary} (${version})..." >&2
curl -sSfL -o "${tmp}/${binary}" "${base}/${binary}"
curl -sSfL -o "${tmp}/${binary}.sha256" "${base}/${binary}.sha256"

want=$(cut -d' ' -f1 <"${tmp}/${binary}.sha256")
got=$(sha256 "${tmp}/${binary}")
if [ "$want" != "$got" ]; then
	echo "install: checksum mismatch for ${binary}" >&2
	echo "  expected ${want}" >&2
	echo "  got      ${got}" >&2
	exit 1
fi

chmod +x "${tmp}/${binary}"
mv "${tmp}/${binary}" "${dir}/goldfinger"

echo "install: goldfinger installed to ${dir}/goldfinger" >&2
case ":${PATH}:" in
	*":${dir}:"*) ;;
	*) echo "install: ${dir} is not on your PATH — add it, e.g. export PATH=\"\$PATH:${dir}\"" >&2 ;;
esac
echo "install: run 'goldfinger guide' for the operator playbook." >&2
