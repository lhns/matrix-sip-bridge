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
	// DirectionInbound is a call from the phone network, offered to the bridge
	// as an INVITE naming the conference it will land in.
	DirectionInbound CallDirection = "inbound"
	// DirectionOutbound is a call the bridge asked the SIP server to place.
	DirectionOutbound CallDirection = "outbound"
)

// CallState is where a call is in its life cycle.
type CallState string

const (
	// StateRinging means the call exists but has not been answered in Matrix.
	StateRinging CallState = "ringing"
	// StateBridged means the call is up: answered, with a LiveKit SIP
	// participant carrying the audio. Only a bridged call is watched for the
	// participant leaving.
	StateBridged CallState = "bridged"
	// StateEnded is terminal.
	StateEnded CallState = "ended"
)

// Call is one telephone call being bridged.
type Call struct {
	// CallID is the bridge's own identifier, not the SIP server's and not
	// LiveKit's.
	CallID string
	// PortalID is the E.164 number without the leading plus, matching the
	// bridgev2 portal and ghost IDs.
	PortalID string
	RoomID   id.RoomID

	Direction CallDirection
	// Conference is the name of the conference holding the call. It is what
	// the bridge passes to livekit-sip as sip_call_to.
	Conference string

	// LKRoom is the LiveKit room name derived from RoomID; see
	// calls.LiveKitRoomName.
	LKRoom string
	// LKIdentity is the participant identity given to livekit-sip, derived so
	// that Matrix clients can attribute the stream to the ghost.
	LKIdentity string
	// LKParticipant is the participant ID LiveKit assigned, empty until the
	// SIP participant has been created.
	LKParticipant string

	State CallState
	// CreatedAt is when the row was written, which for both directions is
	// within a few hundred milliseconds of the phone starting to ring.
	CreatedAt time.Time
	// UpdatedAt is the last write to the row, and there is no column that
	// means "answered at". For a bridged call the last write before the
	// timeline record is read is the ringing -> bridged transition, so it is
	// the answer to within one Update, and that is what the call duration is
	// measured from. For a ringing call it means nothing in particular, which
	// is why staleness measures that one from CreatedAt instead.
	UpdatedAt time.Time
}

// CallQuery runs the queries over the sip_call table.
type CallQuery struct {
	*dbutil.QueryHelper[*Call]
}

func newCall(*dbutil.QueryHelper[*Call]) *Call {
	return &Call{}
}

const (
	callColumns = `call_id, portal_id, room_id, direction, conference,
	               lk_room, lk_identity, lk_participant, state, created_at, updated_at`

	insertCallQuery = `
		INSERT INTO sip_call (` + callColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`
	// The state guard is what stops a late writer from resurrecting a call
	// that has already been torn down. Every terminal path goes through
	// End, and nothing may write a row back out of that state.
	updateCallQuery = `
		UPDATE sip_call
		SET conference = $2, lk_room = $3, lk_identity = $4,
		    lk_participant = $5, state = $6, updated_at = $7
		WHERE call_id = $1 AND state <> 'ended'
	`
	// transitionCallQuery and endCallQuery are the only state changes. Both
	// are compare-and-swap, so exactly one caller owns each transition of a
	// call even when a leave event, a ring timeout and a watcher tick all
	// arrive at once.
	transitionCallQuery = `
		UPDATE sip_call SET state = $3, updated_at = $4
		WHERE call_id = $1 AND state = $2
	`
	endCallQuery = `
		UPDATE sip_call SET state = 'ended', updated_at = $2
		WHERE call_id = $1 AND state <> 'ended'
	`
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
	endAllCallsQuery = `UPDATE sip_call SET state = 'ended', updated_at = $1 WHERE state <> 'ended'`
)

// Scan reads one row.
func (c *Call) Scan(row dbutil.Scannable) (*Call, error) {
	var createdAt, updatedAt int64
	err := row.Scan(
		&c.CallID, &c.PortalID, &c.RoomID, &c.Direction, &c.Conference,
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
		c.CallID, c.PortalID, c.RoomID, c.Direction, c.Conference,
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
		c.CallID, c.Conference, c.LKRoom, c.LKIdentity,
		c.LKParticipant, c.State, c.UpdatedAt.UnixMilli())
}

// Transition moves a call between two states and reports whether this caller
// is the one that did it. A false return is not an error: it means another
// path got there first, and this one must not act.
func (cq *CallQuery) Transition(ctx context.Context, c *Call, from, to CallState) (bool, error) {
	return cq.swap(ctx, c, to, transitionCallQuery, c.CallID, from, to)
}

// End marks a call ended from whatever state it is in, and reports whether
// this caller is the one that ended it. Teardown runs only for the winner.
func (cq *CallQuery) End(ctx context.Context, c *Call) (bool, error) {
	return cq.swap(ctx, c, StateEnded, endCallQuery, c.CallID)
}

func (cq *CallQuery) swap(ctx context.Context, c *Call, to CallState, query string, args ...any) (bool, error) {
	now := time.Now()
	res, err := cq.GetDB().Exec(ctx, query, append(args, now.UnixMilli())...)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		return false, nil
	}
	c.State = to
	c.UpdatedAt = now
	return true, nil
}

// GetActiveByPortal returns the call in progress for a number, or nil.
func (cq *CallQuery) GetActiveByPortal(ctx context.Context, portalID string) (*Call, error) {
	return cq.QueryOne(ctx, getActiveCallByPortalQuery, portalID)
}

// GetActiveByConference resolves a conference name back to a call, or nil.
func (cq *CallQuery) GetActiveByConference(ctx context.Context, conference string) (*Call, error) {
	return cq.QueryOne(ctx, getActiveCallByConferenceQuery, conference)
}

// GetAllActive returns every call not yet ended.
func (cq *CallQuery) GetAllActive(ctx context.Context) ([]*Call, error) {
	return cq.QueryMany(ctx, getAllActiveCallsQuery)
}

// EndAll marks every call ended.
//
// Call state lives in LiveKit and the SIP server, not here; this table is only
// a cache of it. After a bridge restart the cache is stale and every leg it
// names is gone, so startup clears it rather than trying to resume.
func (cq *CallQuery) EndAll(ctx context.Context) error {
	return cq.Exec(ctx, endAllCallsQuery, time.Now().UnixMilli())
}
