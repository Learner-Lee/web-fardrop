#!/usr/bin/env bash
set -euo pipefail
cd /storage/blog/web-fardrop
IFS= read -r edge_token
[[ "${edge_token}" =~ ^[0-9a-f]{64}$ ]]
install -d -m 0700 /etc/web-fardrop
umask 077
{
  printf '%s\n' 'FARDROP_LISTEN=127.0.0.1:18171'
  printf '%s\n' 'FARDROP_ORIGIN=http://fardrop-origin'
  printf '%s\n' 'FARDROP_CACHE_MIB=512'
  printf 'FARDROP_EDGE_TOKEN=%s\n' "$edge_token"
} > /etc/web-fardrop/edge.env.new
mv /etc/web-fardrop/edge.env.new /etc/web-fardrop/edge.env
install -m 0755 fardrop.new fardrop.next
mv fardrop.next fardrop
install -m 0644 edge.service /etc/systemd/system/web-fardrop-edge.service
systemctl daemon-reload
systemctl enable web-fardrop-edge.service
systemctl restart web-fardrop-edge.service
systemctl is-active web-fardrop-edge.service
