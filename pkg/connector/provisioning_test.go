// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type fakeMatrixAPI struct {
	bridgev2.MatrixAPI
	mxid   id.UserID
	mu     sync.Mutex
	states []event.Type
}

func (m *fakeMatrixAPI) GetMXID() id.UserID   { return m.mxid }
func (m *fakeMatrixAPI) IsDoublePuppet() bool { return false }
func (m *fakeMatrixAPI) EnsureJoined(context.Context, id.RoomID, ...bridgev2.EnsureJoinedParams) error {
	return nil
}
func (m *fakeMatrixAPI) EnsureInvited(context.Context, id.RoomID, id.UserID) error { return nil }
func (m *fakeMatrixAPI) SetDisplayName(context.Context, string) error              { return nil }
func (m *fakeMatrixAPI) SetAvatarURL(context.Context, id.ContentURIString) error   { return nil }
func (m *fakeMatrixAPI) SetExtraProfileMeta(context.Context, any) error            { return nil }
func (m *fakeMatrixAPI) SendState(_ context.Context, _ id.RoomID, kind event.Type, _ string, _ *event.Content, _ time.Time) (*mautrix.RespSendEvent, error) {
	m.mu.Lock()
	m.states = append(m.states, kind)
	m.mu.Unlock()
	return &mautrix.RespSendEvent{EventID: "$state:example.org"}, nil
}

type fakeMatrixConnector struct {
	bridgev2.MatrixConnector
	bot *fakeMatrixAPI
}

func (m *fakeMatrixConnector) Init(*bridgev2.Bridge)         {}
func (m *fakeMatrixConnector) BotIntent() bridgev2.MatrixAPI { return m.bot }
func (m *fakeMatrixConnector) NewUserIntent(context.Context, id.UserID, string) (bridgev2.MatrixAPI, string, error) {
	return nil, "", fmt.Errorf("no double puppet configured")
}
func (m *fakeMatrixConnector) GetCapabilities() *bridgev2.MatrixCapabilities {
	return &bridgev2.MatrixCapabilities{}
}
func (m *fakeMatrixConnector) SendBridgeStatus(context.Context, *status.BridgeState) error {
	return nil
}
func (m *fakeMatrixConnector) ServerName() string { return "example.org" }
func (m *fakeMatrixConnector) GhostIntent(userID networkid.UserID) bridgev2.MatrixAPI {
	return &fakeMatrixAPI{mxid: id.UserID("@discord_" + userID + ":example.org")}
}
func (m *fakeMatrixConnector) GetPowerLevels(context.Context, id.RoomID) (*event.PowerLevelsEventContent, error) {
	return &event.PowerLevelsEventContent{Users: map[id.UserID]int{m.bot.mxid: 100}}, nil
}
func (m *fakeMatrixConnector) GetMemberInfo(context.Context, id.RoomID, id.UserID) (*event.MemberEventContent, error) {
	return &event.MemberEventContent{Membership: event.MembershipJoin}, nil
}
func (m *fakeMatrixConnector) GetMembers(context.Context, id.RoomID) (map[id.UserID]*event.MemberEventContent, error) {
	return map[id.UserID]*event.MemberEventContent{"@admin:example.org": {Membership: event.MembershipJoin}, m.bot.mxid: {Membership: event.MembershipJoin}}, nil
}

func TestProvisioningBindPersistsRelayAndPreservesRoomMetadata(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", RelayLoginID: "456", WebhookURL: "https://discord.com/api/webhooks/555/private-token"}}
	d := &DiscordConnector{Config: config}
	db, err := dbutil.NewWithDialect("file:"+t.TempDir()+"/bridge.db?_foreign_keys=on", "sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bot := &fakeMatrixAPI{mxid: "@discordbot:example.org"}
	mx := &fakeMatrixConnector{bot: bot}
	br := bridgev2.NewBridge("discord", db, zerolog.Nop(), &bridgeconfig.BridgeConfig{
		Relay: bridgeconfig.RelayConfig{Enabled: true}, Permissions: bridgeconfig.PermissionConfig{"@admin:example.org": &bridgeconfig.PermissionLevelAdmin},
	}, mx, d, commands.NewProcessor)
	br.BackgroundCtx = context.Background()
	if err = br.DB.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/webhooks/555/private-token"):
			return response(200, `{"id":"555","channel_id":"123","type":1}`)
		case strings.HasSuffix(r.URL.Path, "/channels/123"):
			return response(200, `{"id":"123","guild_id":"789","name":"discord-name","topic":"discord-topic","type":0,"parent_id":"700"}`)
		default:
			t.Fatalf("unexpected Discord request: %s", r.URL.Path)
			return nil, nil
		}
	})}
	user, err := br.GetUserByMXID(context.Background(), "@admin:example.org")
	if err != nil {
		t.Fatal(err)
	}
	login, err := user.NewLogin(context.Background(), &database.UserLogin{ID: "456", Metadata: &LoginMetadata{Token: "bot-token"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { login.BridgeState.Destroy() })
	prov := &testProvisioning{router: http.NewServeMux(), user: user}
	d.RegisterProvisioning(prov)
	resp := httptest.NewRecorder()
	prov.router.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/v3/discord/channels/123/bridge", strings.NewReader(`{"room_id":"!room:example.org","login_id":"456"}`)))
	if resp.Code != http.StatusOK {
		t.Fatalf("bind failed: %d %s", resp.Code, resp.Body.String())
	}
	stored, err := br.DB.Portal.GetByKey(context.Background(), networkid.PortalKey{ID: "123"})
	if err != nil {
		t.Fatal(err)
	}
	if stored.MXID != "!room:example.org" || stored.RelayLoginID != "456" || stored.Metadata.(*PortalMetadata).WebhookID != "555" {
		t.Fatal("binding or relay credentials were not persisted")
	}
	if stored.Name != "" || stored.Topic != "" || stored.Metadata.(*PortalMetadata).IsThread {
		t.Fatal("binding overwrote Matrix metadata or treated category parent as a thread")
	}
	bot.mu.Lock()
	for _, state := range bot.states {
		if state == event.StateRoomName || state == event.StateTopic {
			t.Error("disabled metadata sync sent state")
		}
	}
	bot.mu.Unlock()
	resp = httptest.NewRecorder()
	prov.router.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/v3/discord/channels", nil))
	var list struct {
		Channels []struct {
			RoomID  id.RoomID `json:"room_id"`
			LoginID string    `json:"relay_login_id"`
		} `json:"channels"`
	}
	if err = json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Channels) != 1 || list.Channels[0].RoomID != stored.MXID || list.Channels[0].LoginID != "456" || strings.Contains(resp.Body.String(), "private-token") {
		t.Fatal("channel listing failed or exposed webhook credentials")
	}
	resp = httptest.NewRecorder()
	prov.router.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/v3/discord/channels/123/bridge", strings.NewReader(`{"room_id":"!different:example.org","login_id":"456"}`)))
	if resp.Code != http.StatusConflict {
		t.Fatal("existing mapping was overwritten")
	}
}
