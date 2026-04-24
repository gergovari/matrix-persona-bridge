package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"go.mau.fi/util/configupgrade"
	"go.mau.fi/util/random"
	"maunium.net/go/mautrix/bridgev2"
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
	OutboundURL  string `json:"outbound_url"`
	InboundToken string `json:"inbound_token"`
	HeaderName   string `json:"header_name"`
	HeaderValue  string `json:"header_value"`
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
network_url: https://webhook.local
inbound:
  port: 8080
  path: /webhook
`, &c.Config, configupgrade.SimpleUpgrader(func(helper configupgrade.Helper) {})
}

func (c *WebhookConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		UserLogin: func() any {
			return &UserLoginMetadata{}
		},
	}
}

func (c *WebhookConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	login.Client = &WebhookAPI{}
	
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
	User        *bridgev2.User
	Connector   *WebhookConnector
	PersonaID   string
	OutboundURL string
}

func (pl *PersonaLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "persona-details",
		Instructions: "Enter details for the new Persona",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type: bridgev2.LoginInputFieldTypeUsername,
					ID:   "persona_id",
					Name: "Persona ID (e.g., n8n-bot-1)",
				},
				{
					Type: bridgev2.LoginInputFieldTypeUsername,
					ID:   "outbound_url",
					Name: "Outbound Webhook URL",
				},
			},
		},
	}, nil
}

func (pl *PersonaLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	pl.PersonaID = input["persona_id"]
	pl.OutboundURL = input["outbound_url"]

	inboundToken := random.String(32) // Generate a secure 32-character random token
	headerName := "X-Webhook-Token"
	headerValue := random.String(32)

	ul, err := pl.User.NewLogin(ctx, &database.UserLogin{
		ID:         networkid.UserLoginID(pl.PersonaID),
		RemoteName: pl.PersonaID,
		Metadata: &UserLoginMetadata{
			OutboundURL:  pl.OutboundURL,
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

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       "complete",
		Instructions: fmt.Sprintf("Persona created successfully!\n\n**Keep these details secret:**\n- **Inbound URL:** `http://<host>:%d%s/%s`\n- **Required Header Name:** `%s`\n- **Required Header Token:** `%s`\n\nConfigure your webhook backend to send this header with every request.", pl.Connector.Config.Inbound.Port, pl.Connector.Config.Inbound.Path, inboundToken, headerName, headerValue),
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
	Action string `json:"action"` // "send_message", "join_room"
	RoomID string `json:"room_id"`
	Text   string `json:"text"`
}

func (c *WebhookConnector) handleInbound(w http.ResponseWriter, r *http.Request) {
	inboundToken := r.PathValue("inboundToken")
	if inboundToken == "" {
		http.Error(w, "Missing inbound token", http.StatusBadRequest)
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
		http.Error(w, "Invalid token or persona not found", http.StatusUnauthorized)
		return
	}

	if loginMeta.HeaderName != "" {
		if r.Header.Get(loginMeta.HeaderName) != loginMeta.HeaderValue {
			http.Error(w, "Invalid security header", http.StatusUnauthorized)
			return
		}
	}

	ghost, err := c.Bridge.GetGhostByID(r.Context(), networkid.UserID(login.ID))
	if err != nil || ghost == nil {
		http.Error(w, "Ghost not found", http.StatusInternalServerError)
		return
	}

	var payload InboundWebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	roomID := id.RoomID(payload.RoomID)

	if payload.Action == "send_message" {
		content := &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    payload.Text,
		}
		_, err = ghost.Intent.SendMessage(r.Context(), roomID, event.EventMessage, &event.Content{Parsed: content}, nil)
	} else if payload.Action == "join_room" {
		err = ghost.Intent.EnsureJoined(r.Context(), roomID)
	} else {
		http.Error(w, "Unknown action", http.StatusBadRequest)
		return
	}

	if err != nil {
		c.Bridge.Log.Err(err).Msg("Failed to execute inbound action")
		http.Error(w, "Matrix error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
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
	if !ok || meta.OutboundURL == "" {
		return
	}

	payload := OutboundPayload{
		PersonaID: string(login.ID),
		Event:     evt,
	}
	data, _ := json.Marshal(payload)
	resp, err := http.Post(meta.OutboundURL, "application/json", bytes.NewReader(data))
	if err != nil {
		c.Bridge.Log.Err(err).Str("persona", string(login.ID)).Msg("Failed to send outbound webhook")
		return
	}
	resp.Body.Close()
}

// Dummy WebhookAPI implementation to satisfy bridgev2
type WebhookAPI struct{}

var _ bridgev2.NetworkAPI = (*WebhookAPI)(nil)

func (a *WebhookAPI) Connect(ctx context.Context) {}
func (a *WebhookAPI) Disconnect()                 {}
func (a *WebhookAPI) IsLoggedIn() bool            { return true }
func (a *WebhookAPI) LogoutRemote(ctx context.Context) {}
func (a *WebhookAPI) IsThisUser(ctx context.Context, userID networkid.UserID) bool { return false }
func (a *WebhookAPI) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures { return &event.RoomFeatures{} }
func (a *WebhookAPI) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) { return nil, nil }
func (a *WebhookAPI) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) { return &bridgev2.ChatInfo{}, nil }
func (a *WebhookAPI) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) { return &bridgev2.UserInfo{}, nil }
