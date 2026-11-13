#!/usr/bin/env bash
# Build the APK and check it in a scratch root. Runs offline inside the
# apk-tools image: /src read-only source, /input/arednlink-hf the pinned
# armv7 binary, /out writable. Usage: check-package.sh [APK_TOOL]
set -euo pipefail
apk_tool="${1:-/opt/apk3/apk}"
LD_LIBRARY_PATH="$(dirname "${apk_tool}")"
export LD_LIBRARY_PATH
pkgdir=/src/platforms/aredn/package
version="$(sed -n 's/^PACKAGE_VERSION=//p' "${pkgdir}/release.env")"
pkg="/out/arednlink-hf-${version}.apk"
die() { printf 'check-package: %s\n' "$1" >&2; exit 1; }

"${apk_tool}" --version
bash "${pkgdir}/prepare.sh" /input/arednlink-hf "${pkg}" "${apk_tool}"
[ "$(head -c4 "${pkg}" | od -An -tx1 | tr -d ' \n')" = 41444264 ] || die "not an ADB v3 package"
info="$("${apk_tool}" adbdump "${pkg}")"
grep -q "version: ${version}$" <<<"${info}" || die "wrong package version"
grep -q 'arch: armv7$' <<<"${info}" || die "wrong architecture"
grep -q 'license: GPL-2.0-only$' <<<"${info}" || die "license missing"
! grep -qE '^\s*scripts:' <<<"${info}" || die "unexpected lifecycle scripts"

scratch=/out/scratch
rm -rf "${scratch}" && mkdir -p "${scratch}"
"${apk_tool}" --arch armv7 --root "${scratch}" --allow-untrusted --initdb --no-network add "${pkg}"
cmp -s "${scratch}/usr/sbin/arednlink-hf" /input/arednlink-hf || die "installed binary differs"
[ "$(stat -c '%a %u' "${scratch}/etc/arednlink-hf")" = "700 65534" ] || die "config directory mode or owner"
[ "$(stat -c '%a %u' "${scratch}/etc/arednlink-hf/config.json")" = "600 65534" ] || die "config file mode or owner"
if compgen -G "${scratch}/etc/rc.d/*arednlink*" > /dev/null; then die "service enabled at install"; fi

printf ' ' >> "${scratch}/etc/arednlink-hf/config.json"
before="$(sha256sum < "${scratch}/etc/arednlink-hf/config.json")"
fixture=/out/fixture
mkdir -p "${fixture}"
cp /src/platforms/aredn/config.example.json "${fixture}/config.json"
printf '\n' >> "${fixture}/config.json"
PACKAGE_VERSION="${version%-r*}-r1" PACKAGE_CONFIG="${fixture}/config.json" \
	bash "${pkgdir}/prepare.sh" /input/arednlink-hf "${fixture}/upgrade.apk" "${apk_tool}"
"${apk_tool}" --root "${scratch}" --allow-untrusted --no-network add --upgrade "${fixture}/upgrade.apk"
[ "$(sha256sum < "${scratch}/etc/arednlink-hf/config.json")" = "${before}" ] || die "upgrade replaced edited config"
[ -e "${scratch}/etc/arednlink-hf/config.json.apk-new" ] || die "upgrade did not stage new default"

"${apk_tool}" --root "${scratch}" --no-network del arednlink-hf
[ ! -e "${scratch}/usr/sbin/arednlink-hf" ] || die "binary left after removal"
[ -e "${scratch}/etc/arednlink-hf/config.json" ] || die "edited config removed"
rm -rf "${scratch}" "${fixture}"
sha256sum "${pkg}"
echo "package checks passed"
