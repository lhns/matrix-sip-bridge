// Command matrix-sip-bridge bridges Matrix to a SIP/telephony network reached
// through Asterisk: text as SIP MESSAGE, and calls as real media via LiveKit.
package main

import (
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/lhns/matrix-sip-bridge/pkg/connector"
)

// Version is the latest release. mxmain compares it with the git tag baked in
// at build time to tell a release build from a dev one.
var Version = "0.1.0"

// Tag, Commit and BuildTime are filled in by the linker; see the Makefile.
var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	m := mxmain.BridgeMain{
		Name:        "matrix-sip-bridge",
		Description: "A Matrix-SIP bridge for messages and calls through Asterisk",
		URL:         "https://github.com/lhns/matrix-sip-bridge",
		Version:     Version,
		Connector:   &connector.SIPConnector{},
	}
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}
