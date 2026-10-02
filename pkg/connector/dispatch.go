// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"reflect"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/rs/zerolog"
)

// beeper/discordgo only dispatches gateway events to Session.EventHandler: its
// typed Session.AddHandler dispatch is disabled, so handlers added that way are
// never called. The client keeps its own typed handlers instead, and runs them
// from a queue, one event at a time in the order they arrived, so a slow one (an
// edit fetching its message) never holds up the gateway's reader.

const eventQueueSize = 1024

// on adds a handler for one kind of gateway event: func(*discordgo.Session, *discordgo.T).
func (c *DiscordClient) on(handler any) {
	fn := reflect.ValueOf(handler)
	t := fn.Type()
	if t.Kind() != reflect.Func || t.NumIn() != 2 || t.In(0) != reflect.TypeOf((*discordgo.Session)(nil)) {
		panic("discord event handlers are func(*discordgo.Session, *discordgo.T)")
	}
	if c.handlers == nil {
		c.handlers = make(map[reflect.Type][]reflect.Value)
	}
	c.handlers[t.In(1)] = append(c.handlers[t.In(1)], fn)
}

// startDispatch has the session's events handled by the handlers added with on.
// Call it once they're all added, before the session's opened.
func (c *DiscordClient) startDispatch() {
	c.events = make(chan any, eventQueueSize)
	c.stopped = make(chan struct{})
	events, stopped := c.events, c.stopped
	c.Session.EventHandler = func(evt any) {
		if len(c.handlers[reflect.TypeOf(evt)]) == 0 {
			return
		}
		select {
		case events <- evt:
		case <-stopped:
		}
	}
	go func() {
		for {
			select {
			case evt := <-events:
				c.handle(evt)
			case <-stopped:
				return
			}
		}
	}()
}

func (c *DiscordClient) handle(evt any) {
	defer func() {
		if err := recover(); err != nil {
			c.UserLogin.Log.Error().Any("panic", err).Str("event_type", reflect.TypeOf(evt).String()).
				Msg("Discord event handler panicked")
		}
	}()
	args := []reflect.Value{reflect.ValueOf(c.Session), reflect.ValueOf(evt)}
	for _, fn := range c.handlers[reflect.TypeOf(evt)] {
		fn.Call(args)
	}
}

// stopDispatch stops handling events: the client's been replaced, or logged out.
func (c *DiscordClient) stopDispatch() {
	c.stopOnce.Do(func() {
		if c.stopped != nil {
			close(c.stopped)
		}
	})
}

// discordLogger passes discordgo's own logging (the gateway connecting, closing,
// reconnecting) to the login's log.
func discordLogger(log zerolog.Logger) func(msgL, caller int, format string, a ...any) {
	return func(msgL, caller int, format string, a ...any) {
		level := zerolog.DebugLevel
		switch msgL {
		case discordgo.LogError:
			level = zerolog.ErrorLevel
		case discordgo.LogWarning:
			level = zerolog.WarnLevel
		case discordgo.LogInformational:
			level = zerolog.InfoLevel
		}
		log.WithLevel(level).Msgf(strings.TrimSpace(format), a...) // zerolog-allow-msgf
	}
}
