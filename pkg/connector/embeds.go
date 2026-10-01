// SPDX-License-Identifier: AGPL-3.0-or-later
// Embed rendering adapted from the legacy converter (Tulir Asokan, AGPL).
package connector

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
)

const (
	embedHTMLWrapper         = `<blockquote class="discord-embed">%s</blockquote>`
	embedHTMLWrapperColor    = `<blockquote class="discord-embed" background-color="#%06X">%s</blockquote>`
	embedHTMLAuthorWithImage = `<p class="discord-embed-author"><img data-mx-emoticon height="24" src="%s" title="Author icon" alt="">&nbsp;<span>%s</span></p>`
	embedHTMLAuthorPlain     = `<p class="discord-embed-author"><span>%s</span></p>`
	embedHTMLAuthorLink      = `<a href="%s">%s</a>`
	embedHTMLTitleWithLink   = `<p class="discord-embed-title"><a href="%s"><strong>%s</strong></a></p>`
	embedHTMLTitlePlain      = `<p class="discord-embed-title"><strong>%s</strong></p>`
	embedHTMLDescription     = `<p class="discord-embed-description">%s</p>`
	embedHTMLFieldName       = `<th>%s</th>`
	embedHTMLFieldValue      = `<td>%s</td>`
	embedHTMLFields          = `<table class="discord-embed-fields"><tr>%s</tr><tr>%s</tr></table>`
	embedHTMLLinearField     = `<p class="discord-embed-field" x-inline="%s"><strong>%s</strong><br><span>%s</span></p>`
	embedHTMLImage           = `<p class="discord-embed-image"><img src="%s" alt="" title="Embed image"></p>`
	embedHTMLFooterWithImage = `<p class="discord-embed-footer"><sub><img data-mx-emoticon height="20" src="%s" title="Footer icon" alt="">&nbsp;<span>%s</span>%s</sub></p>`
	embedHTMLFooterPlain     = `<p class="discord-embed-footer"><sub><span>%s</span>%s</sub></p>`
	embedHTMLFooterOnlyDate  = `<p class="discord-embed-footer"><sub>%s</sub></p>`
	embedHTMLDate            = `<time datetime="%s">%s</time>`
	embedFooterDateSeparator = ` • `
)

func (portal *discordFormatContext) convertDiscordRichEmbed(ctx context.Context, intent bridgev2.MatrixAPI, embed *discordgo.MessageEmbed) string {
	log := zerolog.Ctx(ctx)
	var htmlParts []string
	if embed.Author != nil {
		var authorHTML string
		authorNameHTML := html.EscapeString(embed.Author.Name)
		if embed.Author.URL != "" {
			authorNameHTML = fmt.Sprintf(embedHTMLAuthorLink, html.EscapeString(embed.Author.URL), authorNameHTML)
		}
		authorHTML = fmt.Sprintf(embedHTMLAuthorPlain, authorNameHTML)
		if embed.Author.ProxyIconURL != "" {
			dbFile, err := portal.client.embedImage(ctx, intent, embed.Author.ProxyIconURL)
			if err != nil {
				log.Warn().Err(err).Msg("Failed to reupload author icon in embed")
			} else {
				authorHTML = fmt.Sprintf(embedHTMLAuthorWithImage, dbFile, authorNameHTML)
			}
		}
		htmlParts = append(htmlParts, authorHTML)
	}
	if embed.Title != "" {
		var titleHTML string
		baseTitleHTML := portal.renderDiscordMarkdownOnlyHTML(embed.Title, false)
		if embed.URL != "" {
			titleHTML = fmt.Sprintf(embedHTMLTitleWithLink, html.EscapeString(embed.URL), baseTitleHTML)
		} else {
			titleHTML = fmt.Sprintf(embedHTMLTitlePlain, baseTitleHTML)
		}
		htmlParts = append(htmlParts, titleHTML)
	}
	if embed.Description != "" {
		htmlParts = append(htmlParts, fmt.Sprintf(embedHTMLDescription, portal.renderDiscordMarkdownOnlyHTML(embed.Description, true)))
	}
	for i := 0; i < len(embed.Fields); i++ {
		item := embed.Fields[i]
		if portal.client.Main.Config.EmbedFieldsAsTables {
			splitItems := []*discordgo.MessageEmbedField{item}
			if item.Inline && len(embed.Fields) > i+1 && embed.Fields[i+1].Inline {
				splitItems = append(splitItems, embed.Fields[i+1])
				i++
				if len(embed.Fields) > i+1 && embed.Fields[i+1].Inline {
					splitItems = append(splitItems, embed.Fields[i+1])
					i++
				}
			}
			headerParts := make([]string, len(splitItems))
			contentParts := make([]string, len(splitItems))
			for j, splitItem := range splitItems {
				headerParts[j] = fmt.Sprintf(embedHTMLFieldName, portal.renderDiscordMarkdownOnlyHTML(splitItem.Name, false))
				contentParts[j] = fmt.Sprintf(embedHTMLFieldValue, portal.renderDiscordMarkdownOnlyHTML(splitItem.Value, true))
			}
			htmlParts = append(htmlParts, fmt.Sprintf(embedHTMLFields, strings.Join(headerParts, ""), strings.Join(contentParts, "")))
		} else {
			htmlParts = append(htmlParts, fmt.Sprintf(embedHTMLLinearField,
				strconv.FormatBool(item.Inline),
				portal.renderDiscordMarkdownOnlyHTML(item.Name, false),
				portal.renderDiscordMarkdownOnlyHTML(item.Value, true),
			))
		}
	}
	if embed.Image != nil {
		dbFile, err := portal.client.embedImage(ctx, intent, embed.Image.ProxyURL)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to reupload image in embed")
		} else {
			htmlParts = append(htmlParts, fmt.Sprintf(embedHTMLImage, dbFile))
		}
	}
	var embedDateHTML string
	if embed.Timestamp != "" {
		formattedTime := embed.Timestamp
		parsedTS, err := time.Parse(time.RFC3339, embed.Timestamp)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to parse timestamp in embed")
		} else {
			formattedTime = parsedTS.Format(discordTimestampStyle('F').Format())
		}
		embedDateHTML = fmt.Sprintf(embedHTMLDate, html.EscapeString(embed.Timestamp), html.EscapeString(formattedTime))
	}
	if embed.Footer != nil {
		var footerHTML string
		var datePart string
		if embedDateHTML != "" {
			datePart = embedFooterDateSeparator + embedDateHTML
		}
		footerHTML = fmt.Sprintf(embedHTMLFooterPlain, html.EscapeString(embed.Footer.Text), datePart)
		if embed.Footer.ProxyIconURL != "" {
			dbFile, err := portal.client.embedImage(ctx, intent, embed.Footer.ProxyIconURL)
			if err != nil {
				log.Warn().Err(err).Msg("Failed to reupload footer icon in embed")
			} else {
				footerHTML = fmt.Sprintf(embedHTMLFooterWithImage, dbFile, html.EscapeString(embed.Footer.Text), datePart)
			}
		}
		htmlParts = append(htmlParts, footerHTML)
	} else if embed.Timestamp != "" {
		htmlParts = append(htmlParts, fmt.Sprintf(embedHTMLFooterOnlyDate, embedDateHTML))
	}

	if len(htmlParts) == 0 {
		return ""
	}

	compiledHTML := strings.Join(htmlParts, "")
	if embed.Color != 0 {
		compiledHTML = fmt.Sprintf(embedHTMLWrapperColor, embed.Color, compiledHTML)
	} else {
		compiledHTML = fmt.Sprintf(embedHTMLWrapper, compiledHTML)
	}
	return compiledHTML
}

func (c *DiscordClient) embedImage(ctx context.Context, intent bridgev2.MatrixAPI, url string) (id.ContentURIString, error) {
	data, err := c.Main.download(ctx, url)
	if err != nil {
		return "", err
	}
	uri, _, err := intent.UploadMedia(ctx, "", data, "embed", http.DetectContentType(data))
	return uri, err
}
func (c *DiscordClient) convertEmbeds(ctx context.Context, p *bridgev2.Portal, intent bridgev2.MatrixAPI, message *discordgo.Message, result *bridgev2.ConvertedMessage, meta *MessageMetadata) error {
	if !enabled(c.Main.Config.syncFor(string(p.ID)).Embeds) {
		return nil
	}
	dc := &discordFormatContext{ctx: ctx, client: c, portal: p, guildID: metaGuild(p), mentions: &event.Mentions{}}
	var body strings.Builder
	var previews []*event.BeeperLinkPreview
	for i, e := range message.Embeds {
		if i == 0 && message.MessageReference == nil && webhookReplyPattern.MatchString(e.Description) {
			continue
		}
		if e.Type == discordgo.EmbedTypeLink || e.Type == discordgo.EmbedTypeArticle || (e.Type == discordgo.EmbedTypeVideo && strings.Contains(message.Content, e.URL)) {
			preview := &event.BeeperLinkPreview{MatchedURL: e.URL}
			preview.Title, preview.Description = e.Title, e.Description
			var url string
			var width, height int
			if e.Image != nil {
				url = e.Image.ProxyURL
				width, height = e.Image.Width, e.Image.Height
			} else if e.Thumbnail != nil {
				url = e.Thumbnail.ProxyURL
				width, height = e.Thumbnail.Width, e.Thumbnail.Height
			}
			if url != "" {
				part, err := c.mediaPart(ctx, p, intent, &discordgo.MessageAttachment{URL: url, Filename: "preview", Width: width, Height: height}, "preview", meta)
				if err == nil && part.Content.Info != nil {
					preview.ImageURL = part.Content.URL
					preview.ImageEncryption = part.Content.File
					preview.ImageWidth = event.IntOrString(width)
					preview.ImageHeight = event.IntOrString(height)
					preview.ImageSize = event.IntOrString(part.Content.Info.Size)
					preview.ImageType = part.Content.Info.MimeType
				}
			}
			previews = append(previews, preview)
			continue
		}
		if e.Type == discordgo.EmbedTypeVideo || e.Type == discordgo.EmbedTypeGifv || (e.Type == discordgo.EmbedTypeImage && e.Video != nil) {
			if e.Video != nil {
				url := e.Video.ProxyURL
				if url == "" {
					url = e.Video.URL
				}
				part, err := c.mediaPart(ctx, p, intent, &discordgo.MessageAttachment{URL: url, Filename: fmt.Sprintf("embed-%d.mp4", i), ContentType: "video/mp4", Width: e.Video.Width, Height: e.Video.Height}, networkid.PartID(fmt.Sprintf("video-%d", i)), meta)
				if err != nil {
					return err
				}
				result.Parts = append(result.Parts, part)
				continue
			}
		}
		body.WriteString(dc.convertDiscordRichEmbed(ctx, intent, e))
	}
	if body.Len() > 0 || len(previews) > 0 {
		var content *event.MessageEventContent
		for _, part := range result.Parts {
			if part.ID == "" {
				content = part.Content
				break
			}
		}
		if content == nil {
			content = &event.MessageEventContent{MsgType: event.MsgText, Mentions: &event.Mentions{}}
			result.Parts = append(result.Parts, &bridgev2.ConvertedMessagePart{ID: "", Type: event.EventMessage, Content: content, DBMetadata: meta})
		}
		content.EnsureHasHTML()
		if content.FormattedBody != "" && body.Len() > 0 {
			content.FormattedBody += "<br><br>"
		}
		content.FormattedBody += body.String()
		content.Body = format.HTMLToText(content.FormattedBody)
		content.Mentions = content.Mentions.Merge(dc.mentions)
		content.BeeperLinkPreviews = previews
	}
	return nil
}
func metaGuild(p *bridgev2.Portal) string {
	if m, ok := p.Metadata.(*PortalMetadata); ok {
		return m.GuildID
	}
	return ""
}
