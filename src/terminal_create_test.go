package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type terminalCreateBackend struct {
	*memBackend
	focus any
}

func (b *terminalCreateBackend) HomeDir() (string, error) { return "/home/test", nil }

func (b *terminalCreateBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	if method == "workspace.create" {
		p, _ := params.(map[string]any)
		b.focus = p["focus"]
	}
	return json.RawMessage(`{"workspace":{"workspace_id":"ws"},"root_pane":{"pane_id":"p1"}}`), nil
}

func TestCreateTerminalFocus(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{name: "defaults true", body: `{}`, want: true},
		{name: "caller opts out", body: `{"focus":false}`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &terminalCreateBackend{memBackend: newMemBackend()}
			prev := curBackend()
			setBackend(b)
			t.Cleanup(func() { setBackend(prev) })

			req := httptest.NewRequest(http.MethodPost, "/api/create-terminal", strings.NewReader(tc.body))
			res := httptest.NewRecorder()
			serveCreateTerminal(res, req)

			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
			}
			if got, ok := b.focus.(bool); !ok || got != tc.want {
				t.Fatalf("workspace.create focus = %#v, want %v", b.focus, tc.want)
			}
		})
	}
}
