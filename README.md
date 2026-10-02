# mautrix-discord

A Matrix–Discord bridge based on [bridgev2](https://github.com/mautrix/go/tree/main/bridgev2)
and the [Discord client fork used by the original bridge](https://github.com/beeper/discordgo).

The default configuration supports admin-managed bot relay with per-channel
webhooks. Admins select channels, bind them to Matrix rooms through config,
commands, or provisioning, and choose which messages and metadata are synced.
Matrix members can use the relay without a Discord login.

Optional personal accounts restore the legacy token/QR login, DMs/group DMs,
guild discovery and spaces, native replies, read state, and application commands.
The port also includes rich formatting/embeds, custom emoji reuse, thread
creation and history, direct media, and animated sticker conversion.

See [setup, provisioning, and sync controls](docs/bridgev2.md),
[example-config.yaml](example-config.yaml), and the [feature matrix](ROADMAP.md).
Build with Go 1.26 or newer and a C compiler using `./build.sh`.
Test with `go test -race -tags goolm ./...`.

Use a fresh database and appservice registration. Database migration from the
original bridge is outside this port's scope. The original implementation is
preserved in the separate [legacy module](legacy/).

Automated tests use mocked Discord/Matrix services, SQLite, and a local QR
websocket exchange. Live Discord/homeserver validation is still required before
using this version in production.

## Discussion

Matrix room: [#discord:maunium.net](https://matrix.to/#/#discord:maunium.net)
