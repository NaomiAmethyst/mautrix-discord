// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"os"
	"strings"
	"unicode/utf16"

	"github.com/bwmarrin/discordgo"
	"go.mau.fi/util/ptr"
	"go.mau.fi/util/variationselector"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var _ bridgev2.EditHandlingNetworkAPI = (*DiscordClient)(nil)
var _ bridgev2.RedactionHandlingNetworkAPI = (*DiscordClient)(nil)
var _ bridgev2.ReactionHandlingNetworkAPI = (*DiscordClient)(nil)
var _ bridgev2.TypingHandlingNetworkAPI = (*DiscordClient)(nil)
var _ bridgev2.RoomNameHandlingNetworkAPI = (*DiscordClient)(nil)
var _ bridgev2.RoomTopicHandlingNetworkAPI = (*DiscordClient)(nil)

func (c *DiscordClient) ensureWebhook(ctx context.Context, portal *bridgev2.Portal) (string, string, error) {
	c.Main.webhookMu.Lock()
	defer c.Main.webhookMu.Unlock()
	if !c.allowed(string(portal.ID)) {
		return "", "", fmt.Errorf("channel is not allowlisted for this login")
	}
	meta := portal.Metadata.(*PortalMetadata)
	config := c.Main.Config.channel(string(portal.ID))
	if config == nil {
		config = &ChannelConfig{}
	}
	if config.WebhookURL == "" && meta.WebhookID != "" && meta.WebhookToken != "" {
		c.Main.ownWebhooks.Store(meta.WebhookID, true)
		return meta.WebhookID, meta.WebhookToken, nil
	}
	ch, err := c.channel(ctx, string(portal.ID))
	if err != nil {
		return "", "", err
	}
	if !supportedChannel(ch) {
		return "", "", fmt.Errorf("channel does not support webhook relay")
	}
	targetID := ch.ID
	if isThread(ch) {
		targetID = ch.ParentID
	}
	var webhook *discordgo.Webhook
	if config.WebhookURL != "" {
		webhookID, token, err := parseWebhook(config.WebhookURL)
		if err != nil {
			return "", "", err
		}
		if meta.WebhookID == webhookID && meta.WebhookToken == token {
			c.Main.ownWebhooks.Store(webhookID, true)
			return webhookID, token, nil
		}
		webhook, err = c.Session.WebhookWithToken(webhookID, token, discordgo.WithContext(ctx))
		if err != nil {
			return "", "", fmt.Errorf("failed to validate configured webhook")
		}
		if webhook.ChannelID != targetID || webhook.Type != discordgo.WebhookTypeIncoming {
			return "", "", fmt.Errorf("configured webhook belongs to a different channel or is not an incoming webhook")
		}
		webhook.Token = token
	} else {
		if !c.Main.Config.CreateWebhooks {
			return "", "", fmt.Errorf("provide webhook_url or enable network.create_webhooks")
		}
		webhooks, err := c.Session.ChannelWebhooks(targetID, discordgo.WithContext(ctx))
		if err != nil {
			return "", "", fmt.Errorf("failed to list channel webhooks; bot needs Manage Webhooks")
		}
		for _, candidate := range webhooks {
			if candidate.Name == c.Main.Config.WebhookName && candidate.User != nil && candidate.User.ID == string(c.UserLogin.ID) && candidate.Token != "" && candidate.Type == discordgo.WebhookTypeIncoming {
				webhook = candidate
				break
			}
		}
		if webhook == nil {
			webhook, err = c.Session.WebhookCreate(targetID, c.Main.Config.WebhookName, "", discordgo.WithContext(ctx))
			if err != nil {
				return "", "", fmt.Errorf("failed to create webhook; bot needs Manage Webhooks")
			}
		}
	}
	meta.WebhookID, meta.WebhookToken = webhook.ID, webhook.Token
	meta.GuildID, meta.ParentID = ch.GuildID, ch.ParentID
	meta.IsThread = isThread(ch)
	c.Main.ownWebhooks.Store(webhook.ID, true)
	if err = portal.Save(ctx); err != nil {
		return "", "", err
	}
	return webhook.ID, webhook.Token, nil
}

func (c *DiscordClient) outgoingContent(ctx context.Context, portal *bridgev2.Portal, content *event.MessageEventContent, sticker bool) (string, []*discordgo.File, error) {
	sc := c.Main.Config.syncFor(string(portal.ID))
	if !c.allowed(string(portal.ID)) || !enabled(sc.Messages) {
		return "", nil, fmt.Errorf("message sync is disabled for this channel")
	}
	if err := c.requireDMAccess(string(portal.ID)); err != nil {
		return "", nil, err
	}
	text := c.matrixText(ctx, portal, content)
	if content.MsgType == event.MsgEmote {
		text = "* " + text
	}
	if content.MsgType.IsMedia() || content.URL != "" || content.File != nil {
		if (!sticker && !enabled(sc.Attachments)) || (sticker && !enabled(sc.Stickers)) {
			return "", nil, fmt.Errorf("attachment sync is disabled for this channel")
		}
		if content.Info != nil && int64(content.Info.Size) > c.Main.fileLimit() {
			return "", nil, fmt.Errorf("attachment exceeds the configured transfer limit")
		}
		uri := content.URL
		if content.File != nil {
			uri = content.File.URL
		}
		if uri == "" {
			return "", nil, fmt.Errorf("message has no attachment URL")
		}
		var data []byte
		err := c.Main.Bridge.Bot.DownloadMediaToFile(ctx, uri, content.File, false, func(file *os.File) error {
			stat, err := file.Stat()
			if err != nil {
				return err
			}
			if stat.Size() > c.Main.fileLimit() {
				return fmt.Errorf("attachment exceeds the configured transfer limit")
			}
			data, err = io.ReadAll(io.LimitReader(file, c.Main.fileLimit()+1))
			return err
		})
		if err != nil {
			return "", nil, fmt.Errorf("failed to download Matrix attachment: %w", err)
		}
		if int64(len(data)) > c.Main.fileLimit() {
			return "", nil, fmt.Errorf("attachment exceeds the configured transfer limit")
		}
		name := content.FileName
		if name == "" {
			name = content.Body
			text = ""
		} else if name == content.Body {
			text = ""
		}
		mimeType := "application/octet-stream"
		if content.Info != nil && content.Info.MimeType != "" {
			mimeType = content.Info.MimeType
		}
		if sticker {
			if extensions, _ := mime.ExtensionsByType(mimeType); len(extensions) > 0 {
				name = "sticker" + extensions[0]
			}
			text = ""
		}
		return text, []*discordgo.File{{Name: safeFileName(name), ContentType: mimeType, Reader: bytes.NewReader(data)}}, nil
	}
	if content.MsgType == event.MsgLocation {
		text += "\n" + content.GeoURI
	}
	return text, nil, nil
}

func (c *DiscordClient) senderProfile(ctx context.Context, msg *bridgev2.MatrixMessage) (name, avatarURL string) {
	if msg.OrigSender != nil {
		name = msg.OrigSender.FormattedName
		if name == "" {
			name = msg.OrigSender.Displayname
		}
		if name == "" {
			name = string(msg.OrigSender.UserID)
		}
		avatarURL = string(msg.OrigSender.AvatarURL)
	} else {
		name = string(msg.Event.Sender)
		member, err := c.Main.Bridge.Matrix.GetMemberInfo(ctx, msg.Portal.MXID, msg.Event.Sender)
		if err == nil && member != nil {
			if member.Displayname != "" {
				name = member.Displayname
			}
			avatarURL = string(member.AvatarURL)
		}
	}
	name = strings.ReplaceAll(strings.ReplaceAll(name, "@", "＠"), "`", "")
	name = strings.Map(func(r rune) rune {
		if r < 32 {
			return -1
		}
		return r
	}, name)
	if len([]rune(name)) > 80 {
		name = string([]rune(name)[:80])
	}
	if name == "" {
		name = "Matrix user"
	}
	if c.Main.Config.WebhookAvatars && avatarURL != "" {
		if media, ok := c.Main.Bridge.Matrix.(bridgev2.MatrixConnectorWithPublicMedia); ok {
			avatarURL = media.GetPublicMediaAddress(id.ContentURIString(avatarURL))
		} else {
			avatarURL = ""
		}
	} else {
		avatarURL = ""
	}
	return
}

func (c *DiscordClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if msg.Event.Type == event.EventSticker && !enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).Stickers) {
		return nil, fmt.Errorf("sticker sync is disabled")
	}
	text, files, err := c.outgoingContent(ctx, msg.Portal, msg.Content, msg.Event.Type == event.EventSticker)
	if err != nil {
		return nil, err
	}
	if len(utf16.Encode([]rune(text))) > 2000 {
		return nil, fmt.Errorf("Discord messages are limited to 2000 characters")
	}
	markSpoilers(files, msg.Event)
	if c.Session.IsUser && msg.OrigSender == nil {
		return c.directMessage(ctx, msg, text, files)
	}
	webhookID, token, err := c.ensureWebhook(ctx, msg.Portal)
	if err != nil {
		return nil, err
	}
	channelID := string(msg.Portal.ID)
	ch, err := c.channel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	var threadID string
	if isThread(ch) {
		threadID = channelID
	}
	if msg.ThreadRoot != nil && threadID == "" {
		if !enabled(c.Main.Config.syncFor(channelID).Threads) {
			return nil, fmt.Errorf("thread sync is disabled")
		}
		threadID, err = c.ensureThread(ctx, msg.Portal, msg.ThreadRoot, msg.Content.Body)
		if err != nil {
			return nil, err
		}
		channelID = threadID
	}
	if len(utf16.Encode([]rune(text))) > 2000 {
		return nil, fmt.Errorf("Discord webhook messages are limited to 2000 characters")
	}
	if text == "" && len(files) == 0 {
		return nil, fmt.Errorf("message contains no supported content")
	}
	name, avatar := c.senderProfile(ctx, msg)
	params := &discordgo.WebhookParams{Content: text, Username: name, AvatarURL: avatar, Files: files, AllowedMentions: c.allowedMentions(ctx, msg.Portal, msg.Content, matrixSender(msg.Event), text)}
	if msg.ReplyTo != nil && validSnowflake(string(msg.ReplyTo.ID)) {
		params.Embeds = []*discordgo.MessageEmbed{c.replyEmbed(ctx, msg, ch.GuildID)}
	}
	if !enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).Embeds) {
		params.Flags = discordgo.MessageFlagsSuppressEmbeds
	}
	execute := func() (*discordgo.Message, error) {
		if threadID == "" {
			return c.Session.WebhookExecute(webhookID, token, true, params, discordgo.WithContext(ctx))
		}
		return c.Session.WebhookThreadExecute(webhookID, token, true, threadID, params, discordgo.WithContext(ctx))
	}
	sent, err := execute()
	if err != nil {
		if newID, newToken, recovered := c.recoverWebhook(ctx, msg.Portal, webhookID, err, files); recovered {
			webhookID, token = newID, newToken
			sent, err = execute()
		}
	}
	if err != nil || sent == nil {
		return nil, fmt.Errorf("Discord webhook send failed; check webhook and channel permissions")
	}
	return &bridgev2.MatrixMessageResponse{DB: &database.Message{ID: networkid.MessageID(sent.ID), SenderID: networkid.UserID(c.UserLogin.ID), Timestamp: sent.Timestamp,
		Metadata: &MessageMetadata{ChannelID: channelID, WebhookID: webhookID, Excerpt: replyExcerpt(msg.Content.Body)}}}, nil
}

func (c *DiscordClient) ownedWebhook(ctx context.Context, portal *bridgev2.Portal, target *database.Message) (string, string, error) {
	if err := c.checkMessageChannel(portal, target); err != nil {
		return "", "", err
	}
	meta, ok := target.Metadata.(*MessageMetadata)
	if !ok || meta.WebhookID == "" || target.SenderID != networkid.UserID(c.UserLogin.ID) {
		return "", "", fmt.Errorf("only messages sent through this relay can be changed from Matrix")
	}
	webhookID, token, err := c.ensureWebhook(ctx, portal)
	if err != nil {
		return "", "", err
	}
	if meta.WebhookID != webhookID {
		return "", "", fmt.Errorf("message belongs to a different webhook")
	}
	return webhookID, token, nil
}
func (c *DiscordClient) HandleMatrixEdit(ctx context.Context, msg *bridgev2.MatrixEdit) error {
	if !enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).Edits) {
		return fmt.Errorf("edit sync is disabled")
	}
	text, files, err := c.outgoingContent(ctx, msg.Portal, msg.Content, false)
	if err != nil {
		return err
	}
	if len(utf16.Encode([]rune(text))) > 2000 {
		return fmt.Errorf("Discord webhook messages are limited to 2000 characters")
	}
	if meta, ok := msg.EditTarget.Metadata.(*MessageMetadata); ok && meta.WebhookID == "" && c.Session.IsUser {
		markSpoilers(files, msg.Event)
		return c.directEdit(ctx, msg, text, files)
	}
	webhookID, token, err := c.ownedWebhook(ctx, msg.Portal, msg.EditTarget)
	if err != nil {
		return err
	}
	meta := msg.EditTarget.Metadata.(*MessageMetadata)
	markSpoilers(files, msg.Event)
	params := &discordgo.WebhookEdit{Content: &text, AllowedMentions: c.allowedMentions(ctx, msg.Portal, msg.Content, matrixSender(msg.Event), text), Files: files}
	if len(files) > 0 {
		params.Attachments = ptr.Ptr([]*discordgo.MessageAttachment{})
	}
	_, err = c.Session.WebhookMessageEdit(webhookID, token, string(msg.EditTarget.ID), params, discordgo.WithContext(ctx), threadQuery(msg.Portal, meta.ChannelID))
	if err != nil {
		return fmt.Errorf("Discord webhook edit failed")
	}
	meta.Excerpt = replyExcerpt(msg.Content.Body)
	return nil
}
func (c *DiscordClient) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	if meta, ok := msg.TargetMessage.Metadata.(*MessageMetadata); ok && meta.ThreadNotice != "" {
		return nil
	}
	if !enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).Deletes) {
		return fmt.Errorf("delete sync is disabled")
	}
	if meta, ok := msg.TargetMessage.Metadata.(*MessageMetadata); ok && meta.WebhookID == "" && c.Session.IsUser {
		if err := c.checkMessageChannel(msg.Portal, msg.TargetMessage); err != nil {
			return err
		}
		if msg.TargetMessage.SenderID != networkid.UserID(c.UserLogin.ID) {
			return fmt.Errorf("only own Discord messages can be deleted")
		}
		if err := c.Session.ChannelMessageDelete(meta.ChannelID, string(msg.TargetMessage.ID), discordgo.WithContext(ctx)); err != nil {
			return fmt.Errorf("Discord message deletion failed")
		}
		return nil
	}
	webhookID, token, err := c.ownedWebhook(ctx, msg.Portal, msg.TargetMessage)
	if err != nil {
		return err
	}
	meta := msg.TargetMessage.Metadata.(*MessageMetadata)
	if err = c.Session.WebhookMessageDelete(webhookID, token, string(msg.TargetMessage.ID), discordgo.WithContext(ctx), threadQuery(msg.Portal, meta.ChannelID)); err != nil {
		return fmt.Errorf("Discord webhook deletion failed")
	}
	return nil
}
func (c *DiscordClient) PreHandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	if !c.allowed(string(msg.Portal.ID)) || !enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).Reactions) {
		return bridgev2.MatrixReactionPreResponse{}, fmt.Errorf("reaction sync is disabled")
	}
	emoji := msg.Content.RelatesTo.Key
	if strings.HasPrefix(emoji, "mxc://") {
		var err error
		emoji, err = c.Main.discordEmoji(ctx, emoji)
		if err != nil {
			return bridgev2.MatrixReactionPreResponse{}, err
		}
	}
	emojiID := variationselector.Remove(emoji)
	if split := strings.LastIndexByte(emoji, ':'); split >= 0 && validSnowflake(emoji[split+1:]) {
		emojiID = emoji[split+1:]
	}
	return bridgev2.MatrixReactionPreResponse{SenderID: networkid.UserID(c.UserLogin.ID), EmojiID: networkid.EmojiID(emojiID), Emoji: emoji}, nil
}
func messageChannel(target *database.Message, portal *bridgev2.Portal) string {
	if meta, ok := target.Metadata.(*MessageMetadata); ok && meta.ChannelID != "" {
		return meta.ChannelID
	}
	return string(portal.ID)
}

func (c *DiscordClient) checkMessageChannel(portal *bridgev2.Portal, target *database.Message) error {
	if !c.allowed(string(portal.ID)) {
		return fmt.Errorf("channel is not allowlisted for this login")
	}
	if messageChannel(target, portal) != string(portal.ID) && !enabled(c.Main.Config.syncFor(string(portal.ID)).Threads) {
		return fmt.Errorf("thread sync is disabled")
	}
	return nil
}

func (c *DiscordClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	if !c.allowed(string(msg.Portal.ID)) || !enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).Reactions) {
		return nil, fmt.Errorf("reaction sync is disabled")
	}
	if err := c.checkMessageChannel(msg.Portal, msg.TargetMessage); err != nil {
		return nil, err
	}
	if msg.PreHandleResp.Emoji == joinThreadReaction {
		if err := c.joinThread(ctx, msg.Portal, msg.TargetMessage); err != nil {
			return nil, err
		}
		return &database.Reaction{Metadata: &ReactionMetadata{ThreadJoin: true}}, nil
	}
	if meta, ok := msg.TargetMessage.Metadata.(*MessageMetadata); ok && meta.ThreadNotice != "" {
		return nil, fmt.Errorf("react with join thread to join this thread")
	}
	if err := c.Session.MessageReactionAdd(messageChannel(msg.TargetMessage, msg.Portal), string(msg.TargetMessage.ID), msg.PreHandleResp.Emoji, discordgo.WithContext(ctx)); err != nil {
		return nil, fmt.Errorf("Discord reaction failed")
	}
	return &database.Reaction{Metadata: &ReactionMetadata{ChannelID: messageChannel(msg.TargetMessage, msg.Portal), DiscordEmoji: msg.PreHandleResp.Emoji}}, nil
}
func (c *DiscordClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	if !c.allowed(string(msg.Portal.ID)) || !enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).Reactions) {
		return fmt.Errorf("reaction sync is disabled")
	}
	channelID, emoji := string(msg.Portal.ID), string(msg.TargetReaction.EmojiID)
	if meta, ok := msg.TargetReaction.Metadata.(*ReactionMetadata); ok {
		if meta.ThreadJoin {
			return nil
		}
		if meta.ChannelID != "" {
			channelID = meta.ChannelID
		}
		if meta.DiscordEmoji != "" {
			emoji = meta.DiscordEmoji
		}
	}
	if channelID != string(msg.Portal.ID) && !enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).Threads) {
		return fmt.Errorf("thread sync is disabled")
	}
	if err := c.Session.MessageReactionRemove(channelID, string(msg.TargetReaction.MessageID), emoji, "@me", discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("Discord reaction removal failed")
	}
	return nil
}
func (c *DiscordClient) HandleMatrixTyping(ctx context.Context, msg *bridgev2.MatrixTyping) error {
	if !msg.IsTyping || !c.allowed(string(msg.Portal.ID)) || !enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).Typing) {
		return nil
	}
	if err := c.Session.ChannelTyping(string(msg.Portal.ID), discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("Discord typing notification failed")
	}
	return nil
}

func threadQuery(portal *bridgev2.Portal, channelID string) discordgo.RequestOption {
	return func(config *discordgo.RequestConfig) {
		if channelID == "" {
			return
		}
		meta := portal.Metadata.(*PortalMetadata)
		if channelID != string(portal.ID) || meta.IsThread {
			query := config.Request.URL.Query()
			query.Set("thread_id", channelID)
			config.Request.URL.RawQuery = query.Encode()
		}
	}
}

func (c *DiscordClient) HandleMatrixRoomName(ctx context.Context, msg *bridgev2.MatrixRoomName) (bool, error) {
	if !c.allowed(string(msg.Portal.ID)) || !enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).RoomName) {
		return false, nil
	}
	if _, err := c.Session.ChannelEdit(string(msg.Portal.ID), &discordgo.ChannelEdit{Name: msg.Content.Name}, discordgo.WithContext(ctx)); err != nil {
		return false, fmt.Errorf("Discord channel name update failed")
	}
	msg.Portal.Name, msg.Portal.NameSet = msg.Content.Name, true
	return true, nil
}
func (c *DiscordClient) HandleMatrixRoomTopic(ctx context.Context, msg *bridgev2.MatrixRoomTopic) (bool, error) {
	if !c.allowed(string(msg.Portal.ID)) || !enabled(c.Main.Config.syncFor(string(msg.Portal.ID)).RoomTopic) {
		return false, nil
	}
	endpoint := discordgo.EndpointChannel(string(msg.Portal.ID))
	if _, err := c.Session.RequestWithBucketID("PATCH", endpoint, map[string]string{"topic": msg.Content.Topic}, endpoint, discordgo.WithContext(ctx)); err != nil {
		return false, fmt.Errorf("Discord channel topic update failed")
	}
	msg.Portal.Topic, msg.Portal.TopicSet = msg.Content.Topic, true
	return true, nil
}

func matrixSender(evt *event.Event) id.UserID {
	if evt == nil {
		return ""
	}
	return evt.Sender
}
