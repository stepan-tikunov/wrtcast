#!/bin/sh
# Builds the ucode module wrtcast keeps its state with (sqlite/sqlite.c) for
# OpenWrt aarch64, against the headers of the router's ucode (UCODE_REF) and
# the libsqlite3-0 package (installed by deploy.sh). Alpine's libc soname
# differs from OpenWrt's, so it's patched.
set -e
cd "$(dirname "$0")/.."
UCODE_REF=${UCODE_REF:-3f64c808}  # ucode 2025.07.18~3f64c808 of OpenWrt 24.10
mkdir -p build
docker run --rm --platform linux/arm64 -v "$PWD":/src -w /src -e UCODE_REF="$UCODE_REF" alpine:3.22 sh -ec '
	apk add -q gcc musl-dev sqlite-dev json-c-dev git patchelf >/dev/null
	git clone -q https://github.com/jow-/ucode.git /tmp/ucode
	git -C /tmp/ucode checkout -q "$UCODE_REF"
	gcc -Os -Wall -Wextra -Wno-unused-parameter -fPIC -shared \
		-Wl,-z,max-page-size=4096 -I/tmp/ucode/include \
		-o build/sqlite.so sqlite/sqlite.c -lsqlite3
	strip build/sqlite.so
	patchelf --page-size 4096 --replace-needed libc.musl-aarch64.so.1 libc.so build/sqlite.so
'
ls -l build/sqlite.so
