#!/bin/sh
# Monthly heartbeats partition maintenance for MariaDB (DESIGN.md §6).
# Requires: mysql client, DB_HOST, DB_USER, DB_PASSWORD, DB_NAME
set -eu

RETENTION_MONTHS="${RETENTION_MONTHS:-12}"

if [ -z "${DB_HOST:-}" ] || [ -z "${DB_USER:-}" ] || [ -z "${DB_PASSWORD:-}" ]; then
  echo "partition-manage: missing DB_HOST, DB_USER, or DB_PASSWORD" >&2
  exit 1
fi

DB_NAME="${DB_NAME:-phoenix}"

mysql_exec() {
  MYSQL_PWD="${DB_PASSWORD}" mysql -h "${DB_HOST}" -u "${DB_USER}" "${DB_NAME}" -N -e "$1"
}

# Next calendar month boundary for LESS THAN (UTC).
next_month=$(date -u -d "$(date -u +%Y-%m-01) +1 month" +%Y-%m-01 2>/dev/null || date -u -v1d -v+1m +%Y-%m-01)
part_name="p$(date -u -d "${next_month}" +%Y%m 2>/dev/null || date -u -j -f "%Y-%m-%d" "${next_month}" +%Y%m)"

echo "partition-manage: ensuring partition ${part_name} VALUES LESS THAN (${next_month})"

exists=$(mysql_exec "SELECT COUNT(*) FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA='${DB_NAME}' AND TABLE_NAME='heartbeats' AND PARTITION_NAME='${part_name}'" || echo "0")
if [ "${exists}" = "0" ]; then
  mysql_exec "ALTER TABLE heartbeats ADD PARTITION (PARTITION ${part_name} VALUES LESS THAN (UNIX_TIMESTAMP('${next_month} 00:00:00')))"
  echo "partition-manage: added ${part_name}"
else
  echo "partition-manage: ${part_name} already exists"
fi

# Drop partitions older than retention (best-effort; names pYYYYMM).
cutoff=$(date -u -d "$(date -u +%Y-%m-01) -${RETENTION_MONTHS} months" +%Y%m 2>/dev/null || date -u -v1d -v-${RETENTION_MONTHS}m +%Y%m)
for old in $(mysql_exec "SELECT PARTITION_NAME FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA='${DB_NAME}' AND TABLE_NAME='heartbeats' AND PARTITION_NAME LIKE 'p%' AND PARTITION_NAME < 'p${cutoff}'" || true); do
  echo "partition-manage: dropping ${old}"
  mysql_exec "ALTER TABLE heartbeats DROP PARTITION ${old}" || true
done

echo "partition-manage: done"