package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// browser plays a subscribed browser: it has the keys, and decrypts
// what a push service hands it as RFC 8291 says.
type browser struct {
	priv   *ecdh.PrivateKey
	secret []byte
}

func newBrowser(t *testing.T) *browser {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	return &browser{priv, secret}
}

func (b *browser) sub(endpoint string) Subscription {
	return Subscription{Endpoint: endpoint, P256dh: b64(b.priv.PublicKey().Bytes()), Auth: b64(b.secret)}
}

func (b *browser) decrypt(t *testing.T, body []byte) []byte {
	t.Helper()
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if rs != recordSize || idlen != 65 {
		t.Fatalf("header: rs %d idlen %d", rs, idlen)
	}
	asPub := body[21 : 21+idlen]
	as, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	cek, nonce, err := keys(b.priv, as, asPub, b.priv.PublicKey().Bytes(), b.secret, salt)
	if err != nil {
		t.Fatal(err)
	}
	gcm, _ := newGCM(cek)
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if plain[len(plain)-1] != 2 {
		t.Fatalf("not the last record: %x", plain[len(plain)-1])
	}
	return plain[:len(plain)-1]
}

func TestSendEncryptsAndSigns(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t)
	var got *http.Request
	var body []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, body = r, must(io.ReadAll(r.Body))
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	s := &Sender{Key: key, Subject: "mailto:factory@example.com", Client: srv.Client(), now: func() time.Time { return time.Unix(1_800_000_000, 0) }}
	m := Message{Title: "Factory", Body: "Your OK on the Download CSV button", URL: "/app/"}
	if err := s.Send(context.Background(), b.sub(srv.URL+"/push/1"), m, true); err != nil {
		t.Fatal(err)
	}
	var back Message
	if err := json.Unmarshal(b.decrypt(t, body), &back); err != nil || back != m {
		t.Fatalf("the browser read %+v %v", back, err)
	}
	if got.Header.Get("Content-Encoding") != "aes128gcm" || got.Header.Get("Urgency") != "high" || got.Header.Get("TTL") == "" {
		t.Fatalf("headers %v", got.Header)
	}
	// The VAPID JWT: for the push service's origin, signed by the key it names.
	auth := got.Header.Get("Authorization")
	tok, k, ok := strings.Cut(strings.TrimPrefix(auth, "vapid t="), ", k=")
	if !ok || k != s.PublicKey() {
		t.Fatalf("authorization %q", auth)
	}
	parts := strings.Split(tok, ".")
	claims := map[string]any{}
	json.Unmarshal(must(unb64(parts[1])), &claims)
	if claims["aud"] != srv.URL || claims["sub"] != "mailto:factory@example.com" || claims["exp"].(float64) != float64(1_800_000_000+12*3600) {
		t.Fatalf("claims %v", claims)
	}
	sig := must(unb64(parts[2]))
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&key.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("the JWT's signature doesn't verify")
	}

	if err := s.Send(context.Background(), b.sub(srv.URL+"/gone"), m, false); err != ErrGone {
		t.Fatalf("a gone subscription: %v", err)
	}
	if err := s.Send(context.Background(), b.sub("http://example.com/push"), m, false); err == nil {
		t.Fatal("an http endpoint was taken")
	}
	enc, _ := EncodeKey(key)
	if again, err := DecodeKey(enc); err != nil || !again.Equal(key) {
		t.Fatalf("the key doesn't survive being kept: %v", err)
	}
	if _, err := encrypt(b.sub(srv.URL), bytes.Repeat([]byte("x"), 5000)); err == nil {
		t.Fatal("a message too long for one record was taken")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// RFC 8291's example (appendix A): the same keys, salt and message give
// the same content key, nonce and ciphertext.
func TestRFC8291Example(t *testing.T) {
	d := func(s string) []byte { return must(unb64(s)) }
	as := must(ecdh.P256().NewPrivateKey(d("yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw")))
	uaPub := d("BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	ua := must(ecdh.P256().NewPublicKey(uaPub))
	if b64(as.PublicKey().Bytes()) != "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8" {
		t.Fatal("as_public")
	}
	cek, nonce, err := keys(as, ua, as.PublicKey().Bytes(), uaPub, d("BTBZMqHH6r4Tts7J_aSIgg"), d("DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	if b64(cek) != "oIhVW04MRdy2XN9CiKLxTg" || b64(nonce) != "4h_95klXJ5E_qnoN" {
		t.Fatalf("cek %s nonce %s", b64(cek), b64(nonce))
	}
	gcm := must(newGCM(cek))
	sealed := gcm.Seal(nil, nonce, append([]byte("When I grow up, I want to be a watermelon"), 2), nil)
	if b64(sealed) != "8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ" {
		t.Fatalf("ciphertext %s", b64(sealed))
	}
}
