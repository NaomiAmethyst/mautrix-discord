// SPDX-License-Identifier: AGPL-3.0-or-later
// Discord remote authentication, based on the legacy bridge protocol implementation.
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
	"net/http"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/coder/websocket"
)

type Client struct {
	HTTP  *http.Client
	URL   string
	QR    chan string
	Done  chan struct{}
	key   *rsa.PrivateKey
	mu    sync.Mutex
	token string
	err   error
}

func New(client *http.Client) (*Client, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return &Client{HTTP: client, URL: "wss://remote-auth-gateway.discord.gg/?v=2", QR: make(chan string, 1), Done: make(chan struct{}), key: key}, nil
}
func (c *Client) Result() (string, error) { c.mu.Lock(); defer c.mu.Unlock(); return c.token, c.err }
func (c *Client) Run(ctx context.Context) {
	token, err := c.run(ctx)
	c.mu.Lock()
	c.token, c.err = token, err
	c.mu.Unlock()
	close(c.Done)
}
func (c *Client) decrypt(raw string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	return rsa.DecryptOAEP(sha256.New(), rand.Reader, c.key, data, nil)
}
func (c *Client) run(parent context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	headers := make(http.Header)
	for key, value := range discordgo.DroidWSHeaders {
		headers.Set(key, value)
	}
	dialHTTP := *c.HTTP
	dialHTTP.Timeout = 0
	conn, _, err := websocket.Dial(ctx, c.URL, &websocket.DialOptions{HTTPClient: &dialHTTP, HTTPHeader: headers})
	if err != nil {
		return "", fmt.Errorf("failed to connect to Discord QR authentication")
	}
	defer conn.CloseNow()
	write := func(op string, data map[string]any) error {
		if data == nil {
			data = map[string]any{}
		}
		data["op"] = op
		b, _ := json.Marshal(data)
		return conn.Write(ctx, websocket.MessageText, b)
	}
	var heartbeatStarted bool
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return "", fmt.Errorf("Discord QR authentication connection closed")
		}
		var packet struct {
			OP          string `json:"op"`
			Interval    int    `json:"heartbeat_interval"`
			Nonce       string `json:"encrypted_nonce"`
			Fingerprint string `json:"fingerprint"`
			Ticket      string `json:"ticket"`
		}
		if json.Unmarshal(raw, &packet) != nil {
			return "", fmt.Errorf("invalid QR authentication response")
		}
		switch packet.OP {
		case "hello":
			if heartbeatStarted || packet.Interval < 1000 {
				return "", fmt.Errorf("invalid QR authentication heartbeat")
			}
			heartbeatStarted = true
			publicKey, err := x509.MarshalPKIXPublicKey(&c.key.PublicKey)
			if err != nil {
				return "", err
			}
			if err = write("init", map[string]any{"encoded_public_key": base64.StdEncoding.EncodeToString(publicKey)}); err != nil {
				return "", err
			}
			go func(interval time.Duration) {
				ticker := time.NewTicker(interval)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if write("heartbeat", nil) != nil {
							cancel()
							return
						}
					}
				}
			}(time.Duration(packet.Interval) * time.Millisecond)
		case "nonce_proof":
			nonce, err := c.decrypt(packet.Nonce)
			if err != nil {
				return "", fmt.Errorf("invalid QR authentication nonce")
			}
			hash := sha256.Sum256(nonce)
			if err = write("nonce_proof", map[string]any{"proof": base64.RawURLEncoding.EncodeToString(hash[:])}); err != nil {
				return "", err
			}
		case "pending_remote_init":
			select {
			case c.QR <- "https://discord.com/ra/" + packet.Fingerprint:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		case "pending_login":
			session, err := discordgo.New("")
			if err != nil {
				return "", err
			}
			session.Client = c.HTTP
			encrypted, err := session.RemoteAuthLogin(packet.Ticket)
			if err != nil {
				return "", fmt.Errorf("Discord rejected QR authentication; CAPTCHA may require token login")
			}
			token, err := c.decrypt(encrypted)
			if err != nil {
				return "", fmt.Errorf("invalid QR authentication token")
			}
			return string(token), nil
		case "cancel":
			return "", fmt.Errorf("Discord QR authentication cancelled")
		case "pending_ticket", "heartbeat_ack":
		default:
			return "", fmt.Errorf("unexpected QR authentication response")
		}
	}
}
