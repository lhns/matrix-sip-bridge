package database

import (
	"context"
	"time"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/id"
)

// CallDirection says who placed the call.
type CallDirection string

const (
	// DirectionInbound is a call from the phone network, parked by Asterisk in
	// a ConfBridge and waiting for the bridge to pull it into LiveKit.
	DirectionInbound CallDirection = "inbound"
	// DirectionOutbound is a call the bridge asked Asterisk to place.
	DirectionOutbound CallDirection = "outbound"
)

// CallState is where a call is in its life cycle.
type CallState string

const (
	// StateRinging means the SIP leg exists but no Matrix user has joined the
	// RTC session yet, so there is nothing to bridge the media to.
	StateRinging CallState = "ringing"
	// StateBridged means a LiveKit SIP participant has been created.
	StateBridged CallState = "bridged"
	// StateEnded is terminal.
	StateEnded CallState = "ended"
)

// Call is one telephone call being bridged.
type Call struct {
	qh *dbutil.QueryHelper[*Call]

	// CallID is the bridge's own identifier, not Asterisk's and not LiveKit's.
	CallID string
	// PortalID is the E.164 number without the leading plus, matching the
	// bridgev2 portal and ghost IDs.
	PortalID string
	RoomID   id.RoomID

	Direction CallDirection
	// Conference is the Asterisk ConfBridge name holding the SIP leg.
	Conference string
	// Channel is the Asterisk channel of the SIP leg, needed to hang it up.
	Channel string

	// LKRoom is the LiveKit room name derived from RoomID; see
	// calls.LiveKitRoomName.
	LKRoom string
	// LKIdentity is the participant identity given to livekit-sip, derived so
	// that Matrix clients can attribute the stream to the ghost.
	LKIdentity string
	// LKParticipant is the participant ID LiveKit assigned, empty until the
	// SIP participant has been created.
	LKParticipant string

	State     CallState
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CallQuery runs the queries over the sip_call table.
type CallQuery struct {
	*dbutil.QueryHelper[*Call]
}

func newCall(qh *dbutil.QueryHelper[*Call]) *Call {
	return &Call{qh: qh}
}

const (
	callColumns = `call_id, portal_id, room_id, direction, conference, channel,
	               lk_room, lk_identity, lk_participant, state, created_at, updated_at`

	insertCallQuery = `
		INSERT INTO sip_call (` + callColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`
	updateCallQuery = `
		UPDATE sip_call
		SET conference = $2, channel = $3, lk_room = $4, lk_identity = $5,
		    lk_participant = $6, state = $7, updated_at = $8
		WHERE call_id = $1
	`
	getCallByIDQuery = `SELECT ` + callColumns + ` FROM sip_call WHERE call_id = $1`
	// An "active" call is anything not yet ended. There is at most one per
	// number, so ordering by created_at only matters if state got out of sync.
	getActiveCallByPortalQuery = `
		SELECT ` + callColumns + `
		FROM sip_call WHERE portal_id = $1 AND state <> 'ended'
		ORDER BY created_at DESC LIMIT 1
	`
	getActiveCallByConferenceQuery = `
		SELECT ` + callColumns + `
		FROM sip_call WHERE conference = $1 AND state <> 'ended'
		ORDER BY created_at DESC LIMIT 1
	`
	getAllActiveCallsQuery = `
		SELECT ` + callColumns + ` FROM sip_call WHERE state <> 'ended'
	`
	endAllCallsQuery      = `UPDATE sip_call SET state = 'ended', updated_at = $1 WHERE state <> 'ended'`
	deleteEndedCallsQuery = `DELETE FROM sip_call WHERE state = 'ended' AND updated_at < $1`
)

// Scan reads one row.
func (c *Call) Scan(row dbutil.Scannable) (*Call, error) {
	var createdAt, updatedAt int64
	err := row.Scan(
		&c.CallID, &c.PortalID, &c.RoomID, &c.Direction, &c.Conference, &c.Channel,
		&c.LKRoom, &c.LKIdentity, &c.LKParticipant, &c.State, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	c.CreatedAt = time.UnixMilli(createdAt)
	c.UpdatedAt = time.UnixMilli(updatedAt)
	return c, nil
}

func (c *Call) insertValues() []any {
	return []any{
		c.CallID, c.PortalID, c.RoomID, c.Direction, c.Conference, c.Channel,
		c.LKRoom, c.LKIdentity, c.LKParticipant, c.State,
		c.CreatedAt.UnixMilli(), c.UpdatedAt.UnixMilli(),
	}
}

// Insert stores a new call.
func (cq *CallQuery) Insert(ctx context.Context, c *Call) error {
	now := time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	return cq.Exec(ctx, insertCallQuery, c.insertValues()...)
}

// Update writes back the mutable fields.
func (cq *CallQuery) Update(ctx context.Context, c *Call) error {
	c.UpdatedAt = time.Now()
	return cq.Exec(ctx, updateCallQuery,
		c.CallID, c.Conference, c.Channel, c.LKRoom, c.LKIdentity,
		c.LKParticipant, c.State, c.UpdatedAt.UnixMilli())
}

// GetByID returns one call, or nil if it does not exist.
func (cq *CallQuery) GetByID(ctx context.Context, callID string) (*Call, error) {
	return cq.QueryOne(ctx, getCallByIDQuery, callID)
}

// GetActiveByPortal returns the call in progress for a number, or nil.
func (cq *CallQuery) GetActiveByPortal(ctx context.Context, portalID string) (*Call, error) {
	return cq.QueryOne(ctx, getActiveCallByPortalQuery, portalID)
}

// GetActiveByConference resolves a ConfBridge event back to a call, or nil.
func (cq *CallQuery) GetActiveByConference(ctx context.Context, conference string) (*Call, error) {
	return cq.QueryOne(ctx, getActiveCallByConferenceQuery, conference)
}

// GetAllActive returns every call not yet ended.
func (cq *CallQuery) GetAllActive(ctx context.Context) ([]*Call, error) {
	return cq.QueryMany(ctx, getAllActiveCallsQuery)
}

// EndAll marks every call ended.
//
// Call state lives in Asterisk and LiveKit, not here; this table is only a
// cache of it. After a bridge restart the cache is stale and the channels it
// names are gone, so startup clears it rather than trying to resume.
func (cq *CallQuery) EndAll(ctx context.Context) error {
	return cq.Exec(ctx, endAllCallsQuery, time.Now().UnixMilli())
}

// DeleteEndedBefore prunes finished calls kept only for debugging.
func (cq *CallQuery) DeleteEndedBefore(ctx context.Context, cutoff time.Time) error {
	return cq.Exec(ctx, deleteEndedCallsQuery, cutoff.UnixMilli())
}
