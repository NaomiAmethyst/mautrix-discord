// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/event"
)

func markSpoilers(files []*discordgo.File, evt *event.Event) {
	if evt == nil || evt.Content.Raw == nil || evt.Content.Raw["page.codeberg.everypizza.msc4193.spoiler"] != true {
		return
	}
	for _, file := range files {
		if !strings.HasPrefix(file.Name, "SPOILER_") {
			file.Name = "SPOILER_" + file.Name
		}
	}
}

// Upload URLs are supplied by the authenticated Discord attachment API. Never
// forward a Discord token to the upload server or follow a redirect with it.
func (c *DiscordClient) prepareAttachments(ctx context.Context, cid string, files []*discordgo.File) ([]*discordgo.MessageAttachment, error) {
	var result []*discordgo.MessageAttachment
	for i, file := range files {
		data, err := io.ReadAll(io.LimitReader(file.Reader, c.Main.fileLimit()+1))
		if err != nil || int64(len(data)) > c.Main.fileLimit() {
			return nil, fmt.Errorf("attachment exceeds transfer limit or could not be read")
		}
		fileID := strconv.Itoa(i)
		prepared, err := c.Session.ChannelAttachmentCreate(cid, &discordgo.ReqPrepareAttachments{Files: []*discordgo.FilePrepare{{ID: fileID, Name: file.Name, Size: len(data), OriginalContentType: file.ContentType}}}, discordgo.WithContext(ctx))
		if err != nil || prepared == nil || len(prepared.Attachments) != 1 {
			return nil, fmt.Errorf("Discord attachment preparation failed")
		}
		att := prepared.Attachments[0]
		u, err := url.Parse(att.UploadURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || att.UploadFilename == "" {
			return nil, fmt.Errorf("Discord returned an invalid attachment upload URL")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("Discord attachment upload failed")
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := c.Main.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Discord attachment upload failed")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("Discord attachment upload failed")
		}
		result = append(result, &discordgo.MessageAttachment{ID: strconv.FormatInt(att.ID, 10), Filename: file.Name, UploadedFilename: att.UploadFilename, OriginalContentType: file.ContentType})
	}
	return result, nil
}
