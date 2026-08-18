#!/bin/bash
# quarry installer. Downloads the latest release binary for your platform into
# ~/.local/bin. The repo is private, so it authenticates with GITHUB_TOKEN, GH_TOKEN,
# or the gh CLI.
#
# Usage:
#   gh api repos/curbol/quarry/contents/install.sh --jq .content | base64 -d | bash
set -euo pipefail

REPO="curbol/quarry"
BINARY_NAME="quarry"
INSTALL_DIR="${HOME}/.local/bin"

log()  { printf 'INFO: %s\n' "$1"; }
err()  { printf 'ERROR: %s\n' "$1" >&2; }

# Prints an auth header, or nothing when no token is available. The explicit
# `return 0` matters under `set -e`: without it the function's status is that of the
# `[[ -n ]]` test, so having no token would abort the script at `hdr=$(auth_header)`
# — before any of the messages below explaining that a token is what's missing.
auth_header() {
  local token="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
  if [[ -z "$token" ]] && command -v gh >/dev/null 2>&1; then
    token=$(gh auth token 2>/dev/null || true)
  fi
  [[ -n "$token" ]] && echo "Authorization: token $token"
  return 0
}

detect_platform() {
  local os arch
  case "$(uname -s)" in
    Darwin*) os="mac" ;;
    Linux*)  os="linux" ;;
    *) err "unsupported OS $(uname -s); on Windows use the release zip directly"; exit 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch="intel" ;;
    arm64|aarch64) [[ "$os" == "mac" ]] && arch="apple" || arch="arm64" ;;
    *) err "unsupported arch $(uname -m)"; exit 1 ;;
  esac
  PLATFORM="${os}-${arch}"
  log "platform: $PLATFORM"
}

latest_version() {
  local hdr; hdr=$(auth_header)
  local opts=(-fsSL); [[ -n "$hdr" ]] && opts+=(-H "$hdr")
  # `|| true` so a 401/404 or an unmatched grep leaves VERSION empty for the check
  # below to explain, instead of `pipefail` aborting the script without a word.
  VERSION=$(curl "${opts[@]}" "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/') || true
  VERSION=${VERSION#v}
  [[ -n "$VERSION" ]] || { err "could not resolve latest version (private repo needs gh auth or GITHUB_TOKEN)"; exit 1; }
  log "latest version: $VERSION"
}

# Confirms the unpacked artifact is a native binary for this platform before it lands
# on PATH, mirroring the same check `quarry update` makes. It catches a release that
# shipped the wrong artifact, or an error page saved under the binary's name.
verify_binary() {
  local f="$1" magic
  [[ -s "$f" ]] || { err "the downloaded binary is empty"; return 1; }
  magic=$(od -An -N4 -tx1 "$f" | tr -d ' \n')
  case "$(uname -s)" in
    Linux*)  [[ "$magic" == 7f454c46 ]] || { err "the downloaded file is not a Linux executable"; return 1; } ;;
    Darwin*) case "$magic" in cffaedfe|cefaedfe|cafebabe) ;; *) err "the downloaded file is not a macOS executable"; return 1 ;; esac ;;
  esac
}

install() {
  local file="${BINARY_NAME}-${VERSION}-${PLATFORM}.zip"
  local tmp; tmp=$(mktemp -d)
  local hdr; hdr=$(auth_header)
  local url
  if [[ -n "$hdr" ]]; then
    # Private repo: resolve the asset's API URL, then download with the token.
    url=$(curl -fsSL -H "$hdr" "https://api.github.com/repos/${REPO}/releases/tags/v${VERSION}" \
      | grep -F -B3 "\"name\": \"${file}\"" | grep -F '"url"' | sed -E 's/.*"url": "([^"]+)".*/\1/') || true
    [[ -n "$url" ]] || { err "asset ${file} not found in release v${VERSION}"; rm -rf "$tmp"; exit 1; }
    curl -fsSL -H "$hdr" -H "Accept: application/octet-stream" -o "${tmp}/${file}" "$url"
  else
    curl -fsSL -o "${tmp}/${file}" "https://github.com/${REPO}/releases/download/v${VERSION}/${file}"
  fi

  command -v unzip >/dev/null 2>&1 || { err "unzip is required"; rm -rf "$tmp"; exit 1; }
  unzip -q "${tmp}/${file}" -d "$tmp"
  verify_binary "${tmp}/${BINARY_NAME}" || { rm -rf "$tmp"; exit 1; }
  mkdir -p "$INSTALL_DIR"
  mv "${tmp}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
  chmod +x "${INSTALL_DIR}/${BINARY_NAME}"
  rm -rf "$tmp"
  log "installed to ${INSTALL_DIR}/${BINARY_NAME}"
}

check_path() {
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) log "note: $INSTALL_DIR is not on your PATH; add: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
  esac
}

detect_platform
latest_version
install
check_path
"${INSTALL_DIR}/${BINARY_NAME}" version || err "installed but 'quarry version' failed"
