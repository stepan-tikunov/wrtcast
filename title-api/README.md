# Title API

Build and run:

```sh
make
./title-api
```

The gRPC server listens on `:50051` and the HTTP/JSON gateway on `:8080` by
default. Configuration:

- `GRPC_ADDR` — listen address;
- `HTTP_ADDR` — HTTP gateway listen address;
- `MOVIE_PROVIDER_BASE_URL` — provider base URL (a built-in default is used
  when omitted).

The anti-bot session is initialized before the gRPC listener starts, refreshed
every five minutes, and refreshed immediately when a request encounters a new
challenge.

`title_id` is the first eight hexadecimal characters of SHA-1 of the provider
hostname followed by the decimal site title ID. For the default provider and
site ID `42185`, it is:

```text
67c18f3642185
```

Full-text title search accepts queries of at least three characters:

```sh
grpcurl -plaintext \
  -d '{"query":"Дюна"}' \
  localhost:50051 \
  title.v1.TitleService/SearchTitles
```

The same method is exposed through its `google.api.http` annotation:

```sh
curl --get --data-urlencode 'query=Дюна' \
  'http://localhost:8080/v1/search'
```

Search results contain the title ID, title, year, absolute poster URL, and
duration in seconds. Up to 30 results are returned by the provider's first
search page. The provider does not include duration in search cards, so
`durationSeconds` is `0` when it cannot be determined without fetching every
title separately.

Full title details, including the authoritative media type and a series season
and episode catalog, are available through `GetTitleDetails`:

```sh
curl 'http://localhost:8080/v1/titles/67c18f3643222'

grpcurl -plaintext \
  -d '{"title_id":"67c18f3643222"}' \
  localhost:50051 \
  title.v1.TitleService/GetTitleDetails
```

`GetPlaybackStreams` returns movie streams when only `title_id` is provided. For
a series, pass `season_number` and `episode_number` to select the episode:

```sh
curl 'http://localhost:8080/v1/titles/67c18f3642185/playback-streams'

curl --get \
  --data-urlencode 'season_number=1' \
  --data-urlencode 'episode_number=1' \
  'http://localhost:8080/v1/titles/67c18f3654016/playback-streams'

grpcurl -plaintext \
  -d '{"title_id":"67c18f3654016","season_number":1,"episode_number":"1"}' \
  localhost:50051 \
  title.v1.TitleService/GetPlaybackStreams
```

Season and episode fields are ignored for movies.

The HTTP gateway allows requests from any origin (`Access-Control-Allow-Origin: *`)
and handles CORS preflight requests.

Poster images on the provider site are behind the same anti-bot challenge, so
they are served through the service's session:

```sh
curl --get --data-urlencode 'url=<moviePosterLink>' \
  'http://localhost:8080/v1/posters' -o poster.jpg
```

Only URLs on the provider host are accepted (`400` otherwise); upstream failures
return `502`.

Signed media URLs are generated for the Chromecast receiver User-Agent because
the CDN validates the same User-Agent on HLS segment requests. Override it for a
different receiver firmware with `MOVIE_PROVIDER_USER_AGENT`.

The response contains one entry per playable embed. Each entry has a signed HLS
master playlist URL, a deduplicated list of audio tracks, and available subtitle
tracks. Video variants are intentionally omitted because the HLS player selects
them adaptively from the master playlist.
