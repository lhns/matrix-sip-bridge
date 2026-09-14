package calls

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// LiveKitConfig points the bridge at a LiveKit server. All of it is
// site-specific and comes from the bridge config file.
type LiveKitConfig struct {
	URL       string        `yaml:"url"`
	APIKey    string        `yaml:"api_key"`
	APISecret string        `yaml:"api_secret"`
	Timeout   time.Duration `yaml:"timeout"`

	// TrunkName identifies the outbound trunk object the bridge reconciles.
	TrunkName string `yaml:"trunk_name"`
	// TrunkAddress is the SIP host livekit-sip dials to reach the conference.
	TrunkAddress string `yaml:"trunk_address"`
	// TrunkNumber is the caller number livekit-sip presents.
	TrunkNumber string `yaml:"trunk_number"`
	// TrunkAuthUsername and TrunkAuthPassword are optional SIP digest
	// credentials for the trunk.
	TrunkAuthUsername string `yaml:"trunk_auth_username"`
	TrunkAuthPassword string `yaml:"trunk_auth_password"`
	// TrunkReconcileInterval is how often the trunk is re-checked; see
	// reconcileTrunk for what that is guarding against.
	TrunkReconcileInterval time.Duration `yaml:"trunk_reconcile_interval"`
}

// LiveKitClient talks to the LiveKit SIP service over twirp.
//
// The JSON encoding of twirp is used rather than protobuf so that the bridge
// does not have to vendor livekit/protocol, which pulls in most of the LiveKit
// server SDK for three RPCs.
type LiveKitClient struct {
	cfg  LiveKitConfig
	http *http.Client
}

// NewLiveKitClient builds a client. It does not contact the server.
func NewLiveKitClient(cfg LiveKitConfig) *LiveKitClient {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &LiveKitClient{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.Timeout},
	}
}

// grants are the LiveKit access-token claims. The JSON names follow LiveKit's
// wire format, which is lowerCamelCase and not the Go convention.
type grants struct {
	Video *videoGrant `json:"video,omitempty"`
	SIP   *sipGrant   `json:"sip,omitempty"`
}

type videoGrant struct {
	RoomAdmin  bool   `json:"roomAdmin,omitempty"`
	RoomJoin   bool   `json:"roomJoin,omitempty"`
	RoomCreate bool   `json:"roomCreate,omitempty"`
	Room       string `json:"room,omitempty"`
}

type sipGrant struct {
	Admin bool `json:"admin,omitempty"`
	Call  bool `json:"call,omitempty"`
}

type tokenClaims struct {
	jwt.RegisteredClaims
	grants
}

// token mints a short-lived LiveKit access token. Tokens are minted per request
// rather than cached: they are cheap, and a cached token outliving a rotated
// API secret is a confusing failure mode.
func (c *LiveKitClient) token(g grants) (string, error) {
	now := time.Now()
	claims := tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    c.cfg.APIKey,
			Subject:   c.cfg.APIKey,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-30 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		},
		grants: g,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(c.cfg.APISecret))
}

// twirpError is the JSON error body twirp returns on a non-200.
type twirpError struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

func (e *twirpError) Error() string {
	return fmt.Sprintf("livekit %s: %s", e.Code, e.Msg)
}

// maxResponseBytes caps how much of a LiveKit reply is read, so a misrouted
// request that lands on some other HTTP server cannot exhaust memory.
const maxResponseBytes = 1 << 20

// call performs one twirp JSON RPC against the livekit.SIP service.
func (c *LiveKitClient) call(ctx context.Context, method string, g grants, req, resp any) error {
	return c.callService(ctx, "livekit.SIP", method, g, req, resp)
}

// callService performs one twirp JSON RPC against any LiveKit service.
func (c *LiveKitClient) callService(ctx context.Context, service, method string, g grants, req, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", method, err)
	}
	tok, err := c.token(g)
	if err != nil {
		return fmt.Errorf("mint token: %w", err)
	}
	url := strings.TrimSuffix(c.cfg.URL, "/") + "/twirp/" + service + "/" + method
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+tok)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%s: read response: %w", method, err)
	}
	if httpResp.StatusCode != http.StatusOK {
		var te twirpError
		if json.Unmarshal(respBody, &te) == nil && te.Code != "" {
			return &te
		}
		return fmt.Errorf("%s: http %d", method, httpResp.StatusCode)
	}
	if resp == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, resp); err != nil {
		return fmt.Errorf("%s: decode response: %w", method, err)
	}
	return nil
}

// SIPOutboundTrunk is the subset of livekit.SIPOutboundTrunkInfo the bridge
// reads or writes.
type SIPOutboundTrunk struct {
	SipTrunkID   string   `json:"sip_trunk_id,omitempty"`
	Name         string   `json:"name,omitempty"`
	Address      string   `json:"address,omitempty"`
	Numbers      []string `json:"numbers,omitempty"`
	AuthUsername string   `json:"auth_username,omitempty"`
	AuthPassword string   `json:"auth_password,omitempty"`
}

type listSIPOutboundTrunkResponse struct {
	Items []*SIPOutboundTrunk `json:"items"`
}

// ListSIPOutboundTrunk returns every outbound trunk the SIP service knows.
func (c *LiveKitClient) ListSIPOutboundTrunk(ctx context.Context) ([]*SIPOutboundTrunk, error) {
	var resp listSIPOutboundTrunkResponse
	err := c.call(ctx, "ListSIPOutboundTrunk", grants{SIP: &sipGrant{Admin: true}}, struct{}{}, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Items, nil
}

type createSIPOutboundTrunkRequest struct {
	Trunk *SIPOutboundTrunk `json:"trunk"`
}

// CreateSIPOutboundTrunk registers an outbound trunk.
func (c *LiveKitClient) CreateSIPOutboundTrunk(ctx context.Context, trunk *SIPOutboundTrunk) (*SIPOutboundTrunk, error) {
	var resp SIPOutboundTrunk
	err := c.call(ctx, "CreateSIPOutboundTrunk", grants{SIP: &sipGrant{Admin: true}},
		&createSIPOutboundTrunkRequest{Trunk: trunk}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

type updateSIPOutboundTrunkRequest struct {
	SipTrunkID string `json:"sip_trunk_id"`
	// Replace is the "replace" arm of the request's action oneof. LiveKit
	// swaps the whole stored object for this one, keeping only the trunk ID,
	// so every field must be sent; the "update" arm merges instead and cannot
	// clear a field.
	Replace *SIPOutboundTrunk `json:"replace"`
}

// UpdateSIPOutboundTrunk replaces an existing trunk in place, keeping its ID.
func (c *LiveKitClient) UpdateSIPOutboundTrunk(ctx context.Context, id string, trunk *SIPOutboundTrunk) (*SIPOutboundTrunk, error) {
	var resp SIPOutboundTrunk
	err := c.call(ctx, "UpdateSIPOutboundTrunk", grants{SIP: &sipGrant{Admin: true}},
		&updateSIPOutboundTrunkRequest{SipTrunkID: id, Replace: trunk}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// CreateSIPParticipantRequest asks livekit-sip to place a SIP call and put the
// far end into a LiveKit room.
type CreateSIPParticipantRequest struct {
	SipTrunkID          string `json:"sip_trunk_id"`
	SipCallTo           string `json:"sip_call_to"`
	RoomName            string `json:"room_name"`
	ParticipantIdentity string `json:"participant_identity"`
	ParticipantName     string `json:"participant_name,omitempty"`
	WaitUntilAnswered   bool   `json:"wait_until_answered,omitempty"`
}

// SIPParticipant is the subset of livekit.SIPParticipantInfo the bridge keeps.
type SIPParticipant struct {
	ParticipantID string `json:"participant_id,omitempty"`
}

// CreateSIPParticipant dials out through livekit-sip.
//
// The bridge always originates rather than letting LiveKit accept an inbound
// leg through a dispatch rule, because only this API lets the participant
// identity be chosen; see LiveKitIdentity.
func (c *LiveKitClient) CreateSIPParticipant(ctx context.Context, req *CreateSIPParticipantRequest) (*SIPParticipant, error) {
	var resp SIPParticipant
	g := grants{
		SIP: &sipGrant{Call: true},
		Video: &videoGrant{
			RoomJoin:   true,
			RoomCreate: true,
			RoomAdmin:  true,
			Room:       req.RoomName,
		},
	}
	if err := c.call(ctx, "CreateSIPParticipant", g, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// roomService is the twirp service that owns participants, as opposed to SIP
// trunks and SIP participants.
const roomService = "livekit.RoomService"

// createRoomRequest is the subset of livekit.CreateRoomRequest the bridge
// sends. Every optional field is left zero on purpose: LiveKit only overwrites
// empty_timeout, departure_timeout, max_participants and metadata on an
// existing room when the request sets them, so an empty request cannot clobber
// the settings the MatrixRTC authorisation service gave a room it created.
type createRoomRequest struct {
	Name string `json:"name"`
}

// Room is the subset of livekit.Room the bridge reads back.
type Room struct {
	Sid  string `json:"sid,omitempty"`
	Name string `json:"name,omitempty"`
}

// EnsureRoom creates the LiveKit room if it does not exist yet.
//
// Nothing else will. MatrixRTC deployments run the SFU with room.auto_create
// off and let the authorisation service create each room as it hands out the
// first token, so a room nobody has joined from Matrix does not exist, and
// livekit-sip -- whose own token carries no roomCreate grant -- cannot join
// one: CreateSIPParticipant fails with "update room failed: not found".
//
// CreateRoom on an existing room is an update, not an error, so this is safe
// to call before every call in either direction.
func (c *LiveKitClient) EnsureRoom(ctx context.Context, room string) (*Room, error) {
	var resp Room
	g := grants{Video: &videoGrant{RoomCreate: true}}
	if err := c.callService(ctx, roomService, "CreateRoom", g, &createRoomRequest{Name: room}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

type listParticipantsRequest struct {
	Room string `json:"room"`
}

type listParticipantsResponse struct {
	Participants []struct {
		Identity string `json:"identity,omitempty"`
	} `json:"participants"`
}

// ParticipantPresent reports whether an identity is still in a LiveKit room.
// It is the bridge's only liveness signal for a call in progress; see
// Subsystem.runParticipantWatcher.
func (c *LiveKitClient) ParticipantPresent(ctx context.Context, room, identity string) (bool, error) {
	var resp listParticipantsResponse
	g := grants{Video: &videoGrant{RoomAdmin: true, Room: room}}
	if err := c.callService(ctx, roomService, "ListParticipants", g, &listParticipantsRequest{Room: room}, &resp); err != nil {
		return false, err
	}
	for _, p := range resp.Participants {
		if p.Identity == identity {
			return true, nil
		}
	}
	return false, nil
}

type removeParticipantRequest struct {
	Room     string `json:"room"`
	Identity string `json:"identity"`
}

// RemoveParticipant disconnects a participant.
//
// For the SIP participant this is the only way the bridge can hang up on the
// phone: it drops livekit-sip's leg out of the conference. Whether that also
// ends the far end's call is up to the conference configuration, not to this.
func (c *LiveKitClient) RemoveParticipant(ctx context.Context, room, identity string) error {
	g := grants{Video: &videoGrant{RoomAdmin: true, Room: room}}
	return c.callService(ctx, roomService, "RemoveParticipant", g,
		&removeParticipantRequest{Room: room, Identity: identity}, nil)
}
