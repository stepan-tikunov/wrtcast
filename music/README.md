# music

Говорит, где в аудио играет музыка и что это за трек, чтобы title-api
показывал, что звучит в фильме или серии (см. «Музыка» в title-api). Состояния
нет, кроме ограничителя запросов к Shazam: аудио на входе, оценки или трек на
выходе.

**Где играет музыка.** YAMNet, TFLite-классификатор AudioSet на 521 класс
(Apache 2.0, скачивается при сборке образа и в репозитории не лежит), каждые
полсекунды оценивает окно 0,975 с аудио 16 кГц моно. Из оценок берутся три:
музыка (`Music`), речь (`Speech`) и максимум по «песенным» классам (Singing,
Song, Vocal music, Pop music, Rock music и т. п.). На сервере YAMNet работает
примерно в 40 раз быстрее реального времени.

```sh
curl --data-binary @segment.aac http://127.0.0.1:50061/v1/classify
# {"model":"yamnet","hop":0.5,"window":0.975,"duration":4.011,
#  "frames":[[0.0,0.941,0.016,0.0],[0.5,…],…],
#  "features":[[31.2,-4.5,…],…],"ms":251}
```

`frames` — `[t, music, speech, song]`, где `t` — начало окна в секундах от
начала аудио. `features` — тембр каждого окна: 12 MFCC (40 мел-полос от 60 Гц
до 7,6 кГц, без c0, то есть без громкости), считаются numpy. По ним title-api
находит стык двух песен. На вход годится любое аудио, которое читает ffmpeg,
в том числе ADTS AAC, который title-api вырезает из сегмента. Разбирается не
больше 120 с.

**Что играет.** Shazam через [shazamio](https://github.com/shazamio/ShazamIO),
неофициальный клиент. Подпись считается здесь, в Shazam уходит только она.
Хватает 8–12 с аудио, берутся первые 20 с.

```sh
curl --data-binary @clip.aac http://127.0.0.1:50061/v1/recognize
# {"match":{"key":"109354078","title":"Don't Stop Believin'","artist":"Journey",
#  "cover":"https://…/400x400cc.jpg","url":"https://www.shazam.com/track/…",
#  "isrc":"USSM18100116","apple":"160024096","apple_url":"https://music.apple.com/…",
#  "spotify":"spotify:search:…","offset":94.7,"timeskew":-0.0002,
#  "frequencyskew":-0.0004,"duration":248.9},"ms":1946}
```

- `offset` — с какой секунды трека начинается присланное аудио; отсюда
  title-api знает, где трек начался в серии.
- `duration` — длина трека по открытому iTunes Lookup (id из `apple`), или
  `null`; по ней title-api не переспрашивает Shazam о продолжении той же песни
  после диалога. Длины запоминаются в памяти.
- `timeskew` / `frequencyskew` — насколько аудио растянуто или сдвинуто по
  тону относительно записи; у ошибочных совпадений они обычно больше.
- Нет совпадения — `{"match":null}`.

**Ограничитель.** Не больше `MUSIC_SHAZAM_MAX` запросов за любые
`MUSIC_SHAZAM_WINDOW` секунд (по умолчанию 8 за 300 с): редкие запросы
подряд проходят, сплошной поток — нет. Если Shazam ответил 429, запросов нет
`MUSIC_SHAZAM_COOLDOWN` секунд (час). В эти моменты и при
`MUSIC_SHAZAM=off` ответ 503 `{"error":"limited"|"off","retry_after":s}`.
Повторов нет вовсе. shazamio по умолчанию молча повторяет 429 до 20 раз с
растущими паузами: так на прогоне раз в ~6 с каждый запрос тянулся ~25 с
вместо одной секунды.

Запросы к Shazam и iTunes идут с адреса сервера, не через домашний SOCKS
title-api: в compose у контейнера нет `HTTPS_PROXY`.

`GET /healthz` — `{"ok":true,…}`, когда YAMNet загружен.

Переменные: `MUSIC_ADDR` (по умолчанию `127.0.0.1:50061`), `MUSIC_THREADS`
(потоки TFLite, 1), `MUSIC_MODELS` (каталог моделей, `/models`) и
`MUSIC_SHAZAM*` выше.

```sh
docker build -t wrtcast/music:latest . && docker compose up -d
```

Образ собирается прямо на сервере, как `faces`.
