package database

import (
	"context"
	"testing"
	"time"
)

func inboundMsg(uid, portal string) *InboundMessage {
	return &InboundMessage{
		UID: uid, LoginID: "@alice:example.com", PortalID: portal, Sender: "+15551234567",
		Body: "hi", ReceivedAt: time.UnixMilli(1_700_000_000_000),
	}
}

func TestInboundOutboxKeepsOrderAndIgnoresDuplicateUIDs(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	for _, uid := range []string{"a", "b", "c"} {
		if ok, err := db.Inbound.Insert(ctx, inboundMsg(uid, "home-+15551234567")); err != nil || !ok {
			t.Fatalf("insert %s = %v, %v; want a new row", uid, ok, err)
		}
	}
	if ok, err := db.Inbound.Insert(ctx, inboundMsg("b", "home-+15551234567")); err != nil || ok {
		t.Fatalf("second insert of b = %v, %v; want no new row and no error", ok, err)
	}
	rows, err := db.Inbound.Pending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].UID != "a" || rows[1].UID != "b" || rows[2].UID != "c" {
		t.Fatalf("pending = %+v, want a b c in order", rows)
	}
	if !rows[0].ReceivedAt.Equal(time.UnixMilli(1_700_000_000_000)) || rows[0].Body != "hi" {
		t.Errorf("row did not round-trip: %+v", rows[0])
	}
	if err := db.Inbound.Delete(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if n, err := db.Inbound.Count(ctx); err != nil || n != 2 {
		t.Fatalf("count = %d, %v; want 2", n, err)
	}
}
