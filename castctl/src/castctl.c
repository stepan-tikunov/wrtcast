/*
 * castctl - minimal Chromecast controller (CASTV2 protocol).
 *
 * Wire format: TLS to port 8009, each message is a 4-byte big-endian length
 * followed by a protobuf-encoded CastMessage whose payload is a JSON string.
 *
 * Every invocation is stateless: connect, find the running app / media
 * session, send one command, wait for the reply, disconnect. The media
 * session lives on the Chromecast, so no daemon is needed.
 */
#define _GNU_SOURCE /* strcasestr */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <stdint.h>
#include <stdarg.h>
#include <unistd.h>
#include <time.h>
#include <poll.h>
#include <netdb.h>
#include <ifaddrs.h>
#include <net/if.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <sys/socket.h>

#include <mbedtls/version.h>
#include <mbedtls/net_sockets.h>
#include <mbedtls/ssl.h>
#include <mbedtls/entropy.h>
#include <mbedtls/ctr_drbg.h>
#if MBEDTLS_VERSION_MAJOR >= 3
#include <psa/crypto.h>
#endif

#define DEFAULT_PORT "8009"
#define TIMEOUT_MS 10000
#define LOAD_TIMEOUT_S 20 /* how long play waits for playback to start */
#define MAX_MSG (64 * 1024)
#define MAX_TRACKS 32
#define MAX_SUBS 16
#define SUB_TRACK_BASE 101 /* ids for side-loaded subtitles, clear of HLS ones */
#define MAX_DEVICES 32
#define DISCOVER_MS 2000 /* default mDNS listening window */

#define NS_CONN     "urn:x-cast:com.google.cast.tp.connection"
#define NS_HEART    "urn:x-cast:com.google.cast.tp.heartbeat"
#define NS_RECEIVER "urn:x-cast:com.google.cast.receiver"
#define NS_MEDIA    "urn:x-cast:com.google.cast.media"

#define DEFAULT_MEDIA_RECEIVER "CC1AD845"
#define SENDER   "sender-castctl"
#define RECEIVER "receiver-0"

static int verbose;

static void die(const char *fmt, ...)
{
	va_list ap;
	fprintf(stderr, "castctl: ");
	va_start(ap, fmt);
	vfprintf(stderr, fmt, ap);
	va_end(ap);
	fputc('\n', stderr);
	exit(1);
}

/* ------------------------------------------------------------------ */
/* Tiny JSON reader: navigates raw JSON text without building a tree.  */

static const char *ws(const char *p)
{
	while (*p == ' ' || *p == '\t' || *p == '\n' || *p == '\r')
		p++;
	return p;
}

/* Returns pointer just past the value starting at p, or NULL. */
static const char *skip_value(const char *p)
{
	int depth = 0;

	p = ws(p);
	do {
		switch (*p) {
		case '\0':
			return NULL;
		case '"':
			for (p++; *p != '"'; p++) {
				if (!*p)
					return NULL;
				if (*p == '\\' && !*++p)
					return NULL;
			}
			p++;
			break;
		case '{': case '[':
			depth++; p++;
			break;
		case '}': case ']':
			depth--; p++;
			break;
		default:
			if (depth == 0) {
				/* scalar: number / true / false / null */
				while (*p && !strchr(",}] \t\r\n", *p))
					p++;
				return p;
			}
			p++;
		}
	} while (depth > 0);
	return p;
}

/*
 * Look up a dot-separated path ("status.0.media.duration"); numeric
 * segments index into arrays. Returns pointer to the value or NULL.
 */
static const char *jget(const char *p, const char *path)
{
	char seg[64];

	while (p && *path) {
		size_t n = strcspn(path, ".");
		if (n >= sizeof(seg))
			return NULL;
		memcpy(seg, path, n);
		seg[n] = 0;
		path += n + (path[n] == '.');

		p = ws(p);
		if (*p == '[') {
			char *end;
			long idx = strtol(seg, &end, 10);
			if (*end)
				return NULL;
			p = ws(p + 1);
			while (idx-- > 0 && p && *p != ']') {
				p = skip_value(p);
				if (p) p = ws(p);
				if (p && *p == ',') p = ws(p + 1);
			}
			if (!p || *p == ']')
				return NULL;
		} else if (*p == '{') {
			const char *found = NULL;
			p = ws(p + 1);
			while (*p == '"') {
				const char *k = p + 1;
				const char *v = skip_value(p);
				if (!v) return NULL;
				int match = (size_t)(v - 1 - k) == n && !memcmp(k, seg, n);
				v = ws(v);
				if (*v != ':') return NULL;
				v = ws(v + 1);
				if (match) { found = v; break; }
				p = skip_value(v);
				if (!p) return NULL;
				p = ws(p);
				if (*p == ',') p = ws(p + 1);
			}
			p = found;
		} else {
			return NULL;
		}
	}
	return p;
}

/* Copies a JSON string value (simple unescape) into buf. */
static int jstr(const char *json, const char *path, char *buf, size_t len)
{
	const char *p = jget(json, path);
	size_t i = 0;

	if (!p || *p != '"' || !len)
		return 0;
	for (p++; *p && *p != '"' && i + 1 < len; p++) {
		if (*p == '\\' && p[1])
			p++;
		buf[i++] = *p;
	}
	buf[i] = 0;
	return 1;
}

static int jnum(const char *json, const char *path, double *out)
{
	const char *p = jget(json, path);
	char *end;

	if (!p)
		return 0;
	*out = strtod(p, &end);
	return end != p;
}

static int jbool(const char *json, const char *path)
{
	const char *p = jget(json, path);
	return p && !strncmp(p, "true", 4);
}

/* Writes s as a JSON string literal (with quotes) into out. */
static void jescape(char *out, size_t len, const char *s)
{
	size_t i = 0;

	if (len < 3)
		die("buffer too small");
	out[i++] = '"';
	for (; *s && i + 8 < len; s++) {
		unsigned char c = *s;
		if (c == '"' || c == '\\') {
			out[i++] = '\\';
			out[i++] = c;
		} else if (c < 0x20) {
			i += snprintf(out + i, len - i, "\\u%04x", c);
		} else {
			out[i++] = c;
		}
	}
	if (*s)
		die("string too long");
	out[i++] = '"';
	out[i] = 0;
}

/* ------------------------------------------------------------------ */
/* TLS transport                                                       */

static mbedtls_net_context net;
static mbedtls_ssl_context ssl;
static mbedtls_ssl_config conf;
static mbedtls_entropy_context entropy;
static mbedtls_ctr_drbg_context drbg;

static void tls_connect(const char *host, const char *port)
{
	int ret;

#if MBEDTLS_VERSION_MAJOR >= 3
	if (psa_crypto_init() != PSA_SUCCESS)
		die("psa_crypto_init failed");
#endif
	mbedtls_net_init(&net);
	mbedtls_ssl_init(&ssl);
	mbedtls_ssl_config_init(&conf);
	mbedtls_entropy_init(&entropy);
	mbedtls_ctr_drbg_init(&drbg);

	if (mbedtls_ctr_drbg_seed(&drbg, mbedtls_entropy_func, &entropy,
				  (const unsigned char *)"castctl", 7))
		die("rng seed failed");
	if ((ret = mbedtls_net_connect(&net, host, port, MBEDTLS_NET_PROTO_TCP)))
		die("cannot connect to %s:%s (%d)", host, port, ret);
	if (mbedtls_ssl_config_defaults(&conf, MBEDTLS_SSL_IS_CLIENT,
					MBEDTLS_SSL_TRANSPORT_STREAM,
					MBEDTLS_SSL_PRESET_DEFAULT))
		die("ssl config failed");
	/* Chromecasts use self-signed device certificates. */
	mbedtls_ssl_conf_authmode(&conf, MBEDTLS_SSL_VERIFY_NONE);
	mbedtls_ssl_conf_rng(&conf, mbedtls_ctr_drbg_random, &drbg);
	mbedtls_ssl_conf_read_timeout(&conf, TIMEOUT_MS);
	if (mbedtls_ssl_setup(&ssl, &conf))
		die("ssl setup failed");
	mbedtls_ssl_set_bio(&ssl, &net, mbedtls_net_send, NULL,
			    mbedtls_net_recv_timeout);
	while ((ret = mbedtls_ssl_handshake(&ssl))) {
		if (ret != MBEDTLS_ERR_SSL_WANT_READ &&
		    ret != MBEDTLS_ERR_SSL_WANT_WRITE)
			die("TLS handshake failed (-0x%04x)", -ret);
	}
}

static void tls_close(void)
{
	mbedtls_ssl_close_notify(&ssl);
	mbedtls_net_free(&net);
	mbedtls_ssl_free(&ssl);
	mbedtls_ssl_config_free(&conf);
	mbedtls_ctr_drbg_free(&drbg);
	mbedtls_entropy_free(&entropy);
}

static void tls_write(const unsigned char *buf, size_t len)
{
	while (len) {
		int ret = mbedtls_ssl_write(&ssl, buf, len);
		if (ret == MBEDTLS_ERR_SSL_WANT_READ ||
		    ret == MBEDTLS_ERR_SSL_WANT_WRITE)
			continue;
		if (ret < 0)
			die("write failed (-0x%04x)", -ret);
		buf += ret;
		len -= ret;
	}
}

static void tls_read(unsigned char *buf, size_t len)
{
	while (len) {
		int ret = mbedtls_ssl_read(&ssl, buf, len);
		if (ret == MBEDTLS_ERR_SSL_WANT_READ ||
		    ret == MBEDTLS_ERR_SSL_WANT_WRITE)
			continue;
		if (ret == MBEDTLS_ERR_SSL_TIMEOUT)
			die("timeout waiting for device");
		if (ret <= 0)
			die("connection closed (-0x%04x)", -ret);
		buf += ret;
		len -= ret;
	}
}

/* ------------------------------------------------------------------ */
/* CastMessage protobuf (hand-rolled, only the fields we use)          */
/*   1 protocol_version (varint, 0)   2 source_id   3 destination_id   */
/*   4 namespace   5 payload_type (varint, 0=string)   6 payload_utf8  */

static size_t pb_varint(unsigned char *p, uint64_t v)
{
	size_t n = 0;
	do {
		p[n++] = (v & 0x7f) | (v > 0x7f ? 0x80 : 0);
		v >>= 7;
	} while (v);
	return n;
}

static size_t pb_string(unsigned char *p, int field, const char *s)
{
	size_t len = strlen(s), n = 0;
	p[n++] = (field << 3) | 2;
	n += pb_varint(p + n, len);
	memcpy(p + n, s, len);
	return n + len;
}

static void cast_send(const char *dst, const char *ns, const char *payload)
{
	static unsigned char buf[MAX_MSG + 4];
	unsigned char *p = buf + 4;
	size_t n = 0;

	if (strlen(payload) + strlen(ns) + strlen(dst) + 64 > MAX_MSG)
		die("message too large");
	p[n++] = (1 << 3) | 0; p[n++] = 0;
	n += pb_string(p + n, 2, SENDER);
	n += pb_string(p + n, 3, dst);
	n += pb_string(p + n, 4, ns);
	p[n++] = (5 << 3) | 0; p[n++] = 0;
	n += pb_string(p + n, 6, payload);

	buf[0] = n >> 24; buf[1] = n >> 16; buf[2] = n >> 8; buf[3] = n;
	if (verbose)
		fprintf(stderr, "> [%s] %s\n", ns, payload);
	tls_write(buf, n + 4);
}

struct cast_msg {
	char ns[128];
	char src[128];
	char *payload; /* NUL-terminated JSON */
};

static int pb_read_varint(const unsigned char **p, const unsigned char *end,
			  uint64_t *v)
{
	int shift = 0;
	*v = 0;
	while (*p < end && shift < 64) {
		unsigned char b = *(*p)++;
		*v |= (uint64_t)(b & 0x7f) << shift;
		if (!(b & 0x80))
			return 1;
		shift += 7;
	}
	return 0;
}

static void copy_field(char *dst, size_t cap, const unsigned char *s, size_t n)
{
	if (n >= cap)
		n = cap - 1;
	memcpy(dst, s, n);
	dst[n] = 0;
}

/* Reads one message; answers heartbeat PINGs transparently. */
static void cast_recv(struct cast_msg *m)
{
	static unsigned char buf[MAX_MSG + 1];
	static char payload[MAX_MSG + 1];

	for (;;) {
		unsigned char hdr[4];
		const unsigned char *p, *end;
		size_t len;

		tls_read(hdr, 4);
		len = (size_t)hdr[0] << 24 | hdr[1] << 16 | hdr[2] << 8 | hdr[3];
		if (len > MAX_MSG)
			die("incoming message too large (%zu)", len);
		tls_read(buf, len);

		memset(m, 0, sizeof(*m));
		payload[0] = 0;
		m->payload = payload;
		p = buf;
		end = buf + len;
		while (p < end) {
			uint64_t key, v;
			if (!pb_read_varint(&p, end, &key))
				die("bad protobuf");
			if ((key & 7) == 0) {
				if (!pb_read_varint(&p, end, &v))
					die("bad protobuf");
			} else if ((key & 7) == 2) {
				if (!pb_read_varint(&p, end, &v) || v > (uint64_t)(end - p))
					die("bad protobuf");
				switch (key >> 3) {
				case 2: copy_field(m->src, sizeof(m->src), p, v); break;
				case 4: copy_field(m->ns, sizeof(m->ns), p, v); break;
				case 6: copy_field(payload, sizeof(payload), p, v); break;
				}
				p += v;
			} else {
				die("unexpected protobuf wire type %d", (int)(key & 7));
			}
		}
		if (verbose)
			fprintf(stderr, "< [%s] %s\n", m->ns, m->payload);

		if (!strcmp(m->ns, NS_HEART)) {
			char type[16];
			if (jstr(payload, "type", type, sizeof(type)) &&
			    !strcmp(type, "PING"))
				cast_send(m->src, NS_HEART, "{\"type\":\"PONG\"}");
			continue;
		}
		return;
	}
}

/* ------------------------------------------------------------------ */
/* Request/response helpers                                            */

static int next_request_id = 1;

/*
 * Sends a request carrying a fresh requestId and waits for the reply with
 * the same requestId. "body" is the JSON object contents without braces.
 * Returns the reply payload (valid until the next receive).
 */
static const char *request(const char *dst, const char *ns, const char *body)
{
	static char buf[MAX_MSG];
	struct cast_msg m;
	int id = next_request_id++;
	time_t deadline = time(NULL) + TIMEOUT_MS / 1000;

	snprintf(buf, sizeof(buf), "{\"requestId\":%d,%s}", id, body);
	cast_send(dst, ns, buf);
	while (time(NULL) <= deadline) {
		double rid;
		cast_recv(&m);
		if (!strcmp(m.ns, NS_CONN)) {
			char type[16];
			if (jstr(m.payload, "type", type, sizeof(type)) &&
			    !strcmp(type, "CLOSE") && !strcmp(m.src, dst))
				die("device closed the channel");
			continue;
		}
		if (jnum(m.payload, "requestId", &rid) && (int)rid == id)
			return m.payload;
	}
	die("timeout waiting for reply");
	return NULL;
}

static void virtual_connect(const char *dst)
{
	cast_send(dst, NS_CONN,
		  "{\"type\":\"CONNECT\",\"origin\":{},"
		  "\"userAgent\":\"castctl\",\"senderInfo\":{\"sdkType\":2,"
		  "\"version\":\"15.605.1.3\",\"browserVersion\":\"44.0.2403.30\","
		  "\"platform\":4,\"systemVersion\":\"Macintosh; Intel Mac OS X10_10_3\","
		  "\"connectionType\":1}}");
}

static void check_reply_type(const char *reply, const char *what)
{
	char type[48];

	jstr(reply, "type", type, sizeof(type));
	if (!strcmp(type, "LAUNCH_ERROR") || !strcmp(type, "LOAD_FAILED") ||
	    !strcmp(type, "LOAD_CANCELLED") || !strcmp(type, "INVALID_REQUEST") ||
	    !strcmp(type, "INVALID_PLAYER_STATE") || !strcmp(type, "ERROR")) {
		char reason[64] = "";
		jstr(reply, "reason", reason, sizeof(reason));
		die("%s failed: %s%s%s", what, type, *reason ? " " : "", reason);
	}
}

struct app {
	char app_id[32];
	char name[64];
	char session_id[64];
	char transport_id[64];
	double volume;
	int muted;
};

static int app_supports_media(const char *r, int idx)
{
	char path[64], ns[128];
	int i;

	for (i = 0; ; i++) {
		snprintf(path, sizeof(path),
			 "status.applications.%d.namespaces.%d.name", idx, i);
		if (!jstr(r, path, ns, sizeof(ns)))
			return 0;
		if (!strcmp(ns, NS_MEDIA))
			return 1;
	}
}

/*
 * Fills app with the first running application that speaks the media
 * namespace (idle screens like "Backdrop" don't); returns 0 if none.
 */
static int receiver_status(struct app *a)
{
	const char *r = request(RECEIVER, NS_RECEIVER, "\"type\":\"GET_STATUS\"");
	char path[64];
	int i;

	memset(a, 0, sizeof(*a));
	jnum(r, "status.volume.level", &a->volume);
	a->muted = jbool(r, "status.volume.muted");
	for (i = 0; ; i++) {
		snprintf(path, sizeof(path), "status.applications.%d", i);
		if (!jget(r, path))
			return 0;
		if (app_supports_media(r, i))
			break;
	}
#define APP_FIELD(field, dst) \
	snprintf(path, sizeof(path), "status.applications.%d." field, i); \
	jstr(r, path, dst, sizeof(dst))
	APP_FIELD("transportId", a->transport_id);
	APP_FIELD("appId", a->app_id);
	APP_FIELD("displayName", a->name);
	APP_FIELD("sessionId", a->session_id);
#undef APP_FIELD
	return *a->transport_id != 0;
}

static void launch_media_receiver(struct app *a)
{
	const char *r;
	int i;

	r = request(RECEIVER, NS_RECEIVER,
		    "\"type\":\"LAUNCH\",\"appId\":\"" DEFAULT_MEDIA_RECEIVER "\"");
	check_reply_type(r, "launch");
	/* The first status may arrive before the app has a transport. */
	for (i = 0; i < 10; i++) {
		if (receiver_status(a) && !strcmp(a->app_id, DEFAULT_MEDIA_RECEIVER))
			return;
		usleep(300 * 1000);
	}
	die("media receiver did not start");
}

struct media {
	long session_id; /* 0 = no media session */
	char state[32];
	char idle_reason[32];
	char url[2048];
	char content_type[64];
	char stream_type[16];
	double time;
	double duration;
	int ntracks;        /* -1 = track list not reported in this status */
	struct track {
		long id;
		char type[8];   /* AUDIO, VIDEO, TEXT */
		char lang[16];
		char name[64];
	} tracks[MAX_TRACKS];
	int nactive;        /* -1 = not reported */
	long active[MAX_TRACKS];
};

static void parse_tracks(const char *r, struct media *md)
{
	char path[64];
	double v;
	int i;

	if (jget(r, "status.0.media.tracks")) {
		md->ntracks = 0;
		for (i = 0; i < MAX_TRACKS; i++) {
			struct track *t = &md->tracks[md->ntracks];
			snprintf(path, sizeof(path), "status.0.media.tracks.%d", i);
			const char *tr = jget(r, path);
			if (!tr)
				break;
			memset(t, 0, sizeof(*t));
			if (!jnum(tr, "trackId", &v))
				continue;
			t->id = (long)v;
			jstr(tr, "type", t->type, sizeof(t->type));
			jstr(tr, "language", t->lang, sizeof(t->lang));
			jstr(tr, "name", t->name, sizeof(t->name));
			md->ntracks++;
		}
	}
	if (jget(r, "status.0.activeTrackIds")) {
		md->nactive = 0;
		for (i = 0; i < MAX_TRACKS; i++) {
			snprintf(path, sizeof(path), "status.0.activeTrackIds.%d", i);
			if (!jnum(r, path, &v))
				break;
			md->active[md->nactive++] = (long)v;
		}
	}
}

/*
 * Updates md from a MEDIA_STATUS reply. Replies to commands carry only the
 * changed fields (no "media" object), so absent fields are left untouched.
 */
static void parse_media_status(const char *r, struct media *md)
{
	double v;

	if (!jnum(r, "status.0.mediaSessionId", &v)) {
		md->session_id = 0;
		return;
	}
	md->session_id = (long)v;
	jstr(r, "status.0.playerState", md->state, sizeof(md->state));
	if (!jstr(r, "status.0.idleReason", md->idle_reason,
		  sizeof(md->idle_reason)))
		*md->idle_reason = 0;
	jnum(r, "status.0.currentTime", &md->time);
	jstr(r, "status.0.media.contentId", md->url, sizeof(md->url));
	jstr(r, "status.0.media.contentType", md->content_type,
	     sizeof(md->content_type));
	jstr(r, "status.0.media.streamType", md->stream_type,
	     sizeof(md->stream_type));
	jnum(r, "status.0.media.duration", &md->duration);
	parse_tracks(r, md);
}

static void media_status(const char *transport, struct media *md)
{
	const char *r = request(transport, NS_MEDIA, "\"type\":\"GET_STATUS\"");
	check_reply_type(r, "media status");
	memset(md, 0, sizeof(*md));
	md->ntracks = md->nactive = -1;
	parse_media_status(r, md);
}

/* ------------------------------------------------------------------ */
/* Output                                                              */

static void print_status(const struct app *a, const struct media *md)
{
	char esc[4200];

	printf("{\"app\":");
	if (*a->app_id) {
		jescape(esc, sizeof(esc), a->name);
		printf("{\"id\":\"%s\",\"name\":%s}", a->app_id, esc);
	} else {
		printf("null");
	}
	printf(",\"volume\":%.2f,\"muted\":%s", a->volume,
	       a->muted ? "true" : "false");
	if (md && md->session_id) {
		jescape(esc, sizeof(esc), md->url);
		printf(",\"media\":{\"state\":\"%s\",\"time\":%.1f,"
		       "\"duration\":%.1f,\"live\":%s,\"url\":%s",
		       md->state, md->time, md->duration,
		       !strcmp(md->stream_type, "LIVE") ? "true" : "false", esc);
		if (*md->idle_reason)
			printf(",\"idleReason\":\"%s\"", md->idle_reason);
		if (md->ntracks >= 0) {
			printf(",\"tracks\":[");
			for (int i = 0; i < md->ntracks; i++) {
				const struct track *t = &md->tracks[i];
				int on = 0;
				for (int j = 0; j < md->nactive; j++)
					on |= md->active[j] == t->id;
				jescape(esc, sizeof(esc), t->name);
				printf("%s{\"id\":%ld,\"type\":\"%s\",\"lang\":\"%s\","
				       "\"name\":%s,\"active\":%s}", i ? "," : "",
				       t->id, t->type, t->lang, esc, on ? "true" : "false");
			}
			printf("]");
		}
		printf("}");
	} else {
		printf(",\"media\":null");
	}
	printf("}\n");
}

/* ------------------------------------------------------------------ */
/* Discovery: one-shot mDNS query for _googlecast._tcp.local           */

/*
 * The query is sent from an ephemeral port, so devices answer by unicast
 * straight to our socket (RFC 6762 "legacy unicast"); nothing has to bind
 * 5353 and no mDNS daemon is needed. It goes out of every IPv4 multicast
 * interface: on a router the default multicast route points at the WAN.
 */

#define MDNS_ADDR "224.0.0.251"
#define MDNS_PORT 5353
#define CAST_SERVICE "_googlecast._tcp.local"

enum { DNS_A = 1, DNS_PTR = 12, DNS_TXT = 16, DNS_SRV = 33 };

struct device {
	char instance[256]; /* PTR target, e.g. Chromecast-<id>._googlecast... */
	char target[256];   /* SRV target host name */
	char ip[INET_ADDRSTRLEN];
	int port;
	char name[128];     /* TXT fn= (friendly name) */
	char model[64];     /* TXT md= */
	char id[64];        /* TXT id= */
};

/*
 * Reads a possibly compressed name at off into out (dotted, no trailing
 * dot). Returns the offset just past the name in place, or -1.
 */
static int dns_name(const unsigned char *m, int len, int off, char *out,
		    size_t cap)
{
	int end = -1, hops = 0;
	size_t n = 0;

	*out = 0;
	while (off < len) {
		int l = m[off];
		if ((l & 0xc0) == 0xc0) {
			if (off + 1 >= len || ++hops > 16)
				return -1;
			if (end < 0)
				end = off + 2;
			off = (l & 0x3f) << 8 | m[off + 1];
			continue;
		}
		if (l & 0xc0)
			return -1;
		off++;
		if (!l)
			return end < 0 ? off : end;
		if (off + l > len || n + l + 2 > cap)
			return -1;
		if (n)
			out[n++] = '.';
		memcpy(out + n, m + off, l);
		n += l;
		out[n] = 0;
		off += l;
	}
	return -1;
}

static struct device *find_device(struct device *d, int n, const char *inst)
{
	for (int i = 0; i < n; i++)
		if (!strcasecmp(d[i].instance, inst))
			return &d[i];
	return NULL;
}

/* Copies the value of "key=value" TXT string p (length l) if key matches. */
static void txt_value(const unsigned char *p, int l, const char *key,
		      char *out, size_t cap)
{
	size_t k = strlen(key);

	if ((size_t)l <= k || memcmp(p, key, k) || p[k] != '=')
		return;
	l -= k + 1;
	if ((size_t)l >= cap)
		l = cap - 1;
	memcpy(out, p + k + 1, l);
	out[l] = 0;
}

static void parse_txt(struct device *d, const unsigned char *p, int len)
{
	for (int i = 0; i < len && i + 1 + p[i] <= len; i += 1 + p[i]) {
		txt_value(p + i + 1, p[i], "fn", d->name, sizeof(d->name));
		txt_value(p + i + 1, p[i], "md", d->model, sizeof(d->model));
		txt_value(p + i + 1, p[i], "id", d->id, sizeof(d->id));
	}
}

/*
 * Folds one response into the device list. Records come in any order and
 * across sections, so PTR creates the device and SRV/TXT/A fill it in;
 * the sender address is the fallback when no A record is included.
 */
static int parse_mdns(const unsigned char *m, int len, const char *from,
		      struct device *d, int n)
{
	char name[256], rdname[256];
	int off = 12, count, i;

	if (len < 12 || !(m[2] & 0x80)) /* not a response */
		return n;
	count = (m[6] << 8 | m[7]) + (m[8] << 8 | m[9]) + (m[10] << 8 | m[11]);
	for (i = 0; i < (m[4] << 8 | m[5]); i++) { /* skip echoed questions */
		if ((off = dns_name(m, len, off, name, sizeof(name))) < 0)
			return n;
		off += 4;
	}
	/* Two passes: PTRs first, so SRV/TXT/A can attach regardless of order. */
	for (int pass = 0; pass < 2; pass++) {
		int o = off;
		for (i = 0; i < count; i++) {
			int type, rdlen;
			struct device *dev;
			if ((o = dns_name(m, len, o, name, sizeof(name))) < 0 ||
			    o + 10 > len)
				break;
			type = m[o] << 8 | m[o + 1];
			rdlen = m[o + 8] << 8 | m[o + 9];
			o += 10;
			if (o + rdlen > len)
				break;
			if (pass == 0 && type == DNS_PTR &&
			    !strcasecmp(name, CAST_SERVICE) &&
			    dns_name(m, len, o, rdname, sizeof(rdname)) >= 0 &&
			    !find_device(d, n, rdname) && n < MAX_DEVICES) {
				memset(&d[n], 0, sizeof(d[n]));
				snprintf(d[n].instance, sizeof(d[n].instance),
					 "%s", rdname);
				snprintf(d[n].ip, sizeof(d[n].ip), "%s", from);
				n++;
			} else if (pass == 1 && type == DNS_SRV && rdlen > 6 &&
				   (dev = find_device(d, n, name))) {
				dev->port = m[o + 4] << 8 | m[o + 5];
				dns_name(m, len, o + 6, dev->target,
					 sizeof(dev->target));
			} else if (pass == 1 && type == DNS_TXT &&
				   (dev = find_device(d, n, name))) {
				parse_txt(dev, m + o, rdlen);
			}
			o += rdlen;
		}
	}
	/* A records: match SRV targets of devices from this packet. */
	for (i = 0; i < count; i++) {
		int type, rdlen;
		if ((off = dns_name(m, len, off, name, sizeof(name))) < 0 ||
		    off + 10 > len)
			break;
		type = m[off] << 8 | m[off + 1];
		rdlen = m[off + 8] << 8 | m[off + 9];
		off += 10;
		if (off + rdlen > len)
			break;
		if (type == DNS_A && rdlen == 4)
			for (int j = 0; j < n; j++)
				if (!strcasecmp(d[j].target, name))
					inet_ntop(AF_INET, m + off, d[j].ip,
						  sizeof(d[j].ip));
		off += rdlen;
	}
	return n;
}

static void mdns_query(int fd)
{
	unsigned char q[64] = { 0, 0, 0, 0, 0, 1 }; /* id 0, 1 question */
	size_t n = 12;
	const char *label = CAST_SERVICE, *dot;
	struct sockaddr_in to = { .sin_family = AF_INET,
				  .sin_port = htons(MDNS_PORT) };
	struct ifaddrs *ifs, *i;

	do {
		dot = strchr(label, '.');
		size_t l = dot ? (size_t)(dot - label) : strlen(label);
		q[n++] = l;
		memcpy(q + n, label, l);
		n += l;
		label = dot + 1;
	} while (dot);
	q[n++] = 0;
	q[n++] = 0; q[n++] = DNS_PTR;
	q[n++] = 0x80; q[n++] = 1; /* class IN, unicast response requested */
	inet_pton(AF_INET, MDNS_ADDR, &to.sin_addr);

	if (getifaddrs(&ifs))
		die("getifaddrs failed");
	for (i = ifs; i; i = i->ifa_next) {
		struct in_addr a;
		if (!i->ifa_addr || i->ifa_addr->sa_family != AF_INET ||
		    !(i->ifa_flags & IFF_UP) || !(i->ifa_flags & IFF_MULTICAST) ||
		    (i->ifa_flags & IFF_LOOPBACK))
			continue;
		a = ((struct sockaddr_in *)i->ifa_addr)->sin_addr;
		if (setsockopt(fd, IPPROTO_IP, IP_MULTICAST_IF, &a, sizeof(a)) ||
		    sendto(fd, q, n, 0, (struct sockaddr *)&to, sizeof(to)) < 0) {
			if (verbose)
				fprintf(stderr, "mdns: send via %s failed\n",
					i->ifa_name);
		} else if (verbose) {
			fprintf(stderr, "mdns: query via %s\n", i->ifa_name);
		}
	}
	freeifaddrs(ifs);
}

static long now_ms(void)
{
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return ts.tv_sec * 1000L + ts.tv_nsec / 1000000;
}

static int device_matches(const struct device *d, const char *want)
{
	return !strcasecmp(d->name, want) || !strcasecmp(d->id, want);
}

/*
 * Collects devices that answer within ms milliseconds; with want set,
 * returns as soon as a device with that name or id shows up.
 */
static int discover(struct device *d, int ms, const char *want)
{
	int fd = socket(AF_INET, SOCK_DGRAM, 0), n = 0;
	unsigned char ttl = 255;
	long start = now_ms(), resent = 0;

	if (fd < 0)
		die("socket failed");
	setsockopt(fd, IPPROTO_IP, IP_MULTICAST_TTL, &ttl, sizeof(ttl));
	mdns_query(fd);
	for (;;) {
		long left = start + ms - now_ms();
		struct pollfd p = { .fd = fd, .events = POLLIN };
		unsigned char buf[9000];
		struct sockaddr_in from;
		socklen_t fl = sizeof(from);
		char ip[INET_ADDRSTRLEN];
		int len;

		if (left <= 0)
			break;
		/* UDP is lossy: repeat the query once halfway through. */
		if (!resent && left > ms / 2) {
			if (poll(&p, 1, left - ms / 2) <= 0) {
				resent = 1;
				mdns_query(fd);
				continue;
			}
		} else if (poll(&p, 1, left) <= 0) {
			continue;
		}
		len = recvfrom(fd, buf, sizeof(buf), 0,
			       (struct sockaddr *)&from, &fl);
		if (len <= 0)
			continue;
		inet_ntop(AF_INET, &from.sin_addr, ip, sizeof(ip));
		if (verbose)
			fprintf(stderr, "mdns: %d bytes from %s\n", len, ip);
		n = parse_mdns(buf, len, ip, d, n);
		for (int i = 0; want && i < n; i++)
			if (device_matches(&d[i], want))
				goto out;
	}
out:
	close(fd);
	return n;
}

static void print_devices(const struct device *d, int n)
{
	char name[300], model[150], id[150];

	printf("[");
	for (int i = 0; i < n; i++) {
		jescape(name, sizeof(name), d[i].name);
		jescape(model, sizeof(model), d[i].model);
		jescape(id, sizeof(id), d[i].id);
		printf("%s{\"name\":%s,\"host\":\"%s\",\"port\":%d,"
		       "\"model\":%s,\"id\":%s}", i ? "," : "", name, d[i].ip,
		       d[i].port ? d[i].port : atoi(DEFAULT_PORT), model, id);
	}
	printf("]\n");
}

/*
 * Lets -H take a friendly name ("Living Room TV") or device id: anything
 * that does not resolve as a host is looked up via discovery.
 */
static void resolve_device(char *host, size_t cap, const char **port)
{
	static char portbuf[8];
	struct addrinfo hints = { .ai_family = AF_UNSPEC }, *res;
	struct device d[MAX_DEVICES];
	int n;

	if (!strchr(host, ' ') && !getaddrinfo(host, NULL, &hints, &res)) {
		freeaddrinfo(res);
		return;
	}
	n = discover(d, DISCOVER_MS, host);
	for (int i = 0; i < n; i++) {
		if (device_matches(&d[i], host)) {
			snprintf(host, cap, "%s", d[i].ip);
			snprintf(portbuf, sizeof(portbuf), "%d",
				 d[i].port ? d[i].port : atoi(DEFAULT_PORT));
			*port = portbuf;
			return;
		}
	}
	die("device \"%s\" not found", host);
}

/* ------------------------------------------------------------------ */
/* Commands                                                            */

static void usage(void)
{
	fprintf(stderr,
"usage: castctl [-v] -H host[:port] <command> [args]\n"
"       castctl [-v] discover [-t seconds]\n"
"\n"
"Host may also be a device's friendly name or id, found via mDNS.\n"
"\n"
"commands:\n"
"  discover [-t seconds]               list Chromecasts on the local network\n"
"                                      as JSON (listens 2 s by default)\n"
"  play <url> [-t content-type] [-l] [-s url lang name]...\n"
"                                      load and play a stream (HLS by default;\n"
"                                      -l marks it as a live stream); waits\n"
"                                      until playback starts or fails; each -s\n"
"                                      adds a WebVTT subtitle track (off until\n"
"                                      selected with \"subs\")\n"
"  pause | resume                      pause / resume playback\n"
"  stop                                stop media\n"
"  quit                                close the app on the device\n"
"  seek <seconds>                      seek to absolute position\n"
"  ff <seconds> | rew <seconds>        seek forward / backward\n"
"  audio <id|lang|name>                switch audio track (see \"tracks\"\n"
"                                      in status; name matches substring)\n"
"  subs <id|lang|name|off>             switch / disable subtitles\n"
"  volume <0..100>                     set device volume\n"
"  mute | unmute\n"
"  status                              print JSON status\n"
"\n"
"Host can also be given via CASTCTL_HOST environment variable.\n");
	exit(2);
}

/* Connects to the running app and its media session; dies if none. */
static void attach_media(struct app *a, struct media *md)
{
	if (!receiver_status(a))
		die("nothing is playing");
	virtual_connect(a->transport_id);
	media_status(a->transport_id, md);
	if (!md->session_id)
		die("no active media session");
}

static void media_command(struct app *a, struct media *md, const char *body)
{
	char buf[512];
	const char *r;

	snprintf(buf, sizeof(buf), "%s,\"mediaSessionId\":%ld", body,
		 md->session_id);
	r = request(a->transport_id, NS_MEDIA, buf);
	check_reply_type(r, "command");
	parse_media_status(r, md);
}

/*
 * The reply to LOAD is not a reliable verdict: the receiver may answer with
 * an early MEDIA_STATUS while the player is still IDLE and send LOAD_FAILED
 * (or IDLE with an idleReason) only once the stream turns out to be broken.
 * Reads the media channel until playback starts or fails. Still BUFFERING at
 * the deadline counts as started: the network may just be slow.
 */
static void wait_for_playback(const char *transport, const char *r,
			      struct media *md)
{
	time_t deadline = time(NULL) + LOAD_TIMEOUT_S;
	long session = 0;
	struct cast_msg m;
	char type[48];
	double v;

	for (;;) {
		check_reply_type(r, "load");
		jstr(r, "type", type, sizeof(type));
		/* Statuses of the session replaced by this LOAD are ignored. */
		if (!strcmp(type, "MEDIA_STATUS") &&
		    (!session || !jnum(r, "status.0.mediaSessionId", &v) ||
		     (long)v == session)) {
			parse_media_status(r, md);
			if (!md->session_id) {
				if (session)
					die("load failed: media session ended");
			} else {
				session = md->session_id;
				if (!strcmp(md->state, "PLAYING") ||
				    !strcmp(md->state, "PAUSED"))
					return;
				if (!strcmp(md->state, "IDLE") && *md->idle_reason)
					die("load failed: IDLE %s", md->idle_reason);
			}
		}
		if (time(NULL) >= deadline) {
			if (!strcmp(md->state, "BUFFERING"))
				return;
			die("load failed: playback did not start in %d s",
			    LOAD_TIMEOUT_S);
		}
		/* Heartbeat PINGs every few seconds keep this from blocking long. */
		for (;;) {
			cast_recv(&m);
			if (!strcmp(m.ns, NS_CONN)) {
				if (jstr(m.payload, "type", type, sizeof(type)) &&
				    !strcmp(type, "CLOSE") && !strcmp(m.src, transport))
					die("device closed the channel");
				continue;
			}
			if (!strcmp(m.ns, NS_MEDIA) && !strcmp(m.src, transport))
				break;
			if (time(NULL) >= deadline)
				break;
		}
		r = m.payload;
	}
}

/*
 * Subtitles that are not in the HLS manifest are side-loaded: LOAD lists them
 * as WebVTT text tracks, and they can be switched with "subs" afterwards.
 * The receiver fetches them itself, so the server must allow CORS.
 */
static size_t append_subs(char *body, size_t n, size_t cap, char **subs,
			  int nsubs)
{
	char url[4200], lang[64], name[200];
	int i;

	if (!nsubs)
		return n;
	n += snprintf(body + n, cap - n, ",\"tracks\":[");
	for (i = 0; i < nsubs; i++) {
		jescape(url, sizeof(url), subs[i * 3]);
		jescape(lang, sizeof(lang), subs[i * 3 + 1]);
		jescape(name, sizeof(name), subs[i * 3 + 2]);
		n += snprintf(body + n, cap - n,
			      "%s{\"trackId\":%d,\"type\":\"TEXT\","
			      "\"subtype\":\"SUBTITLES\",\"trackContentId\":%s,"
			      "\"trackContentType\":\"text/vtt\","
			      "\"language\":%s,\"name\":%s}",
			      i ? "," : "", SUB_TRACK_BASE + i, url, lang, name);
		if (n >= cap)
			die("too many subtitles");
	}
	return n + snprintf(body + n, cap - n, "]");
}

static void cmd_play(int argc, char **argv)
{
	static char body[MAX_MSG - 256];
	char url[4200], ctype[200];
	const char *content_type = "application/x-mpegURL";
	const char *stream_type = "BUFFERED";
	const char *src = NULL;
	char *subs[MAX_SUBS * 3];
	int nsubs = 0;
	struct app a;
	struct media md;
	const char *r;
	size_t n;
	int i;

	for (i = 1; i < argc; i++) {
		if (!strcmp(argv[i], "-t") && i + 1 < argc)
			content_type = argv[++i];
		else if (!strcmp(argv[i], "-l"))
			stream_type = "LIVE";
		else if (!strcmp(argv[i], "-s") && i + 3 < argc) {
			if (nsubs == MAX_SUBS)
				die("at most %d subtitle tracks", MAX_SUBS);
			memcpy(&subs[nsubs++ * 3], &argv[i + 1], 3 * sizeof(*argv));
			i += 3;
		} else if (!src)
			src = argv[i];
		else
			usage();
	}
	if (!src)
		usage();

	if (!receiver_status(&a) || strcmp(a.app_id, DEFAULT_MEDIA_RECEIVER))
		launch_media_receiver(&a);
	virtual_connect(a.transport_id);

	jescape(url, sizeof(url), src);
	jescape(ctype, sizeof(ctype), content_type);
	n = snprintf(body, sizeof(body),
		     "\"type\":\"LOAD\",\"autoplay\":true,\"currentTime\":0,"
		     "\"media\":{\"contentId\":%s,\"contentType\":%s,"
		     "\"streamType\":\"%s\"",
		     url, ctype, stream_type);
	n = append_subs(body, n, sizeof(body) - 2, subs, nsubs);
	snprintf(body + n, sizeof(body) - n, "}");
	r = request(a.transport_id, NS_MEDIA, body);
	memset(&md, 0, sizeof(md));
	md.ntracks = md.nactive = -1;
	wait_for_playback(a.transport_id, r, &md);
	print_status(&a, &md);
}

static double parse_seconds(const char *s)
{
	char *end;
	double v = strtod(s, &end);
	if (end == s || *end || v < 0)
		die("invalid number of seconds: %s", s);
	return v;
}

static void cmd_seek(struct app *a, struct media *md, double t)
{
	char body[128];

	if (t < 0)
		t = 0;
	if (md->duration > 0 && t > md->duration)
		t = md->duration;
	snprintf(body, sizeof(body), "\"type\":\"SEEK\",\"currentTime\":%.3f", t);
	media_command(a, md, body);
}

static void cmd_volume(const char *field, const char *value)
{
	char body[128];
	struct app a;
	const char *r;

	snprintf(body, sizeof(body),
		 "\"type\":\"SET_VOLUME\",\"volume\":{\"%s\":%s}", field, value);
	r = request(RECEIVER, NS_RECEIVER, body);
	check_reply_type(r, "volume");
	if (receiver_status(&a) && *a.transport_id) {
		struct media md;
		virtual_connect(a.transport_id);
		media_status(a.transport_id, &md);
		print_status(&a, &md);
	} else {
		print_status(&a, NULL);
	}
}

static const struct track *find_track(const struct media *md, const char *type,
				     const char *sel)
{
	char *end;
	long id = strtol(sel, &end, 10);
	int i;

	for (i = 0; i < md->ntracks; i++)
		if (!strcmp(md->tracks[i].type, type) &&
		    (*end ? !strcasecmp(md->tracks[i].lang, sel) : md->tracks[i].id == id))
			return &md->tracks[i];
	for (i = 0; *end && i < md->ntracks; i++)
		if (!strcmp(md->tracks[i].type, type) &&
		    strcasestr(md->tracks[i].name, sel))
			return &md->tracks[i];
	return NULL;
}

/*
 * Replaces the active track of the given type (AUDIO or TEXT) with the
 * selected one; sel == NULL just disables tracks of that type.
 */
static void cmd_select_track(struct app *a, struct media *md, const char *type,
			     const char *sel)
{
	char body[512];
	const struct track *t = NULL;
	size_t n;
	int i, j;

	if (md->ntracks <= 0)
		die("device reports no tracks for this media");
	if (sel && !(t = find_track(md, type, sel)))
		die("no %s track matching \"%s\"", type, sel);

	n = snprintf(body, sizeof(body),
		     "\"type\":\"EDIT_TRACKS_INFO\",\"activeTrackIds\":[");
	for (i = 0; i < md->nactive; i++) {
		int same_type = 0;
		for (j = 0; j < md->ntracks; j++)
			if (md->tracks[j].id == md->active[i] &&
			    !strcmp(md->tracks[j].type, type))
				same_type = 1;
		if (!same_type)
			n += snprintf(body + n, sizeof(body) - n, "%ld,",
				      md->active[i]);
	}
	if (t)
		n += snprintf(body + n, sizeof(body) - n, "%ld,", t->id);
	if (body[n - 1] == ',')
		n--;
	snprintf(body + n, sizeof(body) - n, "]");
	media_command(a, md, body);
}

int main(int argc, char **argv)
{
	char host[256], *colon;
	const char *port = DEFAULT_PORT;
	const char *h = getenv("CASTCTL_HOST");
	const char *cmd;
	struct app a;
	struct media md;
	int opt;

	while ((opt = getopt(argc, argv, "+vH:h")) != -1) {
		switch (opt) {
		case 'v': verbose = 1; break;
		case 'H': h = optarg; break;
		default: usage();
		}
	}
	argc -= optind;
	argv += optind;
	if (argc < 1)
		usage();
	cmd = argv[0];

	if (!strcmp(cmd, "discover")) {
		struct device d[MAX_DEVICES];
		double t = DISCOVER_MS / 1000.0;
		if (argc == 3 && !strcmp(argv[1], "-t"))
			t = parse_seconds(argv[2]);
		else if (argc != 1)
			usage();
		if (t <= 0 || t > 60)
			die("timeout must be 0..60 seconds");
		print_devices(d, discover(d, t * 1000, NULL));
		return 0;
	}
	if (!h || !*h)
		usage();

	snprintf(host, sizeof(host), "%s", h);
	if ((colon = strrchr(host, ':')) && !strchr(colon + 1, ']') &&
	    colon == strchr(host, ':')) {
		*colon = 0;
		port = colon + 1;
	}
	resolve_device(host, sizeof(host), &port);

	tls_connect(host, port);
	virtual_connect(RECEIVER);

	if (!strcmp(cmd, "play")) {
		cmd_play(argc, argv);
	} else if (!strcmp(cmd, "status")) {
		if (receiver_status(&a)) {
			virtual_connect(a.transport_id);
			media_status(a.transport_id, &md);
			print_status(&a, &md);
		} else {
			print_status(&a, NULL);
		}
	} else if (!strcmp(cmd, "pause") || !strcmp(cmd, "resume") ||
		   !strcmp(cmd, "stop")) {
		attach_media(&a, &md);
		media_command(&a, &md, !strcmp(cmd, "pause") ?
			      "\"type\":\"PAUSE\"" : !strcmp(cmd, "resume") ?
			      "\"type\":\"PLAY\"" : "\"type\":\"STOP\"");
		print_status(&a, &md);
	} else if (!strcmp(cmd, "seek") || !strcmp(cmd, "ff") ||
		   !strcmp(cmd, "rew")) {
		double t;
		if (argc != 2)
			usage();
		t = parse_seconds(argv[1]);
		attach_media(&a, &md);
		if (!strcmp(cmd, "ff"))
			t = md.time + t;
		else if (!strcmp(cmd, "rew"))
			t = md.time - t;
		cmd_seek(&a, &md, t);
		print_status(&a, &md);
	} else if (!strcmp(cmd, "audio") || !strcmp(cmd, "subs")) {
		int is_audio = !strcmp(cmd, "audio");
		if (argc != 2)
			usage();
		attach_media(&a, &md);
		cmd_select_track(&a, &md, is_audio ? "AUDIO" : "TEXT",
				 !is_audio && !strcmp(argv[1], "off") ? NULL : argv[1]);
		print_status(&a, &md);
	} else if (!strcmp(cmd, "quit")) {
		char body[128];
		if (receiver_status(&a)) {
			snprintf(body, sizeof(body),
				 "\"type\":\"STOP\",\"sessionId\":\"%s\"",
				 a.session_id);
			check_reply_type(request(RECEIVER, NS_RECEIVER, body),
					 "quit");
			receiver_status(&a);
		}
		print_status(&a, NULL);
	} else if (!strcmp(cmd, "volume")) {
		char v[16];
		if (argc != 2)
			usage();
		snprintf(v, sizeof(v), "%.2f", parse_seconds(argv[1]) / 100.0);
		if (atof(v) > 1)
			die("volume must be 0..100");
		cmd_volume("level", v);
	} else if (!strcmp(cmd, "mute") || !strcmp(cmd, "unmute")) {
		cmd_volume("muted", !strcmp(cmd, "mute") ? "true" : "false");
	} else {
		usage();
	}

	tls_close();
	return 0;
}
