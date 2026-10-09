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
	// PortalID is the bridgev2 portal and ghost ID, which is "<line>-<number>"
	// or bare digits for a portal predating lines -- not a dialable number.
	// See phonenum.NumberFromID.
	PortalID string
	RoomID   id.RoomID
	// Receiver is the Matrix user whose portal room the call is in: each user
	// has a room of their own per number, so one phone call can ring several
	// rooms at once. Empty for a row from before per-user portals.
	Receiver id.UserID
	// MatrixUser is the Matrix user on the call: inbound, whoever answered, so
	// empty until then; outbound, whoever dialled.
	MatrixUser id.UserID

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
	callColumns = `call_id, portal_id, room_id, receiver, matrix_user, direction, conference,
	               lk_room, lk_identity, lk_participant, state, created_at, updated_at`

	insertCallQuery = `
		INSERT INTO sip_call (` + callColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
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
	// answerCallQuery is the claim on a conference: the first leg to win it is
	// the one that gets answered. Several legs share a conference, so the
	// guard looks at the conference, not at this row.
	answerCallQuery = `
		UPDATE sip_call SET state = 'bridged', matrix_user = $3, updated_at = $4
		WHERE call_id = $1 AND state = 'ringing'
		  AND NOT EXISTS (SELECT 1 FROM sip_call o WHERE o.conference = $2 AND o.state = 'bridged')
	`
	// An "active" call is anything not yet ended. There is at most one per
	// room, so ordering by created_at only matters if state got out of sync.
	getActiveCallByRoomQuery = `
		SELECT ` + callColumns + `
		FROM sip_call WHERE room_id = $1 AND state <> 'ended'
		ORDER BY created_at DESC LIMIT 1
	`
	getActiveCallByConferenceQuery = `
		SELECT ` + callColumns + `
		FROM sip_call WHERE conference = $1 AND receiver = $2 AND state <> 'ended'
		ORDER BY created_at DESC LIMIT 1
	`
	answeredElsewhereQuery = `
		SELECT ` + callColumns + `
		FROM sip_call
		WHERE conference = $1 AND call_id <> $2 AND direction = 'inbound'
		  AND matrix_user <> '' AND state = 'bridged' AND created_at >= $3
		ORDER BY created_at DESC LIMIT 1
	`
	getCallByIDQuery = `
		SELECT ` + callColumns + ` FROM sip_call WHERE call_id = $1
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
		&c.CallID, &c.PortalID, &c.RoomID, &c.Receiver, &c.MatrixUser, &c.Direction, &c.Conference,
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
		c.CallID, c.PortalID, c.RoomID, c.Receiver, c.MatrixUser, c.Direction, c.Conference,
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

// Answer moves a ringing call to bridged for matrixUser, unless another leg of
// the same conference is already bridged, and reports whether this caller
// won. Like Transition, false is not an error. The claim and the transition
// are one statement so two legs cannot both win.
func (cq *CallQuery) Answer(ctx context.Context, c *Call, matrixUser id.UserID) (bool, error) {
	won, err := cq.swap(ctx, c, StateBridged, answerCallQuery, c.CallID, c.Conference, matrixUser)
	if won {
		c.MatrixUser = matrixUser
	}
	return won, err
}

// GetActiveByRoom returns the call in progress in a portal room, or nil.
func (cq *CallQuery) GetActiveByRoom(ctx context.Context, roomID id.RoomID) (*Call, error) {
	return cq.QueryOne(ctx, getActiveCallByRoomQuery, roomID)
}

// GetActiveByConference returns the call in progress in a conference for one
// receiver's room, or nil. Legs of one phone call share the conference and
// differ in the receiver.
func (cq *CallQuery) GetActiveByConference(ctx context.Context, conference string, receiver id.UserID) (*Call, error) {
	return cq.QueryOne(ctx, getActiveCallByConferenceQuery, conference, receiver)
}

// AnsweredElsewhere returns the other inbound leg of c's conference that a
// Matrix user answered, or nil. Only a bridged row counts: a winner whose
// media failed ends without a 200, so the SIP server never cancelled for it.
// within bounds how far before c an answered leg may have been created, so a
// previous call to the same number is not mistaken for this one's winner.
func (cq *CallQuery) AnsweredElsewhere(ctx context.Context, c *Call, within time.Duration) (*Call, error) {
	return cq.QueryOne(ctx, answeredElsewhereQuery, c.Conference, c.CallID, c.CreatedAt.Add(-within).UnixMilli())
}

// GetByCallID returns one call whatever its state, or nil. The state is the
// caller's to check: an event naming a call that has already ended is normal.
func (cq *CallQuery) GetByCallID(ctx context.Context, callID string) (*Call, error) {
	return cq.QueryOne(ctx, getCallByIDQuery, callID)
}

// GetAllActive returns every call not yet ended.
func (cq *CallQuery) GetAllActive(ctx context.Context) ([]*Call, error) {
	return cq.QueryMany(ctx, getAllActiveCallsQuery)
}

// EndAll marks every call ended and returns how many were open.
//
// Call state lives in LiveKit and the SIP server, not here; this table is only
// a cache of it. After a bridge restart the cache is stale and every leg it
// names is gone, so startup clears it rather than trying to resume.
func (cq *CallQuery) EndAll(ctx context.Context) (int64, error) {
	res, err := cq.GetDB().Exec(ctx, endAllCallsQuery, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
