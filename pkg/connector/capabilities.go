// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
)

func support(enabled bool) event.CapabilitySupportLevel {
	if enabled {
		return event.CapLevelFullySupported
	}
	return event.CapLevelRejected
}
func (c *DiscordClient) GetCapabilities(_ context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	sc := c.Main.Config.syncFor(string(portal.ID))
	allowed := c.allowed(string(portal.ID)) && enabled(sc.Messages)
	features := &event.RoomFeatures{
		MaxTextLength: 2000,
		Reply:         event.CapLevelPartialSupport,
		Edit:          support(allowed && enabled(sc.Edits)), Delete: support(allowed && enabled(sc.Deletes)),
		Reaction:            support(allowed && enabled(sc.Reactions)),
		TypingNotifications: allowed && enabled(sc.Typing),
		Thread:              event.CapLevelRejected,
		File:                event.FileFeatureMap{},
		// Relayed messages go through the channel's webhook, under the Matrix sender's own name
		// and avatar, so the bridge leaves them as they are rather than prefixing the sender's
		// name (bridge.relay.message_formats).
		PerMessageProfileRelay: allowed,
	}
	if allowed && enabled(sc.Threads) {
		features.Thread = event.CapLevelFullySupported
	}
	features.CustomEmojiReactions = allowed && enabled(sc.Reactions)
	if c.Session.IsUser {
		features.Reply = support(allowed)
	}
	features.Formatting = event.FormattingFeatureMap{}
	for _, kind := range []event.FormattingFeature{event.FmtBold, event.FmtItalic, event.FmtUnderline, event.FmtStrikethrough, event.FmtInlineCode, event.FmtCodeBlock, event.FmtSyntaxHighlighting, event.FmtBlockquote, event.FmtInlineLink, event.FmtUserLink, event.FmtRoomLink, event.FmtSpoiler} {
		features.Formatting[kind] = support(allowed)
	}
	for _, msgType := range []event.MessageType{event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile, event.CapMsgSticker, event.CapMsgVoice} {
		fileSupported := allowed && ((msgType == event.CapMsgSticker && enabled(sc.Stickers)) || (msgType != event.CapMsgSticker && enabled(sc.Attachments)))
		level := support(fileSupported)
		if msgType == event.CapMsgVoice && fileSupported {
			level = event.CapLevelPartialSupport
		}
		features.File[msgType] = &event.FileFeatures{MimeTypes: map[string]event.CapabilitySupportLevel{"*/*": level},
			Caption: event.CapLevelFullySupported, MaxCaptionLength: 2000, MaxSize: c.Main.fileLimit()}
	}
	return features
}
