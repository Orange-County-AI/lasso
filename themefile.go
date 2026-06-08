package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Lasso's appearance (light/dark) normally lives only in the browser
// (web/src/lib/mode.ts, localStorage). We mirror it to an authoritative file at
// ~/.lasso/settings.json so external tools — e.g. a shell statusline — can read
// the live light/dark state without driving a browser. settings.json is a
// general-purpose settings file; the theme lives under its top-level "theme"
// key. The browser POSTs to /api/theme whenever the applied appearance changes;
// this file is the source of truth for those readers.

// themeState is the shape of the "theme" object inside settings.json. The field
// order/names are a published contract for outside readers, so keep them stable.
type themeState struct {
	// Mode is the user's preference: "system" follows the OS, "light"/"dark"
	// pin it.
	Mode string `json:"mode"`
	// Resolved is the concrete appearance actually applied ("system" collapsed to
	// what the OS reports) — what a reader should key off.
	Resolved string `json:"resolved"`
	// Palette is the design-system name. Onyx is the only one today, but readers
	// shouldn't assume that, so we record it explicitly.
	Palette string `json:"palette"`
	// UpdatedAt is when this was last written, RFC3339/UTC.
	UpdatedAt string `json:"updatedAt"`
}

// themePalette is lasso's single design system (see theme.go). Persisted so a
// reader can branch on it even though it's constant today.
const themePalette = "onyx"

// themeSettingsKey is the top-level key the theme object nests under in
// settings.json.
const themeSettingsKey = "theme"

func lassoSettingsPath() string { return filepath.Join(lassoDir(), "settings.json") }

// validThemeMode / validResolved gate the enum values the handler accepts, so a
// bad client write can't poison the file readers depend on.
func validThemeMode(m string) bool {
	return m == "system" || m == "light" || m == "dark"
}

func validResolved(r string) bool {
	return r == "light" || r == "dark"
}

// readSettings loads settings.json as a generic key→raw-JSON map so we can
// merge a single key without knowing (or dropping) the rest. A missing or
// unparseable file yields an empty map — settings.json is shared with other
// tools, so we never fail a write just because we couldn't read what was there
// (but we do log a parse failure, since that means we're about to overwrite a
// corrupt file).
func readSettings() map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	b, err := os.ReadFile(lassoSettingsPath())
	if err != nil {
		return m // missing (or unreadable) → start fresh
	}
	if err := json.Unmarshal(b, &m); err != nil {
		log.Printf("settings.json unparseable, starting from empty: %v", err)
		return map[string]json.RawMessage{}
	}
	return m
}

// writeSettings serializes the whole settings map and writes settings.json
// atomically: a temp file in the same dir followed by os.Rename, so a
// concurrent reader never observes a half-written file.
func writeSettings(m map[string]json.RawMessage) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	dir := lassoDir()
	// Temp file in the SAME dir so os.Rename is an atomic same-filesystem move.
	tmp, err := os.CreateTemp(dir, "settings-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename succeeds.
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, lassoSettingsPath())
}

// writeThemeFile read-modify-writes settings.json: it stamps palette+updatedAt
// onto {mode, resolved}, replaces ONLY the "theme" key, and writes the whole map
// back. Any other top-level keys are preserved verbatim. Callers must pass
// already-validated enum values.
func writeThemeFile(mode, resolved string) (themeState, error) {
	ts := themeState{
		Mode:      mode,
		Resolved:  resolved,
		Palette:   themePalette,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(ts)
	if err != nil {
		return ts, err
	}
	m := readSettings()
	m[themeSettingsKey] = raw
	if err := writeSettings(m); err != nil {
		return ts, err
	}
	return ts, nil
}

// readThemeFile returns just the nested "theme" object from settings.json.
func readThemeFile() (themeState, error) {
	var ts themeState
	b, err := os.ReadFile(lassoSettingsPath())
	if err != nil {
		return ts, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return ts, err
	}
	raw, ok := m[themeSettingsKey]
	if !ok {
		return ts, nil // no theme key yet → zero value
	}
	err = json.Unmarshal(raw, &ts)
	return ts, err
}

// seedThemeFile makes sure settings.json has a "theme" key before any UI toggle,
// so a reader started before the browser still finds the appearance. Onyx is
// dark-forward, so the default is mode "system" resolved to "dark". A file that
// already has a "theme" key is left alone (the browser keeps it current); any
// other existing keys are preserved.
func seedThemeFile() error {
	m := readSettings()
	if _, ok := m[themeSettingsKey]; ok {
		return nil // theme already present; don't clobber a live value
	}
	_, err := writeThemeFile("system", "dark")
	return err
}

// ---------------------------------------------------------------------------
// GET/POST /api/theme — authoritative on-disk appearance for external readers.
// The wire shape is the theme object; on disk it merges into settings.json.
// ---------------------------------------------------------------------------

func serveTheme(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ts, err := readThemeFile()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, ts)
	case http.MethodPost:
		var in struct {
			Mode     string `json:"mode"`
			Resolved string `json:"resolved"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Reject unknown enum values rather than persisting garbage the statusline
		// would then have to defend against.
		if !validThemeMode(in.Mode) || !validResolved(in.Resolved) {
			http.Error(w, "mode must be system|light|dark and resolved light|dark", http.StatusBadRequest)
			return
		}
		ts, err := writeThemeFile(in.Mode, in.Resolved)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, ts)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}
