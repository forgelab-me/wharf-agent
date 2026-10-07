package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func goodDest() backupDestination {
	return backupDestination{Server: "nas.example.lan", Share: "backups", Subdir: "wharf", Version: "3.0",
		Username: "wharf", Password: "s3cret-pw", RepoDir: "h1", RepoPassword: "repo-pw"}
}

func TestValidateDestination(t *testing.T) {
	if err := validateDestination(goodDest()); err != nil {
		t.Fatalf("a good destination: %v", err)
	}
	for name, mutate := range map[string]func(*backupDestination){
		"comma in the password":      func(d *backupDestination) { d.Password = "a,b" },
		"comma in the user":          func(d *backupDestination) { d.Username = "a,uid=0" },
		"option smuggled in a share": func(d *backupDestination) { d.Share = "x,vers=1.0" },
		"newline in the password":    func(d *backupDestination) { d.Password = "a\nb" },
		"empty password":             func(d *backupDestination) { d.Password = "" },
		"server with a slash":        func(d *backupDestination) { d.Server = "nas/evil" },
		"server with a comma":        func(d *backupDestination) { d.Server = "nas,addr=evil" },
		"folder going up":            func(d *backupDestination) { d.Subdir = "wharf/../x" },
		"folder with a space":        func(d *backupDestination) { d.Subdir = "my backups" },
		"absolute folder":            func(d *backupDestination) { d.Subdir = "/wharf" },
		"unknown SMB version":        func(d *backupDestination) { d.Version = "1.0" },
		"bad domain":                 func(d *backupDestination) { d.Domain = "x,y" },
		"repo dir with a slash":      func(d *backupDestination) { d.RepoDir = "a/b" },
		"no repo password":           func(d *backupDestination) { d.RepoPassword = "" },
	} {
		d := goodDest()
		mutate(&d)
		if err := validateDestination(d); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	d := goodDest()
	d.Subdir, d.Domain = "", "HOME"
	if err := validateDestination(d); err != nil {
		t.Errorf("no folder and a domain are fine: %v", err)
	}
}

func TestValidateRequest(t *testing.T) {
	req := backupRequest{Dest: goodDest(), HostTag: "wharf-h1", Volumes: []string{"blog_data", "db.v2"}, Mode: "stop"}
	if err := validateRequest(req, true); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../etc", "a b", "-rm", "a:/b", "x;y", ""} {
		r := req
		r.Volumes = []string{bad}
		if validateRequest(r, true) == nil {
			t.Errorf("volume name %q must be refused", bad)
		}
	}
	r := req
	r.Mode = "pause"
	if validateRequest(r, true) == nil {
		t.Error("an unknown mode must be refused")
	}
	r = req
	r.Volumes = nil
	if validateRequest(r, true) == nil || validateRequest(r, false) != nil {
		t.Error("a run needs volumes, a test does not")
	}
}

func TestRepoPath(t *testing.T) {
	d := goodDest()
	if got := repoPath(d); got != "/repo/wharf/h1" {
		t.Errorf("repoPath = %q", got)
	}
	d.Subdir = ""
	if got := repoPath(d); got != "/repo/h1" {
		t.Errorf("repoPath without a folder = %q", got)
	}
}

func TestCIFSVolumeBodyCarriesTheOptionsAndALabel(t *testing.T) {
	d := goodDest()
	d.Domain = "HOME"
	body := cifsVolumeBody("wharf-bk-r1", d)
	opts := body["DriverOpts"].(map[string]string)
	if opts["type"] != "cifs" || opts["device"] != "//nas.example.lan/backups" {
		t.Errorf("opts = %v", opts)
	}
	for _, want := range []string{"addr=nas.example.lan", "username=wharf", "password=s3cret-pw", "vers=3.0", "domain=HOME"} {
		if !strings.Contains(opts["o"], want) {
			t.Errorf("mount options lack %q: %q", want, opts["o"])
		}
	}
	if body["Labels"].(map[string]string)[backupLabelKey] != "1" || body["Driver"] != "local" {
		t.Errorf("body = %v", body)
	}
}

func TestHelperArgumentsNeverHoldASecret(t *testing.T) {
	s := &backupSession{req: backupRequest{Dest: goodDest(), HostTag: "wharf-h1"}, volume: "wharf-bk-r1", prefix: "wharf-bk-r1"}
	args := s.dockerRunArgs("wharf-bk-r1-1", []string{"blog_data:/volumes/blog_data:ro"}, []string{"backup", "--json", "/volumes/blog_data"})
	joined := strings.Join(args, " ")
	for _, secret := range []string{"s3cret-pw", "repo-pw"} {
		if strings.Contains(joined, secret) {
			t.Errorf("the arguments of docker run, visible in the host's process list, hold %q: %s", secret, joined)
		}
	}
	for _, want := range []string{"--rm", "--label wharf.backup=1", "-e RESTIC_PASSWORD", "-v wharf-bk-r1:/repo", "-v blog_data:/volumes/blog_data:ro", resticImage + " backup --json /volumes/blog_data"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if !strings.Contains(resticImage, "@sha256:") {
		t.Error("the helper image is pinned by digest")
	}
}

func TestScrub(t *testing.T) {
	got := scrub("mount //x failed with password=s3cret-pw and repo-pw", "s3cret-pw", "repo-pw", "")
	if strings.Contains(got, "s3cret-pw") || strings.Contains(got, "repo-pw") {
		t.Errorf("scrub = %q", got)
	}
}

func TestStatusFilterKeepsOnlyWhatMatters(t *testing.T) {
	var out bytes.Buffer
	f := &statusFilter{dst: &out}
	f.Write([]byte(`{"message_type":"status","percent_done":0.5}` + "\n" + `{"message_type":"summary","snapshot_id":"abcd1234"}` + "\n" + `{"message_type":"sta`))
	f.Write([]byte(`tus"}` + "\n" + `{"message_type":"error","error":"x"}` + "\n"))
	got := out.String()
	if strings.Contains(got, `"status"`) || !strings.Contains(got, "summary") || !strings.Contains(got, `"error"`) {
		t.Errorf("filtered = %q", got)
	}
}

func TestCapBuffer(t *testing.T) {
	b := &capBuffer{max: 10}
	b.Write([]byte("0123456789abcdef"))
	if s := b.String(); !strings.HasPrefix(s, "0123456789") || !strings.Contains(s, "6 more bytes") {
		t.Errorf("capped = %q", s)
	}
}

func TestParseSummaryAndSnapshotsAndForget(t *testing.T) {
	out := `{"message_type":"error","error":"x"}
{"message_type":"summary","files_new":3,"files_changed":1,"files_unmodified":9,"data_added":1745,"total_bytes_processed":3002186,"snapshot_id":"ad31ce87"}
`
	s, ok := parseSummary(out)
	if !ok || s.SnapshotID != "ad31ce87" || s.FilesNew != 3 || s.DataAdded != 1745 || s.TotalBytesProcessed != 3002186 {
		t.Errorf("summary = %+v %v", s, ok)
	}
	if _, ok := parseSummary(`{"message_type":"status"}`); ok {
		t.Error("no summary line, no summary")
	}

	snaps, err := parseSnapshots(`[{"id":"ad31ce87aa","short_id":"ad31ce87","time":"2026-10-07T13:11:49Z","hostname":"wharf-h1","tags":["wharf","volume:blog"],"paths":["/volumes/blog"]}]`)
	if err != nil || len(snaps) != 1 || snaps[0].Hostname != "wharf-h1" || !hasTag(snaps[0].Tags, "volume:blog") {
		t.Errorf("snapshots = %+v %v", snaps, err)
	}
	if _, err := parseSnapshots("not json"); err == nil {
		t.Error("garbage is an error")
	}

	// what `forget --json --prune` really prints: the plan, then the prune's own text
	kept, removed, err := parseForget(`[{"keep":[{"id":"a"}],"remove":[{"id":"b"}]}]` + "\nloading indexes...\nfinding data that is still in use\ndone\n")
	if err != nil || kept != 1 || removed != 1 {
		t.Errorf("forget with trailing prune text = %d %d %v", kept, removed, err)
	}
	kept, removed, err = parseForget(`[{"keep":[{"id":"a"},{"id":"b"}],"remove":[{"id":"c"}]},{"keep":[{"id":"d"}],"remove":null}]`)
	if err != nil || kept != 3 || removed != 1 {
		t.Errorf("forget = %d %d %v", kept, removed, err)
	}
}

func TestRetentionArgs(t *testing.T) {
	if len(retentionArgs(retentionPolicy{})) != 0 {
		t.Error("no policy keeps everything")
	}
	got := strings.Join(retentionArgs(retentionPolicy{KeepLast: 3, KeepDaily: 7, KeepMonthly: 6}), " ")
	if got != "--keep-last 3 --keep-daily 7 --keep-monthly 6" {
		t.Errorf("args = %q", got)
	}
}

func TestClassify(t *testing.T) {
	for exit, want := range map[int]string{10: codeNotInitialized, 11: codeLocked, 12: codeWrongPassword, 125: codeDocker, 130: codeCancelled, 1: codeRestic} {
		if code, _ := classify(resticOut{Exit: exit, Stderr: "boom"}); code != want {
			t.Errorf("exit %d: %s, want %s", exit, code, want)
		}
	}
}

func TestParseUsersLeavesOutTheAgentAndTheHelpers(t *testing.T) {
	out := "aaa111bbb222\tblog\t\nself0123456789\twharf-agent\t\nccc333\twharf-bk-r1-1\t1\n"
	got := parseUsers(out, "self01234567")
	if len(got) != 1 || got[0].Name != "blog" {
		t.Errorf("users = %+v", got)
	}
}

// ---- the executor, against fakes

type fakeRestic struct {
	calls [][]string
	reply func(args []string) resticOut
}

func (f *fakeRestic) restic(_ context.Context, _ []string, _ bool, args ...string) resticOut {
	f.calls = append(f.calls, args)
	return f.reply(args)
}

func (f *fakeRestic) called(prefix ...string) bool {
	for _, c := range f.calls {
		if len(c) >= len(prefix) && strings.Join(c[:len(prefix)], " ") == strings.Join(prefix, " ") {
			return true
		}
	}
	return false
}

func (f *fakeRestic) calledWith(sub string) bool {
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), sub) {
			return true
		}
	}
	return false
}

type fakeDocker struct {
	volumes   map[string]bool
	users     map[string][]usingContainer
	stopFail  string
	startFail string
	events    []string
}

func (d *fakeDocker) volumeExists(_ context.Context, n string) bool { return d.volumes[n] }
func (d *fakeDocker) runningUsing(_ context.Context, v string) ([]usingContainer, error) {
	return d.users[v], nil
}
func (d *fakeDocker) stop(_ context.Context, c usingContainer) error {
	d.events = append(d.events, "stop "+c.Name)
	if c.Name == d.stopFail {
		return errors.New("refused")
	}
	return nil
}
func (d *fakeDocker) start(_ context.Context, c usingContainer) error {
	d.events = append(d.events, "start "+c.Name)
	if c.Name == d.startFail {
		return errors.New("no such container")
	}
	return nil
}
func (d *fakeDocker) createVolume(_ context.Context, n, _ string) error {
	d.volumes[n] = true
	d.events = append(d.events, "create "+n)
	return nil
}
func (d *fakeDocker) removeVolume(_ context.Context, n string) error {
	delete(d.volumes, n)
	d.events = append(d.events, "remove "+n)
	return nil
}

func happyRestic() *fakeRestic {
	f := &fakeRestic{}
	f.reply = func(args []string) resticOut {
		switch args[0] {
		case "cat":
			return resticOut{}
		case "backup":
			return resticOut{Stdout: `{"message_type":"summary","files_new":2,"data_added":1500,"total_bytes_processed":9000,"snapshot_id":"aabbccdd"}` + "\n"}
		case "snapshots":
			id := args[len(args)-1]
			return resticOut{Stdout: `[{"id":"` + id + `ffff","hostname":"wharf-h1","tags":["wharf","volume:x"]}]`}
		case "forget":
			return resticOut{Stdout: `[{"keep":[{"id":"a"},{"id":"b"}],"remove":[{"id":"c"}]}]`}
		}
		return resticOut{}
	}
	return f
}

func testReq(mode string, vols ...string) backupRequest {
	return backupRequest{Dest: goodDest(), HostTag: "wharf-h1", Volumes: vols, Mode: mode}
}

func TestBackupSucceedsAndVerifiesTheSnapshot(t *testing.T) {
	r, d := happyRestic(), &fakeDocker{volumes: map[string]bool{"blog": true}}
	req := testReq("live", "blog")
	req.Retention = retentionPolicy{KeepLast: 2}
	rep := (&backupExecutor{r: r, d: d}).backup(context.Background(), req)

	if rep.Status != "success" || len(rep.Volumes) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	v := rep.Volumes[0]
	if v.SnapshotID != "aabbccdd" || v.DataAdded != 1500 || v.Retention != "kept 2, removed 1" {
		t.Errorf("volume = %+v", v)
	}
	if !r.calledWith("--host wharf-h1") || !r.calledWith("--tag volume:blog") {
		t.Errorf("the host and the volume tag must be explicit: %v", r.calls)
	}
	if !r.called("snapshots", "--json", "aabbccdd") {
		t.Error("the new snapshot is read back")
	}
}

func TestBackupStopsAtANotInitializedRepository(t *testing.T) {
	r := happyRestic()
	inner := r.reply
	r.reply = func(a []string) resticOut {
		if a[0] == "cat" {
			return resticOut{Exit: 10}
		}
		return inner(a)
	}
	rep := (&backupExecutor{r: r, d: &fakeDocker{volumes: map[string]bool{"v1": true}}}).backup(context.Background(), testReq("live", "v1"))
	if rep.Status != "error" || rep.Code != codeNotInitialized || r.called("backup") {
		t.Errorf("report = %+v, calls = %v", rep, r.calls)
	}
}

func TestExitThreeIsAWarningNotAFailure(t *testing.T) {
	r := happyRestic()
	inner := r.reply
	r.reply = func(a []string) resticOut {
		if a[0] == "backup" {
			o := inner(a)
			o.Exit = 3
			return o
		}
		return inner(a)
	}
	rep := (&backupExecutor{r: r, d: &fakeDocker{volumes: map[string]bool{"v1": true}}}).backup(context.Background(), testReq("live", "v1"))
	if rep.Status != "warning" || rep.Volumes[0].SnapshotID == "" {
		t.Errorf("report = %+v", rep)
	}
}

func TestAMissingVolumeFailsOnlyThatVolume(t *testing.T) {
	r := happyRestic()
	d := &fakeDocker{volumes: map[string]bool{"v1": true}}
	rep := (&backupExecutor{r: r, d: d}).backup(context.Background(), testReq("live", "v1", "ghost"))
	if rep.Status != "error" || rep.Volumes[0].Status != "success" || rep.Volumes[1].Status != "error" {
		t.Errorf("report = %+v", rep)
	}
}

func TestAnUnreadableSnapshotFailsAndNothingIsRemoved(t *testing.T) {
	r := happyRestic()
	inner := r.reply
	r.reply = func(a []string) resticOut {
		if a[0] == "snapshots" {
			return resticOut{Exit: 1, Stderr: "no such snapshot"}
		}
		return inner(a)
	}
	req := testReq("live", "v1")
	req.Retention = retentionPolicy{KeepLast: 1}
	rep := (&backupExecutor{r: r, d: &fakeDocker{volumes: map[string]bool{"v1": true}}}).backup(context.Background(), req)
	if rep.Status != "error" || r.called("forget") {
		t.Errorf("a green but unreadable snapshot must not let retention run: %+v %v", rep, r.calls)
	}
}

func TestRetentionNeverRunsWhenThePolicyKeepsNothing(t *testing.T) {
	r := happyRestic()
	inner := r.reply
	r.reply = func(a []string) resticOut {
		if a[0] == "forget" {
			return resticOut{Stdout: `[{"keep":[],"remove":[{"id":"a"},{"id":"b"}]}]`}
		}
		return inner(a)
	}
	req := testReq("live", "v1")
	req.Retention = retentionPolicy{KeepLast: 1}
	rep := (&backupExecutor{r: r, d: &fakeDocker{volumes: map[string]bool{"v1": true}}}).backup(context.Background(), req)
	if rep.Status != "warning" || !strings.HasPrefix(rep.Volumes[0].Retention, "skipped") {
		t.Errorf("report = %+v", rep)
	}
	for _, c := range r.calls {
		if c[0] == "forget" && !strings.Contains(strings.Join(c, " "), "--dry-run") {
			t.Errorf("a real forget ran after a dry run that kept nothing: %v", c)
		}
	}
}

func TestStopModeAlwaysRestarts(t *testing.T) {
	r := happyRestic()
	inner := r.reply
	r.reply = func(a []string) resticOut {
		if a[0] == "backup" {
			return resticOut{Exit: 1, Stderr: "disk error"}
		}
		return inner(a)
	}
	d := &fakeDocker{volumes: map[string]bool{"v1": true, "v2": true}, users: map[string][]usingContainer{
		"v1": {{ID: "1", Name: "db"}}, "v2": {{ID: "1", Name: "db"}, {ID: "2", Name: "app"}}}}
	rep := (&backupExecutor{r: r, d: d}).backup(context.Background(), testReq("stop", "v1", "v2"))
	if rep.Status != "error" {
		t.Errorf("report = %+v", rep)
	}
	if got := strings.Join(d.events, ","); got != "stop db,stop app,start app,start db" {
		t.Errorf("a container shared by two volumes is stopped once, and every one comes back even when the backup failed: %s", got)
	}
}

func TestARestartFailureIsAWarningThatSaysSo(t *testing.T) {
	d := &fakeDocker{volumes: map[string]bool{"v1": true}, startFail: "db", users: map[string][]usingContainer{"v1": {{ID: "1", Name: "db"}}}}
	rep := (&backupExecutor{r: happyRestic(), d: d}).backup(context.Background(), testReq("stop", "v1"))
	if rep.Status != "warning" || rep.Code != codeRestartFailed || !strings.Contains(rep.Message, "db") || rep.Volumes[0].Status != "success" {
		t.Errorf("report = %+v", rep)
	}
}

func TestAContainerThatWillNotStopAbortsTheBackupAndRestartsTheOthers(t *testing.T) {
	r := happyRestic()
	d := &fakeDocker{volumes: map[string]bool{"v1": true}, stopFail: "app",
		users: map[string][]usingContainer{"v1": {{ID: "1", Name: "db"}, {ID: "2", Name: "app"}}}}
	rep := (&backupExecutor{r: r, d: d}).backup(context.Background(), testReq("stop", "v1"))
	if rep.Status != "error" || r.called("backup") {
		t.Errorf("no copy is taken of a volume that could not be quiesced: %+v", rep)
	}
	if got := strings.Join(d.events, ","); got != "stop db,stop app,start db" {
		t.Errorf("events = %s", got)
	}
}

func restoreRestic(snapHost string, tags ...string) *fakeRestic {
	f := &fakeRestic{}
	f.reply = func(a []string) resticOut {
		if a[0] == "snapshots" {
			tg := `"` + strings.Join(tags, `","`) + `"`
			return resticOut{Stdout: `[{"id":"aabbccdd11223344","hostname":"` + snapHost + `","tags":[` + tg + `]}]`}
		}
		return resticOut{}
	}
	return f
}

func restoreReq() backupRequest {
	r := testReq("", "")
	r.Volumes = nil
	r.Snapshot, r.Volume, r.NewVolume = "aabbccdd", "blog", "blog-restored"
	return r
}

func TestRestoreIntoANewVolume(t *testing.T) {
	r, d := restoreRestic("wharf-h1", "wharf", "volume:blog"), &fakeDocker{volumes: map[string]bool{"blog": true}}
	rep := (&backupExecutor{r: r, d: d}).restore(context.Background(), restoreReq())
	if rep.Status != "success" || rep.Restored != "blog-restored" || !d.volumes["blog-restored"] {
		t.Fatalf("report = %+v", rep)
	}
	if !r.calledWith("restore aabbccdd11223344:/volumes/blog --target /restore") {
		t.Errorf("calls = %v", r.calls)
	}
}

func TestRestoreRefusesWhatItShouldNot(t *testing.T) {
	good := restoreRestic("wharf-h1", "wharf", "volume:blog")

	r := restoreReq()
	d := &fakeDocker{volumes: map[string]bool{"blog-restored": true}}
	if rep := (&backupExecutor{r: good, d: d}).restore(context.Background(), r); rep.Status != "error" || !strings.Contains(rep.Message, "already exists") {
		t.Errorf("an existing volume is never overwritten: %+v", rep)
	}
	if good.called("restore") {
		t.Error("nothing was restored")
	}

	for name, mut := range map[string]func(*backupRequest){
		"a bad snapshot id": func(r *backupRequest) { r.Snapshot = "../x" },
		"a bad volume":      func(r *backupRequest) { r.Volume = "a/b" },
		"a bad new name":    func(r *backupRequest) { r.NewVolume = "-x" },
	} {
		rq := restoreReq()
		mut(&rq)
		if rep := (&backupExecutor{r: restoreRestic("wharf-h1", "volume:blog"), d: &fakeDocker{volumes: map[string]bool{}}}).restore(context.Background(), rq); rep.Status != "error" || rep.Code != codeValidation {
			t.Errorf("%s: %+v", name, rep)
		}
	}

	// a snapshot of another volume, or of another host
	for _, f := range []*fakeRestic{restoreRestic("wharf-h1", "wharf", "volume:other"), restoreRestic("wharf-h2", "wharf", "volume:blog")} {
		d := &fakeDocker{volumes: map[string]bool{}}
		if rep := (&backupExecutor{r: f, d: d}).restore(context.Background(), restoreReq()); rep.Status != "error" || d.volumes["blog-restored"] {
			t.Errorf("a snapshot of something else must not be restored: %+v", rep)
		}
	}
}

func TestAFailedRestoreLeavesNoNewVolume(t *testing.T) {
	r := restoreRestic("wharf-h1", "wharf", "volume:blog")
	inner := r.reply
	r.reply = func(a []string) resticOut {
		if a[0] == "restore" {
			return resticOut{Exit: 1, Stderr: "no space left"}
		}
		return inner(a)
	}
	d := &fakeDocker{volumes: map[string]bool{}}
	rep := (&backupExecutor{r: r, d: d}).restore(context.Background(), restoreReq())
	if rep.Status != "error" || d.volumes["blog-restored"] {
		t.Errorf("report = %+v, volumes = %v", rep, d.volumes)
	}
}

func TestContainersComeBackBeforeRetentionRuns(t *testing.T) {
	d := &fakeDocker{volumes: map[string]bool{"v1": true}, users: map[string][]usingContainer{"v1": {{ID: "1", Name: "db"}}}}
	r := happyRestic()
	inner := r.reply
	r.reply = func(a []string) resticOut {
		if a[0] == "forget" {
			d.events = append(d.events, "forget")
		}
		return inner(a)
	}
	req := testReq("stop", "v1")
	req.Retention = retentionPolicy{KeepLast: 2}
	rep := (&backupExecutor{r: r, d: d}).backup(context.Background(), req)
	if rep.Status != "success" {
		t.Fatalf("report = %+v", rep)
	}
	got := strings.Join(d.events, ",")
	if !strings.HasPrefix(got, "stop db,start db,forget") {
		t.Errorf("retention does not need the containers stopped, so they restart first: %s", got)
	}
}
