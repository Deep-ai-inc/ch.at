#!/usr/bin/env bash
# Installed root-owned at /usr/local/sbin/chat-deploy. A dedicated SSH key uses
# command="sudo -n /usr/local/sbin/chat-deploy",restrict in authorized_keys.
set -euo pipefail
umask 077
exec 9>/run/lock/chat-deploy.lock
flock -w 120 9

incoming=$(mktemp /usr/local/bin/.chat-incoming.XXXXXX)
trap 'rm -f "$incoming"' EXIT
# Limit both upload time and binary size before touching the running service.
timeout 120 head -c 52428801 > "$incoming"
size=$(stat -c %s "$incoming")
test "$size" -gt 1048576
test "$size" -le 52428800
test "$(od -An -tx1 -N4 "$incoming" | tr -d ' \n')" = 7f454c46
chmod 755 "$incoming"
chown root:root "$incoming"

cp -p /usr/local/bin/chat /usr/local/bin/chat.previous
mv -f "$incoming" /usr/local/bin/chat
healthy=false
if systemctl restart chat; then
  for attempt in {1..15}; do
    if systemctl is-active --quiet chat && \
       curl --fail --silent --show-error --max-time 2 http://127.0.0.1/board > /dev/null; then
      healthy=true
      break
    fi
    sleep 1
  done
fi
if [ "$healthy" != true ]; then
  cp -p /usr/local/bin/chat.previous "$incoming"
  mv -f "$incoming" /usr/local/bin/chat
  systemctl restart chat
  echo 'Health check failed; restored the previous binary.' >&2
  exit 1
fi
sha256sum /usr/local/bin/chat
