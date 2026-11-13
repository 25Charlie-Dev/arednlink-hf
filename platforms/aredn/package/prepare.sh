#!/usr/bin/env bash
set -euo pipefail
umask 022

binary_path="${1:?usage: prepare.sh ARMV7_BINARY OUTPUT_APK [APK_TOOL]}"
output_path="${2:?usage: prepare.sh ARMV7_BINARY OUTPUT_APK [APK_TOOL]}"
apk_tool="${3:-apk}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
project_root="$(cd "${script_dir}/../../.." && pwd)"
# The pinned default is the alpha-019a50d control-socket armv7 binary (tags kiss
# and js8call, -trimpath, stripped). Override PACKAGE_BINARY_SHA256 for another build.
expected_sha256="${PACKAGE_BINARY_SHA256:-ec7114dde37009743274c7c59e081c87a9648f109c316df34f4627f9e026ec68}"
package_version="${PACKAGE_VERSION:-0.1.0_alpha9-r0}"
config_path="${PACKAGE_CONFIG:-${project_root}/platforms/aredn/config.example.json}"

if [[ "$(id -u)" -ne 0 ]]; then
    printf 'run only as root inside the isolated packaging container\n' >&2
    exit 1
fi

if [[ ! -f "${binary_path}" || ! -x "${binary_path}" ]]; then
    printf 'binary is missing or not executable: %s\n' "${binary_path}" >&2
    exit 1
fi
case "${binary_path}" in
    /input/*) ;;
    *) printf 'binary must come from the read-only /input mount\n' >&2; exit 1 ;;
esac
case "${output_path}" in
    /out/*) ;;
    *) printf 'package output must be written under /out\n' >&2; exit 1 ;;
esac
printf '%s  %s\n' "${expected_sha256}" "${binary_path}" | sha256sum --check --status

output_dir="$(dirname "${output_path}")"
mkdir -p -- "${output_dir}"
stage_root="$(mktemp -d "${output_dir}/.arednlink-hf-package.XXXXXX")"
trap 'rm -rf -- "${stage_root}"' EXIT
pkgroot="${stage_root}/root"
mkdir -p -- "${pkgroot}/usr/sbin" "${pkgroot}/etc/init.d" \
    "${pkgroot}/etc/arednlink-hf" "${pkgroot}/lib/apk/packages"
chmod 0755 "${pkgroot}/usr" "${pkgroot}/usr/sbin" "${pkgroot}/etc" \
    "${pkgroot}/etc/init.d" "${pkgroot}/lib" "${pkgroot}/lib/apk" \
    "${pkgroot}/lib/apk/packages"

# Native UI payload. Destinations follow the relative imports in the ucode files
# (entry loads /app/root.ut and ./arednlink-hf/gate.uc; the page imports
# ../arednlink-hf/*.uc) and the asset URLs the page requests.
# The uhttpd route mapping is NOT installed here; the operator adds it manually.
app_src="${script_dir}/../app"
www_src="${script_dir}/../www"
install -d -m 0755 -- "${pkgroot}/app" "${pkgroot}/app/arednlink-hf" \
    "${pkgroot}/app/main" "${pkgroot}/www" "${pkgroot}/www/apps" \
    "${pkgroot}/www/apps/arednlink-hf" "${pkgroot}/www/cgi-bin" \
    "${pkgroot}/www/cgi-bin/apps" "${pkgroot}/www/cgi-bin/apps/arednlink-hf"
install -m 0644 -- "${app_src}/arednlink-hf-entry.ut" "${pkgroot}/app/arednlink-hf-entry.ut"
install -m 0644 -- "${app_src}/arednlink-hf/gate.uc" "${app_src}/arednlink-hf/ipc.uc" \
    "${pkgroot}/app/arednlink-hf/"
install -m 0644 -- "${app_src}/main/arednlink-hf.ut" "${pkgroot}/app/main/arednlink-hf.ut"
install -m 0644 -- "${www_src}/apps/arednlink-hf/icon.svg" \
    "${www_src}/apps/arednlink-hf/ui.css" "${www_src}/apps/arednlink-hf/ui.js" \
    "${pkgroot}/www/apps/arednlink-hf/"
# Sidebar-discovery anchor; the uhttpd ucode_prefix mapping takes precedence over
# this file for the same URL. Until the mapping exists it answers 503.
install -m 0755 -- "${www_src}/cgi-bin/apps/arednlink-hf/admin" \
    "${pkgroot}/www/cgi-bin/apps/arednlink-hf/admin"

install -m 0755 -- "${binary_path}" "${pkgroot}/usr/sbin/arednlink-hf"
install -m 0755 -- "${script_dir}/../etc/init.d/arednlink-hf" \
    "${pkgroot}/etc/init.d/arednlink-hf"
install -m 0600 -- "${config_path}" \
    "${pkgroot}/etc/arednlink-hf/config.json"
chmod 0700 "${pkgroot}/etc/arednlink-hf"
chown 65534:65534 "${pkgroot}/etc/arednlink-hf" \
    "${pkgroot}/etc/arednlink-hf/config.json"
printf '/etc/arednlink-hf/config.json\n' \
    > "${pkgroot}/lib/apk/packages/arednlink-hf.conffiles"

"${apk_tool}" mkpkg \
    --info 'name:arednlink-hf' \
    --info "version:${package_version}" \
    --info 'arch:armv7' \
    --info 'license:GPL-2.0-only' \
    --info 'maintainer:25Charlie-Dev <gwot25c@atomicmail.io>' \
    --info 'description:HF discovery daemon and native AREDN UI files for AREDN' \
    --files "${pkgroot}" \
    --output "${output_path}"
