#!/bin/sh
# Builds the ffmpeg the volume leveler runs: static (musl), aarch64, with
# nothing but AAC, MPEG-TS and the filters wrtcast uses (~1.9 MB).
set -e
cd "$(dirname "$0")/.."
mkdir -p build
docker run --rm --platform linux/arm64 -v "$PWD/build":/out alpine:3.22 sh -ec '
	apk add -q build-base nasm curl xz linux-headers >/dev/null
	curl -sL https://ffmpeg.org/releases/ffmpeg-6.1.2.tar.xz | tar xJ
	cd ffmpeg-6.1.2
	./configure --disable-everything --disable-doc --disable-ffplay --disable-ffprobe \
		--disable-network --disable-autodetect --disable-debug \
		--enable-small --enable-static --disable-shared --extra-ldflags=-static \
		--enable-decoder=aac --enable-encoder=aac --enable-parser=aac \
		--enable-demuxer=mpegts --enable-muxer=mpegts \
		--enable-protocol=file,pipe,concat \
		--enable-filter=dynaudnorm,alimiter,atrim,aresample,aformat,anull,abuffer,abuffersink \
		--enable-bsf=noise >/dev/null
	make -j"$(nproc)" >/dev/null
	strip ffmpeg
	cp ffmpeg /out/wrtcast-ffmpeg
'
ls -l build/wrtcast-ffmpeg
