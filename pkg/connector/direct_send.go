// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

func nonce() string { return strconv.FormatInt(time.Now().UnixNano(), 10) }
func (c *DiscordClient) directMessage(ctx context.Context, msg *bridgev2.MatrixMessage, text string, files []*discordgo.File) (*bridgev2.MatrixMessageResponse, error) {
	cid := string(msg.Portal.ID)
	if msg.ThreadRoot != nil {
		var err error
		cid, err = c.ensureThread(ctx, msg.Portal, msg.ThreadRoot, msg.Content.Body)
		if err != nil {
			return nil, err
		}
	}
	req := &discordgo.MessageSend{Content: text, Files: files, Nonce: nonce(), AllowedMentions: c.allowedMentions(ctx, msg.Portal, msg.Content, matrixSender(msg.Event), text)}
	if c.Main.Config.UseDiscordCDNUpload && len(files) > 0 {
		var err error
		req.Attachments, err = c.prepareAttachments(ctx, cid, files)
		if err != nil {
			return nil, err
		}
		req.Files = nil
		if msg.Event.Type == event.EventSticker {
			for _, attachment := range req.Attachments {
				attachment.Description = msg.Content.Body
			}
		}
	}
	if msg.ReplyTo != nil && validSnowflake(string(msg.ReplyTo.ID)) && messageChannel(msg.ReplyTo, msg.Portal) == cid {
		req.Reference = &discordgo.MessageReference{MessageID: string(msg.ReplyTo.ID), ChannelID: cid}
	}
	if !enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).Embeds) {
		flag := discordgo.MessageFlagsSuppressEmbeds
		req.Flags = &flag
	}
	txn := networkid.TransactionID(req.Nonce)
	msg.AddPendingToIgnore(txn)
	sent, err := c.Session.ChannelMessageSendComplex(cid, req, discordgo.WithContext(ctx))
	if err != nil || sent == nil {
		msg.RemovePending(txn)
		return nil, fmt.Errorf("Discord message send failed; check channel permissions")
	}
	return &bridgev2.MatrixMessageResponse{DB: &database.Message{ID: networkid.MessageID(sent.ID), SenderID: networkid.UserID(c.UserLogin.ID), Timestamp: sent.Timestamp, Metadata: &MessageMetadata{ChannelID: cid, Excerpt: replyExcerpt(msg.Content.Body)}}, RemovePending: txn}, nil
}
func (c *DiscordClient) directEdit(ctx context.Context, msg *bridgev2.MatrixEdit, text string, files []*discordgo.File) error {
	if err := c.checkMessageChannel(msg.Portal, msg.EditTarget); err != nil {
		return err
	}
	if msg.EditTarget.SenderID != networkid.UserID(c.UserLogin.ID) {
		return fmt.Errorf("only own Discord messages can be edited")
	}
	req := &discordgo.MessageEdit{ID: string(msg.EditTarget.ID), Channel: messageChannel(msg.EditTarget, msg.Portal), Content: &text, Files: files, AllowedMentions: c.allowedMentions(ctx, msg.Portal, msg.Content, matrixSender(msg.Event), text)}
	if c.Main.Config.UseDiscordCDNUpload && len(files) > 0 {
		attachments, err := c.prepareAttachments(ctx, req.Channel, files)
		if err != nil {
			return err
		}
		req.Attachments = &attachments
		req.Files = nil
	}
	_, err := c.Session.ChannelMessageEditComplex(req, discordgo.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("Discord message edit failed")
	}
	if meta, ok := msg.EditTarget.Metadata.(*MessageMetadata); ok {
		meta.Excerpt = replyExcerpt(msg.Content.Body)
	}
	return nil
}
