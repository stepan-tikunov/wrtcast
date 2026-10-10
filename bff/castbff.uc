#!/usr/bin/ucode
// castbff — CGI backend for the Chromecast remote. Proxies the movie-api
// HTTP/JSON gateway, drives castctl and relays media (HLS playlists, segments,
// posters) so upstream headers and CORS are under our control.
// Configured in /etc/config/castbff.
'use strict';

import { popen, readfile, writefile, stdin, stdout } from 'fs';
import { cursor } from 'uci';

const STATE = '/tmp/castbff.json';
const COMMANDS = {
	pause: null, resume: null, stop: null, quit: null, mute: null, unmute: null,
	seek: 'num', ff: 'num', rew: 'num', volume: 'num', audio: 'str', subs: 'str',
};
const REASONS = {
	'200': 'OK', '400': 'Bad Request', '401': 'Unauthorized',
	'404': 'Not Found', '405': 'Method Not Allowed', '502': 'Bad Gateway',
};
const MIME = {
	m3u8: 'application/vnd.apple.mpegurl', m3u: 'application/vnd.apple.mpegurl',
	ts: 'video/mp2t', aac: 'audio/aac', mp4: 'video/mp4', m4s: 'video/iso.segment',
	m4a: 'audio/mp4', vtt: 'text/vtt', webvtt: 'text/vtt', key: 'application/octet-stream',
	jpg: 'image/jpeg', jpeg: 'image/jpeg', png: 'image/png', webp: 'image/webp',
};

const uci = cursor();
const conf = (k, d) => uci.get('castbff', 'main', k) ?? d;
const API = rtrim(conf('api_url', 'http://127.0.0.1:8080'), '/');
const CAST_HOST = conf('cast_host', '');
const TOKEN = conf('token', '');
const PROXY = conf('proxy', '1') == '1';
// Must match the User-Agent movie-api used to obtain the signed media URLs.
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
		fail(502, 'movie-api request failed');
	let j = json(r.out);
	if (type(j) != 'object')
		fail(502, 'movie-api returned invalid JSON');
	return j;
}

function castctl(...args) {
	let cmd = 'castctl -H ' + shq(CAST_HOST);
	for (let a in args)
		cmd += ' ' + shq(a);
	let r = run(cmd);
	if (r.code != 0)
		fail(502, r.out || 'castctl failed');
	let lines = split(r.out, '\n');
	let j = json(lines[-1]);
	if (type(j) != 'object')
		fail(502, 'castctl returned invalid JSON');
	return j;
}

// Character count of a UTF-8 string (continuation bytes are skipped).
function ulength(s) {
	let n = 0;
	for (let i = 0; i < length(s); i++)
		if ((ord(s, i) & 0xC0) != 0x80)
			n++;
	return n;
}

function movie_id(p) {
	if (!match(p.id ?? '', /^[0-9A-Za-z]{1,64}$/))
		fail(400, 'invalid id');
	return p.id;
}

// Streams of a movie, or of an episode when season is given.
function streams(id, season, episode) {
	let r = api(season == null ? `/v1/movies/${id}/streams` :
		`/v1/series/${id}/seasons/${season}/episodes/${urlenc(episode)}/streams`);
	return map(r.streams ?? [], (s, i) => ({
		index: i,
		url: s.masterPlaylistUrl,
		audio: map(s.audioTracks ?? [], a => ({
			id: a.id, name: a.name, lang: a.language, default: !!a.default,
		})),
		subs: map(s.subtitles ?? [], t => ({
			id: t.id, name: t.name, lang: t.language, forced: !!t.forced,
		})),
	}));
}

// Adds the movie started via /play and human-readable track labels: the
// receiver names tracks by the manifest NAME, which is the movie-api track id.
function with_now(st) {
	let now = json(readfile(STATE) ?? 'null');
	if (!now || !st.media || st.media.url != now.url)
		return st;
	st.now = {
		id: now.id, stream: now.stream, title: now.title, poster: now.poster,
		season: now.season, episode: now.episode,
	};
	for (let t in st.media.tracks ?? [])
		t.label = now.labels?.[t.name] ?? t.name;
	return st;
}

// ---- media proxy -----------------------------------------------------------
//
// /api/p/<name>?u=<url> fetches <url> with our User-Agent and extra headers.
// <name> is the upstream file name, kept so receivers can sniff the format by
// extension. Playlists are rewritten so every URI points back at the proxy;
// everything else is streamed through.

function fetch_cmd(url) {
	let cmd = `uclient-fetch -q -T 20 -U ${shq(USER_AGENT)}`;
	for (let h in (type(HEADERS) == 'array' ? HEADERS : [HEADERS]))
		cmd += ' --header=' + shq(h);
	return `${cmd} -O - ${shq(url)} 2>/dev/null`;
}

// Only public http(s) URLs and movie-api itself: the proxy must not reach
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
function purl(u, base, name) {
	let m = match(u, /^[^?#]*\/([^\/?#]*)/);
	name = replace(name ?? m?.[1] ?? '', /[^A-Za-z0-9._-]/g, '') || 'file';
	return `${base ?? ''}/api/p/${name}?u=${urlenc(u)}`;
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

function rewrite_playlist(body, base) {
	let fix = u => match(u, /^(data|skd):/) ? u : purl(resolve(base, u));
	return join('\n', map(split(body, '\n'), line => {
		let l = rtrim(line, '\r');
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

function proxy(name, u) {
	let url = proxy_target(u);
	if (!url)
		fail(400, 'bad url');
	let e = ext(name);
	let pipe = popen(fetch_cmd(url));
	if (!pipe)
		fail(502, 'popen failed');

	if (e == 'm3u8' || e == 'm3u') {
		let body = pipe.read('all') ?? '';
		if (pipe.close() != 0 || substr(body, 0, 7) != '#EXTM3U')
			fail(502, 'upstream playlist fetch failed');
		media_head(200, MIME[e]);
		print(rewrite_playlist(body, url), '\n');
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

// Poster images sit behind the provider's anti-bot page; movie-api fetches
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
		let r = api('/v1/movies:search?query=' + urlenc(q));
		return {
			results: map(r.results ?? [], m => ({
				id: m.movieId, title: m.movieTitle,
				year: m.movieYear,
				type: m.mediaType == 'MEDIA_TYPE_SERIES' ? 'series' : 'movie',
				poster: poster(m.moviePosterLink),
			})),
		};
	},

	'GET /streams': p => ({ streams: streams(movie_id(p)) }),

	'GET /series': p => {
		let r = api(`/v1/series/${movie_id(p)}`);
		return {
			seasons: map(r.seasons ?? [], s => ({
				season: s.seasonNumber,
				episodes: map(s.episodes ?? [], e => ({
					episode: e.episodeNumber, title: e.title,
					duration: +(e.durationSeconds ?? 0),
				})),
			})),
		};
	},

	'GET /status': p => with_now(castctl('status')),

	'POST /play': p => {
		let id = movie_id(p), season = null, episode = null;
		if (p.season != null || p.episode != null) {
			if (!match(p.season ?? '', /^[0-9]{1,4}$/) ||
			    !match(p.episode ?? '', /^[0-9A-Za-z._-]{1,16}$/))
				fail(400, 'invalid season or episode');
			season = +p.season;
			episode = p.episode;
		}
		let list = streams(id, season, episode);
		let n = +(p.stream ?? 0);
		let s = list[n];
		if (!s || !s.url)
			fail(404, 'stream not found');
		let url = PROXY ? purl(s.url, public_base()) : s.url;
		let st = castctl('play', url);
		let labels = {};
		for (let t in [...s.audio, ...s.subs])
			labels[t.id] = t.name;
		writefile(STATE, sprintf('%J', {
			id, stream: n, url, title: p.title ?? null, season, episode, labels,
			// Only our own proxy paths, so the UI never loads arbitrary URLs.
			poster: length(p.poster ?? '') <= 4096 &&
				match(p.poster ?? '', /^\/api\/p\/[^ ]+$/) ? p.poster : null,
		}));
		return with_now(st);
	},

	'POST /cmd': p => {
		let c = p.c ?? '', kind = COMMANDS[c], arg = p.arg ?? '';
		if (!exists(COMMANDS, c))
			fail(400, 'unknown command');
		if (kind == null)
			return with_now(castctl(c));
		if (kind == 'num' && !match(arg, /^[0-9]+(\.[0-9]+)?$/))
			fail(400, 'numeric arg required');
		if (kind == 'str' && (arg == '' || length(arg) > 128))
			fail(400, 'arg required');
		return with_now(castctl(c, arg));
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
	proxy(pm[1], parse_form(getenv('QUERY_STRING'), {}).u);
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
