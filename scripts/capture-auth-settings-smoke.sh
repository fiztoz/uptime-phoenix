#!/usr/bin/env bash
# Deterministic auth/settings smoke for verification-plan step 4.
# Exercises register → me → POST /api/api-keys → ownership → DELETE → empty GET.
set -euo pipefail

BASE_URL="${PHOENIX_BASE_URL:-http://localhost:3000}"
OUT="${1:-${SCRATCH:-.}/auth-settings-smoke.json}"
USERNAME="smoke_$(date +%s)"
PASSWORD="smokepass123"

fail() {
  echo "capture-auth-settings-smoke: $*" >&2
  exit 1
}

REG=$(curl -sf -X POST "$BASE_URL/api/auth/register" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}") || fail "register failed"
TOKEN=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])' <<<"$REG")

ME=$(curl -sf "$BASE_URL/api/auth/me" -H "Authorization: Bearer $TOKEN") || fail "GET /api/auth/me failed"
ME_USER_ID=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["user"]["id"])' <<<"$ME")

CREATE=$(curl -sf -w '\n%{http_code}' -X POST "$BASE_URL/api/api-keys" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"smoke-key","scopes":["read","write"]}') || fail "POST /api/api-keys failed"
CREATE_BODY=$(echo "$CREATE" | sed '$d')
CREATE_STATUS=$(echo "$CREATE" | tail -n1)
[[ "$CREATE_STATUS" == "201" ]] || fail "POST /api/api-keys status=$CREATE_STATUS body=$CREATE_BODY"

KEY_ID=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["api_key"]["id"])' <<<"$CREATE_BODY")
KEY_USER_ID=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["api_key"]["user_id"])' <<<"$CREATE_BODY")
if [ "$KEY_USER_ID" = "$ME_USER_ID" ]; then
  OWNERSHIP_MATCH=true
else
  OWNERSHIP_MATCH=false
fi
[[ "$OWNERSHIP_MATCH" == "true" ]] || fail "ownership mismatch: api_key.UserID=$KEY_USER_ID me.user.id=$ME_USER_ID"

DELETE_STATUS=$(curl -sf -o /dev/null -w '%{http_code}' -X DELETE \
  "$BASE_URL/api/api-keys/$KEY_ID" \
  -H "Authorization: Bearer $TOKEN") || fail "DELETE /api/api-keys/$KEY_ID failed"
[[ "$DELETE_STATUS" == "204" ]] || fail "DELETE status=$DELETE_STATUS (want 204)"

LIST=$(curl -sf "$BASE_URL/api/api-keys" -H "Authorization: Bearer $TOKEN") || fail "GET /api/api-keys after delete failed"
[[ "$LIST" == "[]" ]] || fail "list after delete not empty: $LIST"

mkdir -p "$(dirname "$OUT")"
export SMOKE_OUT="$OUT" SMOKE_BASE_URL="$BASE_URL" SMOKE_USERNAME="$USERNAME"
export SMOKE_ME="$ME" SMOKE_CREATE_BODY="$CREATE_BODY" SMOKE_LIST="$LIST"
export SMOKE_CREATE_STATUS="$CREATE_STATUS" SMOKE_DELETE_STATUS="$DELETE_STATUS"
export SMOKE_ME_USER_ID="$ME_USER_ID" SMOKE_KEY_USER_ID="$KEY_USER_ID"
export SMOKE_OWNERSHIP_MATCH="$OWNERSHIP_MATCH"
python3 - <<'PY'
import json
import os

out = {
    "base_url": os.environ["SMOKE_BASE_URL"],
    "username": os.environ["SMOKE_USERNAME"],
    "me": json.loads(os.environ["SMOKE_ME"]),
    "create_status": int(os.environ["SMOKE_CREATE_STATUS"]),
    "create": json.loads(os.environ["SMOKE_CREATE_BODY"]),
    "ownership_match": os.environ["SMOKE_OWNERSHIP_MATCH"] == "true",
    "me_user_id": int(os.environ["SMOKE_ME_USER_ID"]),
    "api_key_user_id": int(os.environ["SMOKE_KEY_USER_ID"]),
    "delete_status": int(os.environ["SMOKE_DELETE_STATUS"]),
    "list_after_delete": json.loads(os.environ["SMOKE_LIST"]),
}
path = os.environ["SMOKE_OUT"]
with open(path, "w") as f:
    json.dump(out, f, indent=2)
print(f"wrote {path}")
PY