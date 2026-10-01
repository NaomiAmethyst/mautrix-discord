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
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/mediaproxy"
)

type mediaTestAPI struct {
	fakeMatrixAPI
	uploads int
}

func (m *mediaTestAPI) UploadMedia(_ context.Context, _ id.RoomID, _ []byte, _ string, _ string) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	m.uploads++
	return "mxc://example.org/emoji", nil, nil
}

func TestDiscordFormattingParity(t *testing.T) {
	c := testClient(t, defaultConfig(t), nil)
	p := &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "123"}, Metadata: &PortalMetadata{GuildID: "789"}}}
	tests := []struct {
		input, html string
		room        bool
	}{
		{"__under__ ||secret||", "<u>under</u>", false},
		{"@everyone", "@room", true},
		{"`@everyone`", "<code>@everyone</code>", false},
		{"`<:emoji:999>`", "<code>&lt;:emoji:999&gt;</code>", false},
		{"<t:1700000000:F>", "<time ", false},
		{"<script>alert(1)</script>", "&lt;script&gt;", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			content := c.discordText(context.Background(), p, tt.input)
			if !strings.Contains(content.FormattedBody, tt.html) || content.Mentions.Room != tt.room {
				t.Fatalf("unexpected conversion: %#v", content)
			}
			if strings.Contains(content.FormattedBody, "<script>") {
				t.Fatal("raw HTML was allowed")
			}
			if strings.HasPrefix(tt.input, "<t:") && strings.Contains(content.FormattedBody, "&lt;t:") {
				t.Fatal("timestamp syntax was duplicated")
			}
		})
	}
}
func TestCustomEmojiReuseSurvivesRestart(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123"}}
	downloads := 0
	c := testClient(t, config, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "cdn.discordapp.com" {
			t.Fatalf("wrong emoji host: %s", r.URL)
		}
		downloads++
		return response(200, "image")
	})
	p := testPortal(t, c)
	api := &mediaTestAPI{}
	c.Main.Bridge.Bot = api
	content := c.discordText(context.Background(), p, "hello <:party:999>")
	if !strings.Contains(content.FormattedBody, `data-mx-emoticon`) || !strings.Contains(content.FormattedBody, `mxc://example.org/emoji`) {
		t.Fatalf("custom emoji was not rendered: %s", content.FormattedBody)
	}
	c2 := &DiscordClient{Session: c.Session, UserLogin: c.UserLogin}
	c2.Main = &DiscordConnector{Bridge: c.Main.Bridge, Config: config, HTTP: c.Main.HTTP}
	uri, err := c2.emojiMXC(context.Background(), "999", "party", false)
	if err != nil || uri != "mxc://example.org/emoji" || downloads != 1 || api.uploads != 1 {
		t.Fatalf("emoji cache not reused: %s %v downloads=%d", uri, err, downloads)
	}
	pre, err := c2.PreHandleMatrixReaction(context.Background(), &bridgev2.MatrixReaction{MatrixEventBase: bridgev2.MatrixEventBase[*event.ReactionEventContent]{Portal: p, Content: &event.ReactionEventContent{RelatesTo: event.RelatesTo{Key: string(uri)}}}})
	if err != nil || pre.Emoji != "party:999" || pre.EmojiID != "999" {
		t.Fatalf("could not reuse custom reaction: %#v %v", pre, err)
	}
	if _, err = c2.Main.discordEmoji(context.Background(), "mxc://example.org/unknown"); err == nil {
		t.Fatal("arbitrary emoji upload was accepted")
	}
}
func TestPersonalAndBotDiscoveryPolicies(t *testing.T) {
	config := defaultConfig(t)
	config.PersonalAccounts = true
	config.SyncDMs = true
	config.Guilds = []GuildConfig{{ID: "789", Mode: "create-on-message"}}
	c := testClient(t, config, nil)
	c.UserLogin.Metadata = &LoginMetadata{AccountType: "user"}
	c.Session.IsUser = true
	dm := &discordgo.Channel{ID: "888", Type: discordgo.ChannelTypeDM, Recipients: []*discordgo.User{{ID: "111", Username: "Alice"}}}
	_ = c.Session.State.ChannelAdd(dm)
	if !c.allowed("888") || c.portalKey("888").Receiver != "456" {
		t.Fatal("DMs weren't isolated by account")
	}
	if !c.allowed("123") || !c.canCreate("123") {
		t.Fatal("create-on-message mode was ignored")
	}
	p := &bridgev2.Portal{Portal: &database.Portal{PortalKey: c.portalKey("888"), Metadata: &PortalMetadata{}}}
	info, err := c.GetChatInfo(context.Background(), p)
	if err != nil || *info.Type != database.RoomTypeDM || *info.Name != "Alice" || info.Members.OtherUserID != "111" {
		t.Fatalf("DM metadata incorrect: %#v %v", info, err)
	}
	c.Session.IsUser = false
	if c.allowed("888") {
		t.Fatal("bot imported a personal DM")
	}
	c.Main.Config.Guilds = nil
	if c.allowed("123") {
		t.Fatal("bot imported a non-allowlisted guild channel")
	}
}
func TestThreadCreationFromMatrix(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", Sync: SyncConfig{Threads: ptr.Ptr(true)}}}
	creates := 0
	c := testClient(t, config, func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return response(404, `{"code":10003,"message":"Unknown Channel"}`)
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages/555/threads") {
			creates++
			return response(200, `{"id":"555","parent_id":"123","guild_id":"789","type":11}`)
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		return nil, nil
	})
	p := testPortal(t, c)
	root := &database.Message{ID: "555", Metadata: &MessageMetadata{ChannelID: "123"}}
	for i := 0; i < 2; i++ {
		tid, err := c.ensureThread(context.Background(), p, root, "Discussion")
		if err != nil || tid != "555" {
			t.Fatalf("thread creation failed: %s %v", tid, err)
		}
	}
	if creates != 1 {
		t.Fatal("thread was created more than once")
	}
	c.Main.Config.Channels[0].Sync.Threads = ptr.Ptr(false)
	if _, err := c.ensureThread(context.Background(), p, root, "disabled"); err == nil {
		t.Fatal("disabled thread creation was accepted")
	}
}
func TestThreadBackfillUsesThreadChannel(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", Sync: SyncConfig{Threads: ptr.Ptr(true), Backfill: ptr.Ptr(true)}}}
	c := testClient(t, config, func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/channels/555/messages") {
			t.Fatalf("history fetched from wrong channel: %s", r.URL)
		}
		return response(200, `[{"id":"666","channel_id":"555","timestamp":"2026-10-01T12:00:00Z","author":{"id":"111","username":"Alice"},"content":"thread history"}]`)
	})
	_ = c.Session.State.ChannelAdd(&discordgo.Channel{ID: "555", ParentID: "123", GuildID: "789", Type: discordgo.ChannelTypeGuildPublicThread})
	p := testPortal(t, c)
	resp, err := c.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{Portal: p, ThreadRoot: "555", Count: 10})
	if err != nil || len(resp.Messages) != 1 || *resp.Messages[0].ThreadRoot != "555" {
		t.Fatalf("thread history incorrect: %#v %v", resp, err)
	}
}
func TestWebhookReplyPreviewRoundTrip(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123"}}
	c := testClient(t, config, nil)
	p := testPortal(t, c)
	p.Metadata.(*PortalMetadata).GuildID = "789"
	target := &database.Message{ID: "555", SenderID: "111", Metadata: &MessageMetadata{ChannelID: "123", Excerpt: "previous <message>"}}
	embed := c.replyEmbed(context.Background(), &bridgev2.MatrixMessage{MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{Portal: p}, ReplyTo: target}, "789")
	msg := &discordgo.Message{ID: "666", ChannelID: "123", Content: "reply", Embeds: []*discordgo.MessageEmbed{embed}}
	converted, err := c.convertMessage(context.Background(), p, nil, msg, "")
	if err != nil || converted.ReplyTo == nil || converted.ReplyTo.MessageID != "555" || len(converted.Parts) != 1 || strings.Contains(converted.Parts[0].Content.Body, "Replying to") {
		t.Fatalf("reply preview wasn't restored as relation: %#v %v", converted, err)
	}
}
func TestDirectMediaRejectsUnsafeIDsAndRefreshesAttachments(t *testing.T) {
	c := testClient(t, defaultConfig(t), func(r *http.Request) (*http.Response, error) {
		return response(200, `{"id":"555","attachments":[{"id":"666","url":"https://cdn.discordapp.com/attachments/123/666/file?ex=ffffffff"}]}`)
	})
	c.Main.Config.Channels = []ChannelConfig{{ID: "123"}}
	c.Main.clients.Store(c.UserLogin.ID, c)
	for _, raw := range []string{`{}`, `{"url":"http://localhost/private"}`, `{"url":"https://evil.example/file"}`} {
		if _, err := c.Main.Download(context.Background(), networkid.MediaID(raw), nil); err == nil {
			t.Fatal("unsafe media ID accepted")
		}
	}
	raw, _ := json.Marshal(remoteMedia{URL: "https://cdn.discordapp.com/attachments/123/666/file?ex=1", Channel: "123", Message: "555", Attachment: "666"})
	result, err := c.Main.Download(context.Background(), networkid.MediaID(raw), nil)
	if err != nil || !strings.Contains(result.(*mediaproxy.GetMediaResponseURL).URL, "ex=ffffffff") {
		t.Fatalf("expired attachment not refreshed: %#v %v", result, err)
	}
}
func TestAccountLoadAndNativeDirectSend(t *testing.T) {
	config := defaultConfig(t)
	config.PersonalAccounts = true
	config.Channels = []ChannelConfig{{ID: "123"}}
	d := &DiscordConnector{Config: config}
	db, err := dbutil.NewWithDialect("file:"+t.TempDir()+"/bridge.db?_foreign_keys=on", "sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	br := bridgev2.NewBridge("discord", db, zerolog.Nop(), &bridgeconfig.BridgeConfig{Permissions: bridgeconfig.PermissionConfig{"@alice:example.org": &bridgeconfig.PermissionLevelUser}}, &fakeMatrixConnector{bot: &fakeMatrixAPI{mxid: "@bot:example.org"}}, d, commands.NewProcessor)
	br.BackgroundCtx = context.Background()
	if err = br.DB.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "user-token" || !strings.HasSuffix(r.URL.Path, "/channels/123/messages") {
			t.Fatalf("personal send used wrong auth or endpoint: %s", r.URL)
		}
		var payload discordgo.MessageSend
		if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Nonce == "" || payload.Content != "hello" {
			t.Fatal("invalid direct message payload")
		}
		return response(200, `{"id":"999","channel_id":"123","timestamp":"2026-10-01T12:00:00Z"}`)
	})}
	user, err := br.GetUserByMXID(context.Background(), "@alice:example.org")
	if err != nil {
		t.Fatal(err)
	}
	login, err := user.NewLogin(context.Background(), &database.UserLogin{ID: "456", Metadata: &LoginMetadata{Token: "user-token", AccountType: "user"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { login.BridgeState.Destroy() })
	c := login.Client.(*DiscordClient)
	if !c.Session.IsUser || c.Session.Identify.Intents != 0 {
		t.Fatal("user token initialized as bot")
	}
	_ = c.Session.State.GuildAdd(&discordgo.Guild{ID: "789"})
	_ = c.Session.State.ChannelAdd(&discordgo.Channel{ID: "123", GuildID: "789", Type: discordgo.ChannelTypeGuildText})
	p, err := br.GetPortalByKey(context.Background(), networkid.PortalKey{ID: "123"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.HandleMatrixMessage(context.Background(), &bridgev2.MatrixMessage{MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{Portal: p, Event: &event.Event{Type: event.EventMessage, ID: "$test", Sender: user.MXID}, Content: &event.MessageEventContent{MsgType: event.MsgText, Body: "hello"}}})
	if err != nil || result.DB.ID != "999" || result.RemovePending == "" || result.DB.Metadata.(*MessageMetadata).WebhookID != "" {
		t.Fatalf("personal message not sent directly: %#v %v", result, err)
	}
}
func TestLegacyProvisioningDisallowsUnpermittedLogin(t *testing.T) {
	c := testClient(t, defaultConfig(t), nil)
	prov := &testProvisioning{router: http.NewServeMux(), user: &bridgev2.User{}}
	c.Main.RegisterProvisioning(prov)
	for _, path := range []string{"/v1/login/token", "/v1/login/qr", "/v1/guilds/789"} {
		method := "POST"
		body := strings.NewReader(`{"token":"Bot secret"}`)
		if strings.HasSuffix(path, "/qr") {
			method = "GET"
		}
		req := httptest.NewRequest(method, path, body)
		w := httptest.NewRecorder()
		if strings.Contains(path, "/guilds/") {
			req.Method = "DELETE"
		}
		prov.router.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("unpermitted compatibility request accepted: %s %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), "secret") {
			t.Fatal("token leaked")
		}
	}
}
func TestMatrixMentionRequiresRoomNotificationPower(t *testing.T) {
	config := defaultConfig(t)
	config.AllowMentions = true
	c := testClient(t, config, nil)
	p := testPortal(t, c)
	bot := &fakeMatrixAPI{mxid: "@bot:example.org"}
	c.Main.Bridge.Matrix = &fakeMatrixConnector{bot: bot}
	content := &event.MessageEventContent{Mentions: &event.Mentions{Room: true}}
	low := c.allowedMentions(context.Background(), p, content, "@alice:example.org", "hello @everyone")
	high := c.allowedMentions(context.Background(), p, content, bot.mxid, "hello @everyone")
	if len(low.Parse) != 0 || len(high.Parse) != 1 {
		t.Fatal("room mention bypassed power levels")
	}
}
func TestMediaBoundsStillApplyWithParityOptions(t *testing.T) {
	c := testClient(t, defaultConfig(t), func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 101)))}, nil
	})
	c.Main.Config.MaxAttachmentSize = 100
	if _, err := c.Main.download(context.Background(), "https://cdn.discordapp.com/file"); err == nil {
		t.Fatal("unbounded download")
	}
	if got := replyExcerpt(strings.Repeat("界", 80)); len([]rune(got)) != 73 {
		t.Fatal("reply excerpt cut multibyte text incorrectly")
	}
	if mediaExpiry("https://cdn.discordapp.com/file?ex=1") != time.Unix(1, 0) {
		t.Fatal("expiry parsing failed")
	}
}

func TestGuildThreadsRemainInParentAndRespectOptOut(t *testing.T) {
	config := defaultConfig(t)
	config.Guilds = []GuildConfig{{ID: "789", Mode: "everything"}}
	config.Sync.Threads = ptr.Ptr(true)
	c := testClient(t, config, nil)
	c.UserLogin.Metadata = &LoginMetadata{}
	_ = c.Session.State.ChannelAdd(&discordgo.Channel{ID: "555", ParentID: "123", GuildID: "789", Type: discordgo.ChannelTypeGuildPublicThread})
	pid, tid := c.eventPortal(context.Background(), "555")
	if pid != "123" || tid != "555" {
		t.Fatalf("guild thread got a separate portal: %s %s", pid, tid)
	}
	c.Main.Config.Sync.Threads = ptr.Ptr(false)
	if pid, _ = c.eventPortal(context.Background(), "555"); pid != "" {
		t.Fatal("guild thread ignored opt-out")
	}
}
