package web

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/claptimes5/factory/internal/push"
)

// A browser turns push on, gets a test, and turns it off; the endpoint
// never comes back out.
func TestPushDevices(t *testing.T) {
	t.Parallel()
	_, _, _, s := apiSetup(t)
	var got atomic.Int32
	pushSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer pushSrv.Close()
	key, _ := push.NewKey()
	s.Push = &push.Sender{Key: key, Subject: "mailto:f@example.com", Client: pushSrv.Client()}
	h, _ := s.Handler()
	srv := httpServer(t, h)

	var p APIPush
	getJSON(t, srv+"/api/v1/push", &p)
	if !p.Available || p.PublicKey == "" || len(p.Devices) != 0 {
		t.Fatalf("before: %+v", p)
	}
	if code, _ := postJSON(t, srv+"/api/v1/push/test", "", "{}"); code != 400 {
		t.Fatalf("a test with no device: %d", code)
	}
	browser, _ := ecdh.P256().GenerateKey(rand.Reader)
	secret := make([]byte, 16)
	rand.Read(secret)
	enc := base64.RawURLEncoding.EncodeToString
	endpoint := pushSrv.URL + "/send/abc"
	sub := `{"endpoint":"` + endpoint + `","keys":{"p256dh":"` + enc(browser.PublicKey().Bytes()) + `","auth":"` + enc(secret) + `"},"agent":"iPhone"}`
	if code, body := postJSON(t, srv+"/api/v1/push/subscribe", "", sub); code != 200 {
		t.Fatalf("subscribe: %d %s", code, body)
	}
	if code, body := postJSON(t, srv+"/api/v1/push/subscribe", "", `{"endpoint":"http://insecure/x","keys":{"p256dh":"a","auth":"b"}}`); code != 400 {
		t.Fatalf("an http endpoint: %d %s", code, body)
	}
	body := get(t, srv+"/api/v1/push")
	if strings.Contains(body, endpoint) || !strings.Contains(body, PushID(endpoint)) || !strings.Contains(body, "iPhone") {
		t.Fatalf("devices: %s", body)
	}
	if code, body := postJSON(t, srv+"/api/v1/push/test", "", "{}"); code != 200 || got.Load() != 1 {
		t.Fatalf("test: %d %s, %d pushes", code, body, got.Load())
	}
	postJSON(t, srv+"/api/v1/push/unsubscribe", "", `{"id":"`+PushID(endpoint)+`"}`)
	getJSON(t, srv+"/api/v1/push", &p)
	if len(p.Devices) != 0 {
		t.Fatalf("after unsubscribe: %+v", p.Devices)
	}
}
