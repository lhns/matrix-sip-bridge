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
	rows, err := db.Inbound.Heads(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].UID != "a" || rows[1].UID != "d" {
		t.Fatalf("heads = %+v, want a (portal 1) then d (portal 2)", rows)
	}
	if !rows[0].ReceivedAt.Equal(time.UnixMilli(1_700_000_000_000)) || rows[0].Body != "hi" {
		t.Errorf("row did not round-trip: %+v", rows[0])
	}
	if err := db.Inbound.Delete(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if n, err := db.Inbound.Count(ctx); err != nil || n != 3 {
		t.Fatalf("count = %d, %v; want 3", n, err)
	}
}
