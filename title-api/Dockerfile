# Go cross-compiles, so the build stage runs natively on the builder and only
# the final stage is per-platform.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN mkdir /state
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
	go build -trimpath -ldflags='-s -w' -o /title-api ./cmd/title-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /title-api /title-api
COPY --from=build --chown=65532:65532 /state /var/lib/title-api
ENTRYPOINT ["/title-api"]
