// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"os"
	"path/filepath"
	"testing"

	up "go.mau.fi/util/configupgrade"
	"go.mau.fi/util/ptr"
	"gopkg.in/yaml.v3"
)

func defaultConfig(t *testing.T) Config {
	t.Helper()
	var config Config
	if err := yaml.Unmarshal([]byte(ExampleConfig), &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestChannelPolicyAndOverrides(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", RelayLoginID: "456", Sync: SyncConfig{Attachments: ptr.Ptr(false), Typing: ptr.Ptr(true)}}}
	client := testClient(t, config, nil)
	if !client.allowed("123") || client.allowed("999") {
		t.Fatal("allowlist was not enforced")
	}
	client.UserLogin.ID = "789"
	if client.allowed("123") {
		t.Fatal("channel was accessible to the wrong login")
	}
	sync := config.syncFor("123")
	if enabled(sync.Attachments) || !enabled(sync.Messages) || !enabled(sync.Typing) {
		t.Fatal("per-channel false and inherited defaults were not applied")
	}
	if !enabled(config.Sync.Attachments) || enabled(config.Sync.Typing) {
		t.Fatal("channel overrides mutated global defaults")
	}
}

func TestConfigValidation(t *testing.T) {
	for _, test := range []struct {
		name     string
		channels []ChannelConfig
		invalid  bool
	}{
		{"empty", nil, false},
		{"channel", []ChannelConfig{{ID: "123"}}, false},
		{"mapping", []ChannelConfig{{ID: "123", RoomID: "!room:example.org", RelayLoginID: "456"}}, false},
		{"duplicate channel", []ChannelConfig{{ID: "123"}, {ID: "123"}}, true},
		{"bad channel", []ChannelConfig{{ID: "../123"}}, true},
		{"missing login", []ChannelConfig{{ID: "123", RoomID: "!room:example.org"}}, true},
		{"bad room", []ChannelConfig{{ID: "123", RoomID: "#room:example.org", RelayLoginID: "456"}}, true},
		{"duplicate room", []ChannelConfig{{ID: "123", RoomID: "!room:example.org", RelayLoginID: "456"}, {ID: "124", RoomID: "!room:example.org", RelayLoginID: "456"}}, true},
		{"wrong webhook host", []ChannelConfig{{ID: "123", WebhookURL: "https://example.org/api/webhooks/123/token"}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := defaultConfig(t)
			config.Channels = test.channels
			err := (&DiscordConnector{Config: config}).ValidateConfig()
			if (err != nil) != test.invalid {
				t.Fatalf("unexpected validation result: %v", err)
			}
		})
	}
}

func TestWebhookURLValidation(t *testing.T) {
	for _, raw := range []string{"https://discord.com/api/webhooks/123/secret", "https://discord.com/api/v10/webhooks/123/secret"} {
		id, token, err := parseWebhook(raw)
		if err != nil || id != "123" || token != "secret" {
			t.Fatalf("valid URL rejected: %v", err)
		}
	}
	for _, raw := range []string{"http://discord.com/api/webhooks/123/token", "https://discord.com.evil.org/api/webhooks/123/token", "https://user:pass@discord.com/api/webhooks/123/token", "https://discord.com:443/api/webhooks/123/token", "https://discord.com/api/webhooks/123/token?thread_id=124"} {
		if _, _, err := parseWebhook(raw); err == nil {
			t.Fatal("unsafe webhook URL accepted")
		}
	}
}

func TestConfigUpgradePreservesChannelOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("channels:\n  - id: \"123\"\n    relay_login_id: \"456\"\n    sync:\n      attachments: false\nsync:\n  messages: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d := &DiscordConnector{}
	_, _, upgrader := d.GetConfig()
	data, _, err := up.Do(path, false, &up.StructUpgrader{SimpleUpgrader: upgrader.DoUpgrade, Base: ExampleConfig})
	if err != nil {
		t.Fatal(err)
	}
	var config Config
	if err = yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Channels) != 1 || config.Channels[0].RelayLoginID != "456" || enabled(config.syncFor("123").Attachments) || enabled(config.Sync.Messages) {
		t.Fatal("config upgrade discarded explicit false or structured channel config")
	}
	if !enabled(config.Sync.Edits) {
		t.Fatal("new sync defaults were not filled")
	}
}
