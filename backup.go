// Volume backups: restic, in a throwaway helper container, writing to a SMB
// share that Docker itself mounts as a temporary volume. Cf. ARCHITECTURE.md,
// "Sauvegardes de volumes".
//
// Every restic call is one `docker run` of the official restic image, with the
// volumes mounted read-only and the repository volume mounted read-write. No
// restic runs in the controller, and no script runs in the helper: the agent
// passes restic its arguments and reads what it prints.
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
	"regexp"
	"strings"
	"sync"
	"time"
)

// resticImage is the restic project's own image, pinned by digest: a tag can be
// moved, a digest cannot. Multi-arch (amd64, arm64, arm/v7, 386).
const resticImage = "restic/restic:0.18.1@sha256:39d9072fb5651c80d75c7a811612eb60b4c06b32ffe87c2e9f3c7222e1797e76"

const (
	// backupLabel marks every container and volume this file creates, so a
	// crash that leaves some behind can be cleaned up at the next start.
	backupLabelKey = "wharf.backup-helper" // not "wharf.backup": that one is the name a user is likely to give their own volumes
	backupLabel    = backupLabelKey + "=1"

	backupQuickTimeout = 3 * time.Minute // test, init, list: includes a first pull of the image
	backupRunTimeout   = 12 * time.Hour
)

// backupDestination is a SMB share and the restic repository on it.
type backupDestination struct {
	Server       string `json:"server"`
	Share        string `json:"share"`
	Subdir       string `json:"subdir,omitempty"`
	Version      string `json:"version"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	Domain       string `json:"domain,omitempty"`
	RepoDir      string `json:"repo_dir"`      // one repository per host, under Subdir
	RepoPassword string `json:"repo_password"` // restic's own password: losing it loses the backups
}

type retentionPolicy struct {
	KeepLast    int `json:"keep_last,omitempty"`
	KeepDaily   int `json:"keep_daily,omitempty"`
	KeepWeekly  int `json:"keep_weekly,omitempty"`
	KeepMonthly int `json:"keep_monthly,omitempty"`
}

// backupRequest is what the controller sends for every backup action.
type backupRequest struct {
	RunID     string            `json:"run_id,omitempty"`
	Dest      backupDestination `json:"dest"`
	HostTag   string            `json:"host_tag"` // restic --host: stable, or snapshots stop grouping
	Volumes   []string          `json:"volumes,omitempty"`
	Mode      string            `json:"mode,omitempty"` // "live" | "stop"
	Retention retentionPolicy   `json:"retention,omitempty"`

	// restore
	Snapshot  string `json:"snapshot,omitempty"`
	Volume    string `json:"volume,omitempty"`
	NewVolume string `json:"new_volume,omitempty"`
}

// ---- validation: everything below ends up in a docker option string or a path

var (
	smbServerRe  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	smbShareRe   = regexp.MustCompile(`^[A-Za-z0-9._$ -]+$`)
	subdirRe     = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
	smbDomainRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	safeNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	snapshotIDRe = regexp.MustCompile(`^[a-f0-9]{8,64}$`)
	smbVersions  = map[string]bool{"2.0": true, "2.1": true, "3.0": true, "3.02": true, "3.1.1": true}
)

// validateCredentialText: a SMB user name or password goes into a comma
// separated option string, so a comma would end it early and let the rest be
// read as another option.
func validateCredentialText(what, v string) error {
	if v == "" {
		return fmt.Errorf("%s is required", what)
	}
	if strings.ContainsRune(v, ',') {
		return fmt.Errorf("%s cannot contain a comma (it would end the mount options)", what)
	}
	for _, r := range v {
		if r < ' ' || r == 0x7f {
			return fmt.Errorf("%s cannot contain control characters", what)
		}
	}
	return nil
}

func validateDestination(d backupDestination) error {
	if !smbServerRe.MatchString(d.Server) {
		return errors.New("the server must be a host name or an IP address")
	}
	if !smbShareRe.MatchString(d.Share) {
		return errors.New("the share name has characters a share cannot have")
	}
	if d.Subdir != "" && !subdirRe.MatchString(d.Subdir) {
		return errors.New("the folder must be plain path segments (letters, digits, . _ -), without ..")
	}
	for _, seg := range strings.Split(d.Subdir, "/") {
		if seg == ".." || seg == "." {
			return errors.New("the folder must not contain . or .. segments")
		}
	}
	if !smbVersions[d.Version] {
		return errors.New("the SMB version must be 2.0, 2.1, 3.0, 3.02 or 3.1.1")
	}
	if err := validateCredentialText("the user name", d.Username); err != nil {
		return err
	}
	if err := validateCredentialText("the password", d.Password); err != nil {
		return err
	}
	if d.Domain != "" && !smbDomainRe.MatchString(d.Domain) {
		return errors.New("the domain has characters a domain cannot have")
	}
	if !safeNameRe.MatchString(d.RepoDir) {
		return errors.New("invalid repository folder")
	}
	if d.RepoPassword == "" {
		return errors.New("the repository password is missing")
	}
	return nil
}

func validateRequest(req backupRequest, needVolumes bool) error {
	if err := validateDestination(req.Dest); err != nil {
		return err
	}
	if !safeNameRe.MatchString(req.HostTag) {
		return errors.New("invalid host tag")
	}
	if needVolumes && len(req.Volumes) == 0 {
		return errors.New("no volume to back up")
	}
	for _, v := range req.Volumes {
		if !safeNameRe.MatchString(v) {
			return fmt.Errorf("invalid volume name %q", v)
		}
	}
	if req.Mode != "" && req.Mode != "live" && req.Mode != "stop" {
		return errors.New("mode must be live or stop")
	}
	return nil
}

// repoPath is where the restic repository lives inside the helper.
func repoPath(d backupDestination) string {
	if d.Subdir != "" {
		return "/repo/" + d.Subdir + "/" + d.RepoDir
	}
	return "/repo/" + d.RepoDir
}

// ---- the temporary CIFS volume

// cifsVolumeBody is the JSON for the engine's POST /volumes/create. It goes
// through the API and not `docker volume create`, whose arguments would show
// the password in the host's process list.
func cifsVolumeBody(name string, d backupDestination) map[string]any {
	o := "addr=" + d.Server + ",username=" + d.Username + ",password=" + d.Password + ",vers=" + d.Version
	if d.Domain != "" {
		o += ",domain=" + d.Domain
	}
	return map[string]any{
		"Name":   name,
		"Driver": "local",
		"Labels": map[string]string{backupLabelKey: "1"},
		"DriverOpts": map[string]string{
			"type":   "cifs",
			"device": "//" + d.Server + "/" + d.Share,
			"o":      o,
		},
	}
}

func engineClient() (*http.Client, error) {
	sock := dockerSocket()
	if sock == "" {
		return nil, errors.New("backups need the Docker socket (DOCKER_HOST is not a unix socket)")
	}
	return unixClient(sock), nil
}

func createCIFSVolume(ctx context.Context, name string, d backupDestination) error {
	client, err := engineClient()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(cifsVolumeBody(name, d))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/volumes/create", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("create the share volume: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("create the share volume: %s", scrub(strings.TrimSpace(string(msg)), d.Password))
	}
	return nil
}

// scrub removes secrets from anything that may be shown or logged.
func scrub(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "********")
		}
	}
	return s
}

// ---- running docker and restic

type resticOut struct {
	Exit   int // restic's exit code, or 125 when docker itself failed
	Stdout string
	Stderr string
}

// capBuffer keeps at most max bytes and counts what it dropped.
type capBuffer struct {
	buf     bytes.Buffer
	max     int
	dropped int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) <= room {
			c.buf.Write(p)
		} else {
			c.buf.Write(p[:room])
			c.dropped += len(p) - room
		}
	} else {
		c.dropped += len(p)
	}
	return len(p), nil
}

func (c *capBuffer) String() string {
	if c.dropped > 0 {
		return c.buf.String() + fmt.Sprintf("\n… %d more bytes not kept", c.dropped)
	}
	return c.buf.String()
}

// statusFilter drops restic's per-second progress lines (--json) and keeps
// everything else, so a long backup does not fill the buffer with progress.
type statusFilter struct {
	dst     io.Writer
	pending []byte
}

func (f *statusFilter) Write(p []byte) (int, error) {
	f.pending = append(f.pending, p...)
	for {
		i := bytes.IndexByte(f.pending, '\n')
		if i < 0 {
			break
		}
		line := f.pending[:i+1]
		if !bytes.Contains(line, []byte(`"message_type":"status"`)) {
			f.dst.Write(line)
		}
		f.pending = f.pending[i+1:]
	}
	return len(p), nil
}

// backupSession is one run's temporary share volume and the name its helper
// containers carry.
type backupSession struct {
	req    backupRequest
	volume string
	prefix string
	seq    int
	mu     sync.Mutex
}

func newBackupSession(ctx context.Context, req backupRequest, id string) (*backupSession, error) {
	if err := ensureResticImage(ctx); err != nil {
		return nil, err
	}
	s := &backupSession{req: req, volume: "wharf-bk-" + id, prefix: "wharf-bk-" + id}
	if err := createCIFSVolume(ctx, s.volume, req.Dest); err != nil {
		return nil, err
	}
	return s, nil
}

// Close removes the share volume: it holds the password in its options.
func (s *backupSession) Close() {
	out, err := exec.Command("docker", "volume", "rm", "-f", s.volume).CombinedOutput()
	if err != nil {
		log.Printf("backup: remove volume %s: %v: %s", s.volume, err, strings.TrimSpace(string(out)))
	}
}

func ensureResticImage(ctx context.Context) error {
	if exec.CommandContext(ctx, "docker", "image", "inspect", resticImage).Run() == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "docker", "pull", "-q", resticImage).CombinedOutput()
	if err != nil {
		return fmt.Errorf("pull the restic image: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerRunArgs builds the docker run argument list for one helper. The
// secrets never appear in it: they are passed by name (-e NAME) and live in the
// docker CLI's environment.
func (s *backupSession) dockerRunArgs(name string, mounts []string, resticArgs []string) []string {
	args := []string{
		"run", "--rm", "--name", name, "--label", backupLabel,
		"--security-opt", "no-new-privileges",
		"-e", "RESTIC_PASSWORD", "-e", "RESTIC_REPOSITORY",
		"-v", s.volume + ":/repo",
	}
	for _, m := range mounts {
		args = append(args, "-v", m)
	}
	args = append(args, resticImage)
	return append(args, resticArgs...)
}

// restic runs one restic command in a helper and waits for it. On cancellation
// or timeout the helper is stopped (not just the docker client), so restic gets
// the chance to release the repository lock.
func (s *backupSession) restic(ctx context.Context, mounts []string, quiet bool, resticArgs ...string) resticOut {
	s.mu.Lock()
	s.seq++
	name := fmt.Sprintf("%s-%d", s.prefix, s.seq)
	s.mu.Unlock()

	cmd := exec.CommandContext(ctx, "docker", s.dockerRunArgs(name, mounts, resticArgs)...)
	cmd.Env = append(os.Environ(),
		"RESTIC_PASSWORD="+s.req.Dest.RepoPassword,
		"RESTIC_REPOSITORY="+repoPath(s.req.Dest),
	)
	stdout, stderr := &capBuffer{max: 1 << 20}, &capBuffer{max: 256 << 10}
	if quiet {
		cmd.Stdout = &statusFilter{dst: stdout}
	} else {
		cmd.Stdout = stdout
	}
	cmd.Stderr = stderr
	cmd.Cancel = func() error {
		_ = exec.Command("docker", "stop", "-t", "20", name).Run()
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 40 * time.Second

	err := cmd.Run()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			exit = 125
			stderr.Write([]byte(err.Error()))
		}
	}
	secrets := []string{s.req.Dest.Password, s.req.Dest.RepoPassword}
	return resticOut{Exit: exit, Stdout: scrub(stdout.String(), secrets...), Stderr: scrub(stderr.String(), secrets...)}
}

// ---- reading restic

// backup error codes: a closed list, so the page can say what kind of failure it is.
const (
	codeValidation     = "VALIDATION"
	codeNotInitialized = "REPO_NOT_INITIALIZED"
	codeWrongPassword  = "WRONG_PASSWORD"
	codeLocked         = "REPO_LOCKED"
	codeDocker         = "DOCKER"
	codeRestic         = "RESTIC"
	codeIntegrity      = "INTEGRITY"
	codeCancelled      = "CANCELLED"
	codeRestartFailed  = "STOPPED_RESTART_FAILED"
)

// classify turns a restic exit code (and docker's 125) into a code and a sentence.
func classify(o resticOut) (code, msg string) {
	text := strings.TrimSpace(o.Stderr)
	switch o.Exit {
	case 10:
		return codeNotInitialized, "the repository is not initialized for this host: use Initialize the repository on the job's page (once per host)"
	case 11:
		return codeLocked, "the repository is locked by another operation"
	case 12:
		return codeWrongPassword, "the repository password is wrong"
	case 125:
		return codeDocker, "docker could not run the helper: " + firstLines(text, 3)
	case 130, 137, 143:
		return codeCancelled, "the operation was interrupted"
	}
	return codeRestic, "restic failed (exit " + fmt.Sprint(o.Exit) + "): " + firstLines(text, 4)
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " / ")
}

// resticSummary is the last line of `restic backup --json`.
type resticSummary struct {
	MessageType         string `json:"message_type"`
	FilesNew            int    `json:"files_new"`
	FilesChanged        int    `json:"files_changed"`
	FilesUnmodified     int    `json:"files_unmodified"`
	DataAdded           int64  `json:"data_added"`
	TotalBytesProcessed int64  `json:"total_bytes_processed"`
	SnapshotID          string `json:"snapshot_id"`
}

// parseSummary finds the summary line; ok is false when there is none.
func parseSummary(stdout string) (resticSummary, bool) {
	var found resticSummary
	ok := false
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, `"message_type":"summary"`) {
			continue
		}
		var s resticSummary
		if json.Unmarshal([]byte(line), &s) == nil && s.MessageType == "summary" {
			found, ok = s, true
		}
	}
	return found, ok
}

type resticSnapshot struct {
	ID       string   `json:"id"`
	ShortID  string   `json:"short_id"`
	Time     string   `json:"time"`
	Hostname string   `json:"hostname"`
	Tags     []string `json:"tags"`
	Paths    []string `json:"paths"`
}

func parseSnapshots(stdout string) ([]resticSnapshot, error) {
	var snaps []resticSnapshot
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &snaps); err != nil {
		return nil, fmt.Errorf("restic answered something that is not a list of snapshots")
	}
	return snaps, nil
}

// forgetGroup is one group of `restic forget --json`.
type forgetGroup struct {
	Keep   []json.RawMessage `json:"keep"`
	Remove []json.RawMessage `json:"remove"`
}

func parseForget(stdout string) (kept, removed int, err error) {
	// with --prune, restic prints the plan as JSON and then the prune's progress as
	// plain text: only the first value is the plan.
	var groups []forgetGroup
	if err := json.NewDecoder(strings.NewReader(stdout)).Decode(&groups); err != nil {
		return 0, 0, errors.New("restic answered something that is not a retention plan")
	}
	for _, g := range groups {
		kept += len(g.Keep)
		removed += len(g.Remove)
	}
	return kept, removed, nil
}

// retentionArgs are the restic flags of a policy; none means keep everything.
func retentionArgs(p retentionPolicy) []string {
	var args []string
	add := func(flag string, n int) {
		if n > 0 {
			args = append(args, flag, fmt.Sprint(n))
		}
	}
	add("--keep-last", p.KeepLast)
	add("--keep-daily", p.KeepDaily)
	add("--keep-weekly", p.KeepWeekly)
	add("--keep-monthly", p.KeepMonthly)
	return args
}

func volumeTag(v string) string { return "volume:" + v }

// ---- quick actions: test, initialize, list

type testResult struct {
	Mounted     bool   `json:"mounted"`
	Writable    bool   `json:"writable"`
	Initialized bool   `json:"initialized"`
	Message     string `json:"message,omitempty"`
}

// probeScript writes and removes a file in the repository folder, to prove the
// share is writable by this user.
const probeScript = `mkdir -p "$1" && touch "$1/.wharf-probe" && rm "$1/.wharf-probe"`

func runQuick(ctx context.Context, req backupRequest, id string, fn func(s *backupSession) (any, error)) ([]byte, error) {
	if err := validateRequest(req, false); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, backupQuickTimeout)
	defer cancel()
	s, err := newBackupSession(ctx, req, id)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	v, err := fn(s)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func backupTest(ctx context.Context, req backupRequest, id string) ([]byte, error) {
	return runQuick(ctx, req, id, func(s *backupSession) (any, error) {
		res := testResult{}
		probe := s.probe(ctx)
		if probe.Exit != 0 {
			_, msg := classify(probe)
			res.Message = msg
			return res, nil
		}
		res.Mounted, res.Writable = true, true
		cat := s.restic(ctx, nil, false, "cat", "config")
		switch cat.Exit {
		case 0:
			res.Initialized = true
		case 10:
			res.Message = "the share is writable; the repository is not initialized yet"
		default:
			_, res.Message = classify(cat)
		}
		return res, nil
	})
}

// probe runs the writability check through the restic image's own sh.
func (s *backupSession) probe(ctx context.Context) resticOut {
	s.mu.Lock()
	s.seq++
	name := fmt.Sprintf("%s-%d", s.prefix, s.seq)
	s.mu.Unlock()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", name, "--label", backupLabel,
		"--security-opt", "no-new-privileges", "-v", s.volume+":/repo", "--entrypoint", "sh", resticImage,
		"-c", probeScript, "sh", repoPath(s.req.Dest))
	stderr := &capBuffer{max: 64 << 10}
	cmd.Stderr = stderr
	err := cmd.Run()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			exit = 125
			stderr.Write([]byte(err.Error()))
		}
	}
	return resticOut{Exit: exit, Stderr: scrub(stderr.String(), s.req.Dest.Password, s.req.Dest.RepoPassword)}
}

func backupInit(ctx context.Context, req backupRequest, id string) ([]byte, error) {
	return runQuick(ctx, req, id, func(s *backupSession) (any, error) {
		if o := s.probe(ctx); o.Exit != 0 {
			_, msg := classify(o)
			return nil, errors.New(msg)
		}
		if o := s.restic(ctx, nil, false, "cat", "config"); o.Exit == 0 {
			return nil, errors.New("the repository is already initialized")
		}
		o := s.restic(ctx, nil, false, "init")
		if o.Exit != 0 {
			_, msg := classify(o)
			return nil, errors.New(msg)
		}
		return map[string]bool{"initialized": true}, nil
	})
}

func backupSnapshots(ctx context.Context, req backupRequest, id string) ([]byte, error) {
	return runQuick(ctx, req, id, func(s *backupSession) (any, error) {
		args := []string{"snapshots", "--json", "--host", req.HostTag}
		if req.Volume != "" {
			if !safeNameRe.MatchString(req.Volume) {
				return nil, errors.New("invalid volume name")
			}
			args = append(args, "--tag", volumeTag(req.Volume))
		}
		o := s.restic(ctx, nil, false, args...)
		if o.Exit != 0 {
			_, msg := classify(o)
			return nil, errors.New(msg)
		}
		snaps, err := parseSnapshots(o.Stdout)
		if err != nil {
			return nil, err
		}
		return map[string]any{"snapshots": snaps}, nil
	})
}
