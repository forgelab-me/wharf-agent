// Health of the agent, for the image's HEALTHCHECK. The agent listens on no
// port, so its health is a small file of timestamps that the running process
// keeps up to date and `wharf-agent healthcheck` reads.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// controllerFreshness: the status check runs every 10 s (cf. pollLoop).
	controllerFreshness = 60 * time.Second
	// stateFreshness: the host state is pushed at least every stateResyncInterval,
	// three cycles of slack before it counts as stuck.
	stateFreshness = 3 * stateResyncInterval
	// connectGrace: after the host first shows as connected, the first state
	// push has this long to happen before its absence counts.
	connectGrace = 150 * time.Second
)

// healthState is what the file holds, as Unix seconds; 0 means never.
type healthState struct {
	Controller  int64 `json:"controller"`   // last status check the controller answered
	Connected   int64 `json:"connected"`    // when this host first showed as connected (0 while pending or rejected)
	StatePushed int64 `json:"state_pushed"` // last host state the tunnel delivered
}

var (
	healthMu  sync.Mutex
	healthCur healthState
)

// healthPath is where the process writes and the check reads, in the same container.
func healthPath() string { return filepath.Join(os.TempDir(), "wharf-agent.health") }

func writeHealth(path string, s healthState) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return // a read-only filesystem only makes the check report unhealthy
	}
	_ = os.Rename(tmp, path)
}

// markControllerSeen records a status check the controller answered, and
// whether the host is connected (approved), which is when state pushes are expected.
func markControllerSeen(connected bool) {
	healthMu.Lock()
	defer healthMu.Unlock()
	now := time.Now().Unix()
	healthCur.Controller = now
	switch {
	case connected && healthCur.Connected == 0:
		healthCur.Connected = now
	case !connected:
		// pending, or rejected: no state is expected, whatever was pushed before
		healthCur.Connected, healthCur.StatePushed = 0, 0
	}
	writeHealth(healthPath(), healthCur)
}

// markStatePushed records a host state the controller received.
func markStatePushed() {
	healthMu.Lock()
	defer healthMu.Unlock()
	healthCur.StatePushed = time.Now().Unix()
	writeHealth(healthPath(), healthCur)
}

// checkHealth says why the agent is unhealthy, or returns nil. An agent that
// is waiting for approval is healthy: the controller answers, and waiting is
// its normal state.
func checkHealth(s healthState, now time.Time) error {
	age := func(t int64) time.Duration { return now.Sub(time.Unix(t, 0)) }
	if s.Controller == 0 || age(s.Controller) > controllerFreshness {
		return errors.New("the controller has not answered for over a minute")
	}
	if s.Connected == 0 {
		return nil
	}
	if s.StatePushed != 0 && age(s.StatePushed) <= stateFreshness {
		return nil
	}
	if s.StatePushed == 0 && age(s.Connected) <= connectGrace {
		return nil // just connected, the first push is on its way
	}
	return errors.New("no host state has reached the controller recently (Docker or the tunnel)")
}

// runHealthcheck is `wharf-agent healthcheck`: exit 0 when healthy, 1 otherwise.
func runHealthcheck() int {
	data, err := os.ReadFile(healthPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy: no health file, the agent has not reached the controller yet")
		return 1
	}
	var s healthState
	if err := json.Unmarshal(data, &s); err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy: unreadable health file")
		return 1
	}
	if err := checkHealth(s, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	return 0
}
