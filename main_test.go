package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func testLogger() *zerolog.Logger {
	l := zerolog.New(io.Discard)
	return &l
}

// failingPinger fails the first n calls and then succeeds, recording how often
// it was asked.
func failingPinger(n int, calls *int) func(context.Context) error {
	return func(context.Context) error {
		*calls++
		if *calls <= n {
			return errors.New("connection refused")
		}
		return nil
	}
}

func TestWaitForDBRetriesUntilTheDatabaseAnswers(t *testing.T) {
	var calls int
	err := waitForDB(context.Background(), failingPinger(3, &calls), time.Millisecond, 2*time.Millisecond, time.Second, testLogger())
	if err != nil {
		t.Fatalf("waitForDB: %v", err)
	}
	if calls != 4 {
		t.Errorf("pinged %d times, want 4", calls)
	}
}

func TestWaitForDBSucceedsWithoutSleepingOnTheFirstTry(t *testing.T) {
	var calls int
	start := time.Now()
	err := waitForDB(context.Background(), failingPinger(0, &calls), time.Second, time.Second, time.Minute, testLogger())
	if err != nil {
		t.Fatalf("waitForDB: %v", err)
	}
	if calls != 1 {
		t.Errorf("pinged %d times, want 1", calls)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("slept %s before the first ping", elapsed)
	}
}

func TestWaitForDBGivesUpAtTheDeadline(t *testing.T) {
	var calls int
	refused := errors.New("connection refused")
	down := func(context.Context) error {
		calls++
		return refused
	}
	err := waitForDB(context.Background(), down, time.Millisecond, 2*time.Millisecond, 20*time.Millisecond, testLogger())
	if err == nil {
		t.Fatal("waitForDB returned nil for a database that never answered")
	}
	// The last ping's error has to survive, or the log says only "deadline".
	if !errors.Is(err, refused) {
		t.Errorf("error %v does not wrap the ping error", err)
	}
	if calls < 2 {
		t.Errorf("pinged %d times, want at least 2", calls)
	}
}

func TestWaitForDBStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls int
	err := waitForDB(ctx, failingPinger(1, &calls), time.Hour, time.Hour, time.Hour, testLogger())
	if err == nil {
		t.Fatal("waitForDB returned nil for a cancelled context")
	}
}

// The backoff has to grow, or a slow database is hammered for the whole
// deadline: 250ms flat is 240 dials a minute.
func TestWaitForDBBacksOff(t *testing.T) {
	var calls int
	start := time.Now()
	err := waitForDB(context.Background(), failingPinger(3, &calls), 20*time.Millisecond, time.Second, 5*time.Second, testLogger())
	if err != nil {
		t.Fatalf("waitForDB: %v", err)
	}
	// 20 + 40 + 80 = 140ms of sleeping; a flat 20ms backoff would be 60ms.
	if elapsed := time.Since(start); elapsed < 130*time.Millisecond {
		t.Errorf("three retries took %s, too fast for a doubling backoff", elapsed)
	}
}

func TestWaitForDBCapsTheDelay(t *testing.T) {
	var calls int
	start := time.Now()
	err := waitForDB(context.Background(), failingPinger(4, &calls), 20*time.Millisecond, 30*time.Millisecond, 5*time.Second, testLogger())
	if err != nil {
		t.Fatalf("waitForDB: %v", err)
	}
	// 20 + 30 + 30 + 30 = 110ms; uncapped doubling would be 20+40+80+160 = 300ms.
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("four retries took %s, the delay is not capped", elapsed)
	}
}
