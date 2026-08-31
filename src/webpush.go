package main

// Web Push — the notification transport that reaches a phone.
//
// This is the whole point of the feature: lasso is served over a public
// hostname behind Cloudflare Access, added to an iOS home screen, and iOS 16.4+
// delivers Web Push to a home-screen web app even when it is closed. Nothing
// else lasso can do reaches a locked phone: an SSE toast needs an open tab, and
// a notification the browser raises needs the page in the foreground.
//
// Four RFCs, no dependencies — the standard library covers all of it since Go
// 1.24 (crypto/ecdh, crypto/hkdf, crypto/ecdsa's raw key encoding):
//
//	RFC 8030  the push protocol (POST to the endpoint, TTL/Urgency headers,
//	          404/410 = the subscription is dead and must be dropped)
//	RFC 8188  the aes128gcm content coding (salt || rs || idlen || keyid || …)
//	RFC 8291  how the content-encryption key is derived from the subscription's
//	          p256dh public key and auth secret (sealWebPush)
//	RFC 8292  VAPID: an ES256 JWT identifying this server (vapidAuthHeader)
//
// The encryption is end-to-end: the push service (Apple's, Google's, Mozilla's)
// relays a blob it cannot read. It sees only that this lasso talks to that
// device, and how often.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// pushSchema holds the registered devices. Appended to the schema exec in
// openDB alongside the other feature-owned schemas (oauthSchema, groupsSchema).
//
// The endpoint is the primary key because it IS the identity of a subscription:
// the browser mints a new one when its keys rotate or permission is
// re-granted, so a re-subscribe from the same phone is a new row and the old
// one is collected the next time the push service answers 410 for it.
const pushSchema = `
CREATE TABLE IF NOT EXISTS push_subscriptions (
  endpoint   TEXT PRIMARY KEY,
  p256dh     TEXT NOT NULL,
  auth       TEXT NOT NULL,
  -- The page origin the subscription was created from. Carried because it is
  -- what the VAPID "sub" claim is derived from (see vapidSubject).
  origin     TEXT NOT NULL DEFAULT '',
  label      TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '',
  last_ok    TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT ''
);
`

// pushSubscription is one registered device.
type pushSubscription struct {
	Endpoint  string `json:"endpoint"`
	P256dh    string `json:"p256dh"`
	Auth      string `json:"auth"`
	Origin    string `json:"origin,omitempty"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	LastOK    string `json:"last_ok,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// b64 is base64url without padding — the encoding every Web Push field uses
// (VAPID keys, the JWT, the subscription's p256dh/auth as the Push API hands
// them to the browser).
var b64 = base64.RawURLEncoding

// b64decode accepts both the unpadded form the Push API produces and the padded
// form, since a client that round-trips a subscription through its own JSON may
// hand back either.
func b64decode(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	return b64.DecodeString(s)
}

// ---------------------------------------------------------------------------
// db
// ---------------------------------------------------------------------------

func upsertPushSubscription(s pushSubscription) error {
	if db == nil {
		return errors.New("no db")
	}
	_, err := db.Exec(`INSERT INTO push_subscriptions (endpoint, p256dh, auth, origin, label, created_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(endpoint) DO UPDATE SET
			p256dh=excluded.p256dh, auth=excluded.auth, origin=excluded.origin,
			label=excluded.label, last_error=''`,
		s.Endpoint, s.P256dh, s.Auth, s.Origin, s.Label, time.Now().UTC().Format(time.RFC3339))
	return err
}

func listPushSubscriptions() ([]pushSubscription, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.Query(`SELECT endpoint, p256dh, auth, origin, label, created_at, last_ok, last_error
		FROM push_subscriptions ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pushSubscription
	for rows.Next() {
		var s pushSubscription
		if err := rows.Scan(&s.Endpoint, &s.P256dh, &s.Auth, &s.Origin, &s.Label,
			&s.CreatedAt, &s.LastOK, &s.LastError); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func deletePushSubscription(endpoint string) error {
	if db == nil {
		return errors.New("no db")
	}
	_, err := db.Exec(`DELETE FROM push_subscriptions WHERE endpoint=?`, endpoint)
	return err
}

// countPushSubscriptions is the transport's active() check, so it runs on the
// watcher's poll interval. A nil db (CLI subcommands, tests) reads as zero,
// which turns the whole pipeline off rather than erroring.
func countPushSubscriptions() int {
	if db == nil {
		return 0
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM push_subscriptions`).Scan(&n); err != nil {
		return 0
	}
	return n
}

func markPushOK(endpoint string) {
	if db == nil {
		return
	}
	_, _ = db.Exec(`UPDATE push_subscriptions SET last_ok=?, last_error='' WHERE endpoint=?`,
		time.Now().UTC().Format(time.RFC3339), endpoint)
}

// markPushError records why a device last failed. Kept on the row (rather than
// only logged) because the Settings tab shows it: a subscription that has been
// failing for a week looks identical to a healthy one otherwise.
func markPushError(endpoint, detail string) {
	if db == nil {
		return
	}
	if len(detail) > 200 {
		detail = detail[:200]
	}
	_, _ = db.Exec(`UPDATE push_subscriptions SET last_error=? WHERE endpoint=?`, detail, endpoint)
}

// ---------------------------------------------------------------------------
// VAPID (RFC 8292)
// ---------------------------------------------------------------------------

// vapidKeySetting is the settings key holding this server's VAPID private key,
// base64url-encoded raw P-256 scalar.
//
// It is generated once, on the first request that needs it, and must then never
// change: a browser pins the public key into every subscription it creates
// (applicationServerKey), so a new key silently invalidates every registered
// device until each re-subscribes. It lives in lasso.db in the clear because it
// has to be usable unattended — same trust model as the OAuth client secret
// this db already holds.
const vapidKeySetting = "push_vapid_private"

// vapidTTL is how long a signed JWT stays valid. Push services cap this at 24h
// and reject anything longer; 12h is the conventional choice, and the token is
// minted per delivery anyway.
const vapidTTL = 12 * time.Hour

var vapidCache struct {
	sync.Mutex
	key *ecdsa.PrivateKey
	pub string // base64url uncompressed point — the browser's applicationServerKey
}

// vapidKey returns this server's VAPID keypair, generating and persisting it on
// first use.
func vapidKey() (*ecdsa.PrivateKey, string, error) {
	vapidCache.Lock()
	defer vapidCache.Unlock()
	if vapidCache.key != nil {
		return vapidCache.key, vapidCache.pub, nil
	}
	stored, err := getSetting(vapidKeySetting)
	if err != nil {
		return nil, "", err
	}
	var key *ecdsa.PrivateKey
	if stored != "" {
		raw, err := b64decode(stored)
		if err != nil {
			return nil, "", fmt.Errorf("stored VAPID key is not base64url: %w", err)
		}
		if key, err = ecdsa.ParseRawPrivateKey(elliptic.P256(), raw); err != nil {
			return nil, "", fmt.Errorf("stored VAPID key: %w", err)
		}
	} else {
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return nil, "", err
		}
		raw, err := key.Bytes()
		if err != nil {
			return nil, "", err
		}
		if err := setSetting(vapidKeySetting, b64.EncodeToString(raw)); err != nil {
			return nil, "", fmt.Errorf("persist VAPID key: %w", err)
		}
		log.Printf("notify: generated this server's VAPID keypair (push_vapid_private in lasso.db)")
	}
	pub, err := key.PublicKey.Bytes()
	if err != nil {
		return nil, "", err
	}
	vapidCache.key, vapidCache.pub = key, b64.EncodeToString(pub)
	return vapidCache.key, vapidCache.pub, nil
}

// vapidSubject is the JWT's "sub" claim: who to contact about this server's
// push traffic.
//
// Apple validates the format and answers 403 BadJwtToken for anything that is
// not a mailto: address or a full https URL — notably rejecting the
// "mailto:…@localhost" a self-hosted server would otherwise invent. The
// subscription's own page origin is both valid and truthful (it is literally
// the deployment sending the push), so that is the default. LASSO_PUSH_CONTACT
// overrides it for anyone who wants a real address in front of their push
// provider. The mailto fallback is only reachable for a non-https origin, i.e.
// a loopback dev subscription, which is never Apple's service.
func vapidSubject(origin string) string {
	if v := strings.TrimSpace(os.Getenv("LASSO_PUSH_CONTACT")); v != "" {
		return v
	}
	if o := strings.TrimSuffix(strings.TrimSpace(origin), "/"); strings.HasPrefix(o, "https://") {
		return o
	}
	return "mailto:lasso@" + localHostname()
}

// vapidAuthHeader builds the Authorization header for one delivery: an ES256
// JWT whose audience is the push service's origin, plus the public key the
// service checks it against.
func vapidAuthHeader(endpoint, subject string, key *ecdsa.PrivateKey, pub string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("bad push endpoint %q", endpoint)
	}
	claims, err := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(vapidTTL).Unix(),
		"sub": subject,
	})
	if err != nil {
		return "", err
	}
	// The header is fixed, so it is a constant rather than a marshalled map:
	// {"typ":"JWT","alg":"ES256"}.
	signing := "eyJ0eXAiOiJKV1QiLCJhbGciOiJFUzI1NiJ9." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	// JWS wants the fixed-width r||s pair, not ASN.1 — hence Sign rather than
	// SignASN1, and FillBytes rather than Bytes (which would drop leading zeros
	// and shorten the signature).
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + b64.EncodeToString(sig) + ", k=" + pub, nil
}

// ---------------------------------------------------------------------------
// message encryption (RFC 8291 over RFC 8188)
// ---------------------------------------------------------------------------

// webPushRecordSize is the "rs" written into the content-coding header. A push
// message is a single record (RFC 8291 §4) and rs must exceed the record's
// length, so the maximum a push service must accept doubles as the value.
const webPushRecordSize = 4096

// maxWebPushPlaintext is what fits in one 4096-octet body: the 86-octet header,
// the padding delimiter, and the AEAD tag come out of the same budget
// (RFC 8291 §4).
const maxWebPushPlaintext = 3993

// sealWebPush encrypts plaintext for one subscription, returning the request
// body for the push endpoint.
//
// salt and the application-server (ephemeral) key are parameters rather than
// generated inside, so the RFC 8291 §5 test vector can be reproduced exactly.
// sealWebPushRandom is what callers use.
func sealWebPush(uaPublic, authSecret, plaintext, salt []byte, as *ecdh.PrivateKey) ([]byte, error) {
	if len(plaintext) > maxWebPushPlaintext {
		return nil, fmt.Errorf("push payload too large: %d > %d bytes", len(plaintext), maxWebPushPlaintext)
	}
	if len(salt) != 16 {
		return nil, fmt.Errorf("push salt must be 16 bytes, got %d", len(salt))
	}
	if len(authSecret) != 16 {
		return nil, fmt.Errorf("subscription auth secret must be 16 bytes, got %d", len(authSecret))
	}
	// NewPublicKey rejects a point that is not on P-256, which is the validation
	// RFC 8291 §7 requires before using a subscription's key.
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("subscription public key: %w", err)
	}
	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, fmt.Errorf("ecdh: %w", err)
	}
	uaBytes, asBytes := ua.Bytes(), as.PublicKey().Bytes()

	// RFC 8291 §3.3: fold the auth secret into the ECDH secret. The auth secret
	// is the HKDF salt here, and both public keys ride in the info string.
	keyInfo := make([]byte, 0, len("WebPush: info")+1+len(uaBytes)+len(asBytes))
	keyInfo = append(keyInfo, "WebPush: info"...)
	keyInfo = append(keyInfo, 0)
	keyInfo = append(keyInfo, uaBytes...)
	keyInfo = append(keyInfo, asBytes...)
	ikm, err := hkdf.Key(sha256.New, shared, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	// RFC 8188 §2.2: the content encryption key and nonce come from the record's
	// own salt. No sequence-number XOR — a push message is one record, number 0.
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// 0x02 is the last-record padding delimiter; a receiver MUST discard any
	// other value (RFC 8188 §2, RFC 8291 §4).
	record := make([]byte, 0, len(plaintext)+1)
	record = append(record, plaintext...)
	record = append(record, 0x02)

	body := make([]byte, 0, 16+4+1+len(asBytes)+len(record)+gcm.Overhead())
	body = append(body, salt...)
	body = binary.BigEndian.AppendUint32(body, webPushRecordSize)
	body = append(body, byte(len(asBytes))) // idlen; the keyid is the AS public key
	body = append(body, asBytes...)
	return gcm.Seal(body, nonce, record, nil), nil
}

// sealWebPushRandom is sealWebPush with a fresh salt and a fresh ephemeral key,
// as every real delivery must use.
func sealWebPushRandom(uaPublic, authSecret, plaintext []byte) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return sealWebPush(uaPublic, authSecret, plaintext, salt, as)
}

// ---------------------------------------------------------------------------
// delivery (RFC 8030)
// ---------------------------------------------------------------------------

// errPushGone marks the one failure that means "forget this device": the push
// service says the subscription no longer exists (404/410 — the app was
// deleted, permission revoked, or the browser rotated its keys). Every other
// failure is transient and the row stays.
var errPushGone = errors.New("push subscription is gone")

// pushTTL is how long the push service may hold an undelivered message. A
// blocked agent that surfaces on a phone half a day later is noise, not news;
// half an hour covers a tunnel blip or a phone in a lift.
const pushTTL = 30 * 60

// pushHTTP is deliberately its own client with a short timeout: a push service
// that stalls must not hold a delivery goroutine for minutes.
var pushHTTP = &http.Client{Timeout: 15 * time.Second}

// sendWebPush POSTs one encrypted message to one subscription.
func sendWebPush(ctx context.Context, s pushSubscription, payload []byte) error {
	uaPublic, err := b64decode(s.P256dh)
	if err != nil {
		return fmt.Errorf("p256dh: %w", err)
	}
	authSecret, err := b64decode(s.Auth)
	if err != nil {
		return fmt.Errorf("auth secret: %w", err)
	}
	key, pub, err := vapidKey()
	if err != nil {
		return err
	}
	body, err := sealWebPushRandom(uaPublic, authSecret, payload)
	if err != nil {
		return err
	}
	auth, err := vapidAuthHeader(s.Endpoint, vapidSubject(s.Origin), key, pub, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", fmt.Sprint(pushTTL))
	// A blocked agent is exactly the case Urgency: high exists for — it must
	// wake a sleeping device, since nothing moves until the human answers.
	req.Header.Set("Urgency", "high")
	resp, err := pushHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Bodies are short diagnostics ("BadJwtToken", "VapidPkHashMismatch"); read
	// a bounded amount so a hostile endpoint can't stream forever.
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return errPushGone
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	}
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(detail)))
}

// webPushPayload is the JSON the service worker receives (web/public/sw.js).
// Everything the notification needs must be in here: the worker runs with no
// page and cannot call back into lasso — behind Cloudflare Access it has no
// credentials of its own — so a payload that only carried an id would be a
// notification the phone cannot render.
type webPushPayload struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag,omitempty"`
	Host  string `json:"host,omitempty"`
}

// webPushChannel is the notifTransport (notify.go) for Web Push.
type webPushChannel struct{}

func (webPushChannel) name() string { return "web-push" }

func (webPushChannel) active() bool { return countPushSubscriptions() > 0 }

func (webPushChannel) deliver(ctx context.Context, n notification) error {
	subs, err := listPushSubscriptions()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(webPushPayload{
		Kind:  string(n.Kind),
		Title: n.Title,
		Body:  n.Body,
		Tag:   n.Tag,
		Host:  n.Host,
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range subs {
		switch err := sendWebPush(ctx, s, payload); {
		case errors.Is(err, errPushGone):
			log.Printf("notify: web-push: dropping dead subscription %s", pushDeviceName(s))
			_ = deletePushSubscription(s.Endpoint)
		case err != nil:
			markPushError(s.Endpoint, err.Error())
			errs = append(errs, fmt.Errorf("%s: %w", pushDeviceName(s), err))
		default:
			markPushOK(s.Endpoint)
		}
	}
	return errors.Join(errs...)
}

// pushDeviceName is a subscription's short identity for logs: its label plus a
// fingerprint of the endpoint, never the endpoint itself (it is a bearer
// capability to push to that device).
func pushDeviceName(s pushSubscription) string {
	sum := sha256.Sum256([]byte(s.Endpoint))
	id := b64.EncodeToString(sum[:4])
	if s.Label == "" {
		return id
	}
	return s.Label + " (" + id + ")"
}

// deviceLabel names a device from its User-Agent for the Settings list. A
// coarse guess on purpose: it exists so a user with three phones can tell which
// row to remove, not to fingerprint anything.
func deviceLabel(ua string) string {
	switch {
	case ua == "":
		return "device"
	case strings.Contains(ua, "iPhone"):
		return "iPhone"
	case strings.Contains(ua, "iPad"):
		return "iPad"
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "Macintosh"):
		return "Mac"
	case strings.Contains(ua, "Windows"):
		return "Windows"
	case strings.Contains(ua, "Linux"):
		return "Linux"
	}
	return "device"
}

// ---------------------------------------------------------------------------
// HTTP: /api/push, /api/push/subscribe, /api/push/unsubscribe, /api/push/test
// ---------------------------------------------------------------------------

// pushConfig is GET /api/push — what the Settings tab needs to render the
// section: the key to subscribe with, and which devices are already registered.
// Endpoints are deliberately absent (see pushDeviceName).
type pushConfig struct {
	PublicKey string          `json:"public_key"`
	Devices   []pushDeviceRow `json:"devices"`
}

type pushDeviceRow struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	CreatedAt string `json:"created_at,omitempty"`
	LastOK    string `json:"last_ok,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

func servePushConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_, pub, err := vapidKey()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	subs, err := listPushSubscriptions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cfg := pushConfig{PublicKey: pub, Devices: []pushDeviceRow{}}
	for _, s := range subs {
		sum := sha256.Sum256([]byte(s.Endpoint))
		cfg.Devices = append(cfg.Devices, pushDeviceRow{
			ID:        b64.EncodeToString(sum[:4]),
			Label:     s.Label,
			CreatedAt: s.CreatedAt,
			LastOK:    s.LastOK,
			LastError: s.LastError,
		})
	}
	writeJSON(w, cfg)
}

// pushSubscribeReq is the browser's PushSubscription, plus the page origin (for
// the VAPID subject — see vapidSubject). The browser's own JSON shape is
// {endpoint, keys:{p256dh, auth}}, so it is accepted verbatim.
type pushSubscribeReq struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	Origin string `json:"origin"`
}

func servePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req pushSubscribeReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// https-only: the endpoint is a URL lasso will POST to unattended, and every
	// real push service is https. It also keeps a typo from turning lasso into a
	// request forwarder for some other scheme.
	u, err := url.Parse(strings.TrimSpace(req.Endpoint))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		http.Error(w, "endpoint must be an https URL", http.StatusBadRequest)
		return
	}
	uaPublic, err := b64decode(req.Keys.P256dh)
	if err != nil {
		http.Error(w, "keys.p256dh is not base64url", http.StatusBadRequest)
		return
	}
	if _, err := ecdh.P256().NewPublicKey(uaPublic); err != nil {
		http.Error(w, "keys.p256dh is not a P-256 public key", http.StatusBadRequest)
		return
	}
	authSecret, err := b64decode(req.Keys.Auth)
	if err != nil || len(authSecret) != 16 {
		http.Error(w, "keys.auth must be 16 base64url-encoded bytes", http.StatusBadRequest)
		return
	}
	origin := strings.TrimSpace(req.Origin)
	if origin == "" {
		origin = r.Header.Get("Origin")
	}
	sub := pushSubscription{
		Endpoint: u.String(),
		P256dh:   req.Keys.P256dh,
		Auth:     req.Keys.Auth,
		Origin:   origin,
		Label:    deviceLabel(r.UserAgent()),
	}
	if err := upsertPushSubscription(sub); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("notify: web-push: registered %s", pushDeviceName(sub))
	writeJSON(w, map[string]any{"ok": true, "devices": countPushSubscriptions()})
}

func servePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Endpoint) == "" {
		http.Error(w, "endpoint is required", http.StatusBadRequest)
		return
	}
	if err := deletePushSubscription(strings.TrimSpace(req.Endpoint)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "devices": countPushSubscriptions()})
}

// servePushTest sends one notification through the real pipeline — same
// transport, same encryption, same service worker — so "it says it's on but
// nothing arrives" is one button away from a concrete error. It answers with
// the per-device result rather than a bare 200, since a failure here is the
// whole point of pressing it.
func servePushTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	subs, err := listPushSubscriptions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(subs) == 0 {
		http.Error(w, "no device is subscribed", http.StatusBadRequest)
		return
	}
	n := notification{
		Kind:  notifTest,
		Title: "lasso notifications are on",
		Body:  "You'll get one when an agent blocks, and when an agent pings you.",
		Tag:   "lasso-test",
		Host:  requestHost(r),
	}
	// Through the abstraction, not straight at this file's transport: the button
	// is meant to exercise exactly what a real notification takes.
	res := notifyNow(r.Context(), n)
	if !res.Sent {
		writeJSON(w, map[string]any{"ok": false, "error": res.Detail})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "devices": len(subs), "warning": res.Detail})
}
