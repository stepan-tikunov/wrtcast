#!/bin/sh
# Builds castctl for OpenWrt aarch64 (musl, mbedtls 3.6) inside an Alpine
# container. Alpine's libc soname differs from OpenWrt's, so it's patched.
set -e
cd "$(dirname "$0")/.."
mkdir -p build
docker run --rm --platform linux/arm64 -v "$PWD":/src -w /src alpine:3.22 sh -ec '
	apk add -q gcc musl-dev mbedtls-dev patchelf >/dev/null
	gcc -Os -Wall -Wextra -Wno-unused-parameter -Wl,-z,max-page-size=4096 \
		-o build/castctl-aarch64 src/castctl.c \
		-lmbedtls -lmbedx509 -lmbedcrypto
	strip build/castctl-aarch64
	patchelf --page-size 4096 --replace-needed libc.musl-aarch64.so.1 libc.so build/castctl-aarch64
	patchelf --page-size 4096 --set-interpreter /lib/ld-musl-aarch64.so.1 build/castctl-aarch64
'
ls -l build/castctl-aarch64
