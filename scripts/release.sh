#!/usr/bin/env bash
# release.sh — build, package, and publish a GitHub release for openagent-cli.
#
# Produces per-OS/arch archives, each containing the binary, LICENSE, the
# matching install script, huawei-skills.txt, and SOUL.md; plus a SHA256SUMS.txt
# over all archives. Also uploads SYSTEM.md (Huawei Cloud OpenAPI retrieval
# recipe) as a top-level asset. Then creates a GitHub release via `gh` (or
# uploads to an existing one with --clobber, so re-running on the same tag is
# idempotent), and mirrors the artifacts to Huawei Cloud OBS
# (openagent/<ver>/ + openagent/latest/).
#
# Requires: go, git, gh, zip, hcloud (Huawei Cloud KooCLI, configured with
# AK/SK + endpoint in ~/.obsutilconfig — see
# https://support.huaweicloud.com/usermanual-hcli/hcli_04_009.html).
#
# Config (env vars):
#   OPENAGENT_CLI_NAME   binary + artifact name prefix (default: openagent-cli)
#   OPENAGENT_VERSION    explicit version tag (default: from `git describe`)
#   REPO                 GitHub owner/name (default: inferred from origin remote)
#   DRY_RUN              set to any non-empty value to build+package but skip gh + OBS
#   SKIP_OBS             set to any non-empty value to skip OBS upload (gh only)
#   OBS_ENDPOINT         OBS bucket endpoint (default: https://twb.obs.cn-north-4.myhuaweicloud.com)
#   OBS_PREFIX           OBS object prefix (default: openagent)
#   HUWEICLOUDOPENAPI_TAR        path to snapshot tarball (default: huaweicloudopenapi.tar.gz in cwd)
#   FORCE_HUWEICLOUDOPENAPI      set to any non-empty value to force snapshot re-upload even if hash matches
#
# Usage:
#   bash scripts/release.sh                 # version from latest git tag
#   OPENAGENT_VERSION=v1.2.3 bash scripts/release.sh
#   OPENAGENT_CLI_NAME=mycli bash scripts/release.sh
#   REPO=owner/name bash scripts/release.sh  # override inferred origin remote
#   DRY_RUN=1 bash scripts/release.sh       # build+package only, no upload
#   SKIP_OBS=1 bash scripts/release.sh      # GitHub release only, no OBS mirror

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

NAME="${OPENAGENT_CLI_NAME:-openagent-cli}"
REPO="${REPO:-}"

# Resolve REPO from the `origin` git remote (owner/name), so forks don't
# hard-code the upstream path. REPO env var overrides.
if [[ -z "${REPO}" ]]; then
  origin_url="$(git remote get-url origin 2>/dev/null || true)"
  if [[ "${origin_url}" == git@github.com:* ]]; then
    REPO="${origin_url#git@github.com:}"
    REPO="${REPO%.git}"
  elif [[ "${origin_url}" == https://github.com/* ]]; then
    REPO="${origin_url#https://github.com/}"
    REPO="${REPO%.git}"
  else
    echo "[release] could not infer REPO from origin remote; set REPO=owner/name" >&2
    exit 1
  fi
fi

info() { printf '%s\n' "$*"; }
die()  { printf '[release] %s\n' "$*" >&2; exit 1; }

need_cmd() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }

need_cmd go
need_cmd git
need_cmd gh
need_cmd zip

# macOS ships shasum, not sha256sum. Wrap both.
sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  else
    shasum -a 256 "$@"
  fi
}

# ── version ────────────────────────────────────────────────────────────────────
if [[ -n "${OPENAGENT_VERSION:-}" ]]; then
  VER="${OPENAGENT_VERSION}"
else
  VER="$(git describe --tags --abbrev=0 2>/dev/null || true)"
  [[ -n "${VER}" ]] || die "no git tag found; pass OPENAGENT_VERSION=vX.Y.Z"
fi

# Validate VER shape — guards ldflags injection against weird tag names.
if [[ ! "${VER}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-.+][0-9A-Za-z.-]+)?$ ]]; then
  die "invalid version (expected vX.Y.Z with optional pre-release): ${VER}"
fi

# NAME ends up in filenames, find -name patterns, and globs — reject anything
# outside [A-Za-z0-9._-] so '*', spaces, etc. can't break the build matrix.
if [[ ! "${NAME}" =~ ^[A-Za-z0-9._-]+$ ]]; then
  die "invalid OPENAGENT_CLI_NAME (allowed: letters, digits, . _ -): ${NAME}"
fi

# Dirty tree first — before we create any tag, so a failed run leaves no
# dangling tag pointing at HEAD.
if [[ -n "$(git status --porcelain)" ]]; then
  die "working tree is dirty; commit or stash first"
fi

# Reject re-tagging onto a different commit. Same commit → idempotent (we'll
# just upload --clobber below).
if git rev-parse -q --verify "refs/tags/${VER}" >/dev/null 2>&1; then
  if [[ "$(git rev-list -n 1 "${VER}")" != "$(git rev-parse HEAD)" ]]; then
    die "tag ${VER} already exists and points at a different commit; bump the version"
  fi
else
  info "Creating lightweight tag ${VER} at HEAD"
  git tag "${VER}"
fi

# Precheck gh auth so we fail fast before a long build.
gh auth status >/dev/null 2>&1 || die "gh not authenticated; run gh auth login"

info "Releasing ${NAME} ${VER} for ${REPO}"

# ── validate inputs (zero-cost: release already reads these) ──────────────────
SKILLS_FILE="scripts/huawei-skills.txt"
[[ -f "${SKILLS_FILE}" ]] || die "missing ${SKILLS_FILE}"
[[ -s "${SKILLS_FILE}" ]] || die "${SKILLS_FILE} is empty"
if grep -nv '^huawei-cloud-' "${SKILLS_FILE}" | grep -q .; then
  die "${SKILLS_FILE} has lines not matching ^huawei-cloud-"
fi
if grep -nq '^$' "${SKILLS_FILE}"; then
  die "${SKILLS_FILE} has empty lines"
fi
[[ -f LICENSE ]] || die "LICENSE missing in repo root"
SOUL_FILE="scripts/SOUL.md"
[[ -f "${SOUL_FILE}" ]] || die "missing ${SOUL_FILE}"
[[ -s "${SOUL_FILE}" ]] || die "${SOUL_FILE} is empty"
SYSTEM_FILE="scripts/SYSTEM.md"
[[ -f "${SYSTEM_FILE}" ]] || die "missing ${SYSTEM_FILE}"
[[ -s "${SYSTEM_FILE}" ]] || die "${SYSTEM_FILE} is empty"

# ── build matrix ───────────────────────────────────────────────────────────────
# OS_ARCH pairs; artifact suffix mirrors install.sh's uname mapping.
TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
  "windows/arm64"
)

DIST_DIR="dist"
rm -rf "${DIST_DIR}"
mkdir -p "${DIST_DIR}"

LDFLAGS="-s -w -X main.version=${VER}"

build_one() {
  local goos="$1" goarch="$2"
  local ext=""
  [[ "${goos}" == "windows" ]] && ext=".exe"

  local bin="${DIST_DIR}/${NAME}-${goos}-${goarch}${ext}"
  info "  building ${goos}/${goarch}"
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
    go build -trimpath -ldflags "${LDFLAGS}" -o "${bin}" ./cmd/cli/
}

package_one() {
  local goos="$1" goarch="$2"
  local ext=""
  [[ "${goos}" == "windows" ]] && ext=".exe"

  local bin="${DIST_DIR}/${NAME}-${goos}-${goarch}${ext}"
  local staging="${DIST_DIR}/stage-${goos}-${goarch}"
  rm -rf "${staging}"
  mkdir -p "${staging}"

  cp "${bin}" "${staging}/${NAME}${ext}"
  cp LICENSE "${staging}/LICENSE"

  local arch_name
  case "${goarch}" in
    amd64) arch_name="amd64" ;;
    arm64) arch_name="arm64" ;;
    *)     arch_name="${goarch}" ;;
  esac

  local artifact
  if [[ "${goos}" == "windows" ]]; then
    artifact="${DIST_DIR}/${NAME}_${VER}_${goos}_${arch_name}.zip"
    ( cd "${staging}" && zip -r -q "../$(basename "${artifact}")" . )
  else
    artifact="${DIST_DIR}/${NAME}_${VER}_${goos}_${arch_name}.tar.gz"
    tar -C "${staging}" -czf "${artifact}" .
  fi

  rm -rf "${staging}"
}

for t in "${TARGETS[@]}"; do
  IFS='/' read -r goos goarch <<< "${t}"
  build_one  "${goos}" "${goarch}"
  package_one "${goos}" "${goarch}"
done

# Remove intermediate binaries; keep archives + checksums.
find "${DIST_DIR}" -maxdepth 1 -type f -name "${NAME}-*" -delete

# ── checksums ──────────────────────────────────────────────────────────────────
( cd "${DIST_DIR}" && sha256_file *.tar.gz *.zip > SHA256SUMS.txt )

info "Artifacts:"
( cd "${DIST_DIR}" && ls -1 )
info ""
info "Checksums:"
cat "${DIST_DIR}/SHA256SUMS.txt"

# ── publish ────────────────────────────────────────────────────────────────────
if [[ -n "${DRY_RUN:-}" ]]; then
  info "DRY_RUN set — skipping gh release and OBS upload"
  exit 0
fi

# Idempotent: if the release already exists, upload assets with --clobber;
# otherwise create it. (Tag idempotency above already handles the tag.)
# Top-level files shared across platforms, uploaded alongside archives so
# install.sh can fetch them without unpacking an archive first.
EXTRA_FILES=("${SKILLS_FILE}" "${SOUL_FILE}" "${SYSTEM_FILE}" scripts/install.sh scripts/install.ps1)

if gh release view "${VER}" --repo "${REPO}" >/dev/null 2>&1; then
  info "Release ${VER} already exists; uploading assets with --clobber"
  gh release upload "${VER}" "${DIST_DIR}"/{*.tar.gz,*.zip,SHA256SUMS.txt} "${EXTRA_FILES[@]}" \
    --repo "${REPO}" --clobber
else
  info "Creating GitHub release ${VER} for ${REPO}"
  gh release create "${VER}" "${DIST_DIR}"/{*.tar.gz,*.zip,SHA256SUMS.txt} "${EXTRA_FILES[@]}" \
    --repo "${REPO}" --generate-notes --title "${VER}"
fi

info "Released ${VER} to GitHub"

# ── mirror to Huawei Cloud OBS ─────────────────────────────────────────────────
# release.sh uploads the same artifacts to OBS so install.sh can fetch them
# without GitHub connectivity. Layout:
#   ${OBS_PREFIX}/<ver>/{artifacts,SHA256SUMS.txt}
#   ${OBS_PREFIX}/latest/{artifacts,SHA256SUMS.txt}   ← cleared + rewritten each release
# Bucket is public-read; install.sh downloads anonymously.
#
# Config (env vars, all optional with defaults):
#   OBS_ENDPOINT  default https://twb.obs.cn-north-4.myhuaweicloud.com
#   OBS_PREFIX    default openagent
#   SKIP_OBS      set to any non-empty value to skip OBS upload
# Credentials: hcloud obs reads ~/.obsutilconfig (ak/sk/endpoint). See
#   https://support.huaweicloud.com/usermanual-hcli/hcli_04_009.html
OBS_ENDPOINT="${OBS_ENDPOINT:-https://twb.obs.cn-north-4.myhuaweicloud.com}"
OBS_PREFIX="${OBS_PREFIX:-openagent}"

if [[ -n "${SKIP_OBS:-}" ]]; then
  info "SKIP_OBS set — skipping OBS upload"
  exit 0
fi

# Bucket name = host's first label (twb in twb.obs.cn-north-4.myhuaweicloud.com).
OBS_BUCKET="$(printf '%s' "${OBS_ENDPOINT}" | sed -E 's|^https?://([^./]+).*|\1|')"
[[ -n "${OBS_BUCKET}" ]] || die "could not parse bucket name from OBS_ENDPOINT=${OBS_ENDPOINT}"

need_cmd hcloud

# hcloud obs cp needs the bucket endpoint without scheme for obs:// URLs, but the
# obsutil config already holds the endpoint. We just use obs://<bucket>/<key>.
obs_key_for() { printf '%s/%s/%s' "${OBS_PREFIX}" "$1" "$2"; }

upload_artifacts_to() {
  local dest_prefix="$1"   # e.g. "v0.0.1" or "latest"
  local f
  # Per-platform archives + checksums (from dist/)
  for f in "${DIST_DIR}"/*.tar.gz "${DIST_DIR}"/*.zip "${DIST_DIR}/SHA256SUMS.txt"; do
    [[ -f "${f}" ]] || continue
    local base; base="$(basename "${f}")"
    local key; key="$(obs_key_for "${dest_prefix}" "${base}")"
    info "  OBS: ${base} → obs://${OBS_BUCKET}/${key}"
    # -acl=public-read: bucket is public-read but objects don't inherit ACL
    # on upload — set explicitly so install.sh can fetch anonymously.
    hcloud obs cp "${f}" "obs://${OBS_BUCKET}/${key}" -f -acl=public-read >/dev/null \
      || die "OBS upload failed for ${base} to ${dest_prefix}/"
  done
  # Top-level files shared across platforms (not inside archives, so install.sh
  # can fetch them independently and archives stay small + non-redundant).
  for f in "${SKILLS_FILE}" "${SOUL_FILE}" "${SYSTEM_FILE}" scripts/install.sh scripts/install.ps1; do
    [[ -f "${f}" ]] || continue
    local base; base="$(basename "${f}")"
    local key; key="$(obs_key_for "${dest_prefix}" "${base}")"
    info "  OBS: ${base} → obs://${OBS_BUCKET}/${key}"
    hcloud obs cp "${f}" "obs://${OBS_BUCKET}/${key}" -f -acl=public-read >/dev/null \
      || die "OBS upload failed for ${base} to ${dest_prefix}/"
  done
}

# Clear latest/ before uploading (accept brief empty-window for a clean single-version latest).
clear_obs_latest() {
  info "  OBS: clearing ${OBS_PREFIX}/latest/"
  local listing
  listing="$(hcloud obs ls "obs://${OBS_BUCKET}/${OBS_PREFIX}/latest/" -d -limit=1000 2>/dev/null || true)"
  if ! printf '%s' "${listing}" | grep -q 'File number: 0'; then
    # There are objects to delete. Extract keys and rm each.
    printf '%s' "${listing}" | awk '/^obs:\/\// {print $1}' | while read -r obj_url; do
      hcloud obs rm "${obj_url}" -f >/dev/null 2>&1 || true
    done
  fi
}

info "Uploading artifacts to OBS bucket ${OBS_BUCKET} (${OBS_ENDPOINT})"
upload_artifacts_to "${VER}"
clear_obs_latest
upload_artifacts_to "latest"

# ── OpenAPI snapshot: version-independent, uploaded to both OBS and a dedicated
# long-lived GitHub release so install.sh/ps1 can fetch from either at a stable
# URL. Content-addressed skip: compare local .sha256 vs remote .sha256, only
# upload when content changed (or FORCE_HUWEICLOUDOPENAPI=1).
#   OBS:    obs://<bucket>/<prefix>/huaweicloudopenapi/{tar.gz, .sha256}
#   GitHub: releases/download/huaweicloudopenapi/{tar.gz, .sha256}
SNAP_TAR="${HUWEICLOUDOPENAPI_TAR:-huaweicloudopenapi.tar.gz}"
SNAP_SHA="${SNAP_TAR}.sha256"
SNAP_TAG="huaweicloudopenapi"
SNAP_OBS_PREFIX="${OBS_PREFIX}/huaweicloudopenapi"

# Read the hash field (first whitespace-delimited token) from a .sha256 file.
sha_first_field() { awk '{print $1; exit}' "$1" 2>/dev/null; }

if [[ ! -f "${SNAP_TAR}" ]]; then
  info "huaweicloudopenapi.tar.gz not found locally (set HUWEICLOUDOPENAPI_TAR=path to provide); skipping API snapshot upload"
else
  local_hash="$(sha_first_field "${SNAP_SHA}")"

  # ── GitHub: dedicated release tag "huaweicloudopenapi" (stable URL) ──
  # Ensure the release exists, then compare remote .sha256 to decide upload.
  if gh release view "${SNAP_TAG}" --repo "${REPO}" >/dev/null 2>&1; then
    gh_needs_upload=0
    if [[ -z "${FORCE_HUWEICLOUDOPENAPI:-}" && -n "${local_hash}" ]]; then
      gh_sha_tmp="$(mktemp)"
      if gh release download "${SNAP_TAG}" "${SNAP_SHA##*/}" --repo "${REPO}" -D "$(dirname "${gh_sha_tmp}")" -O "$(basename "${gh_sha_tmp}")" 2>/dev/null; then
        remote_hash="$(sha_first_field "${gh_sha_tmp}")"
        if [[ "${local_hash}" == "${remote_hash}" ]]; then
          info "GitHub: snapshot unchanged (sha256 match); skipping upload"
        else
          gh_needs_upload=1
        fi
      else
        gh_needs_upload=1   # sidecar not found → upload
      fi
      rm -f "${gh_sha_tmp}"
    elif [[ -n "${FORCE_HUWEICLOUDOPENAPI:-}" ]]; then
      gh_needs_upload=1; info "GitHub: FORCE_HUWEICLOUDOPENAPI=1; uploading"
    else
      gh_needs_upload=1; info "GitHub: local .sha256 empty; uploading"
    fi
    if [[ "${gh_needs_upload}" == "1" ]]; then
      info "GitHub: uploading snapshot to ${SNAP_TAG} release (--clobber)"
      gh release upload "${SNAP_TAG}" "${SNAP_TAR}" --repo "${REPO}" --clobber \
        || die "GitHub upload failed for ${SNAP_TAR}"
      if [[ -f "${SNAP_SHA}" ]]; then
        gh release upload "${SNAP_TAG}" "${SNAP_SHA}" --repo "${REPO}" --clobber \
          || die "GitHub upload failed for ${SNAP_SHA}"
      fi
    fi
  else
    info "GitHub: creating ${SNAP_TAG} release for API snapshot"
    gh release create "${SNAP_TAG}" --repo "${REPO}" \
      --title "Huawei Cloud OpenAPI Snapshot" \
      --notes "Version-independent API snapshot for openagent-cli. Updated by release.sh; not a binary release." \
      || die "failed to create ${SNAP_TAG} release"
    gh release upload "${SNAP_TAG}" "${SNAP_TAR}" --repo "${REPO}" --clobber \
      || die "GitHub upload failed for ${SNAP_TAR}"
    if [[ -f "${SNAP_SHA}" ]]; then
      gh release upload "${SNAP_TAG}" "${SNAP_SHA}" --repo "${REPO}" --clobber \
        || die "GitHub upload failed for ${SNAP_SHA}"
    fi
  fi

  # ── OBS (version-independent, under <prefix>/huaweicloudopenapi/) ──
  obs_tar_key="${SNAP_OBS_PREFIX}/huaweicloudopenapi.tar.gz"
  obs_sha_key="${SNAP_OBS_PREFIX}/huaweicloudopenapi.tar.gz.sha256"
  obs_needs_upload=0
  if [[ -z "${FORCE_HUWEICLOUDOPENAPI:-}" && -n "${local_hash}" ]]; then
    obs_sha_tmp="$(mktemp)"
    if hcloud obs cp "obs://${OBS_BUCKET}/${obs_sha_key}" "${obs_sha_tmp}" -f >/dev/null 2>&1; then
      remote_hash="$(sha_first_field "${obs_sha_tmp}")"
      if [[ "${local_hash}" == "${remote_hash}" ]]; then
        info "OBS: snapshot unchanged (sha256 match); skipping upload"
      else
        obs_needs_upload=1
      fi
    else
      obs_needs_upload=1   # sidecar not found → upload
    fi
    rm -f "${obs_sha_tmp}"
  elif [[ -n "${FORCE_HUWEICLOUDOPENAPI:-}" ]]; then
    obs_needs_upload=1; info "OBS: FORCE_HUWEICLOUDOPENAPI=1; uploading"
  else
    obs_needs_upload=1; info "OBS: local .sha256 empty; uploading"
  fi
  if [[ "${obs_needs_upload}" == "1" ]]; then
    info "OBS: uploading snapshot to obs://${OBS_BUCKET}/${SNAP_OBS_PREFIX}/"
    hcloud obs cp "${SNAP_TAR}" "obs://${OBS_BUCKET}/${obs_tar_key}" -f -acl=public-read >/dev/null \
      || die "OBS upload failed for huaweicloudopenapi.tar.gz"
    if [[ -f "${SNAP_SHA}" ]]; then
      hcloud obs cp "${SNAP_SHA}" "obs://${OBS_BUCKET}/${obs_sha_key}" -f -acl=public-read >/dev/null \
        || die "OBS upload failed for huaweicloudopenapi.tar.gz.sha256"
    fi
    info "OBS: API snapshot uploaded"
  fi
fi

info "Mirrored to OBS: ${OBS_PREFIX}/${VER}/ and ${OBS_PREFIX}/latest/"

