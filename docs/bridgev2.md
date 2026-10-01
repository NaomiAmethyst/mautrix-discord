# Bridgev2 setup and provisioning

The default deployment is an admin-managed Discord bot relay using per-channel
webhooks. Personal accounts and guild discovery are optional. Use a **fresh
bridgev2 database and appservice registration**; the bridgev1 database schema is
not migrated. The old implementation remains in `legacy/` as a separate module.

## Admin-managed relay

1. Build with Go 1.24+ and a C compiler: `./build.sh`. The default build uses
   goolm; it does not require libolm. The Docker image includes sticker conversion
   tools. For a local build, install `lottieconverter` and `ffmpeg` if needed.
2. Copy `example-config.yaml` to `config.yaml` and configure the homeserver,
   appservice, database, encryption, provisioning secret, and Matrix permissions.
3. Keep `bridge.split_portals: false`, `bridge.relay.enabled: true`, and
   `bridge.relay.admin_only: true` for shared relay rooms. Set your Matrix account
   to `admin`; other participating users need `relay` permission.
4. Add selected Discord channel IDs to `network.channels`. Keep `network.guilds`
   empty and `network.personal_accounts: false` for explicit-channel-only relay.
5. Run `./mautrix-discord -g -c config.yaml -r registration.yaml`, register the
   generated registration with the homeserver, and start the bridge.
6. Create a Discord bot, enable Message Content Intent, and add it to the guild.
   Selected channels need View Channel, Read Message History, Send Messages,
   and Manage Webhooks. Enable Add Reactions and thread permissions when using
   those features. A configured existing webhook can avoid Manage Webhooks.
7. Log in using provisioning or `login bot-token` in a bridge management room.
   Bot login requires bridge admin permission.
8. Invite the bridge bot into each existing Matrix room and grant permission to
   send `m.bridge` and the state you want it to manage, usually power level 50.
9. Bind through config, provisioning, or `!discord bridge-channel <channel ID>
   [login ID]` in the destination room. Binding enables relay and rejects
   conflicting mappings.

The repository example already has relay message formats without a sender
prefix, because the Discord webhook username supplies attribution. If using a
framework-generated example, configure these defaults yourself:

```yaml
bridge:
    personal_filtering_spaces: false
    split_portals: false
    relay:
        enabled: true
        admin_only: true
        default_relays: []
        displayname_format: "{{ .DisambiguatedName }}"
        message_formats:
            m.text: "{{ .Message }}"
            m.notice: "{{ .Message }}"
            m.emote: "{{ .Message }}"
            m.file: "{{ .Caption }}"
            m.image: "{{ .Caption }}"
            m.audio: "{{ .Caption }}"
            m.video: "{{ .Caption }}"
            m.location: "{{ .Message }}"
    permissions:
        "*": relay
        "@admin:example.org": admin
```

A declarative mapping is restored on connection:

```yaml
network:
    channels:
        - id: "123456789012345678"
          room_id: "!existing-room:example.org"
          relay_login_id: "987654321098765432"
          # Optional existing incoming webhook:
          # webhook_url: "https://discord.com/api/webhooks/WEBHOOK_ID/TOKEN"
          sync:
              room_name: false
              room_topic: false
              typing: false
```

`relay_login_id` is the Discord bot user ID returned on login completion. It
restricts that channel to the selected login and is required with `room_id`.
Without `room_id`, create or bind the portal using commands/provisioning; that
mapping persists. Removing a channel from the explicit list stops its sync unless
it is independently selected through a guild mode. It does not delete history.
Changing the config requires a restart.

Webhook credentials are persisted and reused. `create_webhooks: false` requires
an existing configured/stored webhook. Configured URLs are checked against the
Discord channel. If a managed webhook was deleted, the bridge can replace it
following a definitive Discord rejection; configured webhook URLs must be fixed
by the admin. Tokens and webhook credentials are not returned by provisioning.

For webhook avatars, enable `public_media.enabled` and set
`appservice.public_address` to an HTTPS address reachable by Discord. Bridgev2
supplies signed media URLs. Without public media, sender names still work.
Mentions are suppressed by default. `allow_mentions: true` allows explicit user
mentions; everyone/here additionally requires Matrix room-notification permission.
Relay reactions use the bot's identity, so multiple Matrix users share one Discord
reaction per emoji.

## Personal accounts and guilds

Set `network.personal_accounts: true` to enable `user-token`, `oauth-token`, and
`qr` login flows. Personal users need Matrix bridge `user` permission. Bot login
and changes to bot guild modes remain admin-only. DMs use receiver-specific
portals; `sync_dms` disables DM discovery, and
`startup_private_channel_create_limit` bounds initial DM room creation. New DMs
can be discovered when their messages arrive.

Set `dm_spaces: true` to group DMs into a dedicated Matrix space, and enable
`bridge.personal_filtering_spaces` for a per-login main space. `rejoin-space`
can restore an invitation to the main, DM, or selected guild space.

Personal-account Matrix messages use native Discord sends and reply references.
Other Matrix members use a portal's relay login if one is configured. Native
bridgev2 double puppeting handles messages sent from other Discord clients and
own read state; configure its `double_puppet` section as appropriate.

Guild selection is separate from the explicit channel list. Config example:

```yaml
network:
    personal_accounts: true
    guilds:
        - id: "123456789012345678"
          mode: create-on-message
    guild_spaces: true
    sync:
        read_receipts: true
        threads: true
        backfill: true
backfill:
    enabled: true
```

| Guild mode | Behavior |
| --- | --- |
| `nothing` | No discovery; explicit channel entries still apply |
| `if-portal-exists` | Sync already bridged guild channels |
| `create-on-message` | Create channel portals when a message arrives |
| `everything` | Create accessible guild channel portals during discovery |

Choose per-login modes using `guilds bridge <guild ID>`, `guilds bridging-mode
<guild ID> <mode>`, or provisioning. Saved per-login modes override the configured
guild defaults. `guild_spaces` creates guild/category hierarchy;
`restricted_rooms` makes channel rooms joinable through their guild space when
that space exists. `mute_channels_on_create` applies initial Matrix muting.
Deletion after a Discord channel deletion or guild departure is separately opt-in.
Explicit guild unbridging requires bridge admin permission.

Other commands include `create-portal`, `bridge [--replace[=delete]]`, `unbridge`,
`disconnect`, `reconnect`, `login-token`, and `login-qr`, plus bridgev2's native
login, relay, and portal management commands. `unbridge` removes the mapping and
bridge identities while keeping the room and human members. Remove a declarative
`room_id` entry to prevent rebinding. `commands` and `exec` provide Discord
application command search/help and execution for personal accounts, with the
argument types supported by legacy.

## Provisioning

Native provisioning uses `/_matrix/provision/v3`. Supply
`Authorization: Bearer <provisioning.shared_secret>` and `user_id=<Matrix ID>`.
Matrix access-token authentication is also supported when
`provisioning.allow_matrix_auth` permits it.

```sh
curl -H "Authorization: Bearer $PROVISIONING_SECRET" \
  'http://localhost:29334/_matrix/provision/v3/login/flows?user_id=@admin:example.org'

curl -X POST -H "Authorization: Bearer $PROVISIONING_SECRET" \
  'http://localhost:29334/_matrix/provision/v3/login/start/bot-token?user_id=@admin:example.org'
```

The start response contains a process `login_id`, `step_id`, and input fields.
Submit the token to the returned step:

```sh
curl -H "Authorization: Bearer $PROVISIONING_SECRET" \
  -H 'Content-Type: application/json' \
  --data '{"token":"YOUR_DISCORD_BOT_TOKEN"}' \
  'http://localhost:29334/_matrix/provision/v3/login/step/PROCESS_ID/fi.mau.discord.login.bot_token/user_input?user_id=@admin:example.org'
```

`complete.user_login_id` is the Discord account ID. The gateway connects
asynchronously. Native `whoami`, `logins`, and `logout/{loginID}` routes manage
account state. QR login uses the framework's display-and-wait flow: start `qr`,
display the returned challenge, and follow the API's next-step instructions.
Logging out disconnects the account; it does not rotate its Discord token or
remove Discord webhooks.

Admin channel listing and existing-room binding:

```sh
curl -H "Authorization: Bearer $PROVISIONING_SECRET" \
  'http://localhost:29334/_matrix/provision/v3/discord/channels?user_id=@admin:example.org'

curl -H "Authorization: Bearer $PROVISIONING_SECRET" \
  -H 'Content-Type: application/json' \
  --data '{"room_id":"!room:example.org","login_id":"987654321098765432"}' \
  'http://localhost:29334/_matrix/provision/v3/discord/channels/123456789012345678/bridge?user_id=@admin:example.org'
```

Guild discovery uses `GET /v3/discord/guilds` and `POST
/v3/discord/guilds/{guildID}` with `{"mode":"create-on-message"}`. Personal users
can select their guilds; bot users require admin. `DELETE` unbridges the guild
and requires admin. The explicit channel endpoints bind selected channels;
guild endpoints separately control discovery.

Compatibility routes under `/_matrix/provision/v1` are:

| Method | Route |
| --- | --- |
| GET | `/ping`, `/guilds` |
| POST | `/disconnect`, `/reconnect`, `/logout`, `/login/token` |
| GET, WebSocket | `/login/qr` |
| POST, DELETE | `/guilds/{guildID}` |

They use the same framework authentication and the user's default login. Legacy
token login accepts `Bot ` or `Bearer ` prefixes; an unprefixed token selects the
personal-account flow. Personal flows must be enabled in config. Prefer v3 for
new clients; v1 preserves the account/guild request shapes, rather than the old
bridge's entire authentication or custom-prefix configuration.

## Sync controls and media

`network.sync` supplies defaults. Each explicit channel's `sync` overrides only
the fields it specifies, including explicit `false`. Threads sharing a parent
portal inherit the parent's controls.

| Setting | Default | Behavior |
| --- | --- | --- |
| `messages` | true | Message bodies/media, edits, and history |
| `attachments` | true | File/image/video/audio transfers in both directions |
| `embeds` | true | Rich embeds and previews; outgoing preview suppression |
| `stickers` | true | Incoming Discord stickers and outgoing Matrix stickers |
| `edits` | true | Incoming edits; edits of own native/relay messages |
| `deletes` | true | Incoming deletions; deletion of own native/relay messages |
| `reactions` | true | Unicode and cached Discord custom emoji reactions |
| `typing` | false | Typing for logged-in senders |
| `read_receipts` | false | Own Discord read state; requires a personal account |
| `user_profiles` | true | Learn sender metadata, guild/webhook per-message profiles |
| `room_name` | false | Channel/room names in both directions |
| `room_topic` | false | Channel/room topics in both directions |
| `room_avatar` | false | Incoming DM/group DM avatar updates |
| `threads` | false | Threads inside their parent Matrix room, including creation/joining |
| `backfill` | false | Channel/thread history through bridgev2 |

Created rooms receive initial names even with ongoing name sync disabled.
Binding an existing room preserves its name/topic when their controls are false.
Ghost profiles are shared between portals; disabling profile updates in one room
does not erase profiles already learned elsewhere.

History also requires top-level `backfill.enabled: true`. Framework settings
control batch queues and initial/catch-up/thread limits; Discord fetches are
bounded to 100 messages per API call and returned chronologically. Relay echoes
are excluded. Enable `autojoin_threads` to join when Matrix receipts indicate a
thread was opened; the join-notice reaction also explicitly joins a thread.

Media transfers honor both `network.max_attachment_size` and Matrix's upload
limit. Unavailable or oversized incoming files become notices; outgoing oversized
media is rejected. `cache_media` controls reuse of uploads and downloads.
`use_discord_cdn_upload` selects Discord's separate upload API for personal sends;
webhooks continue using multipart uploads. `custom_emoji_reactions: false` uses
shortcodes instead of MXC reaction keys. Previously bridged custom emoji can be
reused; uploading arbitrary new Discord emoji is beyond legacy parity.

For Lottie stickers, configure `animated_sticker.target` as `png`, `gif`, `webp`,
or `webm`, with bounded dimensions/FPS. `disable` produces a notice. Conversion
also falls back to a notice when its tool is unavailable or fails. The Docker
runtime includes Alpine's [lottieconverter](https://pkgs.alpinelinux.org/package/v3.22/community/x86_64/lottieconverter)
and [ffmpeg](https://pkgs.alpinelinux.org/package/v3.22/community/x86_64/ffmpeg) packages.
`embed_fields_as_tables` selects the rich embed layout.

Direct media uses the framework's top-level `direct_media` configuration. It
creates signed Matrix media IDs and refreshes expiring Discord attachment URLs
using an account that can access the channel. Encrypted-room attachments still
use encrypted Matrix uploads. Emoji/avatar/sticker media may use direct media.
Downloads and redirects are restricted to Discord CDN hosts.

Name templates and proxy settings are documented in the network example.
The proxy applies to Discord REST, gateway, QR, and media traffic. Text sends
are bounded to Discord's 2000 UTF-16 code-unit limit. Enable `bridge.cross_room_replies` for references between already bridged,
selected channels. Relay replies use preview
embeds with jump links because incoming webhooks lack native reply references.

## Validation boundaries

Automated tests cover the feature matrix in `ROADMAP.md`, including SQLite
persistence, API permission boundaries, prepared media uploads, webhook recovery,
thread creation/history/joining, and a local encrypted QR-token exchange. A
process-level smoke test checks authenticated provisioning against a mocked
homeserver (`python3 tests/provisioning_smoke.py --binary ./mautrix-discord`,
requires PyYAML). These are not live Discord/homeserver acceptance tests.

Before production use, exercise your bot's actual permissions and intents,
provisioning login, encryption/media, relay echo filtering, restart recovery,
and any optional personal-account features against a test deployment. Database
migration, role/power-level sync, presence, general membership actions, and
application command argument types absent from legacy remain outside this port.
