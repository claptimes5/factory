// Package push sends Web Push notifications to browsers that subscribed,
// such as the dashboard installed on a phone's home screen. It encrypts
// each message for the browser (RFC 8291, aes128gcm) and signs the
// request with the factory's VAPID key (RFC 8292), with the standard
// library alone.
package push

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
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Subscription is what a browser's PushManager.subscribe gives: where to
// send, and the keys to encrypt for it (base64url, as browsers give them).
type Subscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// Message is what the service worker shows.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// URL opens when the notification is tapped, such as /app/.
	URL string `json:"url"`
	// Tag replaces an earlier notification with the same tag.
	Tag string `json:"tag,omitempty"`
}

// ErrGone is a subscription the push service no longer has: forget it.
var ErrGone = errors.New("push: the subscription is gone")

// Sender signs and sends pushes.
type Sender struct {
	Key *ecdsa.PrivateKey
	// Subject is who runs the sender, a mailto: or https: URL push
	// services may contact.
	Subject string
	Client  *http.Client
	now     func() time.Time
}

// NewKey makes a VAPID key.
func NewKey() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) }

// EncodeKey and DecodeKey keep a key as text (base64 of its DER form).
func EncodeKey(k *ecdsa.PrivateKey) (string, error) {
	b, err := x509.MarshalECPrivateKey(k)
	return base64.StdEncoding.EncodeToString(b), err
}

func DecodeKey(s string) (*ecdsa.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return x509.ParseECPrivateKey(b)
}

// PublicKey is the key a browser subscribes with (applicationServerKey):
// the uncompressed point, base64url.
func (s *Sender) PublicKey() string {
	pub, err := s.Key.PublicKey.ECDH()
	if err != nil {
		return ""
	}
	return b64(pub.Bytes())
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// unb64 reads base64url with or without padding, as browsers vary.
func unb64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// Send pushes m to sub. Urgent ones ask the push service to wake the
// device at once. ErrGone means the browser unsubscribed.
func (s *Sender) Send(ctx context.Context, sub Subscription, m Message, urgent bool) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	body, err := encrypt(sub, payload)
	if err != nil {
		return err
	}
	auth, err := s.vapid(sub.Endpoint)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Authorization", auth)
	if urgent {
		req.Header.Set("Urgency", "high")
	} else {
		req.Header.Set("Urgency", "normal")
	}
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode/100 != 2:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("push: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// vapid is the Authorization header: a JWT for the push service's origin,
// signed with the key, and the key itself.
func (s *Sender) vapid(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("push: %q isn't an https endpoint", endpoint)
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	head := b64([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": now().Add(12 * time.Hour).Unix(), "sub": s.Subject})
	if err != nil {
		return "", err
	}
	unsigned := head + "." + b64(claims)
	sum := sha256.Sum256([]byte(unsigned))
	r, sv, err := ecdsa.Sign(rand.Reader, s.Key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	return "vapid t=" + unsigned + "." + b64(sig) + ", k=" + s.PublicKey(), nil
}

// recordSize is the one record's size the body declares.
const recordSize = 4096

// encrypt is RFC 8291's aes128gcm for one record: a header with a random
// salt and a new key pair's public key, then the payload sealed with a
// key both sides derive from that pair and the browser's keys.
func encrypt(sub Subscription, payload []byte) ([]byte, error) {
	uaPub, err := unb64(sub.P256dh)
	if err != nil {
		return nil, fmt.Errorf("push: p256dh: %w", err)
	}
	secret, err := unb64(sub.Auth)
	if err != nil {
		return nil, fmt.Errorf("push: auth: %w", err)
	}
	ua, err := ecdh.P256().NewPublicKey(uaPub)
	if err != nil {
		return nil, fmt.Errorf("push: p256dh: %w", err)
	}
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	cek, nonce, err := keys(as, ua, as.PublicKey().Bytes(), uaPub, secret, salt)
	if err != nil {
		return nil, err
	}
	if len(payload)+1+16 > recordSize-86 {
		return nil, fmt.Errorf("push: a %d-byte message is too long for one record", len(payload))
	}
	gcm, err := newGCM(cek)
	if err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, append(payload, 2), nil) // 2: the last record, no padding
	head := make([]byte, 0, 16+4+1+65)
	head = append(head, salt...)
	head = binary.BigEndian.AppendUint32(head, recordSize)
	asPub := as.PublicKey().Bytes()
	head = append(head, byte(len(asPub)))
	head = append(head, asPub...)
	return append(head, sealed...), nil
}

// keys derives the content key and nonce from the shared secret of one
// side's private key and the other's public key.
func keys(priv *ecdh.PrivateKey, pub *ecdh.PublicKey, asPub, uaPub, secret, salt []byte) ([]byte, []byte, error) {
	shared, err := priv.ECDH(pub)
	if err != nil {
		return nil, nil, err
	}
	prkKey, err := hkdf.Extract(sha256.New, shared, secret)
	if err != nil {
		return nil, nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(uaPub)+string(asPub), 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	return cek, nonce, err
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
