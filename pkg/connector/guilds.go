// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
)

func validGuildMode(mode string) bool {
	switch mode {
	case "nothing", "if-portal-exists", "create-on-message", "everything":
		return true
	}
	return false
}
func (c *DiscordClient) setGuildMode(ctx context.Context, gid, mode string) error {
	if !validSnowflake(gid) || !validGuildMode(mode) {
		return fmt.Errorf("invalid guild ID or bridging mode")
	}
	guild, err := cachedGuild(c.Session.State, gid)
	if err != nil {
		guild, err = c.Session.Guild(gid, discordgo.WithContext(ctx))
	}
	if err != nil {
		return fmt.Errorf("guild is not accessible to this account")
	}
	c.stateMu.Lock()
	meta := c.UserLogin.Metadata.(*LoginMetadata)
	if meta.GuildModes == nil {
		meta.GuildModes = make(map[string]string)
	}
	old, hadOld := meta.GuildModes[gid]
	meta.GuildModes[gid] = mode
	err = c.UserLogin.Save(ctx)
	if err != nil {
		if hadOld {
			meta.GuildModes[gid] = old
		} else {
			delete(meta.GuildModes, gid)
		}
	}
	c.stateMu.Unlock()
	if err != nil {
		return err
	}
	if mode != "nothing" {
		if c.Main.Config.GuildSpaces {
			p, err := c.Main.Bridge.GetPortalByKey(ctx, c.portalKey("guild:"+gid))
			if err != nil {
				return err
			}
			if p.MXID == "" {
				info, err := c.spaceInfo(ctx, p)
				if err != nil {
					return err
				}
				if err = p.CreateMatrixRoom(ctx, c.UserLogin, info); err != nil {
					return fmt.Errorf("failed to create guild space")
				}
			}
		}
		c.syncGuild(guild)
	}
	return nil
}
func (c *DiscordClient) unbridgeGuild(ctx context.Context, gid string, onlyForMe bool) error {
	if err := c.setGuildMode(ctx, gid, "nothing"); err != nil {
		return err
	}
	portals, err := c.Main.Bridge.DB.Portal.GetAllWithMXID(ctx)
	if err != nil {
		return err
	}
	for _, p := range portals {
		meta := p.Metadata.(*PortalMetadata)
		if meta.GuildID == gid || string(p.ID) == "guild:"+gid {
			c.UserLogin.QueueRemoteEvent(&simplevent.ChatDelete{EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatDelete, PortalKey: p.PortalKey}, OnlyForMe: onlyForMe})
		}
	}
	return nil
}

type guildEntry struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MXID         string `json:"mxid"`
	AvatarURL    string `json:"avatar_url"`
	BridgingMode string `json:"bridging_mode"`
	AutoBridge   bool   `json:"auto_bridge_channels"`
}

func (c *DiscordClient) guildEntries(ctx context.Context) []guildEntry {
	c.Session.State.RLock()
	guilds := append([]*discordgo.Guild(nil), c.Session.State.Guilds...)
	c.Session.State.RUnlock()
	result := make([]guildEntry, 0, len(guilds))
	for _, g := range guilds {
		g = snapshotGuild(c.Session.State, g)
		mode := c.guildMode(g.ID)
		entry := guildEntry{ID: g.ID, Name: g.Name, BridgingMode: mode, AutoBridge: mode == "everything"}
		if c.Main.Bridge.DB != nil {
			p, _ := c.Main.Bridge.DB.Portal.GetByKey(ctx, networkid.PortalKey{ID: networkid.PortalID("guild:" + g.ID)})
			if p != nil {
				entry.MXID = string(p.MXID)
				entry.AvatarURL = string(p.AvatarMXC)
			}
		}
		result = append(result, entry)
	}
	return result
}
func (c *DiscordClient) guildLeft(ctx context.Context, gid string) {
	if !c.Main.Config.DeleteGuildOnLeave {
		return
	}
	portals, err := c.Main.Bridge.DB.Portal.GetAllWithMXID(ctx)
	if err != nil {
		return
	}
	for _, p := range portals {
		if p.Metadata.(*PortalMetadata).GuildID == gid || string(p.ID) == "guild:"+gid || (strings.HasPrefix(string(p.ID), "category:") && p.ParentKey.ID == networkid.PortalID("guild:"+gid)) {
			c.UserLogin.QueueRemoteEvent(&simplevent.ChatDelete{EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatDelete, PortalKey: p.PortalKey}, OnlyForMe: true})
		}
	}
}
