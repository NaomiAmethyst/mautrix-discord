// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
)

const joinThreadReaction = "join thread"

func (c *DiscordClient) queueThreadNotice(ctx context.Context, p *bridgev2.Portal, tid string, timestamp time.Time) {
	if c.Main.Bridge.DB == nil || !c.allowed(string(p.ID)) || !enabled(c.Main.Config.syncFor(string(p.ID)).Threads) {
		return
	}
	root, _ := c.Main.Bridge.DB.Message.GetFirstPartByID(ctx, p.Receiver, networkid.MessageID(tid))
	if root == nil {
		return
	}
	messageID := networkid.MessageID("thread-notice:" + tid)
	text := "Thread created. React with \"join thread\" to join it on Discord."
	if c.Main.Config.AutojoinThreads {
		text = "Thread created. Opening this thread will join it on Discord."
	}
	if timestamp.IsZero() {
		timestamp = root.Timestamp
	}
	meta := &MessageMetadata{ChannelID: tid, ThreadNotice: tid}
	c.UserLogin.QueueRemoteEvent(&simplevent.PreConvertedMessage{
		EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventMessage, PortalKey: p.PortalKey, Timestamp: timestamp,
			PostHandleFunc: func(ctx context.Context, p *bridgev2.Portal) {
				key := database.Key("discord.thread.notice.reaction." + string(p.MXID) + ":" + tid)
				if c.Main.Bridge.DB.KV.Get(ctx, key) != "" {
					return
				}
				msg, _ := c.Main.Bridge.DB.Message.GetFirstPartByID(ctx, p.Receiver, messageID)
				if msg == nil {
					return
				}
				_, err := c.Main.Bridge.Bot.SendMessage(ctx, p.MXID, event.EventReaction, &event.Content{Parsed: &event.ReactionEventContent{RelatesTo: event.RelatesTo{Type: event.RelAnnotation, EventID: msg.MXID, Key: joinThreadReaction}}}, nil)
				if err == nil {
					c.Main.Bridge.DB.KV.Set(ctx, key, "1")
				}
			}},
		ID:   messageID,
		Data: &bridgev2.ConvertedMessage{ThreadRoot: ptr.Ptr(networkid.MessageID(tid)), Parts: []*bridgev2.ConvertedMessagePart{noticePart("", text, meta)}},
	})
}

func (c *DiscordClient) joinThread(ctx context.Context, p *bridgev2.Portal, target *database.Message) error {
	if !enabled(c.Main.Config.syncFor(string(p.ID)).Threads) {
		return fmt.Errorf("thread sync is disabled")
	}
	tid := string(target.ID)
	if meta, ok := target.Metadata.(*MessageMetadata); ok && meta.ThreadNotice != "" {
		tid = meta.ThreadNotice
	}
	if target.ThreadRoot != "" {
		tid = string(target.ThreadRoot)
	}
	ch, err := c.channel(ctx, tid)
	if err != nil || !isThread(ch) || ch.ParentID != string(p.ID) {
		return fmt.Errorf("reaction target is not a thread in this channel")
	}
	if err := c.Session.ThreadJoin(ch.ID, discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("failed to join Discord thread")
	}
	go c.syncThread(ch)
	return nil
}
