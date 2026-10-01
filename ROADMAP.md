# Bridgev2 feature matrix

The bridgev2 port preserves the original bridge's implemented workflows with a
fresh database. The default deployment is an explicit-channel bot relay;
personal accounts and guild discovery require configuration. The original
feature list remains in [legacy/ROADMAP.md](legacy/ROADMAP.md).

| Feature | Bridgev2 implementation |
| --- | --- |
| Lifecycle, storage, encryption, double puppeting | Native bridgev2 framework |
| Bot token login | Native v3 provisioning and commands; bridge admin only |
| User/OAuth tokens and mobile QR login | Optional personal account flows; native v3 and v1 compatibility routes |
| Explicit channels and existing-room mappings | Config, admin provisioning, and bridge commands |
| Global/per-channel sync controls | Messages, attachments, embeds, stickers, edits, deletes, reactions, typing, receipts, profiles, room metadata, threads, history |
| Webhook relay | Per-sender names/avatars, persisted credentials, echo filtering, deleted managed webhook recovery |
| Personal messages | Native sends, edits, deletes, reply references, and nonce-based echo reconciliation |
| Formatting | Markdown, underline, spoilers, user/channel/role mentions, everyone/here, timestamps, custom emoji images |
| Attachments | Bounded transfer, encrypted Matrix uploads, cache reuse, spoilers, optional Discord CDN upload API |
| Replies | Native incoming/personal replies, opt-in cross-room references, relay reply previews |
| Rich embeds/previews | HTML rich embeds, fields/tables, media and link previews |
| Stickers | PNG/APNG/GIF; optional Lottie conversion to PNG/GIF/WebP/WebM |
| Reactions | Unicode and cached Discord custom emoji; no arbitrary custom emoji upload |
| Profiles | Global/friend names, template fields, guild nicknames/avatars, webhook per-message profiles, bot/contact metadata |
| DMs/group DMs | Optional; receiver-specific portals, DM grouping space, member/name/avatar updates, startup limit |
| Guild discovery | Per-login modes: nothing, existing portals, on message, everything |
| Guild/category spaces | Hierarchy, avatars/names, optional restricted rooms and mute-on-create |
| Thread lifecycle | Creation from Matrix, root relations, join notices/reactions, optional join-on-open |
| Backfill | Channel and thread history; gated by both network sync and framework settings |
| Typing and own read status | Opt-in; read status requires a personal account |
| Discord application commands | Search/help and execution with the argument types supported by legacy |
| Commands | Guild modes, portal creation/binding/replacement/unbridge, connection checks/control, space rejoin, legacy login aliases, native framework commands |
| Provisioning | Native v3 plus legacy v1 ping/login/connection/guild routes; same framework authentication |
| Direct media | Framework-signed Matrix media IDs, Discord CDN validation, expiring attachment URL refresh |
| Proxy support | REST, media, gateway, and QR connection |
| Old database/room migration | Out of scope by design; fresh database required |
| Live Discord/Matrix validation | Pending deployment with real accounts/homeserver |

The legacy bridge does not implement role-to-power-level sync, presence,
general invite/kick/leave actions, interactive components, arbitrary custom
emoji uploads, or application command subcommand groups/mention/attachment
arguments. These remain beyond feature parity.

Automated coverage includes relay persistence and permission boundaries,
native personal sends, formatting, custom emoji reuse, media bounds/cache,
thread routing/creation/history/joining, reply previews, provisioning, and the
QR cryptographic exchange. It does not establish compatibility with every live
Discord client protocol version or homeserver.
