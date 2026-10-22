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

func TestInboundOutboxKeepsOrderIgnoresDuplicateUIDsAndReturnsPortalHeads(t *testing.T) {
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
	if ok, err := db.Inbound.Insert(ctx, inboundMsg("d", "home-+15557654321")); err != nil || !ok {
		t.Fatalf("insert d = %v, %v", ok, err)
	}
	// A text received earlier but stored later (drained from the spool) is the
	// head of its portal.
	early := inboundMsg("e", "home-+15557654321")
	early.ReceivedAt = early.ReceivedAt.Add(-time.Hour)
	if ok, err := db.Inbound.Insert(ctx, early); err != nil || !ok {
		t.Fatalf("insert e = %v, %v", ok, err)
	}
	rows, err := db.Inbound.Heads(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].UID != "e" || rows[1].UID != "a" {
		t.Fatalf("heads = %+v, want e (portal 2, received first) then a (portal 1)", rows)
	}
	if !rows[1].ReceivedAt.Equal(time.UnixMilli(1_700_000_000_000)) || rows[1].Body != "hi" {
		t.Errorf("row did not round-trip: %+v", rows[1])
	}
	if err := db.Inbound.Delete(ctx, rows[1].ID); err != nil {
		t.Fatal(err)
	}
	if n, err := db.Inbound.Count(ctx); err != nil || n != 4 {
		t.Fatalf("count = %d, %v; want 4", n, err)
	}
}
