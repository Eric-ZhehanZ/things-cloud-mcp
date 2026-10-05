package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sync/atomic"
)

// serverReady is set once the initial Things sync and history setup are
// done; /healthz fails until then so no tunnel connector routes to a node
// that can't answer.
var serverReady atomic.Bool

// nodeName is NODE_NAME: which server this is, for logs, health checks and
// backup manifests.
func nodeName() string { return os.Getenv("NODE_NAME") }

// handleHealthz is the unauthenticated liveness check used by Docker (and,
// through it, cloudflared's start order). It reveals nothing about tasks.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	body := map[string]string{"status": "ok", "node": nodeName()}
	if !serverReady.Load() {
		body["status"] = "starting"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(body)
		return
	}
	if davDB != nil {
		body["dav_store"] = davDB.mode()
	}
	jsonResponse(w, body)
}

// handleDAVStoreStatus reports the CalDAV store's backend and row counts,
// for checking a migration and failover drills.
func handleDAVStoreStatus(w http.ResponseWriter, r *http.Request) {
	if davDB == nil {
		jsonError(w, "CalDAV store not configured", http.StatusNotFound)
		return
	}
	counts, err := davDB.tableCounts()
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, map[string]any{"node": nodeName(), "mode": davDB.mode(), "rows": counts})
}
