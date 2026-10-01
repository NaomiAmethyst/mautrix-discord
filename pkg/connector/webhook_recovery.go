// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"context"
	"errors"
	"io"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridgev2"
)

// Retry only a definitive rejection of a deleted/invalid managed webhook.
// Timeouts and other uncertain send outcomes must not cause a second message.
func (c *DiscordClient) recoverWebhook(ctx context.Context, p *bridgev2.Portal, failedID string, sendErr error, files []*discordgo.File) (string, string, bool) {
	var restErr *discordgo.RESTError
	if !errors.As(sendErr, &restErr) || restErr.Message == nil || (restErr.Message.Code != discordgo.ErrCodeUnknownWebhook && restErr.Message.Code != discordgo.ErrCodeInvalidWebhookTokenProvided) {
		return "", "", false
	}
	if config := c.Main.Config.channel(string(p.ID)); config != nil && config.WebhookURL != "" {
		return "", "", false
	}
	if !c.Main.Config.CreateWebhooks {
		return "", "", false
	}
	for _, file := range files {
		seeker, ok := file.Reader.(io.Seeker)
		if !ok {
			return "", "", false
		}
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return "", "", false
		}
	}
	c.Main.webhookMu.Lock()
	meta := p.Metadata.(*PortalMetadata)
	if meta.WebhookID == failedID {
		oldToken := meta.WebhookToken
		meta.WebhookID, meta.WebhookToken = "", ""
		if err := p.Save(ctx); err != nil {
			meta.WebhookID, meta.WebhookToken = failedID, oldToken
			c.Main.webhookMu.Unlock()
			return "", "", false
		}
	}
	c.Main.webhookMu.Unlock()
	webhookID, token, err := c.ensureWebhook(ctx, p)
	return webhookID, token, err == nil
}
