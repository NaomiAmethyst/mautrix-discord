// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Downloads are limited to Discord's media hosts, including redirects, and
// bounded while streaming rather than trusting the Content-Length header.
func discordMediaURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil &&
		(u.Host == "cdn.discordapp.com" || u.Host == "media.discordapp.net" || u.Host == "cdn.discord.com")
}
func (d *DiscordConnector) download(ctx context.Context, rawURL string) ([]byte, error) {
	if !discordMediaURL(rawURL) {
		return nil, fmt.Errorf("unsupported Discord media URL")
	}
	client := *d.HTTP
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !discordMediaURL(req.URL.String()) {
			return fmt.Errorf("unsupported Discord media redirect")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid Discord media URL")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Discord media download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Discord media download returned HTTP %d", resp.StatusCode)
	}
	limit := d.fileLimit()
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("attachment exceeds the configured transfer limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read Discord media")
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("attachment exceeds the configured transfer limit")
	}
	return data, nil
}

func safeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	return name
}
