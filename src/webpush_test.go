package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The RFC 8291 §5 example, verbatim. It is the only way to prove the key
// derivation is right without a browser: every intermediate value is defined by
// the salt and the sender's ephemeral key, so pinning both reproduces the
// published body byte for byte. A single wrong info string or a swapped
// HKDF salt/IKM pair changes the ciphertext completely.
const (
	rfc8291Plaintext  = "When I grow up, I want to be a watermelon"
	rfc8291AuthSecret = "BTBZMqHH6r4Tts7J_aSIgg"
	rfc8291UAPublic   = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	rfc8291UAPrivate  = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	rfc8291ASPrivate  = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	rfc8291Salt       = "DGv6ra1nlYgDCS1FRnbzlw"
	rfc8291Body       = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64decode(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

func TestSealWebPushMatchesRFC8291Vector(t *testing.T) {
	as, err := ecdh.P256().NewPrivateKey(mustB64(t, rfc8291ASPrivate))
	if err != nil {
		t.Fatalf("sender key: %v", err)
	}
	body, err := sealWebPush(
		mustB64(t, rfc8291UAPublic),
		mustB64(t, rfc8291AuthSecret),
		[]byte(rfc8291Plaintext),
		mustB64(t, rfc8291Salt),
		as,
	)
	if err != nil {
		t.Fatalf("sealWebPush: %v", err)
	}
	if got := b64.EncodeToString(body); got != rfc8291Body {
		t.Errorf("body mismatch\n got %s\nwant %s", got, rfc8291Body)
	}
}

// openWebPush is the receiver half — the derivation a browser runs — so a
// round-trip test can assert the phone would actually be able to read what
// lasso sent (and that the record is framed the way RFC 8188 says).
func openWebPush(t *testing.T, uaPrivate *ecdh.PrivateKey, authSecret, body []byte) []byte {
	t.Helper()
	if len(body) < 21 {
		t.Fatalf("body too short: %d bytes", len(body))
	}
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if rs != webPushRecordSize {
		t.Errorf("rs = %d, want %d", rs, webPushRecordSize)
	}
	if idlen != 65 {
		t.Fatalf("keyid length = %d, want 65 (an uncompressed P-256 point)", idlen)
	}
	asPublicBytes := body[21 : 21+idlen]
	ciphertext := body[21+idlen:]
	asPublic, err := ecdh.P256().NewPublicKey(asPublicBytes)
	if err != nil {
		t.Fatalf("keyid is not a P-256 point: %v", err)
	}
	shared, err := uaPrivate.ECDH(asPublic)
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	keyInfo := append([]byte("WebPush: info\x00"), uaPrivate.PublicKey().Bytes()...)
	keyInfo = append(keyInfo, asPublicBytes...)
	ikm, err := hkdf.Key(sha256.New, shared, authSecret, string(keyInfo), 32)
	if err != nil {
		t.Fatalf("hkdf key: %v", err)
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		t.Fatalf("hkdf extract: %v", err)
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		t.Fatalf("hkdf cek: %v", err)
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		t.Fatalf("hkdf nonce: %v", err)
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	record, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if len(record) == 0 || record[len(record)-1] != 0x02 {
		t.Fatalf("record does not end in the 0x02 padding delimiter")
	}
	return record[:len(record)-1]
}

func TestSealWebPushRoundTripsWithFreshKeys(t *testing.T) {
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("receiver key: %v", err)
	}
	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"title":"blocked","body":"claude needs you"}`)
	body, err := sealWebPushRandom(ua.PublicKey().Bytes(), authSecret, want)
	if err != nil {
		t.Fatalf("sealWebPushRandom: %v", err)
	}
	if got := openWebPush(t, ua, authSecret, body); string(got) != string(want) {
		t.Errorf("round trip = %q, want %q", got, want)
	}
}

func TestSealWebPushRejectsBadInputs(t *testing.T) {
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	good := ua.PublicKey().Bytes()
	auth := make([]byte, 16)
	salt := make([]byte, 16)

	cases := []struct {
		name             string
		uaPub            []byte
		auth, salt, text []byte
	}{
		// A key that isn't on the curve must be refused before it is used, per
		// RFC 8291 §7 — the stdlib check is the one doing the work here.
		{"public key not on P-256", append([]byte{0x04}, make([]byte, 64)...), auth, salt, []byte("x")},
		{"auth secret wrong length", good, make([]byte, 12), salt, []byte("x")},
		{"salt wrong length", good, auth, make([]byte, 8), []byte("x")},
		// One record only, and a push service need not accept more than 4096
		// octets — so an oversized payload has to fail here rather than at the
		// push service, where it would look like a delivery outage.
		{"payload too large", good, auth, salt, make([]byte, maxWebPushPlaintext+1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := sealWebPush(c.uaPub, c.auth, c.text, c.salt, as); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

// resetVapidCache clears the memoized keypair so each test derives one from its
// own temp db rather than inheriting the previous test's.
func resetVapidCache(t *testing.T) {
	t.Helper()
	clear := func() {
		vapidCache.Lock()
		vapidCache.key, vapidCache.pub = nil, ""
		vapidCache.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func TestVapidKeyPersistsAcrossReload(t *testing.T) {
	openTestDB(t)
	resetVapidCache(t)
	_, pub, err := vapidKey()
	if err != nil {
		t.Fatalf("vapidKey: %v", err)
	}
	if raw := mustB64(t, pub); len(raw) != 65 || raw[0] != 0x04 {
		t.Fatalf("public key is not an uncompressed P-256 point (%d bytes)", len(raw))
	}
	// A second call after dropping the memo must read the stored key, not mint a
	// new one: the browser pins this value into every subscription, so a key that
	// changes across a restart silently breaks every registered device.
	resetVapidCache(t)
	_, again, err := vapidKey()
	if err != nil {
		t.Fatalf("vapidKey (reload): %v", err)
	}
	if again != pub {
		t.Errorf("key changed across reload:\n%s\n%s", pub, again)
	}
}

func TestVapidAuthHeaderIsAVerifiableES256JWT(t *testing.T) {
	openTestDB(t)
	resetVapidCache(t)
	key, pub, err := vapidKey()
	if err != nil {
		t.Fatalf("vapidKey: %v", err)
	}
	now := time.Now()
	hdr, err := vapidAuthHeader("https://web.push.apple.com/xyz/abc", "https://lasso.example.com", key, pub, now)
	if err != nil {
		t.Fatalf("vapidAuthHeader: %v", err)
	}
	rest, ok := strings.CutPrefix(hdr, "vapid t=")
	if !ok {
		t.Fatalf("header does not start with `vapid t=`: %q", hdr)
	}
	jwt, k, ok := strings.Cut(rest, ", k=")
	if !ok {
		t.Fatalf("header carries no `k=` key: %q", hdr)
	}
	if k != pub {
		t.Errorf("k = %q, want the server's public key %q", k, pub)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts, want 3", len(parts))
	}
	var header struct{ Typ, Alg string }
	if err := json.Unmarshal(mustB64(t, parts[0]), &header); err != nil {
		t.Fatalf("header: %v", err)
	}
	if header.Typ != "JWT" || header.Alg != "ES256" {
		t.Errorf("header = %+v, want JWT/ES256", header)
	}
	var claims struct {
		Aud string `json:"aud"`
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(mustB64(t, parts[1]), &claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	// The audience is the push service's ORIGIN — path and query stripped. A
	// service rejects a token whose aud names the full endpoint.
	if claims.Aud != "https://web.push.apple.com" {
		t.Errorf("aud = %q, want the endpoint's origin", claims.Aud)
	}
	if claims.Sub != "https://lasso.example.com" {
		t.Errorf("sub = %q", claims.Sub)
	}
	if d := time.Unix(claims.Exp, 0).Sub(now); d <= 0 || d > 24*time.Hour {
		t.Errorf("exp is %v out; must be in the future and within the 24h services allow", d)
	}
	sig := mustB64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want the 64-byte r||s pair", len(sig))
	}
	pubKey, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), mustB64(t, k))
	if err != nil {
		t.Fatalf("parse k: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pubKey, digest[:], r, s) {
		t.Error("signature does not verify against the advertised key")
	}
}

func TestVapidSubject(t *testing.T) {
	// Apple answers 403 BadJwtToken for anything that is not a mailto: or an
	// https URL, so the page origin is the default.
	if got := vapidSubject("https://lasso.orangecountyai.com/"); got != "https://lasso.orangecountyai.com" {
		t.Errorf("https origin: got %q", got)
	}
	// A loopback dev origin is not a valid subject; the mailto fallback is.
	if got := vapidSubject("http://127.0.0.1:8090"); !strings.HasPrefix(got, "mailto:") {
		t.Errorf("loopback origin: got %q, want a mailto: fallback", got)
	}
	t.Setenv("LASSO_PUSH_CONTACT", "mailto:me@example.com")
	if got := vapidSubject("https://lasso.orangecountyai.com"); got != "mailto:me@example.com" {
		t.Errorf("explicit contact must win: got %q", got)
	}
}

// pushTestSub registers a subscription pointed at endpoint, returning the
// receiver key and auth secret so the test can decrypt what lands.
func pushTestSub(t *testing.T, endpoint string) (*ecdh.PrivateKey, []byte) {
	t.Helper()
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	sub := pushSubscription{
		Endpoint: endpoint,
		P256dh:   b64.EncodeToString(ua.PublicKey().Bytes()),
		Auth:     b64.EncodeToString(auth),
		Origin:   "https://lasso.example.com",
		Label:    "iPhone",
	}
	if err := upsertPushSubscription(sub); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return ua, auth
}

func TestWebPushDeliverEncryptsPayloadAndSetsProtocolHeaders(t *testing.T) {
	openTestDB(t)
	resetVapidCache(t)

	type got struct {
		auth, encoding, ttl, urgency, contentType string
		body                                      []byte
	}
	seen := make(chan got, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, b); err != nil {
			t.Errorf("read body: %v", err)
		}
		seen <- got{
			auth:        r.Header.Get("Authorization"),
			encoding:    r.Header.Get("Content-Encoding"),
			ttl:         r.Header.Get("TTL"),
			urgency:     r.Header.Get("Urgency"),
			contentType: r.Header.Get("Content-Type"),
			body:        b,
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	ua, uaAuth := pushTestSub(t, srv.URL+"/device/1")
	err := webPushChannel{}.deliver(context.Background(), notification{
		Kind:  notifAgentBlocked,
		Title: "Fix the push flow",
		Body:  "claude is blocked on titan and needs you.",
		Tag:   "blocked:local\x00p1",
		Host:  "local",
	})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	g := <-seen
	if !strings.HasPrefix(g.auth, "vapid t=") || !strings.Contains(g.auth, ", k=") {
		t.Errorf("Authorization = %q, want a VAPID header", g.auth)
	}
	if g.encoding != "aes128gcm" {
		t.Errorf("Content-Encoding = %q", g.encoding)
	}
	if g.contentType != "application/octet-stream" {
		t.Errorf("Content-Type = %q", g.contentType)
	}
	if g.ttl == "" {
		t.Error("TTL header is required by RFC 8030 and was not sent")
	}
	if g.urgency != "high" {
		t.Errorf("Urgency = %q, want high (a blocked agent must wake the device)", g.urgency)
	}
	var payload webPushPayload
	if err := json.Unmarshal(openWebPush(t, ua, uaAuth, g.body), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	// Everything the worker needs must be in the encrypted payload: behind
	// Cloudflare Access it cannot fetch anything back from lasso.
	if payload.Title != "Fix the push flow" || payload.Host != "local" ||
		payload.Kind != string(notifAgentBlocked) || payload.Tag == "" || payload.Body == "" {
		t.Errorf("payload = %+v", payload)
	}
	// A successful delivery clears any prior error and stamps last_ok.
	subs, _ := listPushSubscriptions()
	if len(subs) != 1 || subs[0].LastOK == "" || subs[0].LastError != "" {
		t.Errorf("subscription after success = %+v", subs)
	}
}

func TestWebPushDropsGoneSubscriptionAndKeepsFailingOne(t *testing.T) {
	openTestDB(t)
	resetVapidCache(t)

	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer gone.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "BadJwtToken", http.StatusForbidden)
	}))
	defer broken.Close()

	pushTestSub(t, gone.URL+"/dead")
	pushTestSub(t, broken.URL+"/alive")

	err := webPushChannel{}.deliver(context.Background(), notification{Kind: notifTest, Title: "hi"})
	if err == nil {
		t.Error("a 403 must surface as an error so the Settings test button can show it")
	}
	subs, _ := listPushSubscriptions()
	if len(subs) != 1 {
		t.Fatalf("want 1 surviving subscription, got %d", len(subs))
	}
	if !strings.Contains(subs[0].Endpoint, "/alive") {
		t.Errorf("wrong subscription survived: %q", subs[0].Endpoint)
	}
	// A transient failure is recorded, never pruned: the device is fine, our
	// credentials or the service are not.
	if !strings.Contains(subs[0].LastError, "403") {
		t.Errorf("last_error = %q, want the 403 recorded", subs[0].LastError)
	}
}

func TestSendWebPushGoneIsSentinel(t *testing.T) {
	openTestDB(t)
	resetVapidCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	sub := pushSubscription{
		Endpoint: srv.URL,
		P256dh:   b64.EncodeToString(ua.PublicKey().Bytes()),
		Auth:     b64.EncodeToString(make([]byte, 16)),
	}
	if err := sendWebPush(context.Background(), sub, []byte("{}")); !errors.Is(err, errPushGone) {
		t.Errorf("404 gave %v, want errPushGone", err)
	}
}

func TestPushSubscribeValidatesTheSubscription(t *testing.T) {
	openTestDB(t)
	resetVapidCache(t)
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	good := map[string]any{
		"endpoint": "https://web.push.apple.com/abc",
		"keys": map[string]string{
			"p256dh": b64.EncodeToString(ua.PublicKey().Bytes()),
			"auth":   b64.EncodeToString(make([]byte, 16)),
		},
		"origin": "https://lasso.example.com",
	}
	post := func(body map[string]any) int {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, "/api/push/subscribe", strings.NewReader(string(b)))
		r.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X)")
		w := httptest.NewRecorder()
		servePushSubscribe(w, r)
		return w.Code
	}
	if code := post(good); code != http.StatusOK {
		t.Fatalf("valid subscription: %d", code)
	}
	subs, _ := listPushSubscriptions()
	if len(subs) != 1 || subs[0].Label != "iPhone" || subs[0].Origin != "https://lasso.example.com" {
		t.Fatalf("stored subscription = %+v", subs)
	}
	// Re-subscribing from the same device replaces the row rather than piling up
	// duplicates that would each get their own copy of every notification.
	if code := post(good); code != http.StatusOK {
		t.Fatalf("re-subscribe: %d", code)
	}
	if n := countPushSubscriptions(); n != 1 {
		t.Errorf("after re-subscribe: %d subscriptions, want 1", n)
	}

	bad := []struct {
		name string
		mut  func(m map[string]any)
	}{
		{"http endpoint", func(m map[string]any) { m["endpoint"] = "http://push.example.com/x" }},
		{"not a url", func(m map[string]any) { m["endpoint"] = "not-a-url" }},
		{"bad p256dh", func(m map[string]any) {
			m["keys"] = map[string]string{"p256dh": "!!!", "auth": b64.EncodeToString(make([]byte, 16))}
		}},
		{"p256dh off curve", func(m map[string]any) {
			m["keys"] = map[string]string{
				"p256dh": b64.EncodeToString(append([]byte{0x04}, make([]byte, 64)...)),
				"auth":   b64.EncodeToString(make([]byte, 16)),
			}
		}},
		{"short auth secret", func(m map[string]any) {
			m["keys"] = map[string]string{
				"p256dh": b64.EncodeToString(ua.PublicKey().Bytes()),
				"auth":   b64.EncodeToString(make([]byte, 8)),
			}
		}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			m := map[string]any{}
			for k, v := range good {
				m[k] = v
			}
			c.mut(m)
			if code := post(m); code != http.StatusBadRequest {
				t.Errorf("got %d, want 400", code)
			}
		})
	}
}

func TestNotifyEnabledFollowsSubscriptions(t *testing.T) {
	openTestDB(t)
	resetVapidCache(t)
	useNotifTransport(t) // nothing registered until the test registers it
	registerNotifTransport(webPushChannel{})
	if notifyEnabled() {
		t.Error("no device is subscribed; the watcher must not poll")
	}
	pushTestSub(t, "https://web.push.apple.com/abc")
	if !notifyEnabled() {
		t.Error("a subscribed device must switch the pipeline on")
	}
}
