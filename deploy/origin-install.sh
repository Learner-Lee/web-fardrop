#!/usr/bin/env bash
set -euo pipefail
cd /storage/Project/web-fardrop
if ! id web-fardrop >/dev/null 2>&1; then
  useradd --system -M -d /storage/Project/web-fardrop/data -s /usr/sbin/nologin web-fardrop
fi
install -d -m 0700 -o web-fardrop -g web-fardrop data
install -d -m 0700 /etc/web-fardrop
if [[ ! -e /etc/web-fardrop/origin.env ]]; then
  umask 077
  {
    printf '%s\n' 'FARDROP_LISTEN=127.0.0.1:18172'
    printf '%s\n' 'FARDROP_EDGE_IP=127.0.0.1'
    printf '%s\n' 'FARDROP_TUNNEL_URL=wss://web.alenli.cn/about/fardrop/_origin'
    printf '%s\n' 'FARDROP_DATA=/storage/Project/web-fardrop/data'
    printf 'FARDROP_EDGE_TOKEN=%s\n' "$(openssl rand -hex 32)"
    printf 'FARDROP_ADMIN_TOKEN=%s\n' "$(openssl rand -hex 32)"
  } > /etc/web-fardrop/origin.env.new
  mv /etc/web-fardrop/origin.env.new /etc/web-fardrop/origin.env
fi
install -m 0755 fardrop.new fardrop.next
mv fardrop.next fardrop
install -m 0644 origin.service /etc/systemd/system/web-fardrop-origin.service
systemctl daemon-reload
systemctl enable web-fardrop-origin.service
systemctl restart web-fardrop-origin.service
systemctl is-active web-fardrop-origin.service
