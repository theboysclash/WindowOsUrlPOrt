#!/usr/bin/env bash
# Starts the VM relay (port 8080) and the Scramjet web proxy (port 8081) in the
# background and makes both ports public so they open without a GitHub sign-in.
set -u
cd "$(dirname "$0")/.."

up() { curl -fsS -m 2 -o /dev/null "http://127.0.0.1:$1$2" 2>/dev/null; }

if ! up 8080 /_relay/health; then
  go build -o /tmp/vmrelay ./cmd/relay || { echo "relay build failed" > RELAY-LINK.txt; exit 1; }
  setsid nohup /tmp/vmrelay -listen :8080 -link-file RELAY-LINK.txt >/tmp/vmrelay.log 2>&1 </dev/null &
  for _ in $(seq 1 30); do up 8080 /_relay/health && break; sleep 1; done
fi

if ! up 8081 /; then
  [ -d proxy/node_modules ] || (cd proxy && npx -y pnpm@10.18.3 install --frozen-lockfile)
  (cd proxy && PORT=8081 setsid nohup node src/index.js >/tmp/proxy.log 2>&1 </dev/null &)
  for _ in $(seq 1 30); do up 8081 / && break; sleep 1; done
fi

public=no
if [ -n "${CODESPACE_NAME:-}" ]; then
  for _ in $(seq 1 10); do
    if gh codespace ports visibility 8080:public 8081:public -c "$CODESPACE_NAME" >/dev/null 2>&1; then
      public=yes
      break
    fi
    sleep 3
  done
fi

domain="${GITHUB_CODESPACES_PORT_FORWARDING_DOMAIN:-app.github.dev}"
if ! grep -q "Web proxy" RELAY-LINK.txt 2>/dev/null; then
  cat >> RELAY-LINK.txt <<EOF

Web proxy (Scramjet) - open this and type any website or search:

https://${CODESPACE_NAME:-YOUR-CODESPACE}-8081.${domain}
EOF
fi

if [ "$public" = no ] && ! grep -q "Port Visibility" RELAY-LINK.txt 2>/dev/null; then
  cat >> RELAY-LINK.txt <<'EOF'

IMPORTANT: the ports could not be made public automatically.
Open the PORTS tab at the bottom, right-click port 8080 and port 8081,
choose Port Visibility > Public.
EOF
fi

cat RELAY-LINK.txt
