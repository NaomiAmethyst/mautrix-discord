// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
)

func TestCDNPreparedUploadDoesNotForwardToken(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123"}}
	requests := 0
	c := testClient(t, config, func(r *http.Request) (*http.Response, error) {
		requests++
		switch requests {
		case 1:
			if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/channels/123/attachments") || r.Header.Get("Authorization") == "" {
				t.Fatal("incorrect attachment preparation request")
			}
			var req discordgo.ReqPrepareAttachments
			if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Files) != 1 || req.Files[0].Size != 5 || req.Files[0].Name != "SPOILER_a.png" {
				t.Fatal("incorrect preparation metadata")
			}
			return response(200, `{"attachments":[{"id":0,"upload_filename":"uploaded-a.png","upload_url":"https://upload.example.org/file"}]}`)
		case 2:
			body, _ := io.ReadAll(r.Body)
			if r.Method != "PUT" || r.Header.Get("Authorization") != "" || r.URL.Host != "upload.example.org" || string(body) != "image" {
				t.Fatal("upload body or credential isolation failed")
			}
			return response(200, "")
		default:
			t.Fatal("unexpected request")
			return nil, nil
		}
	})
	files := []*discordgo.File{{Name: "a.png", ContentType: "image/png", Reader: bytes.NewBufferString("image")}}
	markSpoilers(files, &event.Event{Content: event.Content{Raw: map[string]any{"page.codeberg.everypizza.msc4193.spoiler": true}}})
	attachments, err := c.prepareAttachments(context.Background(), "123", files)
	if err != nil || len(attachments) != 1 || attachments[0].UploadedFilename != "uploaded-a.png" || requests != 2 {
		t.Fatalf("prepared upload failed: %#v %v", attachments, err)
	}
}

func TestManagedWebhookRecoversOnlyDefinitiveRejection(t *testing.T) {
	for _, definitive := range []bool{false, true} {
		t.Run(fmt.Sprint(definitive), func(t *testing.T) {
			config := defaultConfig(t)
			config.Channels = []ChannelConfig{{ID: "123"}}
			sends, creates := 0, 0
			c := testClient(t, config, func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.Contains(r.URL.Path, "/webhooks/555/"):
					sends++
					if definitive {
						return response(404, `{"code":10015,"message":"Unknown Webhook"}`)
					}
					return nil, fmt.Errorf("uncertain send failure")
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/channels/123/webhooks"):
					return response(200, `[]`)
				case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/channels/123/webhooks"):
					creates++
					return response(200, `{"id":"556","token":"new-token","channel_id":"123","type":1}`)
				case strings.Contains(r.URL.Path, "/webhooks/556/"):
					sends++
					return response(200, `{"id":"999","channel_id":"123","timestamp":"2026-10-01T12:00:00Z"}`)
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
					return nil, nil
				}
			})
			p := testPortal(t, c)
			meta := p.Metadata.(*PortalMetadata)
			meta.WebhookID, meta.WebhookToken = "555", "old-token"
			msg := &bridgev2.MatrixMessage{MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{Portal: p, Event: &event.Event{Type: event.EventMessage}, Content: &event.MessageEventContent{MsgType: event.MsgText, Body: "hello"}, OrigSender: &bridgev2.OrigSender{UserID: "@alice:example.org", MemberEventContent: event.MemberEventContent{Displayname: "Alice"}}}}
			result, err := c.HandleMatrixMessage(context.Background(), msg)
			if definitive {
				if err != nil || creates != 1 || sends != 2 || result.DB.Metadata.(*MessageMetadata).WebhookID != "556" {
					t.Fatalf("webhook was not recovered: %#v %v", result, err)
				}
				stored, _ := c.Main.Bridge.DB.Portal.GetByKey(context.Background(), p.PortalKey)
				if stored.Metadata.(*PortalMetadata).WebhookToken != "new-token" {
					t.Fatal("replacement webhook was not persisted")
				}
			} else if err == nil || sends != 1 || creates != 0 {
				t.Fatal("uncertain send was retried")
			}
		})
	}
}

func TestMediaCacheSkipsDownloadsAndHonorsLoweredLimit(t *testing.T) {
	config := defaultConfig(t)
	config.CacheMedia = "always"
	downloads := 0
	c := testClient(t, config, func(r *http.Request) (*http.Response, error) { downloads++; return response(200, "image") })
	p := testPortal(t, c)
	api := &mediaTestAPI{}
	att := &discordgo.MessageAttachment{ID: "999", URL: "https://cdn.discordapp.com/attachments/123/999/image.png?ex=aaa", Filename: "image.png", ContentType: "image/png"}
	for i := 0; i < 2; i++ {
		part, err := c.mediaPart(context.Background(), p, api, att, "999", &MessageMetadata{ChannelID: "123"})
		if err != nil || part.Content.URL == "" {
			t.Fatalf("media transfer failed: %#v %v", part, err)
		}
		att.URL = "https://cdn.discordapp.com/attachments/123/999/image.png?ex=bbb"
	}
	if downloads != 1 || api.uploads != 1 {
		t.Fatal("attachment cache did not reuse the transfer")
	}
	c.Main.Config.MaxAttachmentSize = 2
	part, err := c.mediaPart(context.Background(), p, api, att, "999", &MessageMetadata{})
	if err != nil || part.Content.MsgType != event.MsgNotice {
		t.Fatal("cached media bypassed the lowered transfer limit")
	}
}

func TestPerMessageProfilesRespectSyncToggle(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123"}}
	c := testClient(t, config, nil)
	p := testPortal(t, c)
	msg := &discordgo.Message{Author: &discordgo.User{ID: "999", Username: "Global"}, GuildID: "789", ChannelID: "123", Content: "hello", Member: &discordgo.Member{Nick: "Guild nickname"}}
	converted, err := c.convertMessage(context.Background(), p, &mediaTestAPI{}, msg, "")
	if err != nil || converted.Parts[0].Content.BeeperPerMessageProfile.Displayname != "Guild nickname" || converted.Parts[0].Extra["fi.mau.discord.guild_member_metadata"] == nil {
		t.Fatal("guild profile was lost")
	}
	msg.WebhookID = "888"
	converted, err = c.convertMessage(context.Background(), p, &mediaTestAPI{}, msg, "")
	if err != nil || converted.Parts[0].Content.BeeperPerMessageProfile.Displayname != "Global" || converted.Parts[0].Extra["fi.mau.discord.webhook_metadata"] == nil {
		t.Fatal("webhook profile was lost")
	}
	c.Main.Config.Channels[0].Sync.UserProfiles = ptr.Ptr(false)
	converted, err = c.convertMessage(context.Background(), p, &mediaTestAPI{}, msg, "")
	if err != nil || converted.Parts[0].Content.BeeperPerMessageProfile != nil {
		t.Fatal("disabled profiles were still imported")
	}
}

func TestJoinThreadReactionUsesMembershipEndpoint(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", Sync: SyncConfig{Threads: ptr.Ptr(true)}}}
	joined := 0
	c := testClient(t, config, func(r *http.Request) (*http.Response, error) {
		if r.Method != "PUT" || !strings.HasSuffix(r.URL.Path, "/channels/555/thread-members/@me") {
			t.Fatalf("join used wrong endpoint: %s %s", r.Method, r.URL.Path)
		}
		joined++
		return response(204, "")
	})
	_ = c.Session.State.ChannelAdd(&discordgo.Channel{ID: "555", ParentID: "123", GuildID: "789", Type: discordgo.ChannelTypeGuildPublicThread})
	p := &bridgev2.Portal{Portal: &database.Portal{PortalKey: c.portalKey("123")}}
	target := &database.Message{ID: "thread-notice:555", ThreadRoot: "555", Metadata: &MessageMetadata{ChannelID: "555", ThreadNotice: "555"}}
	result, err := c.HandleMatrixReaction(context.Background(), &bridgev2.MatrixReaction{MatrixEventBase: bridgev2.MatrixEventBase[*event.ReactionEventContent]{Portal: p}, TargetMessage: target, PreHandleResp: &bridgev2.MatrixReactionPreResponse{Emoji: joinThreadReaction}})
	if err != nil || !result.Metadata.(*ReactionMetadata).ThreadJoin || joined != 1 {
		t.Fatalf("join reaction failed: %#v %v", result, err)
	}
}

func TestCrossRoomRepliesRequireOptInAndSelectedDestination(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123"}, {ID: "124"}}
	c := testClient(t, config, nil)
	p := testPortal(t, c)
	target := &database.Portal{PortalKey: c.portalKey("124"), MXID: "!other:example.org", Metadata: &PortalMetadata{GuildID: "789"}}
	if err := c.Main.Bridge.DB.Portal.Insert(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	msg := &discordgo.Message{ID: "555", ChannelID: "123", Content: "reply", Author: &discordgo.User{ID: "999", Username: "Alice"}, MessageReference: &discordgo.MessageReference{MessageID: "444", ChannelID: "124"}}
	converted, err := c.convertMessage(context.Background(), p, &mediaTestAPI{}, msg, "")
	if err != nil || converted.ReplyTo != nil {
		t.Fatal("cross-room reply did not honor default opt-out")
	}
	c.Main.Bridge.Config.CrossRoomReplies = true
	converted, err = c.convertMessage(context.Background(), p, &mediaTestAPI{}, msg, "")
	if err != nil || converted.ReplyTo == nil || converted.ReplyToRoom.ID != "124" {
		t.Fatal("selected cross-room reply was lost")
	}
	c.Main.Config.Channels = c.Main.Config.Channels[:1]
	converted, err = c.convertMessage(context.Background(), p, &mediaTestAPI{}, msg, "")
	if err != nil || converted.ReplyTo != nil {
		t.Fatal("cross-room reply crossed the channel selection boundary")
	}
}

func TestGatewayStateSnapshotsDuringUpdates(t *testing.T) {
	config := defaultConfig(t)
	config.Guilds = []GuildConfig{{ID: "789", Mode: "everything"}}
	config.GuildSpaces = false
	c := testClient(t, config, nil)
	_ = c.Session.State.ChannelAdd(&discordgo.Channel{ID: "124", GuildID: "789", Type: discordgo.ChannelTypeGuildText, Name: "before"})
	p := &bridgev2.Portal{Portal: &database.Portal{PortalKey: c.portalKey("124"), Metadata: &PortalMetadata{}}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			_ = c.Session.State.ChannelAdd(&discordgo.Channel{ID: "124", GuildID: "789", Type: discordgo.ChannelTypeGuildText, Name: fmt.Sprint(i)})
			_ = c.Session.State.GuildAdd(&discordgo.Guild{ID: "789", Name: fmt.Sprint(i)})
		}
	}()
	for i := 0; i < 500; i++ {
		if !c.allowed("124") {
			t.Fatal("selected channel became unavailable")
		}
		if _, err := c.GetChatInfo(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if _, err := cachedGuild(c.Session.State, "789"); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}

func TestAttachmentDescriptionsAndVoiceMetadata(t *testing.T) {
	config := defaultConfig(t)
	c := testClient(t, config, func(*http.Request) (*http.Response, error) { return response(200, "audio") })
	p := testPortal(t, c)
	part, err := c.mediaPart(context.Background(), p, &mediaTestAPI{}, &discordgo.MessageAttachment{ID: "999", URL: "https://cdn.discordapp.com/attachments/123/999/audio.ogg", Filename: "SPOILER_audio.ogg", Description: "Voice caption", ContentType: "audio/ogg", DurationSeconds: 2.5, Waveform: []byte{1}}, "999", &MessageMetadata{})
	if err != nil || part.Content.Body != "Voice caption" || part.Content.FileName != "SPOILER_audio.ogg" || part.Content.MSC3245Voice == nil || part.Content.MSC1767Audio.Duration != 2500 || part.Extra["page.codeberg.everypizza.msc4193.spoiler"] != true {
		t.Fatalf("attachment metadata was lost: %#v %v", part, err)
	}
}

func TestPersonalDMSpaceIsAccountScoped(t *testing.T) {
	config := defaultConfig(t)
	config.PersonalAccounts = true
	c := testClient(t, config, nil)
	c.Session.IsUser = true
	ch := &discordgo.Channel{ID: "777", Type: discordgo.ChannelTypeDM, Recipients: []*discordgo.User{{ID: "999", Username: "Alice"}}}
	_ = c.Session.State.ChannelAdd(ch)
	p := &bridgev2.Portal{Portal: &database.Portal{PortalKey: c.portalKey(ch.ID), Metadata: &PortalMetadata{}}}
	info, err := c.GetChatInfo(context.Background(), p)
	if err != nil || info.ParentID == nil || *info.ParentID != "dms:456" || p.Receiver != "456" {
		t.Fatalf("DM grouping was lost: %#v %v", info, err)
	}
	space := &bridgev2.Portal{Portal: &database.Portal{PortalKey: c.portalKey("dms:456"), Metadata: &PortalMetadata{}}}
	if _, err := c.GetChatInfo(context.Background(), space); err != nil {
		t.Fatal(err)
	}
	c.UserLogin.ID = "888"
	if _, err := c.GetChatInfo(context.Background(), space); err == nil {
		t.Fatal("another account could access the DM space")
	}
}

func TestLegacySystemMessageAndBotHistory(t *testing.T) {
	if !bridgeMessageType(discordgo.MessageTypeThreadCreated) || !bridgeMessageType(discordgo.MessageTypeContextMenuCommand) || bridgeMessageType(discordgo.MessageTypeChannelNameChange) {
		t.Fatal("legacy message type handling changed")
	}
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123", Sync: SyncConfig{Backfill: ptr.Ptr(true)}}}
	c := testClient(t, config, func(*http.Request) (*http.Response, error) {
		return response(200, `[{"id":"555","channel_id":"123","content":"external bot message","author":{"id":"456","bot":true},"timestamp":"2026-10-01T12:00:00Z"}]`)
	})
	p := testPortal(t, c)
	result, err := c.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{Portal: p, Count: 1})
	if err != nil || len(result.Messages) != 1 || !result.Messages[0].Sender.IsFromMe {
		t.Fatal("bot messages from outside the bridge were dropped")
	}
}
