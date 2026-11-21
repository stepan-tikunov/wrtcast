#!/usr/bin/ucode
// wrtcast — CGI backend for the Chromecast remote. Proxies the title-api
// HTTP/JSON gateway, drives castctl and relays media (HLS playlists, segments,
// posters) so upstream headers and CORS are under our control. Chromecasts
// are found with castctl discover; requests pick one with device=<id>.
// Configured in /etc/config/wrtcast.
'use strict';

import { popen, open, readfile, writefile, rename, unlink, lsdir, mkdir, stat, access, stdin, stdout } from 'fs';
import { cursor } from 'uci';
import * as sqlite from 'sqlite';
import { abs } from 'math';

const DB = '/tmp/wrtcast.db';                // all state, see "state" below
const MAX_TITLES = 200;                      // progress kept for this many titles
const MAX_EPISODES = 50;                     // and this many episodes of each
const FLUSH = 300;                           // seconds between flash writes
const DONE = 0.85;                           // an episode left this far in is watched
const DEVICES_TTL = 60;                      // seconds before /devices rescans
const HORIZON = 75;                          // seconds ahead a section is waited for (see plan)
const AHEAD = 120;                           // and the next episode is prepared (see ahead)
const DRC = '/tmp/wrtcast.drc/';             // audio segments of the leveler
const ERR = '/tmp/wrtcast.err.';           // + ns; stderr of a proxied fetch
const PAUSES = '/tmp/wrtcast.pauses';       // where pauses came from, see pause_note
const CACHE = '/tmp/wrtcast.cache/';        // segments kept, see "segment cache"
const CACHE_KEEP = 16;                       // seconds kept from where a playback would start
const CACHE_DAYS = 7;                        // and for so long at most
const COMMANDS = {
	pause: null, resume: null, stop: null, quit: null, mute: null, unmute: null,
	seek: 'num', ff: 'num', rew: 'num', volume: 'num', speed: 'num', audio: 'str',
	subs: 'str', substyle: 'style',
};
const REASONS = {
	'200': 'OK', '400': 'Bad Request', '401': 'Unauthorized', '403': 'Forbidden',
	'404': 'Not Found', '405': 'Method Not Allowed', '409': 'Conflict', '500': 'Internal Server Error',
	'502': 'Bad Gateway', '503': 'Service Unavailable',
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
// The watch history on flash (see keep).
const KEEP = conf('db_file', '/etc/wrtcast.db');
// A minimal static ffmpeg (scripts/build-ffmpeg.sh); without it the volume
// leveler is not offered.
const FFMPEG = conf('ffmpeg', '/usr/bin/wrtcast-ffmpeg');
// MiB of segments kept in /tmp, which is the router's memory (see "segment
// cache"); 0 keeps none.
const CACHE_MB = +conf('cache_mb', 40);
// The music of what plays, which title-api tells (see "music").
const MUSIC = conf('music', '1') == '1';

function reply(code, body) {
	printf('Status: %d %s\r\nContent-Type: application/json\r\n' +
	       'Cache-Control: no-store\r\n\r\n%J\n', code, REASONS[`${code}`], body);
	exit(0);
}

// Run before a failure reply: /play sets it while the state it wrote ahead
// of castctl must be put back if the play fails.
let undo = null;

function fail(code, msg) {
	let u = undo;
	undo = null;
	if (u)
		u();
	reply(code, { error: msg });
}

// A JSON file, or null when it is missing or cut short.
function load(f) {
	try {
		return json(readfile(f) ?? 'null');
	} catch (e) {
		return null;
	}
}

// ---- state -----------------------------------------------------------------
//
// What wrtcast keeps between requests is in one SQLite database in /tmp
// (sqlite/sqlite.c), shared by the concurrent CGI processes: per TV, what
// /play started and the like (tvs); what /play started with each URL
// (played); per title-api media, its CDN headers, the position the TV told
// and the bytes of its segments streamed (media, segments); the playlists of
// the volume leveler (drc); the devices found (kv); and the watch history
// (titles, progress) with the titles saved to watch (saved). Only the
// history outlives a reboot: it is copied to
// flash every few minutes (see keep) and back on the first request after a
// boot. Files are left for the leveler's segments and fetches' stderr.

const SCHEMA = 4;

// The history tables in schema s: per title, when it was last watched, its
// name and poster for the recently watched list, a series' last episode and
// what it was watched with (see watched_with); the position in a movie (ep
// '') or in each episode ("season:episode"); the settings (see setting);
// and the titles saved to watch, with when they were saved.
const SAVED = s => `CREATE TABLE ${s}.saved (id TEXT PRIMARY KEY, title TEXT, poster TEXT, t INTEGER NOT NULL)`;
const HISTORY = s => `
	CREATE TABLE ${s}.titles (
		id TEXT PRIMARY KEY, title TEXT, poster TEXT,
		season INTEGER, episode TEXT, prefs TEXT, t INTEGER NOT NULL
	);
	CREATE TABLE ${s}.progress (
		id TEXT NOT NULL, ep TEXT NOT NULL, pos REAL NOT NULL, dur REAL NOT NULL,
		done INTEGER NOT NULL, t INTEGER NOT NULL, PRIMARY KEY (id, ep)
	);
	CREATE TABLE ${s}.settings (key TEXT PRIMARY KEY, value TEXT);
	${SAVED(s)};`;
const TITLE_COLS = 'id, title, poster, season, episode, prefs, t';
const PROGRESS_COLS = 'id, ep, pos, dur, done, t';

// The segment cache (see there): per file its key, its video (c), start and
// stop in seconds (null for an init segment), bytes, the media it was
// loaded with, when it was last used, and whether it is where a playback
// would start (keep).
const CACHE_TABLE = `
	CREATE TABLE cache (key TEXT PRIMARY KEY, c TEXT NOT NULL, start REAL, stop REAL,
		bytes INTEGER NOT NULL, media TEXT, t INTEGER NOT NULL, keep INTEGER NOT NULL);
	CREATE INDEX cache_c ON cache (c);`;

// Documents are JSON text: per TV (key, see state_key) now, what /play
// started; subs, the subtitles it side-loaded (see /sub); list, the streams
// last asked for (see title_streams); watch, the tick's failed checks; seen,
// when now last played (see watch_end); next_lock, while the next episode
// starts (see play_next).
const TABLES = `
	CREATE TABLE tvs (
		key TEXT PRIMARY KEY, now TEXT, subs TEXT, list TEXT, watch TEXT,
		seen TEXT, next_lock INTEGER
	);
	CREATE TABLE played (url TEXT PRIMARY KEY, now TEXT NOT NULL, t INTEGER NOT NULL);
	CREATE TABLE media (id TEXT PRIMARY KEY, hdr TEXT, pos TEXT, t INTEGER NOT NULL);
	CREATE TABLE segments (media TEXT NOT NULL, bytes INTEGER NOT NULL,
		ms INTEGER NOT NULL, t INTEGER NOT NULL);
	CREATE INDEX segments_media ON segments (media);
	CREATE TABLE drc (key TEXT PRIMARY KEY, meta TEXT NOT NULL, t INTEGER NOT NULL);
	CREATE TABLE kv (key TEXT PRIMARY KEY, value);` + CACHE_TABLE;

let db = null;

function db_fail() {
	fail(500, 'state: ' + (db?.error() ?? sqlite.error() ?? 'unknown error'));
}

// The history as the wrtcast before the database kept it, into the titles
// and progress of db: { titles: { <id>: { title, poster, t, prefs, pos,
// dur, done (a movie) or last: { season, episode }, eps: {
// "<season>:<episode>": { pos, dur, done, t } } } } }.
function import_json(P) {
	let put = (q, ...a) => db.run(q, ...a) ?? db_fail();
	for (let id, e in (type(P?.titles) == 'object' ? P.titles : {})) {
		let eps = type(e?.eps) == 'object' ? e.eps : null;
		if (!+e?.t || (eps && !e.last))
			continue;
		put(`INSERT INTO titles (${TITLE_COLS}) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, e.title, e.poster, eps ? +e.last.season : null,
			eps ? `${e.last.episode}` : null, e.prefs, +e.t);
		for (let ep, x in eps ?? { '': e })
			if (x?.pos != null)
				put(`INSERT INTO progress (${PROGRESS_COLS}) VALUES (?, ?, ?, ?, ?, ?)`,
					id, ep, +x.pos, +(x.dur ?? 0), !!x.done, +(x.t ?? e.t));
	}
}

// Creates the tables in a new database (after a boot) and fills the history
// from flash, or from the JSON file of the wrtcast before the database.
// Another request may be doing the same: the one that gets the write lock
// first does it. A database of schema 1 (before the settings) gets the
// settings table, one before the segment cache its table and one before
// the saved titles theirs; a copy on flash of schema 1 has no settings,
// and one before schema 4 no saved titles.
function init() {
	let exec = q => db.exec(q) ?? db_fail();
	let version = () => (db.query('PRAGMA user_version') ?? db_fail())[0].user_version;
	exec('PRAGMA journal_mode = WAL');
	let keep = access(KEEP) ? KEEP : null;
	if (keep)
		db.run('ATTACH ? AS keep', `file:${keep}?mode=ro`) ?? db_fail();
	exec('BEGIN IMMEDIATE');
	let v = version();
	if (v == 0) {
		exec(HISTORY('main') + TABLES);
		if (keep)
			exec(`INSERT INTO titles (${TITLE_COLS}) SELECT ${TITLE_COLS} FROM keep.titles;
				INSERT INTO progress (${PROGRESS_COLS}) SELECT ${PROGRESS_COLS} FROM keep.progress`);
		else
			import_json(load('/tmp/wrtcast.progress.json') ??
				load(conf('progress_file', '/etc/wrtcast.progress.json')));
	} else {
		if (v < 2)
			exec('CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT)');
		if (v < 3)
			exec(CACHE_TABLE);
		if (v < 4)
			exec(SAVED('main'));
	}
	if (v != SCHEMA) {
		let kept = name => keep && length(db.query('SELECT 1 FROM keep.sqlite_master WHERE name = ?', name) ?? []);
		if (v < 2 && kept('settings'))
			exec('INSERT OR REPLACE INTO settings (key, value) SELECT key, value FROM keep.settings');
		if (v < 4 && kept('saved'))
			exec('INSERT OR REPLACE INTO saved (id, title, poster, t) SELECT id, title, poster, t FROM keep.saved');
		exec(`PRAGMA user_version = ${SCHEMA}`);
	}
	exec('COMMIT');
	if (keep)
		exec('DETACH keep');
}

function store() {
	if (db)
		return db;
	db = sqlite.open(DB, 5000);
	if (!db)
		db_fail();
	if (db.exec('PRAGMA synchronous = OFF') == null) // tmpfs
		db_fail();
	if (db.query('PRAGMA user_version')?.[0]?.user_version != SCHEMA)
		init();
	return db;
}

// Rows of a query; null parameters are NULL, objects and arrays JSON.
function sql_rows(q, ...a) {
	let r = store().query(q, ...a);
	if (r == null)
		db_fail();
	return r;
}

const sql_row = (q, ...a) => sql_rows(q, ...a)[0];

// Runs a statement; the number of rows it changed.
function sql_do(q, ...a) {
	let n = store().run(q, ...a);
	if (n == null)
		db_fail();
	return n;
}

function sql_exec(q) {
	if (store().exec(q) == null)
		db_fail();
}

function kv_get(k) {
	return sql_row('SELECT value FROM kv WHERE key = ?', k)?.value;
}

function kv_set(k, v) {
	sql_do('INSERT INTO kv (key, value) VALUES (?, ?) ' +
	       'ON CONFLICT (key) DO UPDATE SET value = excluded.value', k, v);
}

// Notes in PAUSES what paused the TV of key, or seeked it by itself: the
// page, the lock screen widget (where headphones and watches send their
// pause too), or the TV, its remote or another app on the network; and how
// long a next episode the router started took. The last 64 KiB or so are
// kept.
function pause_note(key, what) {
	if (stat(PAUSES)?.size > 65536)
		rename(PAUSES, PAUSES + '.old');
	let t = localtime(), f = open(PAUSES, 'a');
	if (!f)
		return;
	f.write(sprintf('%04d-%02d-%02d %02d:%02d:%02d %s %s\n', t.year, t.mon, t.mday,
	                t.hour, t.min, t.sec, key, what));
	f.close();
}

// A JSON document of the TV with state key (see TABLES); null without one.
function tv_get(key, what) {
	let v = sql_row(`SELECT ${what} AS v FROM tvs WHERE key = ?`, key)?.v;
	return v != null ? json(v) : null;
}

function tv_set(key, what, v) {
	sql_do(`INSERT INTO tvs (key, ${what}) VALUES (?, ?) ` +
	       `ON CONFLICT (key) DO UPDATE SET ${what} = excluded.${what}`, key, v);
}

// hdr or pos of a title-api media (see media_headers); null without it.
function media_get(m, what) {
	let v = sql_row(`SELECT ${what} AS v FROM media WHERE id = ?`, m)?.v;
	return v != null ? json(v) : null;
}

function media_set(m, what, v) {
	sql_do(`INSERT INTO media (id, ${what}, t) VALUES (?, ?, ?) ` +
	       `ON CONFLICT (id) DO UPDATE SET ${what} = excluded.${what}, t = excluded.t`,
	       m, v, time());
}

// FNV-1a, for short file names.
function hash(s) {
	let h = 0x811c9dc5;
	for (let i = 0; i < length(s); i++)
		h = ((h ^ ord(s, i)) * 0x01000193) & 0xffffffff;
	return sprintf('%08x', h);
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

// absent: the statuses that answer null, as what title-api has none of.
function api(path, absent) {
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
	if (index(absent ?? [], r.status) >= 0)
		return null;
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

// A POST of body, as JSON, to title-api: { status, out } as api has them.
// The body goes through a pipe, never on a command line or into a file,
// as it may carry a key; only the answer is written to /tmp.
function api_post(path, body) {
	let f = ERR + clock()[1];
	let p = popen(`uclient-fetch -T 20 -O ${shq(f)} --header='Content-Type: application/json' ` +
		`--post-file=/dev/stdin ${shq(API + path)} 2>${shq(f + '.e')}`, 'w');
	if (!p)
		return { status: 0, out: '' };
	p.write(sprintf('%J', body));
	p.close();
	let m = match(readfile(f + '.e') ?? '', /HTTP error ([0-9]+)/), out = trim(readfile(f) ?? '');
	unlink(f);
	unlink(f + '.e');
	return { status: m ? +m[1] : out == '' ? 0 : 200, out };
}

const SECTION_KINDS = { KIND_RECAP: 'recap', KIND_INTRO: 'intro', KIND_CREDITS: 'credits' };

// title-api's sections of a movie or an episode: [{ kind: recap, intro or
// credits, start, end, skip }], skip false when there is more to see after
// them, such as a scene after the credits.
function sections(list) {
	let r = [];
	for (let x in list ?? [])
		if (SECTION_KINDS[x?.kind] && +x.endSeconds > +x.startSeconds)
			push(r, { kind: SECTION_KINDS[x.kind], start: +x.startSeconds,
				end: +x.endSeconds, skip: !!x.skippable });
	return sort(r, (a, b) => a.start - b.start);
}

// ---- title-api media headers -----------------------------------------------
//
// The alternative player's CDN wants headers that change during a playback:
// title-api holds the player's playback socket, whose edge_hash replaces the
// device fingerprint. So the proxy URLs of a stream that is a title-api
// master carry its media id (&m=) instead of the headers (&h=), and fetches
// take the headers from its media row, kept for the ttl title-api gives. A
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
// any, is kept with the headers (see fetch_cmd), and so are the sections
// on the media's video once title-api aligned it with the primary
// player's, or whether it still does (see sections_of). One request
// refreshes them while the others go on with what there is for a
// few seconds (a few may still refresh at once); a failed refresh keeps
// the stale ones too.
function media_headers(m, q, t, force) {
	let c = media_get(m, 'hdr'), now = time();
	let claimed = now - (c?.claimed ?? 0) < 10;
	if (type(c?.headers) != 'array')
		c = null;
	if (!force && (c && now - c.fetched < c.ttl || claimed))
		return c?.headers;
	if (c)
		c.claimed = now;
	media_set(m, 'hdr', c ?? { claimed: now });
	let qs = [], pos = media_get(m, 'pos');
	if (now - (pos?.at ?? 0) > 180)
		pos = null;
	if (t == null && pos)
		t = pos.pos + (pos.paused ? 0 : (now - pos.at) * (pos.rate ?? 1));
	let all = sql_row('SELECT count(*) AS n, total(bytes) AS b FROM segments WHERE media = ?', m);
	let last = sql_row('SELECT total(bytes) AS b, total(ms) AS ms FROM (SELECT bytes, ms ' +
		'FROM segments WHERE media = ? ORDER BY rowid DESC LIMIT 5)', m);
	let n = all.n, b = all.b, rb = last.b, rms = last.ms;
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
			proxy: match(r.proxy ?? '', /^\/v1\/media\/[0-9a-f]{32}\/cdn$/) ? r.proxy : null,
			sections: type(r.sections) == 'array' && length(r.sections) ? sections(r.sections) : null,
			aligning: !!r.aligning };
		media_set(m, 'hdr', c);
	}
	return c?.headers;
}

// Tells title-api the playback of media m ended, so its socket says so and
// closes. In the background: it takes title-api a moment, and a failure
// changes nothing. uclient-fetch can't send DELETE, hence the POST.
function media_end(m) {
	if (!valid_media(m))
		return;
	sql_do('DELETE FROM media WHERE id = ?', m);
	sql_do('DELETE FROM segments WHERE media = ?', m);
	system(`uclient-fetch -q -T 5 -O /dev/null --post-data= ` +
		`${shq(API + '/v1/media/' + m + '/session/end')} </dev/null >/dev/null 2>&1 &`);
}

// ---- devices ---------------------------------------------------------------

// Cached device list no older than max_age seconds (any age when null).
function cached(max_age) {
	let v = kv_get('devices'), c = v != null ? json(v) : null;
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
		kv_set('devices', { time: time(), devices: list });
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
	// A TV starting a stream drops the channel of the one before now and
	// then ("device closed the channel"); a status is asked again at once.
	if (r.code != 0 && args[0] == 'status' && index(r.out, 'closed the channel') >= 0)
		r = run('castctl -H ' + shq(host_of(dev)) + cmd);
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

// What /play started on the TV of a state key; null for nothing.
const now_get = key => tv_get(key, 'now');
const now_set = (key, v) => tv_set(key, 'now', v);

// What /play started with a URL is kept apart from the state of its TV too
// (played): a play castctl gave up on may start on the TV all the same, and
// of two plays to one TV at once the one that lost puts back the state from
// before both. Returns the state of the TV (key, at host) that plays media
// m: now, or when m is another stream, what /play started with that URL,
// which becomes the state. Not while a play is under way (see busy_set),
// the TV still plays the last stream then (castctl waits for the receiver
// up to 20 s, again at a new address). A play that failed and put the last
// state back is over: the stream it started on the TV all the same is taken
// at once, however recent the state put back.
function adopt(key, host, now, m) {
	let u = m?.url, busy = kv_get('busy:' + key);
	if (!u || now?.url == u || busy != null && time() - busy < 90)
		return now;
	let p = sql_row('SELECT now FROM played WHERE url = ?', u);
	if (!p)
		return now;
	p = json(p.now);
	p.host = host;
	now_set(key, p);
	return p;
}

// Marks a play under way on the TV of key, or clears the mark. One the web
// server killed leaves it, which counts for 90 s at most (see adopt).
function busy_set(key, on) {
	if (on)
		kv_set('busy:' + key, time());
	else
		sql_do('DELETE FROM kv WHERE key = ?', 'busy:' + key);
}

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

// ---- sections --------------------------------------------------------------
//
// title-api tells the recap, the intro and the credits of a movie or an
// episode (see sections). With autoskip the router skips the recap and the
// intro, and with autonext it starts the next episode once the credits
// start, also with the page closed. A CGI has no timer, so the status
// requests and the tick (see watch_end) plan what is due next and leave a
// process (wrtcast due) that sleeps until then, checks the TV and does it.

// The time now, in seconds with a fraction.
function clock_now() {
	let c = clock();
	return c[0] + c[1] / 1e9;
}

// What title-api tells of the sections on the video of now, a title-api
// media, as of its headers: { sections, aligning }; null for another
// stream. With refresh, stale headers are asked for again (see
// media_headers), which tells title-api the playback goes on, so only
// while the TV plays the media.
function aligned(now, refresh) {
	if (!valid_media(now?.media))
		return null;
	if (refresh)
		media_headers(now.media);
	return media_get(now.media, 'hdr');
}

// The sections of now on the video the TV plays, of dur seconds. The
// sections are marked on the primary player's video, which another
// source's may differ from by a logo or a few seconds at either end. The
// alternative player's video title-api aligns with it by their frames, and
// then tells the sections on it (see aligned). Otherwise the ones at the
// start move back by what a shorter video lacks and the credits on by what
// a longer one has, so a skip never cuts into the episode and the next one
// never starts before the credits do. A video off by more than a minute is
// another cut, and gets none.
function sections_of(now, dur) {
	let a = aligned(now);
	if (a?.sections)
		return a.sections;
	let list = now?.sections ?? [], ref = +(now?.sdur ?? 0), r = [];
	let d = ref > 0 ? dur - ref : 0;
	if (!length(list) || !(dur > 0) || d > 60 || d < -60)
		return r;
	for (let x in list) {
		let shift = x.kind == 'credits' ? max(0, d) : min(0, d);
		let start = max(0, x.start + shift), end = min(dur, x.end + shift);
		if (end - start >= 1)
			push(r, { ...x, start, end });
	}
	return r;
}

// The skippable recap and intro (and what runs on right after them) that
// pos is within, among list: { to, their end, idx, their indexes }; null
// for none. As due skips them.
function skips_at(list, pos) {
	for (let i, x in list) {
		if (x.kind == 'credits' || !x.skip || pos < x.start - 0.5 || pos >= x.end - 1)
			continue;
		let d = { to: x.end, idx: [i] };
		for (let j = i + 1; j < length(list); j++) {
			let y = list[j];
			if (y.kind == 'credits' || !y.skip || y.start - d.to > 1)
				break;
			d.to = y.end;
			push(d.idx, j);
		}
		return d;
	}
	return null;
}

// Where the skips of list after pos land (see skips_at): the ends of the
// recaps and intros, one right after another counting as one, that start
// after pos.
function skip_ends(list, pos) {
	let r = [];
	for (let x in list) {
		let d = x.kind != 'credits' && x.skip && x.start >= pos ? skips_at(list, x.start) : null;
		if (d && r[-1] != d.to)
			push(r, d.to);
	}
	return r;
}

// Where a playback of a video with sections list from pos plays without a
// seek of its own: from pos, with autoskip past the recap and the intro pos
// is within (as prepare starts it), and from where each skip ahead lands.
// The windows of the segment cache start there (see cache_mark, prefetch).
function starts_of(list, pos, autoskip) {
	let d = autoskip ? skips_at(list, pos) : null, from = d ? d.to : pos;
	return [from, ...(autoskip ? skip_ends(list, from) : [])];
}

// The plan of the TV of key: { url, done: { <index>: true } of the sections
// skipped, and of the waiting process token, i and act of what it waits
// for, at when }.
const plan_get = key => json(kv_get('due:' + key) ?? 'null');
const plan_set = (key, p) => kv_set('due:' + key, p);

// The indexes of the sections of now skipped (see plan_get).
function skipped(key, now) {
	let p = plan_get(key);
	return p?.url == now.url ? map(keys(p.done ?? {}), i => +i) : [];
}

// What is due next while now plays as media status m tells: { in, the
// seconds until it, i, its section, act: skip, to where, idx, the
// sections skipped, or next }, the nearest one; null for none. done are the
// sections skipped already: rewound into, they are not skipped again. A
// recap and the intro right after it are skipped at once. While title-api
// still aligns the video at the start of a playback (for 20 s at most), a
// skip waits for it, a second at a time.
function due(now, m, done) {
	if (m?.url != now.url || m.state != 'PLAYING')
		return null;
	let pos = +(m.time ?? 0), rate = +(m.rate ?? 1) || 1;
	let hold = !!aligned(now, true)?.aligning;
	let list = sections_of(now, +(m.duration ?? 0)), best = null;
	for (let i, x in list) {
		let d = null;
		if (x.kind != 'credits' && x.skip && now.autoskip && !done?.[i]) {
			d = { act: 'skip', to: x.end, idx: [i] };
			for (let j = i + 1; j < length(list); j++) {
				let y = list[j];
				if (y.kind == 'credits' || !y.skip || y.start - d.to > 1)
					break;
				d.to = y.end;
				push(d.idx, j);
			}
			// A second short of the end is not worth a seek.
			if (pos >= d.to - 1)
				continue;
		} else if (x.kind == 'credits' && x.skip && now.autonext && now.season != null &&
		           !now.next_at) {
			if (pos >= x.end - 1)
				continue;
			d = { act: 'next' };
		} else {
			continue;
		}
		d.in = max(0, (x.start - pos) / rate);
		if (hold && d.act == 'skip')
			d.in = max(d.in, 1);
		d.i = i;
		if (!best || d.in < best.in)
			best = d;
	}
	return best;
}

// Whether the credits of now have started on the TV (st) and the next
// episode is due (see play_next): within half a second, as wake acts.
function at_credits(now, st) {
	let d = st.app?.id == 'CC1AD845' ? due(now, st.media, null) : null;
	return d?.act == 'next' && d.in <= 0.5;
}

// Leaves a process to do what is due within HORIZON seconds, the time to
// the next tick, unless one already waits for it then. Returns what is due
// (see due), also later.
function plan(key, now, m) {
	let p = plan_get(key);
	if (p?.url != now.url)
		p = { url: now.url, done: {} };
	let d = due(now, m, p.done);
	if (!d || d.in > HORIZON)
		return d;
	let at = clock_now() + d.in;
	if (p.token && p.i == d.i && p.act == d.act && p.at - at < 1.5 && at - p.at < 1.5)
		return d;
	p.token = hash(`${clock_now()} ${key}`);
	p.i = d.i;
	p.act = d.act;
	p.to = d.to;
	p.at = at;
	plan_set(key, p);
	system(`${shq(sourcepath())} due ${shq(key)} ${p.token} </dev/null >/dev/null 2>&1 &`);
	return d;
}

// ---- segment cache ---------------------------------------------------------
//
// The segments of a playback's video pass through files in CACHE, and those
// where a playback would start again stay: from where an episode or a movie
// was left off (see cache_mark), and from where the next episode starts,
// which the router loads ahead (see prefetch). A play started there gets
// its first segments from the router at once, instead of a second and a
// half each through title-api's relay; also days later, with other signed
// links. A segment is known by the play's video (c: the title, the episode,
// the source and the variant, see cache_id), the durations of its
// playlist's segments (another cut gets other keys) and its times in it
// (see seg_keys); the router adds the keys to the playlists it relays
// (see rewrite_playlist). The primary player's videos, which the router
// loads from their CDN at once, pass through it as well: the segment a TV
// is paused in tells who is on the screen (see /faces). CACHE_MB bound the
// files (see cache_put), and the tick leaves of the videos no TV plays only
// the segments where a playback would start (see cache_trim). A boot
// empties it with the rest of /tmp.

const cache_id = (id, season, episode, source, width) =>
	hash(`${id}|${season}:${episode}|${source}|${width}`);

// <c>-<fingerprint of the durations>-i of an init segment, or -<start
// ms>-<stop ms> of a segment.
const CACHE_KEY = /^([0-9a-f]{8})-[0-9a-f]{8}-(i|([0-9]+)-([0-9]+))$/;

// The cache keys of the lines of a media playlist of video c, by line
// index: of its URI lines and of its init segment's #EXT-X-MAP. None for
// byte ranges or encryption, which the cache leaves alone.
function seg_keys(lines, c) {
	let durs = [], keys = [];
	for (let l in lines) {
		if (match(l, /^#EXT-X-BYTERANGE/) || match(l, /^#EXT-X-MAP:.*BYTERANGE/) ||
		    match(l, /^#EXT-X-KEY:(.*,)?METHOD=(AES|SAMPLE)/))
			return keys;
		let d = match(l, /^#EXTINF: *([0-9.]+)/);
		if (d)
			push(durs, d[1]);
	}
	let fp = hash(join(',', durs)), pos = 0, dur = null;
	for (let i, l in lines) {
		let d = match(l, /^#EXTINF: *([0-9.]+)/);
		if (d) {
			dur = +d[1];
		} else if (match(l, /^#EXT-X-MAP:/)) {
			keys[i] = `${c}-${fp}-i`;
		} else if (l != '' && substr(l, 0, 1) != '#' && dur != null) {
			keys[i] = sprintf('%s-%s-%d-%d', c, fp, int(pos * 1000 + 0.5), int((pos + dur) * 1000 + 0.5));
			pos += dur;
			dur = null;
		}
	}
	return keys;
}

// The cached file of key k: { bytes, media it was loaded with }; null
// without one.
function cache_get(k) {
	let r = sql_row('SELECT bytes, media FROM cache WHERE key = ?', k);
	if (r && !access(CACHE + k)) {
		sql_do('DELETE FROM cache WHERE key = ?', k);
		return null;
	}
	if (r)
		sql_do('UPDATE cache SET t = ? WHERE key = ?', time(), k);
	return r;
}

function cache_drop(k) {
	unlink(CACHE + k);
	sql_do('DELETE FROM cache WHERE key = ?', k);
}

// Whether a segment may be written: /tmp takes the router's memory, of
// which 32 MiB stay free.
function cache_room() {
	let a = match(readfile('/proc/meminfo') ?? '', /MemAvailable: *([0-9]+) kB/);
	mkdir(CACHE);
	return CACHE_MB > 0 && (!a || +a[1] > 32768);
}

// Keeps file f, a segment of n bytes loaded with media m, as key k; keep
// marks it where a playback would start. Beyond CACHE_MB others go: those
// behind what the TV plays, those of other videos, those furthest ahead of
// it, of those where a playback would start the oldest, and last those
// around where the TV is (from 10 s behind to 30 s ahead) and its init
// segment, which a pause is looked at by (see /faces). Where the TV
// is runs on from where it was last seen playing: with the page closed
// that is up to a minute ago (see tick), and the segments where it is now
// are the ones the next mark wants (see cache_mark).
function cache_put(k, f, n, m, keep) {
	let x = match(k, CACHE_KEY);
	if (!x || !rename(f, CACHE + k)) {
		unlink(f);
		return;
	}
	sql_do('INSERT OR REPLACE INTO cache (key, c, start, stop, bytes, media, t, keep) ' +
	       'VALUES (?, ?, ?, ?, ?, ?, ?, ?)', k, x[1], x[3] != null ? +x[3] / 1000.0 : null,
	       x[4] != null ? +x[4] / 1000.0 : null, n, m, time(), keep ? 1 : 0);
	let over = sql_row('SELECT total(bytes) AS b FROM cache').b - CACHE_MB * 1048576;
	if (over <= 0)
		return;
	let tv = sql_row('SELECT now, seen FROM tvs WHERE now IS NOT NULL ORDER BY ' +
	                 "json_extract(seen, '$.t') DESC LIMIT 1");
	let now = json(tv?.now ?? 'null'), seen = json(tv?.seen ?? 'null');
	let c = now?.c ?? '', pos = 0;
	if (now && seen?.url == now.url)
		pos = +(seen.pos ?? 0) + (seen.playing ? min(max(time() - seen.t, 0), 300) : 0);
	for (let r in sql_rows(`SELECT key, bytes FROM cache ORDER BY
			CASE WHEN c = ? AND (start IS NULL OR stop > ? AND start < ?) THEN 4
				WHEN keep = 1 THEN 3 WHEN c = ? AND stop <= ? THEN 0 WHEN c != ? THEN 1 ELSE 2 END,
			CASE WHEN keep = 0 AND c = ? AND stop > ? THEN -start ELSE t END`,
			c, pos - 10, pos + 30, c, pos - 10, c, c, pos - 10)) {
		if (over <= 0)
			break;
		cache_drop(r.key);
		over -= r.bytes;
	}
}

// Marks the segments of video c within CACHE_KEEP seconds from each of
// starts (see starts_of), and only those.
function cache_mark(c, starts) {
	if (!c || !length(starts))
		return;
	let a = [];
	for (let w in starts)
		push(a, w, w + CACHE_KEEP);
	sql_do(`UPDATE cache SET keep = (start IS NULL OR ${join(' OR ',
	       map(starts, w => '(stop > ? AND start < ?)'))}) WHERE c = ?`, ...a, c);
}

// ---- music -----------------------------------------------------------------
//
// What plays in a video: title-api tells it, a minute and a half before it
// plays. The alternative player's videos (fMP4) it hears in the segments
// its relay passes on to the TV and loads ahead. A primary player's video
// doesn't pass it, and has its audio apart, a rendition per dub: the router
// tells title-api where the TV plays it and in which dub (POST
// /v1/music/play), and title-api loads that audio ahead itself; the dub is
// then part of the video's source. The status carries the music (see
// music_of): title-api is told and asked again at most every MUSIC_EVERY
// seconds, by a process of its own the status never waits for; the tick
// keeps telling it while no page asks for the status.
const MUSIC_EVERY = 10;

// The query of title-api's music of a video: the source (see music_source)
// hashed (FNV-1a), as title-api hashes it.
const music_query = (id, season, episode, source) => sprintf('title_id=%s&season=%d&episode=%d&source=%s',
	urlenc(id), +(season ?? 0), +(match(`${episode ?? ''}`, /^[0-9]+/)?.[0] ?? 0), hash(source ?? ''));

// The source of the music of what now plays: the stream's name (a dub's,
// which may be cut otherwise), and a primary player's dub the TV plays, as
// seen (null while unknown).
function music_source(now, seen) {
	if (now.fmp4)
		return now.source;
	let a = seen?.url == now.url ? seen.audio ?? seen.audio_name : null;
	return a != null ? `${now.source} | ${a}` : null;
}

// title-api's music of what now plays of source: { tracks, pending,
// received } (see GET /music), or { reason }.
function music_ask(now, source) {
	if (!MUSIC)
		return { reason: 'off' };
	if (source == null)
		return { reason: 'no_audio' };
	let r = api('/v1/music?' + music_query(now.id, now.season, now.episode, source), [503]);
	if (!r)
		return { reason: 'off' };
	return { tracks: r.tracks ?? [], pending: +(r.pending ?? 0), received: +(r.received ?? 0) };
}

// The music of what the TV of key plays (now) as last told (see music_ask),
// null before; told again in the background once MUSIC_EVERY old, by one
// process at a time (its lock mzl:<key> in kv, held 30 s at most).
function music_of(key, now) {
	if (!MUSIC)
		return { reason: 'off' };
	let source = music_source(now, tv_get(key, 'seen'));
	if (source == null)
		return null;
	let q = music_query(now.id, now.season, now.episode, source), x = json(kv_get('mz:' + key) ?? 'null'), t = time();
	if ((x?.q != q || t - x.t >= MUSIC_EVERY) &&
	    sql_do('INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE ' +
	           'SET value = excluded.value WHERE CAST(value AS INTEGER) < ?', 'mzl:' + key, t, t - 30))
		system(`${shq(sourcepath())} music ${shq(key)} </dev/null >/dev/null 2>&1 &`);
	return x?.q == q ? x.r : null;
}

// The process music_of left: tells title-api where the TV of key plays a
// primary player's video, and asks it for the music of what it plays.
function music_fetch(key) {
	let now = now_get(key), seen = tv_get(key, 'seen'), source = now?.id ? music_source(now, seen) : null;
	if (source != null) {
		let q = music_query(now.id, now.season, now.episode, source);
		let master = now.streams?.[now.stream]?.url;
		if (!now.fmp4 && master && seen.audio_name != null)
			api_post('/v1/music/play?' + q, {
				name: source, master, audio: seen.audio_name,
				position: +seen.pos + (seen.playing ? max(time() - seen.t, 0) : 0), playing: !!seen.playing,
			});
		kv_set('mz:' + key, { q, t: time(), r: music_ask(now, source) });
	}
	kv_set('mzl:' + key, 0);
}

// ---- watch progress --------------------------------------------------------
//
// Where each title was left off: a movie's position, or a series' last
// episode and the position in each of its episodes (titles, progress).
// Copied to flash every few minutes, or right away when a title or an
// episode starts or gets watched. Beyond MAX_TITLES and MAX_EPISODES the
// least recently watched ones are dropped; however long ago a title was
// watched, it is kept.

// The progress row of a movie ('') or an episode.
const ep_key = (season, episode) => season == null ? '' : `${season}:${episode}`;

// { id, title, poster, season, episode (a series' last), prefs, t } of a
// title; null if it was never watched.
function title_row(id) {
	let e = sql_row(`SELECT ${TITLE_COLS} FROM titles WHERE id = ?`, id);
	if (e)
		e.prefs = e.prefs != null ? json(e.prefs) : null;
	return e;
}

// { pos, dur, done, t } of a movie or an episode (see ep_key), or null.
function progress_row(id, ep) {
	let x = sql_row('SELECT pos, dur, done, t FROM progress WHERE id = ? AND ep = ?', id, ep);
	if (x)
		x.done = !!x.done;
	return x;
}

// Streams of a movie, or of an episode when season is given. A series
// never watched, or saved to watch, is told to title-api as played first:
// it marks the episodes of those (see /marking).
function streams(id, season, episode) {
	let first = !title_row(id) || !!sql_row('SELECT 1 AS x FROM saved WHERE id = ?', id);
	let r = api(`/v1/titles/${id}/playback-streams` + (season == null ? '' :
		`?season_number=${season}&episode_number=${urlenc(episode)}` +
		(first ? '&first_play=true' : '')));
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
		// Marked on a video of sdur seconds (see sections_of).
		sections: sections(s.sections), sdur: +(s.sectionsDurationSeconds ?? 0),
	}));
}

// The streams of a title, remembered per TV for 10 minutes: the UI lists the
// sources before playing and may retry with another, all from one provider
// request. force asks the provider again.
function title_streams(dev, id, season, episode, force) {
	let key = state_key(dev), c = tv_get(key, 'list');
	if (!force && c?.streams && c.id == id && c.season == season &&
	    c.episode == episode && time() - c.fetched < 600)
		return c;
	c = { id, season, episode, streams: streams(id, season, episode), fetched: time() };
	tv_set(key, 'list', c);
	return c;
}

// Copies the history to flash, into a new file renamed over KEEP, so a cut
// never leaves half of one. A failed copy waits for the next change or tick.
function keep() {
	let d = store(), tmp = `${KEEP}.${clock()[1]}`;
	let ok = d.run('ATTACH ? AS keep', tmp) != null;
	ok = ok && d.exec(`PRAGMA keep.journal_mode = OFF; BEGIN; ${HISTORY('keep')}
		INSERT INTO keep.titles (${TITLE_COLS}) SELECT ${TITLE_COLS} FROM titles;
		INSERT INTO keep.progress (${PROGRESS_COLS}) SELECT ${PROGRESS_COLS} FROM progress;
		INSERT INTO keep.settings (key, value) SELECT key, value FROM settings;
		INSERT INTO keep.saved (id, title, poster, t) SELECT id, title, poster, t FROM saved;
		COMMIT`) != null;
	if (!ok)
		d.exec('ROLLBACK');
	d.exec('DETACH keep');
	if (ok && rename(tmp, KEEP))
		kv_set('flushed', time());
	else
		unlink(tmp);
}

// The history changed: copied to flash when forced or FLUSH seconds after
// the last copy.
function changed(force) {
	kv_set('changed', time());
	if (force || time() - (kv_get('flushed') ?? 0) >= FLUSH)
		keep();
}

// A setting of the router, as the page last set it ('0' or '1'), kept on
// flash with the history; null while never set. autonext: the next episode
// starts by itself; autoskip: the recap and the intro are skipped.
function setting(k) {
	return sql_row('SELECT value FROM settings WHERE key = ?', k)?.value;
}

function set_setting(k, v) {
	sql_do('INSERT INTO settings (key, value) VALUES (?, ?) ' +
	       'ON CONFLICT (key) DO UPDATE SET value = excluded.value', k, v);
	changed(true);
}

// The settings as the page shows them: autonext is on unless turned off.
const settings = () => ({
	autonext: (setting('autonext') ?? '1') == '1',
	autoskip: setting('autoskip') == '1',
	// Never set on the router: the page may still have them of before.
	unset: setting('autonext') == null && setting('autoskip') == null,
});

// What a title is watched with, for its next start (see play): the source,
// the quality, and the dub and subtitles by name, since track ids differ
// between sources and episodes; subs '' is off. A kind of track the TV
// lists none of keeps what was there.
function watched_with(now, m, was) {
	// The tick's status (castctl's) has no labels: they are looked up as
	// with_now does, or a tick took subtitles that were on for off.
	let active = type => {
		let all = filter(m.tracks ?? [], t => t.type == type);
		if (!length(all))
			return null;
		let t = filter(all, t => t.active)[0];
		return t ? t.label ?? now.labels?.[t.name] ?? t.name : '';
	};
	let audio = active('AUDIO'), subs = active('TEXT');
	return {
		source: now.source ?? was?.source, quality: now.quality ?? was?.quality,
		audio: audio || was?.audio, subs: subs ?? was?.subs,
	};
}

// Records the position of now (what /play started) from the receiver's
// media status. Positions under a second are skipped: the receiver reports
// 0 while it loads a stream started further in.
function remember(now, m) {
	let finished = m.state == 'IDLE' && m.idleReason == 'FINISHED';
	let pos = +(m.time ?? 0), dur = +(m.duration ?? 0);
	if (!finished && (pos < 1 || index(['PLAYING', 'PAUSED', 'BUFFERING'], m.state) < 0))
		return;
	// Where a play would start again (see resume_at, prepare) and land
	// after its skips.
	cache_mark(now.c, starts_of(sections_of(now, dur), pos > 10 ? pos - 5 : 0, !!now.autoskip));
	let e = title_row(now.id), k = ep_key(now.season, now.episode);
	let x = progress_row(now.id, k), t = time();
	let important = !e || !x;
	if (now.season != null)
		important ||= e?.season != now.season || e?.episode != now.episode;
	// Only an end the receiver reached itself marks a title watched here
	// (see next_episode). A watched episode stays so while it is still near
	// the end: a poll can catch it right after the next one was started.
	let done = finished || (!!x?.done && dur > 0 && pos >= dur * DONE);
	important ||= !!x?.done != done;
	let w = watched_with(now, m, e?.prefs);
	important ||= sprintf('%J', w) != sprintf('%J', e?.prefs);
	if (finished)
		pos = dur;
	// A long pause shouldn't keep rewriting the same position.
	if (!important && x.pos == pos && x.dur == dur)
		return;
	sql_exec('BEGIN IMMEDIATE');
	// The title and the poster are for the recently watched list.
	sql_do(`INSERT INTO titles (${TITLE_COLS}) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET title = coalesce(excluded.title, title),
		poster = coalesce(excluded.poster, poster), season = excluded.season,
		episode = excluded.episode, prefs = excluded.prefs, t = excluded.t`,
		now.id, now.title, now.poster, now.season,
		now.season != null ? now.episode : null, w, t);
	sql_do(`INSERT INTO progress (${PROGRESS_COLS}) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (id, ep) DO UPDATE SET pos = excluded.pos, dur = excluded.dur,
		done = excluded.done, t = excluded.t`, now.id, k, pos, dur, done, t);
	sql_do('DELETE FROM progress WHERE id = ? AND ep NOT IN ' +
	       '(SELECT ep FROM progress WHERE id = ? ORDER BY t DESC LIMIT ?)',
	       now.id, now.id, MAX_EPISODES);
	if (sql_do('DELETE FROM titles WHERE id NOT IN ' +
	           '(SELECT id FROM titles ORDER BY t DESC LIMIT ?)', MAX_TITLES))
		sql_do('DELETE FROM progress WHERE id NOT IN (SELECT id FROM titles)');
	sql_exec('COMMIT');
	changed(important);
}

// Starting another episode of a series marks the last one watched if it was
// left near the end: people switch off during the credits, which may run
// for minutes. Movies just resume wherever they were stopped.
function next_episode(id, season, episode) {
	let e = title_row(id);
	if (e?.season == null)
		return;
	let l = ep_key(e.season, e.episode), x = progress_row(id, l);
	if (!x || x.done || l == ep_key(season, episode) ||
	    !(x.dur > 0 && x.pos >= x.dur * DONE))
		return;
	sql_do('UPDATE progress SET done = 1 WHERE id = ? AND ep = ?', id, l);
	changed(true);
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

// The volume (0..1) of the TV of key that what /play started was last played
// at: set from the page, or seen in the TV's status. A TV switched off and on
// again comes back at full volume.
const volume_get = key => kv_get('vol:' + key);

function volume_keep(key, v) {
	let was = volume_get(key);
	if (was == null || abs(was - v) >= 0.005)
		kv_set('vol:' + key, v);
}

// Puts the volume the TV of dev was last played at back once a play has
// started there; the status after that. A restarted box may tell that
// volume while it plays at full, and takes no level it already has, so
// the same one goes by a step off it.
function volume_back(dev, st) {
	let key = state_key(dev), v = volume_get(key);
	if (v == null || st?.volume == null)
		return st;
	if (abs(+st.volume - v) < 0.01)
		st = castctl_try(dev, 'volume', sprintf('%.0f', v * 100 + (v < 0.5 ? 1 : -1))) ?? st;
	else
		pause_note(key, sprintf('volume %.2f at the start, put back to %.2f', +st.volume, v));
	let r = castctl_try(dev, 'volume', sprintf('%.0f', v * 100));
	if (r?.volume == null || abs(+r.volume - v) >= 0.01)
		pause_note(key, sprintf('volume %.2f not taken at the start: %s', v,
		                        r ? sprintf('%.2f', +r.volume) : 'castctl failed'));
	return r ?? st;
}

// The episode after the one of season and episode of title id, crossing
// into the next season: { season, episode }; null after the last one or
// for one title-api does not list.
function episode_after(id, season, episode) {
	let S = api(`/v1/titles/${id}`).seasons ?? [], si = -1, ei = -1;
	for (let i, x in S)
		if (`${x.seasonNumber}` == `${season}`) {
			si = i;
			for (let j, e in x.episodes ?? [])
				if (`${e.episodeNumber}` == `${episode}`)
					ei = j;
		}
	if (si < 0 || ei < 0)
		return null;
	let s = S[si], e = s.episodes[ei + 1];
	if (!e) {
		s = S[si + 1];
		e = s?.episodes?.[0];
	}
	return e ? { season: +s.seasonNumber, episode: `${e.episodeNumber}` } : null;
}

// The next episode is prepared ahead (see watch_end), so that at the
// credits play_next only has to tell the TV: found, its master asked for
// (which opens the player's socket, as a playback start does), its format
// probed and its first segments loaded into the cache (see list_ahead).
// { url, of the episode it follows, t, when that started, fetched, when it
// ended, season and episode, null after the last one, and ready, what
// prepare made of it } of the TV of key. What is ready is good for ten
// minutes, as title_streams keeps streams.
const ahead_get = key => json(kv_get('next:' + key) ?? 'null');
const ready = x => !!x?.fetched && time() - x.fetched < 600;

// Leaves a process to prepare the episode after now ahead, unless one has
// or is at it (for two minutes at most: a failed one is tried again).
function ahead(key, now) {
	let x = ahead_get(key);
	if (x?.url == now.url && (x.fetched ? ready(x) : time() - x.t < 120))
		return;
	// Gone stale: the socket of its media says it ended.
	if (x?.ready?.m)
		media_end(x.ready.m);
	kv_set('next:' + key, { url: now.url, t: time() });
	system(`${shq(sourcepath())} ahead ${shq(key)} </dev/null >/dev/null 2>&1 &`);
}

// Records when now was last seen on the TV and, once it has ended, starts
// the next episode. Another process does that, so the status request or
// the tick neither waits for title-api and the TV nor fails with them; a
// lock keeps polls and ticks from starting it twice (two minutes at most:
// by then play_next has marked the episode, see ended).
function watch_end(now, st) {
	let key = state_key({ host: now.host }), m = st.media;
	if (m?.url == now.url && index(['PLAYING', 'BUFFERING', 'PAUSED'], m.state) >= 0) {
		// A pause the router did not send (see /cmd).
		let was = tv_get(key, 'seen'), sent = kv_get('pause:' + key);
		if (m.state == 'PAUSED' && was?.url == now.url && was.playing &&
		    (sent == null || +sent < was.t - 1))
			pause_note(key, sprintf('paused on the TV, not by the router, at %.0f of %.0f s (playing at %.0f s %d s ago)',
			                        +(m.time ?? 0), +(m.duration ?? 0), was.pos, time() - was.t));
		let track = type => filter(m.tracks ?? [], t => t.type == type && t.active)[0]?.name;
		let active = type => now.labels?.[track(type)];
		tv_set(key, 'seen', {
			url: now.url, pos: +(m.time ?? 0), dur: +(m.duration ?? 0),
			playing: m.state != 'PAUSED', t: time(),
			audio: active('AUDIO'), subs: active('TEXT'),
			// The audio's manifest NAME, which title-api follows (see "music").
			audio_name: track('AUDIO'),
		});
		// For the next headers refresh of a title-api media (see
		// media_headers), with no castctl call of its own.
		if (valid_media(now.media))
			media_set(now.media, 'pos', {
				pos: +(m.time ?? 0), dur: +(m.duration ?? 0), paused: m.state == 'PAUSED',
				buffering: m.state == 'BUFFERING', rate: +(m.rate ?? 1), at: time(),
			});
		// Set from the TV's remote or another app, past the start of a
		// playback, which volume_back sees to; within its first minute a
		// box that went back to its own volume gets the kept one again,
		// unless the page has set one since the start (volset:<key>, see
		// /cmd): its command takes a second, and a status meanwhile put the
		// volume back and forth between the old and the new one.
		let vol = volume_get(key), set = +(kv_get('volset:' + key) ?? 0) >= +(now.started ?? 0);
		if (st.volume != null && (time() - (now.started ?? 0) > 60 || set))
			volume_keep(key, +st.volume);
		else if (st.volume != null && vol != null && abs(+st.volume - vol) >= 0.01) {
			pause_note(key, sprintf('volume %.2f %d s after the start, put back to %.2f',
			                        +st.volume, time() - (now.started ?? 0), vol));
			castctl_try({ host: now.host }, 'volume', sprintf('%.0f', vol * 100));
		}
		let d = plan(key, now, m);
		// The episode after this one is prepared ahead, from AHEAD before
		// the credits or the end it starts at.
		if (d?.act == 'next' && d.in <= AHEAD || now.autonext && now.season != null &&
		    !now.next_at && +(m.duration ?? 0) > 0 && +m.duration - +(m.time ?? 0) <= AHEAD)
			ahead(key, now);
		return;
	}
	if (!ended(now, st, tv_get(key, 'seen')))
		return;
	let t = time();
	if (sql_do('UPDATE tvs SET next_lock = ? WHERE key = ? AND coalesce(next_lock, 0) < ?',
	           t, key, t - 120))
		system(`${shq(sourcepath())} next ${shq(key)} </dev/null >/dev/null 2>&1 &`);
}

// The position to resume a movie or an episode from; null to start over.
function resume_at(id, season, episode) {
	let x = progress_row(id, ep_key(season, episode));
	return x && !x.done && x.pos > 10 ? x.pos - 5 : null;
}

// Of the videos no TV plays (seen within three minutes), only the
// segments where a playback would start stay (see cache_mark): none of an
// episode or a movie watched to the end, none of one forgotten, none older
// than CACHE_DAYS. Files without a row, of fetches cut short, go too.
function cache_trim() {
	let playing = {}, t = time();
	for (let r in sql_rows('SELECT now, seen FROM tvs WHERE now IS NOT NULL')) {
		let now = json(r.now), seen = json(r.seen ?? 'null');
		if (now?.c && seen?.url == now.url && t - seen.t < 180)
			playing[now.c] = true;
	}
	for (let r in sql_rows('SELECT DISTINCT c FROM cache')) {
		let v = json(kv_get('c:' + r.c) ?? 'null');
		let x = v ? progress_row(v.id, ep_key(v.season, v.episode)) : null;
		let all = !v || !!x?.done;
		for (let y in sql_rows('SELECT key, keep, start, t FROM cache WHERE c = ?', r.c))
			if (y.t < t - CACHE_DAYS * 86400 || !playing[r.c] && (all || !y.keep))
				cache_drop(y.key);
		// An init segment alone serves nothing.
		if (!sql_row('SELECT 1 AS x FROM cache WHERE c = ? AND start IS NOT NULL', r.c))
			for (let y in sql_rows('SELECT key FROM cache WHERE c = ?', r.c))
				cache_drop(y.key);
	}
	for (let r in sql_rows("SELECT key FROM kv WHERE key LIKE 'c:%'"))
		if (!sql_row('SELECT 1 AS x FROM cache WHERE c = ?', replace(r.key, /^[a-z]+:/, '')))
			sql_do('DELETE FROM kv WHERE key = ?', r.key);
	for (let name in lsdir(CACHE) ?? []) {
		let st = stat(CACHE + name);
		if (st && st.mtime < t - 300 &&
		    !sql_row('SELECT 1 AS x FROM cache WHERE key = ?', name))
			unlink(CACHE + name);
	}
}

// Run by cron every minute: records the progress of what /play started
// while no page polls the status, e.g. with the phone locked. A TV that
// fails five checks in a row (switched off, or playing something else) is
// left alone until the next /play.
function tick() {
	for (let r in sql_rows('SELECT key, now FROM tvs WHERE now IS NOT NULL')) {
		let k = r.key, now = json(r.now);
		if (!now?.host || !now.url)
			continue;
		let w = tv_get(k, 'watch');
		let misses = w?.url == now.url ? w.misses : 0;
		if (misses >= 5)
			continue;
		let st = castctl_try({ host: now.host }, 'status'), m = st?.media;
		now = adopt(k, now.host, now, m);
		if (st)
			watch_end(now, st);
		if (m?.url == now.url)
			remember(now, m);
		// title-api follows a primary video's audio while told (see "music").
		if (m?.url == now.url && index(['PLAYING', 'BUFFERING'], m.state) >= 0)
			music_of(k, now);
		misses = m?.url == now.url && m.state != 'IDLE' ? 0 : misses + 1;
		if (misses != w?.misses || w?.url != now.url)
			tv_set(k, 'watch', { url: now.url, misses });
		// title-api keeps the playback socket open while the headers are
		// asked for, also while the receiver has enough buffered to fetch
		// nothing, and learns the position.
		if (now.media && misses == 0)
			media_headers(now.media, quality_digits(now.quality), m.time, true);
		// Playback ended (see above).
		if (misses == 5)
			media_end(now.media);
	}
	// The headers, positions and bytes of media not played for a day, what
	// /play started and the leveler's playlists of a day ago.
	let t = time();
	sql_do('DELETE FROM media WHERE t < ?', t - 86400);
	sql_do('DELETE FROM segments WHERE t < ?', t - 86400);
	sql_do('DELETE FROM played WHERE t < ?', t - 86400);
	sql_do('DELETE FROM drc WHERE t < ?', t - 86400);
	cache_trim();
	// Error files of proxied fetches the web server cut short.
	for (let name in lsdir('/tmp') ?? []) {
		let st = match(name, /^wrtcast\.err\.[0-9]+$/) ? stat('/tmp/' + name) : null;
		if (st && st.mtime < t - 60)
			unlink('/tmp/' + name);
	}
	// The last position before playback stopped would otherwise not reach
	// flash.
	let flushed = kv_get('flushed') ?? 0;
	if ((kv_get('changed') ?? 0) > flushed && t - flushed >= FLUSH)
		keep();
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
// pv marks a seek preview's fetch to the relay, which keeps it out of the
// playback it reports.
function fetch_cmd(url, h, err, m, pv) {
	let out = u => `-O - ${shq(u)} 2>${err ? shq(err) : '/dev/null'}`;
	let cmd = `uclient-fetch ${err ? '' : '-q '}-T 20`;
	let lines = valid_headers(h) ? split(h, '\n') : [];
	if (valid_media(m) && substr(url, 0, length(API) + 1) != API + '/') {
		let dyn = media_headers(m), relay = media_get(m, 'hdr')?.proxy;
		if (relay)
			return `${cmd} ${out(API + relay + '?u=' + urlenc(url) + (pv ? '&pv=1' : ''))}`;
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
	st.settings = settings();
	let key = state_key(dev), now = adopt(key, host_of(dev), now_get(key), st.media);
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
		now_set(key, now);
	}
	st.now = {
		id: now.id, stream: now.stream, title: now.title, poster: now.poster,
		season: now.season, episode: now.episode,
		quality: now.quality ?? 'auto', qualities: now.qualities ?? [],
		source: now.source ?? null,
		sources: map(now.streams ?? [], s => s.name),
		// Whether the volume leveler is on, and whether the source allows it.
		drc: !!now.drc, drc_ok: !!now.drc_ok,
		// Whether the router starts the next episode once this one ends
		// (or its credits start).
		autonext: !!now.autonext,
		// Whether the router skips the recap and the intro, the sections on
		// the video the TV plays (see sections_of), and the ones it skipped
		// (by index), which it leaves be once they are rewound into.
		autoskip: !!now.autoskip,
		sections: sections_of(now, +(st.media.duration ?? 0)),
		skipped: skipped(key, now),
		// The music of the video, as last told (see music_of).
		music: music_of(key, now),
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
// drc_playlist). c, the video of a play (see cache_id), goes to the variant
// of q, and in its media playlist the segments get their cache keys (see
// seg_keys).
function rewrite_playlist(body, base, q, h, drc, m, c) {
	let fix = u => match(u, /^(data|skd):/) ? u : purl(resolve(base, u), null, null, h, m);
	let lines = map(split(body, '\n'), line => rtrim(line, '\r'));
	lines = keep_variants(lines, l => playable(match(l, /CODECS="([^"]*)"/)?.[1]));
	if (q)
		lines = keep_variants(lines, l => +(match(l, /RESOLUTION=([0-9]+)x/)?.[1] ?? 0) == q);
	lines = unique_audio_langs(lines);
	let master = index(body, '#EXT-X-STREAM-INF') >= 0;
	let keys = c && !master ? seg_keys(lines, c) : [], out = [];
	for (let i, l in lines) {
		let k = keys[i] ? `&k=${keys[i]}` : '';
		if (l == '') {
			push(out, l);
		} else if (substr(l, 0, 1) != '#') {
			push(out, fix(trim(l)) + k + (c && q && master ? `&c=${c}` : ''));
		} else {
			let flag = drc && match(l, /^#EXT-X-MEDIA:(.*,)?TYPE=AUDIO(,|$)/) ? '&drc=1' : '';
			push(out, replace(l, /URI="([^"]*)"/g, (all, u) => `URI="${fix(u)}${flag}${k}"`));
		}
	}
	return join('\n', out);
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
	sql_do('INSERT OR REPLACE INTO drc (key, meta, t) VALUES (?, ?, ?)', key,
		{ segs, durs, h: valid_headers(h) ? h : null, m: valid_media(m) ? m : null }, time());
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
	mkdir(DRC);
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
		let m = match(f, /^([0-9a-f]{8})\.([0-9]+)\./);
		if (!m)
			continue;
		let st = stat(DRC + f);
		if (m[1] == key ? +m[2] < i - 2 || +m[2] > i + 2 : st && st.mtime < time() - 600)
			unlink(DRC + f);
	}
}

function drc_segment(key, i) {
	let meta = sql_row('SELECT meta FROM drc WHERE key = ?', key)?.meta;
	meta = meta != null ? json(meta) : null;
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

// Records the bytes of a segment of media m streamed since start (a clock())
// and the milliseconds that took, for title-api (see media_headers).
function count_bytes(m, n, start) {
	let end = clock();
	sql_do('INSERT INTO segments (media, bytes, ms, t) VALUES (?, ?, ?, ?)', m, n,
		int((end[0] - start[0]) * 1000 + (end[1] - start[1]) / 1000000), time());
}

// pv marks the fetches of the seek preview (the web UI's frames over the
// seek bar): not the receiver's, they don't count as its segments. c is
// the video of a play's playlists and k a segment's cache key (see
// "segment cache"): a cached segment goes out from the cache, another one
// is kept as it streams.
function proxy(name, u, q, h, drc, m, pv, c, k) {
	let start = clock();
	let url = proxy_target(u);
	if (!url)
		fail(400, 'bad url');
	if (h != null && !valid_headers(h))
		fail(400, 'bad headers');
	if (m != null && !valid_media(m))
		fail(400, 'bad media');
	c = match(c ?? '', /^[0-9a-f]{8}$/) ? c : null;
	k = match(k ?? '', CACHE_KEY) ? k : null;
	let e = ext(name), err = ERR + clock()[1];
	let hit = k ? cache_get(k) : null, f = hit ? open(CACHE + k) : null;
	if (f) {
		media_head(200, MIME[e] ?? 'application/octet-stream');
		let n = 0;
		for (let b = f.read(65536); b != null && b != ''; b = f.read(65536)) {
			stdout.write(b);
			n += length(b);
		}
		f.close();
		// Loaded another day, or by another play: the player loads it now.
		if (valid_media(m) && !pv && hit.media != m)
			count_bytes(m, n, start);
		exit(0);
	}
	let pipe = popen(fetch_cmd(url, h, err, m, pv));
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
			rewrite_playlist(body, url, match(q ?? '', /^[0-9]{1,5}$/) ? +q : null, h, drc, m, c);
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
	let part = CACHE + k + '.' + clock()[1];
	let tee = k && !pv && cache_room() ? open(part, 'w') : null;
	let n = 0;
	while (chunk != null && chunk != '') {
		stdout.write(chunk);
		if (tee)
			tee.write(chunk);
		n += length(chunk);
		chunk = pipe.read(65536);
	}
	let ok = pipe.close() == 0;
	if (tee) {
		tee.close();
		if (ok)
			cache_put(k, part, n, m, false);
		else
			unlink(part);
	}
	// The media's segments, as the player counts the fragments it loaded.
	if (valid_media(m) && !pv && !(e in ['vtt', 'webvtt', 'key']))
		count_bytes(m, n, start);
	exit(0);
}

// Absolute proxy base for URLs handed to the receiver. play_next has no
// request and takes the base of the last play.
let next_base = null;
// When play sent the TV what to load, for play_next's note.
let load_at = null;
function public_base() {
	return next_base ?? rtrim(conf('public_url', '') || `http://${getenv('HTTP_HOST')}`, '/');
}

// Poster images sit behind the provider's anti-bot page; title-api fetches
// them with its session.
// A poster the page sent back: only our own proxy paths, so the UI never
// loads arbitrary URLs; else null.
const own_poster = u => length(u ?? '') <= 4096 && match(u ?? '', /^\/api\/p\/[^ ]+$/) ? u : null;

function poster(u) {
	if (!u)
		return null;
	let via_api = `${API}/v1/posters?url=${urlenc(u)}`;
	return PROXY ? purl(via_api, null, 'poster.jpg') : via_api;
}

// What a play needs before the TV is told (see start): the stream, its
// master and format, the quality, where it starts and castctl's arguments;
// p as /play takes it, keep as play_next does. The next episode is
// prepared a while before it starts (see list_ahead).
function prepare(dev, p, keep) {
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
	if (p.rate != null && !match(p.rate, /^[0-9](\.[0-9]{1,2})?$/))
		fail(400, 'invalid rate');
	if (p.quality != null && !match(p.quality, /^(auto|[0-9]{3,4}p)$/))
		fail(400, 'invalid quality');
	if (p.style != null && !valid_style(p.style))
		fail(400, 'invalid subtitle style');
	if (p.source != null && (length(p.source) > 100 || match(p.source, /[[:cntrl:]]/)))
		fail(400, 'invalid source');
	for (let k in ['drc', 'autonext', 'autoskip'])
		if (p[k] != null && !match(p[k], /^[01]$/))
			fail(400, `invalid ${k}`);
	// A title watched before starts again with what it was watched with
	// (see watched_with); a replay (reuse) tells all of it itself, and a
	// source or a stream asked for wins.
	let pref = p.reuse == '1' ? null : title_row(id)?.prefs;
	if (pref?.source != null && p.source == null && p.stream == null)
		p.source = pref.source;
	if (match(pref?.quality ?? '', /^(auto|[0-9]{3,4}p)$/))
		p.quality = pref.quality;
	let key = state_key(dev), last = now_get(key);
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
	// A title started from its poster for the first time asks for the
	// source, as its page does, when there is a choice.
	if (p.auto == '1' && pref?.source == null && p.source == null && p.stream == null &&
	    length(list) > 1)
		reply(409, { error: 'choose a source', sources: map(list, x => x.name) });
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
	// Subtitles are off unless picked, so an off one needs nothing.
	if (pref?.audio && p.audio == null)
		p.audio = filter(s.audio, a => a.name == pref.audio)[0]?.id;
	let ps = pref?.subs ? filter(s.subs, t => t.url && t.name == pref.subs)[0] : null;
	if (ps && p.subs == null)
		p.subs = 'sub' + ps.id;
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
	let autoskip = p.autoskip != null ? p.autoskip == '1' : setting('autoskip') == '1';
	// Tells title-api the quality and the position before the CDN sees
	// a request; what the TV told of an earlier play of the media doesn't
	// count.
	if (m) {
		sql_do('UPDATE media SET pos = NULL WHERE id = ?', m);
		media_headers(m, quality_digits(quality), start != null ? +start : null, true);
	}
	// With autoskip a start within the recap or the intro (an episode from
	// its start) goes past them at once, by the sections on this video if
	// title-api tells them (which tells it the new position too), else by
	// the marked ones. A replay (reuse) stays where it was.
	let lead = null, sections = (m ? media_get(m, 'hdr')?.sections : null) ?? s.sections ?? [];
	if (autoskip && p.reuse != '1') {
		lead = skips_at(sections, +(start ?? 0));
		if (lead) {
			start = lead.to;
			if (m)
				media_headers(m, quality_digits(quality), start, true);
		}
	}
	// Probed once per source, on the variant this play picks; another
	// episode of the source the last play was of is taken to be alike.
	if (body != null) {
		if (last?.id == id && last.source == s.name && last.fmp4 != null) {
			s.fmp4 = last.fmp4;
			s.drc = last.drc_ok;
		} else {
			s.fmp4 = is_fmp4(s.url, body, h, width, m);
			s.drc = PROXY && !s.fmp4 && drc_supported(s.url, body, h, m);
		}
	}
	let drc = p.drc == '1' && !!s.drc;
	// The video's segments are cached (see "segment cache"), of a known
	// variant: with auto quality the TV picks.
	let c = PROXY && width ? cache_id(id, season, episode, s.name, width) : null;
	if (c)
		kv_set('c:' + c, { id, season, episode });
	let url = PROXY ? purl(s.url, public_base(), null, h, m) +
		(width ? `&q=${width}` : '') + (drc ? '&drc=1' : '') + (c ? `&c=${c}` : '') : s.url;
	// Subtitles are separate WebVTT files, not part of the HLS manifest,
	// so they are side-loaded; the receiver names them "sub<id>".
	let args = ['play', url], labels = {};
	if (+(start ?? 0) > 0)
		push(args, '-p', sprintf('%.1f', +start));
	if (s.fmp4)
		push(args, '-f', 'fmp4');
	// Signed subtitle URLs can be ~2 KB each through the proxy, too much
	// for the 64 KB LOAD message, so the receiver gets short /sub/<tv>/<n>
	// links that look the real URL up in what is written beforehand.
	let picked = pick_subs(s.subs);
	for (let i, t in picked) {
		push(args, '-s', PROXY ? `${public_base()}/api/sub/${key}/${i}.vtt` : t.url,
		     t.lang || 'und', 'sub' + t.id);
		labels['sub' + t.id] = t.name;
	}
	for (let t in s.audio)
		labels[t.id] = t.name;
	if (p.style != null)
		push(args, '-y', p.style);
	return {
		key, id, season, episode, n, s, list, fetched, m, h, c, url, width, quality, quals,
		start, lead, drc, autoskip, sections, args, labels, subs: map(picked, t => t.url),
		p: { title: p.title, poster: p.poster, style: p.style, autonext: p.autonext,
			audio: p.audio, subs: p.subs, rate: p.rate },
	};
}

// Tells the TV to play what prepare made ready (r) and makes it the state
// of the TV.
function start(dev, r) {
	let key = r.key, last = now_get(key), p = r.p, url = r.url, m = r.m, lead = r.lead;
	if (r.season != null)
		next_episode(r.id, r.season, r.episode);
	tv_set(key, 'subs', { urls: r.subs, headers: r.h, media: m });
	// The state goes first: uhttpd may kill us (script_timeout, or the
	// phone went away) while castctl waits for playback, and the TV then
	// plays this stream all the same. A failed play puts the last one back.
	let now = {
		id: r.id, stream: r.n, source: r.s.name, url, host: host_of(dev), media: m, c: r.c,
		title: p.title ?? null, season: r.season,
		episode: r.episode, labels: r.labels, quality: r.quality,
		qualities: map(r.quals, v => v.label),
		drc: r.drc, drc_ok: !!r.s.drc, fmp4: !!r.s.fmp4,
		// Kept for the next episode the router starts itself (see play_next).
		// Without autonext a replay of the same title (an older page)
		// keeps what was set.
		autonext: p.autonext != null ? p.autonext == '1' : settings().autonext,
		// The recap and the intro are skipped by the router (see plan),
		// as the next episode is started.
		autoskip: r.autoskip,
		sections: r.s.sections ?? [], sdur: r.s.sdur ?? 0,
		style: p.style ?? null,
		streams: r.list, fetched: r.fetched, started: time(),
		poster: own_poster(p.poster),
	};
	let put_back = () => {
		now_set(key, last);
		busy_set(key, false);
	};
	busy_set(key, true);
	now_set(key, now);
	// What the start went past counts as skipped (see plan).
	if (lead) {
		let done = {};
		for (let i in lead.idx)
			done[i] = true;
		plan_set(key, { url, done });
	}
	sql_do('INSERT OR REPLACE INTO played (url, now, t) VALUES (?, ?, ?)', url, now, time());
	undo = put_back;
	load_at = clock_now();
	let st = castctl(dev, ...r.args);
	undo = null;
	busy_set(key, false);
	// The socket of what this replaces says it ended.
	if (last?.media != m)
		media_end(last?.media);
	// castctl found the TV at a new address from DHCP: the state moves to
	// its key, and the old key gets back what it had.
	if (host_of(dev) != now.host) {
		now.host = host_of(dev);
		now_set(state_key(dev), now);
		put_back();
	}
	for (let k in ['audio', 'subs'])
		if (p[k] != null)
			st = castctl_try(dev, k, p[k]) ?? st;
	// A replay keeps the speed (see the page's replay).
	if (p.rate != null && +p.rate != 1)
		st = castctl_try(dev, 'speed', p.rate) ?? st;
	st = volume_back(dev, st);
	st = with_now(st, dev);
	// The TV plays: title-api learns the duration and the position now.
	if (m)
		media_headers(m, quality_digits(r.quality), null, true);
	return st;
}

// Starts a movie or an episode; play also switches the quality or the source
// of what is on: with reuse=1 it takes the streams from the last play of the
// same title if they are still fresh (title-api keeps synthetic masters for
// 6 hours), and start/audio/subs restore the position and tracks. keep, from
// play_next: { audio, subs } track names to pick when audio/subs are not
// given.
function play(dev, p, keep) {
	return start(dev, prepare(dev, p, keep));
}

// What /play would be asked for the episode e after now: the source,
// quality, leveler, subtitle style, dub and subtitles of now.
const next_params = (now, e) => ({
	id: now.id, title: now.title, poster: now.poster,
	season: `${e.season}`, episode: e.episode,
	source: now.source, quality: now.quality, drc: now.drc ? '1' : '0',
	autonext: '1', autoskip: now.autoskip ? '1' : '0', style: now.style,
});

// What prefetch needs of what the TV of now plays.
function now_ready(now) {
	let s = now.streams?.[now.stream];
	return { c: now.c, m: now.media, s, h: s?.headers ?? '',
		width: filter(s?.variants ?? [], v => v.label == now.quality)[0]?.width };
}

// Loads into the cache the segments of CACHE_KEEP seconds from each of
// starts of what prepare made ready (r), one at a time through title-api as
// the TV would load them: the variant's media playlist, its init segment and
// the segments. A failure ends it. Returns the segments loaded.
function prefetch(r, starts) {
	if (!r?.c || !r.s?.url || !r.width || !valid_media(r.m) || !length(starts) || !cache_room())
		return 0;
	let inf = null, uri = null;
	for (let l in split(fetch_text(r.s.url, r.h, r.m) ?? '', '\n')) {
		l = trim(l);
		if (substr(l, 0, 18) == '#EXT-X-STREAM-INF:')
			inf = l;
		else if (inf != null && l != '' && substr(l, 0, 1) != '#') {
			if (playable(match(inf, /CODECS="([^"]*)"/)?.[1]) &&
			    +(match(inf, /RESOLUTION=([0-9]+)x/)?.[1] ?? 0) == r.width) {
				uri = l;
				break;
			}
			inf = null;
		}
	}
	let url = uri ? resolve(r.s.url, uri) : null, pl = url ? fetch_text(url, r.h, r.m) : null;
	if (substr(pl ?? '', 0, 7) != '#EXTM3U')
		return 0;
	let lines = map(split(pl, '\n'), l => rtrim(l, '\r'));
	let keys = seg_keys(lines, r.c), n = 0;
	let wanted = x => x[2] == 'i' || length(filter(starts, w =>
		+x[4] / 1000.0 > w && +x[3] / 1000.0 < w + CACHE_KEEP));
	for (let i, l in lines) {
		let k = keys[i], x = k ? match(k, CACHE_KEY) : null;
		if (!x || !wanted(x))
			continue;
		if (cache_get(k)) {
			sql_do('UPDATE cache SET keep = 1 WHERE key = ?', k);
			continue;
		}
		let u = resolve(url, substr(l, 0, 1) == '#' ? match(l, /URI="([^"]*)"/)?.[1] ?? '' : trim(l));
		let f = CACHE + k + '.' + clock()[1], t0 = clock();
		let size = system(`${fetch_cmd(u, r.h, null, r.m)} >${shq(f)}`) == 0 ? stat(f)?.size : 0;
		if (!(size > 0)) {
			unlink(f);
			break;
		}
		cache_put(k, f, size, r.m, true);
		if (x[2] != 'i')
			count_bytes(r.m, size, t0);
		n++;
	}
	return n;
}

// The process ahead left: prepares the episode after now as play_next
// would start it, and loads its first segments. A failure ends it (see
// fail), and play_next does it all then.
function list_ahead(key) {
	let now = now_get(key), x = ahead_get(key), seen = tv_get(key, 'seen');
	if (!now?.url || now.season == null || x?.url != now.url || x.fetched)
		return;
	let e = episode_after(now.id, now.season, now.episode);
	x = { url: now.url, t: x.t, season: e?.season, episode: e?.episode };
	if (e) {
		next_base = match(now.url, /^(.*)\/api\/p\//)?.[1];
		x.ready = prepare({ host: now.host }, next_params(now, e),
		                  { audio: seen?.audio, subs: seen?.subs });
	}
	x.fetched = time();
	if (now_get(key)?.url != now.url) {
		if (x.ready?.m)
			media_end(x.ready.m);
		return;
	}
	kv_set('next:' + key, x);
	if (x.ready)
		pause_note(key, sprintf('next episode %d:%s prepared in %.0f s, %d segments loaded ahead',
		                        e.season, e.episode, time() - x.t, prefetch(x.ready,
		                        starts_of(x.ready.sections, +(x.ready.start ?? 0), x.ready.autoskip))));
}

// Plays the episode after the one that has just ended on the TV of key (see
// ended), crossing into the next season, with the source, quality, leveler,
// subtitle style, dub and subtitles of the last one.
function play_next(key) {
	let t0 = clock_now(), now = now_get(key), seen = tv_get(key, 'seen');
	let dev = { host: now?.host }, st = now?.host ? castctl_try(dev, 'status') : null;
	if (!st || !ended(now, st, seen) && !at_credits(now, st)) {
		// Not over after all: polls and ticks may start it later.
		sql_do('UPDATE tvs SET next_lock = NULL WHERE key = ?', key);
		return;
	}
	// One try per episode: a failed play puts this state back, and a
	// failure here leaves it as well.
	now.next_at = time();
	now_set(key, now);
	// Prepared ahead (see list_ahead), it is only started.
	let x = ahead_get(key), fresh = x?.url == now.url && ready(x), e;
	if (fresh)
		e = x.season != null ? { season: x.season, episode: x.episode } : null;
	else
		e = episode_after(now.id, now.season, now.episode);
	if (!e)
		return; // the last episode
	next_base = match(now.url, /^(.*)\/api\/p\//)?.[1];
	if (fresh && x.ready) {
		start(dev, x.ready);
	} else {
		if (x?.ready?.m)
			media_end(x.ready.m);
		play(dev, next_params(now, e), { audio: seen?.audio, subs: seen?.subs });
	}
	pause_note(key, sprintf('next episode %d:%s, %s, sent to the TV after %.1f s, playing after %.1f s',
	                        e.season, e.episode, fresh && x.ready ? 'prepared ahead' : 'not prepared',
	                        load_at - t0, clock_now() - t0));
}

// The process plan left (token): sleeps until what it waits for is due,
// checks the TV and does it, and waits for the next thing within HORIZON
// seconds. A newer plan (another token) or another stream on the TV ends
// it.
function wake(key, token) {
	for (let n = 0; n < 10; n++) {
		let p = plan_get(key);
		if (p?.token != token)
			return;
		// The segments a skip lands on are loaded into the cache before
		// it, once per skip with time for it, so that the TV has them at once.
		if (p.act == 'skip' && p.to != null && !p.pre?.[p.i] && p.at - clock_now() > 10) {
			p.pre ??= {};
			p.pre[p.i] = true;
			plan_set(key, p);
			let now = now_get(key);
			if (now?.url == p.url)
				prefetch(now_ready(now), [p.to]);
			continue;
		}
		let wait = p.at - clock_now();
		if (wait > 0)
			sleep(int(min(wait, HORIZON) * 1000));
		p = plan_get(key);
		let now = now_get(key);
		if (p?.token != token || !now?.host || now.url != p.url)
			return;
		let dev = { host: now.host }, st = castctl_try(dev, 'status'), m = st?.media;
		let d = m?.url == now.url ? due(now, m, p.done) : null;
		if (d && d.in > 0.5 && d.in <= HORIZON) {
			p.i = d.i;
			p.act = d.act;
			p.to = d.to;
			p.at = clock_now() + d.in;
			plan_set(key, p);
			continue;
		}
		if (!d || d.in > 0.5) {
			p.token = null;
			plan_set(key, p);
			return;
		}
		if (d.act == 'next') {
			p.token = null;
			plan_set(key, p);
			let t = time();
			if (sql_do('UPDATE tvs SET next_lock = ? WHERE key = ? AND coalesce(next_lock, 0) < ?',
			           t, key, t - 120))
				play_next(key);
			return;
		}
		for (let i in d.idx)
			p.done[i] = true;
		p.at = clock_now();
		plan_set(key, p);
		pause_note(key, sprintf('autoskip seek from %.0f to %.0f s (%s)', +(m.time ?? 0), d.to, m.state));
		castctl_try(dev, 'seek', sprintf('%.1f', d.to));
		// A seek reaches title-api at once (see /cmd).
		if (valid_media(now.media))
			media_headers(now.media, quality_digits(now.quality), d.to, true);
	}
}

// title-api's answers by title, as kept in kv (lost with a reboot):
// { last: the provider's last episode, null if not told, seasons: its
// episode numbers, [[season, [episode, …]], …], as last told, next: TMDB's
// next episode, if the series is coming back, t: when told, movie: not a
// series } (see series_updates).
function kept_updates() {
	let u = kv_get('updates');
	u = u != null ? json(u) : null;
	return type(u) == 'object' ? u : {};
}

// What a card names of a series once the episode watched last (r: its
// season, episode, pos, dur, done) is watched or left past 90 %: the one
// after it on the provider, as told last (x: kept_updates'), or false after
// its last; null while the episode isn't watched or the provider's episodes
// are not known.
function up_next(r, x) {
	if (r?.season == null || !(r.done || r.dur > 0 && r.pos >= r.dur * 0.9) || !x?.seasons)
		return null;
	let all = [];
	for (let s in x.seasons)
		for (let n in s[1])
			push(all, { season: s[0], episode: n });
	for (let i, y in all)
		if (y.season == r.season && y.episode == `${r.episode}`)
			return all[i + 1] ?? false;
	return null;
}

// A row of the lists with done a boolean and upnext (see up_next) of the
// answers of title-api kept (u).
const with_next = u => r => ({ ...r, done: !!r.done, upnext: up_next({ ...r, done: !!r.done }, u[r.id]) });

// The titles saved to watch, latest saved first, with the position of
// those watched as in the recently watched list.
function saved_rows() {
	return map(sql_rows(`SELECT s.id, coalesce(s.title, t.title) AS title,
		coalesce(s.poster, t.poster) AS poster, s.t, t.season, t.episode, p.pos, p.dur, p.done
		FROM saved s LEFT JOIN titles t ON t.id = s.id LEFT JOIN progress p
		ON p.id = s.id AND p.ep = CASE WHEN t.season IS NULL THEN ''
		ELSE t.season || ':' || t.episode END ORDER BY s.t DESC`), with_next(kept_updates()));
}

function recent_rows() {
	return map(sql_rows(`SELECT t.id, t.title, t.poster, t.t, t.season,
		t.episode, p.pos, p.dur, p.done FROM titles t JOIN progress p
		ON p.id = t.id AND p.ep = CASE WHEN t.season IS NULL THEN ''
		ELSE t.season || ':' || t.episode END
		WHERE t.title IS NOT NULL ORDER BY t.t DESC`), with_next(kept_updates()));
}

// ---- saved titles ------------------------------------------------------------
//
// Titles saved to watch are listed under the recently watched ones and
// kept on flash with the history, until removed. A series saved has
// title-api mark the episode it would start with, which the provider may
// leave unmarked (see /marking), as a process of its own: title-api loads
// the provider's pages for it.

// The episode a series would start with, as the page's play button has it:
// the last one watched, or the one after it (next) once that is watched or
// left past 90 %, else the first; { season, episode, next }, or null after
// the last. strict: null too when the last one watched isn't of S.
function start_episode(id, S, strict) {
	let all = [], e = title_row(id), i = -1;
	for (let x in S)
		for (let y in x.episodes ?? [])
			push(all, { season: +x.seasonNumber, episode: `${y.episodeNumber}` });
	if (e?.season != null)
		for (let j, x in all)
			if (x.season == e.season && x.episode == e.episode)
				i = j;
	if (i < 0)
		return strict && e?.season != null ? null : all[0];
	let x = progress_row(id, ep_key(e.season, e.episode));
	if (!(x && (x.done || x.dur > 0 && x.pos >= x.dur * 0.9)))
		return all[i];
	return all[i + 1] ? { ...all[i + 1], next: true } : null;
}

// What the play button of a title's page would start, for /play with
// auto=1 from a poster: sets p's season and episode of a series by
// start_episode, of the provider's episodes as kept (see kept_updates) or,
// when those don't tell it, asked for; a movie is left as is, prepare
// resuming it.
function auto_episode(p) {
	let id = title_id(p), e = title_row(id), x = kept_updates()[id];
	if (e ? e.season == null : x?.movie)
		return;
	let ep = start_episode(id, map(x?.seasons ?? [], s => ({
		seasonNumber: s[0], episodes: map(s[1], n => ({ episodeNumber: n })),
	})), true);
	if (!ep) {
		let r = api(`/v1/titles/${id}`);
		if (r.mediaType != 'MEDIA_TYPE_SERIES')
			return;
		ep = start_episode(id, r.seasons ?? []);
		if (!ep)
			fail(409, 'Сериал досмотрен');
	}
	p.season = `${ep.season}`;
	p.episode = ep.episode;
	// The next one from its start, as the page has it.
	if (ep.next && p.start == null)
		p.start = '0';
}

// Has title-api mark the episode a series saved would start with, while
// the marking is on (wrtcast mark).
function mark_saved(id) {
	let m = api_try('/v1/marking');
	if (!m?.enabled || !m.keySet)
		return;
	let r = api(`/v1/titles/${id}`);
	let e = r.mediaType == 'MEDIA_TYPE_SERIES' ? start_episode(id, r.seasons ?? []) : null;
	if (e)
		api_post(`/v1/titles/${id}/marking`, { seasonNumber: e.season, episodeNumber: e.episode });
}

// ---- series updates ----------------------------------------------------------
//
// The main page's badges of series (see /updates): a new episode on the
// provider, or the next one TMDB has a date for. The provider is asked of
// the series watched (title-api keeps a series' episodes for 5 minutes and
// asks it of them one after another), TMDB alone of those saved and never
// watched.

const UPDATES_MAX = 30;  // series asked of title-api at most, of each kind
const UPDATES_TTL = 300; // seconds title-api's answer of a series is kept

// An episode number's leading number ("10", "1-2"), to tell their order.
const ep_num = e => +(match(`${e}`, /^[0-9]+/)?.[0] ?? 0);
const ep_after = (a, b) => a.season > b.season ||
	a.season == b.season && ep_num(a.episode) > ep_num(b.episode);

// The provider's last episode of each series watched when /updates first
// told it, kept on flash with the history: an episode is new only after
// it, so that one that was there all along, left unwatched, isn't.
function known_episodes() {
	let k = setting('known');
	k = k != null ? json(k) : null;
	return type(k) == 'object' ? k : {};
}

// What is new of the series watched (latest first) and saved: { updates:
// { <id>: { new, next } }, partial }. new: the episode watched last is
// watched, and the provider has one after it that came after it was first
// seen there (see known); next: { season, episode, date, title } of TMDB,
// whether or not it has come. partial: the provider's episodes of some
// came too late, to be asked again.
function series_updates() {
	let watched = map(sql_rows(`SELECT id FROM titles WHERE season IS NOT NULL
		ORDER BY t DESC LIMIT ${UPDATES_MAX}`), r => r.id);
	let others = map(sql_rows(`SELECT id FROM saved WHERE id NOT IN (SELECT id FROM titles)
		ORDER BY t DESC LIMIT ${UPDATES_MAX}`), r => r.id);
	let u = kept_updates(), now = time(), still = {};
	let due = x => !x || !x.movie && (now - x.t >= UPDATES_TTL || x.t > now);
	let ask = filter(watched, id => due(u[id]) || !u[id].movie && !u[id].last);
	let upcoming = filter(others, id => due(u[id]));
	let partial = false;
	if (length(ask) || length(upcoming)) {
		let r = api_post('/v1/series/updates', { titleIds: ask, upcomingTitleIds: upcoming });
		let j = r.status == 200 ? json(r.out) : null, told = {};
		for (let x in j?.updates ?? []) {
			let id = x.titleId, n = x.nextEpisode;
			told[id] = true;
			u[id] = {
				// Not told in time: as told before, if it was.
				last: x.lastEpisode ? { season: +x.lastSeason, episode: x.lastEpisode } : u[id]?.last,
				seasons: length(x.seasons) ? map(x.seasons, s => [+s.seasonNumber, s.episodeNumbers ?? []]) :
					u[id]?.seasons,
				next: x.status == 'TMDB_STATUS_RETURNING' && n?.airDate ? {
					season: +n.seasonNumber, episode: +n.episodeNumber,
					date: n.airDate, title: n.name || null,
				} : null,
				t: now,
			};
			if (!x.lastEpisode && index(ask, id) >= 0) {
				partial = true;
				u[id].t = 0;
			}
		}
		// What title-api leaves out is no series; nothing told is asked
		// again next time.
		if (j)
			for (let id in [...ask, ...upcoming])
				if (!told[id])
					u[id] = { movie: true, t: now };
	}
	let out = {}, upnext = {}, known = known_episodes(), was = sprintf('%J', known);
	for (let id in [...watched, ...others]) {
		let x = u[id];
		if (!x)
			continue;
		still[id] = x;
		if (x.movie)
			continue;
		let e = index(watched, id) >= 0 ? title_row(id) : null, fresh = false;
		if (e)
			upnext[id] = up_next({ ...e, ...progress_row(id, ep_key(e.season, e.episode)) }, x);
		if (e && x.last) {
			known[id] ??= x.last;
			let p = progress_row(id, ep_key(e.season, e.episode));
			fresh = !!p && (p.done || p.dur > 0 && p.pos >= p.dur * 0.9) &&
				ep_after(x.last, e) && ep_after(x.last, known[id]);
		}
		if (fresh || x.next)
			out[id] = { new: fresh, next: x.next };
	}
	for (let id in keys(known))
		if (index(watched, id) < 0)
			delete known[id];
	kv_set('updates', sprintf('%J', still));
	if (sprintf('%J', known) != was)
		set_setting('known', sprintf('%J', known));
	return { updates: out, upnext, partial };
}


const marking = r => ({
	enabled: !!r?.enabled, key: !!r?.keySet, hint: r?.keyHint || null, error: r?.error || null,
});

const tmdb = r => ({ key: !!r?.keySet, hint: r?.keyHint || null, error: r?.error || null });

// A picture of TMDB of a size, through title-api, which keeps them; null
// for none.
function tmdb_img(file, size) {
	if (!file)
		return null;
	let via_api = `${API}/v1/tmdb/images/${size}/${file}`;
	return PROXY ? purl(via_api, null, file) : via_api;
}

const tmdb_type = t => t == 'MEDIA_TYPE_SERIES' ? 'tv' : 'movie';

// An episode a series' details tell (the last or the next), or null.
const tmdb_episode = e => e?.episodeNumber ? {
	season: e.seasonNumber, episode: e.episodeNumber, date: e.airDate || null, title: e.name || null,
} : null;

// Someone in a title, by their TMDB ID (see /tmdb/person); photo null if
// TMDB has none.
const tmdb_person = c => ({
	id: +(c.id ?? 0) || null, name: c.name, character: c.character || null, photo: tmdb_img(c.profilePath, 'w185'),
});

// A title of a collection or of the recommendations.
const tmdb_related = r => ({
	id: +r.title?.id, type: tmdb_type(r.title?.mediaType), title_id: r.titleId || null,
	title: r.name, year: r.year || null, date: r.releaseDate || null,
	poster: tmdb_img(r.posterPath, 'w342'), backdrop: tmdb_img(r.backdropPath, 'w500'),
});

const TMDB_STATUSES = {
	TMDB_STATUS_RETURNING: 'returning', TMDB_STATUS_ENDED: 'ended',
	TMDB_STATUS_CANCELED: 'canceled', TMDB_STATUS_OTHER: 'other',
};

// What TMDB has of a title (see GetTmdbDetails of title-api), its pictures
// as URLs.
function tmdb_details(type, id, r) {
	return {
		id, type, title_id: r.titleId || null,
		title: r.name, original: r.originalName || null, year: r.year || null,
		date: r.releaseDate || null, tagline: r.tagline || null, overview: r.overview || null,
		backdrop: r.backdropPath ? {
			w780: tmdb_img(r.backdropPath, 'w780'), w1280: tmdb_img(r.backdropPath, 'w1280'),
		} : null,
		logo: r.logoPath ? { url: tmdb_img(r.logoPath, 'w500'), aspect: +(r.logoAspect ?? 0) || null } : null,
		poster: tmdb_img(r.posterPath, 'w342'),
		age: r.age || null, rating: +(r.voteAverage ?? 0) || null,
		runtime: +(r.runtimeMinutes ?? 0) || null,
		genres: r.genres ?? [], countries: r.countries ?? [],
		directors: map(r.directors ?? [], tmdb_person), creators: map(r.creators ?? [], tmdb_person),
		status: TMDB_STATUSES[r.status] ?? null,
		last: tmdb_episode(r.lastEpisode), next: tmdb_episode(r.nextEpisode),
		seasons: map(r.seasons ?? [], s => ({
			season: s.seasonNumber ?? 0, episodes: s.episodeCount ?? 0,
			date: s.airDate || null, title: s.name || null,
		})),
		cast: map(r.cast ?? [], tmdb_person),
		// The cast /tmdb/cast tells, of which cast are the first.
		cast_total: +(r.castTotal ?? 0),
		trailers: map(r.videos ?? [], v => ({
			name: v.name, lang: v.language || null, kind: v.kind,
			url: 'https://www.youtube.com/watch?v=' + v.youtubeKey,
			thumb: 'https://i.ytimg.com/vi/' + v.youtubeKey + '/mqdefault.jpg',
		})),
		collection: r.collection?.id ? {
			id: +r.collection.id, name: r.collection.name, parts: map(r.collection.parts ?? [], tmdb_related),
		} : null,
		recommendations: map(r.recommendations ?? [], tmdb_related),
	};
}

// ---- who is on the screen -------------------------------------------------
//
// On a pause the page asks who is in the paused frame (/faces): the router
// sends title-api the segment the TV is paused in, as it kept it (see
// "segment cache"), and where in it the pause is; title-api cuts the frame
// out, finds the faces and tells them by the cast's photos on TMDB.

// A POST of files, one after another, to title-api: { status, out } as
// api_post has them.
function api_upload(path, files) {
	let f = ERR + clock()[1];
	system(`cat ${join(' ', map(files, shq))} | uclient-fetch -T 30 -O ${shq(f)} ` +
		`--header='Content-Type: application/octet-stream' --post-file=/dev/stdin ${shq(API + path)} 2>${shq(f + '.e')}`);
	let m = match(readfile(f + '.e') ?? '', /HTTP error ([0-9]+)/), out = trim(readfile(f) ?? '');
	unlink(f);
	unlink(f + '.e');
	return { status: m ? +m[1] : out == '' ? 0 : 200, out };
}

// A picture of title-api's faces (a path), through the router; null for
// none.
function face_img(path) {
	if (!match(path ?? '', /^\/v1\/faces\/(frames|crops|portraits)\/[0-9a-z:\/]+\.jpg$/))
		return null;
	return PROXY ? purl(API + path, null, replace(path, /^.*\//, '')) : API + path;
}

// The query title-api's faces take of what a TV plays.
const faces_query = now => sprintf('title_id=%s&season=%d&episode=%d', urlenc(now.id),
	+(now.season ?? 0), +(match(`${now.episode ?? ''}`, /^[0-9]+/)?.[0] ?? 0));

// Who title-api told is in a pause, its pictures as URLs: the frame and
// its size, each face's box in it, a picture of the face, and whom it
// was taken for, surely or not, or the two most alike; finding while the
// faces are still being found (none yet), preparing while the cast's
// photos are still being looked at: either is asked again (see
// /faces/pause).
function faces_reply(r) {
	let person = x => x?.id ? {
		id: +x.id, name: x.name, character: x.character || null,
		photo: tmdb_img(x.profile, 'w185'), portrait: face_img(x.portrait),
	} : null;
	return {
		pause: +r.pause, frame: face_img(r.frame), width: +r.width, height: +r.height,
		preparing: !!r.preparing, finding: !!r.finding,
		faces: map(r.faces ?? [], f => ({
			box: f.box, crop: face_img(f.crop), person: person(f.person),
			score: f.score ?? null, sure: !!f.sure,
			alternatives: map(f.alternatives ?? [], a => ({ ...person(a.person), score: a.score })),
		})),
	};
}

// No faces, and why: title-api's status of a faces request, else null.
const faces_reason = status => ({ '409': 'not_matched', '422': 'no_segment', '503': 'off', '404': 'gone' })[`${status}`];

// A TMDB ID the page sent.
function tmdb_id(p) {
	if (!match(p.id ?? '', /^[1-9][0-9]{0,9}$/))
		fail(400, 'invalid id');
	return p.id;
}

// A key for title-api, as the page sends it: printable ASCII, 512 bytes at
// most (TMDB's read access token is a JWT of some 250). musl's regular
// expressions repeat 255 times at most, hence the length apart.
function api_key(key) {
	if (length(key) > 512 || !match(key, /^[!-~]*$/))
		fail(400, 'invalid key');
	return key;
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
			// The title on TMDB, once title-api has matched it (see /tmdb/title).
			tmdb: r.tmdb?.id ? {
				id: +r.tmdb.id, type: r.tmdb.mediaType == 'MEDIA_TYPE_SERIES' ? 'tv' : 'movie',
			} : null,
			// A movie's, marked on a video of duration; an episode's are
			// with the episode.
			sections: series ? null : sections(r.sections),
			seasons: !series ? null : map(r.seasons ?? [], s => ({
				season: s.seasonNumber,
				episodes: map(s.episodes ?? [], e => ({
					episode: e.episodeNumber, title: e.title,
					duration: +(e.durationSeconds ?? 0),
					sections: sections(e.sections),
				})),
			})),
		};
	},

	// Where a title was left off: pos, dur and done of a movie, or the last
	// episode and eps by "season:episode" of a series; null if never watched.
	// prefs: what it was watched with (see watched_with).
	// Without id: the recently watched titles, latest first, each with the
	// position of the movie or of the last episode, and the titles saved to
	// watch, latest saved first, with the same of those watched.
	'GET /progress': p => {
		if (p.id == null)
			return { recent: recent_rows(), saved: saved_rows() };
		let id = title_id(p), e = title_row(id), progress = null;
		if (e?.season != null) {
			let eps = {};
			for (let x in sql_rows("SELECT ep, pos, dur, done, t FROM progress WHERE id = ? AND ep != ''", id))
				eps[x.ep] = { pos: x.pos, dur: x.dur, done: !!x.done, t: x.t };
			progress = { last: { season: e.season, episode: e.episode }, eps };
		} else if (e) {
			let x = progress_row(id, '');
			progress = x ? { pos: x.pos, dur: x.dur, done: x.done } : null;
		}
		return { progress, prefs: e?.prefs ?? null };
	},


	// What is new of the series watched and saved, for the main page's
	// badges (see series_updates); up to 20 s, as the provider is asked.
	'GET /updates': p => series_updates(),

	// Saves a title to watch (on=1), with its name and poster for the list,
	// or removes it (on=0); the titles saved, as /progress lists them.
	'POST /save': p => {
		let id = title_id(p);
		if (!match(p.on ?? '', /^[01]$/))
			fail(400, 'invalid on');
		let title = length(p.title ?? '') <= 512 ? p.title : null;
		if (p.on == '0')
			sql_do('DELETE FROM saved WHERE id = ?', id);
		else if (sql_do('INSERT INTO saved (id, title, poster, t) VALUES (?, ?, ?, ?) ' +
		                'ON CONFLICT (id) DO NOTHING', id, title, own_poster(p.poster), time()))
			system(`${shq(sourcepath())} mark ${shq(id)} </dev/null >/dev/null 2>&1 &`);
		changed(true);
		return { saved: saved_rows() };
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

	// With auto=1 and no season or episode, what the play button of the
	// title's page would start (see auto_episode); a title never started
	// answers 409 with the sources to pick from, if there are several.
	'POST /play': p => {
		let dev = device(p);
		if (p.auto == '1' && p.season == null && p.episode == null)
			auto_episode(p);
		return play(dev, p);
	},

	// Turns the next episode on or off for what is on (see ended).
	'POST /autonext': p => {
		if (!match(p.on ?? '', /^[01]$/))
			fail(400, 'invalid on');
		let dev = device(p), key = state_key(dev), now = now_get(key);
		set_setting('autonext', p.on);
		if (now) {
			now.autonext = p.on == '1';
			now_set(key, now);
		}
		return with_now(castctl(dev, 'status'), dev);
	},

	// Turns the skipping of the recap and the intro on or off, for what is
	// on (see plan) and what starts later.
	'POST /autoskip': p => {
		if (!match(p.on ?? '', /^[01]$/))
			fail(400, 'invalid on');
		let dev = device(p), key = state_key(dev), now = now_get(key);
		set_setting('autoskip', p.on);
		if (now) {
			now.autoskip = p.on == '1';
			now_set(key, now);
		}
		return with_now(castctl(dev, 'status'), dev);
	},

	// title-api's marking of the episodes the provider marks none of:
	// { enabled, key, hint, error }, the key only told whether it is set
	// and by its last characters.
	'GET /marking': p => marking(api('/v1/marking')),

	// Turns the marking on or off (on), sets the Claude API key (key) or
	// removes it (key '').
	'POST /marking': p => {
		let body = {};
		if (p.on != null) {
			if (!match(p.on, /^[01]$/))
				fail(400, 'invalid on');
			body.enabled = p.on == '1';
		}
		if (p.key != null)
			body.apiKey = api_key(p.key);
		let r = api_post('/v1/marking', body);
		if (r.status == 400)
			fail(400, 'Ключ не подошёл');
		if (r.status != 200)
			fail(502, 'title-api request failed' + (r.status ? ` (HTTP ${r.status})` : ''));
		return marking(json(r.out));
	},

	// title-api's matching of the titles opened with TMDB: { key, hint,
	// error }, the key only told whether it is set and by its last
	// characters.
	'GET /tmdb/settings': p => tmdb(api('/v1/tmdb')),

	// Sets the TMDB API key or read access token (key), or removes it (key
	// '').
	'POST /tmdb/settings': p => {
		let r = api_post('/v1/tmdb', { apiKey: api_key(p.key ?? '') });
		if (r.status == 400)
			fail(400, 'Ключ не подошёл');
		if (r.status != 200)
			fail(502, 'title-api request failed' + (r.status ? ` (HTTP ${r.status})` : ''));
		return tmdb(json(r.out));
	},

	// What TMDB has of a title (type movie or tv, id its TMDB ID, as /title
	// tells them), whether the provider has it or not; null if TMDB has no
	// such title or title-api no key. title-api keeps it.
	'GET /tmdb/title': p => {
		if (!match(p.type ?? '', /^(movie|tv)$/))
			fail(400, 'invalid type');
		let id = tmdb_id(p), r = api(`/v1/tmdb/${p.type}/${id}`, [400, 404]);
		return r ? tmdb_details(p.type, +id, r) : null;
	},

	// The title on TMDB of a title /title told without one, as title-api
	// matches it in the background: waits up to 15 s; pending, if it is
	// still to be matched, to be asked again.
	'GET /tmdb/match': p => {
		let r = api(`/v1/titles/${title_id(p)}/tmdb`);
		return {
			tmdb: r.tmdb?.id ? { id: +r.tmdb.id, type: tmdb_type(r.tmdb.mediaType) } : null,
			pending: !!r.pending,
		};
	},

	// Looks for a title of TMDB (type movie or tv, id) on the provider, as
	// title-api does in the background, waiting up to 15 s: state pending
	// (to be asked again), found, with the provider's title_id, or
	// not_found.
	'POST /tmdb/lookup': p => {
		if (!match(p.type ?? '', /^(movie|tv)$/))
			fail(400, 'invalid type');
		let r = api_post(`/v1/tmdb/${p.type}/${tmdb_id(p)}/lookup`, {});
		if (r.status != 200)
			fail(502, 'title-api request failed' + (r.status ? ` (HTTP ${r.status})` : ''));
		let j = json(r.out);
		return {
			state: { STATE_PENDING: 'pending', STATE_FOUND: 'found' }[j?.state] ?? 'not_found',
			title_id: j?.titleId || null,
		};
	},

	// Who is in a title on TMDB, all of them as billed; null as /tmdb/title.
	'GET /tmdb/cast': p => {
		if (!match(p.type ?? '', /^(movie|tv)$/))
			fail(400, 'invalid type');
		let r = api(`/v1/tmdb/${p.type}/${tmdb_id(p)}/cast`, [400, 404]);
		return r ? { cast: map(r.cast ?? [], tmdb_person) } : null;
	},

	// Someone on TMDB (id their TMDB ID): who they are, their pages
	// elsewhere and the titles they are in or made, the best known first,
	// those watched here seen; null as /tmdb/title.
	'GET /tmdb/person': p => {
		let r = api(`/v1/tmdb/person/${tmdb_id(p)}`, [400, 404]);
		if (!r)
			return null;
		let watched = {};
		for (let t in sql_rows('SELECT id FROM titles'))
			watched[t.id] = true;
		return {
			id: +r.id, name: r.name, original: r.originalName || null,
			photo: tmdb_img(r.profilePath, 'w342'), profession: r.profession || null,
			birthday: r.birthday || null, deathday: r.deathday || null, place: r.placeOfBirth || null,
			biography: r.biography || null,
			gender: { TMDB_GENDER_FEMALE: 'female', TMDB_GENDER_MALE: 'male' }[r.gender] ?? null,
			links: map(r.links ?? [], l => ({ site: l.site, url: l.url })),
			credits: map(r.credits ?? [], c => {
				let t = tmdb_related(c.title ?? {});
				return {
					...t, character: c.character || null, jobs: c.jobs ?? [],
					episodes: +(c.episodes ?? 0) || null, acting: !!c.acting, seen: !!watched[t.title_id],
				};
			}),
		};
	},

	// The episodes of a season of a series on TMDB (id its TMDB ID); null
	// as /tmdb/title.
	'GET /tmdb/season': p => {
		if (!match(p.season ?? '', /^[0-9]{1,4}$/))
			fail(400, 'invalid season');
		let r = api(`/v1/tmdb/tv/${tmdb_id(p)}/seasons/${+p.season}`, [400, 404]);
		return r ? {
			episodes: map(r.episodes ?? [], e => ({
				episode: e.episodeNumber, title: e.name || null, overview: e.overview || null,
				still: tmdb_img(e.stillPath, 'w300'), date: e.airDate || null,
				runtime: +(e.runtimeMinutes ?? 0) || null, upcoming: !!e.upcoming,
			})),
		} : null;
	},

	// Who is on the screen of a TV paused: see faces_reply; or, with no
	// faces, why (reason): no_play, playing, no_cache (the quality is auto
	// or the media proxy is off, so no segment is kept), no_segment,
	// not_matched (with TMDB), off (title-api has no faces service).
	'GET /faces': p => {
		let dev = device(p), key = state_key(dev), now = now_get(key);
		let m = castctl(dev, 'status').media;
		if (!now?.id || !m || m.url != now.url)
			return { reason: 'no_play' };
		if (m.state != 'PAUSED')
			return { reason: 'playing' };
		if (!now.c)
			return { reason: 'no_cache' };
		let t = +(m.time ?? 0);
		let seg = sql_rows('SELECT key, start FROM cache WHERE c = ? AND start IS NOT NULL AND start <= ? AND stop > ? ' +
		                   'ORDER BY t DESC LIMIT 1', now.c, t, t)[0];
		if (!seg || !cache_get(seg.key))
			return { reason: 'no_segment' };
		let init = replace(seg.key, /-[0-9]+-[0-9]+$/, '-i'), files = [];
		if (cache_get(init))
			push(files, CACHE + init);
		push(files, CACHE + seg.key);
		let r = api_upload(sprintf('/v1/faces?%s&t=%.3f&offset=%.3f', faces_query(now), t, max(t - +seg.start, 0)), files);
		if (faces_reason(r.status))
			return { reason: faces_reason(r.status) };
		let j = r.status == 200 ? json(r.out) : null;
		if (type(j) != 'object')
			fail(502, 'title-api request failed' + (r.status ? ` (HTTP ${r.status})` : ''));
		return faces_reply(j);
	},

	// The music of what a TV plays, told by now (see "music"): tracks [{
	// start, stop (seconds of the video), offset (seconds into the track
	// at start), title, artist, cover, url (Shazam's), apple_url, spotify,
	// duration }], pending (asks of Shazam yet to be made), received
	// (seconds of audio title-api has); or, with none, why (reason):
	// no_play, no_audio (the dub a primary player's video plays in is not
	// seen yet), off (no music on the router or title-api). The
	// status carries the same, as last told (see music_of).
	'GET /music': p => {
		let key = state_key(device(p)), now = now_get(key);
		return now?.id ? music_ask(now, music_source(now, tv_get(key, 'seen'))) : { reason: 'no_play' };
	},

	// A pause /faces told of (id its pause): its faces, told by the cast's
	// photos looked at by now; while they are being found, title-api waits
	// for them up to 15 s.
	'GET /faces/pause': p => {
		if (!match(p.id ?? '', /^[1-9][0-9]{0,14}$/))
			fail(400, 'invalid id');
		let r = api(`/v1/faces/pauses/${p.id}`, [404, 409, 503]);
		return r ? faces_reply(r) : { reason: 'gone' };
	},

	// The cast's photos of what a TV plays are looked at ahead of a pause.
	'POST /faces/prepare': p => {
		let now = now_get(state_key(device(p)));
		if (!now?.id)
			return { preparing: false };
		let r = api_post('/v1/faces/prepare?' + faces_query(now), {});
		return { preparing: r.status == 200 && !!json(r.out || '{}')?.preparing };
	},

	'POST /cmd': p => {
		let c = p.c ?? '', kind = COMMANDS[c], arg = p.arg ?? '';
		if (!exists(COMMANDS, c))
			fail(400, 'unknown command');
		let dev = device(p);
		// Stopped from the page, the episode has not ended (see ended),
		// and the playback has.
		if (c == 'stop' || c == 'quit') {
			tv_set(state_key(dev), 'seen', null);
			media_end(now_get(state_key(dev))?.media);
		}
		if (kind == 'num' && !match(arg, /^[0-9]+(\.[0-9]+)?$/))
			fail(400, 'numeric arg required');
		if (kind == 'str' && (arg == '' || length(arg) > 128))
			fail(400, 'arg required');
		if (kind == 'style' && !valid_style(arg))
			fail(400, 'invalid subtitle style');
		// Before the command: its status is seen by watch_end at once.
		if (c == 'pause') {
			let src = match(p.src ?? '', /^[a-z ]{1,24}$/) ? p.src : '?';
			kv_set('pause:' + state_key(dev), time());
			pause_note(state_key(dev), sprintf('pause from the %s, %s, %s', src,
			           getenv('REMOTE_ADDR') ?? '?', substr(getenv('HTTP_USER_AGENT') ?? '?', 0, 160)));
		}
		// The volume set here is the one kept, not put back (see watch_end).
		if (c == 'volume') {
			kv_set('volset:' + state_key(dev), time());
			volume_keep(state_key(dev), min(+arg / 100, 1));
		}
		let st = with_now(kind == null ? castctl(dev, c) : castctl(dev, c, arg), dev);
		if (c == 'volume' && st.volume != null)
			volume_keep(state_key(dev), +st.volume);
		// A pause, a resume or a seek reaches title-api at once, not with
		// the next refresh (which a paused TV may not ask for in a while).
		let now = index(['pause', 'resume', 'seek', 'ff', 'rew'], c) >= 0 ?
			now_get(state_key(dev)) : null;
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
if (ARGV[0] == 'ahead' && match(ARGV[1] ?? '', /^[0-9A-Za-z._]+$/)) {
	list_ahead(ARGV[1]);
	exit(0);
}
if (ARGV[0] == 'mark' && match(ARGV[1] ?? '', /^[0-9A-Za-z]{1,64}$/)) {
	mark_saved(ARGV[1]);
	exit(0);
}
if (ARGV[0] == 'music' && match(ARGV[1] ?? '', /^[0-9A-Za-z._]+$/)) {
	music_fetch(ARGV[1]);
	exit(0);
}
if (ARGV[0] == 'due' && match(ARGV[1] ?? '', /^[0-9A-Za-z._]+$/)) {
	wake(ARGV[1], ARGV[2]);
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
	proxy(pm[1], qs.u, qs.q, qs.h, qs.drc == '1', qs.m, qs.pv == '1', qs.c, qs.k);
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
	let subs = tv_get(sm[1], 'subs');
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
