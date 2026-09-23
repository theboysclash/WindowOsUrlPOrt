#!/usr/bin/env bash
# Starts the VM relay in the background on port 8080 and makes the port public
# so the Chromebook can open it without signing in to GitHub.
set -u
cd "$(dirname "$0")/.."

health() { curl -fsS -m 2 http://127.0.0.1:8080/_relay/health >/dev/null 2>&1; }

if ! health; then
  go build -o /tmp/vmrelay ./cmd/relay || { echo "relay build failed" > RELAY-LINK.txt; exit 1; }
  setsid nohup /tmp/vmrelay -listen :8080 -link-file RELAY-LINK.txt >/tmp/vmrelay.log 2>&1 </dev/null &
  for _ in $(seq 1 30); do health && break; sleep 1; done
fi

public=no
if [ -n "${CODESPACE_NAME:-}" ]; then
  for _ in $(seq 1 10); do
    if gh codespace ports visibility 8080:public -c "$CODESPACE_NAME" >/dev/null 2>&1; then
      public=yes
      break
    fi
    sleep 3
  done
fi

if [ "$public" = no ] && ! grep -q "Port Visibility" RELAY-LINK.txt 2>/dev/null; then
  cat >> RELAY-LINK.txt <<'EOF'

IMPORTANT: port 8080 could not be made public automatically.
Open the PORTS tab at the bottom, right-click port 8080 (VM relay),
choose Port Visibility > Public.
EOF
fi

cat RELAY-LINK.txt
