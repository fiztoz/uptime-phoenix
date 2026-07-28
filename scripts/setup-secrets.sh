#!/usr/bin/env bash
# ─── Phoenix — Docker Compose secrets setup ────────────────────────────────────
# Creates .secrets/ directory with generated passwords for docker-compose.
set -euo pipefail

SECRETS_DIR=".secrets"

echo "Setting up Phoenix dev secrets in $SECRETS_DIR/"

mkdir -p "$SECRETS_DIR"

# Generate passwords if files don't exist
for name in mariadb_root_password mariadb_password jwt_secret; do
  file="$SECRETS_DIR/$name"
  if [ ! -f "$file" ]; then
    openssl rand -base64 24 > "$file"
    chmod 600 "$file"
    echo "  Generated $name"
  else
    echo "  $name already exists, skipping"
  fi
done

echo ""
echo "Secrets ready. Run: docker compose up -d"
