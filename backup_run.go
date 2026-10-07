package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ---- the report a run sends back

type volumeResult struct {
	Volume          string `json:"volume"`
	Status          string `json:"status"` // success | warning | error
	SnapshotID      string `json:"snapshot_id,omitempty"`
	FilesNew        int    `json:"files_new,omitempty"`
	FilesChanged    int    `json:"files_changed,omitempty"`
	FilesUnmodified int    `json:"files_unmodified,omitempty"`
	DataAdded       int64  `json:"data_added,omitempty"`
	TotalBytes      int64  `json:"total_bytes,omitempty"`
	Message         string `json:"message,omitempty"`
	Retention       string `json:"retention,omitempty"`
}

type runReport struct {
	Kind     string         `json:"kind"`   // "backup" | "restore"
	Status   string         `json:"status"` // success | warning | error
	Code     string         `json:"code,omitempty"`
	Message  string         `json:"message,omitempty"`
	Volumes  []volumeResult `json:"volumes,omitempty"`
	Restored string         `json:"restored,omitempty"` // the new volume of a restore
}

// ---- what the executor needs from the outside, so it can be tested with fakes

type resticRunner interface {
	restic(ctx context.Context, mounts []string, quiet bool, args ...string) resticOut
}

type usingContainer struct{ ID, Name string }

type hostDocker interface {
	volumeExists(ctx context.Context, name string) bool
	// runningUsing lists the running containers that mount the volume, leaving
	// out the agent itself and the helpers.
	runningUsing(ctx context.Context, volume string) ([]usingContainer, error)
	stop(ctx context.Context, c usingContainer) error
	start(ctx context.Context, c usingContainer) error
	createVolume(ctx context.Context, name, label string) error
	removeVolume(ctx context.Context, name string) error
}

type backupExecutor struct {
	r resticRunner
	d hostDocker
}

// ---- backing up

// The result is named so that the deferred restart, which runs after the return value
// is set, can still turn a clean report into a warning.
func (e *backupExecutor) backup(ctx context.Context, req backupRequest) (rep runReport) {
	rep = runReport{Kind: "backup", Status: "success"}

	if o := e.r.restic(ctx, nil, false, "cat", "config"); o.Exit != 0 {
		rep.Status = "error"
		rep.Code, rep.Message = classify(o)
		return rep
	}

	// Containers stopped for the copy come back as soon as the copies are made,
	// before retention (which does not need them stopped); the deferred call covers
	// every early return.
	var stopped []usingContainer
	var restartFailures []string
	restart := func() {
		for i := len(stopped) - 1; i >= 0; i-- {
			if err := e.d.start(context.Background(), stopped[i]); err != nil {
				restartFailures = append(restartFailures, stopped[i].Name+": "+err.Error())
			}
		}
		stopped = nil
	}
	if req.Mode == "stop" {
		var err error
		stopped, err = e.stopUsers(ctx, req.Volumes)
		defer func() {
			restart()
			if len(restartFailures) > 0 && rep.Status != "error" {
				rep.Status = "warning"
				rep.Code = codeRestartFailed
				rep.Message = strings.TrimSpace(rep.Message + " The data is safe, but these containers could not be restarted: " + strings.Join(restartFailures, "; "))
			}
		}()
		if err != nil {
			rep.Status = "error"
			rep.Code = codeDocker
			rep.Message = err.Error()
			return rep
		}
	}

	for _, v := range req.Volumes {
		rep.Volumes = append(rep.Volumes, e.backupVolume(ctx, req, v))
	}
	restart()

	if flags := retentionArgs(req.Retention); len(flags) > 0 {
		for i := range rep.Volumes {
			if res := &rep.Volumes[i]; res.Status != "error" && res.SnapshotID != "" {
				e.retainVolume(ctx, req, res, flags)
			}
		}
	}

	failed, warned := 0, 0
	for _, res := range rep.Volumes {
		switch res.Status {
		case "error":
			failed++
		case "warning":
			warned++
		}
	}
	switch {
	case failed > 0:
		rep.Status = "error"
		rep.Message = fmt.Sprintf("%d of %d volumes could not be backed up", failed, len(req.Volumes))
	case warned > 0:
		rep.Status = "warning"
		rep.Message = fmt.Sprintf("%d of %d volumes were backed up with a warning", warned, len(req.Volumes))
	}
	return rep
}

func (e *backupExecutor) backupVolume(ctx context.Context, req backupRequest, v string) volumeResult {
	res := volumeResult{Volume: v, Status: "success"}
	if !e.d.volumeExists(ctx, v) {
		res.Status, res.Message = "error", "this volume does not exist on the host"
		return res
	}
	mount := v + ":/volumes/" + v + ":ro"
	o := e.r.restic(ctx, []string{mount}, true, "backup", "--json", "--retry-lock", "5m",
		"--host", req.HostTag, "--tag", "wharf", "--tag", volumeTag(v), "--exclude-caches", "/volumes/"+v)
	switch o.Exit {
	case 0:
	case 3:
		res.Status, res.Message = "warning", "some files could not be read; the snapshot is valid but incomplete"
	default:
		_, res.Message = classify(o)
		res.Status = "error"
		return res
	}
	sum, ok := parseSummary(o.Stdout)
	if !ok || !snapshotIDRe.MatchString(sum.SnapshotID) {
		res.Status, res.Message = "error", "restic finished but reported no snapshot"
		return res
	}
	res.SnapshotID = sum.SnapshotID
	res.FilesNew, res.FilesChanged, res.FilesUnmodified = sum.FilesNew, sum.FilesChanged, sum.FilesUnmodified
	res.DataAdded, res.TotalBytes = sum.DataAdded, sum.TotalBytesProcessed

	// The snapshot has to be readable before this counts as a success, and before
	// retention may remove an older one in its place.
	chk := e.r.restic(ctx, nil, false, "snapshots", "--json", sum.SnapshotID)
	snaps, err := parseSnapshots(chk.Stdout)
	if chk.Exit != 0 || err != nil || len(snaps) != 1 || !strings.HasPrefix(snaps[0].ID, sum.SnapshotID) {
		res.Status, res.Message = "error", "the snapshot was written but cannot be read back; nothing was removed"
		res.SnapshotID = ""
	}
	return res
}

// retainVolume applies retention to a volume whose snapshot was written and read back.
func (e *backupExecutor) retainVolume(ctx context.Context, req backupRequest, res *volumeResult, flags []string) {
	res.Retention, res.Message = e.applyRetention(ctx, req, res.Volume, flags, res.Message)
	if strings.HasPrefix(res.Retention, "skipped") || strings.HasPrefix(res.Retention, "failed") {
		if res.Status == "success" {
			res.Status = "warning"
		}
	}
}

// applyRetention removes old snapshots of one volume: a dry run first, and never
// a policy that would remove every snapshot.
func (e *backupExecutor) applyRetention(ctx context.Context, req backupRequest, v string, flags []string, message string) (summary, msg string) {
	base := append([]string{"forget", "--json", "--retry-lock", "5m", "--host", req.HostTag, "--tag", volumeTag(v), "--group-by", "host,tags"}, flags...)
	dry := e.r.restic(ctx, nil, false, append(base, "--dry-run")...)
	if dry.Exit != 0 {
		_, m := classify(dry)
		return "failed: " + m, join(message, "retention was not applied")
	}
	kept, _, err := parseForget(dry.Stdout)
	if err != nil || kept == 0 {
		return "skipped: the policy would remove every snapshot", join(message, "retention was not applied: the policy would remove every snapshot")
	}
	real := e.r.restic(ctx, nil, false, append(base, "--prune")...)
	if real.Exit != 0 {
		_, m := classify(real)
		return "failed: " + m, join(message, "retention was not applied")
	}
	kept, removed, err := parseForget(real.Stdout)
	if err != nil {
		return "applied", message
	}
	return fmt.Sprintf("kept %d, removed %d", kept, removed), message
}

func join(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// stopUsers stops every running container that mounts one of the volumes and
// returns them so the caller restarts them, whatever happens next.
func (e *backupExecutor) stopUsers(ctx context.Context, volumes []string) ([]usingContainer, error) {
	seen := map[string]bool{}
	var stopped []usingContainer
	for _, v := range volumes {
		users, err := e.d.runningUsing(ctx, v)
		if err != nil {
			return stopped, err
		}
		for _, c := range users {
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			if err := e.d.stop(ctx, c); err != nil {
				return stopped, fmt.Errorf("could not stop %s: %w", c.Name, err)
			}
			stopped = append(stopped, c)
		}
	}
	return stopped, nil
}

// ---- restoring into a new volume

func (e *backupExecutor) restore(ctx context.Context, req backupRequest) runReport {
	rep := runReport{Kind: "restore", Status: "error", Code: codeValidation}
	switch {
	case !snapshotIDRe.MatchString(req.Snapshot):
		rep.Message = "invalid snapshot id"
		return rep
	case !safeNameRe.MatchString(req.Volume):
		rep.Message = "invalid volume name"
		return rep
	case !safeNameRe.MatchString(req.NewVolume):
		rep.Message = "invalid name for the new volume"
		return rep
	case e.d.volumeExists(ctx, req.NewVolume):
		rep.Message = fmt.Sprintf("a volume named %q already exists: restore into a new name", req.NewVolume)
		return rep
	}

	// The snapshot must be of this volume, on this host: never restore whatever id was typed.
	chk := e.r.restic(ctx, nil, false, "snapshots", "--json", req.Snapshot)
	if chk.Exit != 0 {
		rep.Code, rep.Message = classify(chk)
		return rep
	}
	snaps, err := parseSnapshots(chk.Stdout)
	if err != nil || len(snaps) != 1 || snaps[0].Hostname != req.HostTag || !hasTag(snaps[0].Tags, volumeTag(req.Volume)) {
		rep.Message = "that snapshot is not a backup of this volume on this host"
		return rep
	}

	if err := e.d.createVolume(ctx, req.NewVolume, "wharf.restored-from="+snaps[0].ID); err != nil {
		rep.Code, rep.Message = codeDocker, err.Error()
		return rep
	}
	o := e.r.restic(ctx, []string{req.NewVolume + ":/restore"}, false,
		"restore", snaps[0].ID+":/volumes/"+req.Volume, "--target", "/restore", "--retry-lock", "5m")
	if o.Exit != 0 {
		_ = e.d.removeVolume(context.Background(), req.NewVolume)
		rep.Code, rep.Message = classify(o)
		return rep
	}
	rep.Status, rep.Code = "success", ""
	rep.Restored = req.NewVolume
	rep.Message = fmt.Sprintf("snapshot %s restored into the new volume %s", snaps[0].ID[:8], req.NewVolume)
	return rep
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// ---- the real docker, behind hostDocker

type cliDocker struct{ selfID string }

func newCLIDocker() cliDocker {
	host, _ := os.Hostname()
	return cliDocker{selfID: host}
}

func (cliDocker) volumeExists(ctx context.Context, name string) bool {
	return exec.CommandContext(ctx, "docker", "volume", "inspect", name).Run() == nil
}

func (c cliDocker) runningUsing(ctx context.Context, volume string) ([]usingContainer, error) {
	out, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "volume="+volume, "--filter", "status=running",
		"--format", "{{.ID}}\t{{.Names}}\t{{.Label \""+backupLabelKey+"\"}}").Output()
	if err != nil {
		return nil, fmt.Errorf("list the containers using %s: %w", volume, err)
	}
	return parseUsers(string(out), c.selfID), nil
}

// parseUsers reads `docker ps --format "id<TAB>name<TAB>backup-label"` and
// leaves out the agent (its container id is its host name) and the helpers.
func parseUsers(out, selfID string) []usingContainer {
	var refs []usingContainer
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) < 2 || parts[0] == "" {
			continue
		}
		if len(parts) > 2 && parts[2] != "" {
			continue
		}
		if selfID != "" && strings.HasPrefix(parts[0], selfID) {
			continue
		}
		refs = append(refs, usingContainer{ID: parts[0], Name: parts[1]})
	}
	return refs
}

func (cliDocker) stop(ctx context.Context, c usingContainer) error {
	if out, err := exec.CommandContext(ctx, "docker", "stop", "-t", "30", c.ID).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (cliDocker) start(ctx context.Context, c usingContainer) error {
	if out, err := exec.CommandContext(ctx, "docker", "start", c.ID).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (cliDocker) createVolume(ctx context.Context, name, label string) error {
	if out, err := exec.CommandContext(ctx, "docker", "volume", "create", "--label", label, name).CombinedOutput(); err != nil {
		return fmt.Errorf("create the volume: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (cliDocker) removeVolume(ctx context.Context, name string) error {
	return exec.CommandContext(ctx, "docker", "volume", "rm", "-f", name).Run()
}

// ---- starting a run from the tunnel

// backupGate lets one run work at a time on this host: restic is heavy, and
// two runs on the same repository would only wait for each other's lock.
var backupGate = make(chan struct{}, 1)

// backupReporter is where a finished run is reported, set when the tunnel starts.
var backupReporter struct {
	mu     sync.Mutex
	client *http.Client
	base   string
}

func setBackupReporter(client *http.Client, base string) {
	backupReporter.mu.Lock()
	defer backupReporter.mu.Unlock()
	backupReporter.client, backupReporter.base = client, base
}

// startBackupRun checks the request, starts the work in the background and
// answers at once: a backup of gigabytes cannot be a request that waits.
func startBackupRun(msg tunnelMessage) ([]byte, error) {
	req := msg.Backup
	if req == nil {
		return nil, errors.New("no backup request")
	}
	restore := msg.Action == "backup_restore"
	if err := validateRequest(*req, !restore); err != nil {
		return nil, err
	}
	if req.RunID == "" || !safeNameRe.MatchString(req.RunID) {
		return nil, errors.New("invalid run id")
	}
	go func() {
		backupGate <- struct{}{}
		defer func() { <-backupGate }()
		ctx, cancel := context.WithTimeout(context.Background(), backupRunTimeout)
		defer cancel()

		rep := executeRun(ctx, *req, restore)
		reportRun(req.RunID, rep)
	}()
	return []byte(`{"accepted":true}`), nil
}

func executeRun(ctx context.Context, req backupRequest, restore bool) runReport {
	kind := "backup"
	if restore {
		kind = "restore"
	}
	s, err := newBackupSession(ctx, req, req.RunID)
	if err != nil {
		return runReport{Kind: kind, Status: "error", Code: codeDocker, Message: scrub(err.Error(), req.Dest.Password, req.Dest.RepoPassword)}
	}
	defer s.Close()
	e := &backupExecutor{r: s, d: newCLIDocker()}
	if restore {
		return e.restore(ctx, req)
	}
	return e.backup(ctx, req)
}

// reportRun sends the result to the controller, and keeps trying for a while: the
// controller may be restarting, and a finished backup must not be lost for that.
func reportRun(runID string, rep runReport) {
	backupReporter.mu.Lock()
	client, base := backupReporter.client, backupReporter.base
	backupReporter.mu.Unlock()
	if client == nil || base == "" {
		log.Println("backup: no controller to report run", runID, "to")
		return
	}
	body, _ := json.Marshal(rep)
	delay := 5 * time.Second
	for attempt := 0; attempt < 12; attempt++ {
		resp, err := client.Post(base+"/agent/backup-runs/"+runID+"/report", "application/json", bytes.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return
			}
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
				log.Println("backup: the controller refused the report of run", runID, resp.Status)
				return
			}
		}
		time.Sleep(delay)
		if delay < 5*time.Minute {
			delay *= 2
		}
	}
	log.Println("backup: gave up reporting run", runID)
}

// reapBackupLeftovers removes helper containers and share volumes a crash left
// behind: the volume holds the share's password in its options.
func reapBackupLeftovers() {
	for _, kind := range [][]string{{"ps", "-aq"}, {"volume", "ls", "-q"}} {
		args := append(kind, "--filter", "label="+backupLabel)
		out, err := exec.Command("docker", args...).Output()
		if err != nil {
			continue
		}
		ids := strings.Fields(string(out))
		if len(ids) == 0 {
			continue
		}
		rm := []string{"rm", "-f"}
		if kind[0] == "volume" {
			rm = []string{"volume", "rm", "-f"}
		}
		if err := exec.Command("docker", append(rm, ids...)...).Run(); err != nil {
			log.Println("backup: clean up leftovers:", err)
		} else {
			log.Printf("backup: removed %d leftover %s from an earlier run", len(ids), map[bool]string{true: "volume(s)", false: "container(s)"}[kind[0] == "volume"])
		}
	}
}

// handleBackupCommand serves the quick actions, which answer in the reply.
func handleBackupCommand(ctx context.Context, msg tunnelMessage) ([]byte, error) {
	req := msg.Backup
	if req == nil {
		return nil, errors.New("no backup request")
	}
	id := msg.RequestID
	if !safeNameRe.MatchString(id) {
		id = fmt.Sprintf("q%d", time.Now().UnixNano())
	}
	switch msg.Action {
	case "backup_test":
		return backupTest(ctx, *req, id)
	case "backup_init":
		return backupInit(ctx, *req, id)
	case "backup_snapshots":
		return backupSnapshots(ctx, *req, id)
	}
	return nil, fmt.Errorf("unknown backup action %q", msg.Action)
}
