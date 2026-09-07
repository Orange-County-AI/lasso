package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// The Usage sidebar tab: the rich view of what the Luvus Bar widget summarizes.
// Both read the same collectUsage; this side adds a short shared cache so the
// tab's polling across several browser tabs collapses to one upstream round.

var usageCache struct {
	mu      sync.Mutex
	at      time.Time
	payload usagePayload
}

const usageCacheTTL = 25 * time.Second

func serveUsage(w http.ResponseWriter, r *http.Request) {
	usageCache.mu.Lock()
	defer usageCache.mu.Unlock()
	if time.Since(usageCache.at) < usageCacheTTL && usageCache.payload.UpdatedAt != "" {
		writeJSON(w, usageCache.payload)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	usageCache.payload = collectUsage(ctx)
	usageCache.at = time.Now()
	writeJSON(w, usageCache.payload)
}

// usageBarModuleID is the Luvus module that owns the Bar widget. The tab's
// "shown in the Luvus Bar" controls edit THAT module's settings over UHP, so
// there is exactly one source of truth for what the bar shows.
const usageBarModuleID = "lasso.usage-bar"

// usageBarModule is /api/usage-bar's payload: whether the module is linked on
// the default host, and its current settings when it is.
type usageBarModule struct {
	Linked   bool           `json:"linked"`
	Enabled  bool           `json:"enabled"`
	Settings map[string]any `json:"settings,omitempty"`
}

func readUsageBarModule(b Backend) usageBarModule {
	res, err := b.LuvusCall("module.list", map[string]any{})
	if err != nil {
		return usageBarModule{}
	}
	var list struct {
		Modules []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"modules"`
	}
	if json.Unmarshal(res, &list) != nil {
		return usageBarModule{}
	}
	out := usageBarModule{}
	for _, m := range list.Modules {
		if m.ID == usageBarModuleID {
			out.Linked, out.Enabled = true, m.Enabled
		}
	}
	if !out.Linked {
		return out
	}
	res, err = b.LuvusCall("module.settings.list", map[string]any{"id": usageBarModuleID})
	if err != nil {
		return out
	}
	var settings struct {
		Settings []struct {
			Key   string `json:"key"`
			Value any    `json:"value"`
		} `json:"settings"`
	}
	if json.Unmarshal(res, &settings) == nil {
		out.Settings = make(map[string]any, len(settings.Settings))
		for _, s := range settings.Settings {
			out.Settings[s.Key] = s.Value
		}
	}
	return out
}

// serveUsageBar: GET reads the module state; POST {key, value} writes one
// setting and re-runs the module's refresh so the bar repaints immediately.
func serveUsageBar(w http.ResponseWriter, r *http.Request) {
	b := defaultBackend()
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, readUsageBarModule(b))
	case http.MethodPost:
		var req struct {
			Key   string `json:"key"`
			Value any    `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
			http.Error(w, "key and value required", http.StatusBadRequest)
			return
		}
		if _, err := b.LuvusCall("module.settings.set", map[string]any{
			"id": usageBarModuleID, "key": req.Key, "value": req.Value,
		}); err != nil {
			var le *luvusError
			if errors.As(err, &le) && le.Code == "not_found" {
				http.Error(w, "the lasso.usage-bar module is not linked", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// Best-effort repaint: the setting is saved either way, and the next
		// agent-status event re-runs the module's refresh regardless.
		_, _ = b.LuvusCall("module.action.invoke", map[string]any{
			"module": usageBarModuleID, "action": "refresh",
		})
		writeJSON(w, readUsageBarModule(b))
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}
