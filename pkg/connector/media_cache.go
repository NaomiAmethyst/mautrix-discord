// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type cachedMedia struct {
	URI  id.ContentURIString
	File *event.EncryptedFileInfo
	MIME string
	Size int
}

var errMediaDownload = errors.New("Discord media download failed")

func (d *DiscordConnector) transferMedia(ctx context.Context, intent bridgev2.MatrixAPI, p *bridgev2.Portal, rawURL, name, mime string) (cachedMedia, error) {
	encrypted := d.roomEncrypted(ctx, p)
	cache := d.Config.CacheMedia == "always" || (d.Config.CacheMedia == "unencrypted" && !encrypted)
	var key database.Key
	if cache && d.Bridge.DB != nil {
		parsed, _ := url.Parse(rawURL)
		keyURL := rawURL
		if parsed != nil {
			parsed.RawQuery = ""
			keyURL = parsed.String()
		}
		key = database.Key(fmt.Sprintf("discord.media.%x", sha256.Sum256([]byte(fmt.Sprintf("%t:%s", encrypted, keyURL)))))
		d.mediaMu.Lock()
		defer d.mediaMu.Unlock()
		var cached cachedMedia
		if raw := d.Bridge.DB.KV.Get(ctx, key); raw != "" && json.Unmarshal([]byte(raw), &cached) == nil && cached.URI != "" && cached.MIME != "" {
			return cached, nil
		}
	}
	data, err := d.download(ctx, rawURL)
	if err != nil {
		return cachedMedia{}, errMediaDownload
	}
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	uri, file, err := intent.UploadMedia(ctx, p.MXID, data, name, mime)
	if err != nil {
		return cachedMedia{}, fmt.Errorf("failed to upload Discord attachment to Matrix: %w", err)
	}
	cached := cachedMedia{URI: uri, File: file, MIME: mime, Size: len(data)}
	if key != "" {
		encoded, _ := json.Marshal(cached)
		d.Bridge.DB.KV.Set(ctx, key, string(encoded))
	}
	return cached, nil
}

func (d *DiscordConnector) roomEncrypted(ctx context.Context, p *bridgev2.Portal) bool {
	if mx, ok := d.Bridge.Matrix.(*matrix.Connector); ok && mx.StateStore != nil {
		encrypted, err := mx.StateStore.IsEncrypted(ctx, p.MXID)
		return err != nil || encrypted
	}
	return true
}
