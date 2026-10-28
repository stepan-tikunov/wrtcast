#!/bin/sh
# Installs wrtcast on the router and creates a dedicated uhttpd instance
# on port 8088: /api/* is aliased to the CGI script, /www/cast is the web root.
# Existing /etc/config/wrtcast is kept. Adds a cron job for watch progress.
# Needs build/sqlite.so (scripts/build-sqlite.sh).
set -e
cd "$(dirname "$0")/.."
ROUTER=${ROUTER:-root@192.168.1.1}
SSH="ssh ${SSH_OPTS:--o ControlPath=/tmp/ssh-owrt} $ROUTER"

$SSH "mkdir -p /www/cast/cgi-bin && cat > /www/cast/cgi-bin/wrtcast && chmod 755 /www/cast/cgi-bin/wrtcast" < wrtcast.uc
$SSH "cat > /www/cast/index.html" < www/index.html
$SSH "[ -f /etc/config/wrtcast ] || cat > /etc/config/wrtcast" < files/wrtcast.config
# The state's SQLite module (scripts/build-sqlite.sh) and its library.
$SSH "opkg list-installed libsqlite3-0 | grep -q . || opkg install libsqlite3-0"
$SSH "cat > /usr/lib/ucode/sqlite.so.new && mv /usr/lib/ucode/sqlite.so.new /usr/lib/ucode/sqlite.so" < build/sqlite.so
# The volume leveler's ffmpeg, if built (scripts/build-ffmpeg.sh); unchanged
# copies are skipped, it takes a third of a small overlay.
if [ -f build/wrtcast-ffmpeg ]; then
	SUM=$(shasum -a 256 build/wrtcast-ffmpeg | cut -d' ' -f1)
	$SSH "echo '$SUM  /usr/bin/wrtcast-ffmpeg' | sha256sum -c -s 2>/dev/null" ||
		$SSH "cat > /usr/bin/wrtcast-ffmpeg.new && chmod 755 /usr/bin/wrtcast-ffmpeg.new &&
			mv /usr/bin/wrtcast-ffmpeg.new /usr/bin/wrtcast-ffmpeg" < build/wrtcast-ffmpeg
fi
$SSH sh -s <<'REMOTE'
set -e
if [ "$(uci -q get uhttpd.cast)" != uhttpd ]; then
	uci set uhttpd.cast=uhttpd
	uci add_list uhttpd.cast.listen_http='0.0.0.0:8088'
	uci add_list uhttpd.cast.listen_http='[::]:8088'
	uci set uhttpd.cast.home='/www/cast'
	uci set uhttpd.cast.cgi_prefix='/cgi-bin'
	uci set uhttpd.cast.rfc1918_filter='1'
fi
uci -q delete uhttpd.cast.alias || true
uci add_list uhttpd.cast.alias='/api=/cgi-bin/wrtcast'
# /search and /title/<id> are the page's own addresses.
uci set uhttpd.cast.error_page='/index.html'
# The receiver fetches audio and video segments in parallel with UI polling.
uci set uhttpd.cast.max_requests='10'
uci set uhttpd.cast.script_timeout='60'
uci commit uhttpd
/etc/init.d/uhttpd reload
# Records watch progress while no page polls the status.
CRON='* * * * * /www/cast/cgi-bin/wrtcast tick'
if ! grep -qF "$CRON" /etc/crontabs/root 2>/dev/null; then
	echo "$CRON" >> /etc/crontabs/root
	/etc/init.d/cron enable
	/etc/init.d/cron restart
fi
# The watch history survives firmware upgrades.
grep -qx /etc/wrtcast.db /etc/sysupgrade.conf ||
	echo /etc/wrtcast.db >> /etc/sysupgrade.conf
REMOTE
echo "deployed: http://${ROUTER#*@}:8088/api/status"
