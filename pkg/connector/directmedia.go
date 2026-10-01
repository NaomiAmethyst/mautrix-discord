// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/mediaproxy"
)

type remoteMedia struct {
	URL        string `json:"url,omitempty"`
	Channel    string `json:"channel,omitempty"`
	Message    string `json:"message,omitempty"`
	Attachment string `json:"attachment,omitempty"`
}

var _ bridgev2.DirectMediableNetwork = (*DiscordConnector)(nil)

func (d *DiscordConnector) SetUseDirectMedia() { d.directMedia.Store(true) }
func (d *DiscordConnector) directURI(ctx context.Context, media remoteMedia) (id.ContentURIString, error) {
	if !d.directMedia.Load() {
		return "", bridgev2.ErrDirectMediaNotEnabled
	}
	if !discordMediaURL(media.URL) {
		return "", fmt.Errorf("invalid Discord media URL")
	}
	raw, _ := json.Marshal(media)
	return d.Bridge.Matrix.GenerateContentURI(ctx, networkid.MediaID(raw))
}
func mediaExpiry(raw string) time.Time {
	u, err := url.Parse(raw)
	if err != nil {
		return time.Time{}
	}
	expiry, err := strconv.ParseInt(u.Query().Get("ex"), 16, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(expiry, 0)
}
func (d *DiscordConnector) Download(ctx context.Context, mediaID networkid.MediaID, _ map[string]string) (mediaproxy.GetMediaResponse, error) {
	var media remoteMedia
	if len(mediaID) > 4096 || json.Unmarshal([]byte(mediaID), &media) != nil || !discordMediaURL(media.URL) {
		return nil, fmt.Errorf("invalid Discord media ID")
	}
	expiry := mediaExpiry(media.URL)
	if media.Attachment != "" && (expiry.IsZero() || time.Until(expiry) < 5*time.Minute) {
		var found bool
		d.clients.Range(func(_, value any) bool {
			c := value.(*DiscordClient)
			pid, _ := c.eventPortal(ctx, media.Channel)
			if pid == "" {
				return true
			}
			msg, err := c.Session.ChannelMessage(media.Channel, media.Message, discordgo.WithContext(ctx))
			if err != nil {
				return true
			}
			for _, a := range msg.Attachments {
				if a.ID == media.Attachment && discordMediaURL(a.URL) {
					media.URL = a.URL
					found = true
					break
				}
			}
			return !found
		})
		if !found {
			return nil, fmt.Errorf("attachment is unavailable or no logged-in account has access")
		}
		expiry = mediaExpiry(media.URL)
	}
	return &mediaproxy.GetMediaResponseURL{URL: media.URL, ExpiresAt: expiry}, nil
}
