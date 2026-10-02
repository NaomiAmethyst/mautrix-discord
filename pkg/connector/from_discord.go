// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"go.mau.fi/util/variationselector"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
)

func (c *DiscordClient) registerEvents() {
	c.on(func(_ *discordgo.Session, ready *discordgo.Ready) {
		if c.loggedOut.Load() {
			return
		}
		c.gatewayConnected.Store(true)
		c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
		go c.onReady(ready)
	})
	c.on(func(_ *discordgo.Session, _ *discordgo.Resumed) {
		if c.loggedOut.Load() {
			return
		}
		c.gatewayConnected.Store(true)
		c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
	})
	c.on(func(_ *discordgo.Session, _ *discordgo.Disconnect) {
		c.gatewayConnected.Store(false)
		if !c.loggedOut.Load() {
			c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateTransientDisconnect})
		}
	})
	c.on(func(_ *discordgo.Session, _ *discordgo.InvalidAuth) {
		c.loggedOut.Store(true)
		c.Main.clients.Delete(c.UserLogin.ID)
		c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: "discord-invalid-token", Message: "Discord rejected the stored token. Log in again."})
		go c.Disconnect()
	})
	c.on(func(_ *discordgo.Session, evt *discordgo.MessageCreate) { c.onMessage(evt.Message, false) })
	c.on(func(_ *discordgo.Session, evt *discordgo.MessageUpdate) { c.onMessage(evt.Message, true) })
	c.on(func(_ *discordgo.Session, evt *discordgo.MessageDelete) { c.onDelete(evt.ChannelID, evt.ID) })
	c.on(func(_ *discordgo.Session, evt *discordgo.MessageDeleteBulk) {
		for _, messageID := range evt.Messages {
			c.onDelete(evt.ChannelID, messageID)
		}
	})
	c.on(func(_ *discordgo.Session, evt *discordgo.MessageReactionAdd) {
		c.onReaction(evt.MessageReaction, false)
	})
	c.on(func(_ *discordgo.Session, evt *discordgo.MessageReactionRemove) {
		c.onReaction(evt.MessageReaction, true)
	})
	c.on(func(_ *discordgo.Session, evt *discordgo.TypingStart) {
		meta, _ := c.eventMeta(c.Main.Bridge.BackgroundCtx, evt.ChannelID, evt.UserID, time.Unix(int64(evt.Timestamp), 0), bridgev2.RemoteEventTyping)
		if meta.PortalKey.ID == "" || !enabled(c.Main.Config.syncFor(string(meta.PortalKey.ID)).Typing) || evt.UserID == string(c.UserLogin.ID) {
			return
		}
		meta.CreatePortal = false
		c.UserLogin.QueueRemoteEvent(&simplevent.Typing{EventMeta: meta, Timeout: 8 * time.Second})
	})
	c.on(func(_ *discordgo.Session, evt *discordgo.ThreadCreate) { go c.syncThread(evt.Channel) })
	c.on(func(_ *discordgo.Session, evt *discordgo.ThreadListSync) {
		for _, ch := range evt.Threads {
			go c.syncThread(ch)
		}
	})
	c.on(func(_ *discordgo.Session, evt *discordgo.MessageAck) { c.onReadAck(evt.ChannelID, evt.MessageID) })
	c.on(func(_ *discordgo.Session, evt *discordgo.InteractionSuccess) {
		if pending, ok := c.interactions.LoadAndDelete(evt.Nonce); ok {
			pending.(*commands.Event).React("✅")
		}
	})
	c.on(func(_ *discordgo.Session, evt *discordgo.ChannelCreate) {
		c.queueChannel(evt.Channel, c.canCreate(evt.ID))
	})
	c.on(func(_ *discordgo.Session, evt *discordgo.ChannelDelete) { c.onChannelDelete(evt.Channel) })
	c.on(func(_ *discordgo.Session, evt *discordgo.GuildCreate) { go c.syncGuild(evt.Guild) })
	c.on(func(_ *discordgo.Session, evt *discordgo.GuildDelete) {
		if !evt.Unavailable {
			go c.guildLeft(c.Main.Bridge.BackgroundCtx, evt.ID)
		}
	})
	c.on(func(_ *discordgo.Session, evt *discordgo.GuildUpdate) { go c.syncGuild(evt.Guild) })
	c.on(func(_ *discordgo.Session, evt *discordgo.RelationshipAdd) {
		c.updateRelationship(evt.Relationship, false)
	})
	c.on(func(_ *discordgo.Session, evt *discordgo.RelationshipUpdate) {
		c.updateRelationship(evt.Relationship, false)
	})
	c.on(func(_ *discordgo.Session, evt *discordgo.RelationshipRemove) {
		c.updateRelationship(evt.Relationship, true)
	})
	c.on(func(_ *discordgo.Session, evt *discordgo.ChannelUpdate) {
		if !c.allowed(evt.ID) {
			return
		}
		sc := c.Main.Config.syncFor(evt.ID)
		if enabled(sc.RoomName) || enabled(sc.RoomTopic) || enabled(sc.RoomAvatar) || isPrivate(evt.Channel) {
			c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatResync,
				PortalKey: c.portalKey(evt.ID)}, GetChatInfoFunc: c.GetChatInfo})
		}
	})
	c.startDispatch()
}

func (c *DiscordClient) fromOwnWebhook(id string) bool {
	if id == "" {
		return false
	}
	_, ok := c.Main.ownWebhooks.Load(id)
	if ok {
		return true
	}
	for _, ch := range c.Main.Config.Channels {
		if ch.WebhookURL != "" {
			own, _, _ := parseWebhook(ch.WebhookURL)
			if own == id {
				return true
			}
		}
	}
	return false
}

func (c *DiscordClient) onMessage(message *discordgo.Message, edit bool) {
	if message == nil {
		return
	}
	ctx := c.Main.Bridge.BackgroundCtx
	meta, threadID := c.eventMeta(ctx, message.ChannelID, "", message.Timestamp, bridgev2.RemoteEventMessage)
	if meta.PortalKey.ID == "" {
		return
	}
	sc := c.Main.Config.syncFor(string(meta.PortalKey.ID))
	if !enabled(sc.Messages) || (edit && !enabled(sc.Edits)) || c.fromOwnWebhook(message.WebhookID) {
		return
	}
	// MESSAGE_UPDATE is a patch. Fetch the complete message to avoid interpreting
	// omitted attachments/content as removals or losing the author of the edit.
	if edit {
		full, err := c.Session.ChannelMessage(message.ChannelID, message.ID, discordgo.WithContext(ctx))
		if err != nil {
			c.UserLogin.Log.Warn().Msg("Failed to fetch edited Discord message")
			return
		}
		message = full
		if c.fromOwnWebhook(message.WebhookID) {
			return
		}
		meta.Type = bridgev2.RemoteEventEdit
		meta.CreatePortal = false
	}
	if message.Author == nil {
		return
	}
	if !bridgeMessageType(message.Type) {
		return
	}
	meta.Sender = c.messageSender(message.Author.ID)
	if edit && message.EditedTimestamp != nil {
		meta.Timestamp = *message.EditedTimestamp
	}
	if enabled(sc.UserProfiles) {
		meta.PreHandleFunc = func(ctx context.Context, _ *bridgev2.Portal) {
			ghost, err := c.Main.Bridge.GetGhostByID(ctx, networkid.UserID(message.Author.ID))
			if err == nil {
				info := c.userInfo(message.Author)
				name := renderName(c.Main.Config.DisplaynameTemplate, c.profileName(message.Author), struct {
					*discordgo.User
					Webhook     bool
					Application bool
				}{message.Author, message.WebhookID != "", message.ApplicationID != ""})
				info.Name = &name
				ghost.UpdateInfo(ctx, info)
			}
		}
	}
	if message.Flags&discordgo.MessageFlagsHasThread != 0 && enabled(sc.Threads) {
		meta.PostHandleFunc = func(ctx context.Context, portal *bridgev2.Portal) {
			go c.queueThreadNotice(c.Main.Bridge.BackgroundCtx, portal, message.ID, message.Timestamp)
		}
	}
	msg := message
	c.UserLogin.QueueRemoteEvent(&simplevent.Message[*discordgo.Message]{EventMeta: meta, ID: networkid.MessageID(msg.ID),
		TargetMessage: networkid.MessageID(msg.ID), Data: msg, TransactionID: networkid.TransactionID(msg.Nonce),
		ConvertMessageFunc: func(ctx context.Context, p *bridgev2.Portal, intent bridgev2.MatrixAPI, data *discordgo.Message) (*bridgev2.ConvertedMessage, error) {
			return c.convertMessage(ctx, p, intent, data, threadID)
		},
		ConvertEditFunc: func(ctx context.Context, p *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, data *discordgo.Message) (*bridgev2.ConvertedEdit, error) {
			converted, err := c.convertMessage(ctx, p, intent, data, threadID)
			if err != nil {
				return nil, err
			}
			return convertEdit(converted, existing), nil
		},
		HandleExistingFunc: func(context.Context, *bridgev2.Portal, bridgev2.MatrixAPI, []*database.Message, *discordgo.Message) (bridgev2.UpsertResult, error) {
			return bridgev2.UpsertResult{}, nil
		},
	})
}

func convertEdit(converted *bridgev2.ConvertedMessage, existing []*database.Message) *bridgev2.ConvertedEdit {
	result := &bridgev2.ConvertedEdit{}
	parts := make(map[networkid.PartID]*bridgev2.ConvertedMessagePart, len(converted.Parts))
	for _, part := range converted.Parts {
		parts[part.ID] = part
	}
	for _, previous := range existing {
		if part := parts[previous.PartID]; part != nil {
			result.ModifiedParts = append(result.ModifiedParts, part.ToEditPart(previous))
			delete(parts, previous.PartID)
		} else {
			result.DeletedParts = append(result.DeletedParts, previous)
		}
	}
	for _, part := range converted.Parts {
		if parts[part.ID] != nil {
			if result.AddedParts == nil {
				result.AddedParts = &bridgev2.ConvertedMessage{}
			}
			result.AddedParts.Parts = append(result.AddedParts.Parts, part)
		}
	}
	return result
}

func (c *DiscordClient) convertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, message *discordgo.Message, threadID string) (*bridgev2.ConvertedMessage, error) {
	sc := c.Main.Config.syncFor(string(portal.ID))
	ctx = c.messageContext(ctx, message)
	result := &bridgev2.ConvertedMessage{}
	if threadID != "" {
		root := networkid.MessageID(threadID)
		result.ThreadRoot = &root
	}
	if ref := message.MessageReference; ref != nil && ref.MessageID != "" && (ref.ChannelID == message.ChannelID || ref.ChannelID == string(portal.ID)) {
		result.ReplyTo = &networkid.MessageOptionalPartID{MessageID: networkid.MessageID(ref.MessageID)}
	}
	meta := &MessageMetadata{ChannelID: message.ChannelID, WebhookID: message.WebhookID, MessageID: message.ID, Excerpt: replyExcerpt(message.Content)}
	text := message.Content
	if text != "" {
		content := c.discordText(ctx, portal, text)
		result.Parts = append(result.Parts, &bridgev2.ConvertedMessagePart{ID: "", Type: event.EventMessage, Content: &content, DBMetadata: meta})
	}
	if err := c.convertEmbeds(ctx, portal, intent, message, result, meta); err != nil {
		return nil, err
	}
	if enabled(sc.Attachments) {
		for _, attachment := range message.Attachments {
			part, err := c.mediaPart(ctx, portal, intent, attachment, networkid.PartID(attachment.ID), meta)
			if err != nil {
				return nil, err
			}
			result.Parts = append(result.Parts, part)
		}
	}
	if enabled(sc.Stickers) {
		for _, sticker := range message.StickerItems {
			if sticker.FormatType == discordgo.StickerFormatTypeLottie {
				part, err := c.lottieSticker(ctx, portal, intent, sticker, meta)
				if err != nil {
					return nil, err
				}
				result.Parts = append(result.Parts, part)
				continue
			}
			ext := ".png"
			if sticker.FormatType == discordgo.StickerFormatTypeGIF {
				ext = ".gif"
			}
			mime := "image/png"
			if sticker.FormatType == discordgo.StickerFormatTypeGIF {
				mime = "image/gif"
			} else if sticker.FormatType == discordgo.StickerFormatTypeAPNG {
				mime = "image/apng"
			}
			attachment := &discordgo.MessageAttachment{ContentType: mime, Width: 160, Height: 160, ID: sticker.ID, Filename: sticker.Name + ext, URL: "https://cdn.discordapp.com/stickers/" + sticker.ID + ext}
			part, err := c.mediaPart(ctx, portal, intent, attachment, networkid.PartID("sticker-"+sticker.ID), meta)
			if err != nil {
				return nil, err
			}
			part.Type = event.EventSticker
			result.Parts = append(result.Parts, part)
		}
	}
	if len(result.Parts) == 0 && message.Thread != nil {
		result.Parts = append(result.Parts, noticePart("", "Created a thread: "+message.Thread.Name, meta))
	}
	c.addMessageProfiles(ctx, portal, message, result)
	c.convertReply(ctx, portal, message, result)
	return result, nil
}

func noticePart(partID networkid.PartID, text string, meta *MessageMetadata) *bridgev2.ConvertedMessagePart {
	return &bridgev2.ConvertedMessagePart{ID: partID, Type: event.EventMessage, Content: &event.MessageEventContent{MsgType: event.MsgNotice, Body: text}, DBMetadata: meta}
}
func (c *DiscordClient) mediaPart(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, attachment *discordgo.MessageAttachment, partID networkid.PartID, meta *MessageMetadata) (*bridgev2.ConvertedMessagePart, error) {
	if int64(attachment.Size) > c.Main.fileLimit() {
		return noticePart(partID, "[Attachment exceeds transfer limit: "+attachment.Filename+"]", meta), nil
	}
	if c.Main.directMedia.Load() && !c.Main.roomEncrypted(ctx, portal) && attachment.ContentType != "" {
		remote := remoteMedia{URL: attachment.URL}
		if strings.Contains(attachment.URL, "/attachments/") {
			remote.Channel, remote.Message, remote.Attachment = meta.ChannelID, meta.MessageID, attachment.ID
		}
		uri, err := c.Main.directURI(ctx, remote)
		if err == nil {
			content := &event.MessageEventContent{MsgType: mediaType(attachment.ContentType), Body: safeFileName(attachment.Filename), URL: uri, Info: &event.FileInfo{MimeType: attachment.ContentType, Size: attachment.Size, Width: attachment.Width, Height: attachment.Height}}
			part := &bridgev2.ConvertedMessagePart{ID: partID, Type: event.EventMessage, Content: content, DBMetadata: meta}
			decorateAttachment(part, attachment)
			return part, nil
		}
	}
	name := safeFileName(attachment.Filename)
	cached, err := c.Main.transferMedia(ctx, intent, portal, attachment.URL, name, attachment.ContentType)
	if errors.Is(err, errMediaDownload) {
		return noticePart(partID, "[Attachment unavailable: "+attachment.Filename+"]", meta), nil
	}
	if err != nil {
		return nil, err
	}
	if int64(cached.Size) > c.Main.fileLimit() {
		return noticePart(partID, "[Attachment exceeds transfer limit: "+attachment.Filename+"]", meta), nil
	}
	uri, file, mimeType := cached.URI, cached.File, cached.MIME
	msgType := event.MsgFile
	if strings.HasPrefix(mimeType, "image/") {
		msgType = event.MsgImage
	} else if strings.HasPrefix(mimeType, "video/") {
		msgType = event.MsgVideo
	} else if strings.HasPrefix(mimeType, "audio/") {
		msgType = event.MsgAudio
	}
	content := &event.MessageEventContent{MsgType: msgType, Body: name, FileName: name, URL: uri, File: file,
		Info: &event.FileInfo{MimeType: mimeType, Size: cached.Size, Width: attachment.Width, Height: attachment.Height}}
	if file != nil {
		content.URL = ""
	}
	part := &bridgev2.ConvertedMessagePart{ID: partID, Type: event.EventMessage, Content: content, DBMetadata: meta}
	decorateAttachment(part, attachment)
	return part, nil
}

func (c *DiscordClient) onDelete(channelID, messageID string) {
	meta, _ := c.eventMeta(c.Main.Bridge.BackgroundCtx, channelID, "", time.Time{}, bridgev2.RemoteEventMessageRemove)
	meta.CreatePortal = false
	if meta.PortalKey.ID == "" || !enabled(c.Main.Config.syncFor(string(meta.PortalKey.ID)).Deletes) {
		return
	}
	c.UserLogin.QueueRemoteEvent(&simplevent.MessageRemove{EventMeta: meta, TargetMessage: networkid.MessageID(messageID)})
}
func (c *DiscordClient) onReaction(reaction *discordgo.MessageReaction, remove bool) {
	if reaction == nil {
		return
	}
	meta, _ := c.eventMeta(c.Main.Bridge.BackgroundCtx, reaction.ChannelID, reaction.UserID, time.Time{}, bridgev2.RemoteEventReaction)
	meta.CreatePortal = false
	if meta.PortalKey.ID == "" || !enabled(c.Main.Config.syncFor(string(meta.PortalKey.ID)).Reactions) {
		return
	}
	if remove {
		meta.Type = bridgev2.RemoteEventReactionRemove
	}
	emojiID := reaction.Emoji.ID
	emoji := reaction.Emoji.Name
	if emojiID == "" {
		emojiID = variationselector.Remove(emoji)
		emoji = variationselector.Add(emoji)
	} else {
		emoji = ":" + emoji + ":"
		if !remove && c.Main.Config.CustomEmojiReactions {
			if uri, err := c.emojiMXC(c.Main.Bridge.BackgroundCtx, reaction.Emoji.ID, reaction.Emoji.Name, reaction.Emoji.Animated); err == nil {
				emoji = string(uri)
			}
		}
	}
	discordEmoji := reaction.Emoji.Name
	if reaction.Emoji.ID != "" {
		discordEmoji += ":" + reaction.Emoji.ID
	}
	c.UserLogin.QueueRemoteEvent(&simplevent.Reaction{EventMeta: meta, TargetMessage: networkid.MessageID(reaction.MessageID), EmojiID: networkid.EmojiID(emojiID), Emoji: emoji,
		ExtraContent: reactionExtra(reaction.Emoji, emoji), ReactionDBMeta: &ReactionMetadata{ChannelID: reaction.ChannelID, DiscordEmoji: discordEmoji}})
}

func reactionExtra(emoji discordgo.Emoji, mxc string) map[string]any {
	if emoji.ID == "" || !strings.HasPrefix(mxc, "mxc://") {
		return nil
	}
	return map[string]any{"fi.mau.discord.reaction": map[string]any{"id": emoji.ID, "name": emoji.Name, "mxc": mxc}, "com.beeper.reaction.shortcode": ":" + emoji.Name + ":"}
}

func mediaType(mime string) event.MessageType {
	if strings.HasPrefix(mime, "image/") {
		return event.MsgImage
	}
	if strings.HasPrefix(mime, "video/") {
		return event.MsgVideo
	}
	if strings.HasPrefix(mime, "audio/") {
		return event.MsgAudio
	}
	return event.MsgFile
}

func decorateAttachment(part *bridgev2.ConvertedMessagePart, attachment *discordgo.MessageAttachment) {
	if attachment.Description != "" {
		part.Content.Body = attachment.Description
		part.Content.FileName = safeFileName(attachment.Filename)
	}
	if strings.HasPrefix(attachment.Filename, "SPOILER_") {
		part.Extra = map[string]any{"page.codeberg.everypizza.msc4193.spoiler": true}
	}
	if part.Content.MsgType == event.MsgAudio {
		part.Content.Info.Duration = int(attachment.DurationSeconds * 1000)
		if attachment.Waveform != nil {
			part.Content.MSC1767Audio = &event.MSC1767Audio{Duration: part.Content.Info.Duration}
			part.Content.MSC3245Voice = &event.MSC3245Voice{}
		}
	}
}

func bridgeMessageType(kind discordgo.MessageType) bool {
	switch kind {
	case discordgo.MessageTypeChannelNameChange, discordgo.MessageTypeChannelIconChange, discordgo.MessageTypeChannelPinnedMessage:
		return false
	default:
		return true
	}
}
