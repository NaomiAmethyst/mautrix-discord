// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

var webhookReplyPattern = regexp.MustCompile(`^\*\*\[Replying to]\(https://discord.com/channels/(\d+)/(\d+)/(\d+)\)`)

func replyExcerpt(body string) string {
	line, _, _ := strings.Cut(body, "\n")
	chars := []rune(line)
	if len(chars) > 72 {
		return string(chars[:72]) + "…"
	}
	return line
}
func (c *DiscordClient) replyEmbed(ctx context.Context, msg *bridgev2.MatrixMessage, gid string) *discordgo.MessageEmbed {
	target := msg.ReplyTo
	if target == nil {
		return nil
	}
	body, name := "", string(target.SenderID)
	if meta, ok := target.Metadata.(*MessageMetadata); ok {
		body = meta.Excerpt
	}
	if body == "" && c.Main.Bridge.Bot != nil && target.MXID != "" {
		if evt, err := c.Main.Bridge.Bot.GetEvent(ctx, msg.Portal.MXID, target.MXID); err == nil {
			body = replyExcerpt(evt.Content.AsMessage().Body)
		}
	}
	if name == "" {
		name = "Matrix user"
	} else {
		name = "<@" + name + ">"
	}
	if ch, err := cachedChannel(c.Session.State, messageChannel(target, msg.Portal)); err == nil {
		gid = ch.GuildID
	}
	if gid == "" {
		gid = "@me"
	}
	link := fmt.Sprintf("https://discord.com/channels/%s/%s/%s", gid, messageChannel(target, msg.Portal), target.ID)
	return &discordgo.MessageEmbed{Description: fmt.Sprintf("**[Replying to](%s) %s**\n%s", link, name, escapeDiscordMarkdown(body))}
}
func (c *DiscordClient) convertReply(ctx context.Context, portal *bridgev2.Portal, message *discordgo.Message, result *bridgev2.ConvertedMessage) {
	ref := message.MessageReference
	if ref == nil && len(message.Embeds) > 0 {
		if match := webhookReplyPattern.FindStringSubmatch(message.Embeds[0].Description); match != nil && match[1] == metaGuild(portal) {
			ref = &discordgo.MessageReference{ChannelID: match[2], MessageID: match[3]}
		}
	}
	if ref != nil && ref.MessageID != "" {
		channelID := ref.ChannelID
		if channelID == "" {
			channelID = message.ChannelID
		}
		if channelID == message.ChannelID || channelID == string(portal.ID) {
			result.ReplyTo = &networkid.MessageOptionalPartID{MessageID: networkid.MessageID(ref.MessageID)}
		} else if c.Main.Bridge.Config.CrossRoomReplies && c.Main.Bridge.DB != nil {
			pid, _ := c.eventPortal(ctx, channelID)
			if pid != "" {
				key := c.portalKey(pid)
				p, _ := c.Main.Bridge.DB.Portal.GetByKey(ctx, key)
				if p != nil && p.MXID != "" {
					result.ReplyTo = &networkid.MessageOptionalPartID{MessageID: networkid.MessageID(ref.MessageID)}
					result.ReplyToRoom = key
				}
			}
		}
	}
	if message.Interaction != nil {
		for _, part := range result.Parts {
			if part.ID == "" {
				part.Content.EnsureHasHTML()
				part.Content.FormattedBody = "<blockquote>Used /" + html.EscapeString(message.Interaction.Name) + "</blockquote>" + part.Content.FormattedBody
				part.Content.Body = "Used /" + message.Interaction.Name + "\n" + part.Content.Body
				break
			}
		}
	}
	if message.Flags&discordgo.MessageFlagsEphemeral != 0 {
		for _, part := range result.Parts {
			if part.Extra == nil {
				part.Extra = map[string]any{}
			}
			part.Extra["fi.mau.discord.ephemeral"] = true
			part.Content.MsgType = event.MsgNotice
		}
	}
}
