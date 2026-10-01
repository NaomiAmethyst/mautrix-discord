// SPDX-License-Identifier: AGPL-3.0-or-later
package remoteauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRemoteAuthRoundTrip(t *testing.T) {
	encryptedToken := make(chan string, 1)
	serverErrors := make(chan error, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"discord.com", "discordapp.com"}})
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		write := func(value any) error {
			raw, _ := json.Marshal(value)
			return conn.Write(ctx, websocket.MessageText, raw)
		}
		if err = write(map[string]any{"op": "hello", "heartbeat_interval": 1000}); err != nil {
			serverErrors <- err
			return
		}
		_, raw, err := conn.Read(ctx)
		if err != nil {
			serverErrors <- err
			return
		}
		var init struct {
			OP  string `json:"op"`
			Key string `json:"encoded_public_key"`
		}
		if json.Unmarshal(raw, &init) != nil || init.OP != "init" {
			serverErrors <- fmt.Errorf("missing initialization")
			return
		}
		keyData, err := base64.StdEncoding.DecodeString(init.Key)
		if err != nil {
			serverErrors <- err
			return
		}
		pub, err := x509.ParsePKIXPublicKey(keyData)
		if err != nil {
			serverErrors <- err
			return
		}
		proofNonce := []byte("test challenge")
		ciphertext, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub.(*rsa.PublicKey), proofNonce, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		if err = write(map[string]any{"op": "nonce_proof", "encrypted_nonce": base64.StdEncoding.EncodeToString(ciphertext)}); err != nil {
			serverErrors <- err
			return
		}
		_, raw, err = conn.Read(ctx)
		if err != nil {
			serverErrors <- err
			return
		}
		var proof struct {
			OP    string `json:"op"`
			Proof string `json:"proof"`
		}
		_ = json.Unmarshal(raw, &proof)
		expected := sha256.Sum256(proofNonce)
		if proof.OP != "nonce_proof" || proof.Proof != base64.RawURLEncoding.EncodeToString(expected[:]) {
			serverErrors <- fmt.Errorf("invalid nonce proof")
			return
		}
		if err = write(map[string]any{"op": "pending_remote_init", "fingerprint": "test-fingerprint"}); err != nil {
			serverErrors <- err
			return
		}
		ciphertext, err = rsa.EncryptOAEP(sha256.New(), rand.Reader, pub.(*rsa.PublicKey), []byte("test-token"), nil)
		if err != nil {
			serverErrors <- err
			return
		}
		encryptedToken <- base64.StdEncoding.EncodeToString(ciphertext)
		if err = write(map[string]any{"op": "pending_login", "ticket": "test-ticket"}); err != nil {
			serverErrors <- err
			return
		}
		_, _, _ = conn.Read(ctx)
	}))
	defer srv.Close()
	httpClient := srv.Client()
	original := httpClient.Transport
	httpClient.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.String(), srv.URL) {
			return original.RoundTrip(r)
		}
		if r.URL.Host != "discord.com" || !strings.Contains(r.URL.Path, "remote-auth/login") {
			return nil, fmt.Errorf("unexpected request: %s", r.URL)
		}
		raw, _ := json.Marshal(map[string]string{"encrypted_token": <-encryptedToken})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})
	client, err := New(httpClient)
	if err != nil {
		t.Fatal(err)
	}
	client.URL = "wss" + strings.TrimPrefix(srv.URL, "https")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go client.Run(ctx)
	select {
	case qr := <-client.QR:
		if qr != "https://discord.com/ra/test-fingerprint" {
			t.Fatal("incorrect QR link")
		}
	case err := <-serverErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("QR timed out")
	}
	select {
	case <-client.Done:
	case err := <-serverErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("authentication timed out")
	}
	token, err := client.Result()
	if err != nil || token != "test-token" {
		t.Fatalf("authentication failed: %s %v", token, err)
	}
}
func TestCancelledAuthFinishes(t *testing.T) {
	client, err := New(&http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	go client.Run(ctx)
	select {
	case <-client.Done:
	case <-time.After(time.Second):
		t.Fatal("cancelled login did not finish")
	}
	if _, err = client.Result(); err == nil {
		t.Fatal("cancelled login succeeded")
	}
}
