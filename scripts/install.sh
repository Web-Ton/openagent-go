#!/usr/bin/env bash
# install.sh — one-step install for openagent-cli (Linux / macOS).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/<repo>/master/scripts/install.sh | bash
#
# Downloads from Huawei Cloud OBS first (reachable behind GFW), falls back to
# GitHub on failure. OPENAGENT_MIRROR forces one source.
#
# Config (env vars):
#   OPENAGENT_CLI_NAME   binary name (default: openagent-cli)
#   OPENAGENT_VERSION    e.g. v1.2.3 (default: latest from OBS openagent/latest/)
#   REPO                 GitHub owner/name, used for fallback (default: yusheng-g/openagent-go)
#   OPENAGENT_MIRROR     force source: "obs" or "github" (default: try OBS then GitHub)
#   OBS_ENDPOINT         OBS bucket endpoint (default: https://twb.obs.cn-north-4.myhuaweicloud.com)
#   OBS_PREFIX           OBS object prefix (default: openagent)
#   SOUL_OVERWRITE       set to 1 to force-overwrite SOUL.md without prompting
#   HUWEICLOUDOPENAPI_OVERWRITE  set to 1 to force re-download the API snapshot
#   SUDO                 override privilege escalation; set SUDO='' to force direct (even as non-root)
#
# NOTE on checksum scope: SHA256SUMS.txt ships in the same release as the
# binary, so this check defends against bit-flip / CDN corruption but NOT
# against a tampered release (attacker can replace both). For full supply-
# chain integrity, verify a maintainer GPG/cosign signature out-of-band.

set -euo pipefail

NAME="${OPENAGENT_CLI_NAME:-openagent-cli}"
REPO="${REPO:-yusheng-g/openagent-go}"
INSTALL_PREFIX="/opt/${NAME}"
BIN_LINK="/usr/local/bin/${NAME}"

info() { printf '[install] %s\n' "$*"; }
warn() { printf '[install] WARN: %s\n' "$*" >&2; }
die()  { printf '[install] %s\n' "$*" >&2; exit 1; }

need_cmd() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }

need_cmd curl
need_cmd tar

# ── resolve version ────────────────────────────────────────────────────────────
# Download JSON to a file first to avoid SIGPIPE from `curl | grep | head`
# under `set -o pipefail`, and use `grep -m1` (no early-close). Unauthenticated
# GitHub API is rate-limited to 60/h — surface a hint on 403.
TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

# ── download sources ───────────────────────────────────────────────────────────
# OBS is primary (users behind GFW can reach Huawei Cloud), GitHub is fallback.
# OPENAGENT_MIRROR=obs|github forces one source (no fallback). OBS bucket is
# public-read, so no credentials needed to download.
OBS_ENDPOINT="${OBS_ENDPOINT:-https://twb.obs.cn-north-4.myhuaweicloud.com}"
OBS_PREFIX="${OBS_PREFIX:-openagent}"
MIRROR="${OPENAGENT_MIRROR:-}"
case "${MIRROR}" in
  obs|github|"") ;;
  *) die "OPENAGENT_MIRROR must be obs, github, or unset; got: ${MIRROR}" ;;
esac

VER="${OPENAGENT_VERSION:-}"

# ── detect OS / arch (needed for artifact name) ───────────────────────────────
OS="$(uname -s)"
ARCH="$(uname -m)"

case "${OS}" in
  Linux)  os_name="linux" ;;
  Darwin) os_name="darwin" ;;
  *)      die "unsupported OS: ${OS}. Download manually from https://github.com/${REPO}/releases" ;;
esac

case "${ARCH}" in
  x86_64|amd64)  arch_name="amd64" ;;
  aarch64|arm64) arch_name="arm64" ;;
  *) die "unsupported architecture: ${ARCH}. Download manually from https://github.com/${REPO}/releases" ;;
esac

# OBS latest directory holds the most recent release's artifacts. When the user
# passes OPENAGENT_VERSION, use openagent/<ver>/; otherwise openagent/latest/.
obs_path_for_ver() {
  if [[ -z "${VER}" ]]; then printf '%s/latest' "${OBS_PREFIX}"
  else printf '%s/%s' "${OBS_PREFIX}" "${VER}"; fi
}

# Download artifact + checksums from one base URL into TMPDIR. Returns 0 on
# success (both files downloaded), 1 on any failure. Caller decides fallback.
download_from() {
  local base="$1" label="$2"
  local artifact_url="${base}/${ARTIFACT}"
  local sums_url="${base}/SHA256SUMS.txt"
  info "Downloading ${label}: ${artifact_url}"
  if ! curl -fsSL --connect-timeout 10 --max-time 60 --retry 2 --retry-delay 2 \
        -o "${TMPDIR}/${ARTIFACT}" "${artifact_url}"; then
    return 1
  fi
  info "Downloading ${label}: ${sums_url}"
  if ! curl -fsSL --connect-timeout 10 --max-time 60 --retry 2 --retry-delay 2 \
        -o "${TMPDIR}/SHA256SUMS.txt" "${sums_url}"; then
    return 1
  fi
  return 0
}

ARTIFACT="${NAME}_${VER:+${VER}_}${os_name}_${arch_name}.tar.gz"
# NOTE: VER is still empty at this point if user didn't pass it; we fill it after
# resolving "latest" below. Re-derive ARTIFACT once VER is known.

# ── resolve version + download (OBS primary, GitHub fallback) ──────────────────
# For OBS: if VER unset, download from openagent/latest/ (no version-discovery API
# needed — the directory IS the latest). For GitHub: if VER unset, call the
# releases/latest API to learn the tag, then download from the tag URL.

resolve_github_latest() {
  info "Fetching latest release version from GitHub..."
  local api_url="https://api.github.com/repos/${REPO}/releases/latest"
  local http_code
  http_code="$(curl -sS -o "${TMPDIR}/rel.json" -w '%{http_code}' "${api_url}" || true)"
  if [[ "${http_code}" == "403" ]]; then
    die "GitHub API rate-limited (403). Set GITHUB_TOKEN or pass OPENAGENT_VERSION explicitly."
  fi
  [[ "${http_code}" == "200" ]] || return 1
  VER="$(grep -m1 '"tag_name"' "${TMPDIR}/rel.json" \
    | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')"
  [[ -n "${VER}" ]] || return 1
}

# Build the GitHub per-release base URL (needs VER).
gh_base_url() { printf 'https://github.com/%s/releases/download/%s' "${REPO}" "${VER}"; }

download_ok=0

# --- OBS attempt (unless MIRROR=github) ---
if [[ "${MIRROR}" != "github" ]]; then
  # For OBS, if VER unset, we download from latest/ and learn VER from the
  # SHA256SUMS.txt artifact names (they embed the version). Try download first.
  ARTIFACT_LATEST="${NAME}_latest_${os_name}_${arch_name}.tar.gz"
  if [[ -z "${VER}" ]]; then
    # Download latest/SHA256SUMS.txt first to discover the real artifact name.
    obs_base="${OBS_ENDPOINT}/$(obs_path_for_ver)"
    if curl -fsSL --connect-timeout 10 --max-time 60 \
          -o "${TMPDIR}/SHA256SUMS.txt" "${obs_base}/SHA256SUMS.txt"; then
      # Extract the version from the first tar.gz entry in SHA256SUMS.txt.
      VER="$(awk 'match($0, /'"${NAME}"'_v[0-9]+\.[0-9]+\.[0-9][^[:space:]]*'"_${os_name}_${arch_name}"'\.tar\.gz/) {
        s=substr($0, RSTART); match(s, /v[0-9]+\.[0-9]+\.[0-9]([-.+][0-9A-Za-z.-]*)?/);
        print substr(s, RSTART, RLENGTH); exit
      }' "${TMPDIR}/SHA256SUMS.txt" 2>/dev/null || true)"
      if [[ -n "${VER}" ]]; then
        ARTIFACT="${NAME}_${VER}_${os_name}_${arch_name}.tar.gz"
        info "Resolved latest version: ${VER}"
        if curl -fsSL --connect-timeout 10 --max-time 60 --retry 2 --retry-delay 2 \
              -o "${TMPDIR}/${ARTIFACT}" "${obs_base}/${ARTIFACT}"; then
          download_ok=1
          DL_BASE="${obs_base}"
          info "Downloaded from OBS (latest)"
        fi
      fi
    fi
  else
    ARTIFACT="${NAME}_${VER}_${os_name}_${arch_name}.tar.gz"
    if download_from "${OBS_ENDPOINT}/$(obs_path_for_ver)" "OBS"; then
      download_ok=1
      DL_BASE="${OBS_ENDPOINT}/$(obs_path_for_ver)"
      info "Downloaded from OBS (${VER})"
    fi
  fi
  if [[ "${download_ok}" != "1" && "${MIRROR}" == "obs" ]]; then
    die "OPENAGENT_MIRROR=obs but OBS download failed. Check ${OBS_ENDPOINT}/${OBS_PREFIX}/ or set OPENAGENT_MIRROR=github"
  fi
fi

# --- GitHub fallback (unless MIRROR=obs and we already succeeded) ---
if [[ "${download_ok}" != "1" && "${MIRROR}" != "obs" ]]; then
  if [[ -z "${VER}" ]]; then
    if ! resolve_github_latest; then
      die "failed to resolve latest version from GitHub (and OBS download also failed). Pass OPENAGENT_VERSION explicitly, or set OPENAGENT_MIRROR=obs/github."
    fi
  fi
  ARTIFACT="${NAME}_${VER}_${os_name}_${arch_name}.tar.gz"
  warn "OBS download unavailable; falling back to GitHub"
  if ! download_from "$(gh_base_url)" "GitHub"; then
    die "GitHub download also failed. Pass OPENAGENT_VERSION explicitly, or check network."
  fi
  download_ok=1
  DL_BASE="$(gh_base_url)"
  info "Downloaded from GitHub (${VER})"
fi

[[ "${download_ok}" == "1" ]] || die "no download source succeeded"
[[ -n "${VER}" ]] || die "version could not be determined"
info "Installing ${NAME} ${VER}"

# ── verify SHA256 ──────────────────────────────────────────────────────────────
# Literal string match (NOT regex): awk `~` would treat '.' as a wildcard,
# letting an attacker-controlled SHA256SUMS.txt line with a regex-equivalent
# filename steal the match. Use index() + suffix check so every character of
# the artifact name must match exactly.
expected="$(awk -v want="  ${ARTIFACT}" '
  { n = length($0); w = length(want);
    if (n >= w && index(substr($0, n-w+1), want) == 1) { print $1; exit } }
' "${TMPDIR}/SHA256SUMS.txt")"
[[ -n "${expected}" ]] || die "no checksum found for ${ARTIFACT} in SHA256SUMS.txt"

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${TMPDIR}/${ARTIFACT}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "${TMPDIR}/${ARTIFACT}" | awk '{print $1}')"
else
  die "neither sha256sum nor shasum available for verification"
fi

if [[ "${actual}" != "${expected}" ]]; then
  die "checksum mismatch for ${ARTIFACT}: expected ${expected}, got ${actual}"
fi
info "Checksum OK"

# ── fetch top-level files (huawei-skills.txt, SOUL.md) ─────────────────────────
# These live alongside the archive in the release (not inside it), so install.sh
# fetches them separately. Soft-fail: missing skills → skip skill install,
# missing SOUL → skip profile. Never block the binary install.
fetch_toplevel() {
  local fname="$1"
  if curl -fsSL --connect-timeout 10 --max-time 60 --retry 2 --retry-delay 2 \
        -o "${TMPDIR}/${fname}" "${DL_BASE}/${fname}"; then
    return 0
  fi
  return 1
}

fetch_toplevel huawei-skills.txt || warn "huawei-skills.txt not found at ${DL_BASE}/; skill install will be skipped"
fetch_toplevel SOUL.md            || warn "SOUL.md not found at ${DL_BASE}/; profile will not be installed"
fetch_toplevel SYSTEM.md            || warn "SYSTEM.md not found at ${DL_BASE}/; API search recipe will not be installed"

# ── extract (reject path-traversal entries before unpacking) ───────────────────
if tar -tzf "${TMPDIR}/${ARTIFACT}" | grep -E '(^\.\./|/\.\./|^/)' >/dev/null 2>&1; then
  die "archive contains unsafe paths (.. or absolute); refusing to extract"
fi
EXTRACT_DIR="${TMPDIR}/extract"
mkdir -p "${EXTRACT_DIR}"
# --no-same-owner: avoid chown failure when extracting root-owned archives as
# non-root, or inside user namespaces without the archive's uid mapped.
tar --no-same-owner -C "${EXTRACT_DIR}" -xzf "${TMPDIR}/${ARTIFACT}"

[[ -f "${EXTRACT_DIR}/${NAME}" ]] || die "archive does not contain ${NAME}"

# ── install: /opt/<name>/<ver>/<name> + /usr/local/bin/<name> symlink ──────────
# Adaptive privilege escalation: an explicit SUDO env var wins (set SUDO=''
# to force direct even as non-root, e.g. writing into a user-writable prefix);
# otherwise root → direct, non-root with sudo → sudo, else die.
if [[ -n "${SUDO+x}" ]]; then
  : # honor the override as-is
elif [[ "${EUID:-$(id -u)}" -eq 0 ]]; then
  SUDO=""
elif command -v sudo >/dev/null 2>&1; then
  SUDO="sudo"
else
  die "not root and sudo unavailable; re-run as root or install sudo (or set SUDO='')"
fi

# In a non-interactive shell (e.g. `curl | bash`, stdin is the script — not a
# TTY), sudo would prompt for a password and hang. Fail fast with a hint instead.
if [[ "${SUDO}" == "sudo" && ! -t 0 ]]; then
  die "non-interactive shell with sudo; re-run with SUDO='' for a user-writable prefix, or run inside a TTY, or configure NOPASSWD"
fi

# Split SUDO into an array so empty → no args, and `sudo -E` → two args.
read -ra SUDO_ARG <<< "${SUDO}"
run_priv() { "${SUDO_ARG[@]}" "$@"; }

DEST_DIR="${INSTALL_PREFIX}/${VER}"
info "Installing to ${DEST_DIR}"
run_priv mkdir -p "${DEST_DIR}"

# Atomic install: cp to .new, chmod, mv into place. On failure, clean DEST_DIR
# so we never leave a half-written binary or a version dir with no binary.
rollback() {
  info "install failed; rolling back ${DEST_DIR}"
  run_priv rm -rf "${DEST_DIR}" 2>/dev/null || true
}
trap 'rollback; rm -rf "${TMPDIR}"' EXIT

run_priv cp "${EXTRACT_DIR}/${NAME}" "${DEST_DIR}/${NAME}.new"
run_priv chmod 755 "${DEST_DIR}/${NAME}.new"
run_priv mv -f "${DEST_DIR}/${NAME}.new" "${DEST_DIR}/${NAME}"

info "Linking ${BIN_LINK} -> ${DEST_DIR}/${NAME}"
run_priv mkdir -p "$(dirname "${BIN_LINK}")"
run_priv ln -sf "${DEST_DIR}/${NAME}" "${BIN_LINK}"

# Success — drop the rollback trap, keep only tmpdir cleanup.
trap 'rm -rf "${TMPDIR}"' EXIT

# ── SOUL.md profile (soft-coupled: warn on failure, never block install) ─────────
# Copy the agent persona to ~/.openagent/profile/SOUL.md so the CLI picks it up
# as its system persona. Overwrite policy:
#   - SOUL_OVERWRITE=1 env var  → force overwrite, no prompt
#   - target missing            → just write
#   - target exists + TTY       → read -p "覆盖/跳过"
#   - target exists + no TTY    → skip + warn (curl|bash can't prompt); user can
#                                 re-run with SOUL_OVERWRITE=1 to force
install_soul() {
  local src="${TMPDIR}/SOUL.md"
  [[ -f "${src}" ]] || { warn "SOUL.md not downloaded; skipping profile install"; return 0; }

  local profile_dir="${HOME}/.openagent/profile"
  local dst="${profile_dir}/SOUL.md"

  if [[ -f "${dst}" && -z "${SOUL_OVERWRITE:-}" ]]; then
    if [[ -t 0 && -t 1 ]]; then
      # Interactive: ask. Loop until we get a valid answer.
      local ans
      while true; do
        printf '%s\n' "[install] ${dst} already exists." >&2
        printf '%s' "[install] Overwrite? [y/N] " >&2
        read -r ans || ans=""
        case "${ans}" in
          y|Y|yes|YES) break ;;        # overwrite
          n|N|no|NO|"") return 0 ;;    # skip (default on empty/EOF)
        esac
      done
    else
      warn "${dst} already exists; skipping (set SOUL_OVERWRITE=1 to force overwrite)"
      return 0
    fi
  fi

  mkdir -p "${profile_dir}"
  if cp "${src}" "${dst}"; then
    info "SOUL.md installed to ${dst}"
  else
    warn "failed to copy SOUL.md to ${dst}; profile not installed"
  fi
}

install_soul || true

# ── SYSTEM.md recipe (always overwrite — it's a factual recipe, not user data) ──
# Unlike SOUL.md, SYSTEM.md is a shared retrieval recipe, not a personalized
# persona. Always overwrite so users get the latest recipe on re-install.
# Keep a .bak of the previous version so local edits aren't silently destroyed.
install_system() {
  local src="${TMPDIR}/SYSTEM.md"
  [[ -f "${src}" ]] || { warn "SYSTEM.md not downloaded; skipping recipe install"; return 0; }

  local profile_dir="${HOME}/.openagent/profile"
  local dst="${profile_dir}/SYSTEM.md"
  mkdir -p "${profile_dir}"
  if [[ -f "${dst}" ]]; then
    cp "${dst}" "${dst}.bak"
  fi
  if cp "${src}" "${dst}"; then
    info "SYSTEM.md installed to ${dst}"
  else
    warn "failed to copy SYSTEM.md to ${dst}; recipe not installed"
  fi
}

install_system || true

# ── OpenAPI snapshot (~/.openagent/huaweicloudopenapi/) — independent of the binary archive ──
# Fetches huaweicloudopenapi.tar.gz (version-independent, ~22MB gzip) and unpacks to
# ~/.openagent/huaweicloudopenapi/ so SYSTEM.md's recipe can grep it locally. Idempotent: skip
# if the dir already exists (set HUWEICLOUDOPENAPI_OVERWRITE=1 to force re-download).
# Mirror logic mirrors the binary: OBS primary, GitHub fallback, OPENAGENT_MIRROR forces one.
# Soft-fail: never block the binary install.
install_huaweicloudopenapi() {
  local snap_dir="${HOME}/.openagent/huaweicloudopenapi"
  if [[ -d "${snap_dir}" && -z "${HUWEICLOUDOPENAPI_OVERWRITE:-}" ]]; then
    info "${snap_dir} already exists; skipping API snapshot (set HUWEICLOUDOPENAPI_OVERWRITE=1 to re-download)"
    return 0
  fi

  local snap_tar="huaweicloudopenapi.tar.gz"
  local snap_sha="huaweicloudopenapi.tar.gz.sha256"
  local tar_tmp="${TMPDIR}/${snap_tar}"
  local sha_tmp="${TMPDIR}/${snap_sha}"
  local tar_url="" sha_url=""

  # Resolve snapshot source: OBS primary, GitHub fallback (unless MIRROR forces one).
  local snap_dl=0
  if [[ "${MIRROR}" != "github" ]]; then
    local obs_base="${OBS_ENDPOINT}/${OBS_PREFIX}/huaweicloudopenapi"
    tar_url="${obs_base}/${snap_tar}"
    sha_url="${obs_base}/${snap_sha}"
    info "Downloading API snapshot (OBS): ${tar_url}"
    if curl -fsSL --connect-timeout 10 --max-time 120 --retry 2 --retry-delay 2 -o "${tar_tmp}" "${tar_url}" \
       && curl -fsSL --connect-timeout 10 --max-time 30 -o "${sha_tmp}" "${sha_url}"; then
      snap_dl=1
      info "Downloaded API snapshot from OBS"
    fi
  fi
  if [[ "${snap_dl}" != "1" && "${MIRROR}" != "obs" ]]; then
    local gh_base="https://github.com/${REPO}/releases/download/huaweicloudopenapi"
    tar_url="${gh_base}/${snap_tar}"
    sha_url="${gh_base}/${snap_sha}"
    info "Downloading API snapshot (GitHub): ${tar_url}"
    if curl -fsSL --connect-timeout 10 --max-time 120 --retry 2 --retry-delay 2 -o "${tar_tmp}" "${tar_url}" \
       && curl -fsSL --connect-timeout 10 --max-time 30 -o "${sha_tmp}" "${sha_url}"; then
      snap_dl=1
      info "Downloaded API snapshot from GitHub"
    fi
  fi
  if [[ "${snap_dl}" != "1" ]]; then
    warn "huaweicloudopenapi.tar.gz not found on OBS or GitHub; API search will be unavailable"
    return 0
  fi

  # Verify SHA256 (sidecar downloaded above); soft-fail if mismatch.
  local expected actual
  expected="$(awk '{print $1; exit}' "${sha_tmp}" 2>/dev/null || true)"
  if [[ -n "${expected}" ]]; then
    if command -v sha256sum >/dev/null 2>&1; then
      actual="$(sha256sum "${tar_tmp}" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
      actual="$(shasum -a 256 "${tar_tmp}" | awk '{print $1}')"
    else
      actual=""
    fi
    if [[ -n "${actual}" && "${actual}" != "${expected}" ]]; then
      warn "huaweicloudopenapi.tar.gz checksum mismatch (expected ${expected}, got ${actual}); skipping"
      return 0
    fi
    info "API snapshot checksum OK"
  else
    warn "huaweicloudopenapi.tar.gz.sha256 empty or unreadable; skipping integrity check"
  fi

  # Reject path-traversal entries before unpacking (same guard as the binary archive).
  if tar -tzf "${tar_tmp}" | grep -E '(^\.\./|/\.\./|^/)' >/dev/null 2>&1; then
    warn "huaweicloudopenapi.tar.gz contains unsafe paths; refusing to extract"
    return 0
  fi

  rm -rf "${snap_dir}"
  mkdir -p "${snap_dir}"
  if tar --no-same-owner -C "${snap_dir}" -xzf "${tar_tmp}"; then
    local n
    n=$(find "${snap_dir}/products" -name '*.json' 2>/dev/null | wc -l)
    info "API snapshot installed to ${snap_dir}  (${n} products)"
  else
    warn "failed to extract huaweicloudopenapi.tar.gz; API search will be unavailable"
    rm -rf "${snap_dir}"
  fi
}

install_huaweicloudopenapi || true

# ── optional Huawei Cloud skills (soft-coupled: detect + degrade) ──────────────
# Only runs if npx is available AND gitcode.com + registry.npmjs.org/skills both
# respond. On any failure: warn, do not exit, and print the command verbatim so
# the user can run it manually once the blocker clears.
print_skill_command() {
  local skills_file="$1"
  local quoted=()
  while IFS= read -r line; do
    [[ -n "${line}" ]] && quoted+=( "${line}" )
  done < "${skills_file}"
  info "  npx -y skills add -g https://gitcode.com/huaweicloud/huaweicloud-skills.git --skill $(printf '%s ' "${quoted[@]}")-y"
}

install_huawei_skills() {
  local skills_file="${TMPDIR}/huawei-skills.txt"
  [[ -f "${skills_file}" ]] || { warn "huawei-skills.txt not downloaded; skipping skill install"; return 0; }

  if ! command -v npx >/dev/null 2>&1; then
    warn "npx not found; skipping Huawei Cloud skill install"
    print_skill_command "${skills_file}"
    return 0
  fi

  # Probe both endpoints with a short timeout. -I = headers only.
  if ! curl -fsSI -m 5 "https://gitcode.com" >/dev/null 2>&1; then
    warn "gitcode.com unreachable; skipping Huawei Cloud skill install"
    print_skill_command "${skills_file}"
    return 0
  fi
  # Probe the npm registry npx will actually use (npm config get registry),
  # not a hardcoded one — users behind a mirror (npmmirror etc.) would have
  # registry.npmjs.org unreachable while npx still works fine via the mirror.
  local npm_reg
  npm_reg="$(npm config get registry 2>/dev/null)"
  npm_reg="${npm_reg%/}"                       # strip trailing slash
  [[ -n "${npm_reg}" ]] || npm_reg="https://registry.npmjs.org"
  if ! curl -fsSI -m 5 "${npm_reg}/skills" >/dev/null 2>&1; then
    warn "npm registry unreachable (${npm_reg}); skipping Huawei Cloud skill install"
    print_skill_command "${skills_file}"
    return 0
  fi

  info "Installing Huawei Cloud skills..."
  local skills_args=()
  while IFS= read -r line; do
    [[ -n "${line}" ]] && skills_args+=( "${line}" )
  done < "${skills_file}"

  # `npx -y` auto-confirms installing the skills package (no TTY in curl|bash);
  # -g forces global (user-level) scope so skills land in ~/.agents/skills/ regardless
  # of the caller's cwd — without -g, `skills add` auto-detects project scope when run
  # inside a git repo and installs under ./.agents/skills/ instead. The trailing -y
  # is passed to the skills tool itself (skip confirmation).
  if npx -y skills add -g https://gitcode.com/huaweicloud/huaweicloud-skills.git \
        --skill "${skills_args[@]}" -y; then
    info "Huawei Cloud skills installed"
  else
    warn "npx skills add failed; you can retry manually:"
    print_skill_command "${skills_file}"
  fi
}

install_huawei_skills || true

# ── PATH sanity check (Apple Silicon may not have /usr/local/bin in PATH) ──────
if ! command -v "${NAME}" >/dev/null 2>&1; then
  warn "${NAME} installed at ${BIN_LINK} but '${NAME}' not found on PATH"
  warn "add $(dirname "${BIN_LINK}") to your PATH, or re-run with a shell that has it"
fi

# ── done (do not auto-start; just hint) ────────────────────────────────────────
info ""
info "${NAME} ${VER} installed to ${DEST_DIR}"
info "Symlinked at ${BIN_LINK}"
info ""
info "Run:  ${NAME} serve"
info "Docs: https://github.com/${REPO}"
