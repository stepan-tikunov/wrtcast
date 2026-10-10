# castbff

Backend for the Chromecast remote. It runs on the OpenWrt router as a ucode CGI
script (~10 KB), with no extra packages: `ucode`, `ucode-mod-fs`, `ucode-mod-uci`,
`uclient-fetch` and `uhttpd` are already part of the firmware. It proxies the
[movie-api](../movie-api/) HTTP/JSON gateway and calls [`castctl`](../castctl/).

## Install

```sh
scripts/deploy.sh          # ROUTER=root@192.168.1.1 by default
```

The script copies the CGI to `/www/cast/cgi-bin/castbff` and installs
`/etc/config/castbff` only if it doesn't exist yet. It also creates a separate
uhttpd instance, `uhttpd.cast`, on port 8088 (web root `/www/cast`) and maps
`/api` to the CGI with an alias. LuCI on :80/:443 is not touched.

Configuration (`/etc/config/castbff`):

| option | |
|---|---|
| `api_url` | movie-api HTTP gateway base URL |
| `cast_host` | Chromecast `host[:port]` |
| `token` | if non-empty, required as `X-Token` header or `token` param (not for `/api/p`) |
| `proxy` | `1` (default): relay media through `/api/p` |
| `user_agent` | upstream User-Agent for proxied media; must match movie-api's `MOVIE_PROVIDER_USER_AGENT` (default: Chromecast `CrKey/1.56`) |
| `header` | list of extra upstream headers, e.g. `Referer: …` |
| `public_url` | base URL the Chromecast uses to reach the router (default: the request `Host`) |

## Web UI

`www/index.html` (~14 KB, ~5 KB gzipped, no dependencies) is served at
`http://192.168.1.1:8088/`. Search at the top, a grid of cards below; tapping a
card starts the movie; a series card opens a season/episode picker. The bottom
player sheet has the timeline, ±10/30 s,
volume, audio and subtitle selection, and for series "Episodes" and "Next"
(which crosses into the next season). Status is polled every 2 s while the tab
is visible. If a token is configured, the page asks for it once and keeps it in
`localStorage`.

## API

All responses are JSON. Errors return `{"error": "..."}` with status 400, 401,
404, 405 or 502 (502 means movie-api or castctl failed).

| | |
|---|---|
| `GET /api/search?q=` | `{results:[{id,title,year,type,poster}]}`, `type` is `movie` or `series`, `q` ≥ 3 chars |
| `GET /api/streams?id=` | `{streams:[{index,url,audio:[{id,name,lang,default}],subs:[{id,name,lang,forced}]}]}` |
| `GET /api/series?id=` | `{seasons:[{season,episodes:[{episode,title,duration}]}]}` |
| `POST /api/play` `id`, `stream`=0, `title`?, `season`+`episode`? | fetches fresh streams of the movie (or of the episode when `season`/`episode` are given), runs `castctl play`, returns status |
| `POST /api/cmd` `c`, `arg`? | `pause resume stop quit mute unmute` · `seek ff rew volume` (number) · `audio subs` (id/lang/name/`off`) |
| `GET /api/status` | `castctl status`; when the stream was started via `/api/play`, adds `now:{id,stream,title,season,episode}` and a movie-api `label` to each track |

| `GET /api/p/<name>?u=<url>` | media proxy, see below |

POST parameters can be sent in the query string, as a form, or as JSON. The
last `/api/play` is stored in `/tmp/castbff.json`.

```sh
B=http://192.168.1.1:8088/api
curl -G --data-urlencode 'q=Дюна' $B/search
curl -X POST $B/play -d 'id=67c18f3642185&title=Дюна'
curl -X POST $B/cmd -d 'c=ff&arg=30'
curl $B/status
```

## Media proxy

With `proxy` enabled, everything the TV and the browser load goes through the
router: `/api/play` hands the receiver
`http://<router>:8088/api/p/master.m3u8?u=<signed master URL>`, and search
results return posters as `/api/p/poster.jpg?u=<movie-api>/v1/posters?url=…`.

- The upstream request uses `user_agent` and `header` from the config. Every
  response gets `Access-Control-Allow-Origin: *` and a Content-Type based on
  the extension.
- `.m3u8` is fetched in full and rewritten. Each URI line and each `URI="…"`
  attribute is resolved against the playlist URL and pointed back at
  `/api/p/<file name>`, so variant playlists, segments, keys and subtitles are
  proxied too. `<name>` keeps the upstream file name, so the receiver can tell
  the segment format from the extension.
- Anything else is streamed. Headers are sent only after the first upstream
  chunk arrives, so a failed upstream request (for example `410` on an expired
  signature) returns `502` instead of an empty `200`. The client's `Range`
  header is ignored and the whole file is returned.
- Allowed upstreams: public `http(s)` hosts and `api_url`. Loopback, RFC 1918,
  link-local and dotless/`.lan`/`.local` hosts are rejected with `400`.
  Hostnames are not resolved, so a public name that points into the LAN is not
  caught.
