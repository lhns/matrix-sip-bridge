// Package connector implements the bridgev2 NetworkConnector for the
// messaging half of the bridge, and owns the lifecycle of the call subsystem.
package connector

import (
	"context"
	"fmt"
	"slices"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/lhns/matrix-sip-bridge/pkg/calls"
	sipdb "github.com/lhns/matrix-sip-bridge/pkg/database"
	"github.com/lhns/matrix-sip-bridge/pkg/siptransport"
)

// SIPConnector is the bridgev2 network connector.
type SIPConnector struct {
	Config Config

	br    *bridgev2.Bridge
	sip   *siptransport.Transport
	calls *calls.Subsystem
	db    *sipdb.Database

	// cancel stops the SIP endpoint and the call subsystem loops.
	cancel context.CancelFunc
}

var (
	_ bridgev2.NetworkConnector = (*SIPConnector)(nil)
	_ bridgev2.StoppableNetwork = (*SIPConnector)(nil)
)

// Init is called before Start with the bridge fully constructed.
//
// The SIP endpoint is not built here: it validates its configuration and
// resolves its media address, either of which can fail, and Init cannot report
// an error.
func (sc *SIPConnector) Init(bridge *bridgev2.Bridge) {
	sc.br = bridge
	sc.Config.applyDefaults()
	sc.db = sipdb.New(bridge.DB.Database, bridge.Log.With().Str("db_section", "sip").Logger())
	bridge.Commands.(*commands.Processor).AddHandlers(sc.dialCommand())
}

// Start brings up the SIP endpoint and the call subsystem.
func (sc *SIPConnector) Start(ctx context.Context) error {
	// The bridge's own context, not ctx: ctx is the startup context and is
	// cancelled once Start returns.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sc.cancel = cancel

	sc.warnAboutRelay()

	sip, err := siptransport.New(sc.Config.SIP, sc.br.Log.With().Str("component", "sip").Logger())
	if err != nil {
		cancel()
		return err
	}
	sc.sip = sip
	sc.calls = calls.New(
		sc.Config.Calls, sc.br, sipTelephony{sip, sc.Config.SIP.ConferenceHeader}, sc.db,
		sc.br.Log.With().Str("component", "calls").Logger(),
	)

	if err := sc.calls.Start(ctx, sc.eventRegistrar()); err != nil {
		cancel()
		return err
	}

	// Handlers must be registered before the endpoint starts listening, or an
	// INVITE that arrives immediately is answered 503.
	if sc.Config.Messages.Enabled {
		sip.OnMessage(sc.handleInboundMessage)
	}
	if sc.Config.Calls.Enabled {
		sip.OnInvite(func(ctx context.Context, call *siptransport.InboundCall) {
			sc.calls.HandleInboundCall(ctx, call)
		})
	}
	go func() {
		if err := sip.Run(runCtx); err != nil {
			sc.br.Log.Err(err).Msg("The SIP endpoint stopped")
		}
	}()
	go sc.calls.Run(runCtx)
	return nil
}

// sipReady reports whether the SIP endpoint is usable, for the bridge state.
func (sc *SIPConnector) sipReady() bool {
	return sc.sip != nil && sc.sip.Ready()
}

// sipTelephony adapts the transport to the narrow interface the call subsystem
// takes, and owns the one SIP detail that subsystem should not: the name of
// the header carrying the conference.
type sipTelephony struct {
	transport        *siptransport.Transport
	conferenceHeader string
}

func (s sipTelephony) Invite(ctx context.Context, to, conference string) (calls.OutboundLeg, error) {
	return s.transport.Invite(ctx, to, map[string]string{s.conferenceHeader: conference})
}

// warnAboutRelay checks the two settings without which the bridge appears to
// work but silently drops every Matrix message from anyone but the one user who
// happens to own the login.
//
// There is no per-user SIP account to log into, so all Matrix users share the
// single "sip" login and reach it through relay mode.
func (sc *SIPConnector) warnAboutRelay() {
	relay := sc.br.Config.Relay
	if !relay.Enabled {
		sc.br.Log.Warn().Msg("bridge.relay.enabled is false; only the user who logged in can send messages")
		return
	}
	if !slices.Contains(relay.DefaultRelays, LoginID) {
		sc.br.Log.Warn().
			Str("expected", LoginID).
			Msg("bridge.relay.default_relays does not list the shared login; portals will not relay")
	}
}

// Stop shuts the loops down.
func (sc *SIPConnector) Stop() {
	if sc.cancel != nil {
		sc.cancel()
	}
}

// eventRegistrar exposes the appservice event processor to the call subsystem.
//
// bridgev2 has no API for subscribing to arbitrary Matrix event types, so the
// concrete matrix.Connector has to be reached for. If the Matrix connector is
// ever something else, calls simply cannot be bridged, so this degrades to a
// no-op registrar rather than failing the whole bridge.
func (sc *SIPConnector) eventRegistrar() calls.EventRegistrar {
	mx, ok := sc.br.Matrix.(*matrix.Connector)
	if !ok {
		sc.br.Log.Warn().Msg("Matrix connector is not the appservice one, calls will not be bridged")
		return noopRegistrar{}
	}
	return mx.EventProcessor
}

type noopRegistrar struct{}

func (noopRegistrar) On(evtType event.Type, handler func(ctx context.Context, evt *event.Event)) {}

func (sc *SIPConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:      "SIP",
		NetworkURL:       "https://github.com/lhns/matrix-sip-bridge",
		NetworkID:        "sip",
		BeeperBridgeType: "github.com/lhns/matrix-sip-bridge",
		DefaultPort:      29340,
	}
}

func (sc *SIPConnector) GetBridgeInfoVersion() (info, capabilities int) {
	return 1, 1
}

func (sc *SIPConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{}
}

// GetDBMetaTypes declares no extra metadata. Everything the bridge remembers
// beyond bridgev2's own tables is call state, which lives in its own schema.
func (sc *SIPConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{}
}

// LoadUserLogin attaches the client to the one static login.
func (sc *SIPConnector) LoadUserLogin(_ context.Context, login *bridgev2.UserLogin) error {
	if login.ID != LoginID {
		return fmt.Errorf("unexpected login ID %q, expected %q", login.ID, LoginID)
	}
	login.Client = &SIPClient{
		UserLogin: login,
		conn:      sc,
	}
	return nil
}

// GetLoginFlows offers the single flow that just claims the shared login.
func (sc *SIPConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "SIP",
		Description: "Use the bridge's shared SIP endpoint",
		ID:          LoginFlowID,
	}}
}

// CreateLogin returns a login process that completes immediately.
//
// There is nothing to log in to: the SIP identity is in the bridge config, not
// per user. The login exists only because bridgev2 requires a UserLogin to own
// portals and to be a relay target.
func (sc *SIPConnector) CreateLogin(_ context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != LoginFlowID {
		return nil, fmt.Errorf("unknown login flow ID %q", flowID)
	}
	return &SIPLogin{User: user}, nil
}

// SIPLogin is the no-op login process.
type SIPLogin struct {
	User *bridgev2.User
}

var _ bridgev2.LoginProcess = (*SIPLogin)(nil)

// Start finishes the login in one step with a fixed ID, so that every Matrix
// user ends up on the same UserLogin and the same portals.
func (sl *SIPLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	ul, err := sl.User.NewLogin(ctx, &database.UserLogin{
		ID:         networkid.UserLoginID(LoginID),
		RemoteName: "SIP",
	}, &bridgev2.NewLoginParams{
		// A second Matrix user logging in must adopt the existing login rather
		// than create a duplicate, or bridgev2 would try to run two clients
		// over one SIP endpoint.
		DeleteOnConflict: false,
	})
	if err != nil {
		return nil, err
	}
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       "de.lhns.sip.complete",
		Instructions: "Connected to the shared SIP endpoint",
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}

func (sl *SIPLogin) Cancel() {}
