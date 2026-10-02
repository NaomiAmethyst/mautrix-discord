// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/util/dbutil"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func testClient(t *testing.T, config Config, transport roundTripFunc) *DiscordClient {
	t.Helper()
	if transport == nil {
		transport = func(*http.Request) (*http.Response, error) {
			t.Error("unexpected HTTP request")
			return response(400, `{}`)
		}
	}
	main := &DiscordConnector{Config: config, HTTP: &http.Client{Transport: transport}}
	main.Bridge = &bridgev2.Bridge{Config: &bridgeconfig.BridgeConfig{Relay: bridgeconfig.RelayConfig{Enabled: true}}, BackgroundCtx: context.Background()}
	session, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	session.Client = main.HTTP
	session.MaxRestRetries = 0
	if err = session.State.GuildAdd(&discordgo.Guild{ID: "789"}); err != nil {
		t.Fatal(err)
	}
	if err = session.State.ChannelAdd(&discordgo.Channel{ID: "123", GuildID: "789", Name: "test", Type: discordgo.ChannelTypeGuildText}); err != nil {
		t.Fatal(err)
	}
	return &DiscordClient{Main: main, Session: session, UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "456"}}}
}

func testPortal(t *testing.T, client *DiscordClient) *bridgev2.Portal {
	t.Helper()
	db, err := dbutil.NewWithDialect("file:"+t.TempDir()+"/bridge.db?_foreign_keys=on", "sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	client.Main.Bridge.DB = database.New("discord", client.Main.GetDBMetaTypes(), db)
	if err = client.Main.Bridge.DB.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	portal := &bridgev2.Portal{Bridge: client.Main.Bridge, Portal: &database.Portal{
		PortalKey: networkid.PortalKey{ID: "123"}, MXID: "!room:example.org", Metadata: &PortalMetadata{},
	}}
	if err = client.Main.Bridge.DB.Portal.Insert(context.Background(), portal.Portal); err != nil {
		t.Fatal(err)
	}
	return portal
}

func TestWebhookRelayPersistsAndAvoidsEcho(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", RelayLoginID: "456"}}
	var creates, sends int
	client := testClient(t, config, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/channels/123/webhooks"):
			return response(200, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/channels/123/webhooks"):
			creates++
			return response(200, `{"id":"555","token":"private-token","channel_id":"123","type":1}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/webhooks/555/private-token"):
			sends++
			var payload discordgo.WebhookParams
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Username != "Alice" || payload.Content != "hello @everyone" || payload.AllowedMentions == nil || len(payload.AllowedMentions.Parse) != 0 {
				t.Fatalf("wrong relay payload: %#v", payload)
			}
			if r.URL.Query().Get("wait") != "true" {
				t.Fatal("webhook send did not wait for a message ID")
			}
			return response(200, `{"id":"999","channel_id":"123","timestamp":"2026-10-01T12:00:00Z"}`)
		default:
			t.Fatalf("unexpected Discord request: %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	portal := testPortal(t, client)
	msg := &bridgev2.MatrixMessage{MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{
		Portal: portal, Event: &event.Event{Type: event.EventMessage}, Content: &event.MessageEventContent{MsgType: event.MsgText, Body: "hello @everyone"},
		OrigSender: &bridgev2.OrigSender{UserID: "@alice:example.org", MemberEventContent: event.MemberEventContent{Displayname: "Alice"}},
	}}
	for i := 0; i < 2; i++ {
		result, err := client.HandleMatrixMessage(context.Background(), msg)
		if err != nil {
			t.Fatal(err)
		}
		if result.DB.ID != "999" || result.DB.Metadata.(*MessageMetadata).WebhookID != "555" {
			t.Fatal("missing outgoing message mapping")
		}
	}
	stored, err := client.Main.Bridge.DB.Portal.GetByKey(context.Background(), portal.PortalKey)
	if err != nil {
		t.Fatal(err)
	}
	if creates != 1 || sends != 2 || stored.Metadata.(*PortalMetadata).WebhookToken != "private-token" {
		t.Fatal("webhook credentials were not persisted/reused")
	}
	if !client.fromOwnWebhook("555") || client.fromOwnWebhook("556") {
		t.Fatal("own-webhook detection failed")
	}
}

func TestConfiguredWebhookCannotTargetOtherChannel(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", WebhookURL: "https://discord.com/api/webhooks/555/private-token"}}
	client := testClient(t, config, func(*http.Request) (*http.Response, error) {
		return response(200, `{"id":"555","channel_id":"124","type":1}`)
	})
	portal := testPortal(t, client)
	if _, _, err := client.ensureWebhook(context.Background(), portal); err == nil {
		t.Fatal("webhook for another channel was accepted")
	}
	if portal.Metadata.(*PortalMetadata).WebhookID != "" {
		t.Fatal("invalid webhook was persisted")
	}
}

func TestDisabledAndUnlistedEventsNeverSend(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", Sync: SyncConfig{Messages: ptr.Ptr(false), Reactions: ptr.Ptr(false), Deletes: ptr.Ptr(false)}}}
	client := testClient(t, config, nil)
	for _, channelID := range []string{"123", "999"} {
		client.onMessage(&discordgo.Message{ID: "111", ChannelID: channelID, Author: &discordgo.User{ID: "777"}, Content: "hello"}, false)
		client.onMessage(&discordgo.Message{ID: "111", ChannelID: channelID}, true)
		client.onDelete(channelID, "111")
		client.onReaction(&discordgo.MessageReaction{ChannelID: channelID, MessageID: "111", UserID: "777"}, false)
		portal := &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: networkid.PortalID(channelID)}}}
		_, _, err := client.outgoingContent(context.Background(), portal, &event.MessageEventContent{MsgType: event.MsgText, Body: "hello"}, false)
		if err == nil {
			t.Fatal("disabled/unlisted outgoing message accepted")
		}
	}
}

func TestMessagePartsAndEditRemovals(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", Sync: SyncConfig{Attachments: ptr.Ptr(false), Stickers: ptr.Ptr(false)}}}
	client := testClient(t, config, nil)
	portal := &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "123"}}}
	message := &discordgo.Message{ID: "111", ChannelID: "123", Content: "**updated**", Attachments: []*discordgo.MessageAttachment{{ID: "112", URL: "https://cdn.discordapp.com/large-file"}},
		MessageReference: &discordgo.MessageReference{MessageID: "110", ChannelID: "123"}}
	converted, err := client.convertMessage(context.Background(), portal, nil, message, "222")
	if err != nil {
		t.Fatal(err)
	}
	if len(converted.Parts) != 1 || converted.ReplyTo.MessageID != "110" || *converted.ThreadRoot != "222" {
		t.Fatal("conversion ignored sync settings or relations")
	}
	if !strings.Contains(converted.Parts[0].Content.FormattedBody, "<strong>updated</strong>") {
		t.Fatal("markdown was not preserved")
	}
	existing := []*database.Message{{PartID: ""}, {PartID: "112"}}
	edited := convertEdit(converted, existing)
	if len(edited.ModifiedParts) != 1 || len(edited.DeletedParts) != 1 || edited.AddedParts != nil {
		t.Fatal("edit did not remove missing parts")
	}
}

func TestThreadAllowlist(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", Sync: SyncConfig{Threads: ptr.Ptr(true)}}}
	client := testClient(t, config, nil)
	for _, ch := range []*discordgo.Channel{{ID: "222", ParentID: "123", GuildID: "789", Type: discordgo.ChannelTypeGuildPublicThread}, {ID: "223", ParentID: "999", GuildID: "789", Type: discordgo.ChannelTypeGuildPublicThread}, {ID: "224", ParentID: "123", Type: discordgo.ChannelTypeDM}} {
		if err := client.Session.State.ChannelAdd(ch); err != nil {
			t.Fatal(err)
		}
	}
	if portal, thread := client.eventPortal(context.Background(), "222"); portal != "123" || thread != "222" {
		t.Fatal("selected channel thread was not resolved")
	}
	for _, id := range []string{"223", "224"} {
		if portal, _ := client.eventPortal(context.Background(), id); portal != "" {
			t.Fatal("unlisted thread or DM was accepted")
		}
	}
	client.Main.Config.Channels[0].Sync.Threads = ptr.Ptr(false)
	if portal, _ := client.eventPortal(context.Background(), "222"); portal != "" {
		t.Fatal("thread opt-out ignored")
	}
}

type testProvisioning struct {
	router *http.ServeMux
	user   *bridgev2.User
}

func (p *testProvisioning) GetRouter() *http.ServeMux            { return p.router }
func (p *testProvisioning) GetUser(*http.Request) *bridgev2.User { return p.user }

func TestProvisioningAdminAndChannelGuards(t *testing.T) {
	client := testClient(t, defaultConfig(t), nil)
	prov := &testProvisioning{router: http.NewServeMux(), user: &bridgev2.User{}}
	client.Main.RegisterProvisioning(prov)
	for _, path := range []string{"/v3/discord/channels", "/v3/discord/channels/123/bridge"} {
		method := http.MethodGet
		if strings.HasSuffix(path, "/bridge") {
			method = http.MethodPost
		}
		resp := httptest.NewRecorder()
		prov.router.ServeHTTP(resp, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
		if resp.Code != http.StatusForbidden {
			t.Fatalf("non-admin request accepted: %d", resp.Code)
		}
	}
	prov.user.Permissions = bridgeconfig.PermissionLevelAdmin
	resp := httptest.NewRecorder()
	prov.router.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/v3/discord/channels", nil))
	if resp.Code != http.StatusOK || strings.Contains(resp.Body.String(), "token") {
		t.Fatal("empty channel listing failed or exposed credentials")
	}
	resp = httptest.NewRecorder()
	prov.router.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/v3/discord/channels/not-a-channel/bridge", strings.NewReader(`{"room_id":"!room:example.org","login_id":"456"}`)))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("provisioning accepted an invalid channel ID: %d", resp.Code)
	}
}

func TestBotLoginFlowAndRejection(t *testing.T) {
	client := testClient(t, defaultConfig(t), func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bot private-token" {
			t.Fatal("incorrect bot authorization")
		}
		return response(401, `{"message":"private-token"}`)
	})
	admin := &bridgev2.User{Permissions: bridgeconfig.PermissionLevelAdmin}
	if _, err := client.Main.CreateLogin(context.Background(), &bridgev2.User{}, "bot-token"); err == nil {
		t.Fatal("non-admin login accepted")
	}
	process, err := client.Main.CreateLogin(context.Background(), admin, "bot-token")
	if err != nil {
		t.Fatal(err)
	}
	step, err := process.Start(context.Background())
	if err != nil || step.Type != bridgev2.LoginStepTypeUserInput || step.UserInputParams.Fields[0].ID != "token" {
		t.Fatal("wrong provisioning login step")
	}
	_, err = process.(*TokenLogin).SubmitUserInput(context.Background(), map[string]string{"token": "Bot private-token"})
	if err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("login rejection leaked token or accepted invalid credentials")
	}
	process.Cancel()
	if _, err = process.(*TokenLogin).SubmitUserInput(context.Background(), map[string]string{"token": "private-token"}); err == nil {
		t.Fatal("cancelled login accepted input")
	}
}

func TestBackfillOrderingAndExclusions(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", Sync: SyncConfig{Backfill: ptr.Ptr(true)}}}
	client := testClient(t, config, func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("before") != "900" {
			t.Fatal("backfill cursor not used")
		}
		return response(200, `[{"id":"803","channel_id":"123","content":"newer","author":{"id":"777"},"timestamp":"2026-10-01T12:00:03Z"},{"id":"802","channel_id":"123","content":"relay echo","webhook_id":"555","author":{"id":"555"},"timestamp":"2026-10-01T12:00:02Z"},{"id":"801","channel_id":"123","content":"older","author":{"id":"777"},"timestamp":"2026-10-01T12:00:01Z"}]`)
	})
	client.Main.ownWebhooks.Store("555", true)
	portal := &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "123"}}}
	batch, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{Portal: portal, Count: 3, Cursor: "900"})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Messages) != 2 || batch.Messages[0].ID != "801" || batch.Messages[1].ID != "803" || batch.Cursor != "801" || !batch.HasMore {
		t.Fatal("backfill ordering, cursor or echo filtering incorrect")
	}
	client.Main.Config.Channels[0].Sync.Backfill = ptr.Ptr(false)
	batch, err = client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{Portal: portal, Count: 3})
	if err != nil || len(batch.Messages) != 0 || batch.HasMore {
		t.Fatal("backfill opt-out ignored")
	}
}

func TestMediaLimitsAndDestinations(t *testing.T) {
	config := defaultConfig(t)
	config.MaxAttachmentSize = 4
	client := testClient(t, config, func(*http.Request) (*http.Response, error) { return response(200, "12345") })
	if _, err := client.Main.download(context.Background(), "https://cdn.discordapp.com/large-file"); err == nil {
		t.Fatal("streaming media limit ignored")
	}
	if _, err := client.Main.download(context.Background(), "https://localhost/private"); err == nil {
		t.Fatal("arbitrary media host accepted")
	}
	client.Main.SetMaxFileSize(2)
	if client.Main.fileLimit() != 2 {
		t.Fatal("Matrix upload limit ignored")
	}
}

func TestThreadWebhookEdits(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", Sync: SyncConfig{Threads: ptr.Ptr(true)}}}
	client := testClient(t, config, func(r *http.Request) (*http.Response, error) {
		if r.Method != "PATCH" || r.URL.Query().Get("thread_id") != "222" {
			t.Fatal("thread edit omitted thread_id")
		}
		return response(200, `{"id":"999"}`)
	})
	portal := &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "123"}, Metadata: &PortalMetadata{WebhookID: "555", WebhookToken: "private-token"}}}
	msg := &bridgev2.MatrixEdit{MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{Portal: portal, Content: &event.MessageEventContent{MsgType: event.MsgText, Body: "updated"}},
		EditTarget: &database.Message{ID: "999", SenderID: "456", Metadata: &MessageMetadata{ChannelID: "222", WebhookID: "555"}},
	}
	if err := client.HandleMatrixEdit(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	client.Main.Config.Channels[0].Sync.Threads = ptr.Ptr(false)
	if err := client.HandleMatrixEdit(context.Background(), msg); err == nil {
		t.Fatal("thread opt-out ignored for edits")
	}
	client.Main.Config.Channels[0].Sync.Threads = ptr.Ptr(true)
	client.Main.Config.Channels[0].Sync.Edits = ptr.Ptr(false)
	if err := client.HandleMatrixEdit(context.Background(), msg); err == nil {
		t.Fatal("edit opt-out ignored")
	}
}

func TestCategoryParentDoesNotSetWebhookThreadQuery(t *testing.T) {
	portal := &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "123"}, Metadata: &PortalMetadata{ParentID: "700"}}}
	req, err := http.NewRequest(http.MethodPatch, "https://discord.com/api/webhooks/555/token/messages/999", nil)
	if err != nil {
		t.Fatal(err)
	}
	threadQuery(portal, "123")(&discordgo.RequestConfig{Request: req})
	if req.URL.Query().Get("thread_id") != "" {
		t.Fatal("category parent was treated as a Discord thread")
	}
}

func TestStartupPreloadsWebhookEchoIdentities(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123"}}
	client := testClient(t, config, nil)
	portal := testPortal(t, client)
	portal.Metadata.(*PortalMetadata).WebhookID = "555"
	if err := portal.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Main.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !client.fromOwnWebhook("555") {
		t.Fatal("startup did not load stored webhook before connecting logins")
	}
}
