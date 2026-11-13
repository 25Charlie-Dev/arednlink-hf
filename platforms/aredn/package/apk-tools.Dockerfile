# Packaging tool image: upstream apk-tools 3.0.5, the version AREDN firmware ships.
# The source archive is the OpenWrt mirror copy, pinned to the OpenWrt recipe checksum.
FROM alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f
RUN apk add --no-cache build-base meson pkgconf openssl-dev zlib-dev zstd-dev zstd linux-headers bash wget file coreutils
WORKDIR /build
RUN wget -q https://sources.openwrt.org/apk-3.0.5.tar.zst \
 && echo "bdb70571302d4baeafd4558522a49ce4c834c2acb51b954a19a3eea7f20b01b7  apk-3.0.5.tar.zst" | sha256sum -c - \
 && zstd -dc apk-3.0.5.tar.zst | tar -x
RUN cd "$(ls -d /build/*/ | head -1)" \
 && meson setup build -Dhelp=disabled -Dlua=disabled -Ddocs=disabled -Dtests=disabled -Durl_backend=wget \
 && ninja -C build \
 && mkdir -p /opt/apk3 && cp build/src/apk /opt/apk3/apk \
 && for f in build/src/libapk.so*; do [ -d "$f" ] || cp -P "$f" /opt/apk3/; done \
 && LD_LIBRARY_PATH=/opt/apk3 /opt/apk3/apk --version
