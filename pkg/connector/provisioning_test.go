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

	"github.com/bwmarrin/discordgo"
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
	// Shared by every intent: "user type state_key membership".
	log *[]string
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
func (m *fakeMatrixAPI) SendState(_ context.Context, _ id.RoomID, kind event.Type, stateKey string, content *event.Content, _ time.Time) (*mautrix.RespSendEvent, error) {
	m.mu.Lock()
	m.states = append(m.states, kind)
	if m.log != nil {
		entry := fmt.Sprintf("%s %s %s", m.mxid, kind.Type, stateKey)
		if member, ok := content.Parsed.(*event.MemberEventContent); ok {
			entry += " " + string(member.Membership)
		}
		*m.log = append(*m.log, entry)
	}
	m.mu.Unlock()
	return &mautrix.RespSendEvent{EventID: "$state:example.org"}, nil
}

type fakeMatrixConnector struct {
	bridgev2.MatrixConnector
	bot *fakeMatrixAPI
	// Extra members of every room, beyond the admin and the bot.
	members map[id.UserID]*event.MemberEventContent
}

func (m *fakeMatrixConnector) ParseGhostMXID(userID id.UserID) (networkid.UserID, bool) {
	local, ok := strings.CutPrefix(string(userID), "@discord_")
	local, server := strings.CutSuffix(local, ":example.org")
	return networkid.UserID(local), ok && server
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
	return &fakeMatrixAPI{mxid: id.UserID("@discord_" + userID + ":example.org"), log: m.bot.log}
}
func (m *fakeMatrixConnector) GetPowerLevels(context.Context, id.RoomID) (*event.PowerLevelsEventContent, error) {
	return &event.PowerLevelsEventContent{Users: map[id.UserID]int{m.bot.mxid: 100}}, nil
}
func (m *fakeMatrixConnector) GetMemberInfo(context.Context, id.RoomID, id.UserID) (*event.MemberEventContent, error) {
	return &event.MemberEventContent{Membership: event.MembershipJoin}, nil
}
func (m *fakeMatrixConnector) GetMembers(context.Context, id.RoomID) (map[id.UserID]*event.MemberEventContent, error) {
	members := map[id.UserID]*event.MemberEventContent{"@admin:example.org": {Membership: event.MembershipJoin}, m.bot.mxid: {Membership: event.MembershipJoin}}
	for userID, member := range m.members {
		members[userID] = member
	}
	return members, nil
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

// A bridge with an admin logged in as Discord bot 456, answering Discord's API with discord.
func provisioningBridge(t *testing.T, config Config, discord func(*http.Request) (*http.Response, error)) (*bridgev2.Bridge, *fakeMatrixConnector, *testProvisioning) {
	t.Helper()
	d := &DiscordConnector{Config: config}
	db, err := dbutil.NewWithDialect("file:"+t.TempDir()+"/bridge.db?_foreign_keys=on", "sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mx := &fakeMatrixConnector{bot: &fakeMatrixAPI{mxid: "@discordbot:example.org", log: &[]string{}}}
	br := bridgev2.NewBridge("discord", db, zerolog.Nop(), &bridgeconfig.BridgeConfig{
		Relay: bridgeconfig.RelayConfig{Enabled: true}, Permissions: bridgeconfig.PermissionConfig{"@admin:example.org": &bridgeconfig.PermissionLevelAdmin},
	}, mx, d, commands.NewProcessor)
	br.BackgroundCtx = context.Background()
	if err = br.DB.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.HTTP = &http.Client{Transport: roundTripFunc(discord)}
	user, err := br.GetUserByMXID(context.Background(), "@admin:example.org")
	if err != nil {
		t.Fatal(err)
	}
	login, err := user.NewLogin(context.Background(), &database.UserLogin{ID: "456", Metadata: &LoginMetadata{Token: "bot-token"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { login.BridgeState.Destroy() })
	// The bot's guilds, as the gateway would have them on connecting.
	for _, guild := range config.Guilds {
		if err = login.Client.(*DiscordClient).Session.State.GuildAdd(&discordgo.Guild{ID: guild.ID}); err != nil {
			t.Fatal(err)
		}
	}
	prov := &testProvisioning{router: http.NewServeMux(), user: user}
	d.RegisterProvisioning(prov)
	return br, mx, prov
}

func provision(prov *testProvisioning, method, path, body string) *httptest.ResponseRecorder {
	resp := httptest.NewRecorder()
	prov.router.ServeHTTP(resp, httptest.NewRequest(method, path, strings.NewReader(body)))
	return resp
}

// Channels made on the fly (by a reconciler, say) are bound by their guild's mode, without
// listing them in config first, and unbound leaving the room to its people.
func TestProvisioningBindsByGuildModeAndUnbinds(t *testing.T) {
	config := defaultConfig(t)
	config.Guilds = []GuildConfig{{ID: "789", Mode: "if-portal-exists"}, {ID: "790", Mode: "nothing"}}
	created := 0
	br, mx, prov := provisioningBridge(t, config, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/channels/123"):
			return response(200, `{"id":"123","guild_id":"789","name":"made-just-now","type":0}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/channels/124"):
			return response(200, `{"id":"124","guild_id":"789","name":"made-later","type":0}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/channels/125"):
			return response(200, `{"id":"125","guild_id":"790","name":"off-limits","type":0}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/channels/126"):
			return response(404, `{"message":"Unknown Channel","code":10003}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/webhooks"):
			return response(200, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/webhooks"):
			created++
			return response(200, fmt.Sprintf(`{"id":"55%d","token":"hook-token","type":1}`, created))
		default:
			t.Fatalf("unexpected Discord request: %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	mx.members = map[id.UserID]*event.MemberEventContent{"@discord_999:example.org": {Membership: event.MembershipJoin}}

	// Not in network.channels, nor yet in the gateway's cache: fetched, and allowed by its guild.
	if resp := provision(prov, http.MethodPost, "/v3/discord/channels/123/bridge", `{"room_id":"!room:example.org","login_id":"456"}`); resp.Code != http.StatusOK {
		t.Fatalf("bind by guild mode failed: %d %s", resp.Code, resp.Body.String())
	}
	if resp := provision(prov, http.MethodPost, "/v3/discord/channels/125/bridge", `{"room_id":"!other:example.org","login_id":"456"}`); resp.Code != http.StatusForbidden {
		t.Fatalf("bound a channel in a guild set to nothing: %d", resp.Code)
	}
	if resp := provision(prov, http.MethodPost, "/v3/discord/channels/126/bridge", `{"room_id":"!other:example.org","login_id":"456"}`); resp.Code != http.StatusNotFound {
		t.Fatalf("bound a channel Discord doesn't have: %d", resp.Code)
	}

	// Unbinding forgets the portal; the bridge's own users leave, and its bridge info goes.
	*mx.bot.log = nil
	resp := provision(prov, http.MethodDelete, "/v3/discord/channels/123/bridge", "")
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "!room:example.org") {
		t.Fatalf("unbind failed: %d %s", resp.Code, resp.Body.String())
	}
	if stored, err := br.DB.Portal.GetByKey(context.Background(), networkid.PortalKey{ID: "123"}); err != nil || stored != nil {
		t.Fatalf("portal kept after unbinding: %v", err)
	}
	log := strings.Join(*mx.bot.log, "\n")
	for _, want := range []string{
		"@discord_999:example.org m.room.member @discord_999:example.org leave",
		"@discordbot:example.org m.bridge ",
		"@discordbot:example.org uk.half-shot.bridge ",
		"@discordbot:example.org m.room.member @discordbot:example.org leave",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("unbinding didn't send %q; sent:\n%s", want, log)
		}
	}
	if strings.Contains(log, "@admin:example.org leave") {
		t.Errorf("unbinding removed a person from the room; sent:\n%s", log)
	}
	if resp := provision(prov, http.MethodDelete, "/v3/discord/channels/123/bridge", ""); resp.Code != http.StatusNotFound {
		t.Fatalf("unbinding twice: %d", resp.Code)
	}

	// The room is free for another channel, as when a chat gets Discord back.
	if resp := provision(prov, http.MethodPost, "/v3/discord/channels/124/bridge", `{"room_id":"!room:example.org","login_id":"456"}`); resp.Code != http.StatusOK {
		t.Fatalf("rebinding the room failed: %d %s", resp.Code, resp.Body.String())
	}
}
