// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/event"
)

var discordModerator = event.Type{Type: "fi.mau.discord.admin", Class: event.StateEventType}

func (d *DiscordConnector) registerPortalCommands(processor *commands.Processor) {
	processor.AddHandlers(&commands.FullHandler{Name: "login-qr", RequiresLoginPermission: true, Help: commands.HelpMeta{Section: commands.HelpSectionAuth, Description: "Log in by scanning a Discord QR code"}, Func: func(ce *commands.Event) { ce.Args = []string{"qr"}; commands.CommandLogin.Func(ce) }}, &commands.FullHandler{Name: "create-portal", RequiresLogin: true, Help: commands.HelpMeta{Section: commands.HelpSectionChats, Description: "Create a Matrix room for a selected Discord channel", Args: "<channel ID>"}, Func: func(ce *commands.Event) {
		c, err := commandClient(ce)
		if err != nil {
			ce.Reply("%s", err)
			return
		}
		if len(ce.Args) == 1 && validSnowflake(ce.Args[0]) && c.Session.IsUser {
			_, _ = c.channel(ce.Ctx, ce.Args[0])
		}
		if len(ce.Args) != 1 || !c.allowed(ce.Args[0]) || (!c.Session.IsUser && !ce.User.Permissions.Admin) {
			ce.Reply("Select an allowed channel; relay bot channels require an admin")
			return
		}
		p, err := d.Bridge.GetPortalByKey(ce.Ctx, c.portalKey(ce.Args[0]))
		if err != nil {
			ce.Reply("Could not load channel portal")
			return
		}
		if p.MXID != "" {
			ce.Reply("Already bridged: %s", p.MXID)
			return
		}
		info, err := c.GetChatInfo(ce.Ctx, p)
		if err == nil {
			err = p.CreateMatrixRoom(ce.Ctx, c.UserLogin, info)
		}
		if err == nil && !c.Session.IsUser {
			if !d.Bridge.Config.Relay.Enabled {
				ce.Reply("Portal created; enable relay in the config before relaying")
				return
			}
			err = p.SetRelay(ce.Ctx, c.UserLogin)
			if err == nil {
				_, _, err = c.ensureWebhook(ce.Ctx, p)
			}
		}
		if err != nil {
			ce.Reply("Failed to create channel portal")
		} else {
			ce.Reply("Portal created: %s", p.MXID)
		}
	}}, &commands.FullHandler{Name: "bridge", RequiresLogin: true, RequiresEventLevel: discordModerator, Help: commands.HelpMeta{Section: commands.HelpSectionChats, Description: "Bind a selected Discord channel to this Matrix room", Args: "[--replace[=delete]] <channel ID>"}, Func: func(ce *commands.Event) {
		c, err := commandClient(ce)
		if err != nil {
			ce.Reply("%s", err)
			return
		}
		cid := ""
		replace, deleteOld := false, false
		for _, arg := range ce.Args {
			switch arg {
			case "--replace":
				replace = true
			case "--replace=delete":
				replace, deleteOld = true, true
			default:
				if cid != "" || !validSnowflake(arg) {
					ce.Reply("Usage: `$cmdprefix bridge [--replace[=delete]] <channel ID>`")
					return
				}
				cid = arg
			}
		}
		if cid != "" && c.Session.IsUser {
			_, _ = c.channel(ce.Ctx, cid)
		}
		if cid == "" || !c.allowed(cid) || (!c.Session.IsUser && !ce.User.Permissions.Admin) {
			ce.Reply("Channel is unavailable or not permitted")
			return
		}
		p, err := d.Bridge.GetPortalByKey(ce.Ctx, c.portalKey(cid))
		if err != nil {
			ce.Reply("Could not load portal")
			return
		}
		if p.MXID != "" && p.MXID != ce.RoomID {
			if !replace {
				ce.Reply("Already bridged; use --replace to move the mapping")
				return
			}
			if !ce.User.Permissions.Admin {
				pls, err := d.Bridge.Matrix.GetPowerLevels(ce.Ctx, p.MXID)
				if err != nil || pls.GetUserLevel(ce.User.MXID) < pls.GetEventLevel(discordModerator) {
					ce.Reply("You need permission to unbridge the old room")
					return
				}
			}
		}
		d.bindMu.Lock()
		defer d.bindMu.Unlock()
		if current, err := d.Bridge.GetPortalByMXID(ce.Ctx, ce.RoomID); err != nil || (current != nil && current.PortalKey != p.PortalKey) {
			ce.Reply("Room is already bridged to another channel")
			return
		}
		if err = d.Bridge.Bot.EnsureJoined(ce.Ctx, ce.RoomID); err != nil {
			ce.Reply("Invite the bridge bot to this room first")
			return
		}
		pls, err := d.Bridge.Matrix.GetPowerLevels(ce.Ctx, ce.RoomID)
		if err != nil || pls.GetUserLevel(d.Bridge.Bot.GetMXID()) < pls.GetEventLevel(event.StateBridge) {
			ce.Reply("The bridge bot needs permission to send m.bridge state")
			return
		}
		info, err := c.GetChatInfo(ce.Ctx, p)
		if err != nil {
			ce.Reply("Could not fetch channel info")
			return
		}
		if !enabled(d.Config.syncFor(cid).RoomName) {
			info.Name = nil
		}
		if !c.Session.IsUser {
			if !d.Bridge.Config.Relay.Enabled {
				ce.Reply("Enable bridge.relay.enabled first")
				return
			}
			if _, _, err = c.ensureWebhook(ce.Ctx, p); err != nil {
				ce.Reply("Failed to prepare webhook")
				return
			}
		}
		if err = p.UpdateMatrixRoomID(ce.Ctx, ce.RoomID, bridgev2.UpdateMatrixRoomIDParams{FailIfMXIDSet: !replace, DeleteOldRoom: deleteOld}); err != nil {
			ce.Reply("Failed to bind portal")
			return
		}
		c.UserLogin.MarkInPortal(ce.Ctx, p)
		p.UpdateInfo(ce.Ctx, info, c.UserLogin, nil, time.Time{})
		if !c.Session.IsUser {
			if err = p.SetRelay(ce.Ctx, c.UserLogin); err != nil {
				ce.Reply("Room bound, but enabling relay failed")
				return
			}
		}
		ce.Reply("Channel %s is bridged to this room", cid)
	}}, &commands.FullHandler{Name: "unbridge", RequiresPortal: true, RequiresEventLevel: discordModerator, Help: commands.HelpMeta{Section: commands.HelpSectionChats, Description: "Remove this room's Discord mapping without deleting the room"}, Func: func(ce *commands.Event) {
		if err := d.detachPortal(ce.Ctx, ce.Portal); err != nil {
			ce.Reply("Failed to remove mapping")
			return
		}
		ce.Reply("Room unbridged. Remove declarative room_id mappings from config to prevent rebinding on reconnect.")
	}}, &commands.FullHandler{Name: "login-token", RequiresLoginPermission: true, Help: commands.HelpMeta{Section: commands.HelpSectionAuth, Description: "Log in using a Discord token", Args: "<user/bot/oauth> <token>"}, Func: func(ce *commands.Event) {
		defer ce.Redact()
		if len(ce.Args) != 2 {
			ce.Reply("Usage: `$cmdprefix login-token <user/bot/oauth> <token>`")
			return
		}
		flow := map[string]string{"bot": "bot-token", "user": "user-token", "oauth": "oauth-token"}[strings.ToLower(ce.Args[0])]
		process, err := d.CreateLogin(ce.Ctx, ce.User, flow)
		if err != nil {
			ce.Reply("Login flow is disabled or not permitted")
			return
		}
		defer process.Cancel()
		step, err := process.(*TokenLogin).SubmitUserInput(ce.Ctx, map[string]string{"token": ce.Args[1]})
		if err != nil {
			ce.Reply("Discord rejected login")
			return
		}
		ce.Reply("Logged in as %s (%s)", step.CompleteParams.UserLogin.RemoteName, step.CompleteParams.UserLoginID)
	}})
}
func (c *DiscordClient) requireDMAccess(cid string) error {
	if !c.Session.IsUser {
		return nil
	}
	ch, err := cachedChannel(c.Session.State, cid)
	if err != nil || !isPrivate(ch) {
		return nil
	}
	if !c.Main.Config.ForbidDMingStrangers {
		return nil
	}
	for _, u := range ch.Recipients {
		if u.Bot {
			continue
		}
		c.stateMu.RLock()
		r := c.relationships[u.ID]
		c.stateMu.RUnlock()
		if r == nil || r.Type != 1 {
			return fmt.Errorf("messaging users who are not Discord friends is disabled")
		}
	}
	return nil
}

func (d *DiscordConnector) detachPortal(ctx context.Context, p *bridgev2.Portal) error {
	members, err := d.Bridge.Matrix.GetMembers(ctx, p.MXID)
	if err != nil {
		return fmt.Errorf("failed to list room members")
	}
	stateKey := string(p.BridgeID)
	if d.Bridge.Config.NoBridgeInfoStateKey {
		stateKey = ""
	} else if provider, ok := d.Bridge.Matrix.(bridgev2.MatrixConnectorWithBridgeIdentifier); ok {
		stateKey = provider.GetUniqueBridgeID()
	}
	for _, kind := range []event.Type{event.StateBridge, event.StateHalfShotBridge, event.StateBeeperRoomFeatures} {
		if _, err := d.Bridge.Bot.SendState(ctx, p.MXID, kind, stateKey, &event.Content{Raw: map[string]any{}}, time.Time{}); err != nil {
			return fmt.Errorf("failed to clear bridge state")
		}
	}
	roomID := p.MXID
	if err := p.Delete(ctx); err != nil {
		return err
	}
	// Leave only bridge-controlled identities. Keep human members and the room.
	for mxid, member := range members {
		if member.Membership != event.MembershipJoin && member.Membership != event.MembershipInvite {
			continue
		}
		uid, isGhost := d.Bridge.Matrix.ParseGhostMXID(mxid)
		if !isGhost {
			continue
		}
		_, err := d.Bridge.Matrix.GhostIntent(uid).SendState(ctx, roomID, event.StateMember, string(mxid), &event.Content{Parsed: &event.MemberEventContent{Membership: event.MembershipLeave, Reason: "Unbridging room"}}, time.Time{})
		if err != nil {
			d.Bridge.Log.Warn().Msg("Failed to leave unbridged room with a ghost")
		}
	}
	_, err = d.Bridge.Bot.SendState(ctx, roomID, event.StateMember, string(d.Bridge.Bot.GetMXID()), &event.Content{Parsed: &event.MemberEventContent{Membership: event.MembershipLeave, Reason: "Unbridging room"}}, time.Time{})
	if err != nil {
		d.Bridge.Log.Warn().Msg("Failed to leave unbridged room with bridge bot")
	}
	return nil
}
