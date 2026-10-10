# Title API

Сборка и запуск:

```sh
make
./title-api
```

По умолчанию gRPC-сервер слушает `:50051`, а HTTP/JSON-шлюз — `:8080`.
Настройки:

- `GRPC_ADDR` — адрес gRPC-сервера;
- `HTTP_ADDR` — адрес HTTP-шлюза;
- `MOVIE_PROVIDER_BASE_URL` — базовый URL провайдера (если не задан,
  используется встроенный);
- `MOVIE_PROVIDER_USER_AGENT` — User-Agent ресивера (см. ниже);
- `HTTPS_PROXY` / `HTTP_PROXY` — прокси для исходящих запросов, в том числе
  `socks5://…`.

Антибот-сессия создаётся до запуска gRPC-сервера, обновляется каждые пять
минут и сразу же, если запрос наткнулся на новую проверку.

## Docker

Образ для amd64 и arm64 собирается GitHub Actions при каждом изменении
`title-api/` в `master` и публикуется в GitHub Container Registry:

```sh
docker run -p 8080:8080 ghcr.io/stepan-tikunov/wrtcast/title-api
```

Теги: `latest` (последний `master`), `sha-<коммит>` и `<версия>` для тегов
`v*`. Собрать локально: `docker build -t title-api .`

[`compose.yaml`](compose.yaml) рассчитан на VPS, где сервис доступен только
через WireGuard: HTTP-шлюз слушает WireGuard-адрес сервера, gRPC — только
loopback, наружу ничего не публикуется. Исходящие запросы можно направить
через SOCKS5-прокси на домашнем роутере, чтобы провайдер и его CDN видели тот
же IP, с которого потом загружаются сегменты:

```sh
WG_ADDR=10.0.0.1 EGRESS_PROXY=socks5://10.0.0.2:1080 docker compose up -d
docker compose pull && docker compose up -d     # обновление
```

## Идентификаторы

`title_id` — первые восемь шестнадцатеричных символов SHA-1 от имени хоста
провайдера, за которыми идёт десятичный ID тайтла на сайте. Для провайдера по
умолчанию и ID `42185` получается:

```text
67c18f3642185
```

## Поиск

Полнотекстовый поиск принимает запросы от трёх символов:

```sh
grpcurl -plaintext \
  -d '{"query":"Дюна"}' \
  localhost:50051 \
  title.v1.TitleService/SearchTitles
```

Тот же метод доступен через аннотацию `google.api.http`:

```sh
curl --get --data-urlencode 'query=Дюна' \
  'http://localhost:8080/v1/search'
```

Результаты содержат ID тайтла, название, год и абсолютный URL постера.
Возвращается до 30 результатов с первой страницы поиска провайдера.

## Подробности о тайтле

Полная информация о тайтле, включая достоверный тип (фильм или сериал) и
каталог сезонов и серий, доступна через `GetTitleDetails`:

```sh
curl 'http://localhost:8080/v1/titles/67c18f3643222'

grpcurl -plaintext \
  -d '{"title_id":"67c18f3643222"}' \
  localhost:50051 \
  title.v1.TitleService/GetTitleDetails
```

Для фильмов `durationSeconds` берётся из метаданных плеера, если они есть.
Если нет, сервис открывает подписанный master-плейлист HLS и суммирует
длительности `#EXTINF` выбранного медиаплейлиста. У сериалов длительность
указывается для каждой серии, а не для тайтла целиком.

## Потоки

`GetPlaybackStreams` возвращает потоки фильма, если передан только `title_id`.
Для сериала передайте `season_number` и `episode_number`, чтобы выбрать серию:

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

Для фильмов поля сезона и серии игнорируются.

Ответ содержит по одной записи на каждый доступный плеер. В каждой записи —
подписанный URL master-плейлиста HLS, список аудиодорожек без дублей и
доступные субтитры. Видеоварианты намеренно не перечисляются: HLS-плеер
выбирает их сам из master-плейлиста.

Подписанные ссылки на медиа запрашиваются с User-Agent ресивера Chromecast,
потому что CDN проверяет тот же User-Agent при загрузке HLS-сегментов. Для
другой прошивки ресивера его можно заменить через `MOVIE_PROVIDER_USER_AGENT`.

## Постеры

Постеры на сайте провайдера закрыты той же антибот-проверкой, поэтому сервис
отдаёт их через свою сессию:

```sh
curl --get --data-urlencode 'url=<posterLink>' \
  'http://localhost:8080/v1/posters' -o poster.jpg
```

Принимаются только URL на хосте провайдера (иначе `400`); ошибки источника
возвращают `502`.

## CORS

HTTP-шлюз принимает запросы с любого источника
(`Access-Control-Allow-Origin: *`) и обрабатывает preflight-запросы CORS.
