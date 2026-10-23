package database

import (
	"context"
	"time"

	"go.mau.fi/util/dbutil"
)

// InboundMessage is a text accepted from SIP that has not reached Matrix yet.
// The json tags are the spool file format, so renaming one is a format change.
type InboundMessage struct {
	// ID breaks ties in delivery order. It is assigned by the table and is zero
	// until read back, so it is not part of the spool format.
	ID int64 `json:"-"`
	// UID is minted when the text is accepted. It is the idempotency key for
	// the spool-to-table move and the base of the bridgev2 message ID.
	UID string `json:"uid"`
	// LoginID is the bridgev2 login the portal belongs to.
	LoginID string `json:"login_id"`
	// PortalID is the bridgev2 portal ID.
	PortalID string `json:"portal_id"`
	// Sender and Recipient are E.164; Recipient may be empty.
	Sender    string `json:"sender"`
	Recipient string `json:"recipient,omitempty"`
	Body      string `json:"body"`
	Line      string `json:"line,omitempty"`
	// ReceivedAt becomes the Matrix timestamp.
	ReceivedAt time.Time `json:"received_at"`
}

// InboundQuery runs the queries over the sip_inbound_message table.
type InboundQuery struct {
	*dbutil.QueryHelper[*InboundMessage]
}

func newInboundMessage(*dbutil.QueryHelper[*InboundMessage]) *InboundMessage {
	return &InboundMessage{}
}

const (
	inboundColumns = `id, uid, login_id, portal_id, sender, recipient, body, line, received_at`

	// The conflict clause makes a retry, or a spool file whose removal was
	// lost to a crash, a no-op.
	insertInboundQuery = `
		INSERT INTO sip_inbound_message (uid, login_id, portal_id, sender, recipient, body, line, received_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (uid) DO NOTHING
	`
	// The earliest-received message of each portal: a portal with many stuck
	// messages cannot crowd the others out of the batch, and a text drained
	// from the spool (higher id, earlier receive time) is not left behind a
	// later one.
	headInboundQuery = `
		SELECT ` + inboundColumns + ` FROM sip_inbound_message m
		WHERE NOT EXISTS (
			SELECT 1 FROM sip_inbound_message o
			WHERE o.login_id = m.login_id AND o.portal_id = m.portal_id
			AND (o.received_at < m.received_at OR (o.received_at = m.received_at AND o.id < m.id))
		)
		ORDER BY received_at, id LIMIT $1
	`
	deleteInboundQuery = `DELETE FROM sip_inbound_message WHERE id = $1`
	countInboundQuery  = `SELECT COUNT(*) FROM sip_inbound_message`
)

// Scan reads one row.
func (m *InboundMessage) Scan(row dbutil.Scannable) (*InboundMessage, error) {
	var receivedAt int64
	err := row.Scan(&m.ID, &m.UID, &m.LoginID, &m.PortalID, &m.Sender, &m.Recipient, &m.Body, &m.Line, &receivedAt)
	if err != nil {
		return nil, err
	}
	m.ReceivedAt = time.UnixMilli(receivedAt)
	return m, nil
}

// Insert stores a message and reports whether it was new: false means a row
// with the same UID was already there.
func (q *InboundQuery) Insert(ctx context.Context, m *InboundMessage) (bool, error) {
	res, err := q.GetDB().Exec(ctx, insertInboundQuery,
		m.UID, m.LoginID, m.PortalID, m.Sender, m.Recipient, m.Body, m.Line, m.ReceivedAt.UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Heads returns the earliest-received waiting message of each portal, up to
// limit, earliest first. Delivering a head exposes the next message of its portal.
func (q *InboundQuery) Heads(ctx context.Context, limit int) ([]*InboundMessage, error) {
	return q.QueryMany(ctx, headInboundQuery, limit)
}

// Delete removes a delivered message.
func (q *InboundQuery) Delete(ctx context.Context, id int64) error {
	return q.Exec(ctx, deleteInboundQuery, id)
}

// Count is the number of messages waiting.
func (q *InboundQuery) Count(ctx context.Context) (int, error) {
	var n int
	err := q.GetDB().QueryRow(ctx, countInboundQuery).Scan(&n)
	return n, err
}
