// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

type DiscordConnector struct {
	Bridge      *bridgev2.Bridge
	Config      Config
	HTTP        *http.Client
	maxFileSize atomic.Int64
	directMedia atomic.Bool
	clients     sync.Map
	mediaMu     sync.Mutex
	threadMu    sync.Mutex
	webhookMu   sync.Mutex
	bindMu      sync.Mutex
	ownWebhooks sync.Map
}

type LoginMetadata struct {
	Token       string            `json:"token"`
	AccountType string            `json:"account_type,omitempty"`
	GuildModes  map[string]string `json:"guild_modes,omitempty"`
}
type PortalMetadata struct {
	WebhookID    string `json:"webhook_id,omitempty"`
	WebhookToken string `json:"webhook_token,omitempty"`
	GuildID      string `json:"guild_id,omitempty"`
	ParentID     string `json:"parent_id,omitempty"`
	IsThread     bool   `json:"is_thread,omitempty"`
}
type MessageMetadata struct {
	ThreadNotice string `json:"thread_notice,omitempty"`
	Excerpt      string `json:"excerpt,omitempty"`
	MessageID    string `json:"message_id,omitempty"`
	ChannelID    string `json:"channel_id"`
	WebhookID    string `json:"webhook_id,omitempty"`
}
type ReactionMetadata struct {
	ThreadJoin   bool   `json:"thread_join,omitempty"`
	ChannelID    string `json:"channel_id"`
	DiscordEmoji string `json:"discord_emoji"`
}

var _ bridgev2.NetworkConnector = (*DiscordConnector)(nil)
var _ bridgev2.ConfigValidatingNetwork = (*DiscordConnector)(nil)
var _ bridgev2.MaxFileSizeingNetwork = (*DiscordConnector)(nil)

func (d *DiscordConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{DisplayName: "Discord", NetworkURL: "https://discord.com", NetworkID: "discord",
		BeeperBridgeType: "discord", DefaultPort: 29334}
}
func (d *DiscordConnector) Init(br *bridgev2.Bridge) {
	d.Bridge = br
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if d.Config.Proxy != "" {
		if proxy, err := url.Parse(d.Config.Proxy); err == nil {
			transport.Proxy = http.ProxyURL(proxy)
		}
	}
	d.HTTP = &http.Client{Timeout: 60 * time.Second, Transport: transport}
	d.registerCommands()
}
func (d *DiscordConnector) Start(ctx context.Context) error {
	if d.Bridge.Config.SplitPortals {
		return fmt.Errorf("admin-configured relay channels require bridge.split_portals: false")
	}
	portals, err := d.Bridge.DB.Portal.GetAllWithMXID(ctx)
	if err != nil {
		return fmt.Errorf("failed to load portal webhook identities: %w", err)
	}
	for _, portal := range portals {
		if meta := portal.Metadata.(*PortalMetadata); meta.WebhookID != "" {
			d.ownWebhooks.Store(meta.WebhookID, true)
		}
	}
	for _, channel := range d.Config.Channels {
		if channel.WebhookURL != "" {
			webhookID, _, _ := parseWebhook(channel.WebhookURL)
			d.ownWebhooks.Store(webhookID, true)
		}
	}
	return nil
}
func (d *DiscordConnector) GetBridgeInfoVersion() (int, int) { return 1, 1 }
func (d *DiscordConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{}
}
func (d *DiscordConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		UserLogin: func() any { return &LoginMetadata{} },
		Portal:    func() any { return &PortalMetadata{} },
		Message:   func() any { return &MessageMetadata{} },
		Reaction:  func() any { return &ReactionMetadata{} },
	}
}
func (d *DiscordConnector) SetMaxFileSize(size int64) { d.maxFileSize.Store(size) }
func (d *DiscordConnector) fileLimit() int64 {
	if limit := d.maxFileSize.Load(); limit > 0 && limit < d.Config.MaxAttachmentSize {
		return limit
	}
	return d.Config.MaxAttachmentSize
}
func (d *DiscordConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	token := login.Metadata.(*LoginMetadata).Token
	if token == "" {
		return fmt.Errorf("stored bot token is empty")
	}
	meta := login.Metadata.(*LoginMetadata)
	if meta.AccountType == "" || meta.AccountType == "bot" {
		token = "Bot " + token
	} else if !d.Config.PersonalAccounts {
		return fmt.Errorf("personal accounts are disabled in config")
	} else if meta.AccountType == "oauth" {
		token = "Bearer " + token
	}
	session, err := discordgo.New(token)
	if err != nil {
		return fmt.Errorf("failed to initialize Discord client")
	}
	if !session.IsUser {
		session.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentMessageContent |
			discordgo.IntentsGuildMessageReactions | discordgo.IntentsGuildMessageTyping
	}
	session.State.MaxMessageCount = 100
	session.Client = d.HTTP
	gatewayHTTP := *d.HTTP
	gatewayHTTP.Timeout = 0
	session.GatewayHTTPClient = &gatewayHTTP
	session.GatewayDialTimeout = 30 * time.Second
	c := &DiscordClient{Main: d, UserLogin: login, Session: session, relationships: make(map[string]*discordgo.Relationship)}
	if login.Client != nil {
		if previous, ok := login.Client.(*DiscordClient); ok {
			previous.loggedOut.Store(true)
		}
		login.Client.Disconnect()
	}
	login.Client = c
	d.clients.Store(login.ID, c)
	c.registerEvents()
	return nil
}
