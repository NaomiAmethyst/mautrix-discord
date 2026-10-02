// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"testing"
)

// Relayed messages go through a webhook under the sender's own name, so the
// framework mustn't prefix the name onto them too.
func TestRelayedMessagesAreLeftAsTheyAre(t *testing.T) {
	config := defaultConfig(t)
	config.Channels = []ChannelConfig{{ID: "123"}}
	client := testClient(t, config, nil)
	portal := testPortal(t, client)
	if !client.GetCapabilities(context.Background(), portal).PerMessageProfileRelay {
		t.Fatal("a bridged channel's messages would get relay formatting as well as the webhook's name")
	}

	client = testClient(t, defaultConfig(t), nil)
	portal = testPortal(t, client)
	if client.GetCapabilities(context.Background(), portal).PerMessageProfileRelay {
		t.Fatal("a channel that isn't bridged claimed to relay")
	}
}
