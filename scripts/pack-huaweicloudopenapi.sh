#!/usr/bin/env bash
# pack-huaweicloudopenapi.sh — pack the Huawei Cloud OpenAPI snapshot into
# huaweicloudopenapi.tar.gz.
#
# The snapshot is a two-level directory:
#   products/<产品>.json            索引（一产品一文件）
#   api_details/<产品>/<API名>.json  详情（一 API 一文件）
#
# install.sh fetches the resulting huaweicloudopenapi.tar.gz as an independent
# OBS object (openagent/huaweicloudopenapi.tar.gz) and unpacks it to
# ~/.openagent/huaweicloudopenapi/. This script is run by a maintainer when the
# snapshot is refreshed, not by users.
#
# Usage:
#   bash scripts/pack-huaweicloudopenapi.sh /path/to/api_details_products [out.tar.gz]
#
# Defaults: src = ./api_details_products, out = ./huaweicloudopenapi.tar.gz.

set -euo pipefail

SRC_DIR="${1:-api_details_products}"
OUT="${2:-huaweicloudopenapi.tar.gz}"

info() { printf '[pack-huaweicloudopenapi] %s\n' "$*"; }
die()  { printf '[pack-huaweicloudopenapi] %s\n' "$*" >&2; exit 1; }

[[ -d "${SRC_DIR}/products"    ]] || die "missing ${SRC_DIR}/products/"
[[ -d "${SRC_DIR}/api_details" ]] || die "missing ${SRC_DIR}/api_details/"

prod_n=$(find "${SRC_DIR}/products" -name '*.json' | wc -l)
det_n=$(find "${SRC_DIR}/api_details" -name '*.json' | wc -l)
info "Source: ${SRC_DIR}  (${prod_n} product index files, ${det_n} detail files)"

# Pack only the two subdirs so the tarball extracts to products/ + api_details/
# at the cwd of extraction (install.sh unpacks into ~/.openagent/huaweicloudopenapi/).
tar -C "${SRC_DIR}" -czf "${OUT}" products api_details

sz=$(ls -lh "${OUT}" | awk '{print $5}')
info "Wrote ${OUT}  (${sz})"

# Emit the sha256 next to it, so release.sh / install.sh can verify integrity.
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "${OUT}" > "${OUT}.sha256"
elif command -v shasum >/dev/null 2>&1; then
  shasum -a 256 "${OUT}" > "${OUT}.sha256"
else
  die "neither sha256sum nor shasum available"
fi
info "Wrote ${OUT}.sha256"
