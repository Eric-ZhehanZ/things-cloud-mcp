#!/bin/sh
# Copies the CalDAV store from the old (Fly) deployment into rqlite.
#
#   ./migrate-dav-store.sh https://ai.thingsapi.com
#
# Run on one US node, from this directory, while BusyCal is idle and every
# node still has CALDAV_READ_ONLY=true. It asks the old server for a fresh
# manual backup over MCP (a consistent snapshot, unlike copying the live WAL
# database), loads its things-dav.db into rqlite (replacing what is there),
# and compares row counts. Reads API_KEY and the rqlite credentials from
# .env without printing them. Needs curl, unzip and python3.
set -eu

OLD=${1:?usage: $0 https://<old origin>}
cd "$(dirname "$0")"
env_get() { sed -n "s/^$1=//p" .env | tail -1 | sed "s/^'\(.*\)'\$/\1/; s/^\"\(.*\)\"\$/\1/"; }
API_KEY=$(env_get API_KEY)
RQ_URL=$(env_get DAV_RQLITE_URL)
RQ_USER=$(env_get DAV_RQLITE_USER)
RQ_PASS=$(env_get DAV_RQLITE_PASSWORD)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "Creating a backup on $OLD ..."
curl -fsS -X POST "$OLD/mcp" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"things_backup_create","arguments":{}}}' \
  > "$work/resp"
url=$(python3 - "$work/resp" <<'PY'
import json, sys
raw = open(sys.argv[1]).read()
for line in raw.splitlines():          # plain JSON or an SSE "data:" line
    line = line.strip()
    if line.startswith("data:"):
        line = line[5:].strip()
    if line.startswith("{"):
        msg = json.loads(line)
        break
res = msg.get("result") or {}
if res.get("isError") or "error" in msg:
    sys.exit("backup failed: %s" % (msg.get("error") or res))
info = json.loads(res["content"][0]["text"])
print(info["download_url"])
PY
)
curl -fsS -o "$work/backup.zip" "$url"
unzip -q "$work/backup.zip" things-dav.db -d "$work"

echo "Rows in the old store:"
python3 - "$work/things-dav.db" > "$work/old-counts" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
for t in ["snapshots", "aliases", "sidecars", "recent_deletes", "write_log"]:
    print(t, db.execute("SELECT COUNT(*) FROM " + t).fetchone()[0])
PY
cat "$work/old-counts"

echo "Loading into rqlite at $RQ_URL ..."
curl -fsS -u "$RQ_USER:$RQ_PASS" -X POST "$RQ_URL/db/load" \
  -H 'Content-Type: application/octet-stream' --data-binary @"$work/things-dav.db"
echo

echo "Rows in rqlite:"
: > "$work/new-counts"
for t in snapshots aliases sidecars recent_deletes write_log; do
  n=$(curl -fsS -u "$RQ_USER:$RQ_PASS" -G "$RQ_URL/db/query?level=strong" \
        --data-urlencode "q=SELECT COUNT(*) FROM $t" |
      python3 -c 'import json,sys; print(json.load(sys.stdin)["results"][0]["values"][0][0])')
  echo "$t $n" >> "$work/new-counts"
done
cat "$work/new-counts"
if cmp -s "$work/old-counts" "$work/new-counts"; then
  echo "OK: all five tables match."
else
  echo "MISMATCH: do not enable CalDAV writes." >&2
  exit 1
fi
