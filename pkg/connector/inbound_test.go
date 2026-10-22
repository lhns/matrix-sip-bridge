package connector

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	sipdb "github.com/lhns/matrix-sip-bridge/pkg/database"
	"github.com/lhns/matrix-sip-bridge/pkg/inbound"
	"github.com/lhns/matrix-sip-bridge/pkg/metrics"
	"github.com/lhns/matrix-sip-bridge/pkg/siptransport"
)

// downStore is an outbox whose database is unavailable.
type downStore struct{ inbound.Store }

func (downStore) Insert(context.Context, *sipdb.InboundMessage) (bool, error) {
	return false, errors.New("database is down")
}

func inboundSMS() siptransport.InboundMessage {
	return siptransport.InboundMessage{From: "sip:+15551234567@home", Body: "hi", Recipient: "@alice:example.com"}
}

// The 200 only means something once the text is stored. With the database up
// it is in the table and nothing has reached bridgev2 yet.
func TestInboundMessageIsStoredBeforeItIsAnswered(t *testing.T) {
	_, sc := newTestBridge(t, ownerPermissions())
	reg := prometheus.NewRegistry()
	sc.metrics = metrics.New(reg)
	called := false
	sc.queueEvent = func(*bridgev2.UserLogin, bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		called = true
		return bridgev2.EventHandlingResultSuccess
	}
	if err := sc.handleInboundMessage(context.Background(), inboundSMS()); err != nil {
		t.Fatal(err)
	}
	rows, err := sc.db.Inbound.Pending(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].Body != "hi" || rows[0].PortalID != "home-+15551234567" || rows[0].Sender != "+15551234567" {
		t.Fatalf("outbox = %+v, %v; want the one text", rows, err)
	}
	if called {
		t.Error("the event reached bridgev2 before the worker ran")
	}
	if got := counter(t, reg, "sip_bridge_messages_total", metrics.DirectionInbound, metrics.MessageBridged); got != 1 {
		t.Errorf("messages_total{inbound,bridged} = %v, want 1", got)
	}
}

func TestInboundMessageIsSpooledWhenTheDatabaseIsDown(t *testing.T) {
	_, sc := newTestBridge(t, ownerPermissions())
	reg := prometheus.NewRegistry()
	sc.metrics = metrics.New(reg)
	dir := t.TempDir()
	var err error
	if sc.inbound, err = inbound.New(downStore{}, dir, zerolog.Nop(), sc.metrics); err != nil {
		t.Fatal(err)
	}
	if err := sc.handleInboundMessage(context.Background(), inboundSMS()); err != nil {
		t.Fatalf("handleInboundMessage = %v, want the text accepted into the spool", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("spool has %d files, want 1", len(entries))
	}
	if got := counter(t, reg, "sip_bridge_messages_total", metrics.DirectionInbound, metrics.MessageSpooled); got != 1 {
		t.Errorf("messages_total{inbound,spooled} = %v, want 1", got)
	}
}

func TestInboundMessageIsRefusedWhenNothingCanStoreIt(t *testing.T) {
	_, sc := newTestBridge(t, ownerPermissions())
	reg := prometheus.NewRegistry()
	sc.metrics = metrics.New(reg)
	var err error
	if sc.inbound, err = inbound.New(downStore{}, "", zerolog.Nop(), sc.metrics); err != nil {
		t.Fatal(err)
	}
	err = sc.handleInboundMessage(context.Background(), inboundSMS())
	if err == nil {
		t.Fatal("handleInboundMessage = nil, want an error so the SIP server sees a 500")
	}
	var reject *siptransport.RejectError
	if errors.As(err, &reject) {
		t.Errorf("error is a %d rejection; a storage failure must stay a plain error (500)", reject.Code)
	}
	if got := counter(t, reg, "sip_bridge_messages_total", metrics.DirectionInbound, metrics.MessagePersistFailed); got != 1 {
		t.Errorf("messages_total{inbound,persist_failed} = %v, want 1", got)
	}
}

// The worker keeps a row until bridgev2 reports success, and every retry
// carries the same message ID so bridgev2's duplicate check can absorb it.
func TestDeliveryDeletesOnSuccessAndKeepsAStableMessageID(t *testing.T) {
	_, sc := newTestBridge(t, ownerPermissions())
	sc.metrics = metrics.New(prometheus.NewRegistry())
	if err := sc.handleInboundMessage(context.Background(), inboundSMS()); err != nil {
		t.Fatal(err)
	}
	var ids []networkid.MessageID
	var results = []bridgev2.EventHandlingResult{
		bridgev2.EventHandlingResultFailed.WithError(errors.New("homeserver down")),
		bridgev2.EventHandlingResultFailed,
		bridgev2.EventHandlingResultQueued,
		bridgev2.EventHandlingResultSuccess,
	}
	sc.queueEvent = func(_ *bridgev2.UserLogin, evt bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		ids = append(ids, evt.(*simplevent.Message[*inboundMessage]).ID)
		r := results[0]
		results = results[1:]
		return r
	}
	pending := func() int {
		n, err := sc.db.Inbound.Count(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// A near-zero backoff stands in for waiting between attempts.
	sc.inbound.SetBackoff(time.Nanosecond, time.Nanosecond)
	for i := range 4 {
		sc.inbound.DeliverPass(context.Background(), sc.deliverInbound)
		want := 1
		if i == 3 {
			want = 0
		}
		if got := pending(); got != want {
			t.Fatalf("after attempt %d: %d rows, want %d", i+1, got, want)
		}
	}
	if len(ids) != 4 || ids[0] != ids[1] || ids[1] != ids[2] || ids[2] != ids[3] || ids[0] == "" {
		t.Errorf("message IDs across retries = %v, want one stable ID", ids)
	}
}
