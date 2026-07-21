package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
)

// ---------------------------------------------------------------------------
// unified dev logging — one stream for backend + browser
// ---------------------------------------------------------------------------
//
// For debugging the Grid tab's cross-host churn we want the frontend's view of
// events (which panes a poll returned, when a cell attached/released, when the
// active host switched) interleaved with the backend's (pool drops, term
// spawns, discovery flaps) in a single time-ordered log. So in -dev mode we tee
// the standard logger to a file AND expose POST /api/log, which the browser
// posts batched events to — they land in the same file via the same log.Printf.
//
// The file path is stable (so a separate process — an editor, a tail, a coding
// agent — can read it without discovering a temp path): LASSO_DEV_LOG, default
// /tmp/lasso-dev.log. It's truncated on startup so each `mise run dev` run is a
// fresh log.

// devLogPath returns the unified dev-log file path (LASSO_DEV_LOG or the default).
func devLogPath() string {
	if p := os.Getenv("LASSO_DEV_LOG"); p != "" {
		return p
	}
	return "/tmp/lasso-dev.log"
}

// setupDevLog tees the standard logger to the dev-log file (in addition to
// stderr) so both backend log.Printf output and browser-posted events (see
// serveClientLog) accumulate in one place. Only meaningful in -dev; a failure to
// open the file is non-fatal (we just keep logging to stderr).
func setupDevLog() {
	path := devLogPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("devlog:   could not open %s: %v (stderr only)", path, err)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	log.Printf("devlog:   unified backend+browser log -> %s", path)
}

// maxClientLogBody caps the client-log request body so a runaway browser can't
// flood the process. Comfortably larger than any real batch.
const maxClientLogBody = 256 << 10 // 256 KiB

// clientLogEvent is one browser-side log event. Msg is a preformatted line; Data
// (optional) is arbitrary structured context appended as compact JSON.
type clientLogEvent struct {
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data,omitempty"`
}

// clientLogSeq numbers received browser events so gaps (a dropped beacon) are
// visible in the log.
var clientLogSeq struct {
	mu sync.Mutex
	n  int
}

// serveClientLog receives a batch of browser log events and emits each via the
// standard logger, so they interleave with backend logs in the unified dev log.
// Prefixed "client:" to distinguish them from backend lines. Registered only in
// -dev (see runServer) — the production binary has no client-log sink.
func serveClientLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Events []clientLogEvent `json:"events"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxClientLogBody)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	clientLogSeq.mu.Lock()
	for _, e := range req.Events {
		clientLogSeq.n++
		if len(e.Data) > 0 {
			log.Printf("client:   #%d %s %s", clientLogSeq.n, e.Msg, string(e.Data))
		} else {
			log.Printf("client:   #%d %s", clientLogSeq.n, e.Msg)
		}
	}
	clientLogSeq.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
