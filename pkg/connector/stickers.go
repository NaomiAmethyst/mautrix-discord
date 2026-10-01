// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

type AnimatedStickerConfig struct {
	Target string `yaml:"target"`
	Width  int    `yaml:"width"`
	Height int    `yaml:"height"`
	FPS    int    `yaml:"fps"`
}

func (c *DiscordClient) lottieSticker(ctx context.Context, p *bridgev2.Portal, intent bridgev2.MatrixAPI, sticker *discordgo.StickerItem, meta *MessageMetadata) (*bridgev2.ConvertedMessagePart, error) {
	partID := networkid.PartID("sticker-" + sticker.ID)
	notice := noticePart(partID, "[Animated sticker: "+sticker.Name+"]", meta)
	cfg := c.Main.Config.AnimatedSticker
	if cfg.Target == "" || cfg.Target == "disable" {
		return notice, nil
	}
	data, err := c.Main.download(ctx, discordgo.EndpointStickerImage(sticker.ID, discordgo.StickerFormatTypeLottie))
	if err != nil {
		return notice, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "discord-lottie-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	target, mime := cfg.Target, "image/"+cfg.Target
	if target == "webm" {
		mime = "video/webm"
	}
	output := filepath.Join(dir, "output."+target)
	converterTarget := target
	if target == "webm" || target == "webp" {
		converterTarget = "pngs"
		output = filepath.Join(dir, "frame_")
	}
	if target == "png" {
		cfg.FPS = 1
	}
	cmd := exec.CommandContext(ctx, "lottieconverter", "-", output, converterTarget, fmt.Sprintf("%dx%d", cfg.Width, cfg.Height), strconv.Itoa(cfg.FPS))
	cmd.Stdin = bytes.NewReader(data)
	if err = cmd.Run(); err != nil {
		return notice, nil
	}
	if converterTarget == "pngs" {
		codec := "libwebp_anim"
		if target == "webm" {
			codec = "libvpx-vp9"
		}
		frames := output
		output = filepath.Join(dir, "output."+target)
		err = exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-y", "-framerate", strconv.Itoa(cfg.FPS), "-pattern_type", "glob", "-i", frames+"*.png", "-c:v", codec, "-pix_fmt", "yuva420p", output).Run()
		if err != nil {
			return notice, nil
		}
	}
	stat, err := os.Stat(output)
	if err != nil || stat.Size() > c.Main.fileLimit() {
		return notice, nil
	}
	data, err = os.ReadFile(output)
	if err != nil {
		return notice, nil
	}
	uri, file, err := intent.UploadMedia(ctx, p.MXID, data, sticker.Name, mime)
	if err != nil {
		return nil, err
	}
	content := &event.MessageEventContent{Body: sticker.Name, URL: uri, File: file, Info: &event.FileInfo{MimeType: mime, Size: len(data), Width: cfg.Width, Height: cfg.Height}}
	if file != nil {
		content.URL = ""
	}
	return &bridgev2.ConvertedMessagePart{ID: partID, Type: event.EventSticker, Content: content, DBMetadata: meta}, nil
}
