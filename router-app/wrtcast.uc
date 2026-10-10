#!/usr/bin/ucode
// wrtcast — CGI backend for the Chromecast remote. Proxies the title-api
// HTTP/JSON gateway, drives castctl and relays media (HLS playlists, segments,
// posters) so upstream headers and CORS are under our control. Chromecasts
// are found with castctl discover; requests pick one with device=<id>.
// Configured in /etc/config/wrtcast.
'use strict';

import { popen, readfile, writefile, stdin, stdout } from 'fs';
import { cursor } from 'uci';

const STATE = '/tmp/wrtcast.now.';         // + host key; what /play started
const DEVICES = '/tmp/wrtcast.devices.json'; // last discovery result
const DEVICES_TTL = 60;                      // seconds before /devices rescans
const COMMANDS = {
	pause: null, resume: null, stop: null, quit: null, mute: null, unmute: null,
	seek: 'num', ff: 'num', rew: 'num', volume: 'num', audio: 'str', subs: 'str',
	substyle: 'style',
};
const REASONS = {
	'200': 'OK', '400': 'Bad Request', '401': 'Unauthorized',
	'404': 'Not Found', '405': 'Method Not Allowed', '502': 'Bad Gateway',
	'503': 'Service Unavailable',
};
const MIME = {
	m3u8: 'application/vnd.apple.mpegurl', m3u: 'application/vnd.apple.mpegurl',
	ts: 'video/mp2t', aac: 'audio/aac', mp4: 'video/mp4', m4s: 'video/mp4',
	m4a: 'audio/mp4', vtt: 'text/vtt', webvtt: 'text/vtt', key: 'application/octet-stream',
	jpg: 'image/jpeg', jpeg: 'image/jpeg', png: 'image/png', webp: 'image/webp',
};

const uci = cursor();
const conf = (k, d) => uci.get('wrtcast', 'main', k) ?? d;
const API = rtrim(conf('api_url', 'http://127.0.0.1:8080'), '/');
const CAST_HOST = conf('cast_host', '');
const TOKEN = conf('token', '');
const PROXY = conf('proxy', '1') == '1';
// Must match the User-Agent title-api used to obtain the signed media URLs.
const USER_AGENT = conf('user_agent', 'Mozilla/5.0 (X11; Linux armv7l) ' +
	'AppleWebKit/537.36 (KHTML, like Gecko) Chrome/90.0.4430.225 Safari/537.36 ' +
	'CrKey/1.56.500000');
const HEADERS = conf('header', []);

function reply(code, body) {
	printf('Status: %d %s\r\nContent-Type: application/json\r\n' +
	       'Cache-Control: no-store\r\n\r\n%J\n', code, REASONS[`${code}`], body);
	exit(0);
}

const fail = (code, msg) => reply(code, { error: msg });

function shq(s) {
	return "'" + replace(s, "'", "'\\''") + "'";
}

function urlenc(s) {
	return replace(s, /[^A-Za-z0-9._~-]/g, c => sprintf('%%%02X', ord(c)));
}

function urldec(s) {
	return replace(replace(s, '+', ' '), /%([0-9A-Fa-f]{2})/g, (m, h) => chr(hex(h)));
}

function parse_form(s, into) {
	for (let kv in split(s ?? '', '&')) {
		if (kv == '')
			continue;
		let i = index(kv, '=');
		if (i < 0)
			into[urldec(kv)] = '';
		else
			into[urldec(substr(kv, 0, i))] = urldec(substr(kv, i + 1));
	}
	return into;
}

function params() {
	let p = parse_form(getenv('QUERY_STRING'), {});
	let len = +(getenv('CONTENT_LENGTH') ?? 0);
	if (len > 0 && len <= 4096) {
		let body = stdin.read(len);
		if (index(getenv('CONTENT_TYPE') ?? '', 'json') >= 0) {
			let j = json(body);
			for (let k, v in (type(j) == 'object' ? j : {}))
				p[k] = type(v) == 'string' ? v : sprintf('%s', v);
		} else {
			parse_form(body, p);
		}
	}
	return p;
}

// Runs a command; returns { code, out } with stdout and stderr combined.
function run(cmd) {
	let p = popen(cmd + ' 2>&1');
	if (!p)
		return { code: 127, out: 'popen failed' };
	let out = p.read('all') ?? '';
	return { code: p.close(), out: trim(out) };
}

function api(path) {
	let r = run(`uclient-fetch -q -T 20 -O - ${shq(API + path)}`);
	if (r.code != 0)
		fail(502, 'title-api request failed');
	let j = json(r.out);
	if (type(j) != 'object')
		fail(502, 'title-api returned invalid JSON');
	return j;
}

// ---- devices ---------------------------------------------------------------

function discover() {
	let r = run('castctl discover -t 1.5');
	let list = r.code == 0 ? json(r.out) : null;
	if (type(list) != 'array')
		fail(502, r.out || 'discovery failed');
	list = map(list, d => ({
		id: d.id, name: d.name, model: d.model, host: d.host, port: d.port,
	}));
	writefile(DEVICES, sprintf('%J', { time: time(), devices: list }));
	return list;
}

// Cached device list no older than max_age seconds (any age when null).
function cached(max_age) {
	let c = json(readfile(DEVICES) ?? 'null');
	if (type(c) != 'object' || type(c.devices) != 'array' ||
	    (max_age != null && time() - c.time >= max_age))
		return null;
	return c.devices;
}

// Device by id or friendly name; the first one when want is empty.
function pick(list, want) {
	for (let d in list ?? [])
		if (want == '' || d.id == want || lc(d.name ?? '') == lc(want))
			return d;
	return null;
}

const is_host = s => match(s, /^[^ ]*[.:][^ ]*$/);

// Chromecast for this request: device=<id> from the client (only discovered
// devices, so clients can't point castctl at arbitrary hosts), else cast_host
// from the config (an address, a name or an id), else the first one found.
function device(p) {
	let want = p.device ?? '';
	if (want != '' && !match(want, /^[0-9A-Za-z-]{1,64}$/))
		fail(400, 'invalid device');
	if (want == '')
		want = CAST_HOST;
	if (want != '' && is_host(want)) {
		for (let d in cached() ?? [])
			if (want == d.host || want == `${d.host}:${d.port}`)
				return d;
		return { host: want };
	}
	let d = pick(cached(), want) ?? pick(discover(), want);
	if (!d)
		fail(want == '' ? 503 : 404,
		     want == '' ? 'no Chromecast found' : 'device not found');
	return d;
}

const host_of = d => d.port ? `${d.host}:${d.port}` : d.host;

function castctl(dev, ...args) {
	let cmd = '';
	for (let a in args)
		cmd += ' ' + shq(a);
	let r = run('castctl -H ' + shq(host_of(dev)) + cmd);
	// A discovered device may have got a new address from DHCP.
	if (r.code != 0 && dev.id) {
		let d = pick(discover(), dev.id);
		if (d && host_of(d) != host_of(dev)) {
			dev.host = d.host;
			dev.port = d.port;
			r = run('castctl -H ' + shq(host_of(dev)) + cmd);
		}
	}
	if (r.code != 0)
		fail(502, r.out || 'castctl failed');
	let lines = split(r.out, '\n');
	let j = json(lines[-1]);
	if (type(j) != 'object')
		fail(502, 'castctl returned invalid JSON');
	return j;
}

// Like castctl() but returns null instead of failing the request.
function castctl_try(dev, ...args) {
	let cmd = 'castctl -H ' + shq(host_of(dev));
	for (let a in args)
		cmd += ' ' + shq(a);
	let r = run(cmd);
	let j = r.code == 0 ? json(split(r.out, '\n')[-1]) : null;
	return type(j) == 'object' ? j : null;
}

const state_file = dev => STATE + replace(host_of(dev), /[^0-9A-Za-z.]/g, '_');

// Character count of a UTF-8 string (continuation bytes are skipped).
function ulength(s) {
	let n = 0;
	for (let i = 0; i < length(s); i++)
		if ((ord(s, i) & 0xC0) != 0x80)
			n++;
	return n;
}

// castctl subtitle style: "size=1.3,color=#FFFF00,bg=#00000099,edge=outline".
const valid_style = s => length(s) <= 200 &&
	match(s, /^[a-z]+=[#.0-9A-Za-z]+(,[a-z]+=[#.0-9A-Za-z]+)*$/);

function title_id(p) {
	if (!match(p.id ?? '', /^[0-9A-Za-z]{1,64}$/))
		fail(400, 'invalid id');
	return p.id;
}

// Streams of a movie, or of an episode when season is given.
function streams(id, season, episode) {
	let r = api(`/v1/titles/${id}/playback-streams` + (season == null ? '' :
		`?season_number=${season}&episode_number=${urlenc(episode)}`));
	return map(r.streams ?? [], (s, i) => ({
		index: i,
		name: s.name || `Плеер ${i + 1}`,
		url: s.masterPlaylistUrl,
		// Headers the CDN requires (e.g. Origin), as "Name: value" lines.
		headers: join('\n', map(s.proxyHeaders ?? [], h => `${h.name}: ${h.value}`)),
		audio: map(s.audioTracks ?? [], a => ({
			id: a.id, name: a.name, lang: a.language, default: !!a.default,
		})),
		// Provider names end with the track number ("Рус. полные - 5").
		subs: map(s.subtitles ?? [], t => ({
			id: t.id, name: replace(t.name ?? '', / +- +[0-9]+$/, ''),
			lang: t.language, forced: !!t.forced, url: t.playlistUrl,
		})),
	}));
}

// Adds the device, the movie started via /play and human-readable track
// labels: the receiver names tracks by the manifest NAME, which is the
// title-api track id.
function with_now(st, dev) {
	st.device = { id: dev.id ?? null, name: dev.name ?? host_of(dev) };
	let now = json(readfile(state_file(dev)) ?? 'null');
	if (!now || !st.media || st.media.url != now.url)
		return st;
	// Started before qualities were recorded: look them up once from the
	// master playlist the receiver is playing (it plays every variant).
	if (now.qualities == null && PROXY) {
		let u = match(now.url, /[?&]u=([^&]*)/);
		now.src = u ? urldec(u[1]) : null;
		let v = now.src && proxy_target(now.src) ? qualities(now.src) : [];
		now.variants = v;
		now.qualities = map(v, x => x.label);
		now.quality = 'auto';
		writefile(state_file(dev), sprintf('%J', now));
	}
	st.now = {
		id: now.id, stream: now.stream, title: now.title, poster: now.poster,
		season: now.season, episode: now.episode,
		quality: now.quality ?? 'auto', qualities: now.qualities ?? [],
		source: now.source ?? null,
		sources: map(now.streams ?? [], s => s.name),
	};
	for (let t in st.media.tracks ?? []) {
		t.label = now.labels?.[t.name] ?? t.name;
		t.lang = replace(t.lang ?? '', /-x-.*$/, ''); // see unique_audio_langs
	}
	return st;
}

// ---- media proxy -----------------------------------------------------------
//
// /api/p/<name>?u=<url> fetches <url> with our User-Agent and extra headers.
// <name> is the upstream file name, kept so receivers can sniff the format by
// extension. Playlists are rewritten so every URI points back at the proxy;
// everything else is streamed through.

// Per-stream upstream headers travel in the proxy URL (&h=, "Name: value"
// lines) and are inherited by every URL a rewritten playlist points to.
function valid_headers(h) {
	let lines = split(h ?? '', '\n');
	return h != null && h != '' && length(lines) <= 8 &&
		length(filter(lines, l => length(l) > 512 ||
			!match(l, /^[A-Za-z0-9-]{1,40}: [ -~]+$/))) == 0;
}

function fetch_cmd(url, h) {
	let cmd = `uclient-fetch -q -T 20 -U ${shq(USER_AGENT)}`;
	for (let x in (type(HEADERS) == 'array' ? HEADERS : [HEADERS]))
		cmd += ' --header=' + shq(x);
	for (let x in (valid_headers(h) ? split(h, '\n') : []))
		cmd += ' --header=' + shq(x);
	return `${cmd} -O - ${shq(url)} 2>/dev/null`;
}

// Only public http(s) URLs and title-api itself: the proxy must not reach
// into the LAN.
function proxy_target(u) {
	if (substr(u ?? '', 0, length(API) + 1) == API + '/')
		return u;
	let m = match(u ?? '', /^https?:\/\/([^\/?#:@]+|\[[^\]]*\])(:[0-9]+)?([\/?#]|$)/);
	if (!m)
		return null;
	let host = lc(m[1]);
	if (match(host, /^(localhost|.*\.lan|.*\.local|\[.*\]|127\..*|10\..*|192\.168\..*|169\.254\..*|172\.(1[6-9]|2[0-9]|3[01])\..*|0\..*)$/) ||
	    index(host, '.') < 0)
		return null;
	return u;
}

function ext(name) {
	let m = match(name, /\.([A-Za-z0-9]+)$/);
	return m ? lc(m[1]) : '';
}

// Proxy path for an upstream URL; root-relative unless a base is given.
// The name defaults to the upstream file name.
function purl(u, base, name, h) {
	let m = match(u, /^[^?#]*\/([^\/?#]*)/);
	name = replace(name ?? m?.[1] ?? '', /[^A-Za-z0-9._-]/g, '') || 'file';
	return `${base ?? ''}/api/p/${name}?u=${urlenc(u)}` +
		(valid_headers(h) ? `&h=${urlenc(h)}` : '');
}

function resolve(base, ref) {
	if (match(ref, /^[A-Za-z][A-Za-z0-9+.-]*:/))
		return ref;
	let m = match(base, /^([a-z]+:)(\/\/[^\/?#]*)([^?#]*)/);
	if (substr(ref, 0, 2) == '//')
		return m[1] + ref;
	if (substr(ref, 0, 1) == '/')
		return m[1] + m[2] + ref;
	if (substr(ref, 0, 1) == '?')
		return m[1] + m[2] + m[3] + ref;
	return m[1] + m[2] + replace(m[3], /[^\/]*$/, '') + ref;
}

// Receivers switch audio by language, so of several dubs in one language
// only the first is reachable. Such renditions get a private-use subtag
// from their NAME (ru -> ru-x-rus1) to make every language distinct.
function unique_audio_langs(lines) {
	let count = {};
	for (let l in lines) {
		let m = match(l, /^#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="([^"]*)".*LANGUAGE="([^"]*)"/);
		if (m)
			count[m[1] + ' ' + m[2]] = (count[m[1] + ' ' + m[2]] ?? 0) + 1;
	}
	return map(lines, l => {
		let m = match(l, /^#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="([^"]*)".*LANGUAGE="([^"]*)"/);
		let name = match(l, /NAME="([^"]*)"/);
		if (!m || count[m[1] + ' ' + m[2]] < 2 || !name)
			return l;
		let tag = substr(replace(name[1], /[^A-Za-z0-9]/g, ''), 0, 8);
		return tag == '' ? l :
			replace(l, /LANGUAGE="[^"]*"/, `LANGUAGE="${m[2]}-x-${tag}"`);
	});
}

// ---- quality ---------------------------------------------------------------
//
// The receiver picks the video variant itself and, measuring the bandwidth
// through the proxy, often settles on the lowest one. A quality is chosen by
// trimming the master playlist to the variants of one width (&q=<width>).

const quality_label = w => w >= 3200 ? '2160p' : w >= 2400 ? '1440p' :
	w >= 1700 ? '1080p' : w >= 1200 ? '720p' : w >= 800 ? '480p' :
	w >= 560 ? '360p' : '240p';

// Chromecasts before Ultra and Google TV decode only H.264 and VP8, so
// AV1/VP9/HEVC variants (offered next to H.264 ones by some sources) can't
// be chosen.
const playable = codecs => !match(codecs ?? '', /(av01|vp09|vp9|hvc1|hev1|dvh1)/);

// [{ label, width }] of a master playlist, best first, one per label.
function qualities(master, h) {
	let body = run(fetch_cmd(master, h)).out, seen = {}, list = [];
	for (let inf in match(body, /#EXT-X-STREAM-INF:[^\n]*/g) ?? []) {
		let m = match(inf[0], /RESOLUTION=([0-9]+)x[0-9]+/);
		if (!m || !playable(match(inf[0], /CODECS="([^"]*)"/)?.[1]))
			continue;
		let w = +m[1], l = quality_label(w);
		if (!seen[l]) {
			seen[l] = true;
			push(list, { label: l, width: w });
		}
	}
	return sort(list, (a, b) => b.width - a.width);
}

// Whether a master's media playlists use fragmented MP4 (#EXT-X-MAP); the
// receiver assumes MPEG-TS segments unless told.
function is_fmp4(master, h) {
	let body = run(fetch_cmd(master, h)).out;
	let child = match(body, /\n([^#\n][^\n]*)/);
	return !!child && index(run(fetch_cmd(resolve(master, trim(child[1])), h)).out,
		'#EXT-X-MAP') >= 0;
}

// Drops the variants (#EXT-X-STREAM-INF and its URI line) that fail keep,
// unless that would leave none.
function keep_variants(lines, keep) {
	let inf = l => substr(l, 0, 18) == '#EXT-X-STREAM-INF:';
	if (!length(filter(lines, l => inf(l) && keep(l))))
		return lines;
	let out = [], skip = false;
	for (let l in lines) {
		if (inf(l)) {
			skip = !keep(l);
			if (!skip)
				push(out, l);
		} else if (skip && l != '' && substr(l, 0, 1) != '#') {
			skip = false;
		} else {
			push(out, l);
		}
	}
	return out;
}

function rewrite_playlist(body, base, q, h) {
	let fix = u => match(u, /^(data|skd):/) ? u : purl(resolve(base, u), null, null, h);
	let lines = map(split(body, '\n'), line => rtrim(line, '\r'));
	lines = keep_variants(lines, l => playable(match(l, /CODECS="([^"]*)"/)?.[1]));
	if (q)
		lines = keep_variants(lines, l => +(match(l, /RESOLUTION=([0-9]+)x/)?.[1] ?? 0) == q);
	return join('\n', map(unique_audio_langs(lines), l => {
		if (l == '')
			return l;
		if (substr(l, 0, 1) != '#')
			return fix(trim(l));
		return replace(l, /URI="([^"]*)"/g, (all, u) => `URI="${fix(u)}"`);
	}));
}

function media_head(code, mime) {
	printf('Status: %d %s\r\nContent-Type: %s\r\n' +
	       'Access-Control-Allow-Origin: *\r\nCache-Control: %s\r\n\r\n',
	       code, REASONS[`${code}`], mime,
	       substr(mime, 0, 6) == 'image/' ? 'max-age=86400' : 'no-store');
}

function proxy(name, u, q, h) {
	let url = proxy_target(u);
	if (!url)
		fail(400, 'bad url');
	if (h != null && !valid_headers(h))
		fail(400, 'bad headers');
	let e = ext(name);
	let pipe = popen(fetch_cmd(url, h));
	if (!pipe)
		fail(502, 'popen failed');

	if (e == 'm3u8' || e == 'm3u') {
		let body = pipe.read('all') ?? '';
		if (pipe.close() != 0 || substr(body, 0, 7) != '#EXTM3U')
			fail(502, 'upstream playlist fetch failed');
		media_head(200, MIME[e]);
		print(rewrite_playlist(body, url, match(q ?? '', /^[0-9]{1,5}$/) ? +q : null, h), '\n');
		exit(0);
	}

	// Hold back the headers until the first chunk so upstream failures
	// (e.g. 410 on a stale signature) become a 502 instead of an empty 200.
	let chunk = pipe.read(65536);
	if (chunk == null || chunk == '') {
		pipe.close();
		fail(502, 'upstream fetch failed');
	}
	media_head(200, MIME[e] ?? 'application/octet-stream');
	while (chunk != null && chunk != '') {
		stdout.write(chunk);
		chunk = pipe.read(65536);
	}
	pipe.close();
	exit(0);
}

// Absolute proxy base for URLs handed to the receiver.
function public_base() {
	return rtrim(conf('public_url', '') || `http://${getenv('HTTP_HOST')}`, '/');
}

// Poster images sit behind the provider's anti-bot page; title-api fetches
// them with its session.
function poster(u) {
	if (!u)
		return null;
	let via_api = `${API}/v1/posters?url=${urlenc(u)}`;
	return PROXY ? purl(via_api, null, 'poster.jpg') : via_api;
}

const routes = {
	'GET /search': p => {
		let q = trim(p.q ?? '');
		if (ulength(q) < 3)
			fail(400, 'query must be at least 3 characters');
		let r = api('/v1/search?query=' + urlenc(q));
		// Search cards don't tell movies from series; /title does.
		return {
			results: map(r.results ?? [], m => ({
				id: m.titleId, title: m.title, year: m.year,
				poster: poster(m.posterLink),
			})),
		};
	},

	'GET /streams': p => {
		let season = null, episode = null;
		if (p.season != null) {
			if (!match(p.season, /^[0-9]{1,4}$/) ||
			    !match(p.episode ?? '', /^[0-9A-Za-z._-]{1,16}$/))
				fail(400, 'invalid season or episode');
			season = +p.season;
			episode = p.episode;
		}
		return { streams: streams(title_id(p), season, episode) };
	},

	'GET /title': p => {
		let r = api(`/v1/titles/${title_id(p)}`);
		let series = r.mediaType == 'MEDIA_TYPE_SERIES';
		return {
			id: r.titleId, title: r.title, original: r.originalTitle || null,
			year: r.year || null, description: r.description || null,
			poster: poster(r.posterLink),
			type: series ? 'series' : 'movie',
			quality: r.quality || null,
			countries: r.countries ?? [],
			// The provider mixes site sections and years into genres.
			genres: filter(r.genres ?? [], g =>
				!match(g, /^([0-9]+|Фильмы|Сериалы|Премьеры)$/)),
			directors: r.directors ?? [],
			actors: r.actors ?? [],
			ratings: map(r.ratings ?? [], x => ({ source: x.source, value: x.value })),
			duration: +(r.durationSeconds ?? 0),
			seasons: !series ? null : map(r.seasons ?? [], s => ({
				season: s.seasonNumber,
				episodes: map(s.episodes ?? [], e => ({
					episode: e.episodeNumber, title: e.title,
					duration: +(e.durationSeconds ?? 0),
				})),
			})),
		};
	},

	'GET /devices': p => {
		let list = p.refresh == '1' ? discover() : cached(DEVICES_TTL) ?? discover();
		let def = CAST_HOST;
		let d = is_host(def) ?
			filter(list, d => def == d.host || def == host_of(d))[0] :
			pick(list, def);
		return { devices: list, default: d?.id ?? null };
	},

	'GET /status': p => {
		let dev = device(p);
		return with_now(castctl(dev, 'status'), dev);
	},

	// play also switches the quality or the source of what is on: with
	// reuse=1 it takes the streams from the last play of the same title if
	// they are still fresh (title-api keeps synthetic masters for 10 min),
	// and start/audio/subs restore the position and tracks.
	'POST /play': p => {
		let id = title_id(p), season = null, episode = null;
		if (p.season != null || p.episode != null) {
			if (!match(p.season ?? '', /^[0-9]{1,4}$/) ||
			    !match(p.episode ?? '', /^[0-9A-Za-z._-]{1,16}$/))
				fail(400, 'invalid season or episode');
			season = +p.season;
			episode = p.episode;
		}
		for (let k in ['audio', 'subs'])
			if (p[k] != null && !match(p[k], /^[0-9A-Za-z._-]{1,64}$/))
				fail(400, `invalid ${k}`);
		if (p.start != null && !match(p.start, /^[0-9]+(\.[0-9]+)?$/))
			fail(400, 'invalid start');
		if (p.quality != null && !match(p.quality, /^(auto|[0-9]{3,4}p)$/))
			fail(400, 'invalid quality');
		if (p.style != null && !valid_style(p.style))
			fail(400, 'invalid subtitle style');
		if (p.source != null && (length(p.source) > 100 || match(p.source, /[\x00-\x1f]/)))
			fail(400, 'invalid source');
		let dev = device(p);
		let last = json(readfile(state_file(dev)) ?? 'null');
		let list, fetched;
		if (p.reuse == '1' && last?.streams && last.id == id &&
		    last.season == season && last.episode == episode &&
		    time() - last.fetched < 480) {
			list = last.streams;
			fetched = last.fetched;
		} else {
			list = streams(id, season, episode);
			fetched = time();
		}
		// The source is picked by name, so a preference survives episodes
		// whose stream order differs; stream=<n> is the fallback.
		let n = 0;
		for (let i, x in list)
			if (p.source != null ? x.name == p.source : i == +(p.stream ?? 0))
				n = i;
		let s = list[n];
		if (!s || !s.url)
			fail(404, 'stream not found');
		let h = s.headers ?? '';
		if (s.variants == null) {
			s.variants = qualities(s.url, h);
			s.fmp4 = is_fmp4(s.url, h);
		}
		let quals = s.variants;
		// The best quality unless asked otherwise; a missing one falls back
		// to the best below it.
		let quality = p.quality ?? quals[0]?.label ?? 'auto', width = null;
		if (quality != 'auto') {
			let want = +replace(quality, 'p', '');
			let v = filter(quals, v => +replace(v.label, 'p', '') <= want)[0] ?? quals[-1];
			quality = v?.label ?? 'auto';
			width = v?.width;
		}
		let url = PROXY ? purl(s.url, public_base(), null, h) +
			(width ? `&q=${width}` : '') : s.url;
		// Subtitles are separate WebVTT files, not part of the HLS manifest,
		// so they are side-loaded; the receiver names them "sub<id>".
		let args = ['play', url], labels = {};
		if (+(p.start ?? 0) > 0)
			push(args, '-p', p.start);
		if (s.fmp4)
			push(args, '-f', 'fmp4');
		for (let t in s.subs) {
			if (!t.url)
				continue;
			push(args, '-s', PROXY ? purl(t.url, public_base(), null, h) : t.url,
			     t.lang || 'und', 'sub' + t.id);
			labels['sub' + t.id] = t.name;
		}
		for (let t in s.audio)
			labels[t.id] = t.name;
		if (p.style != null)
			push(args, '-y', p.style);
		let st = castctl(dev, ...args);
		for (let k in ['audio', 'subs'])
			if (p[k] != null)
				st = castctl_try(dev, k, p[k]) ?? st;
		writefile(state_file(dev), sprintf('%J', {
			id, stream: n, source: s.name, url, title: p.title ?? null, season,
			episode, labels, quality, qualities: map(quals, v => v.label),
			streams: list, fetched,
			// Only our own proxy paths, so the UI never loads arbitrary URLs.
			poster: length(p.poster ?? '') <= 4096 &&
				match(p.poster ?? '', /^\/api\/p\/[^ ]+$/) ? p.poster : null,
		}));
		return with_now(st, dev);
	},

	'POST /cmd': p => {
		let c = p.c ?? '', kind = COMMANDS[c], arg = p.arg ?? '';
		if (!exists(COMMANDS, c))
			fail(400, 'unknown command');
		let dev = device(p);
		if (kind == null)
			return with_now(castctl(dev, c), dev);
		if (kind == 'num' && !match(arg, /^[0-9]+(\.[0-9]+)?$/))
			fail(400, 'numeric arg required');
		if (kind == 'str' && (arg == '' || length(arg) > 128))
			fail(400, 'arg required');
		if (kind == 'style' && !valid_style(arg))
			fail(400, 'invalid subtitle style');
		return with_now(castctl(dev, c, arg), dev);
	},
};

// The uhttpd alias passes the full URL path (/api/x); direct CGI calls pass /x.
// The media proxy is called by the receiver, which can't send a token, so it
// is exempt from the token check; it only reaches public hosts.
let path = replace(rtrim(getenv('PATH_INFO') ?? '', '/'), /^\/api(\/|$)/, '/');
let pm = match(path, /^\/p\/([^\/]*)$/);
if (pm) {
	if (getenv('REQUEST_METHOD') != 'GET')
		fail(405, 'method not allowed');
	let qs = parse_form(getenv('QUERY_STRING'), {});
	proxy(pm[1], qs.u, qs.q, qs.h);
}

let p = params();
if (TOKEN != '' && (getenv('HTTP_X_TOKEN') ?? p.token) != TOKEN)
	fail(401, 'bad token');

let handler = routes[`${getenv('REQUEST_METHOD')} ${path}`];
if (!handler) {
	for (let m in ['GET', 'POST'])
		if (routes[`${m} ${path}`])
			fail(405, 'method not allowed');
	fail(404, 'not found');
}
reply(200, handler(p));
