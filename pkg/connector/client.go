// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type DiscordClient struct {
	interactions     sync.Map
	Main             *DiscordConnector
	UserLogin        *bridgev2.UserLogin
	Session          *discordgo.Session
	stateMu          sync.RWMutex
	relationships    map[string]*discordgo.Relationship
	lifecycle        sync.Mutex
	connected        bool
	loggedOut        atomic.Bool
	gatewayConnected atomic.Bool

	// Gateway event handlers, by event type, and the queue they're run from (see dispatch.go).
	handlers map[reflect.Type][]reflect.Value
	events   chan any
	stopped  chan struct{}
	stopOnce sync.Once
}

var _ bridgev2.NetworkAPI = (*DiscordClient)(nil)

func (c *DiscordClient) Connect(ctx context.Context) {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	if c.connected || c.loggedOut.Load() || ctx.Err() != nil {
		return
	}
	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})
	if err := c.Session.Open(); err != nil {
		c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateUnknownError, Error: "discord-connect-failed",
			Message: "Failed to connect to Discord. Check the bot token, intents, and network connectivity."})
		return
	}
	c.connected = true
}
func (c *DiscordClient) Disconnect() {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	if c.connected {
		_ = c.Session.Close()
		c.connected = false
		c.gatewayConnected.Store(false)
	}
}
func (c *DiscordClient) LogoutRemote(context.Context) {
	c.loggedOut.Store(true)
	c.Main.clients.Delete(c.UserLogin.ID)
	c.Disconnect()
	c.stopDispatch()
}
func (c *DiscordClient) IsLoggedIn() bool { return !c.loggedOut.Load() }
func (c *DiscordClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return string(userID) == string(c.UserLogin.ID)
}

func (c *DiscordClient) allowed(channelID string) bool {
	if c.loggedOut.Load() {
		return false
	}
	if config := c.Main.Config.channel(channelID); config != nil {
		return config.RelayLoginID == "" || config.RelayLoginID == c.UserLogin.ID
	}
	ch, err := cachedChannel(c.Session.State, channelID)
	if err != nil {
		return false
	}
	if isPrivate(ch) {
		return c.Session.IsUser && c.Main.Config.PersonalAccounts && c.Main.Config.SyncDMs
	}
	if isThread(ch) {
		return false
	}
	return supportedChannel(ch) && c.guildMode(ch.GuildID) != "nothing"
}

func (c *DiscordClient) channel(ctx context.Context, channelID string) (*discordgo.Channel, error) {
	if ch, err := cachedChannel(c.Session.State, channelID); err == nil {
		return ch, nil
	}
	ch, err := c.Session.Channel(channelID, discordgo.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Discord channel %s", channelID)
	}
	if err := c.Session.State.ChannelAdd(ch); err == nil {
		return cachedChannel(c.Session.State, channelID)
	}
	return ch, nil
}

func isThread(ch *discordgo.Channel) bool {
	return ch.Type == discordgo.ChannelTypeGuildPublicThread || ch.Type == discordgo.ChannelTypeGuildPrivateThread || ch.Type == discordgo.ChannelTypeGuildNewsThread
}
func supportedChannel(ch *discordgo.Channel) bool {
	return isPrivate(ch) || ch.GuildID != "" && (ch.Type == discordgo.ChannelTypeGuildText || ch.Type == discordgo.ChannelTypeGuildNews || isThread(ch))
}

// A thread may share its explicitly allowlisted parent's Matrix room. DMs and
// channels outside the allowlist never acquire a portal through discovery.
func (c *DiscordClient) eventPortal(ctx context.Context, channelID string) (portalID string, threadID string) {
	if c.allowed(channelID) {
		return channelID, ""
	}
	var threadsEnabled bool
	for _, ch := range c.Main.Config.Channels {
		if c.allowed(ch.ID) && enabled(c.Main.Config.syncFor(ch.ID).Threads) {
			threadsEnabled = true
			break
		}
	}
	if !threadsEnabled && !enabled(c.Main.Config.Sync.Threads) {
		return "", ""
	}
	ch, err := c.channel(ctx, channelID)
	if err == nil && isThread(ch) && c.allowed(ch.ParentID) && enabled(c.Main.Config.syncFor(ch.ParentID).Threads) {
		return ch.ParentID, channelID
	}
	return "", ""
}

func (c *DiscordClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	channelID := string(portal.ID)
	if strings.HasPrefix(channelID, "dms:") {
		return c.dmSpaceInfo(portal)
	}
	if strings.HasPrefix(channelID, "guild:") || strings.HasPrefix(channelID, "category:") {
		return c.spaceInfo(ctx, portal)
	}
	if !c.allowed(channelID) {
		return nil, fmt.Errorf("channel is not allowlisted for this login")
	}
	ch, err := c.channel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	if !supportedChannel(ch) {
		return nil, fmt.Errorf("only guild text, announcement, and thread channels are supported")
	}
	syncConfig := c.Main.Config.syncFor(channelID)
	info := &bridgev2.ChatInfo{Type: ptr.Ptr(database.RoomTypeDefault), CanBackfill: enabled(syncConfig.Backfill) && enabled(syncConfig.Messages)}
	if enabled(syncConfig.RoomName) || portal.MXID == "" {
		info.Name = &ch.Name
	}
	if enabled(syncConfig.RoomTopic) {
		info.Topic = &ch.Topic
	}
	// Register the login without importing the guild's member list or roles.
	info.Members = &bridgev2.ChatMemberList{IsFull: false, MemberMap: map[networkid.UserID]bridgev2.ChatMember{
		networkid.UserID(c.UserLogin.ID): {EventSender: bridgev2.EventSender{IsFromMe: true, SenderLogin: c.UserLogin.ID, Sender: networkid.UserID(c.UserLogin.ID)}, Membership: event.MembershipJoin},
	}}
	info.ExtraUpdates = func(_ context.Context, p *bridgev2.Portal) bool {
		c.Main.webhookMu.Lock()
		defer c.Main.webhookMu.Unlock()
		meta := p.Metadata.(*PortalMetadata)
		changed := meta.GuildID != ch.GuildID || meta.ParentID != ch.ParentID || meta.IsThread != isThread(ch)
		meta.GuildID, meta.ParentID = ch.GuildID, ch.ParentID
		meta.IsThread = isThread(ch)
		if meta.WebhookID != "" {
			c.Main.ownWebhooks.Store(meta.WebhookID, true)
		}
		return changed
	}
	c.enrichChatInfo(ctx, portal, ch, info)
	return info, nil
}

func (c *DiscordClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	name := "Discord " + string(ghost.ID)
	if string(ghost.ID) == string(c.UserLogin.ID) {
		if c.UserLogin.RemoteName != "" {
			name = c.UserLogin.RemoteName
		}
		return &bridgev2.UserInfo{Name: &name}, nil
	}
	if !enabled(c.Main.Config.Sync.UserProfiles) {
		return &bridgev2.UserInfo{Name: &name}, nil
	}
	user, err := c.Session.User(string(ghost.ID), discordgo.WithContext(ctx))
	if err != nil {
		return &bridgev2.UserInfo{Name: &name}, nil
	}
	return c.userInfo(user), nil
}

func (c *DiscordClient) userInfo(user *discordgo.User) *bridgev2.UserInfo {
	name := c.profileName(user)
	info := &bridgev2.UserInfo{Name: &name, IsBot: &user.Bot, Identifiers: []string{"discord:" + user.ID}}
	avatarURL := user.AvatarURL("256")
	info.Avatar = c.avatar(avatarURL)
	return info
}

func (c *DiscordClient) syncChannels() {
	ctx := c.Main.Bridge.BackgroundCtx
	for _, ch := range c.Main.Config.Channels {
		if !c.allowed(ch.ID) {
			continue
		}
		if ch.WebhookURL != "" {
			webhookID, _, _ := parseWebhook(ch.WebhookURL)
			c.Main.ownWebhooks.Store(webhookID, true)
		}
		if ch.RoomID != "" {
			if _, err := c.Main.bindChannel(ctx, c, ch.ID, ch.RoomID); err != nil {
				c.UserLogin.Log.Error().Str("channel_id", ch.ID).Msg("Failed to bind configured channel; check room permissions and webhook configuration")
			}
			continue
		}
		key := networkid.PortalKey{ID: networkid.PortalID(ch.ID)}
		portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, key)
		if err != nil {
			c.UserLogin.Log.Error().Msg("Failed to load configured portal")
			continue
		}
		if portal != nil {
			c.Main.webhookMu.Lock()
			meta := portal.Metadata.(*PortalMetadata)
			if meta.WebhookID != "" {
				c.Main.ownWebhooks.Store(meta.WebhookID, true)
			}
			c.Main.webhookMu.Unlock()
		}
		if c.Main.Config.AutoCreatePortals || (portal != nil && portal.MXID != "") {
			c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatResync,
				PortalKey: key, CreatePortal: c.Main.Config.AutoCreatePortals,
				PostHandleFunc: func(ctx context.Context, p *bridgev2.Portal) {
					if c.Session.IsUser || !c.Main.Bridge.Config.Relay.Enabled {
						return
					}
					if p.RelayLoginID != "" && p.RelayLoginID != c.UserLogin.ID && ch.RelayLoginID == "" {
						return
					}
					if err := p.SetRelay(ctx, c.UserLogin); err != nil {
						c.UserLogin.Log.Error().Msg("Failed to set channel relay")
					}
					if _, _, err := c.ensureWebhook(ctx, p); err != nil {
						c.UserLogin.Log.Error().Msg("Failed to prepare channel webhook")
					}
				}}, GetChatInfoFunc: c.GetChatInfo,
				CheckNeedsBackfillFunc: func(context.Context, *database.Message) (bool, error) {
					return enabled(c.Main.Config.syncFor(ch.ID).Backfill), nil
				}})
		}
	}
}

func (c *DiscordClient) eventMeta(ctx context.Context, channelID, senderID string, timestamp time.Time, eventType bridgev2.RemoteEventType) (simplevent.EventMeta, string) {
	portalID, threadID := c.eventPortal(ctx, channelID)
	return simplevent.EventMeta{Type: eventType, PortalKey: c.portalKey(portalID),
		Sender:    bridgev2.EventSender{Sender: networkid.UserID(senderID), IsFromMe: senderID == string(c.UserLogin.ID), SenderLogin: loginIDIf(senderID == string(c.UserLogin.ID), c.UserLogin.ID)},
		Timestamp: timestamp, CreatePortal: c.canCreate(portalID)}, threadID
}
func loginIDIf(cond bool, login networkid.UserLoginID) networkid.UserLoginID {
	if cond {
		return login
	}
	return ""
}

// bindChannel is used by both declarative config and the admin provisioning API.
func (d *DiscordConnector) bindChannel(ctx context.Context, c *DiscordClient, channelID string, roomID id.RoomID) (*bridgev2.Portal, error) {
	if !c.allowed(channelID) {
		return nil, fmt.Errorf("channel is not allowlisted for this login")
	}
	if !d.Bridge.Config.Relay.Enabled {
		return nil, fmt.Errorf("bridge.relay.enabled must be true")
	}
	d.bindMu.Lock()
	defer d.bindMu.Unlock()
	portal, err := d.Bridge.GetPortalByKey(ctx, networkid.PortalKey{ID: networkid.PortalID(channelID)})
	if err != nil {
		return nil, err
	}
	if portal.MXID != "" && portal.MXID != roomID {
		return nil, fmt.Errorf("channel is already bridged to another room")
	}
	if existing, err := d.Bridge.GetPortalByMXID(ctx, roomID); err != nil {
		return nil, err
	} else if existing != nil && existing.ID != portal.ID {
		return nil, fmt.Errorf("Matrix room is already bridged to another channel")
	}
	bot := d.Bridge.Bot
	if err = bot.EnsureJoined(ctx, roomID); err != nil {
		return nil, fmt.Errorf("invite the bridge bot to the Matrix room first")
	}
	pls, err := d.Bridge.Matrix.GetPowerLevels(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("failed to check Matrix room permissions")
	}
	if pls.GetUserLevel(bot.GetMXID()) < pls.GetEventLevel(event.StateBridge) {
		return nil, fmt.Errorf("bridge bot needs permission to send m.bridge state")
	}
	info, err := c.GetChatInfo(ctx, portal)
	if err != nil {
		return nil, err
	}
	// Preserve existing Matrix names even when initial portal metadata is empty.
	if !enabled(d.Config.syncFor(channelID).RoomName) {
		info.Name = nil
	}
	if _, _, err = c.ensureWebhook(ctx, portal); err != nil {
		return nil, err
	}
	if err = portal.UpdateMatrixRoomID(ctx, roomID, bridgev2.UpdateMatrixRoomIDParams{FailIfMXIDSet: true}); err != nil {
		return nil, err
	}
	c.UserLogin.MarkInPortal(ctx, portal)
	portal.UpdateInfo(ctx, info, c.UserLogin, nil, time.Time{})
	if err = portal.SetRelay(ctx, c.UserLogin); err != nil {
		return nil, err
	}
	if enabled(d.Config.syncFor(channelID).Backfill) {
		c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatResync, PortalKey: portal.PortalKey},
			CheckNeedsBackfillFunc: func(context.Context, *database.Message) (bool, error) { return true, nil }})
	}
	return portal, nil
}

func (c *DiscordClient) messageSender(uid string) bridgev2.EventSender {
	return bridgev2.EventSender{Sender: networkid.UserID(uid), IsFromMe: uid == string(c.UserLogin.ID), SenderLogin: loginIDIf(uid == string(c.UserLogin.ID), c.UserLogin.ID)}
}

var errNotBridged = errors.New("channel is not bridged")

// unbindChannel forgets a channel's portal and leaves its Matrix room as it was
// for the people in it: the room isn't deleted and nobody but the bridge's own
// users (its ghosts and bot) leaves. Bridgev2's DeleteRoom isn't used, as it
// deletes the whole room where the homeserver can.
func (d *DiscordConnector) unbindChannel(ctx context.Context, channelID string) (id.RoomID, error) {
	d.bindMu.Lock()
	defer d.bindMu.Unlock()
	portal, err := d.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{ID: networkid.PortalID(channelID)})
	if err != nil {
		return "", err
	}
	if portal == nil || portal.MXID == "" {
		return "", errNotBridged
	}
	roomID := portal.MXID
	log := zerolog.Ctx(ctx).With().Str("channel_id", channelID).Stringer("room_id", roomID).Logger()
	// Deleted first, so nothing more is bridged while the room's cleaned up.
	if err = portal.Delete(ctx); err != nil {
		return "", err
	}
	members, err := d.Bridge.Matrix.GetMembers(ctx, roomID)
	if err != nil {
		log.Err(err).Msg("Failed to get members to remove ghosts from unbridged room")
	}
	for userID, member := range members {
		if member.Membership != event.MembershipJoin && member.Membership != event.MembershipInvite {
			continue
		}
		if ghostID, ok := d.Bridge.Matrix.ParseGhostMXID(userID); ok {
			if err = leave(ctx, d.Bridge.Matrix.GhostIntent(ghostID), roomID); err != nil {
				log.Err(err).Stringer("user_id", userID).Msg("Failed to remove ghost from unbridged room")
			}
		}
	}
	bot := d.Bridge.Bot
	stateKey := string(portal.BridgeID)
	if d.Bridge.Config.NoBridgeInfoStateKey {
		stateKey = ""
	} else if provider, ok := d.Bridge.Matrix.(bridgev2.MatrixConnectorWithBridgeIdentifier); ok {
		stateKey = provider.GetUniqueBridgeID()
	}
	for _, kind := range []event.Type{event.StateBridge, event.StateHalfShotBridge} {
		if _, err = bot.SendState(ctx, roomID, kind, stateKey, &event.Content{Raw: map[string]any{}}, time.Time{}); err != nil {
			log.Err(err).Stringer("event_type", kind).Msg("Failed to clear bridge info from unbridged room")
		}
	}
	if err = leave(ctx, bot, roomID); err != nil {
		log.Err(err).Msg("Failed to leave unbridged room")
	}
	return roomID, nil
}

func leave(ctx context.Context, intent bridgev2.MatrixAPI, roomID id.RoomID) error {
	_, err := intent.SendState(ctx, roomID, event.StateMember, intent.GetMXID().String(),
		&event.Content{Parsed: &event.MemberEventContent{Membership: event.MembershipLeave}}, time.Time{})
	return err
}
