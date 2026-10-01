// SPDX-License-Identifier: AGPL-3.0-or-later
// Command argument parsing adapted from the legacy implementation (Tulir Asokan, AGPL).
package connector

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/google/shlex"
	"maunium.net/go/mautrix/bridgev2/commands"
)

func getCommandOptionTypeName(optType discordgo.ApplicationCommandOptionType) string {
	switch optType {
	case discordgo.ApplicationCommandOptionSubCommand:
		return "subcommand"
	case discordgo.ApplicationCommandOptionSubCommandGroup:
		return "subcommand group (unsupported)"
	case discordgo.ApplicationCommandOptionString:
		return "string"
	case discordgo.ApplicationCommandOptionInteger:
		return "integer"
	case discordgo.ApplicationCommandOptionBoolean:
		return "boolean"
	case discordgo.ApplicationCommandOptionUser:
		return "user (unsupported)"
	case discordgo.ApplicationCommandOptionChannel:
		return "channel (unsupported)"
	case discordgo.ApplicationCommandOptionRole:
		return "role (unsupported)"
	case discordgo.ApplicationCommandOptionMentionable:
		return "mentionable (unsupported)"
	case discordgo.ApplicationCommandOptionNumber:
		return "number"
	case discordgo.ApplicationCommandOptionAttachment:
		return "attachment (unsupported)"
	default:
		return fmt.Sprintf("unknown type %d", optType)
	}
}

func parseCommandOptionValue(optType discordgo.ApplicationCommandOptionType, value string) (any, error) {
	switch optType {
	case discordgo.ApplicationCommandOptionSubCommandGroup:
		return nil, fmt.Errorf("subcommand groups aren't supported")
	case discordgo.ApplicationCommandOptionString:
		return value, nil
	case discordgo.ApplicationCommandOptionInteger:
		return strconv.ParseInt(value, 10, 64)
	case discordgo.ApplicationCommandOptionBoolean:
		return strconv.ParseBool(value)
	case discordgo.ApplicationCommandOptionUser:
		return nil, fmt.Errorf("user options aren't supported")
	case discordgo.ApplicationCommandOptionChannel:
		return nil, fmt.Errorf("channel options aren't supported")
	case discordgo.ApplicationCommandOptionRole:
		return nil, fmt.Errorf("role options aren't supported")
	case discordgo.ApplicationCommandOptionMentionable:
		return nil, fmt.Errorf("mentionable options aren't supported")
	case discordgo.ApplicationCommandOptionNumber:
		return strconv.ParseFloat(value, 64)
	case discordgo.ApplicationCommandOptionAttachment:
		return nil, fmt.Errorf("attachment options aren't supported")
	default:
		return nil, fmt.Errorf("unknown option type %d", optType)
	}
}

func indent(text, with string) string {
	split := strings.Split(text, "\n")
	for i, part := range split {
		split[i] = with + part
	}
	return strings.Join(split, "\n")
}

func formatOption(opt *discordgo.ApplicationCommandOption) string {
	argText := fmt.Sprintf("* `%s`: %s", opt.Name, getCommandOptionTypeName(opt.Type))
	if strings.ToLower(opt.Description) != opt.Name {
		argText += fmt.Sprintf(" - %s", opt.Description)
	}
	if opt.Required {
		argText += " (required)"
	}
	if len(opt.Options) > 0 {
		subopts := make([]string, len(opt.Options))
		for i, subopt := range opt.Options {
			subopts[i] = indent(formatOption(subopt), "  ")
		}
		argText += "\n" + strings.Join(subopts, "\n")
	}
	return argText
}

func formatCommand(cmd *discordgo.ApplicationCommand) string {
	baseText := fmt.Sprintf("$cmdprefix exec %s", cmd.Name)
	if len(cmd.Options) > 0 {
		args := make([]string, len(cmd.Options))
		argPlaceholder := "[arg=value ...]"
		for i, opt := range cmd.Options {
			args[i] = formatOption(opt)
			if opt.Required {
				argPlaceholder = "<arg=value ...>"
			}
		}
		baseText = fmt.Sprintf("`%s %s` - %s\n%s", baseText, argPlaceholder, cmd.Description, strings.Join(args, "\n"))
	} else {
		baseText = fmt.Sprintf("`%s` - %s", baseText, cmd.Description)
	}
	return baseText
}

func parseCommandOptions(opts []*discordgo.ApplicationCommandOption, subcommands []string, namedArgs map[string]string) (res []*discordgo.ApplicationCommandOptionInput, err error) {
	subcommandDone := false
	for _, opt := range opts {
		optRes := &discordgo.ApplicationCommandOptionInput{
			Type: opt.Type,
			Name: opt.Name,
		}
		if opt.Type == discordgo.ApplicationCommandOptionSubCommand {
			if !subcommandDone && len(subcommands) > 0 && subcommands[0] == opt.Name {
				subcommandDone = true
				optRes.Options, err = parseCommandOptions(opt.Options, subcommands[1:], namedArgs)
				if err != nil {
					err = fmt.Errorf("error parsing subcommand %s: %v", opt.Name, err)
					break
				}
				subcommands = subcommands[1:]
			} else {
				continue
			}
		} else if argVal, ok := namedArgs[opt.Name]; ok {
			optRes.Value, err = parseCommandOptionValue(opt.Type, argVal)
			if err != nil {
				err = fmt.Errorf("error parsing parameter %s: %v", opt.Name, err)
				break
			}
		} else if opt.Required {
			switch opt.Type {
			case discordgo.ApplicationCommandOptionSubCommandGroup, discordgo.ApplicationCommandOptionUser,
				discordgo.ApplicationCommandOptionChannel, discordgo.ApplicationCommandOptionRole,
				discordgo.ApplicationCommandOptionMentionable, discordgo.ApplicationCommandOptionAttachment:
				err = fmt.Errorf("missing required parameter %s (which is not supported by the bridge)", opt.Name)
			default:
				err = fmt.Errorf("missing required parameter %s", opt.Name)
			}
			break
		} else {
			continue
		}
		res = append(res, optRes)
	}
	if len(subcommands) > 0 {
		err = fmt.Errorf("unparsed subcommands left over (did you forget quoting for parameters with spaces?)")
	}
	return
}

func executeCommand(cmd *discordgo.ApplicationCommand, args []string) (res []*discordgo.ApplicationCommandOptionInput, err error) {
	namedArgs := map[string]string{}
	n := 0
	for _, arg := range args {
		name, value, isNamed := strings.Cut(arg, "=")
		if isNamed {
			namedArgs[name] = value
		} else {
			args[n] = arg
			n++
		}
	}
	return parseCommandOptions(cmd.Options, args[:n], namedArgs)
}

func commandClient(ce *commands.Event) (*DiscordClient, error) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		return nil, fmt.Errorf("log in first")
	}
	client, ok := login.Client.(*DiscordClient)
	if !ok || !client.IsLoggedIn() {
		return nil, fmt.Errorf("Discord login unavailable")
	}
	return client, nil
}
func (d *DiscordConnector) registerAccountCommands(processor *commands.Processor) {
	processor.AddHandlers(&commands.FullHandler{Name: "commands", Aliases: []string{"cmds", "cs"}, RequiresPortal: true, RequiresLogin: true, Help: commands.HelpMeta{Section: commands.HelpSectionChats, Description: "Search Discord application commands or view their arguments", Args: "<search/help> <query/name>"}, Func: func(ce *commands.Event) {
		c, err := commandClient(ce)
		if err != nil {
			ce.Reply("%s", err)
			return
		}
		if len(ce.Args) != 2 || !c.Session.IsUser || !c.allowed(string(ce.Portal.ID)) {
			ce.Reply("Usage: `$cmdprefix commands <search/help> <query>` with a personal account in a bridged channel")
			return
		}
		results, err := c.Session.ApplicationCommandsSearch(string(ce.Portal.ID), ce.Args[1], discordgo.WithContext(ce.Ctx))
		if err != nil {
			ce.Reply("Discord command search failed")
			return
		}
		var lines []string
		for _, cmd := range results {
			if ce.Args[0] == "search" || cmd.Name == ce.Args[1] {
				lines = append(lines, formatCommand(cmd))
			}
		}
		if len(lines) == 0 {
			ce.Reply("No commands found")
		} else {
			ce.Reply("%s", strings.Join(lines, "\n"))
		}
	}}, &commands.FullHandler{Name: "exec", Aliases: []string{"command", "cmd", "e"}, RequiresPortal: true, RequiresLogin: true, Help: commands.HelpMeta{Section: commands.HelpSectionChats, Description: "Execute a Discord application command", Args: "<command> [subcommand] [arg=value ...]"}, Func: func(ce *commands.Event) {
		c, err := commandClient(ce)
		if err != nil {
			ce.Reply("%s", err)
			return
		}
		if !c.Session.IsUser || !c.allowed(string(ce.Portal.ID)) {
			ce.Reply("Application commands require your own Discord account in a bridged channel")
			return
		}
		args, err := shlex.Split(ce.RawArgs)
		if err != nil || len(args) == 0 {
			ce.Reply("Usage: `$cmdprefix exec <command> [arg=value ...]`")
			return
		}
		results, err := c.Session.ApplicationCommandsSearch(string(ce.Portal.ID), args[0], discordgo.WithContext(ce.Ctx))
		if err != nil {
			ce.Reply("Discord command lookup failed")
			return
		}
		var cmd *discordgo.ApplicationCommand
		for _, item := range results {
			if item.Name == args[0] {
				cmd = item
				break
			}
		}
		if cmd == nil {
			ce.Reply("Command not found")
			return
		}
		options, err := executeCommand(cmd, args[1:])
		if err != nil {
			ce.Reply("Invalid arguments: %s", err)
			return
		}
		ch, err := c.channel(ce.Ctx, string(ce.Portal.ID))
		if err != nil {
			ce.Reply("%s", err)
			return
		}
		token := nonce()
		copyEvent := *ce
		copyEvent.Ctx = d.Bridge.BackgroundCtx
		c.interactions.Store(token, &copyEvent)
		if err = c.Session.SendInteractions(ch.GuildID, ch.ID, cmd, options, token, discordgo.WithContext(ce.Ctx)); err != nil {
			c.interactions.Delete(token)
			ce.Reply("Discord rejected the application command")
			return
		}
		time.AfterFunc(10*time.Second, func() {
			if pending, ok := c.interactions.LoadAndDelete(token); ok {
				pending.(*commands.Event).Reply("Timed out waiting for Discord to accept the command")
			}
		})
	}}, &commands.FullHandler{Name: "guilds", Aliases: []string{"servers"}, RequiresLogin: true, Help: commands.HelpMeta{Section: commands.HelpSectionChats, Description: "Manage Discord guild bridging", Args: "<status/bridge/unbridge/bridging-mode> [guild ID] [mode/--entire]"}, Func: func(ce *commands.Event) {
		c, err := commandClient(ce)
		if err != nil {
			ce.Reply("%s", err)
			return
		}
		if !c.Session.IsUser && !ce.User.Permissions.Admin {
			ce.Reply("Only admins may configure relay bot guilds")
			return
		}
		if len(ce.Args) == 0 || ce.Args[0] == "status" {
			var lines []string
			for _, g := range c.guildEntries(ce.Ctx) {
				lines = append(lines, fmt.Sprintf("%s (%s): %s", g.Name, g.ID, g.BridgingMode))
			}
			ce.Reply("%s", strings.Join(lines, "\n"))
			return
		}
		if len(ce.Args) < 2 {
			ce.Reply("Specify a guild ID")
			return
		}
		gid := ce.Args[1]
		switch ce.Args[0] {
		case "bridge":
			mode := "create-on-message"
			if len(ce.Args) > 2 && ce.Args[2] == "--entire" {
				mode = "everything"
			}
			err = c.setGuildMode(ce.Ctx, gid, mode)
		case "bridging-mode":
			if len(ce.Args) < 3 {
				ce.Reply("Current mode: %s", c.guildMode(gid))
				return
			}
			err = c.setGuildMode(ce.Ctx, gid, ce.Args[2])
		case "unbridge":
			if !ce.User.Permissions.Admin {
				ce.Reply("Only admins can unbridge a guild")
				return
			}
			err = c.unbridgeGuild(ce.Ctx, gid, false)
		default:
			ce.Reply("Unknown guild operation")
			return
		}
		if err != nil {
			ce.Reply("%s", err)
		} else {
			ce.Reply("Guild configuration updated")
		}
	}}, &commands.FullHandler{Name: "disconnect", RequiresLogin: true, Help: commands.HelpMeta{Section: commands.HelpSectionAuth, Description: "Disconnect your Discord account"}, Func: func(ce *commands.Event) {
		c, err := commandClient(ce)
		if err != nil {
			ce.Reply("%s", err)
			return
		}
		c.Disconnect()
		ce.Reply("Disconnected")
	}}, &commands.FullHandler{Name: "reconnect", RequiresLogin: true, Help: commands.HelpMeta{Section: commands.HelpSectionAuth, Description: "Reconnect your Discord account"}, Func: func(ce *commands.Event) {
		c, err := commandClient(ce)
		if err != nil {
			ce.Reply("%s", err)
			return
		}
		c.Disconnect()
		go c.Connect(d.Bridge.BackgroundCtx)
		ce.Reply("Reconnecting")
	}})
}
