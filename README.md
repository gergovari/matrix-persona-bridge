# Matrix Persona Webhook Bridge

A generic, highly flexible Application Service bridge that turns Webhooks into fully-featured Matrix users ("Personas"). Built on `mautrix-go` / `bridgev2`, this bridge allows your external services (like Zapier, Make, or custom backends) to transparently control Matrix accounts and listen to all room events.

## Architecture

This is **not** a standard "Portal" bridge (like bridging Telegram/WhatsApp chats into Matrix). Instead, this is a **Persona Bridge**:
1. **Outbound (Matrix -> Webhook)**: The bridge hooks directly into the AppService Event Processor. Any event (messages, invites, state events) that your Persona witnesses is captured and forwarded as raw JSON directly to your Outbound Webhook.
2. **Inbound (Webhook -> Matrix)**: Your webhook backend can send commands (like `send_message` or `join_room`) via an HTTP POST request to a uniquely secure URL. The bridge dynamically controls the Persona's Matrix account to perform the action.

### Security First

The bot's Matrix ID (e.g. `@webhook_bot-1:homeserver.org`) is publicly visible to anyone in the room. To prevent malicious actors from spoofing requests:
1. **Unguessable URLs**: The inbound webhook URL uses a randomly generated 32-character token.
2. **Mandatory Headers**: The inbound request must include an auto-generated security header with a specific token.

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
docker compose run --rm persona-bridge matrix-persona-bridge -g -e

# 2. Edit the config.yaml generated in your ./persona-bridge-data directory.
# Set:
# homeserver.address: "http://synapse:8008"
# homeserver.domain: "yourdomain.com"
# network.network_url: "https://your-public-webhook-domain.com"
# network.inbound.port: 8080
# network.inbound.path: "/webhook"

# 3. Generate the appservice registration file
docker compose run --rm persona-bridge matrix-persona-bridge -g

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
The bot will guide you through the `Create Persona` flow:
1. **Persona ID**: Give it an identifier (e.g., `bot-1`). This will make the ghost's MXID `@webhook_bot-1:yourdomain.com`.
2. **Outbound Webhook URL**: Enter the URL of your webhook listener (e.g., `https://api.yourdomain.com/webhook/matrix-in`).

### Secure Credentials Provided

Once completed, the bot will reply with your generated credentials. Keep these safe!

```text
Persona created successfully!

**Keep these details secret:**
- **Inbound URL:** http://<bridge-host>:8080/webhook/8fX2aB...
- **Required Header Name:** X-Webhook-Token
- **Required Header Token:** dJ8ks9...
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

**Body:**
```json
{
  "action": "send_message",
  "room_id": "!xyzabc:yourdomain.com",
  "text": "Hello from your webhook!"
}
```

*Supported Actions:*
- `send_message`: Sends text to the specified `room_id`.
- `join_room`: Forces the Persona to join the specified `room_id`.

### 2. Outbound Webhook (Matrix -> Backend)

Whenever a Matrix event occurs in a room where your Persona is present (or if the Persona is invited to a room), the bridge intercepts it and sends a `POST` request to your configured **Outbound Webhook URL**.

**Body:**
```json
{
  "persona_id": "bot-1",
  "event": {
    "type": "m.room.message",
    "sender": "@someuser:yourdomain.com",
    "room_id": "!xyzabc:yourdomain.com",
    "content": {
      "msgtype": "m.text",
      "body": "Hi there!"
    },
    "origin_server_ts": 1690000000000,
    "event_id": "$abc123def"
  }
}
```
*Note: The `event` payload is the raw, 100% native JSON directly from the Matrix Homeserver.*
