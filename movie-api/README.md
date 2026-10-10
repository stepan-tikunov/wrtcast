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

Search results contain the external movie ID, title, year, absolute poster URL,
and a cheap path-based `mediaType` hint. Ambiguous categories such as animation
and anime return `MEDIA_TYPE_UNSPECIFIED`; use title metadata for the authoritative
type. Up to 30 results are returned by the provider's first search page.

Title metadata is available over HTTP and gRPC:

```sh
curl 'http://localhost:8080/v1/titles/67c18f3643222'

grpcurl -plaintext \
  -d '{"movie_id":"67c18f3643222"}' \
  localhost:50051 \
  movie.v1.MovieService/GetTitleMetadata
```

`GetTitleMetadata` returns the title, original title, year, description, poster,
quality, countries, genres, directors, actors, Kinopoisk/IMDb ratings, and an
authoritative `mediaType`. The type is detected from the actual player payload:
a seasonal playlist is a series even when its catalog URL is under animation or
anime, while a single HLS source is a movie.

For a series, first request its season and episode catalog. The catalog contains
metadata only, so it does not expose signed URLs that may expire before playback:

```sh
curl 'http://localhost:8080/v1/series/67c18f3620973'
```

After the viewer selects an episode, request fresh streams for that episode:

```sh
curl 'http://localhost:8080/v1/series/67c18f3620973/seasons/8/episodes/1/streams'
```

The equivalent gRPC methods are `GetSeries` and `GetEpisodeStreams`. Passing a
movie ID to either series method returns gRPC `FAILED_PRECONDITION` (HTTP 400).
Passing a series ID to `GetStreamsByMovieID` returns the same type-mismatch
status instead of selecting an arbitrary episode. A missing episode returns
gRPC `NOT_FOUND` (HTTP 404).

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
