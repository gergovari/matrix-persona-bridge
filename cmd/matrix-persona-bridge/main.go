package main

import (
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/gergovari/matrix-persona-bridge/pkg/connector"
)

var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	m := mxmain.BridgeMain{
		Name:        "matrix-persona-bridge",
		Description: "A Matrix-Webhook bridge",
		URL:         "https://github.com/gergovari/matrix-persona-bridge",
		Version:     "0.0.1",
		Connector:   &connector.WebhookConnector{},
	}
	m.PostInit = func() {
		m.Bridge.Commands.(*commands.Processor).AddHandlers(
			connector.CmdAddOutbound,
			connector.CmdRemoveOutbound,
			connector.CmdListOutbound,
			connector.CmdSetDisplayName,
			connector.CmdSetAvatar,
		)
	}
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}
