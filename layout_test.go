package main

import "testing"

// layoutSignature must change when workspaces are reordered (their `number`
// flips), because the pane grid groups/sorts by that order — that's what makes
// the sidebar re-render to match herdr after a reorder. It must NOT change on a
// mere focus change, so a focus move doesn't trigger a full grid reload (the
// focus highlight is updated separately).
func TestLayoutSignatureReorder(t *testing.T) {
	panes := []pane{
		{PaneID: "p1", WorkspaceID: "wa", TabID: "ta"},
		{PaneID: "p2", WorkspaceID: "wb", TabID: "tb"},
	}
	original := []workspace{
		{WorkspaceID: "wa", Label: "alpha", Number: 1},
		{WorkspaceID: "wb", Label: "beta", Number: 2},
	}
	reordered := []workspace{
		{WorkspaceID: "wa", Label: "alpha", Number: 2}, // swapped
		{WorkspaceID: "wb", Label: "beta", Number: 1},
	}

	sigA := layoutSignature(panes, original)
	sigB := layoutSignature(panes, reordered)
	if sigA == sigB {
		t.Fatalf("signature unchanged after reorder; reorder would not refresh the grid\n got: %q", sigA)
	}

	// stable input -> stable signature (no spurious reloads)
	if sigA != layoutSignature(panes, original) {
		t.Fatal("signature not deterministic for identical input")
	}
}

func TestLayoutSignatureIgnoresFocus(t *testing.T) {
	wss := []workspace{{WorkspaceID: "wa", Label: "alpha", Number: 1}}
	unfocused := []pane{{PaneID: "p1", WorkspaceID: "wa", TabID: "ta", Focused: false}}
	focused := []pane{{PaneID: "p1", WorkspaceID: "wa", TabID: "ta", Focused: true}}
	if layoutSignature(unfocused, wss) != layoutSignature(focused, wss) {
		t.Fatal("signature changed on focus alone; would cause a needless grid reload on every focus move")
	}
}

func TestLayoutSignatureMembership(t *testing.T) {
	wss := []workspace{{WorkspaceID: "wa", Label: "alpha", Number: 1}}
	one := []pane{{PaneID: "p1", WorkspaceID: "wa", TabID: "ta"}}
	two := append(append([]pane{}, one...), pane{PaneID: "p2", WorkspaceID: "wa", TabID: "tb"})
	if layoutSignature(one, wss) == layoutSignature(two, wss) {
		t.Fatal("signature unchanged after a pane was added; grid would miss the new pane")
	}
	// pane ordering from herdr is not guaranteed stable — the signature must not
	// depend on the input slice order (it sorts internally).
	twoRev := []pane{two[1], two[0]}
	if layoutSignature(two, wss) != layoutSignature(twoRev, wss) {
		t.Fatal("signature depends on pane input order; would cause spurious reloads")
	}
}

func TestParseStatus(t *testing.T) {
	out := parseStatus(" M index.html\n?? new.txt\nA  staged.go\n D gone.txt\nR  old.go -> renamed.go\n")
	want := []diffFile{
		{Path: "index.html", Status: "modified", Staged: false},
		{Path: "new.txt", Status: "untracked", Staged: false},
		{Path: "staged.go", Status: "added", Staged: true},
		{Path: "gone.txt", Status: "deleted", Staged: false},
		{Path: "renamed.go", Status: "renamed", Staged: true},
	}
	if len(out) != len(want) {
		t.Fatalf("got %d files, want %d: %+v", len(out), len(want), out)
	}
	for i, w := range want {
		if out[i] != w {
			t.Errorf("file %d: got %+v, want %+v", i, out[i], w)
		}
	}
}
