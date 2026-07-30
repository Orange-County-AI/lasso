package main

import (
	"context"
	"testing"
	"time"
)

// addHostClient provisions a per-host credential the way the CLI does, and
// returns its id.
func addHostClient(t *testing.T, id, host, scope string) string {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO oauth_clients (client_id, secret_hash, redirect_uris, name, created_at, host, mcp_scope)
		 VALUES (?, ?, '[]', ?, ?, ?, ?)`,
		id, hashToken("secret-"+id), "agents on "+host, time.Now().UTC().Format(time.RFC3339), host, scope,
	); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestParseTTL(t *testing.T) {
	// Everything that means "no expiry" — the default for a machine credential.
	for _, s := range []string{"", "never", "NONE", "0", "  "} {
		d, err := parseTTL(s)
		if err != nil || d != 0 {
			t.Errorf("parseTTL(%q) = (%v, %v), want (0, nil)", s, d, err)
		}
	}
	cases := map[string]time.Duration{
		"90d":  90 * 24 * time.Hour,
		"2w":   14 * 24 * time.Hour,
		"12h":  12 * time.Hour,
		"30m":  30 * time.Minute,
		"1.5d": 36 * time.Hour,
	}
	for s, want := range cases {
		got, err := parseTTL(s)
		if err != nil || got != want {
			t.Errorf("parseTTL(%q) = (%v, %v), want %v", s, got, err, want)
		}
	}
	// Garbage and non-positive durations are refused rather than silently becoming
	// "never", which would turn a typo into an immortal credential.
	for _, s := range []string{"bananas", "-5d", "0d", "d", "90dd"} {
		if d, err := parseTTL(s); err == nil {
			t.Errorf("parseTTL(%q) = (%v, nil), want an error", s, d)
		}
	}
}

// The default token never expires: stored with an empty expires_at, and
// verifyAccessToken reports it valid with a ZERO expiry to say so.
func TestMintClientTokenNeverExpires(t *testing.T) {
	openTestDB(t)
	stubSSHHosts(t, "gigachad")
	id := addHostClient(t, "pod-cid", "gigachad", scopeSelf)

	tok, c, err := mintClientToken(id, 0)
	if err != nil {
		t.Fatalf("mintClientToken: %v", err)
	}
	if c.Host != "gigachad" {
		t.Errorf("client host = %q, want gigachad", c.Host)
	}
	var expires string
	if err := db.QueryRow(`SELECT expires_at FROM oauth_tokens WHERE token_hash = ?`,
		hashToken(tok)).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires != "" {
		t.Errorf("expires_at = %q, want empty (never expires)", expires)
	}
	gotID, exp, ok := verifyAccessToken(tok)
	if !ok || gotID != id {
		t.Fatalf("verifyAccessToken = (%q, %v), want (%s, true)", gotID, ok, id)
	}
	if !exp.IsZero() {
		t.Errorf("expiry = %v, want the zero time to mean never", exp)
	}
	// The SDK refuses a TokenInfo with a zero Expiration, so the verifier has to
	// synthesize a future one at the boundary — otherwise every never-expiring
	// token would 401 with "token missing expiration".
	ti, err := mcpTokenVerifier(context.Background(), tok, nil)
	if err != nil {
		t.Fatalf("mcpTokenVerifier: %v", err)
	}
	if ti.Expiration.IsZero() || ti.Expiration.Before(time.Now()) {
		t.Errorf("TokenInfo.Expiration = %v, want a future instant", ti.Expiration)
	}
}

// A TTL'd token behaves as before, and an expired one stops verifying.
func TestMintClientTokenWithTTL(t *testing.T) {
	openTestDB(t)
	stubSSHHosts(t, "gigachad")
	id := addHostClient(t, "pod-cid", "gigachad", scopeSelf)

	tok, _, err := mintClientToken(id, 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, exp, ok := verifyAccessToken(tok)
	if !ok || exp.IsZero() {
		t.Fatalf("verifyAccessToken = (%v, %v), want a dated, valid token", exp, ok)
	}
	// Backdate it: an expired token must stop working.
	if _, err := db.Exec(`UPDATE oauth_tokens SET expires_at = ? WHERE token_hash = ?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), hashToken(tok)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := verifyAccessToken(tok); ok {
		t.Error("an expired token still verified")
	}
	// A corrupt expiry fails closed rather than reading as "never".
	if _, err := db.Exec(`UPDATE oauth_tokens SET expires_at = 'not-a-date' WHERE token_hash = ?`,
		hashToken(tok)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := verifyAccessToken(tok); ok {
		t.Error("a token with an unparseable expiry verified")
	}
}

// The purge must not collect never-expiring tokens. SQLite compares expires_at
// as a string and ” sorts before every timestamp, so a naive `expires_at < now`
// would delete exactly the tokens meant to outlive everything.
func TestPurgeKeepsNeverExpiringTokens(t *testing.T) {
	openTestDB(t)
	stubSSHHosts(t, "gigachad")
	id := addHostClient(t, "pod-cid", "gigachad", scopeSelf)

	forever, _, err := mintClientToken(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	doomed, _, err := mintClientToken(id, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE oauth_tokens SET expires_at = ? WHERE token_hash = ?`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), hashToken(doomed)); err != nil {
		t.Fatal(err)
	}

	purgeExpiredOAuth()

	if _, _, ok := verifyAccessToken(forever); !ok {
		t.Error("the purge collected a never-expiring token")
	}
	if _, _, ok := verifyAccessToken(doomed); ok {
		t.Error("the purge left an expired token behind")
	}
}

// Machine tokens are only for credentials an operator minted: a DCR client is
// refused here exactly as it is at the client_credentials endpoint.
func TestMintClientTokenRefusesDCRAndUnknownClients(t *testing.T) {
	openTestDB(t)
	if _, err := db.Exec(
		`INSERT INTO oauth_clients (client_id, secret_hash, redirect_uris, name, created_at)
		 VALUES ('dcr-cid', ?, '[]', 'some connector', ?)`,
		hashToken("s"), time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mintClientToken("dcr-cid", 0); err == nil {
		t.Error("a DCR client got a machine token")
	}
	if _, _, err := mintClientToken("nope", 0); err == nil {
		t.Error("an unknown client got a machine token")
	}
}

// A never-expiring token drives the real endpoint, scoped to its host — the
// whole point of minting one.
func TestNeverExpiringTokenWorksOverTheEndpoint(t *testing.T) {
	openTestDB(t)
	enableOAuth(t, "")
	stubSSHHosts(t, "gigachad")
	if err := appendAgent("gigachad", AgentRecord{ID: "gig1", Title: "pod agent", Type: "git",
		RootPane: "wR:p2", WorkDir: "/w/gig1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	id := addHostClient(t, "pod-cid", "gigachad", scopeSelf)
	tok, _, err := mintClientToken(id, 0)
	if err != nil {
		t.Fatal(err)
	}

	sess := mcpTestSession(t, tok)
	out, errMsg := callListAgents(t, sess, map[string]any{})
	if errMsg != "" {
		t.Fatalf("list_agents with a never-expiring token: %s", errMsg)
	}
	if out.Host != "gigachad" || len(out.Agents) != 1 || out.Agents[0].ID != "gig1" {
		t.Errorf("out = %+v, want gigachad/gig1", out)
	}
	if _, errMsg = callListAgents(t, sess, map[string]any{"host": "local"}); errMsg == "" {
		t.Error("a never-expiring token escaped its host scope")
	}
}
