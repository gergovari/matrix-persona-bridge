# Matrix Persona Webhook Bridge

A generic, highly flexible Application Service bridge that turns Webhooks into fully-featured Matrix users ("Personas"). Built on `mautrix-go` / `bridgev2`, this bridge allows your external services (like Zapier, Make, or custom backends) to transparently control Matrix accounts and listen to all room events.

## Architecture

This is **not** a standard "Portal" bridge (like bridging Telegram/WhatsApp chats into Matrix). Instead, this is a **Persona Bridge**:
1. **Outbound (Matrix -> Webhook)**: The bridge hooks directly into the AppService Event Processor. Any event (messages, invites, state events) that your Persona witnesses is captured and forwarded as raw JSON to **all** configured Outbound Webhook URLs.
2. **Inbound (Webhook -> Matrix)**: Your webhook backend can send commands (like `send_message` or `join_room`) via an HTTP POST request to a uniquely secure URL. The bridge dynamically controls the Persona's Matrix account to perform the action.

### Security First

The bot's Matrix ID (e.g. `@webhook_bot-1:homeserver.org`) is publicly visible to anyone in the room. To prevent malicious actors from spoofing requests:
1. **Unguessable URLs**: The inbound webhook URL uses a randomly generated 32-character token.
2. **Mandatory Headers**: The inbound request must include an auto-generated security header with a specific token.
3. **Outbound Authentication**: All outbound webhook calls include the same `X-Webhook-Token` security header, allowing your backend to verify that events genuinely originate from the bridge.

---

## Deploying with Docker Compose

If you already have a Synapse deployment managed via Docker Compose, you can seamlessly add this bridge as a new service by pointing directly to the Git repository subdirectory.

### 1. Update `docker-compose.yml`

Add the following service block to your existing `docker-compose.yml`. Ensure the path under `build.context` correctly points to the `matrix-persona-bridge` repository:

```yaml
services:
  # ... your existing synapse service ...
  synapse:
    image: matrixdotorg/synapse:latest
    # ...

  persona-bridge:
    build:
      context: ./path/to/matrix-persona-bridge # <--- Change this to the path of this repo
      dockerfile: Dockerfile
    container_name: persona-bridge
    restart: unless-stopped
    volumes:
      - ./persona-bridge-data:/data
    # Use standard docker networking so it can reach Synapse
    # Synapse must also be able to reach persona-bridge:8080!
    depends_on:
      - synapse
```

### 2. Generate Configuration & Registration

Before starting the bridge, you must generate the example configuration and the appservice registration file:

```bash
# 1. Generate example config
docker compose run --rm persona-bridge -g -e

# 2. Edit the config.yaml generated in your ./persona-bridge-data directory.
# Set:
# homeserver.address: "http://synapse:8008"
# homeserver.domain: "yourdomain.com"
# network.network_url: "https://your-public-webhook-domain.com"
# network.inbound.port: 8080
# network.inbound.path: "/webhook"

# 3. Generate the appservice registration file
docker compose run --rm persona-bridge -g

# 4. Register the AppService with Synapse!
# Edit your Synapse homeserver.yaml and add the registration file path:
# app_service_config_files:
#   - /data/persona-bridge-data/registration.yaml
```

### 3. Start the Bridge

After restarting Synapse to load the new AppService registration, start the bridge:

```bash
docker compose up -d persona-bridge
```

---

## Usage & Management

Management of Personas is handled entirely by messaging the bridge bot (usually `@webhookbot:yourdomain.com`) from an admin Matrix account. 

### Creating a Persona

Open a direct chat with the bridge bot and type:
```text
login
```

> **Note:** Just type `login` with no arguments. The bot will automatically start the persona creation flow and ask for a **Persona ID** (e.g., `bot-1`). This will make the ghost's MXID `@webhook_bot-1:yourdomain.com`.

### Secure Credentials Provided

Once completed, the bot will reply with your generated credentials. Keep these safe!

```text
Persona created successfully!

**Keep these details secret:**
- **Inbound URL:** https://your-public-webhook-domain.com/webhook/8fX2aB...
- **Required Header Name:** X-Webhook-Token
- **Required Header Token:** dJ8ks9...

Use `add-outbound bot-1 <url>` to add outbound webhook URLs.
```

> The Inbound URL uses the `network_url` from your bridge configuration. Make sure it points to your publicly reachable bridge endpoint.

### Inviting a Persona to Rooms

After creation, simply **invite the persona's ghost** (e.g., `@webhook_bot-1:yourdomain.com`) to any room. The ghost will automatically accept the invite and start forwarding events.

### Managing Outbound URLs

Each persona can have **multiple outbound webhook URLs**. Every Matrix event the persona witnesses will be forwarded to **all** configured URLs simultaneously.

**Add an outbound URL:**
```text
add-outbound <persona_id> <url>
```
Example:
```text
add-outbound bot-1 https://api.yourdomain.com/webhook/matrix-in
```

**Remove an outbound URL:**
```text
remove-outbound <persona_id> <url>
```

**List all outbound URLs:**
```text
list-outbound <persona_id>
```

### Setting Display Name

Set a custom Matrix display name for a persona's ghost user:
```text
set-displayname <persona_id> <name...>
```
Example:
```text
set-displayname bot-1 My Awesome Bot
```

### Setting Avatar

Set a custom Matrix avatar for a persona's ghost user. Upload an image to any Matrix room first, then use its `mxc://` URL:
```text
set-avatar <persona_id> <mxc://...>
```
Example:
```text
set-avatar bot-1 mxc://yourdomain.com/AbCdEfGhIjKl
```

---

## Webhook Payloads

### 1. Inbound Webhook (Backend -> Matrix)

To make your Persona act on Matrix, send a `POST` request to the **Inbound URL** provided during registration.

**Headers:**
```http
X-Webhook-Token: dJ8ks9...  <-- The secret Header Token
Content-Type: application/json
```

**Response format:** All responses are JSON:
```json
{"event_id": "$abc123"}
{"error": "description"}
```

#### Supported Actions

| Action | Description | Required Fields |
|--------|-------------|-----------------|
| `send_message` | Send text/notice/emote with optional reply/thread | `text` |
| `send_file` | Send image/video/audio/file attachment | `file_url` or `file_data` |
| `send_reaction` | React to an event with emoji | `event_id`, `reaction` |
| `edit_message` | Edit a previously sent message | `event_id`, `text` |
| `redact` | Delete/redact an event | `event_id` |
| `typing` | Show/hide typing indicator | `typing` |
| `read_receipt` | Mark an event as read | `event_id` |
| `join_room` | Join a room | — |
| `leave_room` | Leave a room | — |
| `set_topic` | Set the room topic | `topic` |
| `set_room_name` | Set the room name | `room_name` |

#### `send_message`
```json
{
  "action": "send_message",
  "room_id": "!xyzabc:yourdomain.com",
  "text": "Hello from webhook!",
  "html": "<b>Hello</b> from webhook!",
  "msg_type": "m.text",
  "reply_to": "$event_id_to_reply_to",
  "thread_root": "$thread_root_event_id"
}
```
- `msg_type`: `"m.text"` (default), `"m.notice"`, or `"m.emote"`
- `html`: Optional HTML-formatted body
- `reply_to`: Optional event ID to reply to
- `thread_root`: Optional event ID to create/continue a thread

#### `send_file`
```json
{
  "action": "send_file",
  "room_id": "!xyzabc:yourdomain.com",
  "file_url": "mxc://yourdomain.com/AbCdEfGh",
  "file_name": "photo.png",
  "file_mime": "image/png",
  "file_size": 12345,
  "reply_to": "$optional_reply_event"
}
```
Or upload with base64 data:
```json
{
  "action": "send_file",
  "room_id": "!xyzabc:yourdomain.com",
  "file_data": "iVBORw0KGgo...",
  "file_name": "photo.png",
  "file_mime": "image/png"
}
```

**n8n / Multipart upload:** You can also upload files via `multipart/form-data` (compatible with n8n's HTTP Request node):
```
POST /webhook/<token>
Content-Type: multipart/form-data

action=send_file
room_id=!xyzabc:yourdomain.com
file_name=photo.png
file_mime=image/png
file=@/path/to/photo.png
```
If no `action` field is provided in a multipart request, it defaults to `send_file`.

File type is auto-detected from `file_mime`: `image/*` → image, `video/*` → video, `audio/*` → audio, everything else → file.

#### `send_reaction`
```json
{
  "action": "send_reaction",
  "room_id": "!xyzabc:yourdomain.com",
  "event_id": "$target_event_id",
  "reaction": "👍"
}
```

#### `edit_message`
```json
{
  "action": "edit_message",
  "room_id": "!xyzabc:yourdomain.com",
  "event_id": "$original_event_id",
  "text": "Updated message text",
  "html": "<i>Updated</i> message text"
}
```

#### `redact`
```json
{
  "action": "redact",
  "room_id": "!xyzabc:yourdomain.com",
  "event_id": "$event_to_delete",
  "reason": "Optional reason"
}
```

#### `typing`
```json
{
  "action": "typing",
  "room_id": "!xyzabc:yourdomain.com",
  "typing": true,
  "timeout": 5000
}
```
- `typing`: `true` to start, `false` to stop
- `timeout`: Duration in milliseconds (default: 30000)

#### `read_receipt`
```json
{
  "action": "read_receipt",
  "room_id": "!xyzabc:yourdomain.com",
  "event_id": "$last_read_event_id"
}
```

#### `join_room` / `leave_room`
```json
{
  "action": "join_room",
  "room_id": "!xyzabc:yourdomain.com"
}
```

#### `set_topic` / `set_room_name`
```json
{
  "action": "set_topic",
  "room_id": "!xyzabc:yourdomain.com",
  "topic": "New room topic"
}
```

### 2. Outbound Webhook (Matrix -> Backend)

Whenever a Matrix event occurs in a room where your Persona is present (or if the Persona is invited), the bridge forwards it as a `POST` to **all** configured Outbound Webhook URLs.

**Headers:**
```http
X-Webhook-Token: dJ8ks9...  <-- The persona's secret Header Token
Content-Type: application/json
```

**Body structure:** Every outbound payload wraps the raw Matrix event:
```json
{
  "persona_id": "bot-1",
  "event": { ... }
}
```

The `event` object is the **raw, unmodified JSON** from the Matrix homeserver. The `type` field tells you what kind of event it is.

#### Forwarded Event Types

| Event Type | Description |
|------------|-------------|
| `m.room.message` | Text messages, images, files, audio, video, notices, emotes |
| `m.room.member` | Join, leave, invite, kick, ban membership changes |
| `m.room.name` | Room name changes |
| `m.room.topic` | Room topic changes |
| `m.room.power_levels` | Permission/power level changes |
| `m.room.redaction` | Message deletions |
| `m.reaction` | Emoji reactions |

#### `m.room.message` — Text
```json
{
  "persona_id": "bot-1",
  "event": {
    "type": "m.room.message",
    "sender": "@user:yourdomain.com",
    "room_id": "!abc:yourdomain.com",
    "event_id": "$msg123",
    "origin_server_ts": 1690000000000,
    "content": {
      "msgtype": "m.text",
      "body": "Hello!",
      "format": "org.matrix.custom.html",
      "formatted_body": "<b>Hello!</b>"
    }
  }
}
```
**`content.msgtype` values:** `m.text`, `m.notice`, `m.emote`, `m.image`, `m.video`, `m.audio`, `m.file`, `m.location`

#### `m.room.message` — Reply
When a message is a reply, `content` includes `m.relates_to`:
```json
{
  "content": {
    "msgtype": "m.text",
    "body": "> original message\n\nReply text",
    "m.relates_to": {
      "m.in_reply_to": {
        "event_id": "$original_event_id"
      }
    }
  }
}
```

#### `m.room.message` — File/Image/Video/Audio
```json
{
  "content": {
    "msgtype": "m.image",
    "body": "photo.png",
    "url": "mxc://yourdomain.com/AbCdEfGh",
    "info": {
      "mimetype": "image/png",
      "size": 12345,
      "w": 800,
      "h": 600
    }
  }
}
```
Download media via: `https://yourdomain.com/_matrix/media/v3/download/<server>/<media_id>`

#### `m.room.message` — Edit
```json
{
  "content": {
    "msgtype": "m.text",
    "body": "* Corrected text",
    "m.new_content": {
      "msgtype": "m.text",
      "body": "Corrected text"
    },
    "m.relates_to": {
      "rel_type": "m.replace",
      "event_id": "$original_event_id"
    }
  }
}
```
Check for `m.relates_to.rel_type == "m.replace"` to detect edits. The actual new content is in `m.new_content`.

#### `m.room.member`
```json
{
  "persona_id": "bot-1",
  "event": {
    "type": "m.room.member",
    "sender": "@admin:yourdomain.com",
    "state_key": "@user:yourdomain.com",
    "room_id": "!abc:yourdomain.com",
    "event_id": "$mem456",
    "origin_server_ts": 1690000000000,
    "content": {
      "membership": "join",
      "displayname": "User",
      "avatar_url": "mxc://yourdomain.com/avatar123"
    }
  }
}
```
**`content.membership` values:**
- `invite` — User was invited
- `join` — User joined (or accepted invite)
- `leave` — User left or was kicked
- `ban` — User was banned

**Tip:** When `state_key` matches the persona's ghost MXID and `membership` is `invite`, your backend is being invited to a room. Respond with the `join_room` inbound action to accept.

#### `m.reaction`
```json
{
  "persona_id": "bot-1",
  "event": {
    "type": "m.reaction",
    "sender": "@user:yourdomain.com",
    "room_id": "!abc:yourdomain.com",
    "event_id": "$react789",
    "origin_server_ts": 1690000000000,
    "content": {
      "m.relates_to": {
        "rel_type": "m.annotation",
        "event_id": "$target_event_id",
        "key": "👍"
      }
    }
  }
}
```
The `key` field contains the emoji. The `event_id` in `m.relates_to` points to the reacted-to message.

#### `m.room.redaction`
```json
{
  "persona_id": "bot-1",
  "event": {
    "type": "m.room.redaction",
    "sender": "@user:yourdomain.com",
    "room_id": "!abc:yourdomain.com",
    "event_id": "$redact012",
    "origin_server_ts": 1690000000000,
    "content": {
      "reason": "Sent by mistake"
    },
    "redacts": "$deleted_event_id"
  }
}
```
The `redacts` field identifies the event that was deleted.

#### `m.room.name`
```json
{
  "persona_id": "bot-1",
  "event": {
    "type": "m.room.name",
    "sender": "@admin:yourdomain.com",
    "room_id": "!abc:yourdomain.com",
    "event_id": "$name345",
    "origin_server_ts": 1690000000000,
    "content": {
      "name": "New Room Name"
    }
  }
}
```

#### `m.room.topic`
```json
{
  "persona_id": "bot-1",
  "event": {
    "type": "m.room.topic",
    "sender": "@admin:yourdomain.com",
    "room_id": "!abc:yourdomain.com",
    "event_id": "$topic678",
    "origin_server_ts": 1690000000000,
    "content": {
      "topic": "Updated topic for this room"
    }
  }
}
```

#### `m.room.power_levels`
```json
{
  "persona_id": "bot-1",
  "event": {
    "type": "m.room.power_levels",
    "sender": "@admin:yourdomain.com",
    "room_id": "!abc:yourdomain.com",
    "event_id": "$pl901",
    "origin_server_ts": 1690000000000,
    "content": {
      "users": {
        "@admin:yourdomain.com": 100,
        "@user:yourdomain.com": 0
      },
      "events_default": 0,
      "state_default": 50,
      "ban": 50,
      "kick": 50,
      "invite": 0
    }
  }
}
```

---

## Bot Command Reference

All commands are sent as messages to the bridge bot in a direct chat. Admin privileges are required.

| Command | Description |
|---------|-------------|
| `login` | Create a new Persona (interactive flow) |
| `add-outbound <persona_id> <url>` | Add an outbound webhook URL to a persona |
| `remove-outbound <persona_id> <url>` | Remove an outbound webhook URL from a persona |
| `list-outbound <persona_id>` | List all configured outbound URLs for a persona |
| `set-displayname <persona_id> <name...>` | Set the Matrix display name for a persona's ghost |
| `set-avatar <persona_id> <mxc://...>` | Set the Matrix avatar for a persona's ghost |
| `help` | Show all available commands |
