package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fast backoff so retry tests don't sleep for real.
func fastBackoff(t *testing.T) {
	t.Helper()
	ob, om, op := closeBackoffBase, closeBackoffMax, closePace
	closeBackoffBase, closeBackoffMax, closePace = time.Millisecond, time.Millisecond, time.Millisecond
	t.Cleanup(func() { closeBackoffBase, closeBackoffMax, closePace = ob, om, op })
}

// stubCloser swaps paneCloser for the duration of a test.
func stubCloser(t *testing.T, fn func(id string) error) {
	t.Helper()
	prev := paneCloser
	paneCloser = fn
	t.Cleanup(func() { paneCloser = prev })
}

func TestClosePaneRetriesTransient(t *testing.T) {
	fastBackoff(t)
	calls := 0
	stubCloser(t, func(string) error {
		calls++
		if calls < 3 {
			return &herdrError{Code: "internal", Message: "busy"} // transient
		}
		return nil
	})
	if err := closePane(context.Background(), "p1"); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts, got %d", calls)
	}
}

func TestClosePaneNotFoundIsSuccess(t *testing.T) {
	fastBackoff(t)
	calls := 0
	stubCloser(t, func(string) error {
		calls++
		return &herdrError{Code: "pane_not_found", Message: "pane p1 not found"}
	})
	if err := closePane(context.Background(), "p1"); err != nil {
		t.Fatalf("pane_not_found should be treated as closed, got %v", err)
	}
	if calls != 1 {
		t.Errorf("pane_not_found must not retry; got %d calls", calls)
	}
}

func TestClosePaneInvalidRequestFailsFast(t *testing.T) {
	fastBackoff(t)
	calls := 0
	stubCloser(t, func(string) error {
		calls++
		return &herdrError{Code: "invalid_request", Message: "missing field"}
	})
	if err := closePane(context.Background(), ""); err == nil {
		t.Fatal("invalid_request should fail, not be swallowed")
	}
	if calls != 1 {
		t.Errorf("invalid_request must not retry; got %d calls", calls)
	}
}

func TestClosePaneGivesUpAfterAttempts(t *testing.T) {
	fastBackoff(t)
	calls := 0
	stubCloser(t, func(string) error {
		calls++
		return &herdrError{Code: "internal", Message: "still busy"}
	})
	if err := closePane(context.Background(), "p1"); err == nil {
		t.Fatal("expected failure after exhausting retries")
	}
	if calls != closeAttempts {
		t.Errorf("expected %d attempts, got %d", closeAttempts, calls)
	}
}

func TestClosePaneHonorsContextCancel(t *testing.T) {
	// base backoff is real here (40ms); cancel before the first retry fires.
	calls := 0
	stubCloser(t, func(string) error {
		calls++
		return &herdrError{Code: "internal", Message: "busy"}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := closePane(ctx, "p1"); err == nil {
		t.Fatal("expected ctx cancellation to surface")
	}
	if calls != 1 {
		t.Errorf("cancel should stop after the first attempt; got %d", calls)
	}
}

// TestServeCloseBulkPartial: a bulk close where one pane was already
// cascade-closed (pane_not_found) and one is genuinely broken — the gone pane
// counts as closed, the broken one is reported.
func TestServeCloseBulkPartial(t *testing.T) {
	fastBackoff(t)
	stubCloser(t, func(id string) error {
		switch id {
		case "gone":
			return &herdrError{Code: "pane_not_found", Message: "pane gone not found"}
		case "broken":
			return &herdrError{Code: "internal", Message: "nope"}
		default:
			return nil
		}
	})
	body := strings.NewReader(`{"pane_ids":["ok","gone","broken"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/close", body)
	rec := httptest.NewRecorder()
	serveClose(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got struct {
		Closed []string          `json:"closed"`
		Errors map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	closedSet := map[string]bool{}
	for _, id := range got.Closed {
		closedSet[id] = true
	}
	if !closedSet["ok"] || !closedSet["gone"] {
		t.Errorf("expected ok+gone closed, got %v", got.Closed)
	}
	if _, ok := got.Errors["broken"]; !ok {
		t.Errorf("expected broken reported, got %v", got.Errors)
	}
	if closedSet["broken"] {
		t.Error("broken should not be reported as closed")
	}
}

func TestHerdrErrorParsing(t *testing.T) {
	// herdrError must round-trip from herdr's wire shape.
	var he herdrError
	if err := json.Unmarshal([]byte(`{"code":"pane_not_found","message":"pane x not found"}`), &he); err != nil {
		t.Fatal(err)
	}
	if he.Code != "pane_not_found" {
		t.Errorf("code = %q", he.Code)
	}
	if !strings.Contains(he.Error(), "pane_not_found") {
		t.Errorf("Error() = %q", he.Error())
	}
}
