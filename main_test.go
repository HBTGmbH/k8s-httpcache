package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestBackendChanNil(t *testing.T) {
	ch := backendChan(nil)
	if ch != nil {
		t.Fatal("expected nil channel for nil input")
	}
}

func TestBackendChanNonNil(t *testing.T) {
	input := make(chan backendChange, 1)
	ch := backendChan(input)
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}

	// Verify it's the same channel by sending and receiving.
	input <- backendChange{name: "test"}
	select {
	case bc := <-ch:
		if bc.name != "test" {
			t.Fatalf("got name %q, want test", bc.name)
		}
	default:
		t.Fatal("expected to receive from channel")
	}
}

func TestTimerChanNil(t *testing.T) {
	ch := timerChan(nil)
	if ch != nil {
		t.Fatal("expected nil channel for nil timer")
	}
}

func TestTimerChanNonNil(t *testing.T) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	ch := timerChan(timer)
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}
	if ch != timer.C {
		t.Fatal("expected same channel as timer.C")
	}
}

func TestWatchFileDetectsChange(t *testing.T) {
	f, err := os.CreateTemp("", "watchfile-test-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()

	if _, err := f.Write([]byte("initial")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := watchFile(ctx, path, 50*time.Millisecond)

	// Wait a tick so the watcher reads the initial content.
	time.Sleep(100 * time.Millisecond)

	// Modify the file.
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ch:
		// OK — change detected
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for file change notification")
	}
}

func TestWatchFileNoChangeNoNotification(t *testing.T) {
	f, err := os.CreateTemp("", "watchfile-test-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()

	if _, err := f.Write([]byte("stable")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := watchFile(ctx, path, 50*time.Millisecond)

	// No modification — should not receive anything.
	select {
	case <-ch:
		t.Fatal("unexpected file change notification")
	case <-time.After(300 * time.Millisecond):
		// OK — no notification
	}
}

func TestWatchFileStopsOnContextCancel(t *testing.T) {
	f, err := os.CreateTemp("", "watchfile-test-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	_ = f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch := watchFile(ctx, path, 50*time.Millisecond)

	// Cancel the context immediately.
	cancel()

	// The goroutine should exit. We verify by writing a change and
	// confirming no notification arrives (goroutine has stopped polling).
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(path, []byte("changed-after-cancel"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ch:
		t.Fatal("unexpected notification after context cancel")
	case <-time.After(300 * time.Millisecond):
		// OK — goroutine stopped
	}
}
