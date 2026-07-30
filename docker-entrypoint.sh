#!/bin/sh
set -eu

# Docker creates a fresh named volume as root. Initialise only the durable
# workspace, then drop privileges before ai-proxy reads configuration or
# serves traffic. The mounted configuration directory is deliberately left
# untouched: host ownership remains explicit and predictable.
if [ "$(id -u)" = "0" ]; then
  mkdir -p /var/lib/ai-proxy
  chown -R ai-proxy:ai-proxy /var/lib/ai-proxy
  exec gosu ai-proxy "$@"
fi

exec "$@"
