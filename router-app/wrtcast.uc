#!/usr/bin/ucode
// wrtcast — CGI backend for the Chromecast remote. Proxies the title-api
// HTTP/JSON gateway, drives castctl and relays media (HLS playlists, segments,
// posters) so upstream headers and CORS are under our control. Chromecasts
// are found with castctl discover; requests pick one with device=<id>.
// Configured in /etc/config/wrtcast.
'use strict';

import { popen, open, readfile, writefile, rename, unlink, lsdir, mkdir, stat, access, stdin, stdout } from 'fs';
import { cursor } from 'uci';

const STATE = '/tmp/wrtcast.now.';         // + host key; what /play started
const SUBS = '/tmp/wrtcast.subs.';         // + host key; subtitle URLs for /sub
const LIST = '/tmp/wrtcast.list.';         // + host key; streams last asked for
const WATCH = '/tmp/wrtcast.watch.';       // + host key; failed tick checks
const SEEN = '/tmp/wrtcast.seen.';         // + host key; when what /play started last played
const NEXT = '/tmp/wrtcast.next.';         // + host key; lock while the next episode starts
const PROGRESS = '/tmp/wrtcast.progress.json'; // watch progress, copied to flash
const MAX_TITLES = 200;                      // progress kept for this many titles
const MAX_EPISODES = 50;                     // and this many episodes of each
const FLUSH = 300;                           // seconds between flash writes
const DONE = 0.85;                           // an episode left this far in is watched
const TTL = 30 * 86400;                      // progress older than this is forgotten
const DEVICES = '/tmp/wrtcast.devices.json'; // last discovery result
const DEVICES_TTL = 60;                      // seconds before /devices rescans
const DRC = '/tmp/wrtcast.drc/';             // audio playlists and segments of the leveler
const ERR = '/tmp/wrtcast.err.';           // + ns; stderr of a proxied fetch
const HDR = '/tmp/wrtcast.hdr.';           // + media id; its CDN headers from title-api
const BYTES = '/tmp/wrtcast.bytes.';       // + media id; "<bytes> <ms>" of each segment streamed
const POS = '/tmp/wrtcast.pos.';           // + media id; position and pause the TV last told
const COMMANDS = {
	pause: null, resume: null, stop: null, quit: null, mute: null, unmute: null,
	seek: 'num', ff: 'num', rew: 'num', volume: 'num', audio: 'str', subs: 'str',
	substyle: 'style',
};
const REASONS = {
	'200': 'OK', '400': 'Bad Request', '401': 'Unauthorized', '403': 'Forbidden',
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
const PROGRESS_KEEP = conf('progress_file', '/etc/wrtcast.progress.json');
// A minimal static ffmpeg (scripts/build-ffmpeg.sh); without it the volume
// leveler is not offered.
const FFMPEG = conf('ffmpeg', '/usr/bin/wrtcast-ffmpeg');

function reply(code, body) {
	printf('Status: %d %s\r\nContent-Type: application/json\r\n' +
	       'Cache-Control: no-store\r\n\r\n%J\n', code, REASONS[`${code}`], body);
	exit(0);
}

// Run before a failure reply: /play sets it while the state it wrote ahead
// of castctl must be put back if the play fails.
let undo = null;

function fail(code, msg) {
	if (undo)
		undo();
	reply(code, { error: msg });
}

// A state file as JSON, or null when it is missing or cut short (a full /tmp
// truncates writes, and a parse error would otherwise abort the request).
function load(f) {
	try {
		return json(readfile(f) ?? 'null');
	} catch (e) {
		return null;
	}
}

// Writes JSON through a temporary file so readers never see it cut short.
function save(f, v) {
	let s = sprintf('%J', v), tmp = `${f}.${clock()[1]}`;
	if (writefile(tmp, s) != length(s) || !rename(tmp, f))
		unlink(tmp);
}

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
	// uclient-fetch drops the body of an error reply and only prints
	// "HTTP error N" (not with -q): keep that line of stderr, the body goes
	// to stdout through fd 3.
	let cmd = `{ uclient-fetch -T 20 -O - ${shq(API + path)} 2>&1 >&3 | grep '^HTTP error'; } 3>&1`;
	let fetch = () => {
		let r = run(cmd);
		let m = match(r.out, /^HTTP error ([0-9]+)$/);
		return { status: m ? +m[1] : r.out == '' ? 0 : 200, out: r.out };
	};
	let r = fetch();
	// The provider's players fail now and then; one retry usually helps.
	// A 4xx won't change on retry.
	if (r.status != 200 && (r.status < 400 || r.status >= 500))
		r = fetch();
	if (r.status == 403)
		fail(403, 'Источник показывает этот тайтл только зарегистрированным пользователям');
	if (r.status != 200)
		fail(502, 'title-api request failed' + (r.status ? ` (HTTP ${r.status})` : ''));
	let j = json(r.out);
	if (type(j) != 'object')
		fail(502, 'title-api returned invalid JSON');
	return j;
}

// One title-api request that may fail: the object it answered, or null.
function api_try(path) {
	let r = run(`uclient-fetch -q -T 5 -O - ${shq(API + path)}`);
	try {
		let j = r.code == 0 ? json(r.out) : null;
		return type(j) == 'object' ? j : null;
	} catch (e) {
		return null;
	}
}

// ---- title-api media headers -----------------------------------------------
//
// The alternative player's CDN wants headers that change during a playback:
// title-api holds the player's playback socket, whose edge_hash replaces the
// device fingerprint. So the proxy URLs of a stream that is a title-api
// master carry its media id (&m=) instead of the headers (&h=), and fetches
// take the headers from HDR<id>, kept for the ttl title-api gives. A
// title-api that names a relay of the media's CDN requests (proxy) gets
// them instead: it fetches them as the player's page would, so the CDN sees
// one browser for the whole playback.

// Per-stream upstream headers travel in the proxy URL (&h=, "Name: value"
// lines) and are inherited by every URL a rewritten playlist points to.
function valid_headers(h) {
	let lines = split(h ?? '', '\n');
	return h != null && h != '' && length(lines) <= 8 &&
		length(filter(lines, l => length(l) > 512 ||
			!match(l, /^[A-Za-z0-9-]{1,40}: [ -~]+$/))) == 0;
}

// The title-api media id of a stream URL (its synthetic master), or null.
function media_id(u) {
	return match(u ?? '', /^https?:\/\/[^\/?#]+\/v1\/media\/([0-9a-f]{32})\/master\.m3u8$/)?.[1];
}

function valid_media(m) {
	return type(m) == 'string' && !!match(m, /^[0-9a-f]{32}$/);
}

// "720" of the quality label "720p"; null for auto.
function quality_digits(l) {
	return match(l ?? '', /^([0-9]{3,4})p$/)?.[1];
}

// The headers of media m as "Name: value" lines; null without any. Stale
// ones (or with force) are asked for again, which tells title-api the
// playback goes on (for the alternative player's telemetry too), with q,
// the quality's digits, t, the position (else the TV's last one, run on
// while playing), p, the pause, and d, the duration, as the TV last told
// (see watch_end), w, whether it waits for data (BUFFERING), b, the bytes
// of the media's segments streamed so far, and e, the bandwidth of the
// last few (see count_bytes), when known. The relay title-api names, if
// any, is kept with the headers (see fetch_cmd). One
// request refreshes them while the others go on with what there is for a
// few seconds (a few may still refresh at once); a failed refresh keeps
// the stale ones too.
function media_headers(m, q, t, force) {
	let f = HDR + m, c = load(f), now = time();
	let claimed = now - (c?.claimed ?? 0) < 10;
	if (type(c?.headers) != 'array')
		c = null;
	if (!force && (c && now - c.fetched < c.ttl || claimed))
		return c?.headers;
	if (c)
		c.claimed = now;
	save(f, c ?? { claimed: now });
	let qs = [], pos = load(POS + m), b = 0, n = 0, rb = 0, rms = 0;
	if (now - (pos?.at ?? 0) > 180)
		pos = null;
	if (t == null && pos)
		t = pos.pos + (pos.paused ? 0 : now - pos.at);
	let lines = split(readfile(BYTES + m) ?? '', '\n');
	for (let i, l in lines) {
		let x = match(l, /^([0-9]+) ([0-9]+)$/);
		if (!x)
			continue;
		b += +x[1];
		n++;
		if (i >= length(lines) - 6) {
			rb += +x[1];
			rms += +x[2];
		}
	}
	if (q != null)
		push(qs, `q=${q}`);
	if (type(t) in ['int', 'double'] && t >= 0)
		push(qs, sprintf('t=%d', t));
	if (pos)
		push(qs, `p=${pos.paused ? 1 : 0}`, `w=${pos.buffering ? 1 : 0}`);
	if (+(pos?.dur ?? 0) > 0)
		push(qs, sprintf('d=%.2f', pos.dur));
	if (n)
		push(qs, sprintf('b=%d', b));
	if (rms > 0)
		push(qs, sprintf('e=%d', rb * 8000 / rms));
	let r = api_try(`/v1/media/${m}/headers` + (length(qs) ? '?' + join('&', qs) : ''));
	let hl = map(filter(r?.headers ?? [], x => type(x?.name) == 'string' &&
		type(x?.value) == 'string'), x => `${x.name}: ${x.value}`);
	if (type(r?.headers) == 'array' && (!length(hl) || valid_headers(join('\n', hl)))) {
		c = { headers: hl, fetched: time(), ttl: +r.ttl > 0 ? +r.ttl : 60,
			proxy: match(r.proxy ?? '', /^\/v1\/media\/[0-9a-f]{32}\/cdn$/) ? r.proxy : null };
		save(f, c);
	}
	return c?.headers;
}

// Tells title-api the playback of media m ended, so its socket says so and
// closes. In the background: it takes title-api a moment, and a failure
// changes nothing. uclient-fetch can't send DELETE, hence the POST.
function media_end(m) {
	if (!valid_media(m))
		return;
	unlink(HDR + m);
	unlink(BYTES + m);
	unlink(POS + m);
	system(`uclient-fetch -q -T 5 -O /dev/null --post-data= ` +
		`${shq(API + '/v1/media/' + m + '/session/end')} </dev/null >/dev/null 2>&1 &`);
}

// ---- devices ---------------------------------------------------------------

// Cached device list no older than max_age seconds (any age when null).
function cached(max_age) {
	let c = load(DEVICES);
	if (type(c) != 'object' || type(c.devices) != 'array' ||
	    (max_age != null && time() - c.time >= max_age))
		return null;
	return c.devices;
}

function discover() {
	let r = run('castctl discover -t 1.5');
	let list = r.code == 0 ? json(r.out) : null;
	if (type(list) != 'array')
		fail(502, r.out || 'discovery failed');
	list = map(list, d => ({
		id: d.id, name: d.name, model: d.model, host: d.host, port: d.port,
	}));
	// A TV in standby may miss the scan; an empty one keeps the last devices
	// for lookups, but without a fresh time, so /devices scans again.
	if (length(list) || !length(cached() ?? []))
		save(DEVICES, { time: time(), devices: list });
	return list;
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
		// A bare address means castctl's default port, so a TV keeps one
		// state key whether or not the last discovery saw it (a speaker
		// group on the same address has a port of its own).
		if (index(want, ':') < 0)
			want += ':8009';
		for (let d in cached() ?? [])
			if (want == d.host || want == `${d.host}:${d.port}`)
				return d;
		let m = match(want, /^([^:]+):([0-9]+)$/);
		return m ? { host: m[1], port: +m[2] } : { host: want };
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

const state_key = dev => replace(host_of(dev), /[^0-9A-Za-z.]/g, '_');
const state_file = dev => STATE + state_key(dev);

// Character count of a UTF-8 string (continuation bytes are skipped).
function ulength(s) {
	let n = 0;
	for (let i = 0; i < length(s); i++)
		if ((ord(s, i) & 0xC0) != 0x80)
			n++;
	return n;
}

// castctl drops the tracks that don't fit the LOAD message from the end, so
// Russian, English and Ukrainian ones go first.
function pick_subs(subs) {
	let rank = t => {
		let l = substr(lc(t.lang ?? ''), 0, 2);
		return l == 'ru' ? 0 : l == 'en' ? 1 : l == 'uk' ? 2 : 3;
	};
	let list = [];
	for (let i, t in subs ?? [])
		if (t.url)
			push(list, { t, i });
	list = sort(list, (a, b) => rank(a.t) - rank(b.t) || a.i - b.i);
	return map(list, x => x.t);
}

// castctl subtitle style: "size=1.3,color=#FFFF00,bg=#00000099,edge=outline".
// Each key once and a short string: castctl builds it into a small buffer.
function valid_style(s) {
	if (length(s) > 100 ||
	    !match(s, /^[a-z]+=[#.0-9A-Za-z]+(,[a-z]+=[#.0-9A-Za-z]+)*$/))
		return false;
	let seen = {};
	for (let kv in split(s, ',')) {
		let k = split(kv, '=')[0];
		if (seen[k])
			return false;
		seen[k] = true;
	}
	return true;
}

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

// The streams of a title, remembered per TV for 10 minutes: the UI lists the
// sources before playing and may retry with another, all from one provider
// request. force asks the provider again.
function title_streams(dev, id, season, episode, force) {
	let f = LIST + state_key(dev), c = load(f);
	if (!force && c?.streams && c.id == id && c.season == season &&
	    c.episode == episode && time() - c.fetched < 600)
		return c;
	c = { id, season, episode, streams: streams(id, season, episode), fetched: time() };
	writefile(f, sprintf('%J', c));
	return c;
}

// ---- watch progress --------------------------------------------------------
//
// Where each title was left off: a movie's position, or a series' last
// episode and the position in each of its episodes. Kept in /tmp and copied
// to flash every few minutes, or right away when a title or an episode
// starts or gets watched. Beyond MAX_TITLES and MAX_EPISODES the least
// recently watched ones are dropped, and anything untouched for TTL is
// forgotten, so a title opened a month later starts over.

function progress() {
	let p = load(PROGRESS) ?? load(PROGRESS_KEEP);
	if (type(p?.titles) != 'object')
		return { titles: {} };
	let old = time() - TTL;
	for (let id, e in p.titles) {
		if (e.t < old) {
			delete p.titles[id];
			continue;
		}
		for (let k, x in e.eps ?? {})
			if (x.t < old)
				delete e.eps[k];
	}
	return p;
}

// Keeps the n most recently watched entries of o.
function trim_lru(o, n) {
	let k = sort(keys(o), (a, b) => o[b].t - o[a].t);
	for (let i = n; i < length(k); i++)
		delete o[k[i]];
}

const ep_key = (season, episode) => `${season}:${episode}`;

// Copies progress to flash when forced or FLUSH seconds after the last copy.
function flush(P, force) {
	let due = force || time() - (P.flushed ?? 0) >= FLUSH;
	if (due)
		P.flushed = time();
	save(PROGRESS, P);
	if (due)
		save(PROGRESS_KEEP, P);
}

// Records the position of now (what /play started) from the receiver's
// media status. Positions under a second are skipped: the receiver reports
// 0 while it loads a stream started further in.
function remember(now, m) {
	let finished = m.state == 'IDLE' && m.idleReason == 'FINISHED';
	let pos = +(m.time ?? 0), dur = +(m.duration ?? 0);
	if (!finished && (pos < 1 || index(['PLAYING', 'PAUSED', 'BUFFERING'], m.state) < 0))
		return;
	let P = progress(), e = P.titles[now.id], t = time();
	let important = !e;
	e ??= {};
	let x = e;
	if (now.season != null) {
		let k = ep_key(now.season, now.episode);
		important ||= e.last?.season != now.season || e.last?.episode != now.episode;
		e.last = { season: now.season, episode: now.episode };
		e.eps ??= {};
		important ||= !e.eps[k];
		x = e.eps[k] ??= {};
		x.t = t;
		trim_lru(e.eps, MAX_EPISODES);
	}
	// Only an end the receiver reached itself marks a title watched here
	// (see next_episode). A watched episode stays so while it is still near
	// the end: a poll can catch it right after the next one was started.
	let done = finished || (!!x.done && dur > 0 && pos >= dur * DONE);
	important ||= !!x.done != done;
	if (finished)
		pos = dur;
	// A long pause shouldn't keep rewriting the same position.
	if (!important && x.pos == pos && x.dur == dur)
		return;
	x.pos = pos;
	x.dur = dur;
	x.done = done;
	// For the recently watched list.
	e.title = now.title ?? e.title;
	e.poster = now.poster ?? e.poster;
	e.t = t;
	P.titles[now.id] = e;
	trim_lru(P.titles, MAX_TITLES);
	P.changed = t;
	flush(P, important);
}

// Starting another episode of a series marks the last one watched if it was
// left near the end: people switch off during the credits, which may run
// for minutes. Movies just resume wherever they were stopped.
function next_episode(id, season, episode) {
	let P = progress(), e = P.titles[id], l = e?.last;
	let x = l ? e.eps?.[ep_key(l.season, l.episode)] : null;
	if (!x || x.done || ep_key(l.season, l.episode) == ep_key(season, episode) ||
	    !(x.dur > 0 && x.pos >= x.dur * DONE))
		return;
	x.done = true;
	flush(P, true);
}

// Whether the episode now has played to the end and the next one is due
// (see play_next). The receiver drops the media session at the end, so the
// end is told by the episode going away within its last minute and a half
// about when it would have run out, while playing, not stopped from the
// page (which forgets seen) or with another reason, and with the media
// receiver still up (casting not ended).
function ended(now, st, seen) {
	let m = st.media, left = seen?.dur - seen?.pos, ago = time() - (seen?.t ?? 0);
	if (!now.autonext || now.season == null || now.next_at ||
	    st.app?.id != 'CC1AD845')
		return false;
	if (m && ((m.url ?? '') != '' && m.url != now.url ||
	          m.state != 'IDLE' || m.idleReason && m.idleReason != 'FINISHED'))
		return false;
	return !!seen?.playing && seen.url == now.url && seen.dur > 0 &&
		left <= 90 && ago < 600 && (m?.idleReason == 'FINISHED' || ago >= left - 10);
}

// Records when now was last seen on the TV and, once it has ended, starts
// the next episode. Another process does that, so the status request or
// the tick neither waits for title-api and the TV nor fails with them; a
// lock keeps polls and ticks from starting it twice.
function watch_end(now, st) {
	let key = state_key({ host: now.host }), m = st.media;
	if (m?.url == now.url && index(['PLAYING', 'BUFFERING', 'PAUSED'], m.state) >= 0) {
		let active = type => now.labels?.[filter(m.tracks ?? [], t => t.type == type && t.active)[0]?.name];
		save(SEEN + key, {
			url: now.url, pos: +(m.time ?? 0), dur: +(m.duration ?? 0),
			playing: m.state != 'PAUSED', t: time(),
			audio: active('AUDIO'), subs: active('TEXT'),
		});
		// For the next headers refresh of a title-api media (see
		// media_headers), with no castctl call of its own.
		if (valid_media(now.media))
			save(POS + now.media, {
				pos: +(m.time ?? 0), dur: +(m.duration ?? 0), paused: m.state == 'PAUSED',
				buffering: m.state == 'BUFFERING', at: time(),
			});
		return;
	}
	if (!ended(now, st, load(SEEN + key)))
		return;
	let lock = NEXT + key;
	system(`(mkdir ${shq(lock)} && { ${shq(sourcepath())} next ${shq(key)}; ` +
	       `rmdir ${shq(lock)}; }) </dev/null >/dev/null 2>&1 &`);
}

// The position to resume a movie or an episode from; null to start over.
function resume_at(id, season, episode) {
	let e = progress().titles[id];
	let x = season == null ? e : e?.eps?.[ep_key(season, episode)];
	return x && !x.done && x.pos > 10 ? x.pos - 5 : null;
}

// Run by cron every minute: records the progress of what /play started
// while no page polls the status, e.g. with the phone locked. A TV that
// fails five checks in a row (switched off, or playing something else) is
// left alone until the next /play.
function tick() {
	for (let name in lsdir('/tmp') ?? []) {
		let k = match(name, /^wrtcast\.now\.(.+)$/)?.[1];
		let now = k ? load(STATE + k) : null;
		// The key must be the host's own: save() leaves "<key>.<ns>" temp
		// files for a moment, and host keys have dots too.
		if (!now?.host || !now.url || state_key({ host: now.host }) != k)
			continue;
		let w = load(WATCH + k);
		let misses = w?.url == now.url ? w.misses : 0;
		if (misses >= 5)
			continue;
		let st = castctl_try({ host: now.host }, 'status'), m = st?.media;
		if (st)
			watch_end(now, st);
		if (m?.url == now.url)
			remember(now, m);
		misses = m?.url == now.url && m.state != 'IDLE' ? 0 : misses + 1;
		if (misses != w?.misses || w?.url != now.url)
			save(WATCH + k, { url: now.url, misses });
		// title-api keeps the playback socket open while the headers are
		// asked for, also while the receiver has enough buffered to fetch
		// nothing, and learns the position.
		if (now.media && misses == 0)
			media_headers(now.media, quality_digits(now.quality), m.time, true);
		// Playback ended (see above).
		if (misses == 5)
			media_end(now.media);
	}
	// Error files of proxied fetches the web server cut short, and the
	// headers, bytes and positions of media not played for a day.
	for (let name in lsdir('/tmp') ?? []) {
		let age = match(name, /^wrtcast\.err\.[0-9]+$/) ? 60 :
			match(name, /^wrtcast\.(hdr|bytes|pos)\./) ? 86400 : null;
		let st = age ? stat('/tmp/' + name) : null;
		if (st && st.mtime < time() - age)
			unlink('/tmp/' + name);
	}
	// The last position before playback stopped would otherwise stay in
	// /tmp only.
	let P = load(PROGRESS);
	if (P?.changed > (P?.flushed ?? 0) && time() - P.flushed >= FLUSH)
		flush(P);
}

// ---- media proxy -----------------------------------------------------------
//
// /api/p/<name>?u=<url> fetches <url> with our User-Agent and extra headers.
// <name> is the upstream file name, kept so receivers can sniff the format by
// extension. Playlists are rewritten so every URI points back at the proxy;
// everything else is streamed through.

// With err, stderr goes to that file and keeps the "HTTP error N" line of
// a refused fetch, which -q leaves out. With m, the headers of that
// title-api media (see media_headers) replace the same-named ones of h,
// which stand in for them only while there are none, or, once title-api
// names its relay, the request goes to that, without headers of ours;
// title-api's own URLs (the master, its quality playlists) need none.
function fetch_cmd(url, h, err, m) {
	let out = u => `-O - ${shq(u)} 2>${err ? shq(err) : '/dev/null'}`;
	let cmd = `uclient-fetch ${err ? '' : '-q '}-T 20`;
	let lines = valid_headers(h) ? split(h, '\n') : [];
	if (valid_media(m) && substr(url, 0, length(API) + 1) != API + '/') {
		let dyn = media_headers(m), relay = load(HDR + m)?.proxy;
		if (relay)
			return `${cmd} ${out(API + relay + '?u=' + urlenc(url))}`;
		if (dyn == null && !length(lines)) {
			if (err)
				unlink(err);
			fail(502, 'no CDN headers for the media');
		}
		let name = l => lc(split(l, ':')[0]), own = {};
		for (let l in dyn ?? [])
			own[name(l)] = true;
		lines = [...filter(lines, l => !own[name(l)]), ...(dyn ?? [])];
	}
	cmd += ` -U ${shq(USER_AGENT)}`;
	for (let x in (type(HEADERS) == 'array' ? HEADERS : [HEADERS]))
		cmd += ' --header=' + shq(x);
	for (let x in lines)
		cmd += ' --header=' + shq(x);
	return `${cmd} ${out(url)}`;
}

// Answers a failed proxied fetch: a 403 stays a 403, which receivers don't
// retry (a CDN that blocks the client would only be asked again and
// again), anything else is a 502.
function fetch_fail(err, msg) {
	let m = match(readfile(err) ?? '', /HTTP error ([0-9]+)/);
	unlink(err);
	if (m && +m[1] == 403)
		fail(403, 'upstream refused (HTTP 403)');
	fail(502, msg + (m ? ` (HTTP ${m[1]})` : ''));
}

// A playlist or another small text file; null when the fetch fails.
function fetch_text(url, h, m) {
	let r = run(fetch_cmd(url, h, null, m));
	return r.code == 0 ? r.out : null;
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
// The name defaults to the upstream file name. A title-api media id (m)
// stands for the headers.
function purl(u, base, name, h, m) {
	let f = match(u, /^[^?#]*\/([^\/?#]*)/);
	name = replace(name ?? f?.[1] ?? '', /[^A-Za-z0-9._-]/g, '') || 'file';
	return `${base ?? ''}/api/p/${name}?u=${urlenc(u)}` +
		(valid_media(m) ? `&m=${m}` : valid_headers(h) ? `&h=${urlenc(h)}` : '');
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

// [{ label, width }] of a master playlist body, best first, one per label.
// null when the master itself couldn't be fetched.
function qualities(body) {
	let seen = {}, list = [];
	if (body == null)
		return null;
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

// Adds the device, the movie started via /play and human-readable track
// labels: the receiver names tracks by the manifest NAME, which is the
// title-api track id.
function with_now(st, dev) {
	st.device = { id: dev.id ?? null, name: dev.name ?? host_of(dev) };
	let now = load(state_file(dev));
	if (now)
		watch_end(now, st);
	if (!now || !st.media || st.media.url != now.url)
		return st;
	// Started before qualities were recorded: look them up once from the
	// master playlist the receiver is playing (it plays every variant).
	if (now.qualities == null && PROXY) {
		let u = match(now.url, /[?&]u=([^&]*)/);
		now.src = u ? urldec(u[1]) : null;
		let v = (now.src && proxy_target(now.src) ? qualities(fetch_text(now.src)) : null) ?? [];
		now.variants = v;
		now.qualities = map(v, x => x.label);
		now.quality = 'auto';
		save(state_file(dev), now);
	}
	st.now = {
		id: now.id, stream: now.stream, title: now.title, poster: now.poster,
		season: now.season, episode: now.episode,
		quality: now.quality ?? 'auto', qualities: now.qualities ?? [],
		source: now.source ?? null,
		sources: map(now.streams ?? [], s => s.name),
		// Whether the volume leveler is on, and whether the source allows it.
		drc: !!now.drc, drc_ok: !!now.drc_ok,
		// Whether the router starts the next episode once this one ends.
		autonext: !!now.autonext,
	};
	for (let t in st.media.tracks ?? []) {
		t.label = now.labels?.[t.name] ?? t.name;
		t.lang = replace(t.lang ?? '', /-x-.*$/, ''); // see unique_audio_langs
	}
	remember(now, st.media);
	return st;
}

// Whether a master's media playlists use fragmented MP4 (#EXT-X-MAP); the
// receiver assumes MPEG-TS segments unless told. Of the variants it asks
// for the one that will play: the first playable one of the chosen width,
// else the first playable one, so no other playlist is fetched.
function is_fmp4(master, body, h, width, m) {
	let inf = null, first = null, uri = null;
	for (let l in split(body ?? '', '\n')) {
		l = trim(l);
		if (substr(l, 0, 18) == '#EXT-X-STREAM-INF:') {
			inf = l;
		} else if (inf != null && l != '' && substr(l, 0, 1) != '#') {
			if (playable(match(inf, /CODECS="([^"]*)"/)?.[1])) {
				first ??= l;
				if (width && +(match(inf, /RESOLUTION=([0-9]+)x/)?.[1] ?? 0) == width) {
					uri = l;
					break;
				}
			}
			inf = null;
		}
	}
	uri ??= first;
	// A media playlist rather than a master tells by itself.
	if (uri == null)
		return index(body ?? '', '#EXT-X-MAP') >= 0;
	return index(fetch_text(resolve(master, uri), h, m) ?? '', '#EXT-X-MAP') >= 0;
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

// With drc the audio renditions of a master are asked for leveled (see
// drc_playlist).
function rewrite_playlist(body, base, q, h, drc, m) {
	let fix = u => match(u, /^(data|skd):/) ? u : purl(resolve(base, u), null, null, h, m);
	let lines = map(split(body, '\n'), line => rtrim(line, '\r'));
	lines = keep_variants(lines, l => playable(match(l, /CODECS="([^"]*)"/)?.[1]));
	if (q)
		lines = keep_variants(lines, l => +(match(l, /RESOLUTION=([0-9]+)x/)?.[1] ?? 0) == q);
	return join('\n', map(unique_audio_langs(lines), l => {
		if (l == '')
			return l;
		if (substr(l, 0, 1) != '#')
			return fix(trim(l));
		let flag = drc && match(l, /^#EXT-X-MEDIA:(.*,)?TYPE=AUDIO(,|$)/) ? '&drc=1' : '';
		return replace(l, /URI="([^"]*)"/g, (all, u) => `URI="${fix(u)}${flag}"`);
	}));
}

function media_head(code, mime) {
	printf('Status: %d %s\r\nContent-Type: %s\r\n' +
	       'Access-Control-Allow-Origin: *\r\nCache-Control: %s\r\n\r\n',
	       code, REASONS[`${code}`], mime,
	       substr(mime, 0, 6) == 'image/' ? 'max-age=86400' : 'no-store');
}

// ---- volume leveler --------------------------------------------------------
//
// Films are mixed for cinemas: loud music, quiet speech. With the leveler on,
// the audio segments are run through dynaudnorm, which evens loudness out
// over a few seconds, and re-encoded. Only AAC in audio renditions of their
// own on one MPEG-TS timeline qualifies; the video is relayed untouched.
//
// Each segment is leveled on its own, so it is decoded together with the end
// of the previous one (the leveler's history) and the start of the next one
// (its look-ahead, and the encoder's: without it every segment would fade at
// the end). The output keeps the timestamps and frame count of the original,
// which takes trimming the input to whole AAC frames before the segment
// start and dropping the packets outside it. The raw segments are cached:
// leveling N fetches N+1, which N+1 then needs itself, so the CDN sees each
// segment once, plus the previous one after a seek.

const DRC_FILTER = 'dynaudnorm=f=250:g=13:p=0.9:m=30:r=0.06:s=0,' +
	'alimiter=limit=0.7:attack=5:release=100';
const DRC_PRE = 5;   // seconds of history, well over the leveler's window
const DRC_POST = 3;  // seconds of look-ahead, over half the window (250 ms × 13)

// Whether the audio of a master (url and body) can be leveled.
function drc_supported(master, body, h, m) {
	if (body == null || !access(FFMPEG, 'x'))
		return false;
	let group = null;
	for (let inf in match(body, /#EXT-X-STREAM-INF:[^\n]*/g) ?? []) {
		let codecs = match(inf[0], /CODECS="([^"]*)"/)?.[1] ?? '';
		if (!playable(codecs))
			continue;
		let g = match(inf[0], /AUDIO="([^"]*)"/)?.[1];
		if (!g || !match(codecs, /mp4a\.40\.2/) ||
		    match(codecs, /(ac-3|ec-3|mp4a\.40\.(5|29)|opus|flac)/))
			return false;
		group ??= g;
	}
	let uri = null;
	for (let m in match(body, /#EXT-X-MEDIA:[^\n]*/g) ?? [])
		if (!uri && match(m[0], /TYPE=AUDIO(,|$)/) &&
		    match(m[0], /GROUP-ID="([^"]*)"/)?.[1] == group)
			uri = match(m[0], /URI="([^"]*)"/)?.[1];
	if (!group || !uri)
		return false;
	let pl = fetch_text(resolve(master, uri), h, m) ?? '';
	let seg = trim(match(pl, /\n([^#\n][^\n]*)/)?.[1] ?? '');
	return substr(pl, 0, 7) == '#EXTM3U' && index(pl, '#EXT-X-ENDLIST') >= 0 &&
		!match(pl, /#EXT-X-(MAP|DISCONTINUITY|BYTERANGE)/) &&
		!match(pl, /#EXT-X-KEY:(.*,)?METHOD=(AES|SAMPLE)/) &&
		ext(match(seg, /^[^?#]*/)[0]) == 'ts';
}

// FNV-1a, for short file names.
function hash(s) {
	let h = 0x811c9dc5;
	for (let i = 0; i < length(s); i++)
		h = ((h ^ ord(s, i)) * 0x01000193) & 0xffffffff;
	return sprintf('%08x', h);
}

// Rewrites an audio media playlist so its segments point at /api/a/<key>/<n>.ts
// and keeps their upstream URLs and durations for that handler.
function drc_playlist(body, url, h, m) {
	let key = hash(match(url, /^[^?#]*/)[0]), segs = [], durs = [], dur = 10;
	let out = map(split(body, '\n'), l => {
		l = rtrim(l, '\r');
		if (substr(l, 0, 8) == '#EXTINF:')
			dur = +(match(l, /^#EXTINF:([0-9.]+)/)?.[1] ?? 10);
		if (l == '' || substr(l, 0, 1) == '#')
			return l;
		push(segs, resolve(url, trim(l)));
		push(durs, dur);
		return `/api/a/${key}/${length(segs) - 1}.ts`;
	});
	mkdir(DRC);
	save(`${DRC}${key}.json`, { segs, durs, h: valid_headers(h) ? h : null,
		m: valid_media(m) ? m : null });
	return join('\n', out);
}

// The first audio PES of an MPEG-TS chunk: { pts (90 kHz), rate (Hz, from
// the ADTS header) }, or null.
function ts_audio(s) {
	const RATES = [96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
		16000, 12000, 11025, 8000, 7350];
	for (let at = 0; at + 188 <= length(s); at += 188) {
		if (ord(s, at) != 0x47)
			return null;
		let afc = (ord(s, at + 3) >> 4) & 3, p = at + 4;
		if (!(ord(s, at + 1) & 0x40) || !(afc & 1))
			continue;
		if (afc == 3)
			p += 1 + ord(s, at + 4);
		let sid = ord(s, p + 3);
		if (substr(s, p, 3) != '\x00\x00\x01' || sid < 0xC0 || sid > 0xDF ||
		    !(ord(s, p + 7) & 0x80))
			continue;
		let b = map(split(substr(s, p + 9, 5), ''), c => ord(c));
		let a = p + 9 + ord(s, p + 8);
		return {
			pts: ((b[0] >> 1) & 7) * 1073741824 + (b[1] << 22) +
				((b[2] >> 1) << 15) + (b[3] << 7) + (b[4] >> 1),
			rate: ord(s, a) == 0xFF && (ord(s, a + 1) & 0xF0) == 0xF0 ?
				RATES[(ord(s, a + 2) >> 2) & 15] : null,
		};
	}
	return null;
}

const drc_file = (key, i) => `${DRC}${key}.${i}.ts`;

// Segment i as fetched from upstream, cached; null when the fetch fails.
function drc_raw(meta, key, i) {
	let f = drc_file(key, i), s = readfile(f);
	if (s != null)
		return s;
	let url = proxy_target(meta.segs[i]);
	let pipe = url ? popen(fetch_cmd(url, meta.h, null, meta.m)) : null;
	s = pipe?.read('all');
	if (!pipe || pipe.close() != 0 || ord(s ?? '', 0) != 0x47)
		return null;
	let tmp = `${f}.${clock()[1]}`;
	if (writefile(tmp, s) != length(s) || !rename(tmp, f))
		unlink(tmp);
	return s;
}

// Segment i leveled; null when it can't be (the original is served then).
function drc_level(meta, key, i, cur) {
	let a = ts_audio(substr(cur, 0, 65536));
	if (!a?.rate || !readfile(drc_file(key, i), 1))
		return null;
	let prev = i > 0 ? drc_raw(meta, key, i - 1) : null;
	let next = i + 1 < length(meta.segs) ? drc_raw(meta, key, i + 1) : null;
	let end = next != null ? ts_audio(substr(next, 0, 65536))?.pts : null;
	if (end != null && end <= a.pts)
		end = null;
	// Just the parts of the neighbours that matter, in whole TS packets.
	let part = (s, sec, dur) =>
		188 * int(length(s) * min(1, (sec + 1) / (dur || 10)) / 188);
	let inputs = [], tmp = [];
	if (prev != null) {
		let f = `${DRC}${key}.${i}.pre`, n = part(prev, DRC_PRE, meta.durs[i - 1]);
		writefile(f, substr(prev, length(prev) - n));
		push(inputs, f);
		push(tmp, f);
	}
	push(inputs, drc_file(key, i));
	if (end != null) {
		let f = `${DRC}${key}.${i}.post`;
		writefile(f, substr(next, 0, part(next, DRC_POST, meta.durs[i + 1])));
		push(inputs, f);
		push(tmp, f);
	}
	// 90 kHz ticks to samples; the encoder's frames start 1024 samples
	// before its input, so the input starts a whole number of frames early.
	let rate = a.rate, at = t => int((t * rate + 45000) / 90000);
	let af = (prev != null ? `atrim=start_pts=${at(a.pts) - int(DRC_PRE * rate / 1024) * 1024},` : '') +
		DRC_FILTER + (end != null ? `,atrim=end_pts=${at(end) + 4 * 1024}` : '');
	// Half a frame of slack for rounding to 90 kHz.
	let drop = `noise=drop=lt(pts*tb*90000\\,${a.pts - 900})` +
		(end != null ? `+gte(pts*tb*90000\\,${end - 900})` : '');
	let out = `${DRC}${key}.${i}.out`, part_out = `${out}.${clock()[1]}`;
	let r = run(`${shq(FFMPEG)} -nostdin -v error -y -copyts ` +
		`-i ${shq('concat:' + join('|', inputs))} -map 0:a:0 -af ${shq(af)} ` +
		`-c:a aac -aac_coder fast -b:a 128k -bsf:a ${shq(drop)} ` +
		`-f mpegts -muxdelay 0 -muxpreload 0 ${shq(part_out)}`);
	for (let f in tmp)
		unlink(f);
	let s = r.code == 0 ? readfile(part_out) : null;
	if (!s || ord(s, 0) != 0x47) {
		unlink(part_out);
		return null;
	}
	if (!rename(part_out, out))
		unlink(part_out);
	return s;
}

// Keeps the segments around the one just served, and other playlists'
// files for a while (another TV may be playing them).
function drc_gc(key, i) {
	for (let f in lsdir(DRC) ?? []) {
		let m = match(f, /^([0-9a-f]{8})\.(json|([0-9]+)\..*)$/);
		if (!m)
			continue;
		let st = stat(DRC + f);
		if (m[1] == key ? m[3] != null && (+m[3] < i - 2 || +m[3] > i + 2) :
		    st && st.mtime < time() - (m[2] == 'json' ? 86400 : 600))
			unlink(DRC + f);
	}
}

function drc_segment(key, i) {
	let meta = load(`${DRC}${key}.json`);
	if (i >= length(meta?.segs ?? []))
		fail(404, 'no such segment');
	let s = readfile(`${DRC}${key}.${i}.out`);
	if (s == null) {
		let cur = drc_raw(meta, key, i);
		if (cur == null)
			fail(502, 'upstream fetch failed');
		s = drc_level(meta, key, i, cur) ?? cur;
	}
	drc_gc(key, i);
	media_head(200, MIME.ts);
	stdout.write(s);
	exit(0);
}

// Appends the bytes of a segment of media m streamed since start (a
// clock()) and the milliseconds that took to BYTES<m>, for title-api (see
// media_headers): one short line, which concurrent requests may append at
// once.
function count_bytes(m, n, start) {
	let end = clock(), f = open(BYTES + m, 'a');
	if (!f)
		return;
	f.write(sprintf('%d %d\n', n, (end[0] - start[0]) * 1000 + (end[1] - start[1]) / 1000000));
	f.close();
}

function proxy(name, u, q, h, drc, m) {
	let start = clock();
	let url = proxy_target(u);
	if (!url)
		fail(400, 'bad url');
	if (h != null && !valid_headers(h))
		fail(400, 'bad headers');
	if (m != null && !valid_media(m))
		fail(400, 'bad media');
	let e = ext(name), err = ERR + clock()[1];
	let pipe = popen(fetch_cmd(url, h, err, m));
	if (!pipe) {
		unlink(err);
		fail(502, 'popen failed');
	}

	if (e == 'm3u8' || e == 'm3u') {
		let body = pipe.read('all') ?? '';
		if (pipe.close() != 0 || substr(body, 0, 7) != '#EXTM3U')
			fetch_fail(err, 'upstream playlist fetch failed');
		unlink(err);
		let text = drc && index(body, '#EXTINF') >= 0 && access(FFMPEG, 'x') ?
			drc_playlist(body, url, h, m) :
			rewrite_playlist(body, url, match(q ?? '', /^[0-9]{1,5}$/) ? +q : null, h, drc, m);
		media_head(200, MIME[e]);
		print(text, '\n');
		exit(0);
	}

	// Hold back the headers until the first chunk so upstream failures
	// (e.g. 410 on a stale signature) become an error reply instead of an
	// empty 200.
	let chunk = pipe.read(65536);
	if (chunk == null || chunk == '') {
		pipe.close();
		fetch_fail(err, 'upstream fetch failed');
	}
	// Gone before the stream, which the receiver may cut short.
	unlink(err);
	media_head(200, MIME[e] ?? 'application/octet-stream');
	let n = 0;
	while (chunk != null && chunk != '') {
		stdout.write(chunk);
		n += length(chunk);
		chunk = pipe.read(65536);
	}
	pipe.close();
	// The media's segments, as the player counts the fragments it loaded.
	if (valid_media(m) && !(e in ['vtt', 'webvtt', 'key']))
		count_bytes(m, n, start);
	exit(0);
}

// Absolute proxy base for URLs handed to the receiver. play_next has no
// request and takes the base of the last play.
let next_base = null;
function public_base() {
	return next_base ?? rtrim(conf('public_url', '') || `http://${getenv('HTTP_HOST')}`, '/');
}

// Poster images sit behind the provider's anti-bot page; title-api fetches
// them with its session.
function poster(u) {
	if (!u)
		return null;
	let via_api = `${API}/v1/posters?url=${urlenc(u)}`;
	return PROXY ? purl(via_api, null, 'poster.jpg') : via_api;
}

// Starts a movie or an episode; play also switches the quality or the source
// of what is on: with reuse=1 it takes the streams from the last play of the
// same title if they are still fresh (title-api keeps synthetic masters for
// 6 hours), and start/audio/subs restore the position and tracks. keep, from
// play_next: { audio, subs } track names to pick when audio/subs are not
// given.
function play(dev, p, keep) {
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
	if (p.source != null && (length(p.source) > 100 || match(p.source, /[[:cntrl:]]/)))
		fail(400, 'invalid source');
	for (let k in ['drc', 'autonext'])
		if (p[k] != null && !match(p[k], /^[01]$/))
			fail(400, `invalid ${k}`);
	let last = load(state_file(dev));
	let list, fetched;
	if (p.reuse == '1' && last?.streams && last.id == id &&
	    last.season == season && last.episode == episode &&
	    time() - last.fetched < 19800) {
		list = last.streams;
		fetched = last.fetched;
	} else {
		let c = title_streams(dev, id, season, episode);
		list = c.streams;
		fetched = c.fetched;
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
	let h = s.headers ?? '', m = media_id(s.url), body = null;
	if (s.variants == null) {
		// title-api assembles a source's master on first request; a
		// source the CDN rejects fails here instead of on the TV.
		body = fetch_text(s.url, h, m);
		// title-api forgets its masters when it restarts, so one of
		// the streams of an earlier play may be gone: the provider is
		// asked for them again.
		if (qualities(body) == null && m && list == last?.streams) {
			let c = title_streams(dev, id, season, episode, true);
			list = c.streams;
			fetched = c.fetched;
			s = filter(list, x => x.name == s.name)[0];
			if (!s?.url)
				fail(404, 'stream not found');
			h = s.headers ?? '';
			m = media_id(s.url);
			body = fetch_text(s.url, h, m);
		}
		s.variants = qualities(body);
		if (s.variants == null)
			reply(502, { error: 'source unavailable', source: s.name,
				sources: map(list, x => x.name) });
	}
	// The next episode started by the router keeps the dub and the
	// subtitles of the last one, found by name: track ids may differ.
	if (keep?.audio && p.audio == null)
		p.audio = filter(s.audio, a => a.name == keep.audio)[0]?.id;
	let ks = keep?.subs ? filter(s.subs, t => t.url && t.name == keep.subs)[0] : null;
	if (ks && p.subs == null)
		p.subs = 'sub' + ks.id;
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
	// Without start (or reuse) the title resumes where it was left off;
	// start=0 plays it from the beginning.
	let start = p.start ?? (p.reuse == '1' ? null : resume_at(id, season, episode));
	// Tells title-api the quality and the position before the CDN sees
	// a request; what the TV told of an earlier play of the media doesn't
	// count.
	if (m) {
		unlink(POS + m);
		media_headers(m, quality_digits(quality), start != null ? +start : null, true);
	}
	// Probed once per source, on the variant this play picks.
	if (body != null) {
		s.fmp4 = is_fmp4(s.url, body, h, width, m);
		s.drc = PROXY && !s.fmp4 && drc_supported(s.url, body, h, m);
	}
	let drc = p.drc == '1' && !!s.drc;
	let url = PROXY ? purl(s.url, public_base(), null, h, m) +
		(width ? `&q=${width}` : '') + (drc ? '&drc=1' : '') : s.url;
	// Subtitles are separate WebVTT files, not part of the HLS manifest,
	// so they are side-loaded; the receiver names them "sub<id>".
	let args = ['play', url], labels = {};
	if (season != null)
		next_episode(id, season, episode);
	if (+(start ?? 0) > 0)
		push(args, '-p', sprintf('%.1f', +start));
	if (s.fmp4)
		push(args, '-f', 'fmp4');
	// Signed subtitle URLs can be ~2 KB each through the proxy, too much
	// for the 64 KB LOAD message, so the receiver gets short /sub/<tv>/<n>
	// links that look the real URL up in a file written beforehand.
	let picked = pick_subs(s.subs);
	writefile(SUBS + state_key(dev), sprintf('%J', {
		urls: map(picked, t => t.url), headers: h, media: m,
	}));
	for (let i, t in picked) {
		push(args, '-s', PROXY ? `${public_base()}/api/sub/${state_key(dev)}/${i}.vtt` : t.url,
		     t.lang || 'und', 'sub' + t.id);
		labels['sub' + t.id] = t.name;
	}
	for (let t in s.audio)
		labels[t.id] = t.name;
	if (p.style != null)
		push(args, '-y', p.style);
	// The state goes first: uhttpd may kill us (script_timeout, or the
	// phone went away) while castctl waits for playback, and the TV then
	// plays this stream all the same. A failed play puts the last one back.
	let f = state_file(dev), now = {
		id, stream: n, source: s.name, url, host: host_of(dev), media: m,
		title: p.title ?? null, season,
		episode, labels, quality, qualities: map(quals, v => v.label),
		drc, drc_ok: !!s.drc,
		// Kept for the next episode the router starts itself (see play_next).
		// Without autonext a replay of the same title (an older page)
		// keeps what was set.
		autonext: p.autonext != null ? p.autonext == '1' : last?.id == id && !!last.autonext,
		style: p.style ?? null,
		streams: list, fetched,
		// Only our own proxy paths, so the UI never loads arbitrary URLs.
		poster: length(p.poster ?? '') <= 4096 &&
			match(p.poster ?? '', /^\/api\/p\/[^ ]+$/) ? p.poster : null,
	};
	let put_back = () => last ? save(f, last) : unlink(f);
	save(f, now);
	undo = put_back;
	let st = castctl(dev, ...args);
	undo = null;
	// The socket of what this replaces says it ended.
	if (last?.media != m)
		media_end(last?.media);
	// castctl found the TV at a new address from DHCP: the state moves to
	// its key, and the old key gets back what it had.
	if (host_of(dev) != now.host) {
		now.host = host_of(dev);
		save(state_file(dev), now);
		put_back();
	}
	for (let k in ['audio', 'subs'])
		if (p[k] != null)
			st = castctl_try(dev, k, p[k]) ?? st;
	st = with_now(st, dev);
	// The TV plays: title-api learns the duration and the position now.
	if (m)
		media_headers(m, quality_digits(quality), null, true);
	return st;
}

// Plays the episode after the one that has just ended on the TV of key (see
// ended), crossing into the next season, with the source, quality, leveler,
// subtitle style, dub and subtitles of the last one.
function play_next(key) {
	let f = STATE + key, now = load(f), seen = load(SEEN + key);
	let dev = { host: now?.host }, st = now?.host ? castctl_try(dev, 'status') : null;
	if (!st || !ended(now, st, seen))
		return;
	// One try per episode: a failed play puts this state back, and a
	// failure here leaves it as well.
	now.next_at = time();
	save(f, now);
	let S = api(`/v1/titles/${now.id}`).seasons ?? [], si = -1, ei = -1;
	for (let i, x in S)
		if (`${x.seasonNumber}` == `${now.season}`) {
			si = i;
			for (let j, e in x.episodes ?? [])
				if (`${e.episodeNumber}` == `${now.episode}`)
					ei = j;
		}
	if (si < 0 || ei < 0)
		return;
	let s = S[si], e = s.episodes[ei + 1];
	if (!e) {
		s = S[si + 1];
		e = s?.episodes?.[0];
	}
	if (!e)
		return; // the last episode
	next_base = match(now.url, /^(.*)\/api\/p\//)?.[1];
	play(dev, {
		id: now.id, title: now.title, poster: now.poster,
		season: `${s.seasonNumber}`, episode: `${e.episodeNumber}`,
		source: now.source, quality: now.quality, drc: now.drc ? '1' : '0',
		autonext: '1', style: now.style,
	}, { audio: seen.audio, subs: seen.subs });
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
		return { streams: title_streams(device(p), title_id(p), season, episode).streams };
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

	// Where a title was left off: pos, dur and done of a movie, or the last
	// episode and eps by "season:episode" of a series; null if never watched.
	// Without id: the recently watched titles, latest first, each with the
	// position of the movie or of the last episode.
	'GET /progress': p => {
		let T = progress().titles;
		if (p.id == null) {
			let recent = [];
			for (let id, e in T) {
				let x = e.eps ? e.eps[ep_key(e.last?.season, e.last?.episode)] : e;
				if (e.title && x)
					push(recent, {
						id, title: e.title, poster: e.poster ?? null, t: e.t,
						season: e.last?.season, episode: e.last?.episode,
						pos: x.pos, dur: x.dur, done: x.done,
					});
			}
			return { recent: sort(recent, (a, b) => b.t - a.t) };
		}
		let e = T[title_id(p)];
		return { progress: !e ? null : e.eps ? { last: e.last, eps: e.eps } :
			{ pos: e.pos, dur: e.dur, done: e.done } };
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

	'POST /play': p => play(device(p), p),

	// Turns the next episode on or off for what is on (see ended).
	'POST /autonext': p => {
		if (!match(p.on ?? '', /^[01]$/))
			fail(400, 'invalid on');
		let dev = device(p), f = state_file(dev), now = load(f);
		if (now) {
			now.autonext = p.on == '1';
			save(f, now);
		}
		return with_now(castctl(dev, 'status'), dev);
	},

	'POST /cmd': p => {
		let c = p.c ?? '', kind = COMMANDS[c], arg = p.arg ?? '';
		if (!exists(COMMANDS, c))
			fail(400, 'unknown command');
		let dev = device(p);
		// Stopped from the page, the episode has not ended (see ended),
		// and the playback has.
		if (c == 'stop' || c == 'quit') {
			unlink(SEEN + state_key(dev));
			media_end(load(state_file(dev))?.media);
		}
		if (kind == 'num' && !match(arg, /^[0-9]+(\.[0-9]+)?$/))
			fail(400, 'numeric arg required');
		if (kind == 'str' && (arg == '' || length(arg) > 128))
			fail(400, 'arg required');
		if (kind == 'style' && !valid_style(arg))
			fail(400, 'invalid subtitle style');
		let st = with_now(kind == null ? castctl(dev, c) : castctl(dev, c, arg), dev);
		// A pause, a resume or a seek reaches title-api at once, not with
		// the next refresh (which a paused TV may not ask for in a while).
		let now = index(['pause', 'resume', 'seek', 'ff', 'rew'], c) >= 0 ?
			load(state_file(dev)) : null;
		if (valid_media(now?.media) && st.media?.url == now.url)
			media_headers(now.media, quality_digits(now.quality), null, true);
		return st;
	},
};

if (ARGV[0] == 'tick') {
	tick();
	exit(0);
}
if (ARGV[0] == 'next' && match(ARGV[1] ?? '', /^[0-9A-Za-z._]+$/)) {
	play_next(ARGV[1]);
	exit(0);
}

// The uhttpd alias passes the full URL path (/api/x); direct CGI calls pass /x.
// The media proxy (and the leveled audio) is called by the receiver, which
// can't send a token, so it is exempt from the token check; it only reaches
// public hosts.
let path = replace(rtrim(getenv('PATH_INFO') ?? '', '/'), /^\/api(\/|$)/, '/');
let pm = match(path, /^\/p\/([^\/]*)$/);
if (pm) {
	if (getenv('REQUEST_METHOD') != 'GET')
		fail(405, 'method not allowed');
	let qs = parse_form(getenv('QUERY_STRING'), {});
	proxy(pm[1], qs.u, qs.q, qs.h, qs.drc == '1', qs.m);
}
// Leveled audio segments (see drc_playlist).
let am = match(path, /^\/a\/([0-9a-f]{8})\/([0-9]{1,6})\.ts$/);
if (am) {
	if (getenv('REQUEST_METHOD') != 'GET')
		fail(405, 'method not allowed');
	drc_segment(am[1], +am[2]);
}
// Subtitles of what /play started, by index (see the short links in /play).
let sm = match(path, /^\/sub\/([0-9A-Za-z._]+)\/([0-9]+)\.vtt$/);
if (sm) {
	let subs = load(SUBS + sm[1]);
	let url = subs?.urls?.[+sm[2]];
	if (!url)
		fail(404, 'no such subtitle');
	proxy('sub.vtt', url, null, subs.headers || null, false, subs.media);
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
