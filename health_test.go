package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestCheckHealth(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }

	for _, tc := range []struct {
		name    string
		s       healthState
		healthy bool
	}{
		{"never reached the controller", healthState{}, false},
		{"controller answered a moment ago, host still pending", healthState{Controller: ago(5 * time.Second)}, true},
		{"controller silent for two minutes", healthState{Controller: ago(2 * time.Minute)}, false},
		{"connected, state pushed recently", healthState{Controller: ago(5 * time.Second), Connected: ago(time.Hour), StatePushed: ago(40 * time.Second)}, true},
		{"connected, state pushed three cycles ago", healthState{Controller: ago(5 * time.Second), Connected: ago(time.Hour), StatePushed: ago(2 * time.Minute)}, true},
		{"connected, no state for long", healthState{Controller: ago(5 * time.Second), Connected: ago(time.Hour), StatePushed: ago(10 * time.Minute)}, false},
		{"just connected, first push on its way", healthState{Controller: ago(5 * time.Second), Connected: ago(30 * time.Second)}, true},
		{"connected for a while and never pushed", healthState{Controller: ago(5 * time.Second), Connected: ago(10 * time.Minute)}, false},
	} {
		if err := checkHealth(tc.s, now); (err == nil) != tc.healthy {
			t.Errorf("%s: err = %v, healthy should be %v", tc.name, err, tc.healthy)
		}
	}
}

func TestMarkingWritesWhatTheCheckReads(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	healthMu.Lock()
	healthCur = healthState{}
	healthMu.Unlock()

	if got := runHealthcheck(); got != 1 {
		t.Errorf("no file yet, the agent has not reached the controller: %d", got)
	}
	markControllerSeen(false)
	if got := runHealthcheck(); got != 0 {
		t.Errorf("answered by the controller, waiting for approval, is healthy: %d", got)
	}
	markControllerSeen(true)
	if got := runHealthcheck(); got != 0 {
		t.Errorf("just connected: %d", got)
	}
	markStatePushed()
	data, err := os.ReadFile(healthPath())
	if err != nil {
		t.Fatal(err)
	}
	var s healthState
	if err := json.Unmarshal(data, &s); err != nil || s.Controller == 0 || s.Connected == 0 || s.StatePushed == 0 {
		t.Errorf("file = %q (%v)", data, err)
	}

	// back to pending: whatever was pushed no longer counts
	markControllerSeen(false)
	data, _ = os.ReadFile(healthPath())
	_ = json.Unmarshal(data, &s)
	if s.Connected != 0 || s.StatePushed != 0 {
		t.Errorf("a host that is no longer connected expects no state: %+v", s)
	}
}

func TestAnUnreadableHealthFileIsUnhealthy(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if err := os.WriteFile(healthPath(), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runHealthcheck(); got != 1 {
		t.Errorf("got %d", got)
	}
}
