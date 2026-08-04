#!/bin/sh
set -eu

umask 077

if [ -n "${RAILWAY_SERVICE_ID:-}" ] && [ -z "${RAILWAY_VOLUME_MOUNT_PATH:-}" ] && [ -z "${GROK2API_STATE_DIR:-}" ]; then
  echo "Railway deployment requires a persistent Volume (recommended mount path: /data)" >&2
  echo "attach a Volume or set GROK2API_STATE_DIR to its mount path" >&2
  exit 1
fi

state_dir="${GROK2API_STATE_DIR:-${RAILWAY_VOLUME_MOUNT_PATH:-/app/data}}"
config_source="${GROK2API_CONFIG_SOURCE:-}"
active_config=/app/config.yaml
persistent_config="${state_dir}/config.yaml"
quality_guard_dir=/var/lib/grok2api-quality-guard

mkdir -p "${state_dir}"
mkdir -p "${quality_guard_dir}"
if [ "$(id -u)" = "0" ]; then
  chown grok2api:grok2api "${state_dir}"
  chown grok2api:grok2api "${quality_guard_dir}"
fi
chmod 0700 "${quality_guard_dir}"

if [ -n "${config_source}" ] && [ -f "${config_source}" ]; then
  # Explicitly mounted configs retain the original Docker Compose behavior.
  cp "${config_source}" "${active_config}"
else
  if [ ! -f "${persistent_config}" ]; then
    echo "initializing persistent config: ${persistent_config}" >&2
    /app/grok2api init-config \
      --template /usr/share/grok2api/config.example.yaml \
      --output "${persistent_config}" \
      --state-dir "${state_dir}" \
      --static-path /app/frontend/dist
  fi
  active_config="${persistent_config}"
fi

chmod 0600 "${active_config}"
if [ "$(id -u)" = "0" ]; then
  chown grok2api:grok2api "${active_config}"
fi

if [ "${1:-}" = "/app/grok2api" ] && [ "${2:-}" != "init-config" ]; then
  set -- "$@" --config "${active_config}" --listen "0.0.0.0:${PORT:-8000}"
fi

if [ "$(id -u)" = "0" ]; then
  exec su-exec grok2api:grok2api "$@"
fi
exec "$@"
