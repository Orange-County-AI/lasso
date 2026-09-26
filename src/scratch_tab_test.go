package main

import (
	"encoding/json"
	"errors"
	"testing"
)

// scratchTabBackend is a herdr with a configurable workspace list, recording
// every call so a test can assert where a scratch agent's tab landed.
type scratchTabBackend struct {
	*memBackend
	workspaces   string
	tabCreateErr error
	calls        []string
	params       map[string]map[string]any
}

func (b *scratchTabBackend) HerdrCall(method string, params any) (json.RawMessage, error) {
	p, _ := params.(map[string]any)
	b.calls = append(b.calls, method)
	b.params[method] = p
	switch method {
	case "workspace.list":
		return json.RawMessage(b.workspaces), nil
	case "tab.create":
		if b.tabCreateErr != nil {
			return nil, b.tabCreateErr
		}
		return json.RawMessage(`{"tab":{"tab_id":"s:t2","workspace_id":"s"},"root_pane":{"pane_id":"p2"}}`), nil
	case "workspace.create":
		return json.RawMessage(`{"workspace":{"workspace_id":"new"},"tab":{"tab_id":"new:t1","workspace_id":"new"},"root_pane":{"pane_id":"p1"}}`), nil
	}
	return json.RawMessage(`{}`), nil
}

func newScratchTabBackend(workspaces string) *scratchTabBackend {
	return &scratchTabBackend{memBackend: newMemBackend(), workspaces: workspaces, params: map[string]map[string]any{}}
}

func TestOpenScratchTabJoinsExistingScratch(t *testing.T) {
	b := newScratchTabBackend(`{"workspaces":[{"workspace_id":"w","label":"~"},{"workspace_id":"s","label":"Scratch"}]}`)
	ws, pane, err := openScratchTab(b, "/scratch/a-1234", "Fix the flaky test", true)
	if err != nil {
		t.Fatal(err)
	}
	if ws != "s" || pane != "p2" {
		t.Fatalf("got ws=%q pane=%q, want s/p2", ws, pane)
	}
	tc := b.params["tab.create"]
	if tc["workspace_id"] != "s" || tc["cwd"] != "/scratch/a-1234" || tc["label"] != "Fix the flaky test" || tc["focus"] != true {
		t.Fatalf("tab.create params = %#v", tc)
	}
	if _, ok := b.params["workspace.create"]; ok {
		t.Fatal("created a second Scratch workspace")
	}
}

func TestOpenScratchTabCreatesScratchWhenMissing(t *testing.T) {
	b := newScratchTabBackend(`{"workspaces":[{"workspace_id":"w","label":"~"}]}`)
	ws, pane, err := openScratchTab(b, "/scratch/a-1234", "Fix it", false)
	if err != nil {
		t.Fatal(err)
	}
	if ws != "new" || pane != "p1" {
		t.Fatalf("got ws=%q pane=%q, want new/p1", ws, pane)
	}
	if got := b.params["workspace.create"]["label"]; got != scratchWorkspaceLabel {
		t.Fatalf("workspace label = %#v, want Scratch", got)
	}
	// The root tab carries the agent's name, not the workspace's.
	if r := b.params["tab.rename"]; r["tab_id"] != "new:t1" || r["label"] != "Fix it" {
		t.Fatalf("tab.rename params = %#v", r)
	}
}

func TestOpenScratchTabRecreatesVanishedScratch(t *testing.T) {
	b := newScratchTabBackend(`{"workspaces":[{"workspace_id":"s","label":"Scratch"}]}`)
	b.tabCreateErr = errors.New("workspace_not_found")
	ws, _, err := openScratchTab(b, "/scratch/x", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	if ws != "new" {
		t.Fatalf("ws = %q, want the recreated workspace", ws)
	}
}

func TestOpenScratchTabSurfacesOtherErrors(t *testing.T) {
	b := newScratchTabBackend(`{"workspaces":[{"workspace_id":"s","label":"Scratch"}]}`)
	b.tabCreateErr = errors.New("boom")
	if _, _, err := openScratchTab(b, "/scratch/x", "x", true); err == nil {
		t.Fatal("want an error")
	}
	if _, ok := b.params["workspace.create"]; ok {
		t.Fatal("a real tab.create failure must not spawn a new workspace")
	}
}
