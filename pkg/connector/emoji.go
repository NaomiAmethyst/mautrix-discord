// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

type cachedEmoji struct {
	URI      id.ContentURIString `json:"uri"`
	ID       string              `json:"id"`
	Name     string              `json:"name"`
	Animated bool                `json:"animated"`
}

func (c *DiscordClient) portalKey(cid string) networkid.PortalKey {
	key := networkid.PortalKey{ID: networkid.PortalID(cid)}
	if ch, err := cachedChannel(c.Session.State, cid); err == nil && isPrivate(ch) {
		key.Receiver = c.UserLogin.ID
	}
	return key
}
func (c *DiscordClient) emojiMXC(ctx context.Context, eid, name string, animated bool) (id.ContentURIString, error) {
	d := c.Main
	if d.Bridge.DB == nil || d.Bridge.Bot == nil {
		return "", fmt.Errorf("media storage unavailable")
	}
	d.mediaMu.Lock()
	defer d.mediaMu.Unlock()
	key := database.Key("discord.emoji." + eid)
	var cached cachedEmoji
	if raw := d.Bridge.DB.KV.Get(ctx, key); raw != "" && json.Unmarshal([]byte(raw), &cached) == nil && cached.URI != "" {
		return cached.URI, nil
	}
	url, mime := discordgo.EndpointEmoji(eid), "image/png"
	if animated {
		url, mime = discordgo.EndpointEmojiAnimated(eid), "image/gif"
	}
	if d.directMedia.Load() {
		if uri, err := d.directURI(ctx, remoteMedia{URL: url}); err == nil {
			cached = cachedEmoji{URI: uri, ID: eid, Name: name, Animated: animated}
			encoded, _ := json.Marshal(cached)
			d.Bridge.DB.KV.Set(ctx, key, string(encoded))
			d.Bridge.DB.KV.Set(ctx, database.Key("discord.emoji.mxc."+string(uri)), string(encoded))
			return uri, nil
		}
	}
	data, err := d.download(ctx, url)
	if err != nil {
		return "", err
	}
	uri, _, err := d.Bridge.Bot.UploadMedia(ctx, "", data, name, mime)
	if err != nil {
		return "", err
	}
	cached = cachedEmoji{URI: uri, ID: eid, Name: name, Animated: animated}
	encoded, _ := json.Marshal(cached)
	d.Bridge.DB.KV.Set(ctx, key, string(encoded))
	d.Bridge.DB.KV.Set(ctx, database.Key("discord.emoji.mxc."+string(uri)), string(encoded))
	return uri, nil
}
func (d *DiscordConnector) discordEmoji(ctx context.Context, uri string) (string, error) {
	if d.Bridge.DB == nil {
		return "", fmt.Errorf("unknown custom emoji")
	}
	var cached cachedEmoji
	if json.Unmarshal([]byte(d.Bridge.DB.KV.Get(ctx, database.Key("discord.emoji.mxc."+uri))), &cached) != nil || cached.ID == "" {
		return "", fmt.Errorf("only custom emojis previously bridged from Discord can be reused")
	}
	return cached.Name + ":" + cached.ID, nil
}
