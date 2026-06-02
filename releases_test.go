package main

import (
	"fmt"
	"runtime"
	"testing"
)

func TestSemverNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.3.0", true},
		{"v0.2.0", "v0.2.1", true},
		{"0.2.0", "0.2.0", false},
		{"0.3.0", "0.2.9", false},
		{"1.0.0", "1.0.1", true},
		{"0.2.0", "v0.10.0", true}, // numeric compare, not lexical
		{"0.2.0", "0.2.1-rc1", true},
		{"0.2.0", "garbage", false}, // unparseable → not newer
		{"", "0.2.0", false},
	}
	for _, c := range cases {
		if got := semverNewer(c.a, c.b); got != c.want {
			t.Errorf("semverNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestAssetName(t *testing.T) {
	want := fmt.Sprintf("lasso-%s-%s", runtime.GOOS, runtime.GOARCH)
	if got := assetName(); got != want {
		t.Errorf("assetName() = %q, want %q", got, want)
	}
}

func TestChecksumFor(t *testing.T) {
	sums := "abc123  lasso-linux-amd64\ndef456  lasso-darwin-arm64\n"
	if got := checksumFor(sums, "lasso-darwin-arm64"); got != "def456" {
		t.Errorf("checksumFor = %q, want def456", got)
	}
	if got := checksumFor(sums, "lasso-windows-amd64"); got != "" {
		t.Errorf("checksumFor(missing) = %q, want empty", got)
	}
}
