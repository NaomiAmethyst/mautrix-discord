// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"fmt"
	"strings"

	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

func (d *DiscordConnector) registerCommands() {
	processor, ok := d.Bridge.Commands.(*commands.Processor)
	if !ok {
		return
	}
	d.registerAccountCommands(processor)
	d.registerSpaceCommands(processor)
	d.registerPortalCommands(processor)
	processor.AddHandlers(&commands.FullHandler{
		Name: "bridge-channel", RequiresAdmin: true,
		Help: commands.HelpMeta{Section: commands.HelpSectionChats, Description: "Bridge an allowlisted Discord channel to this room and enable webhook relay", Args: "<channel ID> [login ID]"},
		Func: func(ce *commands.Event) {
			if len(ce.Args) < 1 || len(ce.Args) > 2 {
				ce.Reply("Usage: `$cmdprefix bridge-channel <channel ID> [login ID]`")
				return
			}
			var loginID string
			if len(ce.Args) == 2 {
				loginID = ce.Args[1]
			}
			client, err := d.adminLogin(ce.User, loginID)
			if err != nil {
				ce.Reply("%s", err)
				return
			}
			if _, err = d.bindChannel(ce.Ctx, client, ce.Args[0], ce.RoomID); err != nil {
				ce.Reply("Failed to bridge channel: %s", err)
				return
			}
			ce.Reply("Bridged channel %s and enabled webhook relay", ce.Args[0])
		},
	}, &commands.FullHandler{
		Name: "channels", RequiresAdmin: true,
		Help: commands.HelpMeta{Section: commands.HelpSectionChats, Description: "List admin-configured Discord channels"},
		Func: func(ce *commands.Event) {
			var lines []string
			for _, ch := range d.Config.Channels {
				portal, err := d.Bridge.GetExistingPortalByKey(ce.Ctx, networkid.PortalKey{ID: networkid.PortalID(ch.ID)})
				if err != nil {
					ce.Reply("Failed to load channels")
					return
				}
				status := "unbridged"
				if portal != nil && portal.MXID != "" {
					status = string(portal.MXID)
				}
				lines = append(lines, fmt.Sprintf("* `%s`: %s", ch.ID, status))
			}
			if len(lines) == 0 {
				ce.Reply("No channels configured. Add channels to `network.channels` and restart.")
				return
			}
			ce.Reply("%s", strings.Join(lines, "\n"))
		},
	})
}
