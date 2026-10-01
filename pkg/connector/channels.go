// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/bwmarrin/discordgo"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
)

func (c *DiscordClient) guildMode(gid string) string {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	if meta, ok := c.UserLogin.Metadata.(*LoginMetadata); ok && meta.GuildModes != nil {
		if mode, ok := meta.GuildModes[gid]; ok {
			return mode
		}
	}
	for _, guild := range c.Main.Config.Guilds {
		if guild.ID == gid {
			return guild.Mode
		}
	}
	return "nothing"
}
func (c *DiscordClient) canCreate(cid string) bool {
	if c.Main.Config.channel(cid) != nil {
		return c.Main.Config.AutoCreatePortals
	}
	ch, err := cachedChannel(c.Session.State, cid)
	if err != nil {
		return false
	}
	if ch.Type == discordgo.ChannelTypeDM || ch.Type == discordgo.ChannelTypeGroupDM {
		return c.Main.Config.PersonalAccounts && c.Main.Config.SyncDMs && c.Session.IsUser
	}
	mode := c.guildMode(ch.GuildID)
	return mode == "everything" || mode == "create-on-message"
}
func isPrivate(ch *discordgo.Channel) bool {
	return ch.Type == discordgo.ChannelTypeDM || ch.Type == discordgo.ChannelTypeGroupDM
}
func renderName(pattern, fallback string, data any) string {
	if pattern == "" {
		return fallback
	}
	tmpl, err := template.New("name").Parse(pattern)
	if err != nil {
		return fallback
	}
	var b bytes.Buffer
	if tmpl.Execute(&b, data) != nil {
		return fallback
	}
	return b.String()
}
func (c *DiscordClient) spaceInfo(ctx context.Context, p *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	cid := string(p.ID)
	var name, icon, guildID string
	var parent *networkid.PortalID
	if strings.HasPrefix(cid, "guild:") {
		gid := strings.TrimPrefix(cid, "guild:")
		guildID = gid
		if c.guildMode(gid) == "nothing" {
			return nil, fmt.Errorf("guild is not bridged")
		}
		g, err := cachedGuild(c.Session.State, gid)
		if err != nil {
			g, err = c.Session.Guild(gid, discordgo.WithContext(ctx))
		}
		if err != nil {
			return nil, fmt.Errorf("failed to fetch guild")
		}
		name = renderName(c.Main.Config.GuildNameTemplate, g.Name, g)
		icon = g.IconURL("256")
	} else {
		ch, err := c.channel(ctx, strings.TrimPrefix(cid, "category:"))
		if err != nil {
			return nil, err
		}
		if c.guildMode(ch.GuildID) == "nothing" {
			return nil, fmt.Errorf("guild is not bridged")
		}
		name = ch.Name
		guildID = ch.GuildID
		parent = ptr.Ptr(networkid.PortalID("guild:" + ch.GuildID))
	}
	info := &bridgev2.ChatInfo{Name: &name, Type: ptr.Ptr(database.RoomTypeSpace), ParentID: parent, ExtraUpdates: func(_ context.Context, p *bridgev2.Portal) bool {
		meta := p.Metadata.(*PortalMetadata)
		changed := meta.GuildID != guildID
		meta.GuildID = guildID
		return changed
	}}
	if icon != "" {
		info.Avatar = c.avatar(icon)
	}
	return info, nil
}
func (c *DiscordClient) avatar(url string) *bridgev2.Avatar {
	avatar := &bridgev2.Avatar{ID: networkid.AvatarID(url), Remove: url == "", Get: func(ctx context.Context) ([]byte, error) { return c.Main.download(ctx, url) }}
	if url != "" && c.Main.directMedia.Load() {
		if uri, err := c.Main.directURI(c.Main.Bridge.BackgroundCtx, remoteMedia{URL: url}); err == nil {
			avatar.MXC = uri
		}
	}
	return avatar
}

func (c *DiscordClient) enrichChatInfo(ctx context.Context, p *bridgev2.Portal, ch *discordgo.Channel, info *bridgev2.ChatInfo) {
	sc := c.Main.Config.syncFor(ch.ID)
	if isPrivate(ch) {
		if c.Main.Config.DMSpaces {
			info.ParentID = ptr.Ptr(networkid.PortalID("dms:" + string(c.UserLogin.ID)))
		}
		roomType := database.RoomTypeDM
		if ch.Type == discordgo.ChannelTypeGroupDM {
			roomType = database.RoomTypeGroupDM
		}
		info.Type = &roomType
		info.Members.IsFull = true
		info.Members.CheckAllLogins = true
		var names []string
		for _, u := range ch.Recipients {
			name := c.profileName(u)
			names = append(names, name)
			member := bridgev2.ChatMember{EventSender: c.messageSender(u.ID), Membership: event.MembershipJoin}
			if enabled(sc.UserProfiles) {
				member.UserInfo = c.userInfo(u)
			}
			info.Members.MemberMap[networkid.UserID(u.ID)] = member
		}
		if ch.Name == "" && (enabled(sc.RoomName) || p.MXID == "") {
			name := strings.Join(names, ", ")
			info.Name = &name
		}
		if len(ch.Recipients) == 1 && roomType == database.RoomTypeDM {
			info.Members.OtherUserID = networkid.UserID(ch.Recipients[0].ID)
			if enabled(sc.RoomAvatar) || p.MXID == "" {
				info.Avatar = c.avatar(ch.Recipients[0].AvatarURL("256"))
			}
		}
		if roomType == database.RoomTypeGroupDM && (enabled(sc.RoomAvatar) || p.MXID == "") {
			url := ""
			if ch.Icon != "" {
				url = discordgo.EndpointGroupIcon(ch.ID, ch.Icon)
			}
			info.Avatar = c.avatar(url)
		}
	} else {
		var guildName, parentName string
		if guild, err := cachedGuild(c.Session.State, ch.GuildID); err == nil {
			guildName = guild.Name
		}
		if parent, err := cachedChannel(c.Session.State, ch.ParentID); err == nil {
			parentName = parent.Name
		}
		if info.Name != nil {
			name := renderName(c.Main.Config.ChannelNameTemplate, ch.Name, map[string]any{"Name": ch.Name, "ParentName": parentName, "GuildName": guildName, "NSFW": ch.NSFW, "Type": ch.Type})
			info.Name = &name
		}
		if c.Main.Config.GuildSpaces && c.guildMode(ch.GuildID) != "nothing" {
			parent := networkid.PortalID("guild:" + ch.GuildID)
			if ch.ParentID != "" && !isThread(ch) {
				parent = networkid.PortalID("category:" + ch.ParentID)
			}
			info.ParentID = &parent
			if c.Main.Config.RestrictedRooms {
				guild, _ := c.Main.Bridge.DB.Portal.GetByKey(ctx, networkid.PortalKey{ID: networkid.PortalID("guild:" + ch.GuildID)})
				if guild != nil && guild.MXID != "" {
					info.JoinRule = &event.JoinRulesEventContent{JoinRule: event.JoinRuleRestricted, Allow: []event.JoinRuleAllow{{Type: event.JoinRuleAllowRoomMembership, RoomID: guild.MXID}}}
				}
			}
		}
		if p.MXID == "" && c.Main.Config.MuteChannelsOnCreate {
			info.UserLocal = &bridgev2.UserLocalPortalInfo{MutedUntil: ptr.Ptr(event.MutedForever)}
		}
	}
}
func (c *DiscordClient) profileName(u *discordgo.User) string {
	name := u.GlobalName
	if name == "" {
		name = u.Username
	}
	c.stateMu.RLock()
	relationship := c.relationships[u.ID]
	c.stateMu.RUnlock()
	if relationship != nil && relationship.Nickname != "" {
		name = relationship.Nickname
	}
	return renderName(c.Main.Config.DisplaynameTemplate, name, struct {
		*discordgo.User
		Webhook     bool
		Application bool
	}{User: u})
}
func (c *DiscordClient) queueChannel(ch *discordgo.Channel, create bool) {
	if ch == nil || !c.allowed(ch.ID) || isThread(ch) {
		return
	}
	c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatResync, PortalKey: c.portalKey(ch.ID), CreatePortal: create}, GetChatInfoFunc: c.GetChatInfo, CheckNeedsBackfillFunc: func(context.Context, *database.Message) (bool, error) {
		return enabled(c.Main.Config.syncFor(ch.ID).Backfill), nil
	}})
}
func (c *DiscordClient) syncGuild(g *discordgo.Guild) {
	if g != nil {
		g = snapshotGuild(c.Session.State, g)
	}
	if g == nil || c.guildMode(g.ID) == "nothing" {
		return
	}
	if c.Main.Config.GuildSpaces {
		c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatResync, PortalKey: networkid.PortalKey{ID: networkid.PortalID("guild:" + g.ID)}, CreatePortal: true}, GetChatInfoFunc: c.GetChatInfo})
		for _, ch := range g.Channels {
			if ch.Type == discordgo.ChannelTypeGuildCategory {
				c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatResync, PortalKey: networkid.PortalKey{ID: networkid.PortalID("category:" + ch.ID)}, CreatePortal: true}, GetChatInfoFunc: c.GetChatInfo})
			}
		}
	}
	permissions := permissionsState(c.Session.State, g, string(c.UserLogin.ID))
	for _, ch := range g.Channels {
		if perms, err := permissions.UserChannelPermissions(string(c.UserLogin.ID), ch.ID); err == nil && perms&discordgo.PermissionViewChannel == 0 {
			continue
		}
		if supportedChannel(ch) {
			c.queueChannel(ch, c.guildMode(g.ID) == "everything")
		}
	}
	for _, ch := range g.Threads {
		go c.syncThread(ch)
	}
}
func (c *DiscordClient) onReady(ready *discordgo.Ready) {
	ready = snapshotReady(c.Session.State, ready)
	c.stateMu.Lock()
	if c.relationships == nil {
		c.relationships = make(map[string]*discordgo.Relationship)
	}
	for _, r := range ready.Relationships {
		c.relationships[r.ID] = r
	}
	c.stateMu.Unlock()
	c.syncChannels()
	for _, g := range ready.Guilds {
		c.syncGuild(g)
	}
	if c.Session.IsUser && c.Main.Config.SyncDMs {
		for i, ch := range ready.PrivateChannels {
			limit := c.Main.Config.StartupDMLimit
			c.queueChannel(ch, limit < 0 || i < limit)
		}
	}
	if ready.ReadState != nil {
		for _, entry := range ready.ReadState.Entries {
			c.onReadAck(entry.ID, string(entry.LastMessageID))
		}
	}
}
func (c *DiscordClient) onReadAck(cid, mid string) {
	meta, _ := c.eventMeta(c.Main.Bridge.BackgroundCtx, cid, string(c.UserLogin.ID), time.Now(), bridgev2.RemoteEventReadReceipt)
	if meta.PortalKey.ID != "" && enabled(c.Main.Config.syncFor(string(meta.PortalKey.ID)).ReadReceipts) {
		meta.CreatePortal = false
		c.UserLogin.QueueRemoteEvent(&remoteReadReceipt{EventMeta: meta, target: networkid.MessageID(mid)})
	}
}
func (c *DiscordClient) onChannelDelete(ch *discordgo.Channel) {
	if ch == nil || isThread(ch) || !c.allowed(ch.ID) || !c.Main.Config.DeletePortalOnChannelDelete {
		return
	}
	c.UserLogin.QueueRemoteEvent(&simplevent.ChatDelete{EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatDelete, PortalKey: c.portalKey(ch.ID)}, OnlyForMe: isPrivate(ch)})
}

func (c *DiscordClient) updateRelationship(r *discordgo.Relationship, remove bool) {
	if r == nil {
		return
	}
	c.stateMu.Lock()
	if c.relationships == nil {
		c.relationships = make(map[string]*discordgo.Relationship)
	}
	if remove {
		delete(c.relationships, r.ID)
	} else {
		c.relationships[r.ID] = r
	}
	c.stateMu.Unlock()
	if !enabled(c.Main.Config.Sync.UserProfiles) || c.Main.Bridge.Matrix == nil {
		return
	}
	ghost, err := c.Main.Bridge.GetGhostByID(c.Main.Bridge.BackgroundCtx, networkid.UserID(r.ID))
	if err != nil {
		return
	}
	info, err := c.GetUserInfo(c.Main.Bridge.BackgroundCtx, ghost)
	if err == nil {
		ghost.UpdateInfo(c.Main.Bridge.BackgroundCtx, info)
	}
}
