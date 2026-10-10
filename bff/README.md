# castbff

Backend for the Chromecast remote. It runs on the OpenWrt router as a ucode CGI
script (~10 KB), with no extra packages: `ucode`, `ucode-mod-fs`, `ucode-mod-uci`,
`uclient-fetch` and `uhttpd` are already part of the firmware. It proxies the
[title-api](../title-api/) HTTP/JSON gateway and calls [`castctl`](../castctl/).

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
| `api_url` | title-api HTTP gateway base URL |
| `cast_host` | default Chromecast: `host[:port]`, friendly name or device id; empty = first one found |
| `token` | if non-empty, required as `X-Token` header or `token` param (not for `/api/p`) |
| `proxy` | `1` (default): relay media through `/api/p` |
| `user_agent` | upstream User-Agent for proxied media; must match title-api's `MOVIE_PROVIDER_USER_AGENT` (default: Chromecast `CrKey/1.56`) |
| `header` | list of extra upstream headers, e.g. `Referer: …` |
| `public_url` | base URL the Chromecast uses to reach the router (default: the request `Host`) |

## Web UI

`www/index.html` (~29 KB, ~10 KB gzipped, no dependencies) is served at
`http://192.168.1.1:8088/`.

- Search at the top; the last results are restored instantly from
  `localStorage`. The card of what is on the TV gets a "На ТВ" badge.
- Tapping a card opens a details page instead of starting playback right away.
  It loads `/api/title` (cached per page load): original title, year,
  countries, duration, Kinopoisk/IMDb ratings, a description that expands on
  tap, genres, director and cast. Search results don't say whether a title is a
  movie or a series, so the type comes from there too: a movie gets a
  "Смотреть на ТВ" button, a series gets season chips and an episode list with
  the current episode highlighted.
- While `/api/play` is running (~9 s), the mini player shows the new movie
  with a spinner, repeated taps are ignored, and status polls don't overwrite
  it. The playback controls (timeline, tracks, buttons) are hidden until the
  new status arrives, since they would still describe the previous movie.
- The mini player has a poster, a progress line and play/pause. Tapping it
  opens the full player over a dimmed backdrop: timeline with elapsed and
  remaining time, ±10 s, volume, audio and subtitle selection. For series the
  transport row gets previous/next episode buttons (crossing season
  boundaries) and there is an "Серии" button for the episode list.
- The phone's back button closes the player and the details page.
- Play/pause updates instantly. Status is polled every 2 s while the tab is
  visible, and the position between polls is extrapolated so the timer ticks
  every second. A lost connection is reported once, not on every poll.
- The TV button next to search opens a "Где смотреть" sheet with the
  Chromecasts found on the network (rescanned on open). The choice is kept in
  `localStorage` and sent as `device` with every status/play/cmd request; with
  several devices the button is highlighted and the full player shows which
  TV it controls. If the saved TV disappears, the page falls back to the
  default one.
- If a token is configured, the page asks for it once and keeps it in
  `localStorage`.
- Motion is CSS-only: skeleton cards while searching, staggered cards and
  episodes, posters fading in, the details page sliding in and out, the
  player sheet expanding (`grid-template-rows` 0fr→1fr) over a fading
  backdrop, a springy play/pause icon. All of it is disabled under
  `prefers-reduced-motion`.

## API

All responses are JSON. Errors return `{"error": "..."}` with status 400, 401,
404, 405, 502 (title-api or castctl failed) or 503 (no Chromecast found).

| | |
|---|---|
| `GET /api/search?q=` | `{results:[{id,title,year,poster}]}`, `q` ≥ 3 chars |
| `GET /api/streams?id=` | `{streams:[{index,url,audio:[{id,name,lang,default}],subs:[{id,name,lang,forced}]}]}` |
| `GET /api/title?id=` | `{id,title,original,year,description,poster,type,quality,countries,genres,directors,actors,ratings:[{source,value}],duration,seasons}`; `type` is `movie` or `series`, `seasons` (`[{season,episodes:[{episode,title,duration}]}]`) is `null` for movies; site sections and years are dropped from `genres` |
| `POST /api/play` `id`, `stream`=0, `title`?, `poster`?, `season`+`episode`? | fetches fresh streams of the movie (or of the episode when `season`/`episode` are given), runs `castctl play`, returns status |
| `POST /api/cmd` `c`, `arg`? | `pause resume stop quit mute unmute` · `seek ff rew volume` (number) · `audio subs` (id/lang/name/`off`) |
| `GET /api/devices` `refresh`? | `{devices:[{id,name,model,host,port}],default}`: `castctl discover`, cached for 60 s unless `refresh=1`; `default` is the id `cast_host` points to |
| `GET /api/status` | `castctl status`; when the stream was started via `/api/play`, adds `now:{id,stream,title,poster,season,episode}` and a title-api `label` to each track; always adds `device:{id,name}` |
| `GET /api/p/<name>?u=<url>` | media proxy, see below |

POST parameters can be sent in the query string, as a form, or as JSON.

`status`, `play` and `cmd` take an optional `device=<id>` from `/api/devices`
(only discovered ids, so clients can't point castctl at arbitrary hosts);
without it `cast_host` is used. Discovery results are cached in
`/tmp/castbff.devices.json`; an unknown id triggers a rescan, and when castctl
fails on a cached address the device is looked up again in case DHCP moved it.
The last `/api/play` per TV is stored in `/tmp/castbff.now.<host>`.

```sh
B=http://192.168.1.1:8088/api
curl -G --data-urlencode 'q=Дюна' $B/search
curl "$B/title?id=67c18f3642185"
curl -X POST $B/play -d 'id=67c18f3642185&title=Дюна'
curl -X POST $B/cmd -d 'c=ff&arg=30'
curl $B/status
curl $B/devices
curl "$B/status?device=0123456789abcdef0123456789abcdef"
```

## Media proxy

With `proxy` enabled, everything the TV and the browser load goes through the
router: `/api/play` hands the receiver
`http://<router>:8088/api/p/master.m3u8?u=<signed master URL>`, and search
results return posters as `/api/p/poster.jpg?u=<title-api>/v1/posters?url=…`.

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
