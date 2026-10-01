// SPDX-License-Identifier: AGPL-3.0-or-later
// Markdown implementation adapted from the legacy formatter (Tulir Asokan, AGPL).
package connector

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/util"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/format/mdext"
)

// escapeFixer is a hacky partial fix for the difference in escaping markdown, used with escapeReplacement
//
// Discord allows escaping with just one backslash, e.g. \__a__,
// but standard markdown requires both to be escaped (\_\_a__)
var escapeFixer = regexp.MustCompile(`\\(__[^_]|\*\*[^*])`)

func escapeReplacement(s string) string {
	return s[:2] + `\` + s[2:]
}

// indentableParagraphParser is the default paragraph parser with CanAcceptIndentedLine.
// Used when disabling CodeBlockParser (as disabling it without a replacement will make indented blocks disappear).
type indentableParagraphParser struct {
	parser.BlockParser
}

var defaultIndentableParagraphParser = &indentableParagraphParser{BlockParser: parser.NewParagraphParser()}

func (b *indentableParagraphParser) CanAcceptIndentedLine() bool {
	return true
}

var removeFeaturesExceptLinks = []any{
	parser.NewListParser(), parser.NewListItemParser(), parser.NewHTMLBlockParser(), parser.NewRawHTMLParser(),
	parser.NewSetextHeadingParser(), parser.NewThematicBreakParser(),
	parser.NewCodeBlockParser(),
}
var removeFeaturesAndLinks = append(removeFeaturesExceptLinks, parser.NewLinkParser())
var fixIndentedParagraphs = goldmark.WithParserOptions(parser.WithBlockParsers(util.Prioritized(defaultIndentableParagraphParser, 500)))
var discordExtensions = goldmark.WithExtensions(extension.Strikethrough, mdext.SimpleSpoiler, mdext.DiscordUnderline, ExtDiscordEveryone, ExtDiscordTag)

var discordRenderer = goldmark.New(
	goldmark.WithParser(mdext.ParserWithoutFeatures(removeFeaturesAndLinks...)),
	fixIndentedParagraphs, format.HTMLOptions, discordExtensions,
)
var discordRendererWithInlineLinks = goldmark.New(
	goldmark.WithParser(mdext.ParserWithoutFeatures(removeFeaturesExceptLinks...)),
	fixIndentedParagraphs, format.HTMLOptions, discordExtensions,
)

func (portal *discordFormatContext) renderDiscordMarkdownOnlyHTMLNoUnwrap(text string, allowInlineLinks bool) string {
	text = escapeFixer.ReplaceAllStringFunc(text, escapeReplacement)

	var buf strings.Builder
	ctx := parser.NewContext()
	ctx.Set(parserContextPortal, portal)
	renderer := discordRenderer
	if allowInlineLinks {
		renderer = discordRendererWithInlineLinks
	}
	err := renderer.Convert([]byte(text), &buf, parser.WithContext(ctx))
	if err != nil {
		panic(fmt.Errorf("markdown parser errored: %w", err))
	}
	return buf.String()
}

func (portal *discordFormatContext) renderDiscordMarkdownOnlyHTML(text string, allowInlineLinks bool) string {
	return format.UnwrapSingleParagraph(portal.renderDiscordMarkdownOnlyHTMLNoUnwrap(text, allowInlineLinks))
}

func (c *DiscordClient) discordText(ctx context.Context, portal *bridgev2.Portal, text string) event.MessageEventContent {
	guildID := ""
	if meta, ok := portal.Metadata.(*PortalMetadata); ok {
		guildID = meta.GuildID
	}
	dc := &discordFormatContext{ctx: ctx, client: c, portal: portal, guildID: guildID, mentions: &event.Mentions{}}
	body := dc.renderDiscordMarkdownOnlyHTML(text, true)
	if strings.Contains(body, `class="discord-mention-`) {
		dc.mentions.Room = true
	}
	return event.MessageEventContent{MsgType: event.MsgText, Body: format.HTMLToText(body), Format: event.FormatHTML, FormattedBody: body, Mentions: dc.mentions}
}
