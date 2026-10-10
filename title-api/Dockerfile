FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /title-api ./cmd/title-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /title-api /title-api
ENTRYPOINT ["/title-api"]
