// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/id"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
)

var discordHTMLParser = &format.HTMLParser{
	TabsToSpaces: 4, Newline: "\n", HorizontalLine: "\n---\n",
	ItalicConverter:    func(text string, _ format.Context) string { return "*" + text + "*" },
	UnderlineConverter: func(text string, _ format.Context) string { return "__" + text + "__" },
	SpoilerConverter: func(text, reason string, _ format.Context) string {
		if reason != "" {
			return "(" + reason + ") ||" + text + "||"
		}
		return "||" + text + "||"
	},
	TextConverter: func(text string, ctx format.Context) string {
		if ctx.TagStack.Has("pre") || ctx.TagStack.Has("code") {
			return text
		}
		return escapeDiscordMarkdown(text)
	},
}

func (c *DiscordClient) matrixText(ctx context.Context, portal *bridgev2.Portal, content *event.MessageEventContent) string {
	if content.Format != event.FormatHTML || content.FormattedBody == "" {
		return escapeDiscordMarkdown(content.Body)
	}
	parser := *discordHTMLParser
	parser.LinkConverter = func(text, href string, _ format.Context) string {
		allowPreview := content.BeeperLinkPreviews == nil
		for _, preview := range content.BeeperLinkPreviews {
			if preview.MatchedURL == href {
				allowPreview = true
				break
			}
		}
		if text == href {
			if allowPreview {
				return href
			}
			return "<" + href + ">"
		}
		if !discordLinkRegexFull.MatchString(href) {
			return escapeDiscordMarkdown(text) + " (" + escapeDiscordMarkdown(href) + ")"
		}
		if !allowPreview {
			href = "<" + href + ">"
		}
		return "[" + escapeDiscordMarkdown(text) + "](" + href + ")"
	}
	parser.PillConverter = func(displayname, mxid, eventID string, _ format.Context) string {
		br := c.Main.Bridge
		if br.Matrix == nil {
			return displayname
		}
		if strings.HasPrefix(mxid, "@") {
			if content.Mentions != nil && !slices.Contains(content.Mentions.UserIDs, id.UserID(mxid)) {
				return displayname
			}
			if uid, ok := br.Matrix.ParseGhostMXID(id.UserID(mxid)); ok {
				return "<@" + string(uid) + ">"
			}
			user, err := br.GetUserByMXID(ctx, id.UserID(mxid))
			if err == nil && user != nil {
				if login := user.GetDefaultLogin(); login != nil {
					return "<@" + string(login.ID) + ">"
				}
			}
		} else if strings.HasPrefix(mxid, "!") && br.DB != nil {
			p, err := br.DB.Portal.GetByMXID(ctx, id.RoomID(mxid))
			if err == nil && p != nil {
				if eventID == "" {
					return "<#" + string(p.ID) + ">"
				}
				msg, _ := br.DB.Message.GetPartByMXID(ctx, id.EventID(eventID))
				if msg != nil {
					guild := p.Metadata.(*PortalMetadata).GuildID
					if guild == "" {
						guild = "@me"
					}
					return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guild, messageChannel(msg, &bridgev2.Portal{Portal: p}), msg.ID)
				}
			}
		}
		return displayname
	}
	parser.ImageConverter = func(src, alt, title, width, height string, isEmoji bool) string {
		if isEmoji {
			if emoji, err := c.Main.discordEmoji(ctx, src); err == nil {
				return "<:" + emoji + ">"
			}
		}
		return alt
	}
	return parser.Parse(content.FormattedBody, format.NewContext(ctx))
}

var outgoingUserMention = regexp.MustCompile(`<@!?(\d+)>`)

func (c *DiscordClient) allowedMentions(ctx context.Context, portal *bridgev2.Portal, content *event.MessageEventContent, sender id.UserID, text string) *discordgo.MessageAllowedMentions {
	result := &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
	if !c.Main.Config.AllowMentions {
		return result
	}
	for _, m := range outgoingUserMention.FindAllStringSubmatch(text, -1) {
		result.Users = append(result.Users, m[1])
	}
	result.RepliedUser = content.Mentions == nil || len(content.Mentions.UserIDs) > 0
	if content.Mentions != nil && content.Mentions.Room && c.Main.Bridge.Matrix != nil {
		pls, err := c.Main.Bridge.Matrix.GetPowerLevels(ctx, portal.MXID)
		if err == nil && pls.GetUserLevel(sender) >= pls.Notifications.Room() {
			result.Parse = append(result.Parse, discordgo.AllowedMentionTypeEveryone)
		}
	}
	return result
}

const discordLinkPattern = `https?://[^<\p{Zs}\x{feff}]*[^"'),.:;\]\p{Zs}\x{feff}]`

// Discord links start with http:// or https://, contain at least two characters afterwards,
// don't contain < or whitespace anywhere, and don't end with "'),.:;]
//
// Zero-width whitespace is mostly in the Format category and is allowed, except \uFEFF isn't for some reason
var discordLinkRegex = regexp.MustCompile(discordLinkPattern)
var discordLinkRegexFull = regexp.MustCompile("^" + discordLinkPattern + "$")

var discordMarkdownEscaper = strings.NewReplacer(
	`\`, `\\`,
	`_`, `\_`,
	`*`, `\*`,
	`~`, `\~`,
	"`", "\\`",
	`|`, `\|`,
	`<`, `\<`,
	`#`, `\#`,
)

func escapeDiscordMarkdown(s string) string {
	submatches := discordLinkRegex.FindAllStringIndex(s, -1)
	if submatches == nil {
		return discordMarkdownEscaper.Replace(s)
	}
	var builder strings.Builder
	offset := 0
	for _, match := range submatches {
		start := match[0]
		end := match[1]
		builder.WriteString(discordMarkdownEscaper.Replace(s[offset:start]))
		builder.WriteString(s[start:end])
		offset = end
	}
	builder.WriteString(discordMarkdownEscaper.Replace(s[offset:]))
	return builder.String()
}
