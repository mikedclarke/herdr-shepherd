#!/bin/sh
# Download a prebuilt herdr-shepherd binary from its GitHub release, verify it
# against the release's SHA256SUMS, and install it. scripts/build.sh calls this
# when the machine has no Go toolchain; it also works on its own:
#
#   curl -fsSL https://raw.githubusercontent.com/mikedclarke/herdr-shepherd/main/scripts/install.sh | sh
#
# Override where it lands, or which release to take:
#
#   ... | INSTALL_DIR=/opt/bin sh
#   ... | VERSION=v0.6.0 sh
#
# POSIX sh only, so it runs on a minimal shell as well as bash.
set -eu

REPO="mikedclarke/herdr-shepherd"
BINARY="herdr-shepherd"
SUMS="SHA256SUMS"

if [ -t 2 ]; then
  BOLD="$(printf '\033[1m')"
  GREEN="$(printf '\033[32m')"
  YELLOW="$(printf '\033[33m')"
  RED="$(printf '\033[31m')"
  RESET="$(printf '\033[0m')"
else
  BOLD=""
  GREEN=""
  YELLOW=""
  RED=""
  RESET=""
fi

# Progress goes to stderr so stdout stays clean for callers.
info() {
  printf '%s==>%s %s\n' "$GREEN" "$RESET" "$1" >&2
}

warn() {
  printf '%s==>%s %s\n' "$YELLOW" "$RESET" "$1" >&2
}

fatal() {
  printf '%serror:%s %s\n' "$RED" "$RESET" "$1" >&2
  exit 1
}

# detect_os maps uname -s onto the token used in release archive names.
detect_os() {
  case "$(uname -s 2>/dev/null || echo unknown)" in
    Linux) echo "linux" ;;
    Darwin) echo "darwin" ;;
    *) fatal "unsupported OS: $(uname -s) — shepherd releases cover Linux and macOS" ;;
  esac
}

detect_arch() {
  case "$(uname -m 2>/dev/null || echo unknown)" in
    x86_64 | amd64) echo "amd64" ;;
    aarch64 | arm64) echo "arm64" ;;
    *) fatal "unsupported architecture: $(uname -m) — releases cover amd64 and arm64" ;;
  esac
}

fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl --fail --show-error --silent --location --output "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget --quiet --output-document "$2" "$1"
  else
    fatal "need curl or wget to download the release"
  fi
}

# resolve_version takes VERSION as given, or follows GitHub's /releases/latest
# redirect. A repository with no releases redirects to the releases index
# instead of a tag, which is the case this script has to explain rather than
# fail obscurely on a 404.
resolve_version() {
  if [ -n "${VERSION:-}" ]; then
    echo "$VERSION"
    return
  fi
  url="https://github.com/${REPO}/releases/latest"
  if command -v curl >/dev/null 2>&1; then
    header="$(curl --silent --location --head "$url" 2>/dev/null | grep -i '^location:' | tail -n 1)"
  elif command -v wget >/dev/null 2>&1; then
    header="$(wget --max-redirect=0 --server-response --output-document=/dev/null "$url" 2>&1 |
      grep -i 'location:' | tail -n 1)"
  else
    fatal "need curl or wget to resolve the latest release"
  fi
  tag="$(printf '%s' "${header##*/}" | tr -d '\r\n ')"
  case "$tag" in
    '' | releases | latest)
      fatal "no releases published at https://github.com/${REPO}/releases yet — install a Go toolchain and run scripts/build.sh, or set VERSION=<tag> once a release exists"
      ;;
  esac
  echo "$tag"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    fatal "need sha256sum or shasum to verify the download"
  fi
}

# verify_checksum matches the archive against its line in SHA256SUMS. An
# unlisted archive is treated as a failure: an unverifiable binary is not an
# improvement over no binary.
verify_checksum() {
  archive_path="$1"
  archive_name="$2"
  sums_path="$3"
  expected="$(awk -v n="$archive_name" '$2 == n || $2 == "*" n { print $1 }' "$sums_path" | head -n 1)"
  if [ -z "$expected" ]; then
    fatal "$archive_name is not listed in $SUMS"
  fi
  actual="$(sha256_of "$archive_path")"
  if [ "$expected" != "$actual" ]; then
    fatal "checksum mismatch for $archive_name (expected $expected, got $actual)"
  fi
}

# pick_install_dir honors INSTALL_DIR (scripts/build.sh passes the plugin's own
# bin/), else ~/.local/bin, else /usr/local/bin.
pick_install_dir() {
  if [ -n "${INSTALL_DIR:-}" ]; then
    echo "$INSTALL_DIR"
    return
  fi
  if [ -d "$HOME/.local/bin" ] || mkdir -p "$HOME/.local/bin" 2>/dev/null; then
    echo "$HOME/.local/bin"
    return
  fi
  echo "/usr/local/bin"
}

install_binary() {
  src="$1"
  dest_dir="$2"
  mkdir -p "$dest_dir" 2>/dev/null || true
  if [ -w "$dest_dir" ]; then
    mv "$src" "$dest_dir/$BINARY"
    chmod +x "$dest_dir/$BINARY"
    return
  fi
  if command -v sudo >/dev/null 2>&1; then
    warn "writing to $dest_dir needs sudo"
    sudo mkdir -p "$dest_dir"
    sudo mv "$src" "$dest_dir/$BINARY"
    sudo chmod +x "$dest_dir/$BINARY"
    return
  fi
  fatal "cannot write to $dest_dir and sudo is not available"
}

warn_if_not_in_path() {
  case ":$PATH:" in
    *":$1:"*) return ;;
  esac
  warn "$1 is not on your \$PATH — add this to your shell rc:"
  printf '\n    %sexport PATH="%s:$PATH"%s\n\n' "$BOLD" "$1" "$RESET" >&2
}

main() {
  command -v tar >/dev/null 2>&1 || fatal "tar is required but not on PATH"

  os="$(detect_os)"
  arch="$(detect_arch)"
  version="$(resolve_version)"
  # Tags carry a leading v; the version slot in the archive name does not.
  archive="${BINARY}_${version#v}_${os}_${arch}.tar.gz"
  base="https://github.com/${REPO}/releases/download/${version}"

  info "installing ${BINARY} ${version} (${os}/${arch})"

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT INT TERM

  fetch "${base}/${archive}" "$tmp/$archive" ||
    fatal "could not download ${base}/${archive}"
  fetch "${base}/${SUMS}" "$tmp/$SUMS" ||
    fatal "could not download ${base}/${SUMS}"
  verify_checksum "$tmp/$archive" "$archive" "$tmp/$SUMS"

  tar -xzf "$tmp/$archive" -C "$tmp" || fatal "could not extract $archive"
  [ -f "$tmp/$BINARY" ] || fatal "$archive did not contain a $BINARY binary"

  dest_dir="$(pick_install_dir)"
  install_binary "$tmp/$BINARY" "$dest_dir"
  info "installed ${BOLD}${dest_dir}/${BINARY}${RESET} (${version})"
  warn_if_not_in_path "$dest_dir"
}

main "$@"
