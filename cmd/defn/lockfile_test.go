package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestEmbeddedLockStatus_DSN(t *testing.T) {
	// DSN-backed paths never check the lockfile — a running dolt
	// sql-server owns concurrency there, not the embedded lockfile.
	msg, held := embeddedLockStatus("root@tcp(127.0.0.1:3307)/defn")
	if held || msg != "" {
		t.Fatalf("DSN path should never be reported as held; got msg=%q held=%v", msg, held)
	}
}

func TestEmbeddedLockStatus_NoLockfile(t *testing.T) {
	// Empty directory — no lockfile — no lock.
	dir := t.TempDir()
	msg, held := embeddedLockStatus(dir)
	if held || msg != "" {
		t.Fatalf("missing lockfile should not be reported as held; got msg=%q held=%v", msg, held)
	}
}

func TestEmbeddedLockStatus_StaleLockfileReaped(t *testing.T) {
	// A lockfile with no flock holder is stale. readServeLock should
	// acquire LOCK_SH and, finding it free, reap the file.
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "serve.pid")
	if err := os.WriteFile(lockPath,
		[]byte(`{"pid": 99999, "http_addr": "127.0.0.1:9500", "started": 1}`),
		0644); err != nil {
		t.Fatal(err)
	}

	msg, held := embeddedLockStatus(dir)
	if held {
		t.Fatalf("stale lockfile should not be reported as held: %s", msg)
	}
	// And it should be reaped.
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lockfile was not reaped: %v", err)
	}
}

func TestEmbeddedLockStatus_LiveFlockReportedHeld(t *testing.T) {
	// Simulate a live serve by opening the lockfile and holding an
	// exclusive flock from this test process. embeddedLockStatus should
	// see the flock (via LOCK_SH|LOCK_NB failing), read metadata, and
	// return the helpful message.
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "serve.pid")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("could not acquire test flock: %v", err)
	}
	if _, err := f.Write([]byte(`{"pid": 12345, "http_addr": "127.0.0.1:9500", "started": 1}`)); err != nil {
		t.Fatal(err)
	}

	msg, held := embeddedLockStatus(dir)
	if !held {
		t.Fatalf("live flock should be reported as held; got msg=%q held=%v", msg, held)
	}
	if !strings.Contains(msg, "pid 12345") {
		t.Fatalf("message should surface the pid:\n%s", msg)
	}
	if !strings.Contains(msg, "127.0.0.1:9500") {
		t.Fatalf("message should surface the MCP endpoint:\n%s", msg)
	}
	if !strings.Contains(msg, "defn worktree") {
		t.Fatalf("message should mention the worktree escape hatch:\n%s", msg)
	}

	// After the test-held flock is released (via defer f.Close), a
	// subsequent call should reap and report free.
	f.Close()
	if msg, held := embeddedLockStatus(dir); held {
		t.Fatalf("after release, lock should not be held: %s", msg)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lockfile not reaped after release: %v", err)
	}
}

func TestEmbeddedLockStatus_RacingOpenPreservesWinnerContent(t *testing.T) {
	// Simulate the write race: a live serve holds the flock with real
	// content, and a losing serve attempts to open the same path. If
	// the open path uses O_TRUNC before flocking, the loser empties
	// the winner's content on its way to failing the flock — later
	// readers would then see empty JSON and misclassify the file as
	// free. Open-without-O_TRUNC must leave the winner's content
	// intact.
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "serve.pid")

	// Winner: write data + hold exclusive flock.
	winner, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer winner.Close()
	if err := syscall.Flock(int(winner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("winner flock: %v", err)
	}
	winnerContent := `{"pid": 42, "http_addr": "127.0.0.1:9600", "started": 1}`
	if _, err := winner.Write([]byte(winnerContent)); err != nil {
		t.Fatal(err)
	}

	// Loser: mimic the writeServeLock open path (no O_TRUNC).
	loser, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(loser.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		loser.Close()
		t.Fatal("loser unexpectedly acquired flock")
	}
	loser.Close()

	// After the loser bailed, the winner's content must still be intact.
	msg, held := embeddedLockStatus(dir)
	if !held {
		t.Fatalf("after losing open, winner should still be reported as held; got msg=%q held=%v", msg, held)
	}
	if !strings.Contains(msg, "pid 42") {
		t.Fatalf("winner's pid missing — content may have been clobbered:\n%s", msg)
	}
}

func TestEmbeddedLockStatus_MalformedLockfileTreatedAsFree(t *testing.T) {
	// A lockfile with garbage JSON should be treated as free — we can
	// safely discard a file nobody's holding a flock on anyway.
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "serve.pid")
	if err := os.WriteFile(lockPath, []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}
	msg, held := embeddedLockStatus(dir)
	if held {
		t.Fatalf("malformed lockfile shouldn't be reported as held: %s", msg)
	}
}
