#!/bin/sh
set -eu

# Docker creates a fresh named volume as root. Initialise only the durable
# workspace, then drop privileges before AetherRelay reads configuration or
# serves traffic. The mounted configuration directory is deliberately left
# untouched: host ownership remains explicit and predictable.
if [ "$(id -u)" = "0" ]; then
  mkdir -p /var/lib/aetherrelay
  chown -R aetherrelay:aetherrelay /var/lib/aetherrelay
  exec gosu aetherrelay "$@"
fi

exec "$@"
