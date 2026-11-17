#!/usr/bin/env bash
# Seeds a THROWAWAY Weave instance with entirely fictional demo data for scripts/demo/record.mjs.
#
#   export DEMO_DIR=$HOME/.cache/weave-demo           # work dir (keys, sessions, frames); never committed
#   export WEAVE_DATA_DIR=$DEMO_DIR/data WEAVE_ADDR=:18120 WEAVE_BASE_URL=http://localhost:18120 \
#     WEAVE_SECRET_KEY=$(openssl rand -hex 32) WEAVE_OIDC_ISSUER=http://localhost:9 \
#     WEAVE_OIDC_CLIENT_ID=w WEAVE_OIDC_CLIENT_SECRET=x
#   go build -o $DEMO_DIR/weave ./cmd/weave && ($DEMO_DIR/weave serve &)      # fresh, empty data dir only
#   scripts/demo/seed.sh $DEMO_DIR/weave                                       # fictional data + sessions
#   node scripts/demo/shots.mjs docs/screenshots   # right after seeding (the demo moves cards, so re-seed between runs)
#   node scripts/demo/record.mjs && python3 scripts/demo/encode.py docs        # needs DEMO_DIR, WEAVE_BASE_URL, PW_CORE, PW_CHROMIUM, FFMPEG
#
# It mints fictional users (example.com addresses), names them, opens browser sessions for them straight in
# SQLite (the OIDC provider is never contacted), and creates projects, boards, cards, labels and comments over
# the REST API. Everything lands in $DEMO_DIR/state.json. Nothing here touches a real account.
set -euo pipefail
BIN=${1:?usage: seed.sh /path/to/weave-binary}
DEMO_DIR=${DEMO_DIR:?set DEMO_DIR}
: "${WEAVE_DATA_DIR:?}" "${WEAVE_BASE_URL:?}"
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$DEMO_DIR"

declare -A ROLE=([avery]=admin [jordan]=boardUser [sam]=boardUser [riley]=boardUser)
printf '{' > "$DEMO_DIR/keys.json"
sep=''
for u in avery jordan sam riley; do
  k=$("$BIN" admin mint-key --email "$u@example.com" --role "${ROLE[$u]}" | sed 's/.*"key":"\([^"]*\)".*/\1/')
  printf '%s"%s":"%s"' "$sep" "$u" "$k" >> "$DEMO_DIR/keys.json"; sep=','
done
printf '}' >> "$DEMO_DIR/keys.json"
chmod 600 "$DEMO_DIR/keys.json"

python3 - "$WEAVE_DATA_DIR/weave.db" "$DEMO_DIR" <<'PY'
import sqlite3, sys, hashlib, secrets, json, datetime
db, out = sys.argv[1], sys.argv[2]
names = {"avery": "Avery Quinn", "jordan": "Jordan Lee", "sam": "Sam Rivera", "riley": "Riley Chen"}
c = sqlite3.connect(db)
now = datetime.datetime.now(datetime.timezone.utc)
exp = (now + datetime.timedelta(days=2)).strftime("%Y-%m-%dT%H:%M:%SZ")
ts = now.strftime("%Y-%m-%dT%H:%M:%SZ")
tokens = {}
for u, n in names.items():
    c.execute("UPDATE users SET name=?, theme=NULL WHERE email=?", (n, f"{u}@example.com"))
    uid = c.execute("SELECT id FROM users WHERE email=?", (f"{u}@example.com",)).fetchone()[0]
    tok = secrets.token_hex(32)
    c.execute("INSERT INTO sessions(id,user_id,token_hash,created_at,expires_at,last_used_at) VALUES(?,?,?,?,?,?)",
              (secrets.token_hex(12), uid, hashlib.sha256(tok.encode()).hexdigest(), ts, exp, ts))
    tokens[u] = {"id": uid, "session": tok}
c.commit()
json.dump(tokens, open(out + "/sessions.json", "w"))
PY
chmod 600 "$DEMO_DIR/sessions.json"
node "$HERE/seed.mjs"
