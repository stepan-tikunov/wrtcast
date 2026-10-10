#!/bin/sh
# Installs wrtcast on the router and creates a dedicated uhttpd instance
# on port 8088: /api/* is aliased to the CGI script, /www/cast is the web root.
# Existing /etc/config/wrtcast is kept.
set -e
cd "$(dirname "$0")/.."
ROUTER=${ROUTER:-root@192.168.1.1}
SSH="ssh ${SSH_OPTS:--o ControlPath=/tmp/ssh-owrt} $ROUTER"

$SSH "mkdir -p /www/cast/cgi-bin && cat > /www/cast/cgi-bin/wrtcast && chmod 755 /www/cast/cgi-bin/wrtcast" < wrtcast.uc
$SSH "cat > /www/cast/index.html" < www/index.html
$SSH "[ -f /etc/config/wrtcast ] || cat > /etc/config/wrtcast" < files/wrtcast.config
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
# The receiver fetches audio and video segments in parallel with UI polling.
uci set uhttpd.cast.max_requests='10'
uci set uhttpd.cast.script_timeout='60'
uci commit uhttpd
/etc/init.d/uhttpd reload
REMOTE
echo "deployed: http://${ROUTER#*@}:8088/api/status"
