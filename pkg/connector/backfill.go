// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

var _ bridgev2.BackfillingNetworkAPI = (*DiscordClient)(nil)

func (c *DiscordClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	channelID := string(params.Portal.ID)
	sc := c.Main.Config.syncFor(channelID)
	if !c.allowed(channelID) || !enabled(sc.Backfill) || !enabled(sc.Messages) {
		return &bridgev2.FetchMessagesResponse{}, nil
	}
	threadID := ""
	if params.ThreadRoot != "" {
		if !enabled(sc.Threads) {
			return &bridgev2.FetchMessagesResponse{}, nil
		}
		threadID = string(params.ThreadRoot)
		ch, err := c.channel(ctx, threadID)
		if err != nil || !isThread(ch) || ch.ParentID != channelID {
			return nil, fmt.Errorf("invalid thread history target")
		}
		channelID = threadID
	}
	count := min(max(params.Count, 1), 100)
	var before, after string
	if params.Forward {
		if params.AnchorMessage != nil {
			after = string(params.AnchorMessage.ID)
		}
	} else if params.Cursor != "" {
		before = string(params.Cursor)
	} else if params.AnchorMessage != nil {
		before = string(params.AnchorMessage.ID)
	}
	messages, err := c.Session.ChannelMessages(channelID, count, before, after, "", discordgo.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Discord channel history")
	}
	result := &bridgev2.FetchMessagesResponse{Forward: params.Forward, HasMore: len(messages) == count, AggressiveDeduplication: true}
	slices.SortFunc(messages, func(a, b *discordgo.Message) int {
		return cmp.Compare(a.Timestamp.UnixMilli(), b.Timestamp.UnixMilli())
	})
	if len(messages) > 0 {
		result.Cursor = networkid.PaginationCursor(messages[0].ID)
	}
	for _, msg := range messages {
		if msg.Author == nil || c.fromOwnWebhook(msg.WebhookID) {
			continue
		}
		if !bridgeMessageType(msg.Type) {
			continue
		}
		converted, err := c.convertMessage(ctx, params.Portal, c.Main.Bridge.Bot, msg, threadID)
		if err != nil {
			return nil, err
		}
		if len(converted.Parts) == 0 {
			continue
		}
		result.Messages = append(result.Messages, &bridgev2.BackfillMessage{ConvertedMessage: converted, ID: networkid.MessageID(msg.ID),
			Sender: c.messageSender(msg.Author.ID), Timestamp: msg.Timestamp, ShouldBackfillThread: threadID == "" && enabled(sc.Threads) && msg.Flags&discordgo.MessageFlagsHasThread != 0})
	}
	return result, nil
}
