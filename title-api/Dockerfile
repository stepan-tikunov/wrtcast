# Go cross-compiles, so the build stage runs natively on the builder and only
# the final stage is per-platform.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN mkdir /state
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
	go build -trimpath -ldflags='-s -w' -o /title-api ./cmd/title-api

# A static ffmpeg decodes the frames the alternative player's video is
# aligned with the primary's by (see internal/provider/align.go).
FROM mwader/static-ffmpeg:7.1 AS ffmpeg

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=ffmpeg /ffmpeg /usr/local/bin/ffmpeg
ENV FFMPEG=/usr/local/bin/ffmpeg
COPY --from=build /title-api /title-api
COPY --from=build --chown=65532:65532 /state /var/lib/title-api
ENTRYPOINT ["/title-api"]
