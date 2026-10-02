// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// beeper/discordgo calls Session.EventHandler for every gateway event, and nothing
// added with Session.AddHandler; the client's own handlers must run from it.
func TestGatewayEventsReachHandlersInOrder(t *testing.T) {
	client := testClient(t, defaultConfig(t), nil)
	var mu sync.Mutex
	var seen []string
	done := make(chan struct{})
	client.on(func(_ *discordgo.Session, evt *discordgo.MessageCreate) {
		mu.Lock()
		seen = append(seen, "message "+evt.ID)
		mu.Unlock()
		if evt.ID == "3" {
			close(done)
		}
	})
	client.on(func(_ *discordgo.Session, _ *discordgo.Ready) {
		mu.Lock()
		seen = append(seen, "ready")
		mu.Unlock()
	})
	client.startDispatch()
	client.Session.EventHandler(&discordgo.Ready{})
	client.Session.EventHandler(&discordgo.TypingStart{}) // nobody handles it
	for _, id := range []string{"1", "2", "3"} {
		client.Session.EventHandler(&discordgo.MessageCreate{Message: &discordgo.Message{ID: id}})
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("events weren't handled")
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(seen, []string{"ready", "message 1", "message 2", "message 3"}) {
		t.Fatalf("handled %v", seen)
	}
}

func TestStoppedClientsHandleNothing(t *testing.T) {
	client := testClient(t, defaultConfig(t), nil)
	handled := make(chan struct{}, 1)
	client.on(func(_ *discordgo.Session, _ *discordgo.Ready) { handled <- struct{}{} })
	client.startDispatch()
	client.stopDispatch()
	client.stopDispatch()
	client.Session.EventHandler(&discordgo.Ready{})
	select {
	case <-handled:
		t.Fatal("a stopped client handled an event")
	case <-time.After(200 * time.Millisecond):
	}
}

// What the bridge registers is reachable through the session.
func TestRegisteredEventsAreDispatched(t *testing.T) {
	client := testClient(t, defaultConfig(t), nil)
	client.registerEvents()
	t.Cleanup(client.stopDispatch)
	if client.Session.EventHandler == nil {
		t.Fatal("no EventHandler: gateway events would be dropped")
	}
	for _, evt := range []any{&discordgo.Ready{}, &discordgo.MessageCreate{}, &discordgo.MessageReactionAdd{},
		&discordgo.Disconnect{}} {
		if len(client.handlers[reflect.TypeOf(evt)]) == 0 {
			t.Errorf("nothing handles %T", evt)
		}
	}
}
