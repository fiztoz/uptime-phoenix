#!/usr/bin/env bash
# Phoenix RabbitMQ monitor — create a least-privilege AMQP user.
#
# Usage (edit variables below, then):
#   bash create-user-rabbitmq.sh
#
# Or with env overrides:
#   PHOENIX_RABBITMQ_PASS='secret' PHOENIX_RABBITMQ_QUEUE='health-check' \
#     bash create-user-rabbitmq.sh
#
# Requires rabbitmqctl on PATH (or run inside the RabbitMQ container):
#   kubectl exec -i rabbitmq-0 -- bash -s < create-user-rabbitmq.sh
#
# Phoenix check modes:
#   1) Connect + channel only  → leave QUEUE and EXCHANGE empty
#   2) Passive queue check    → set QUEUE (needs configure on that name)
#   3) Passive exchange check → set EXCHANGE (configure on that name)

set -euo pipefail

# === EDIT THESE (or set env vars) ===
USER="${PHOENIX_RABBITMQ_USER:-phoenix_monitor}"
PASS="${PHOENIX_RABBITMQ_PASS:-CHANGE_ME_STRONG_PASSWORD}"
VHOST="${PHOENIX_RABBITMQ_VHOST:-/}"
QUEUE="${PHOENIX_RABBITMQ_QUEUE:-}"
EXCHANGE="${PHOENIX_RABBITMQ_EXCHANGE:-}"

if [[ "$PASS" == "CHANGE_ME_STRONG_PASSWORD" ]]; then
  echo "WARNING: using placeholder password. Set PHOENIX_RABBITMQ_PASS or edit this script." >&2
fi

if ! command -v rabbitmqctl >/dev/null 2>&1; then
  echo "rabbitmqctl not found. Run this on a RabbitMQ node or inside the broker container." >&2
  exit 1
fi

if [[ "$VHOST" != "/" ]]; then
  rabbitmqctl add_vhost "$VHOST" 2>/dev/null || true
fi

if rabbitmqctl list_users 2>/dev/null | awk '{print $1}' | grep -qx "$USER"; then
  rabbitmqctl change_password "$USER" "$PASS"
  echo "Updated password for user $USER"
else
  rabbitmqctl add_user "$USER" "$PASS"
  echo "Created user $USER"
fi

rabbitmqctl set_user_tags "$USER" monitoring

# Passive declare requires configure permission matching the resource name.
# Connect + channel only needs empty configure/write/read.
escape_regex() {
  # Escape regex metacharacters commonly found in queue/exchange names.
  printf '%s' "$1" | sed -e 's/[.[\*^$()+?{|]/g' -e 's/\\/\\\\/g' -e 's/\./\\./g'
}

CONFIGURE=""
if [[ -n "$QUEUE" || -n "$EXCHANGE" ]]; then
  parts=()
  [[ -n "$QUEUE" ]] && parts+=("$(escape_regex "$QUEUE")")
  [[ -n "$EXCHANGE" ]] && parts+=("$(escape_regex "$EXCHANGE")")
  IFS='|'
  CONFIGURE="^(${parts[*]})$"
  unset IFS
fi

rabbitmqctl set_permissions -p "$VHOST" "$USER" "$CONFIGURE" "" ""

echo
echo "Done."
echo "  user:       $USER"
echo "  vhost:      $VHOST"
echo "  configure:  ${CONFIGURE:-'(empty — connect/channel only)'}"
echo "  write/read: (empty — no publish/consume)"
echo
if [[ "$VHOST" == "/" ]]; then
  vhost_enc="%2F"
else
  vhost_enc="$VHOST"
fi
echo "Phoenix AMQP URL example:"
echo "  amqp://${USER}:********@HOST:5672/${vhost_enc}"
if [[ -n "$QUEUE" ]]; then
  echo "Set monitor Queue field to: $QUEUE"
fi
if [[ -n "$EXCHANGE" ]]; then
  echo "Set monitor Exchange field to: $EXCHANGE (match Exchange type in the UI)"
fi
