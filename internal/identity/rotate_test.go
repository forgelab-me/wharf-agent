package identity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotationPrepareThenCommit(t *testing.T) {
	dir := t.TempDir()
	cur, err := LoadOrGenerate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if Age(cur) < time.Minute {
		t.Fatalf("a fresh cert is backdated an hour, Age = %v", Age(cur))
	}

	next, err := PrepareNext(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := PrepareNext(dir)
	if err != nil || Fingerprint(again.Certificate[0]) != Fingerprint(next.Certificate[0]) {
		t.Fatalf("PrepareNext must reuse the prepared pair (retryable rotation): %v", err)
	}
	reloaded, _ := LoadOrGenerate(dir)
	if Fingerprint(reloaded.Certificate[0]) != Fingerprint(cur.Certificate[0]) {
		t.Fatal("preparing must not touch the current identity")
	}

	committed, err := CommitNext(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, _ = LoadOrGenerate(dir)
	if Fingerprint(reloaded.Certificate[0]) != Fingerprint(committed.Certificate[0]) ||
		Fingerprint(committed.Certificate[0]) != Fingerprint(next.Certificate[0]) {
		t.Fatal("after commit the new certificate must be the persisted identity")
	}
}

func TestLoadFinishesRotationInterruptedBetweenRenames(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrGenerate(dir); err != nil {
		t.Fatal(err)
	}
	next, err := PrepareNext(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the key rename but before the cert rename.
	if err := os.Rename(filepath.Join(dir, nextKeyFileName), filepath.Join(dir, keyFileName)); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOrGenerate(dir)
	if err != nil {
		t.Fatalf("must recover: %v", err)
	}
	if Fingerprint(got.Certificate[0]) != Fingerprint(next.Certificate[0]) {
		t.Fatal("recovery must land on the prepared identity")
	}
}
