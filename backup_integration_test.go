//go:build integration

// The whole backup path against a real SMB share and a real Docker daemon:
//
//	WHARF_IT_SMB_SERVER=<ip> WHARF_IT_SMB_SHARE=backups WHARF_IT_SMB_USER=u WHARF_IT_SMB_PASSWORD=p \
//	  go test -tags integration -run Integration -v .
//
// It needs the docker CLI and the Docker socket, and creates (then removes)
// volumes and containers named wharf-it-*.
package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func itDest(t *testing.T) backupDestination {
	t.Helper()
	d := backupDestination{
		Server: os.Getenv("WHARF_IT_SMB_SERVER"), Share: os.Getenv("WHARF_IT_SMB_SHARE"),
		Username: os.Getenv("WHARF_IT_SMB_USER"), Password: os.Getenv("WHARF_IT_SMB_PASSWORD"),
		Version: "3.0", Subdir: "wharf-it", RepoDir: "host1", RepoPassword: "it-repo-password",
	}
	if d.Server == "" || d.Share == "" || d.Username == "" || d.Password == "" {
		t.Skip("WHARF_IT_SMB_* is not set")
	}
	return d
}

func sh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestIntegrationBackupAndRestoreOverSMB(t *testing.T) {
	dest := itDest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	src, restored, app := "wharf-it-src", "wharf-it-restored", "wharf-it-app"
	cleanup := func() {
		exec.Command("docker", "rm", "-f", app).Run()
		exec.Command("docker", "volume", "rm", "-f", src, restored).Run()
	}
	cleanup()
	defer cleanup()

	// a volume with a few files, one of them 3 MB of noise
	sh(t, "volume", "create", src)
	sh(t, "run", "--rm", "-v", src+":/d", "alpine", "sh", "-c",
		`mkdir -p /d/sub && echo alpha > /d/a.txt && head -c 3000000 /dev/urandom > /d/sub/big.bin && echo beta > "/d/sub/with space.txt"`)

	base := backupRequest{Dest: dest, HostTag: "wharf-host1", Volumes: []string{src}}

	// 1. the share is reachable and writable, the repository does not exist yet
	out, err := backupTest(ctx, base, "it-test1")
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	var tr testResult
	json.Unmarshal(out, &tr)
	if !tr.Mounted || !tr.Writable || tr.Initialized {
		t.Fatalf("before init: %+v", tr)
	}

	// a wrong password is reported as such and never echoed
	bad := base
	bad.Dest.Password = "definitely-wrong"
	out, err = backupTest(ctx, bad, "it-test2")
	if err != nil {
		t.Fatalf("wrong password test: %v", err)
	}
	json.Unmarshal(out, &tr)
	if tr.Mounted || strings.Contains(tr.Message, "definitely-wrong") || tr.Message == "" {
		t.Errorf("a wrong SMB password: %+v", tr)
	}

	// 2. initialize, once
	if _, err := backupInit(ctx, base, "it-init1"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := backupInit(ctx, base, "it-init2"); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("a second init must be refused: %v", err)
	}

	// 3. back up, live
	run := func(id string, mutate func(*backupRequest)) runReport {
		req := base
		req.RunID = id
		req.Mode = "live"
		if mutate != nil {
			mutate(&req)
		}
		return executeRun(ctx, req, false)
	}
	r1 := run("it-run1", nil)
	if r1.Status != "success" || len(r1.Volumes) != 1 || r1.Volumes[0].SnapshotID == "" || r1.Volumes[0].DataAdded < 3_000_000 {
		t.Fatalf("first backup: %+v", r1)
	}
	t.Logf("first backup: %+v", r1.Volumes[0])

	// 4. change one small file, back up again with retention: only the change is stored
	sh(t, "run", "--rm", "-v", src+":/d", "alpine", "sh", "-c", `echo gamma > /d/c.txt`)
	r2 := run("it-run2", func(r *backupRequest) { r.Retention = retentionPolicy{KeepLast: 1} })
	if r2.Status != "success" || r2.Volumes[0].DataAdded > 100_000 {
		t.Fatalf("second backup should add only the change: %+v", r2)
	}
	if r2.Volumes[0].Retention != "kept 1, removed 1" {
		t.Errorf("retention: %q (the host name is fixed, so both runs are one group)", r2.Volumes[0].Retention)
	}

	// 5. the repository now lists one snapshot of this volume
	qreq := base
	qreq.Volume = src
	out, err = backupSnapshots(ctx, qreq, "it-snaps")
	if err != nil {
		t.Fatalf("snapshots: %v", err)
	}
	var listed struct{ Snapshots []resticSnapshot }
	json.Unmarshal(out, &listed)
	if len(listed.Snapshots) != 1 || listed.Snapshots[0].Hostname != "wharf-host1" {
		t.Fatalf("snapshots: %s", out)
	}
	snap := listed.Snapshots[0].ID

	// 6. restore into a new volume, and compare every file
	rreq := base
	rreq.RunID, rreq.Snapshot, rreq.Volume, rreq.NewVolume = "it-restore", snap, src, restored
	rr := executeRun(ctx, rreq, true)
	if rr.Status != "success" || rr.Restored != restored {
		t.Fatalf("restore: %+v", rr)
	}
	sum := func(vol string) string {
		return sh(t, "run", "--rm", "-v", vol+":/d:ro", "alpine", "sh", "-c", `cd /d && find . -type f -exec sha256sum {} + | sort -k2`)
	}
	if a, b := sum(src), sum(restored); a != b {
		t.Errorf("the restored volume differs:\n%s\n---\n%s", a, b)
	}
	// a second restore to the same name is refused
	if rr := executeRun(ctx, rreq, true); rr.Status != "error" || !strings.Contains(rr.Message, "already exists") {
		t.Errorf("restoring over an existing volume: %+v", rr)
	}

	// 7. stop mode: a container using the volume is stopped for the copy and comes back
	sh(t, "run", "-d", "--name", app, "-v", src+":/d", "alpine", "sleep", "300")
	r3 := run("it-run3", func(r *backupRequest) { r.Mode = "stop" })
	if r3.Status != "success" {
		t.Fatalf("stop-mode backup: %+v", r3)
	}
	if state := sh(t, "inspect", "-f", "{{.State.Running}}", app); state != "true" {
		t.Errorf("the container must be running again, running=%s", state)
	}

	// 8. nothing is left behind: no helper container, no share volume
	if left := sh(t, "ps", "-aq", "--filter", "label="+backupLabel); left != "" {
		t.Errorf("helper containers left behind: %s", left)
	}
	if left := sh(t, "volume", "ls", "-q", "--filter", "label="+backupLabel); left != "" {
		t.Errorf("share volumes left behind (they hold the password): %s", left)
	}
}
