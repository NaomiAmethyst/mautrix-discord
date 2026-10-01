// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"go.mau.fi/mautrix-discord/pkg/connector"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
)

// Set by build.sh using linker flags.
var Tag, Commit, BuildTime = "unknown", "unknown", "unknown"

var bridge = mxmain.BridgeMain{
	Name:        "mautrix-discord",
	URL:         "https://github.com/mautrix/discord",
	Description: "A Matrix-Discord bridge with admin-configured webhook relay.",
	Version:     "0.8.0",
	Connector:   &connector.DiscordConnector{},
}

func main() {
	bridge.PostStart = func() {
		if bridge.Matrix.Provisioning != nil {
			bridge.Connector.(*connector.DiscordConnector).RegisterProvisioning(bridge.Matrix.Provisioning)
		}
	}
	bridge.InitVersion(Tag, Commit, BuildTime)
	bridge.Run()
}
