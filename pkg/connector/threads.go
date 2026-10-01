// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func (c *DiscordClient) ensureThread(ctx context.Context, portal *bridgev2.Portal, root *database.Message, text string) (string, error) {
	if !enabled(c.Main.Config.syncFor(string(portal.ID)).Threads) {
		return "", fmt.Errorf("thread sync is disabled")
	}
	tid := string(root.ID)
	if root.ThreadRoot != "" {
		tid = string(root.ThreadRoot)
	}
	c.Main.threadMu.Lock()
	defer c.Main.threadMu.Unlock()
	if ch, err := c.channel(ctx, tid); err == nil {
		if !isThread(ch) || ch.ParentID != string(portal.ID) {
			return "", fmt.Errorf("thread belongs to another channel")
		}
		return ch.ID, nil
	}
	if root.ThreadRoot != "" || messageChannel(root, portal) != string(portal.ID) {
		return "", fmt.Errorf("thread no longer exists or is inaccessible")
	}
	name := strings.Join(strings.Fields(text), " ")
	if name == "" {
		name = "Matrix thread"
	}
	if len([]rune(name)) > 40 {
		name = string([]rune(name)[:40])
	}
	ch, err := c.Session.MessageThreadStartComplex(string(portal.ID), tid, &discordgo.ThreadStart{Name: name, AutoArchiveDuration: 1440, Type: discordgo.ChannelTypeGuildPublicThread}, discordgo.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("failed to create Discord thread; check thread permissions")
	}
	_ = c.Session.State.ChannelAdd(ch)
	return ch.ID, nil
}

func (c *DiscordClient) syncThread(ch *discordgo.Channel) {
	ctx := c.Main.Bridge.BackgroundCtx
	if ch == nil || !isThread(ch) || !c.allowed(ch.ParentID) || !enabled(c.Main.Config.syncFor(ch.ParentID).Threads) {
		return
	}
	if c.Main.Bridge.DB == nil {
		return
	}
	// Queue the root before the notice and history so relations have an anchor.
	if root, err := c.Session.ChannelMessage(ch.ParentID, ch.ID, discordgo.WithContext(ctx)); err == nil {
		root.Flags |= discordgo.MessageFlagsHasThread
		c.onMessage(root, false)
	}
	if !c.Main.Bridge.Config.Backfill.Enabled || !enabled(c.Main.Config.syncFor(ch.ParentID).Backfill) {
		return
	}
	portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, c.portalKey(ch.ParentID))
	if err != nil || portal == nil {
		return
	}
	anchor, _ := c.Main.Bridge.DB.Message.GetLastThreadMessage(ctx, portal.PortalKey, networkid.MessageID(ch.ID))
	if anchor != nil {
		if meta, ok := anchor.Metadata.(*MessageMetadata); ok && meta.ThreadNotice != "" {
			anchor = nil
		}
	}
	count := c.Main.Bridge.Config.Backfill.Threads.MaxInitialMessages
	if count <= 0 {
		return
	}
	var messages []*bridgev2.BackfillMessage
	params := bridgev2.FetchMessagesParams{Portal: portal, ThreadRoot: networkid.MessageID(ch.ID), AnchorMessage: anchor, Forward: anchor != nil, Count: count}
	remaining := count
	for remaining > 0 {
		params.Count = min(remaining, 100)
		resp, err := c.FetchMessages(ctx, params)
		if err != nil {
			return
		}
		if params.Forward {
			messages = append(messages, resp.Messages...)
		} else {
			messages = append(resp.Messages, messages...)
		}
		remaining -= params.Count
		if !resp.HasMore || resp.Cursor == params.Cursor {
			break
		}
		if params.Forward {
			if len(resp.Messages) == 0 {
				break
			}
			last := resp.Messages[len(resp.Messages)-1]
			params.AnchorMessage = &database.Message{ID: last.ID}
		} else {
			params.Cursor = resp.Cursor
		}
	}
	for _, msg := range messages {
		c.UserLogin.QueueRemoteEvent(&simplevent.PreConvertedMessage{EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventMessage, PortalKey: portal.PortalKey, Sender: msg.Sender, Timestamp: msg.Timestamp}, ID: msg.ID, Data: msg.ConvertedMessage})
	}
}

var _ bridgev2.ReadReceiptHandlingNetworkAPI = (*DiscordClient)(nil)

func (c *DiscordClient) HandleMatrixReadReceipt(ctx context.Context, msg *bridgev2.MatrixReadReceipt) error {
	if !c.allowed(string(msg.Portal.ID)) {
		return nil
	}
	if c.Main.Config.AutojoinThreads && enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).Threads) {
		threadID := ""
		if msg.ExactMessage != nil {
			threadID = string(msg.ExactMessage.ThreadRoot)
		}
		if threadID == "" && msg.Receipt.ThreadID != "" && msg.Receipt.ThreadID != event.ReadReceiptThreadMain && c.Main.Bridge.DB != nil {
			if root, _ := c.Main.Bridge.DB.Message.GetPartByMXID(ctx, id.EventID(msg.Receipt.ThreadID)); root != nil && root.Room == msg.Portal.PortalKey {
				threadID = string(root.ID)
			}
		}
		if threadID != "" {
			if ch, err := c.channel(ctx, threadID); err == nil && isThread(ch) && ch.ParentID == string(msg.Portal.ID) {
				if err := c.Session.ThreadJoin(ch.ID, discordgo.WithContext(ctx)); err == nil {
					go c.syncThread(ch)
				}
			}
		}
	}
	if !c.Session.IsUser || !enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).ReadReceipts) {
		return nil
	}
	target := msg.ExactMessage
	if target == nil && c.Main.Bridge.DB != nil {
		target, _ = c.Main.Bridge.DB.Message.GetLastPartAtOrBeforeTime(ctx, msg.Portal.PortalKey, msg.ReadUpTo)
	}
	if target == nil {
		return nil
	}
	if meta, ok := target.Metadata.(*MessageMetadata); ok && meta.ThreadNotice != "" {
		return nil
	}
	_, err := c.Session.ChannelMessageAckNoToken(messageChannel(target, msg.Portal), string(target.ID), discordgo.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("failed to acknowledge Discord message")
	}
	return nil
}

type remoteReadReceipt struct {
	simplevent.EventMeta
	target networkid.MessageID
}

func (r *remoteReadReceipt) GetLastReceiptTarget() networkid.MessageID { return r.target }
func (r *remoteReadReceipt) GetReceiptTargets() []networkid.MessageID  { return nil }
func (r *remoteReadReceipt) GetReadUpTo() time.Time                    { return time.Time{} }
