#!/usr/bin/ucode
// castbff — CGI backend for the Chromecast remote. Proxies the movie-api
// HTTP/JSON gateway and drives castctl. Configured in /etc/config/castbff.
'use strict';

import { popen, readfile, writefile, stdin } from 'fs';
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

const uci = cursor();
const conf = (k, d) => uci.get('castbff', 'main', k) ?? d;
const API = rtrim(conf('api_url', 'http://127.0.0.1:8080'), '/');
const CAST_HOST = conf('cast_host', '');
const TOKEN = conf('token', '');

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

function streams(id) {
	let r = api(`/v1/movies/${id}/streams`);
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
	st.now = { id: now.id, stream: now.stream, title: now.title };
	for (let t in st.media.tracks ?? [])
		t.label = now.labels?.[t.name] ?? t.name;
	return st;
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
				year: m.movieYear, poster: m.moviePosterLink,
			})),
		};
	},

	'GET /streams': p => ({ streams: streams(movie_id(p)) }),

	'GET /status': p => with_now(castctl('status')),

	'POST /play': p => {
		let id = movie_id(p);
		let list = streams(id);
		let n = +(p.stream ?? 0);
		let s = list[n];
		if (!s || !s.url)
			fail(404, 'stream not found');
		let st = castctl('play', s.url);
		let labels = {};
		for (let t in [...s.audio, ...s.subs])
			labels[t.id] = t.name;
		writefile(STATE, sprintf('%J', {
			id, stream: n, url: s.url, title: p.title ?? null, labels,
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

let p = params();
if (TOKEN != '' && (getenv('HTTP_X_TOKEN') ?? p.token) != TOKEN)
	fail(401, 'bad token');

// The uhttpd alias passes the full URL path (/api/x); direct CGI calls pass /x.
let path = replace(rtrim(getenv('PATH_INFO') ?? '', '/'), /^\/api(\/|$)/, '/');
let handler = routes[`${getenv('REQUEST_METHOD')} ${path}`];
if (!handler) {
	for (let m in ['GET', 'POST'])
		if (routes[`${m} ${path}`])
			fail(405, 'method not allowed');
	fail(404, 'not found');
}
reply(200, handler(p));
