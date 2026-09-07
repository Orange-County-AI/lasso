package main

import "testing"

// A new tab's default name is the smallest free positive integer, judged by the
// names actually in use — not by the tab count, which drifts as soon as a tab
// is closed or renamed.
func TestNextTabName(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{nil, "1"},
		{[]string{"", ""}, "1"},            // unnamed tabs reserve nothing
		{[]string{"1", "2"}, "3"},          // monotonic on a fresh workspace
		{[]string{"1", "3"}, "2"},          // a closed tab's number is reused
		{[]string{"logs", "1", "3b"}, "2"}, // non-numeric names neither reserve nor unlock
		{[]string{"2", "0", "-1"}, "1"},    // zero/negative are not tab numbers
		{[]string{" 1 ", "2"}, "3"},        // whitespace tolerated
	}
	for _, c := range cases {
		if got := nextTabName(c.names); got != c.want {
			t.Errorf("nextTabName(%q) = %q, want %q", c.names, got, c.want)
		}
	}
}
