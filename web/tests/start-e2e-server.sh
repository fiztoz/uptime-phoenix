#!/bin/sh
set -eu

export GOTOOLCHAIN=${GOTOOLCHAIN:-go1.26.6}

e2e_tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/phoenix-e2e.XXXXXX")
app_pid=''
stub_pid=''

cleanup() {
	if [ -n "$app_pid" ]; then kill "$app_pid" 2>/dev/null || true; fi
	if [ -n "$stub_pid" ]; then kill "$stub_pid" 2>/dev/null || true; fi
	rm -rf "$e2e_tmp_dir"
}
trap cleanup EXIT INT TERM

# Reuse a prebuilt dist when CI warmed it; still rebuild when missing.
if [ ! -f dist/index.html ]; then
	bun run build
fi
cd ..
# Ensure embed path exists for go build of cmd/app.
if [ ! -f web/dist/index.html ]; then
	mkdir -p web/dist
	printf '%s\n' '<!doctype html><title>phoenix</title>' > web/dist/index.html
fi
go build -o "$e2e_tmp_dir/uptime-phoenix" ./cmd/app
go build -o "$e2e_tmp_dir/webhook-stub" ./web/tests/webhook_stub.go

"$e2e_tmp_dir/webhook-stub" &
stub_pid=$!

# M5 regional/latency-selection coverage needs the probe surface enabled; the
# key file is exactly 32 raw bytes read once at startup.
probe_key="$e2e_tmp_dir/probe-secret.key"
dd if=/dev/urandom of="$probe_key" bs=32 count=1 2>/dev/null
chmod 600 "$probe_key"

DB_ENGINE=sqlite \
	DB_DSN="file:$e2e_tmp_dir/uptime-phoenix.db?cache=shared" \
	JWT_SECRET=e2e_secret \
	BOOTSTRAP_USERNAME=admin \
	BOOTSTRAP_PASSWORD='ChangeMe123!' \
	PUBLIC_URL='http://127.0.0.1:3100' \
	PORT=3100 \
	PROBES_ENABLED=true \
	PROBE_SECRET_KEY_FILE="$probe_key" \
	ESCALATION_POLL_SECONDS=1 \
	"$e2e_tmp_dir/uptime-phoenix" &
app_pid=$!

wait "$app_pid"
