// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"fmt"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func (c *DiscordClient) dmSpaceInfo(p *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	if !c.Session.IsUser || !c.Main.Config.PersonalAccounts || !c.Main.Config.DMSpaces || string(p.ID) != "dms:"+string(c.UserLogin.ID) {
		return nil, fmt.Errorf("DM space is not accessible to this account")
	}
	return &bridgev2.ChatInfo{Name: ptr.Ptr("Discord DMs"), Type: ptr.Ptr(database.RoomTypeSpace), Members: &bridgev2.ChatMemberList{MemberMap: map[networkid.UserID]bridgev2.ChatMember{networkid.UserID(c.UserLogin.ID): {EventSender: c.messageSender(string(c.UserLogin.ID)), Membership: event.MembershipJoin}}}}, nil
}

func (d *DiscordConnector) registerSpaceCommands(processor *commands.Processor) {
	processor.AddHandlers(&commands.FullHandler{Name: "ping", Help: commands.HelpMeta{Section: commands.HelpSectionAuth, Description: "Check your connection to Discord"}, Func: func(ce *commands.Event) {
		c, err := commandClient(ce)
		if err != nil {
			ce.Reply("You are not logged in to Discord")
			return
		}
		if !c.gatewayConnected.Load() {
			ce.Reply("Logged in as %s (%s), but the Discord gateway is disconnected", c.UserLogin.RemoteName, c.UserLogin.ID)
			return
		}
		ce.Reply("Connected as %s (%s)", c.UserLogin.RemoteName, c.UserLogin.ID)
	}}, &commands.FullHandler{Name: "rejoin-space", RequiresLogin: true, Help: commands.HelpMeta{Section: commands.HelpSectionChats, Description: "Ask for an invite to your main, DM, or selected guild space", Args: "<main/dms/guild ID>"}, Func: func(ce *commands.Event) {
		c, err := commandClient(ce)
		if err != nil || len(ce.Args) != 1 {
			ce.Reply("Usage: `$cmdprefix rejoin-space <main/dms/guild ID>`")
			return
		}
		var roomID id.RoomID
		if strings.EqualFold(ce.Args[0], "main") {
			roomID, err = c.UserLogin.GetSpaceRoom(ce.Ctx)
			if err == nil && roomID == "" {
				ce.Reply("Enable bridge.personal_filtering_spaces to create a main space")
				return
			}
		} else {
			pid := ""
			if strings.EqualFold(ce.Args[0], "dms") && c.Session.IsUser && d.Config.DMSpaces {
				pid = "dms:" + string(c.UserLogin.ID)
			} else if validSnowflake(ce.Args[0]) && c.guildMode(ce.Args[0]) != "nothing" && d.Config.GuildSpaces {
				pid = "guild:" + ce.Args[0]
			}
			if pid == "" {
				ce.Reply("This space is disabled or not selected for your account")
				return
			}
			p, fetchErr := d.Bridge.GetPortalByKey(ce.Ctx, networkid.PortalKey{ID: networkid.PortalID(pid)})
			if fetchErr != nil {
				ce.Reply("Could not load space")
				return
			}
			if p.MXID == "" {
				info, fetchErr := c.GetChatInfo(ce.Ctx, p)
				if fetchErr != nil {
					ce.Reply("Could not load space metadata")
					return
				}
				err = p.CreateMatrixRoom(ce.Ctx, c.UserLogin, info)
			}
			roomID = p.MXID
		}
		if err == nil {
			err = d.Bridge.Bot.EnsureInvited(ce.Ctx, roomID, ce.User.MXID)
		}
		if err != nil {
			ce.Reply("Could not invite you to the space")
			return
		}
		ce.Reply("Invited you to %s", roomID.URI().MatrixToURL())
	}})
}
