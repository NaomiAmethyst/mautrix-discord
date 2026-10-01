// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"

	"maunium.net/go/mautrix/bridgev2/database"

	"github.com/bwmarrin/discordgo"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type mentionNamesKey struct{}

func (c *DiscordClient) messageContext(ctx context.Context, msg *discordgo.Message) context.Context {
	names := make(map[string]string, len(msg.Mentions)+1)
	for _, user := range msg.Mentions {
		if user != nil {
			names[user.ID] = c.profileName(user)
		}
	}
	if msg.Author != nil {
		names[msg.Author.ID] = c.profileName(msg.Author)
	}
	return context.WithValue(ctx, mentionNamesKey{}, names)
}

func (c *DiscordClient) profileAvatar(ctx context.Context, rawURL, name string) id.ContentURIString {
	if rawURL == "" {
		return ""
	}
	if c.Main.directMedia.Load() {
		if uri, err := c.Main.directURI(ctx, remoteMedia{URL: rawURL}); err == nil {
			return uri
		}
	}
	if c.Main.Bridge.Bot == nil {
		return ""
	}
	var key database.Key
	if c.Main.Bridge.DB != nil && c.Main.Config.CacheMedia != "never" {
		key = database.Key(fmt.Sprintf("discord.profile.avatar.%x", sha256.Sum256([]byte(rawURL))))
		c.Main.mediaMu.Lock()
		defer c.Main.mediaMu.Unlock()
		var cached cachedMedia
		if raw := c.Main.Bridge.DB.KV.Get(ctx, key); raw != "" && json.Unmarshal([]byte(raw), &cached) == nil && cached.URI != "" {
			return cached.URI
		}
	}
	data, err := c.Main.download(ctx, rawURL)
	if err != nil {
		return ""
	}
	uri, _, err := c.Main.Bridge.Bot.UploadMedia(ctx, "", data, name, http.DetectContentType(data))
	if err != nil {
		return ""
	}
	if key != "" {
		encoded, _ := json.Marshal(cachedMedia{URI: uri})
		c.Main.Bridge.DB.KV.Set(ctx, key, string(encoded))
	}
	return uri
}

func (c *DiscordClient) addMessageProfiles(ctx context.Context, portal *bridgev2.Portal, msg *discordgo.Message, converted *bridgev2.ConvertedMessage) {
	if msg.Author == nil || !enabled(c.Main.Config.syncFor(string(portal.ID)).UserProfiles) {
		return
	}
	var profile *event.BeeperPerMessageProfile
	var extraKey string
	var extra map[string]any
	if msg.Member != nil {
		member := *msg.Member
		member.User, member.GuildID = msg.Author, msg.GuildID
		avatarURL := ""
		if member.Avatar != "" {
			avatarURL = member.AvatarURL("256")
		}
		avatar := c.profileAvatar(ctx, avatarURL, "guild-avatar.png")
		extraKey = "fi.mau.discord.guild_member_metadata"
		extra = map[string]any{"nick": member.Nick, "avatar_id": member.Avatar, "avatar_url": avatarURL, "avatar_mxc": avatar}
		if member.Nick != "" || avatar != "" {
			name := member.Nick
			if name == "" {
				name = c.profileName(msg.Author)
			}
			profile = &event.BeeperPerMessageProfile{ID: msg.GuildID + "_" + msg.Author.ID, Displayname: name}
			if avatar != "" {
				profile.AvatarURL = ptr.Ptr(avatar)
			}
		}
	}
	if msg.WebhookID != "" {
		avatarURL := ""
		if msg.Author.Avatar != "" {
			avatarURL = msg.Author.AvatarURL("256")
		}
		avatar := c.profileAvatar(ctx, avatarURL, "webhook-avatar.png")
		extraKey = "fi.mau.discord.webhook_metadata"
		extra = map[string]any{"id": msg.WebhookID, "name": msg.Author.Username, "avatar_id": msg.Author.Avatar, "avatar_url": avatarURL, "avatar_mxc": avatar}
		profile = &event.BeeperPerMessageProfile{ID: fmt.Sprintf("%x", sha256.Sum256([]byte(msg.Author.Username+":"+msg.Author.Avatar))), Displayname: msg.Author.Username, AvatarURL: ptr.Ptr(avatar)}
	}
	for _, part := range converted.Parts {
		if extraKey != "" {
			if part.Extra == nil {
				part.Extra = make(map[string]any)
			}
			part.Extra[extraKey] = extra
		}
		if profile == nil {
			continue
		}
		copyProfile := *profile
		part.Content.BeeperPerMessageProfile = &copyProfile
		if msg.WebhookID != "" && msg.ApplicationID == "" && c.Main.Config.PrefixWebhookMessages && (part.Content.MsgType == event.MsgText || part.Content.MsgType == event.MsgNotice || part.Content.FileName != "" && part.Content.FileName != part.Content.Body) {
			part.Content.AddPerMessageProfileFallback()
		}
	}
}
