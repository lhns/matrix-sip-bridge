package calls

import (
	"context"
	"errors"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/id"
)

const (
	testRoom = id.RoomID("!portal:example.com")
	testUser = id.UserID("@alice:example.com")
)

type fakeJoiner struct {
	err    error
	rooms  []id.RoomID
	called int
}

func (f *fakeJoiner) EnsureJoined(_ context.Context, roomID id.RoomID, _ ...bridgev2.EnsureJoinedParams) error {
	f.called++
	f.rooms = append(f.rooms, roomID)
	return f.err
}

type fakeInviter struct {
	err     error
	invited []id.UserID
	called  int
}

func (f *fakeInviter) EnsureInvited(_ context.Context, _ id.RoomID, userID id.UserID) error {
	f.called++
	f.invited = append(f.invited, userID)
	return f.err
}

// A portal room with a pending invite has to recover on its own: an RTC
// membership is only visible to a joined user, so an unaccepted invite means
// the call never rings.
func TestEnsureUserInRoomJoinsViaDoublePuppet(t *testing.T) {
	dp := &fakeJoiner{}
	bot := &fakeInviter{}
	joined, err := ensureUserInRoom(context.Background(), testRoom, testUser, dp, bot)
	if err != nil {
		t.Fatalf("ensureUserInRoom returned %v, want nil", err)
	}
	if !joined {
		t.Error("ensureUserInRoom reported the user as not joined")
	}
	if dp.called != 1 || dp.rooms[0] != testRoom {
		t.Errorf("double puppet joined %v (%d calls), want one join of %s", dp.rooms, dp.called, testRoom)
	}
	if bot.called != 0 {
		t.Errorf("bot invited the user %d times despite the join succeeding", bot.called)
	}
}

// Without double puppeting the bridge must still invite, and must say so: a
// silent fallback is what made the missing join invisible in the logs.
func TestEnsureUserInRoomFallsBackToInvite(t *testing.T) {
	joinFailed := errors.New("M_FORBIDDEN")
	tests := []struct {
		name string
		dp   roomJoiner
		want error
	}{
		{"no double puppet", nil, errNoDoublePuppet},
		{"double puppet fails", &fakeJoiner{err: joinFailed}, joinFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bot := &fakeInviter{}
			joined, err := ensureUserInRoom(context.Background(), testRoom, testUser, tt.dp, bot)
			if joined {
				t.Error("ensureUserInRoom reported the user as joined")
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("ensureUserInRoom returned %v, want %v", err, tt.want)
			}
			if bot.called != 1 || bot.invited[0] != testUser {
				t.Errorf("bot invited %v (%d calls), want one invite of %s", bot.invited, bot.called, testUser)
			}
		})
	}
}

func TestEnsureUserInRoomWithoutBot(t *testing.T) {
	joined, err := ensureUserInRoom(context.Background(), testRoom, testUser, nil, nil)
	if joined {
		t.Error("ensureUserInRoom reported the user as joined")
	}
	if !errors.Is(err, errNoDoublePuppet) {
		t.Errorf("ensureUserInRoom returned %v, want %v", err, errNoDoublePuppet)
	}
}

func TestEnsureUserInRoomReportsBothFailures(t *testing.T) {
	joinFailed := errors.New("join failed")
	inviteFailed := errors.New("invite failed")
	_, err := ensureUserInRoom(context.Background(), testRoom, testUser,
		&fakeJoiner{err: joinFailed}, &fakeInviter{err: inviteFailed})
	if !errors.Is(err, joinFailed) || !errors.Is(err, inviteFailed) {
		t.Errorf("ensureUserInRoom returned %v, want both %v and %v", err, joinFailed, inviteFailed)
	}
}
