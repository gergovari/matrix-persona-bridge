package connector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mau.fi/util/configupgrade"
	"go.mau.fi/util/random"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type Config struct {
	NetworkURL string `yaml:"network_url"`
	Inbound    struct {
		Port int    `yaml:"port"`
		Path string `yaml:"path"`
	} `yaml:"inbound"`
}

type UserLoginMetadata struct {
	OutboundURLs []string `json:"outbound_urls"`
	InboundToken string   `json:"inbound_token"`
	HeaderName   string   `json:"header_name"`
	HeaderValue  string   `json:"header_value"`
}

type WebhookConnector struct {
	Bridge *bridgev2.Bridge
	Config Config

	Personas     map[networkid.UserLoginID]*bridgev2.UserLogin
	PersonasLock sync.RWMutex
}

var _ bridgev2.NetworkConnector = (*WebhookConnector)(nil)

func (c *WebhookConnector) Init(bridge *bridgev2.Bridge) {
	c.Bridge = bridge
	c.Personas = make(map[networkid.UserLoginID]*bridgev2.UserLogin)

	if c.Config.Inbound.Port == 0 {
		c.Config.Inbound.Port = 8080
	}
	if c.Config.Inbound.Path == "" {
		c.Config.Inbound.Path = "/webhook"
	}
	if !strings.HasPrefix(c.Config.Inbound.Path, "/") {
		c.Config.Inbound.Path = "/" + c.Config.Inbound.Path
	}

	go c.startWebhookListener()
}

func (c *WebhookConnector) Start(ctx context.Context) error {
	mxConn, ok := c.Bridge.Matrix.(*matrix.Connector)
	if !ok {
		return fmt.Errorf("matrix connector is not matrix.Connector")
	}

	eventTypes := []event.Type{
		event.EventMessage,
		event.StateMember,
		event.StateRoomName,
		event.StateTopic,
		event.StatePowerLevels,
		event.EventRedaction,
		event.EventReaction,
	}

	for _, t := range eventTypes {
		mxConn.EventProcessor.On(t, func(ctx context.Context, evt *event.Event) {
			go c.handleOutboundMatrixEvent(evt)
		})
	}

	return nil
}

func (c *WebhookConnector) GetName() bridgev2.BridgeName {
	networkURL := c.Config.NetworkURL
	if networkURL == "" {
		networkURL = "https://webhook.local"
	}
	return bridgev2.BridgeName{
		DisplayName: "Webhook",
		NetworkURL:  networkURL,
		NetworkID:   "webhook",
	}
}

func (c *WebhookConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{}
}

func (c *WebhookConnector) GetBridgeInfoVersion() (info, capabilities int) {
	return 1, 1
}

func (c *WebhookConnector) GetConfig() (example string, data any, upgrader configupgrade.Upgrader) {
	return `
# The public URL where the bridge is reachable.
network_url: https://webhook.local
inbound:
  port: 8080
  path: /webhook
`, &c.Config, configupgrade.SimpleUpgrader(func(helper configupgrade.Helper) {
		helper.Copy(configupgrade.Str, "network_url")
		helper.Copy(configupgrade.Int, "inbound", "port")
		helper.Copy(configupgrade.Str, "inbound", "path")
	})
}

func (c *WebhookConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		UserLogin: func() any {
			return &UserLoginMetadata{}
		},
	}
}

func (c *WebhookConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	login.Client = &WebhookAPI{Login: login}
	
	c.PersonasLock.Lock()
	c.Personas[login.ID] = login
	c.PersonasLock.Unlock()
	
	return nil
}

func (c *WebhookConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "Create Persona",
		Description: "Register a new webhook persona",
		ID:          "create-persona",
	}}
}

func (c *WebhookConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != "create-persona" {
		return nil, fmt.Errorf("unknown flow")
	}
	return &PersonaLogin{User: user, Connector: c}, nil
}

type PersonaLogin struct {
	User      *bridgev2.User
	Connector *WebhookConnector
	PersonaID string
}

func (pl *PersonaLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "persona-details",
		Instructions: "Enter an ID for the new Persona. You can add outbound URLs later with the `add-outbound` command.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type: bridgev2.LoginInputFieldTypeUsername,
					ID:   "persona_id",
					Name: "Persona ID (e.g., bot-1)",
				},
			},
		},
	}, nil
}

func (pl *PersonaLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	pl.PersonaID = input["persona_id"]

	inboundToken := random.String(32) // Generate a secure 32-character random token
	headerName := "X-Webhook-Token"
	headerValue := random.String(32)

	ul, err := pl.User.NewLogin(ctx, &database.UserLogin{
		ID:         networkid.UserLoginID(pl.PersonaID),
		RemoteName: pl.PersonaID,
		Metadata: &UserLoginMetadata{
			OutboundURLs: []string{},
			InboundToken: inboundToken,
			HeaderName:   headerName,
			HeaderValue:  headerValue,
		},
	}, &bridgev2.NewLoginParams{
		LoadUserLogin: pl.Connector.LoadUserLogin,
	})
	if err != nil {
		return nil, err
	}

	networkURL := pl.Connector.Config.NetworkURL
	if networkURL == "" {
		networkURL = fmt.Sprintf("http://<host>:%d", pl.Connector.Config.Inbound.Port)
	}
	networkURL = strings.TrimRight(networkURL, "/")

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       "complete",
		Instructions: fmt.Sprintf("Persona created successfully!\n\n**Keep these details secret:**\n- **Inbound URL:** `%s%s/%s`\n- **Required Header Name:** `%s`\n- **Required Header Token:** `%s`\n\nUse `add-outbound %s <url>` to add outbound webhook URLs.", networkURL, pl.Connector.Config.Inbound.Path, inboundToken, headerName, headerValue, pl.PersonaID),
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}

func (pl *PersonaLogin) Cancel() {}

func (c *WebhookConnector) startWebhookListener() {
	mux := http.NewServeMux()
	route := fmt.Sprintf("POST %s/{inboundToken}", c.Config.Inbound.Path)
	mux.HandleFunc(route, c.handleInbound)

	addr := fmt.Sprintf(":%d", c.Config.Inbound.Port)
	c.Bridge.Log.Info().Str("addr", addr).Str("route", route).Msg("Starting webhook listener")
	err := http.ListenAndServe(addr, mux)
	if err != nil {
		c.Bridge.Log.Err(err).Msg("Webhook listener failed")
	}
}

type InboundWebhookPayload struct {
	Action string `json:"action"`
	RoomID string `json:"room_id"`

	// send_message fields
	Text       string `json:"text,omitempty"`
	HTML       string `json:"html,omitempty"`
	MsgType    string `json:"msg_type,omitempty"` // "m.text" (default), "m.notice", "m.emote"
	ReplyTo    string `json:"reply_to,omitempty"`
	ThreadRoot string `json:"thread_root,omitempty"`

	// send_file fields
	FileURL    string `json:"file_url,omitempty"`    // mxc:// URL
	FileData   string `json:"file_data,omitempty"`   // base64-encoded file data (alternative to file_url)
	FileName   string `json:"file_name,omitempty"`
	FileMIME   string `json:"file_mime,omitempty"`
	FileSize   int    `json:"file_size,omitempty"`

	// send_reaction / redact / edit / read_receipt
	EventID  string `json:"event_id,omitempty"`
	Reaction string `json:"reaction,omitempty"`
	Reason   string `json:"reason,omitempty"`

	// edit_message fields (uses EventID + Text/HTML)

	// typing
	Typing  bool `json:"typing,omitempty"`
	Timeout int  `json:"timeout,omitempty"` // ms, default 30000

	// set_topic / set_room_name
	Topic    string `json:"topic,omitempty"`
	RoomName string `json:"room_name,omitempty"`
}

type InboundWebhookResponse struct {
	EventID string `json:"event_id,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (c *WebhookConnector) handleInbound(w http.ResponseWriter, r *http.Request) {
	inboundToken := r.PathValue("inboundToken")
	if inboundToken == "" {
		c.replyJSON(w, http.StatusBadRequest, InboundWebhookResponse{Error: "Missing inbound token"})
		return
	}

	var login *bridgev2.UserLogin
	var loginMeta *UserLoginMetadata
	c.PersonasLock.RLock()
	for _, p := range c.Personas {
		if meta, ok := p.Metadata.(*UserLoginMetadata); ok && meta.InboundToken == inboundToken {
			login = p
			loginMeta = meta
			break
		}
	}
	c.PersonasLock.RUnlock()

	if login == nil {
		c.replyJSON(w, http.StatusUnauthorized, InboundWebhookResponse{Error: "Invalid token or persona not found"})
		return
	}

	if loginMeta.HeaderName != "" {
		if r.Header.Get(loginMeta.HeaderName) != loginMeta.HeaderValue {
			c.replyJSON(w, http.StatusUnauthorized, InboundWebhookResponse{Error: "Invalid security header"})
			return
		}
	}

	ghost, err := c.Bridge.GetGhostByID(r.Context(), networkid.UserID(login.ID))
	if err != nil || ghost == nil {
		c.replyJSON(w, http.StatusInternalServerError, InboundWebhookResponse{Error: "Ghost not found"})
		return
	}

	var payload InboundWebhookPayload
	contentType := r.Header.Get("Content-Type")

	if strings.HasPrefix(contentType, "multipart/form-data") {
		// n8n-style file upload via multipart/form-data
		if err := r.ParseMultipartForm(32 << 20); err != nil { // 32 MB max
			c.replyJSON(w, http.StatusBadRequest, InboundWebhookResponse{Error: "Failed to parse multipart form"})
			return
		}
		payload.Action = r.FormValue("action")
		if payload.Action == "" {
			payload.Action = "send_file" // default for multipart
		}
		payload.RoomID = r.FormValue("room_id")
		payload.Text = r.FormValue("text")
		payload.HTML = r.FormValue("html")
		payload.MsgType = r.FormValue("msg_type")
		payload.ReplyTo = r.FormValue("reply_to")
		payload.ThreadRoot = r.FormValue("thread_root")
		payload.FileName = r.FormValue("file_name")
		payload.FileMIME = r.FormValue("file_mime")
		payload.EventID = r.FormValue("event_id")

		// Read the uploaded file if present
		file, header, fileErr := r.FormFile("file")
		if fileErr == nil {
			defer file.Close()
			fileData, readErr := io.ReadAll(file)
			if readErr != nil {
				c.replyJSON(w, http.StatusBadRequest, InboundWebhookResponse{Error: "Failed to read uploaded file"})
				return
			}
			payload.FileData = base64.StdEncoding.EncodeToString(fileData)
			if payload.FileName == "" {
				payload.FileName = header.Filename
			}
			if payload.FileMIME == "" {
				payload.FileMIME = header.Header.Get("Content-Type")
			}
			payload.FileSize = len(fileData)
		}
	} else {
		// Standard JSON body
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			c.replyJSON(w, http.StatusBadRequest, InboundWebhookResponse{Error: "Invalid JSON body"})
			return
		}
	}

	ctx := r.Context()
	roomID := id.RoomID(payload.RoomID)

	switch payload.Action {
	case "send_message":
		c.handleSendMessage(ctx, w, ghost, roomID, &payload)
	case "send_file":
		c.handleSendFile(ctx, w, ghost, roomID, &payload)
	case "send_reaction":
		c.handleSendReaction(ctx, w, ghost, roomID, &payload)
	case "edit_message":
		c.handleEditMessage(ctx, w, ghost, roomID, &payload)
	case "redact":
		c.handleRedact(ctx, w, ghost, roomID, &payload)
	case "typing":
		c.handleTyping(ctx, w, ghost, roomID, &payload)
	case "read_receipt":
		c.handleReadReceipt(ctx, w, ghost, roomID, &payload)
	case "join_room":
		err = ghost.Intent.EnsureJoined(ctx, roomID)
		if err != nil {
			c.replyJSON(w, http.StatusInternalServerError, InboundWebhookResponse{Error: fmt.Sprintf("Failed to join room: %v", err)})
			return
		}
		c.replyJSON(w, http.StatusOK, InboundWebhookResponse{})
	case "leave_room":
		ghostMXID := ghost.Intent.GetMXID()
		_, err = ghost.Intent.SendState(ctx, roomID, event.StateMember, ghostMXID.String(), &event.Content{
			Parsed: &event.MemberEventContent{Membership: event.MembershipLeave},
		}, time.Now())
		if err != nil {
			c.replyJSON(w, http.StatusInternalServerError, InboundWebhookResponse{Error: fmt.Sprintf("Failed to leave room: %v", err)})
			return
		}
		c.replyJSON(w, http.StatusOK, InboundWebhookResponse{})
	case "set_topic":
		c.handleSetTopic(ctx, w, ghost, roomID, &payload)
	case "set_room_name":
		c.handleSetRoomName(ctx, w, ghost, roomID, &payload)
	default:
		c.replyJSON(w, http.StatusBadRequest, InboundWebhookResponse{Error: fmt.Sprintf("Unknown action: %s", payload.Action)})
	}
}

func (c *WebhookConnector) replyJSON(w http.ResponseWriter, status int, resp InboundWebhookResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

func (c *WebhookConnector) handleSendMessage(ctx context.Context, w http.ResponseWriter, ghost *bridgev2.Ghost, roomID id.RoomID, payload *InboundWebhookPayload) {
	msgType := event.MsgText
	switch payload.MsgType {
	case "m.notice":
		msgType = event.MsgNotice
	case "m.emote":
		msgType = event.MsgEmote
	}

	content := &event.MessageEventContent{
		MsgType: msgType,
		Body:    payload.Text,
	}

	if payload.HTML != "" {
		content.Format = event.FormatHTML
		content.FormattedBody = payload.HTML
	}

	if payload.ReplyTo != "" || payload.ThreadRoot != "" {
		content.RelatesTo = &event.RelatesTo{}
		if payload.ReplyTo != "" {
			content.RelatesTo.InReplyTo = &event.InReplyTo{EventID: id.EventID(payload.ReplyTo)}
		}
		if payload.ThreadRoot != "" {
			content.RelatesTo.Type = event.RelThread
			content.RelatesTo.EventID = id.EventID(payload.ThreadRoot)
		}
	}

	resp, err := ghost.Intent.SendMessage(ctx, roomID, event.EventMessage, &event.Content{Parsed: content}, nil)
	if err != nil {
		c.replyJSON(w, http.StatusInternalServerError, InboundWebhookResponse{Error: fmt.Sprintf("Failed to send message: %v", err)})
		return
	}
	c.replyJSON(w, http.StatusOK, InboundWebhookResponse{EventID: resp.EventID.String()})
}

func (c *WebhookConnector) handleSendFile(ctx context.Context, w http.ResponseWriter, ghost *bridgev2.Ghost, roomID id.RoomID, payload *InboundWebhookPayload) {
	var contentURI id.ContentURIString

	if payload.FileURL != "" {
		// Use pre-uploaded mxc:// URL
		contentURI = id.ContentURIString(payload.FileURL)
	} else if payload.FileData != "" {
		// Upload base64-encoded file data
		data, err := base64.StdEncoding.DecodeString(payload.FileData)
		if err != nil {
			c.replyJSON(w, http.StatusBadRequest, InboundWebhookResponse{Error: "Invalid base64 file data"})
			return
		}
		mime := payload.FileMIME
		if mime == "" {
			mime = "application/octet-stream"
		}
		uploadedURL, _, uploadErr := ghost.Intent.UploadMedia(ctx, roomID, data, payload.FileName, mime)
		if uploadErr != nil {
			c.replyJSON(w, http.StatusInternalServerError, InboundWebhookResponse{Error: fmt.Sprintf("Failed to upload file: %v", uploadErr)})
			return
		}
		contentURI = uploadedURL
	} else {
		c.replyJSON(w, http.StatusBadRequest, InboundWebhookResponse{Error: "Either file_url or file_data is required"})
		return
	}

	// Determine message type from MIME
	msgType := event.MsgFile
	mime := strings.ToLower(payload.FileMIME)
	if strings.HasPrefix(mime, "image/") {
		msgType = event.MsgImage
	} else if strings.HasPrefix(mime, "video/") {
		msgType = event.MsgVideo
	} else if strings.HasPrefix(mime, "audio/") {
		msgType = event.MsgAudio
	}

	fileName := payload.FileName
	if fileName == "" {
		fileName = "file"
	}

	content := &event.MessageEventContent{
		MsgType: msgType,
		Body:    fileName,
		URL:     contentURI,
		Info: &event.FileInfo{
			MimeType: payload.FileMIME,
			Size:     payload.FileSize,
		},
	}

	if payload.ReplyTo != "" || payload.ThreadRoot != "" {
		content.RelatesTo = &event.RelatesTo{}
		if payload.ReplyTo != "" {
			content.RelatesTo.InReplyTo = &event.InReplyTo{EventID: id.EventID(payload.ReplyTo)}
		}
		if payload.ThreadRoot != "" {
			content.RelatesTo.Type = event.RelThread
			content.RelatesTo.EventID = id.EventID(payload.ThreadRoot)
		}
	}

	resp, err := ghost.Intent.SendMessage(ctx, roomID, event.EventMessage, &event.Content{Parsed: content}, nil)
	if err != nil {
		c.replyJSON(w, http.StatusInternalServerError, InboundWebhookResponse{Error: fmt.Sprintf("Failed to send file: %v", err)})
		return
	}
	c.replyJSON(w, http.StatusOK, InboundWebhookResponse{EventID: resp.EventID.String()})
}

func (c *WebhookConnector) handleSendReaction(ctx context.Context, w http.ResponseWriter, ghost *bridgev2.Ghost, roomID id.RoomID, payload *InboundWebhookPayload) {
	if payload.EventID == "" || payload.Reaction == "" {
		c.replyJSON(w, http.StatusBadRequest, InboundWebhookResponse{Error: "event_id and reaction are required"})
		return
	}

	content := &event.ReactionEventContent{
		RelatesTo: event.RelatesTo{
			Type:    event.RelAnnotation,
			EventID: id.EventID(payload.EventID),
			Key:     payload.Reaction,
		},
	}

	resp, err := ghost.Intent.SendMessage(ctx, roomID, event.EventReaction, &event.Content{Parsed: content}, nil)
	if err != nil {
		c.replyJSON(w, http.StatusInternalServerError, InboundWebhookResponse{Error: fmt.Sprintf("Failed to send reaction: %v", err)})
		return
	}
	c.replyJSON(w, http.StatusOK, InboundWebhookResponse{EventID: resp.EventID.String()})
}

func (c *WebhookConnector) handleEditMessage(ctx context.Context, w http.ResponseWriter, ghost *bridgev2.Ghost, roomID id.RoomID, payload *InboundWebhookPayload) {
	if payload.EventID == "" || payload.Text == "" {
		c.replyJSON(w, http.StatusBadRequest, InboundWebhookResponse{Error: "event_id and text are required"})
		return
	}

	newContent := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    payload.Text,
	}
	if payload.HTML != "" {
		newContent.Format = event.FormatHTML
		newContent.FormattedBody = payload.HTML
	}

	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    "* " + payload.Text,
		NewContent: newContent,
		RelatesTo: &event.RelatesTo{
			Type:    event.RelReplace,
			EventID: id.EventID(payload.EventID),
		},
	}
	if payload.HTML != "" {
		content.Format = event.FormatHTML
		content.FormattedBody = "* " + payload.HTML
	}

	resp, err := ghost.Intent.SendMessage(ctx, roomID, event.EventMessage, &event.Content{Parsed: content}, nil)
	if err != nil {
		c.replyJSON(w, http.StatusInternalServerError, InboundWebhookResponse{Error: fmt.Sprintf("Failed to edit message: %v", err)})
		return
	}
	c.replyJSON(w, http.StatusOK, InboundWebhookResponse{EventID: resp.EventID.String()})
}

func (c *WebhookConnector) handleRedact(ctx context.Context, w http.ResponseWriter, ghost *bridgev2.Ghost, roomID id.RoomID, payload *InboundWebhookPayload) {
	if payload.EventID == "" {
		c.replyJSON(w, http.StatusBadRequest, InboundWebhookResponse{Error: "event_id is required"})
		return
	}

	content := &event.Content{
		Parsed: &event.RedactionEventContent{
			Redacts: id.EventID(payload.EventID),
			Reason:  payload.Reason,
		},
	}

	resp, err := ghost.Intent.SendMessage(ctx, roomID, event.EventRedaction, content, nil)
	if err != nil {
		c.replyJSON(w, http.StatusInternalServerError, InboundWebhookResponse{Error: fmt.Sprintf("Failed to redact: %v", err)})
		return
	}
	c.replyJSON(w, http.StatusOK, InboundWebhookResponse{EventID: resp.EventID.String()})
}

func (c *WebhookConnector) handleTyping(ctx context.Context, w http.ResponseWriter, ghost *bridgev2.Ghost, roomID id.RoomID, payload *InboundWebhookPayload) {
	timeout := time.Duration(payload.Timeout) * time.Millisecond
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if !payload.Typing {
		timeout = 0
	}

	err := ghost.Intent.MarkTyping(ctx, roomID, bridgev2.TypingTypeText, timeout)
	if err != nil {
		c.replyJSON(w, http.StatusInternalServerError, InboundWebhookResponse{Error: fmt.Sprintf("Failed to set typing: %v", err)})
		return
	}
	c.replyJSON(w, http.StatusOK, InboundWebhookResponse{})
}

func (c *WebhookConnector) handleReadReceipt(ctx context.Context, w http.ResponseWriter, ghost *bridgev2.Ghost, roomID id.RoomID, payload *InboundWebhookPayload) {
	if payload.EventID == "" {
		c.replyJSON(w, http.StatusBadRequest, InboundWebhookResponse{Error: "event_id is required"})
		return
	}

	err := ghost.Intent.MarkRead(ctx, roomID, id.EventID(payload.EventID), time.Now())
	if err != nil {
		c.replyJSON(w, http.StatusInternalServerError, InboundWebhookResponse{Error: fmt.Sprintf("Failed to send read receipt: %v", err)})
		return
	}
	c.replyJSON(w, http.StatusOK, InboundWebhookResponse{})
}

func (c *WebhookConnector) handleSetTopic(ctx context.Context, w http.ResponseWriter, ghost *bridgev2.Ghost, roomID id.RoomID, payload *InboundWebhookPayload) {
	if payload.Topic == "" {
		c.replyJSON(w, http.StatusBadRequest, InboundWebhookResponse{Error: "topic is required"})
		return
	}

	resp, err := ghost.Intent.SendState(ctx, roomID, event.StateTopic, "", &event.Content{
		Parsed: &event.TopicEventContent{Topic: payload.Topic},
	}, time.Now())
	if err != nil {
		c.replyJSON(w, http.StatusInternalServerError, InboundWebhookResponse{Error: fmt.Sprintf("Failed to set topic: %v", err)})
		return
	}
	c.replyJSON(w, http.StatusOK, InboundWebhookResponse{EventID: resp.EventID.String()})
}

func (c *WebhookConnector) handleSetRoomName(ctx context.Context, w http.ResponseWriter, ghost *bridgev2.Ghost, roomID id.RoomID, payload *InboundWebhookPayload) {
	if payload.RoomName == "" {
		c.replyJSON(w, http.StatusBadRequest, InboundWebhookResponse{Error: "room_name is required"})
		return
	}

	resp, err := ghost.Intent.SendState(ctx, roomID, event.StateRoomName, "", &event.Content{
		Parsed: &event.RoomNameEventContent{Name: payload.RoomName},
	}, time.Now())
	if err != nil {
		c.replyJSON(w, http.StatusInternalServerError, InboundWebhookResponse{Error: fmt.Sprintf("Failed to set room name: %v", err)})
		return
	}
	c.replyJSON(w, http.StatusOK, InboundWebhookResponse{EventID: resp.EventID.String()})
}

func (c *WebhookConnector) handleOutboundMatrixEvent(evt *event.Event) {
	members, err := c.Bridge.Matrix.GetMembers(context.Background(), evt.RoomID)
	if err != nil {
		return
	}

	c.PersonasLock.RLock()
	defer c.PersonasLock.RUnlock()

	for _, login := range c.Personas {
		ghost, err := c.Bridge.GetGhostByID(context.Background(), networkid.UserID(login.ID))
		if err != nil || ghost == nil {
			continue
		}

		inRoom := false
		if mem, ok := members[ghost.Intent.GetMXID()]; ok && mem.Membership != event.MembershipLeave {
			inRoom = true
		} else if evt.Type == event.StateMember && evt.StateKey != nil && *evt.StateKey == string(ghost.Intent.GetMXID()) {
			inRoom = true
		}

		if inRoom {
			c.sendOutbound(login, evt)
		}
	}
}

type OutboundPayload struct {
	PersonaID string       `json:"persona_id"`
	Event     *event.Event `json:"event"`
}

func (c *WebhookConnector) sendOutbound(login *bridgev2.UserLogin, evt *event.Event) {
	meta, ok := login.Metadata.(*UserLoginMetadata)
	if !ok || len(meta.OutboundURLs) == 0 {
		return
	}

	payload := OutboundPayload{
		PersonaID: string(login.ID),
		Event:     evt,
	}
	data, _ := json.Marshal(payload)

	for _, url := range meta.OutboundURLs {
		req, err := http.NewRequest("POST", url, bytes.NewReader(data))
		if err != nil {
			c.Bridge.Log.Err(err).Str("persona", string(login.ID)).Str("url", url).Msg("Failed to create outbound request")
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if meta.HeaderName != "" {
			req.Header.Set(meta.HeaderName, meta.HeaderValue)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			c.Bridge.Log.Err(err).Str("persona", string(login.ID)).Str("url", url).Msg("Failed to send outbound webhook")
			continue
		}
		resp.Body.Close()
	}
}

// WebhookAPI implementation to satisfy bridgev2
type WebhookAPI struct {
	Login *bridgev2.UserLogin
}

var _ bridgev2.NetworkAPI = (*WebhookAPI)(nil)
var _ bridgev2.IdentifierResolvingNetworkAPI = (*WebhookAPI)(nil)

func (a *WebhookAPI) Connect(ctx context.Context) {}
func (a *WebhookAPI) Disconnect()                 {}
func (a *WebhookAPI) IsLoggedIn() bool            { return true }
func (a *WebhookAPI) LogoutRemote(ctx context.Context) {}
func (a *WebhookAPI) IsThisUser(ctx context.Context, userID networkid.UserID) bool { return false }
func (a *WebhookAPI) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures { return &event.RoomFeatures{} }
func (a *WebhookAPI) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        networkid.MessageID(msg.Event.ID),
			SenderMXID: msg.Event.Sender,
		},
	}, nil
}
func (a *WebhookAPI) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) { return &bridgev2.ChatInfo{}, nil }
func (a *WebhookAPI) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) { return &bridgev2.UserInfo{}, nil }

// ResolveIdentifier allows ghosts to accept room invites.
// When a Matrix user invites a persona ghost, the bridgev2 framework checks
// if the NetworkAPI implements IdentifierResolvingNetworkAPI. Without this,
// invites are rejected with "This bridge does not support starting chats".
func (a *WebhookAPI) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	if !createChat {
		return &bridgev2.ResolveIdentifierResponse{
			UserID: networkid.UserID(identifier),
		}, nil
	}
	return &bridgev2.ResolveIdentifierResponse{
		UserID: networkid.UserID(identifier),
		Chat: &bridgev2.CreateChatResponse{
			PortalKey: networkid.PortalKey{
				ID:       networkid.PortalID(identifier),
				Receiver: a.Login.ID,
			},
		},
	}, nil
}

// Bot commands for managing personas

var CmdAddOutbound = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		if len(ce.Args) < 2 {
			ce.Reply("Usage: `add-outbound <persona_id> <url>`")
			return
		}
		personaID := ce.Args[0]
		url := ce.Args[1]

		login, err := ce.Bridge.GetExistingUserLoginByID(ce.Ctx, networkid.UserLoginID(personaID))
		if err != nil || login == nil {
			ce.Reply("Persona `%s` not found.", personaID)
			return
		}

		meta, ok := login.Metadata.(*UserLoginMetadata)
		if !ok {
			ce.Reply("Failed to read persona metadata.")
			return
		}

		for _, existing := range meta.OutboundURLs {
			if existing == url {
				ce.Reply("URL already registered for persona `%s`.", personaID)
				return
			}
		}

		meta.OutboundURLs = append(meta.OutboundURLs, url)
		err = login.Save(ce.Ctx)
		if err != nil {
			ce.Reply("Failed to save: %v", err)
			return
		}

		ce.Reply("Added outbound URL to persona `%s`. Total URLs: %d", personaID, len(meta.OutboundURLs))
	},
	Name: "add-outbound",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Add an outbound webhook URL to a persona",
		Args:        "<persona_id> <url>",
	},
	RequiresAdmin: true,
}

var CmdRemoveOutbound = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		if len(ce.Args) < 2 {
			ce.Reply("Usage: `remove-outbound <persona_id> <url>`")
			return
		}
		personaID := ce.Args[0]
		url := ce.Args[1]

		login, err := ce.Bridge.GetExistingUserLoginByID(ce.Ctx, networkid.UserLoginID(personaID))
		if err != nil || login == nil {
			ce.Reply("Persona `%s` not found.", personaID)
			return
		}

		meta, ok := login.Metadata.(*UserLoginMetadata)
		if !ok {
			ce.Reply("Failed to read persona metadata.")
			return
		}

		found := false
		newURLs := make([]string, 0, len(meta.OutboundURLs))
		for _, existing := range meta.OutboundURLs {
			if existing == url {
				found = true
			} else {
				newURLs = append(newURLs, existing)
			}
		}

		if !found {
			ce.Reply("URL not found for persona `%s`.", personaID)
			return
		}

		meta.OutboundURLs = newURLs
		err = login.Save(ce.Ctx)
		if err != nil {
			ce.Reply("Failed to save: %v", err)
			return
		}

		ce.Reply("Removed outbound URL from persona `%s`. Remaining URLs: %d", personaID, len(meta.OutboundURLs))
	},
	Name: "remove-outbound",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Remove an outbound webhook URL from a persona",
		Args:        "<persona_id> <url>",
	},
	RequiresAdmin: true,
}

var CmdListOutbound = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		if len(ce.Args) < 1 {
			ce.Reply("Usage: `list-outbound <persona_id>`")
			return
		}
		personaID := ce.Args[0]

		login, err := ce.Bridge.GetExistingUserLoginByID(ce.Ctx, networkid.UserLoginID(personaID))
		if err != nil || login == nil {
			ce.Reply("Persona `%s` not found.", personaID)
			return
		}

		meta, ok := login.Metadata.(*UserLoginMetadata)
		if !ok {
			ce.Reply("Failed to read persona metadata.")
			return
		}

		if len(meta.OutboundURLs) == 0 {
			ce.Reply("Persona `%s` has no outbound URLs configured.", personaID)
			return
		}

		msg := fmt.Sprintf("Outbound URLs for persona `%s`:\n", personaID)
		for i, url := range meta.OutboundURLs {
			msg += fmt.Sprintf("%d. `%s`\n", i+1, url)
		}
		ce.Reply(msg)
	},
	Name: "list-outbound",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "List all outbound webhook URLs for a persona",
		Args:        "<persona_id>",
	},
	RequiresAdmin: true,
}

var CmdSetDisplayName = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		if len(ce.Args) < 2 {
			ce.Reply("Usage: `set-displayname <persona_id> <name...>`")
			return
		}
		personaID := ce.Args[0]
		displayName := strings.Join(ce.Args[1:], " ")

		login, err := ce.Bridge.GetExistingUserLoginByID(ce.Ctx, networkid.UserLoginID(personaID))
		if err != nil || login == nil {
			ce.Reply("Persona `%s` not found.", personaID)
			return
		}

		ghost, err := ce.Bridge.GetGhostByID(ce.Ctx, networkid.UserID(login.ID))
		if err != nil || ghost == nil {
			ce.Reply("Ghost for persona `%s` not found.", personaID)
			return
		}

		err = ghost.Intent.SetDisplayName(ce.Ctx, displayName)
		if err != nil {
			ce.Reply("Failed to set display name: %v", err)
			return
		}

		ce.Reply("Display name for persona `%s` set to **%s**.", personaID, displayName)
	},
	Name: "set-displayname",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Set the Matrix display name for a persona's ghost",
		Args:        "<persona_id> <name...>",
	},
	RequiresAdmin: true,
}

var CmdSetAvatar = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		if len(ce.Args) < 2 {
			ce.Reply("Usage: `set-avatar <persona_id> <mxc://...>`")
			return
		}
		personaID := ce.Args[0]
		avatarURL := ce.Args[1]

		login, err := ce.Bridge.GetExistingUserLoginByID(ce.Ctx, networkid.UserLoginID(personaID))
		if err != nil || login == nil {
			ce.Reply("Persona `%s` not found.", personaID)
			return
		}

		ghost, err := ce.Bridge.GetGhostByID(ce.Ctx, networkid.UserID(login.ID))
		if err != nil || ghost == nil {
			ce.Reply("Ghost for persona `%s` not found.", personaID)
			return
		}

		if !strings.HasPrefix(avatarURL, "mxc://") {
			ce.Reply("Invalid URL: must start with `mxc://`")
			return
		}

		err = ghost.Intent.SetAvatarURL(ce.Ctx, id.ContentURIString(avatarURL))
		if err != nil {
			ce.Reply("Failed to set avatar: %v", err)
			return
		}

		ce.Reply("Avatar for persona `%s` updated.", personaID)
	},
	Name: "set-avatar",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Set the Matrix avatar for a persona's ghost (requires mxc:// URL)",
		Args:        "<persona_id> <mxc://...>",
	},
	RequiresAdmin: true,
}

