# castbff

Backend for the Chromecast remote. It runs on the OpenWrt router as a ucode CGI
script (~5 KB), with no extra packages: `ucode`, `ucode-mod-fs`, `ucode-mod-uci`,
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
| `token` | if non-empty, required as `X-Token` header or `token` param |

## Web UI

`www/index.html` (~9 KB, ~4 KB gzipped, no dependencies) is served at
`http://192.168.1.1:8088/`. Search at the top, a grid of cards below; tapping a
card starts the movie. The bottom player sheet has the timeline, ±10/30 s,
volume, audio and subtitle selection. Status is polled every 2 s while the tab
is visible. If a token is configured, the page asks for it once and keeps it in
`localStorage`.

## API

All responses are JSON. Errors return `{"error": "..."}` with status 400, 401,
404, 405 or 502 (502 means movie-api or castctl failed).

| | |
|---|---|
| `GET /api/search?q=` | `{results:[{id,title,year,poster}]}`, `q` ≥ 3 chars |
| `GET /api/streams?id=` | `{streams:[{index,url,audio:[{id,name,lang,default}],subs:[{id,name,lang,forced}]}]}` |
| `POST /api/play` `id`, `stream`=0, `title`? | fetches fresh streams, runs `castctl play <master>`, returns status |
| `POST /api/cmd` `c`, `arg`? | `pause resume stop quit mute unmute` · `seek ff rew volume` (number) · `audio subs` (id/lang/name/`off`) |
| `GET /api/status` | `castctl status`; when the stream was started via `/api/play`, adds `now:{id,stream,title}` and a movie-api `label` to each track |

POST parameters can be sent in the query string, as a form, or as JSON. The
last `/api/play` is stored in `/tmp/castbff.json`.

```sh
B=http://192.168.1.1:8088/api
curl -G --data-urlencode 'q=Дюна' $B/search
curl -X POST $B/play -d 'id=67c18f3642185&title=Дюна'
curl -X POST $B/cmd -d 'c=ff&arg=30'
curl $B/status
```
