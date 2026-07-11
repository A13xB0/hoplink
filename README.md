# hoplink

> **Note:** This project is fully "vibe coded" — written conversationally
> with an AI coding assistant rather than by hand. It's tested against real
> hardware and has a real test suite, but review the code yourself before
> relying on it for anything important.

A Discord bridge for [MeshCore](https://github.com/meshcore-dev/MeshCore) and
[Meshtastic](https://meshtastic.org/) mesh networks.

- **Discord → mesh**: your Discord display name is embedded as the visible
  sender ("`Alice: hello`"), so people on the mesh know who's talking.
- **Mesh → Discord**: each node posts under its own name and a generated
  avatar (via a Discord webhook, so it looks like the node has its own
  Discord identity — not "the bridge bot said: ...").
- Any bridge can relay MeshCore, Meshtastic, or both at once, into the same
  Discord channel. **With both enabled on one bridge, messages also relay
  directly between MeshCore and Meshtastic** — not just via Discord.
- Oversized outbound messages are rejected (not silently chunked forever)
  with a reply in Discord explaining why.
- `sender_format` controls how a relayed message's origin is shown on every
  *other* surface it lands on — e.g. a MeshCore message from "Alice" can show
  up as "Alice", "Alice (MC)", or "Alice (MeshCore)" on Discord and on
  Meshtastic, depending on the setting.

## How the two protocols differ here

- **MeshCore**: messages are sent as fully hand-composed raw RF packets
  (`CMD_SEND_RAW_PACKET`) — see `internal/meshcore`. A MeshCore "hashtag"
  channel needs no setup on the radio itself; the channel secret is derived
  from its name (`sha256("#name")[0:16]`), so hoplink can join any hashtag
  channel on demand.
- **Meshtastic**: messages go through the device's standard client API
  (`internal/meshtastic`) — protobuf messages over a `0x94/0xC3`-framed TCP
  stream. Unlike MeshCore, **the channel must already exist as a slot on the
  attached Meshtastic device** (set up via the official Meshtastic app or
  CLI beforehand); the device itself does that channel's encryption, not
  hoplink. There's no raw-injection equivalent to MeshCore's hashtag
  channels here — the client API doesn't expose one.

## Requirements

- Go 1.26+ (see `go.mod`)
- A MeshCore companion radio reachable over TCP (its built-in WiFi/TCP
  server, default port 5000) — required only if you're bridging MeshCore
- A Meshtastic device with its TCP client API reachable (default port 4403)
  — required only if you're bridging Meshtastic
- A Discord bot (for reading messages) and, per bridged channel, a Discord
  webhook (for posting under node names)

## Setup

1. Copy the example config and fill in your details:

   ```sh
   cp config.example.yaml config.yaml
   ```

2. **Create a Discord bot**:
   - Go to <https://discord.com/developers/applications> → New Application.
   - **Bot** tab → Reset Token → copy it into `discord.bot_token` in
     `config.yaml` (shown only once).
   - On the same page, enable **Message Content Intent** under Privileged
     Gateway Intents — without this the bot receives messages with the text
     stripped out.
   - **OAuth2 → URL Generator** → check the `bot` scope, and under bot
     permissions check at least **Read Messages/View Channels** and **Read
     Message History**. Open the generated URL and invite the bot to your
     server(s).

3. **Create a webhook per bridged Discord channel**: in that channel's
   settings → Integrations → Webhooks → New Webhook → copy its URL into that
   bridge's `discord_webhook_url`. This is what lets each mesh node post
   under its own name/avatar instead of the bot's.

4. Fill in `meshcore:` and/or `meshtastic:` with your radio's address, and
   add one entry under `bridges:` per Discord channel you want bridged (see
   [Config reference](#config-reference) below).

## Running

```sh
go run ./cmd/hoplink --config config.yaml
```

Or build a binary:

```sh
go build -o hoplink ./cmd/hoplink
./hoplink --config config.yaml
```

`hoplink` reconnects each mesh backend independently with exponential
backoff if its connection drops; a MeshCore reconnect never disturbs a live
Meshtastic connection and vice versa. Stop it with Ctrl-C / `SIGTERM`.

## Running with Docker

Build the image yourself:

```sh
docker build -t hoplink .
```

Or use a published image (see [Releases](../../releases) for available
version tags):

```sh
docker pull ghcr.io/hectospark/hoplink:latest
```

Run it with your `config.yaml` mounted read-only into the container at
`/app/config.yaml` (the image's default `CMD`):

```sh
docker run -d \
  --name hoplink \
  --restart unless-stopped \
  -v "$(pwd)/config.yaml:/app/config.yaml:ro" \
  ghcr.io/hectospark/hoplink:latest
```

Or with Docker Compose:

```yaml
services:
  hoplink:
    image: ghcr.io/hectospark/hoplink:latest
    restart: unless-stopped
    volumes:
      - ./config.yaml:/app/config.yaml:ro
```

Follow logs with `docker logs -f hoplink`. Since `config.yaml` holds your
Discord bot token and webhook URLs, never bake it into an image or commit
it — always mount it in at runtime.

## Config reference

See `config.example.yaml` for a fully annotated example. Summary:

### `sender_format:` (top-level)

| Field           | Default | Meaning                                                                                                                                    |
|-----------------|---------|---------------------------------------------------------------------------------------------------------------------------------------------|
| `sender_format` | `none`  | `none` \| `short` \| `full` — how a relayed message's origin surface is shown in the sender name on every *other* destination: "Alice" (none), "Alice (MC)" (short), or "Alice (MeshCore)" (full). Applies wherever a message crosses Discord/MeshCore/Meshtastic. Override per-bridge with `sender_format` under that bridge. |

### `meshcore:` (top-level)

Required only if some bridge has `meshcore.enabled: true`.

| Field             | Default    | Meaning                                                                             |
|-------------------|------------|--------------------------------------------------------------------------------------|
| `host`            | —          | Companion radio's IP                                                                |
| `port`            | `5000`     | Companion TCP port                                                                    |
| `app_name`        | `hoplink` | Identifies this client during the `CMD_APP_START` handshake                          |
| `route`           | `flood`    | `flood` \| `direct`                                                                   |
| `path_hash_bytes` | `3`        | `2` \| `3` — hop-hash width on our outgoing packets; 1-byte hashes are rejected outright |
| `flood_scope`     | `""`       | Optional named flood scope/region; set this if your repeaters run in "scope-only" mode (they silently drop unscoped floods) |

### `meshtastic:` (top-level)

Omit entirely if you have no Meshtastic device. Required only if some bridge
has `meshtastic.enabled: true`.

| Field  | Default | Meaning                                  |
|--------|---------|-------------------------------------------|
| `host` | —       | Attached device's IP                       |
| `port` | `4403`  | Device's client-API TCP port                |

### `discord:`

| Field         | Default        | Meaning                                                                                          |
|---------------|----------------|----------------------------------------------------------------------------------------------------|
| `bot_token`   | —              | Gateway bot token                                                                                  |
| `name_source` | `display_name` | `display_name` \| `username` — which identity to use as the mesh sender when there's no per-server nickname (nickname always wins over either) |

### `limits:`

| Field               | Default | Meaning                                                                                                    |
|---------------------|---------|--------------------------------------------------------------------------------------------------------------|
| `max_message_bytes` | `320`   | A Discord→mesh message composed as `"<Name>: <content>"` longer than this (UTF-8 bytes, pre-chunking) is rejected outright — not chunked — and the sender gets a reply explaining why, in Discord only |

### `coexistence:`

Only matters if you run both a MeshCore radio and a Meshtastic device near
each other and want to reduce RF interference between them.

| Field                    | Default | Meaning                                                                                                                                  |
|--------------------------|---------|---------------------------------------------------------------------------------------------------------------------------------------------|
| `avoid_simultaneous_tx`  | `true`  | Serialises all outbound MeshCore and Meshtastic sends so this process never asks both radios to transmit at the same instant. Best-effort only — neither protocol reports back exact transmit-complete timing, so this reduces the odds of overlap rather than guaranteeing it. No effect if a bridge only uses one backend. |
| `min_gap_ms`             | `100`   | Extra pause held after each send before the next is allowed to start, approximating airtime settle time. Raise this if you still see interference. |

### `bridges:` (one entry per Discord channel)

| Field                 | Meaning                                                                          |
|-----------------------|-------------------------------------------------------------------------------------|
| `name`                | Unique label (used in logs)                                                          |
| `discord_channel_id`  | The Discord channel to bridge                                                        |
| `discord_webhook_url` | That channel's webhook (for posting under node names)                                |
| `guild_id`            | Optional; if set, messages from any other guild are ignored (a sanity check — not needed for correct routing, since Discord channel IDs are already globally unique) |
| `max_message_bytes`   | Optional per-bridge override of `limits.max_message_bytes`                           |
| `sender_format`       | Optional per-bridge override of the top-level `sender_format`                        |
| `meshcore.enabled`    | Turn on the MeshCore side of this bridge                                             |
| `meshcore.hashtag`    | A hashtag channel name (secret derived from it) — exactly one of `hashtag`/`secret_hex`/`public` |
| `meshcore.secret_hex` | An explicit 32-hex-char (16-byte) private channel secret                             |
| `meshcore.public`     | Use MeshCore's well-known default public channel                                     |
| `meshtastic.enabled`  | Turn on the Meshtastic side of this bridge                                           |
| `meshtastic.channel_name` | Name of a channel slot **already configured on the attached device**             |

A bridge can enable MeshCore, Meshtastic, or both. With just one enabled, it
relays to/from Discord only. With both enabled, it *also* relays directly
between MeshCore and Meshtastic — Discord, MeshCore, and Meshtastic all stay
in sync with each other, not just each with Discord individually.

## Testing

```sh
go test ./...
```

Tests use fake in-process TCP "radios" (both protocols) and `httptest`
webhook servers — no real hardware or Discord connection is needed to run
the suite.

## License

AGPL-3.0 with the [Commons Clause](https://commonsclause.com/) — see
[LICENSE](LICENSE). In short: you can use, modify, and redistribute this
freely, including running your own modified version as a service, but if
you do, you must publish your complete source code under the same
license. You may not sell this software or a service substantially based
on it. It's provided free, as-is, with no warranty and no support.
