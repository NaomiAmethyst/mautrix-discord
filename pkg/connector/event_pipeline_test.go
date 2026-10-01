// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type messageSink struct {
	mu            sync.Mutex
	kinds         []event.Type
	threadParents []id.EventID
}
type pipelineMatrixAPI struct {
	*fakeMatrixAPI
	sink *messageSink
}

func (m *pipelineMatrixAPI) SendMessage(_ context.Context, _ id.RoomID, kind event.Type, content *event.Content, _ *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	m.sink.mu.Lock()
	defer m.sink.mu.Unlock()
	m.sink.kinds = append(m.sink.kinds, kind)
	if kind == event.EventMessage {
		m.sink.threadParents = append(m.sink.threadParents, content.AsMessage().GetRelatesTo().GetThreadParent())
	}
	return &mautrix.RespSendEvent{EventID: id.EventID(fmt.Sprintf("$event%d:example.org", len(m.sink.kinds)))}, nil
}

type pipelineMatrix struct {
	*fakeMatrixConnector
	sink *messageSink
}

func (m *pipelineMatrix) BotIntent() bridgev2.MatrixAPI {
	return &pipelineMatrixAPI{fakeMatrixAPI: m.bot, sink: m.sink}
}
func (m *pipelineMatrix) GhostIntent(uid networkid.UserID) bridgev2.MatrixAPI {
	return &pipelineMatrixAPI{fakeMatrixAPI: &fakeMatrixAPI{mxid: id.UserID("@discord_" + uid + ":example.org")}, sink: m.sink}
}
func (m *pipelineMatrix) ParseGhostMXID(mxid id.UserID) (networkid.UserID, bool) {
	raw := string(mxid)
	if !strings.HasPrefix(raw, "@discord_") || !strings.HasSuffix(raw, ":example.org") {
		return "", false
	}
	return networkid.UserID(strings.TrimSuffix(strings.TrimPrefix(raw, "@discord_"), ":example.org")), true
}
func (m *pipelineMatrix) SendMessageStatus(context.Context, *bridgev2.MessageStatus, *bridgev2.MessageStatusEventInfo) {
}

func TestGatewayMessageAndThreadNoticeThroughBridgev2(t *testing.T) {
	ctx := context.Background()
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", Sync: SyncConfig{Threads: ptr.Ptr(true)}}}
	config.Sync.UserProfiles = ptr.Ptr(false)
	d := &DiscordConnector{Config: config}
	db, err := dbutil.NewWithDialect("file:"+t.TempDir()+"/bridge.db?_foreign_keys=on", "sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sink := &messageSink{}
	mx := &pipelineMatrix{fakeMatrixConnector: &fakeMatrixConnector{bot: &fakeMatrixAPI{mxid: "@bot:example.org"}}, sink: sink}
	br := bridgev2.NewBridge("discord", db, zerolog.Nop(), &bridgeconfig.BridgeConfig{CommandPrefix: "!discord", Permissions: bridgeconfig.PermissionConfig{"@admin:example.org": &bridgeconfig.PermissionLevelAdmin}}, mx, d, commands.NewProcessor)
	br.BackgroundCtx = ctx
	if err := br.DB.Upgrade(ctx); err != nil {
		t.Fatal(err)
	}
	user, err := br.GetUserByMXID(ctx, "@admin:example.org")
	if err != nil {
		t.Fatal(err)
	}
	login, err := user.NewLogin(ctx, &database.UserLogin{ID: "456", Metadata: &LoginMetadata{AccountType: "bot", Token: "test-token"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { login.BridgeState.Destroy() })
	c := login.Client.(*DiscordClient)
	_ = c.Session.State.GuildAdd(&discordgo.Guild{ID: "789"})
	_ = c.Session.State.ChannelAdd(&discordgo.Channel{ID: "123", GuildID: "789", Type: discordgo.ChannelTypeGuildText})
	p, err := br.GetPortalByKey(ctx, c.portalKey("123"))
	if err != nil {
		t.Fatal(err)
	}
	p.MXID = "!room:example.org"
	p.Metadata.(*PortalMetadata).GuildID = "789"
	if err := p.Save(ctx); err != nil {
		t.Fatal(err)
	}
	c.onMessage(&discordgo.Message{ID: "555", ChannelID: "123", GuildID: "789", Author: &discordgo.User{ID: "999", Username: "Alice"}, Content: "Root", Timestamp: time.Now(), Flags: discordgo.MessageFlagsHasThread}, false)
	key := database.Key("discord.thread.notice.reaction.!room:example.org:555")
	deadline := time.Now().Add(3 * time.Second)
	for br.DB.KV.Get(ctx, key) == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if br.DB.KV.Get(ctx, key) == "" {
		t.Fatal("thread notice did not complete through the event pipeline")
	}
	root, err := br.DB.Message.GetFirstPartByID(ctx, "", "555")
	if err != nil || root == nil {
		t.Fatalf("root mapping was not persisted: %v", err)
	}
	notice, err := br.DB.Message.GetFirstPartByID(ctx, "", "thread-notice:555")
	if err != nil || notice == nil || notice.ThreadRoot != "555" || notice.Metadata.(*MessageMetadata).ThreadNotice != "555" {
		t.Fatalf("thread notice mapping was not persisted: %#v %v", notice, err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.kinds) != 3 || sink.kinds[2] != event.EventReaction || len(sink.threadParents) != 2 || sink.threadParents[1] != root.MXID {
		t.Fatalf("thread relations or prefilled join reaction failed: %#v %#v", sink.kinds, sink.threadParents)
	}
}
