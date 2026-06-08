package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// All paths resolve under LASSO_DIR (a temp dir) so these tests never touch a
// real ~/.lasso. lassoDir() honors LASSO_DIR and creates the dir, which is what
// writeThemeFile/seedThemeFile rely on.

func TestWriteThemeFileContents(t *testing.T) {
	base := t.TempDir()
	t.Setenv("LASSO_DIR", base)

	ts, err := writeThemeFile("system", "dark")
	if err != nil {
		t.Fatalf("writeThemeFile: %v", err)
	}
	if ts.Mode != "system" || ts.Resolved != "dark" || ts.Palette != "onyx" {
		t.Fatalf("unexpected returned state: %+v", ts)
	}
	if ts.UpdatedAt == "" {
		t.Fatalf("updatedAt not stamped")
	}

	// Read the nested "theme" object back off disk and assert it matches.
	got, err := readThemeFile()
	if err != nil {
		t.Fatalf("readThemeFile: %v", err)
	}
	if got != ts {
		t.Fatalf("round-trip mismatch: wrote %+v read %+v", ts, got)
	}
	if filepath.Dir(lassoSettingsPath()) != base {
		t.Fatalf("settings.json not under LASSO_DIR: %s", lassoSettingsPath())
	}

	// updatedAt must be RFC3339/UTC (the published contract for readers).
	if !strings.HasSuffix(got.UpdatedAt, "Z") {
		t.Fatalf("updatedAt not UTC RFC3339: %q", got.UpdatedAt)
	}
}

func TestWriteThemeFileNoTempLeftBehind(t *testing.T) {
	base := t.TempDir()
	t.Setenv("LASSO_DIR", base)

	if _, err := writeThemeFile("light", "light"); err != nil {
		t.Fatalf("writeThemeFile: %v", err)
	}
	// The atomic write uses a temp file in the same dir then os.Rename — no
	// *.tmp should survive a successful write.
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

// The write must be a merge: an unrelated top-level key already in settings.json
// survives a theme write untouched.
func TestWriteThemeFilePreservesOtherKeys(t *testing.T) {
	base := t.TempDir()
	t.Setenv("LASSO_DIR", base)

	// Seed settings.json with an unrelated key (and a nested value to be sure
	// structured data round-trips verbatim).
	pre := `{"foo":"bar","nested":{"a":1}}`
	if err := os.WriteFile(lassoSettingsPath(), []byte(pre), 0o644); err != nil {
		t.Fatalf("seed settings.json: %v", err)
	}

	if _, err := writeThemeFile("dark", "dark"); err != nil {
		t.Fatalf("writeThemeFile: %v", err)
	}

	var m map[string]json.RawMessage
	b, err := os.ReadFile(lassoSettingsPath())
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal settings.json: %v", err)
	}

	// Unrelated keys preserved (compared semantically — MarshalIndent may
	// reindent nested values' whitespace, but the data must round-trip intact).
	if string(m["foo"]) != `"bar"` {
		t.Fatalf("foo not preserved: %s", m["foo"])
	}
	var nested map[string]int
	if err := json.Unmarshal(m["nested"], &nested); err != nil || nested["a"] != 1 {
		t.Fatalf("nested not preserved: %s (err=%v)", m["nested"], err)
	}
	// Theme present and correct.
	var ts themeState
	if err := json.Unmarshal(m[themeSettingsKey], &ts); err != nil {
		t.Fatalf("unmarshal theme: %v", err)
	}
	if ts.Mode != "dark" || ts.Resolved != "dark" || ts.Palette != "onyx" {
		t.Fatalf("unexpected theme: %+v", ts)
	}
}

func TestSeedThemeFileDefaultsAndNoClobber(t *testing.T) {
	base := t.TempDir()
	t.Setenv("LASSO_DIR", base)

	// Missing → seed writes dark-forward defaults.
	if err := seedThemeFile(); err != nil {
		t.Fatalf("seedThemeFile: %v", err)
	}
	got, err := readThemeFile()
	if err != nil {
		t.Fatalf("readThemeFile: %v", err)
	}
	if got.Mode != "system" || got.Resolved != "dark" {
		t.Fatalf("unexpected seed defaults: %+v", got)
	}

	// theme key present → seed must NOT clobber a newer value the browser persisted.
	if _, err := writeThemeFile("light", "light"); err != nil {
		t.Fatalf("writeThemeFile: %v", err)
	}
	if err := seedThemeFile(); err != nil {
		t.Fatalf("seedThemeFile (existing): %v", err)
	}
	got, err = readThemeFile()
	if err != nil {
		t.Fatalf("readThemeFile: %v", err)
	}
	if got.Mode != "light" || got.Resolved != "light" {
		t.Fatalf("seed clobbered existing value: %+v", got)
	}
}

// seed into a settings.json that exists but has no "theme" key must add theme
// without dropping the other keys.
func TestSeedThemeFilePreservesOtherKeys(t *testing.T) {
	base := t.TempDir()
	t.Setenv("LASSO_DIR", base)

	if err := os.WriteFile(lassoSettingsPath(), []byte(`{"foo":"bar"}`), 0o644); err != nil {
		t.Fatalf("seed settings.json: %v", err)
	}
	if err := seedThemeFile(); err != nil {
		t.Fatalf("seedThemeFile: %v", err)
	}

	var m map[string]json.RawMessage
	b, _ := os.ReadFile(lassoSettingsPath())
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(m["foo"]) != `"bar"` {
		t.Fatalf("foo dropped during seed: %s", m["foo"])
	}
	if _, ok := m[themeSettingsKey]; !ok {
		t.Fatalf("theme not seeded into existing settings.json")
	}
}

func TestServeThemePostValidAndGet(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())

	req := httptest.NewRequest(http.MethodPost, "/api/theme",
		strings.NewReader(`{"mode":"dark","resolved":"dark"}`))
	rec := httptest.NewRecorder()
	serveTheme(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body %q", rec.Code, rec.Body.String())
	}
	var posted themeState
	if err := json.Unmarshal(rec.Body.Bytes(), &posted); err != nil {
		t.Fatalf("decode POST response: %v", err)
	}
	if posted.Mode != "dark" || posted.Resolved != "dark" || posted.Palette != "onyx" {
		t.Fatalf("unexpected POST response: %+v", posted)
	}

	// GET returns the nested theme object we just wrote.
	greq := httptest.NewRequest(http.MethodGet, "/api/theme", nil)
	grec := httptest.NewRecorder()
	serveTheme(grec, greq)
	if grec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", grec.Code)
	}
	var got themeState
	if err := json.Unmarshal(grec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if got != posted {
		t.Fatalf("GET %+v != POST %+v", got, posted)
	}
}

// POST through the handler must also merge, leaving unrelated keys intact.
func TestServeThemePostMergesIntoSettings(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())

	if err := os.WriteFile(lassoSettingsPath(), []byte(`{"foo":"bar"}`), 0o644); err != nil {
		t.Fatalf("seed settings.json: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/theme",
		strings.NewReader(`{"mode":"light","resolved":"light"}`))
	rec := httptest.NewRecorder()
	serveTheme(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body %q", rec.Code, rec.Body.String())
	}

	var m map[string]json.RawMessage
	b, _ := os.ReadFile(lassoSettingsPath())
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(m["foo"]) != `"bar"` {
		t.Fatalf("foo not preserved through handler: %s", m["foo"])
	}
	var ts themeState
	if err := json.Unmarshal(m[themeSettingsKey], &ts); err != nil {
		t.Fatalf("unmarshal theme: %v", err)
	}
	if ts.Mode != "light" || ts.Resolved != "light" {
		t.Fatalf("unexpected theme after handler POST: %+v", ts)
	}
}

func TestServeThemeRejectsBadEnums(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())

	cases := []string{
		`{"mode":"purple","resolved":"dark"}`,   // bad mode
		`{"mode":"system","resolved":"system"}`, // resolved can't be "system"
		`{"mode":"dark"}`,                       // missing resolved
		`not json`,                              // malformed body
	}
	for _, body := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/theme", strings.NewReader(body))
		rec := httptest.NewRecorder()
		serveTheme(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
	// A rejected write must not have created the file.
	if _, err := os.Stat(lassoSettingsPath()); !os.IsNotExist(err) {
		t.Fatalf("bad write created settings.json (err=%v)", err)
	}
}
