// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	_ "embed"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"text/template"

	up "go.mau.fi/util/configupgrade"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

//go:embed example-config.yaml
var ExampleConfig string

// Pointers distinguish an explicit false from an omitted per-channel override.
type SyncConfig struct {
	ReadReceipts *bool `yaml:"read_receipts"`
	RoomAvatar   *bool `yaml:"room_avatar"`
	Messages     *bool `yaml:"messages"`
	Attachments  *bool `yaml:"attachments"`
	Embeds       *bool `yaml:"embeds"`
	Stickers     *bool `yaml:"stickers"`
	Edits        *bool `yaml:"edits"`
	Deletes      *bool `yaml:"deletes"`
	Reactions    *bool `yaml:"reactions"`
	Typing       *bool `yaml:"typing"`
	UserProfiles *bool `yaml:"user_profiles"`
	RoomName     *bool `yaml:"room_name"`
	RoomTopic    *bool `yaml:"room_topic"`
	Threads      *bool `yaml:"threads"`
	Backfill     *bool `yaml:"backfill"`
}

type ChannelConfig struct {
	ID           string                `yaml:"id"`
	RoomID       id.RoomID             `yaml:"room_id"`
	RelayLoginID networkid.UserLoginID `yaml:"relay_login_id"`
	WebhookURL   string                `yaml:"webhook_url"`
	Sync         SyncConfig            `yaml:"sync"`
}

type GuildConfig struct {
	ID   string `yaml:"id"`
	Mode string `yaml:"mode"`
}
type Config struct {
	UseDiscordCDNUpload         bool                  `yaml:"use_discord_cdn_upload"`
	CustomEmojiReactions        bool                  `yaml:"custom_emoji_reactions"`
	ForbidDMingStrangers        bool                  `yaml:"forbid_dming_strangers"`
	PrefixWebhookMessages       bool                  `yaml:"prefix_webhook_messages"`
	RestrictedRooms             bool                  `yaml:"restricted_rooms"`
	AnimatedSticker             AnimatedStickerConfig `yaml:"animated_sticker"`
	PersonalAccounts            bool                  `yaml:"personal_accounts"`
	DMSpaces                    bool                  `yaml:"dm_spaces"`
	SyncDMs                     bool                  `yaml:"sync_dms"`
	StartupDMLimit              int                   `yaml:"startup_private_channel_create_limit"`
	Guilds                      []GuildConfig         `yaml:"guilds"`
	GuildSpaces                 bool                  `yaml:"guild_spaces"`
	DeletePortalOnChannelDelete bool                  `yaml:"delete_portal_on_channel_delete"`
	DeleteGuildOnLeave          bool                  `yaml:"delete_guild_on_leave"`
	MuteChannelsOnCreate        bool                  `yaml:"mute_channels_on_create"`
	DisplaynameTemplate         string                `yaml:"displayname_template"`
	ChannelNameTemplate         string                `yaml:"channel_name_template"`
	GuildNameTemplate           string                `yaml:"guild_name_template"`
	Proxy                       string                `yaml:"proxy"`
	CacheMedia                  string                `yaml:"cache_media"`

	EmbedFieldsAsTables bool            `yaml:"embed_fields_as_tables"`
	AutojoinThreads     bool            `yaml:"autojoin_threads"`
	Channels            []ChannelConfig `yaml:"channels"`
	AutoCreatePortals   bool            `yaml:"auto_create_portals"`
	CreateWebhooks      bool            `yaml:"create_webhooks"`
	WebhookName         string          `yaml:"webhook_name"`
	WebhookAvatars      bool            `yaml:"webhook_avatars"`
	AllowMentions       bool            `yaml:"allow_mentions"`
	MaxAttachmentSize   int64           `yaml:"max_attachment_size"`
	Sync                SyncConfig      `yaml:"sync"`
}

func enabled(value *bool) bool { return value != nil && *value }

func (c *Config) channel(channelID string) *ChannelConfig {
	for i := range c.Channels {
		if c.Channels[i].ID == channelID {
			return &c.Channels[i]
		}
	}
	return nil
}

func (c *Config) syncFor(channelID string) SyncConfig {
	result := c.Sync
	if channel := c.channel(channelID); channel != nil {
		dst, src := reflect.ValueOf(&result).Elem(), reflect.ValueOf(channel.Sync)
		for i := 0; i < dst.NumField(); i++ {
			if !src.Field(i).IsNil() {
				dst.Field(i).Set(src.Field(i))
			}
		}
	}
	return result
}

func validSnowflake(value string) bool {
	if len(value) == 0 || len(value) > 20 || value == "0" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// Accept only Discord incoming webhook URLs, never arbitrary request destinations.
func parseWebhook(raw string) (webhookID, token string, err error) {
	u, parseErr := url.Parse(raw)
	if parseErr != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Host != "discord.com" && u.Host != "discordapp.com") {
		return "", "", fmt.Errorf("expected a Discord HTTPS incoming webhook URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 5 && strings.HasPrefix(parts[1], "v") {
		parts = append(parts[:1], parts[2:]...)
	}
	if len(parts) != 4 || parts[0] != "api" || parts[1] != "webhooks" || !validSnowflake(parts[2]) || parts[3] == "" {
		return "", "", fmt.Errorf("invalid Discord incoming webhook URL")
	}
	return parts[2], parts[3], nil
}

func (d *DiscordConnector) ValidateConfig() error {
	seen, rooms := map[string]bool{}, map[id.RoomID]bool{}
	for _, ch := range d.Config.Channels {
		if !validSnowflake(ch.ID) || seen[ch.ID] {
			return fmt.Errorf("invalid or duplicate channel ID %q", ch.ID)
		}
		seen[ch.ID] = true
		if ch.RelayLoginID != "" && !validSnowflake(string(ch.RelayLoginID)) {
			return fmt.Errorf("invalid relay login ID for channel %s", ch.ID)
		}
		if ch.RoomID != "" {
			if !strings.HasPrefix(string(ch.RoomID), "!") || !strings.Contains(string(ch.RoomID), ":") || rooms[ch.RoomID] {
				return fmt.Errorf("invalid or duplicate Matrix room ID for channel %s", ch.ID)
			}
			if ch.RelayLoginID == "" {
				return fmt.Errorf("channel %s with room_id requires relay_login_id", ch.ID)
			}
			rooms[ch.RoomID] = true
		}
		if ch.WebhookURL != "" {
			if _, _, err := parseWebhook(ch.WebhookURL); err != nil {
				return fmt.Errorf("channel %s: %w", ch.ID, err)
			}
		}
	}
	if d.Config.MaxAttachmentSize <= 0 {
		return fmt.Errorf("max_attachment_size must be positive")
	}
	if d.Config.WebhookName == "" || len([]rune(d.Config.WebhookName)) > 80 {
		return fmt.Errorf("webhook_name must contain 1-80 characters")
	}
	guilds := map[string]bool{}
	for _, guild := range d.Config.Guilds {
		if !validSnowflake(guild.ID) || guilds[guild.ID] || !validGuildMode(guild.Mode) {
			return fmt.Errorf("invalid or duplicate guild configuration")
		}
		guilds[guild.ID] = true
	}
	for _, pattern := range []string{d.Config.DisplaynameTemplate, d.Config.ChannelNameTemplate, d.Config.GuildNameTemplate} {
		if _, err := template.New("name").Parse(pattern); err != nil {
			return fmt.Errorf("invalid name template: %w", err)
		}
	}
	if d.Config.CacheMedia != "" && d.Config.CacheMedia != "always" && d.Config.CacheMedia != "never" && d.Config.CacheMedia != "unencrypted" {
		return fmt.Errorf("cache_media must be always, never, or unencrypted")
	}
	if d.Config.Proxy != "" {
		proxy, err := url.Parse(d.Config.Proxy)
		if err != nil || proxy.Host == "" || (proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5") {
			return fmt.Errorf("invalid proxy URL")
		}
	}
	sticker := d.Config.AnimatedSticker
	switch sticker.Target {
	case "", "disable":
	case "png", "gif", "webp", "webm":
		if sticker.Width < 1 || sticker.Width > 2048 || sticker.Height < 1 || sticker.Height > 2048 || sticker.FPS < 1 || sticker.FPS > 60 {
			return fmt.Errorf("animated_sticker dimensions and FPS are invalid")
		}
	default:
		return fmt.Errorf("invalid animated_sticker target")
	}
	return nil
}

func (d *DiscordConnector) GetConfig() (string, any, up.Upgrader) {
	return ExampleConfig, &d.Config, up.SimpleUpgrader(func(h up.Helper) {
		h.Copy(up.List, "channels")
		h.Copy(up.List, "guilds")
		h.Copy(up.Str, "animated_sticker", "target")
		for _, key := range []string{"width", "height", "fps"} {
			h.Copy(up.Int, "animated_sticker", key)
		}
		for _, key := range []string{"auto_create_portals", "create_webhooks", "webhook_avatars", "allow_mentions", "autojoin_threads", "embed_fields_as_tables", "personal_accounts", "sync_dms", "dm_spaces", "guild_spaces", "delete_portal_on_channel_delete", "delete_guild_on_leave", "mute_channels_on_create", "forbid_dming_strangers", "prefix_webhook_messages", "restricted_rooms", "use_discord_cdn_upload", "custom_emoji_reactions"} {
			h.Copy(up.Bool, key)
		}
		for _, key := range []string{"webhook_name", "displayname_template", "channel_name_template", "guild_name_template", "proxy", "cache_media"} {
			h.Copy(up.Str, key)
		}
		h.Copy(up.Int, "startup_private_channel_create_limit")
		h.Copy(up.Int, "max_attachment_size")
		typ := reflect.TypeOf(SyncConfig{})
		for i := 0; i < typ.NumField(); i++ {
			h.Copy(up.Bool, "sync", typ.Field(i).Tag.Get("yaml"))
		}
	})
}
