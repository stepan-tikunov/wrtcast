# Movie API

Build and run:

```sh
make
./movie-api
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

`movie_id` is the first eight hexadecimal characters of SHA-1 of the provider
hostname followed by the decimal site movie ID. For the default provider and
site ID `42185`, it is:

```text
67c18f3642185
```

Example request with `grpcurl`:

```sh
grpcurl -plaintext \
  -d '{"movie_id":"67c18f3642185"}' \
  localhost:50051 \
  movie.v1.MovieService/GetStreamsByMovieID
```

Full-text movie search accepts queries of at least three characters:

```sh
grpcurl -plaintext \
  -d '{"query":"Дюна"}' \
  localhost:50051 \
  movie.v1.MovieService/SearchMovies
```

The same method is exposed through its `google.api.http` annotation:

```sh
curl 'http://localhost:8080/v1/movies/67c18f3642185/streams'

curl --get --data-urlencode 'query=Дюна' \
  'http://localhost:8080/v1/movies:search'
```

Search results contain the external movie ID, title, year, and absolute poster
URL. Up to 30 results are returned by the provider's first search page.

The HTTP gateway allows requests from any origin (`Access-Control-Allow-Origin: *`)
and handles CORS preflight requests.

Signed media URLs are generated for the Chromecast receiver User-Agent because
the CDN validates the same User-Agent on HLS segment requests. Override it for a
different receiver firmware with `MOVIE_PROVIDER_USER_AGENT`.

The response contains one entry per playable embed. Each entry has a signed HLS
master playlist URL, a deduplicated list of audio tracks, and available subtitle
tracks. Video variants are intentionally omitted because the HLS player selects
them adaptively from the master playlist.
