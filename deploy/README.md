# Self-hosted deployment (replaces Fly.io)

Four nodes run the same `docker-compose.yml`; only `.env` differs.

| Node | Runs | rqlite role | Traffic |
|------|------|-------------|---------|
| us1, us2, us3 | app + rqlite + cloudflared | voter | Cloudflare sends each request to the connector nearest the edge that received it |
| ro (Romania) | app + rqlite + cloudflared | non-voter | Requests landing on European edges, and everything once all US connectors are gone |

- **Routing:** one Cloudflare Tunnel, one connector per node (same token). No load balancer.
- **Things mirror** (`data/things.db`): local per node, rebuilt from Things Cloud on first start.
- **CalDAV store** (merge bases, aliases, sidecars, recent deletes, write log): shared in rqlite.
- **Backups:** an R2 bucket shared by all nodes. One node takes each daily backup (a lease row in rqlite).
- **Health:** `GET /healthz` (no auth) is 200 once the initial Things sync is done. Cloudflare Tunnel doesn't check origins, so cloudflared runs behind a guard (`tunnel/guard.sh`) that polls the app every 2 s: it connects only once the app is healthy and disconnects after two failed checks in a row, so a dead, hung or restarting app hands its traffic to the other nodes within seconds.

## Failure behaviour

| Event | Effect |
|-------|--------|
| One US node down | Invisible. rqlite keeps its 2/3 quorum. |
| rqlite down on one US node | That node's app keeps working (see below); the cluster is unaffected. |
| Two or three US nodes down | Survivors (including Romania) keep serving **everything, CalDAV edits included**. |

**Why Romania keeps CalDAV writes (differs from the original handoff).** A Raft cluster can't accept writes without a majority of voters, so no rqlite topology lets a lone Romania node write when all US voters are gone; promoting it to a voter would make things worse (4 voters need 3, so losing 2 US nodes would already stop writes). Romania therefore stays a non-voter, and the app gets a fallback instead (`server/dav_store_failover.go`, on by default, `DAV_LOCAL_FALLBACK=false` turns it off):

1. Normally every store read and write goes to rqlite, and each node refreshes a local copy (`data/things-dav-fallback.db`) every 5 minutes.
2. When rqlite can't serve (no leader, node unreachable), the node re-seeds that copy from its own rqlite replica (a `level=none` read needs no leader), then reads and writes locally and journals each write (only the latest write per row is kept).
3. Every 5 seconds it checks rqlite's `/readyz`. Once there is a leader again, it replays the journal into rqlite and switches back. The journal is on disk, so a restart mid-outage loses nothing.

During an all-US outage only Romania takes traffic, so there's one writer and the replay can't clash. If a network split ever let two nodes write at once, the later replay wins per row; Things itself stays consistent either way, because Things Cloud orders all task writes. Every edit also syncs Things right before merging, so a node never merges against stale Things data.

`/healthz` shows `"dav_store": "local-fallback since …"` while a node is in this mode, and `/api/dav/store` (API key) shows the mode plus row counts.

## One-time setup per node

Prerequisites: Docker with the compose plugin, and a private network between the four nodes.

### Private network

rqlite listens only on `MESH_IP` and must never be reachable from the internet. Use whatever mesh already exists (Tailscale, a provider VPC). Otherwise create a WireGuard mesh, e.g. `10.77.0.1`–`.3` for us1–us3 and `10.77.0.4` for ro, every node peering with every other, UDP 51820 open between them. Ports 4001 (HTTP) and 4002 (Raft) must be reachable between the mesh addresses, and from the node's own Docker bridge (the app container calls `http://MESH_IP:4001`): with ufw, `ufw allow in on wg0 to any port 4001:4002 proto tcp` and `ufw allow from 172.16.0.0/12 to <MESH_IP> port 4001 proto tcp`.

### Files

```sh
git clone https://github.com/ZhehanZhang/things-cloud-mcp.git /opt/things-cloud-mcp   # or copy the repo
cd /opt/things-cloud-mcp && git checkout <commit>
cd deploy
cp .env.example .env                       # owner fills in the SECRET lines
cp rqlite-auth.json.example rqlite-auth.json   # owner sets the password (= DAV_RQLITE_PASSWORD)
mkdir -p data rqlite
sudo chown 1000:1000 rqlite rqlite-auth.json   # the rqlite image runs as uid 1000 and must read the auth file
chmod 600 .env rqlite-auth.json
```

Per node, set `NODE_NAME`, `MESH_IP`, `DAV_RQLITE_URL=http://<MESH_IP>:4001` and `RQLITE_ROLE_FLAG` (`-bootstrap-expect=3` on US nodes, `-raft-non-voter=true` on ro). `RQLITE_JOIN` is the same everywhere: the three US Raft addresses.

### Image

Two images: the app and the cloudflared guard (`things-tunnel:local`, from `tunnel/`). Either build on the node (`docker compose build`), build once and copy (`docker save things-cloud-mcp:local things-tunnel:local | ssh <node> docker load`), or pull from GHCR: `.github/workflows/image.yml` publishes `ghcr.io/zhehanzhang/things-cloud-mcp:<branch>` and `:sha-<commit>` on every push. A private package needs `docker login ghcr.io` on each node with a read-only token; then set `APP_IMAGE` in `.env` and still build the guard (`docker compose build cloudflared`).

## Cutover from Fly (don't skip steps)

1. **rqlite.** On us1, us2, us3: `docker compose up -d rqlite`. Check a leader exists: `curl -s http://<MESH_IP>:4001/readyz` → `leader ok`, and `curl -s -u things:<pw> http://<MESH_IP>:4001/nodes?nonvoters` lists 3 voters. Then on ro: `docker compose up -d rqlite`; `/nodes?nonvoters` now shows ro with `"voter":false`.
2. **Apps, read-only.** With `CALDAV_READ_ONLY=true` in every `.env`: `docker compose up -d` on all four nodes. Wait for `docker compose ps` to show `app` healthy (the first sync can take minutes). In the Cloudflare dashboard, add a public hostname on the tunnel, e.g. `test.thingsapi.com → http://app:8080`. Verify through it: MCP reads (`/mcp?key=…`), CalDAV listing with colors and CTag (`PROPFIND /dav/me/calendars/` with Depth 1), `/healthz` on every node (`docker compose exec app wget -qO- 127.0.0.1:8080/healthz`), and backup list/download.
3. **Migrate the CalDAV store** while BusyCal is idle (quit it on Mac and iPhone), then go straight to step 4. On us1: `./migrate-dav-store.sh https://things-cloud-mcp-glowing-hill-8460.fly.dev`. It takes a consistent backup on Fly over MCP, loads its `things-dav.db` into rqlite and checks all five tables' row counts match. Stop if it reports a mismatch. Needs `curl`, `unzip` and `python3` on the node (`apt install unzip` if missing).
4. **CalDAV.** Set `CALDAV_READ_ONLY=false` on all nodes and `docker compose up -d app`. Point `cal.thingsapi.com` at the tunnel (public hostname → `http://app:8080`; remove the old DNS record). Test from BusyCal: rename, date, alert, tag, complete/reopen, create in a project, move between lists, delete (lands in Things Trash), drag and delete a Deadlines event.
5. **MCP.** Point `ai.thingsapi.com` and `thingsmcp.zheha.nz` at the tunnel. Verify MCP from claude.ai and that `things_backup_create` returns a link on `ai.thingsapi.com` that downloads.
6. **Failover drills.**
   - `docker compose stop app` on one US node, including the one nearest you (it gets your traffic): at most a few seconds of 502s while its guard disconnects, then the other nodes serve. The guard's log (`docker compose logs cloudflared`) shows `stopping cloudflared`. Start it again; the guard reconnects once the app is healthy.
   - `docker compose stop rqlite` on one US node: edits still work. Start it again.
   - Stop cloudflared (or everything) on all three US nodes: Romania serves, MCP and BusyCal edits keep working (`/healthz` on ro shows `local-fallback`). Start the US nodes: ro's log shows `replayed N journaled writes`, and `/api/dav/store` on a US node shows the edits.
7. **Rollback window.** Keep the Fly app running and idle for one week; rollback is pointing the three hostnames back at Fly. Then take a final backup and `fly apps destroy things-cloud-mcp-glowing-hill-8460` (this deletes the volume and its snapshots).

## Rollback

Point `cal.thingsapi.com`, `ai.thingsapi.com` and `thingsmcp.zheha.nz` back to the Fly origin. CalDAV edits made on the new cluster since cutover are in Things already; only their merge bases, aliases and sidecars stay behind in rqlite (export with `curl -u things:<pw> <rqlite>/db/backup -o dav.db` if needed).

## Adding or removing a node

- **Add a US voter:** set it up as above with `-bootstrap-expect=3` (ignored once the cluster exists) and `docker compose up -d`; it joins through `RQLITE_JOIN`. With 4+ voters, update `RQLITE_JOIN` everywhere.
- **Add a read-only node:** same, with `-raft-non-voter=true`.
- **Remove a node:** `docker compose down` on it, then on any node `curl -u things:<pw> -X DELETE http://<MESH_IP>:4001/remove -d '{"id":"<node>"}'`. Keep at least 3 voters.
- **Replace a dead voter:** remove it as above, wipe its `rqlite/` directory, then start it again to rejoin.

## Operations

- Logs: `docker compose logs -f app` (lines are prefixed with the node name).
- Store state: `curl -H "Authorization: Bearer $API_KEY" https://ai.thingsapi.com/api/dav/store`.
- Upgrade: `git pull` (or new `APP_IMAGE`), `docker compose up -d app`, one node at a time; wait for healthy before the next.
- Mass-delete breaker tripped: `POST /api/dav/resume` on the node that tripped (each node has its own breaker).
