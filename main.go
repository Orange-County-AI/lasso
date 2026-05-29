// Command ttyd-iframe-demo serves a two-column web UI:
//
//	left  = herdr running inside a ttyd terminal (embedded in an iframe)
//	right = a file viewer that follows herdr's *focused pane* cwd, live
//
// It talks to the herdr server over its newline-delimited JSON unix socket
// (subscribe to focus events + poll pane.list for cwd changes) and pushes
// active-pane updates to the browser over SSE.
//
// Everything binds to loopback by default: the left pane is a writable shell,
// so this is NOT meant to be exposed to a network without deliberate thought.
package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var staticFS embed.FS

var (
	listenAddr = flag.String("listen", "127.0.0.1:8090", "address for the web server (loopback by default — the terminal is a writable shell)")
	ttydPort   = flag.Int("ttyd-port", 7682, "loopback port ttyd listens on")
	herdrSock  = flag.String("herdr-sock", defaultSock(), "path to the herdr unix socket")
	termCmd    = flag.String("term-cmd", "herdr", "command ttyd runs in the terminal")
	spawnTtyd  = flag.Bool("spawn-ttyd", true, "spawn and supervise ttyd as a child process")
	pollEvery  = flag.Duration("poll", 2*time.Second, "fallback poll interval for cwd changes")
)

func defaultSock() string {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "herdr", "herdr.sock")
}

func main() {
	flag.Parse()

	// Auth credentials come from the environment (UI_AUTH=user:pass), never
	// argv — so they don't leak via `ps`. Safety guard: refuse to bind to a
	// non-loopback address without auth, so this can't accidentally expose a
	// writable shell on a public interface again.
	authUser, authPass, hasAuth := parseAuth(os.Getenv("UI_AUTH"))
	if !isLoopback(*listenAddr) && !hasAuth {
		log.Fatalf("refusing to listen on non-loopback %q without auth — set UI_AUTH=user:pass "+
			"(or front it with `tailscale serve` and keep -listen on 127.0.0.1)", *listenAddr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *spawnTtyd {
		if err := startTtyd(ctx); err != nil {
			log.Fatalf("ttyd: %v", err)
		}
	}

	hub := newHub()
	go hub.run(ctx)

	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", *ttydPort))
	proxy := httputil.NewSingleHostReverseProxy(target) // handles WS upgrade natively

	mux := http.NewServeMux()
	mux.Handle("/terminal/", proxy)
	mux.HandleFunc("/api/active", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, hub.snapshot())
	})
	mux.HandleFunc("/api/events", hub.serveSSE)
	mux.HandleFunc("/api/files", serveFiles)
	mux.HandleFunc("/api/file", serveFile)
	mux.HandleFunc("/", serveIndex)

	handler := withAuth(mux, authUser, authPass, hasAuth)
	srv := &http.Server{Addr: *listenAddr, Handler: handler}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()

	if hasAuth {
		log.Printf("auth:     enabled (basic, user %q)", authUser)
	} else {
		log.Printf("auth:     DISABLED (loopback only)")
	}
	log.Printf("UI:       http://%s", *listenAddr)
	log.Printf("terminal: ttyd@127.0.0.1:%d running %q (proxied at /terminal/)", *ttydPort, *termCmd)
	log.Printf("herdr:    %s", *herdrSock)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// ttyd child process
// ---------------------------------------------------------------------------

func startTtyd(ctx context.Context) error {
	args := []string{
		"-i", "lo", // loopback only
		"-p", fmt.Sprint(*ttydPort),
		"-b", "/terminal", // base path so assets/ws resolve under the proxy
		"-W",                           // writable
		"-t", "disableLeaveAlert=true", // no confirm dialog inside the iframe
		"-t", "fontSize=13",
	}
	args = append(args, strings.Fields(*termCmd)...)
	cmd := exec.Command("ttyd", args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own process group so we can kill cleanly
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("spawned ttyd (pid %d)", cmd.Process.Pid)
	go func() {
		<-ctx.Done()
		// kill the whole process group (ttyd + the shell it spawned)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}()
	go func() { _ = cmd.Wait() }()
	return nil
}

// ---------------------------------------------------------------------------
// herdr socket client
// ---------------------------------------------------------------------------

// herdrCall does one request/response round-trip on a fresh connection.
func herdrCall(method string, params any) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", *herdrSock, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	req := map[string]any{"id": "ui", "method": method, "params": params}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("herdr error: %s", resp.Error)
	}
	return resp.Result, nil
}

type pane struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	Cwd         string `json:"cwd"`
	Focused     bool   `json:"focused"`
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
}

type workspace struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
}

// Active is the state pushed to the browser.
type Active struct {
	PaneID         string `json:"pane_id"`
	Cwd            string `json:"cwd"`
	WorkspaceID    string `json:"workspace_id"`
	WorkspaceLabel string `json:"workspace_label"`
	TabID          string `json:"tab_id"`
	Agent          string `json:"agent"`
	AgentStatus    string `json:"agent_status"`
}

func fetchActive() (Active, error) {
	res, err := herdrCall("pane.list", map[string]any{})
	if err != nil {
		return Active{}, err
	}
	var pl struct {
		Panes []pane `json:"panes"`
	}
	if err := json.Unmarshal(res, &pl); err != nil {
		return Active{}, err
	}
	var fp *pane
	for i := range pl.Panes {
		if pl.Panes[i].Focused {
			fp = &pl.Panes[i]
			break
		}
	}
	if fp == nil {
		return Active{}, fmt.Errorf("no focused pane")
	}
	a := Active{
		PaneID: fp.PaneID, Cwd: fp.Cwd, WorkspaceID: fp.WorkspaceID,
		TabID: fp.TabID, Agent: fp.Agent, AgentStatus: fp.AgentStatus,
	}
	// resolve workspace label (best effort)
	if res, err := herdrCall("workspace.list", map[string]any{}); err == nil {
		var wl struct {
			Workspaces []workspace `json:"workspaces"`
		}
		if json.Unmarshal(res, &wl) == nil {
			for _, w := range wl.Workspaces {
				if w.WorkspaceID == a.WorkspaceID {
					a.WorkspaceLabel = w.Label
				}
			}
		}
	}
	return a, nil
}

// subscribeFocus opens a long-lived connection subscribed to focus events and
// signals `trigger` whenever one arrives. Reconnects on failure.
func subscribeFocus(ctx context.Context, trigger chan<- struct{}) {
	for ctx.Err() == nil {
		conn, err := net.Dial("unix", *herdrSock)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		sub := `{"id":"ui-sub","method":"events.subscribe","params":{"subscriptions":` +
			`[{"type":"pane.focused"},{"type":"tab.focused"},{"type":"workspace.focused"}]}}` + "\n"
		if _, err := conn.Write([]byte(sub)); err != nil {
			conn.Close()
			continue
		}
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			select {
			case trigger <- struct{}{}:
			default:
			}
		}
		conn.Close()
		time.Sleep(time.Second)
	}
}

// ---------------------------------------------------------------------------
// SSE hub
// ---------------------------------------------------------------------------

type hub struct {
	mu      sync.RWMutex
	cur     Active
	clients map[chan Active]struct{}
}

func newHub() *hub { return &hub{clients: map[chan Active]struct{}{}} }

func (h *hub) snapshot() Active { h.mu.RLock(); defer h.mu.RUnlock(); return h.cur }

func (h *hub) run(ctx context.Context) {
	trigger := make(chan struct{}, 1)
	go subscribeFocus(ctx, trigger)
	ticker := time.NewTicker(*pollEvery)
	defer ticker.Stop()

	refresh := func() {
		a, err := fetchActive()
		if err != nil {
			return
		}
		h.mu.Lock()
		changed := a != h.cur
		h.cur = a
		clients := make([]chan Active, 0, len(h.clients))
		for c := range h.clients {
			clients = append(clients, c)
		}
		h.mu.Unlock()
		if changed {
			for _, c := range clients {
				select {
				case c <- a:
				default:
				}
			}
		}
	}

	refresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
			refresh()
		case <-ticker.C:
			refresh()
		}
	}
}

func (h *hub) serveSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan Active, 4)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	cur := h.cur
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.clients, ch); h.mu.Unlock() }()

	send := func(a Active) {
		b, _ := json.Marshal(a)
		fmt.Fprintf(w, "event: active\ndata: %s\n\n", b)
		fl.Flush()
	}
	send(cur) // prime with current state

	keep := time.NewTicker(25 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case a := <-ch:
			send(a)
		case <-keep.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// file APIs
// ---------------------------------------------------------------------------

type fileEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size,omitempty"`
}

func serveFiles(w http.ResponseWriter, r *http.Request) {
	path := filepath.Clean(r.URL.Query().Get("path"))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	ents, err := os.ReadDir(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	out := make([]fileEntry, 0, len(ents))
	for _, e := range ents {
		fe := fileEntry{Name: e.Name(), Dir: e.IsDir()}
		if !e.IsDir() {
			if info, err := e.Info(); err == nil {
				fe.Size = info.Size()
			}
		}
		out = append(out, fe)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir // dirs first
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, map[string]any{"path": path, "parent": filepath.Dir(path), "entries": out})
}

const maxPreview = 2 << 20 // 2 MiB

func serveFile(w http.ResponseWriter, r *http.Request) {
	path := filepath.Clean(r.URL.Query().Get("path"))
	if !filepath.IsAbs(path) {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		http.Error(w, "not a file", http.StatusNotFound)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if info.Size() > maxPreview {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "[%s is %d bytes — too large to preview (limit %d)]", filepath.Base(path), info.Size(), maxPreview)
		return
	}
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, _ := staticFS.ReadFile("index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// auth
// ---------------------------------------------------------------------------

func parseAuth(s string) (user, pass string, ok bool) {
	if s == "" {
		return "", "", false
	}
	u, p, found := strings.Cut(s, ":")
	if !found || u == "" {
		return "", "", false
	}
	return u, p, true
}

// withAuth gates every request behind HTTP basic auth when enabled. The browser
// caches the credentials per-origin, so a single login covers the page, the
// proxied terminal (incl. its WebSocket), SSE, and the file APIs.
func withAuth(next http.Handler, user, pass string, enabled bool) http.Handler {
	if !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="herdr", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
