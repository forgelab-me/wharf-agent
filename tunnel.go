// Persistent host-state tunnel to the controller. Reports docker ps,
// docker images, docker volume ls and docker network ls state the moment
// something changes (via `docker events`), plus a periodic full resync
// as a safety net for a missed event or an agent restart. Runs on its
// own long-lived WebSocket, entirely separate from the deployment poll
// loop in main.go -- a dropped tunnel never affects whether a queued
// deployment gets picked up, and vice versa.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type containerReport struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Image       string   `json:"image"`
	State       string   `json:"state"`
	Status      string   `json:"status"`
	Ports       string   `json:"ports"`
	Created     string   `json:"created"`
	StackID     string   `json:"stack_id"`
	ServiceName string   `json:"service_name"`
	Mounts      []string `json:"mounts,omitempty"`   // named volumes only, cf. dockerPsSnapshot
	Networks    []string `json:"networks,omitempty"` // cf. dockerPsSnapshot
}

type imageReport struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Size       string `json:"size"`
}

type volumeReport struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
}

type networkReport struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
	Scope  string `json:"scope"`
}

// tunnelMessage covers three message kinds sharing one envelope: "state"
// (agent->controller, the fields above -- all four arrays together per
// send, always, rather than separate message types per resource kind:
// simpler than interleaving/ordering independent message streams over
// the same connection, and every trigger for sending one is equally a
// reason to refresh all four), "command" (controller->agent:
// RequestID/Action/ContainerID), and "command_result" (agent->controller
// reply: RequestID/OK/Output).
type tunnelMessage struct {
	Type       string            `json:"type"`
	Containers []containerReport `json:"containers,omitempty"`
	Images     []imageReport     `json:"images,omitempty"`
	Volumes    []volumeReport    `json:"volumes,omitempty"`
	Networks   []networkReport   `json:"networks,omitempty"`

	// "command" (controller -> agent)
	RequestID   string `json:"request_id,omitempty"`
	Action      string `json:"action,omitempty"` // "restart" | "stop" | ... | "volume_list" | "volume_read" | "volume_write" | "volume_rename" | "volume_delete"
	ContainerID string `json:"container_id,omitempty"`

	// Volume browsing (controller -> agent) -- see handleVolumeCommand.
	VolumeName string `json:"volume_name,omitempty"`
	Path       string `json:"path,omitempty"`
	NewPath    string `json:"new_path,omitempty"`
	Data       string `json:"data,omitempty"`

	// "command_result" (agent -> controller)
	OK     bool   `json:"ok,omitempty"`
	Output string `json:"output,omitempty"`
}

// stateResyncInterval is the safety-net cadence: docker events alone can
// miss things (a restart between reconnects, a dropped line), so a full
// resync happens on this schedule regardless of whether anything was
// reported as changed.
const stateResyncInterval = 45 * time.Second

// runHostStateTunnel dials the controller's tunnel endpoint and keeps it
// open for as long as the process runs, reconnecting on any failure.
// Never returns.
func runHostStateTunnel(httpClient *http.Client, controllerURL string) {
	tunnelURL := strings.Replace(controllerURL, "https://", "wss://", 1) + "/agent/tunnel"
	for {
		if err := tunnelOnce(httpClient, tunnelURL); err != nil {
			log.Println("tunnel:", err, "-- reconnecting in 5s")
		}
		time.Sleep(5 * time.Second)
	}
}

func tunnelOnce(httpClient *http.Client, tunnelURL string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // also kills this connection's docker events subprocess

	conn, _, err := websocket.Dial(ctx, tunnelURL, &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	log.Println("tunnel: connected")

	// Reading is both how a closed connection is noticed promptly
	// (without this, a dead tunnel would only be caught on the next
	// scheduled write, up to stateResyncInterval later) and, now, how a
	// "command" from the controller (restart/stop a container) arrives.
	// writeMu serializes this loop's command-result replies against
	// send()'s own writes below -- both write to the same conn.
	var writeMu sync.Mutex
	closed := make(chan error, 1)
	go func() {
		for {
			var msg tunnelMessage
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				closed <- err
				return
			}
			if msg.Type == "command" {
				go handleCommand(ctx, conn, &writeMu, msg)
			}
		}
	}()

	changed := make(chan struct{}, 1)
	go watchDockerEvents(ctx, changed)

	send := func() error {
		containers, err := dockerPsSnapshot()
		if err != nil {
			log.Println("tunnel: docker ps failed:", err)
			return nil // transient -- don't tear down the tunnel for this
		}
		images, err := dockerImagesSnapshot()
		if err != nil {
			log.Println("tunnel: docker images failed:", err)
			return nil
		}
		volumes, err := dockerVolumesSnapshot()
		if err != nil {
			log.Println("tunnel: docker volume ls failed:", err)
			return nil
		}
		networks, err := dockerNetworksSnapshot()
		if err != nil {
			log.Println("tunnel: docker network ls failed:", err)
			return nil
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return wsjson.Write(ctx, conn, tunnelMessage{Type: "state", Containers: containers, Images: images, Volumes: volumes, Networks: networks})
	}

	if err := send(); err != nil {
		return fmt.Errorf("initial snapshot: %w", err)
	}

	ticker := time.NewTicker(stateResyncInterval)
	defer ticker.Stop()

	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()

	for {
		select {
		case err := <-closed:
			return fmt.Errorf("connection closed: %w", err)
		case <-changed:
			// Coalesce a burst of events (e.g. `compose up` creating
			// several containers at once) into one snapshot.
			debounce.Reset(300 * time.Millisecond)
		case <-debounce.C:
			if err := send(); err != nil {
				return fmt.Errorf("write: %w", err)
			}
		case <-ticker.C:
			if err := send(); err != nil {
				return fmt.Errorf("write: %w", err)
			}
		}
	}
}

// handleCommand runs a restart/stop/logs/inspect/stats/top/*_inspect/
// *_remove action requested by the controller and writes back the
// result under the same request id. Runs
// in its own goroutine (started by tunnelOnce's read loop) so a slow
// docker command never blocks reading the next message off the
// connection; writeMu keeps this reply from interleaving mid-frame with
// a concurrent state snapshot write.
func handleCommand(ctx context.Context, conn *websocket.Conn, writeMu *sync.Mutex, msg tunnelMessage) {
	var out []byte
	var err error
	switch msg.Action {
	case "restart":
		out, err = exec.Command("docker", "restart", msg.ContainerID).CombinedOutput()
	case "stop":
		out, err = exec.Command("docker", "stop", msg.ContainerID).CombinedOutput()
	case "logs":
		out, err = exec.Command("docker", "logs", "--tail", "200", "--timestamps", msg.ContainerID).CombinedOutput()
	case "inspect":
		out, err = exec.Command("docker", "inspect", msg.ContainerID).CombinedOutput()
	case "stats":
		out, err = exec.Command("docker", "stats", "--no-stream", "--format", "{{json .}}", msg.ContainerID).CombinedOutput()
	case "top":
		out, err = exec.Command("docker", "top", msg.ContainerID).CombinedOutput()
	case "host_stats":
		// Three commands, one reply: per-container CPU/mem/net/io (summed
		// controller-side into host-wide figures -- the agent only has
		// docker.sock, not the host's own /proc, so "host" here means
		// "every container on it", not the whole machine, cf.
		// ARCHITECTURE.md), docker's own disk footprint, and `docker info`
		// for the two numbers that ARE real host-wide facts available
		// without /proc -- total memory and CPU count, which the daemon
		// itself already knows and hands out over the same docker.sock.
		// Joined with lines the controller splits back apart -- simpler
		// than three round trips, and none of these fail independently of
		// each other in practice.
		var statsOut, dfOut, infoOut []byte
		var statsErr, dfErr, infoErr error
		statsOut, statsErr = exec.Command("docker", "stats", "--no-stream", "--format", "{{json .}}").CombinedOutput()
		dfOut, dfErr = exec.Command("docker", "system", "df", "--format", "{{json .}}").CombinedOutput()
		infoOut, infoErr = exec.Command("docker", "info", "--format", "{{json .}}").CombinedOutput()
		out = append(append(append(append(statsOut, []byte("\n---df---\n")...), dfOut...), []byte("\n---info---\n")...), infoOut...)
		switch {
		case statsErr != nil:
			err = statsErr
		case dfErr != nil:
			err = dfErr
		default:
			err = infoErr
		}
	case "image_inspect":
		out, err = exec.Command("docker", "inspect", msg.ContainerID).CombinedOutput()
	case "image_history":
		out, err = exec.Command("docker", "history", "--no-trunc", "--format", "{{json .}}", msg.ContainerID).CombinedOutput()
	case "volume_inspect":
		out, err = exec.Command("docker", "volume", "inspect", msg.ContainerID).CombinedOutput()
	case "volume_sizes":
		// -v (verbose) is what actually walks each volume's directory to
		// compute a real size -- cf. ARCHITECTURE.md's earlier deferral of
		// this exact feature, which ruled out running it continuously in
		// the tunnel's background snapshot loop as too slow, not running
		// it at all. On demand, once per /volumes page load, it's a single
		// command either way. --format json here still returns one JSON
		// object per invocation (not one per volume) with a top-level
		// "Volumes" array -- confirmed against a real Docker Desktop
		// install, not assumed from -v's plain-table behavior.
		out, err = exec.Command("docker", "system", "df", "-v", "--format", "{{json .}}").CombinedOutput()
	case "network_inspect":
		out, err = exec.Command("docker", "network", "inspect", msg.ContainerID).CombinedOutput()
	case "image_remove":
		out, err = exec.Command("docker", "rmi", msg.ContainerID).CombinedOutput()
	case "volume_remove":
		out, err = exec.Command("docker", "volume", "rm", msg.ContainerID).CombinedOutput()
	case "network_remove":
		out, err = exec.Command("docker", "network", "rm", msg.ContainerID).CombinedOutput()
	case "volume_list", "volume_read", "volume_write", "volume_rename", "volume_delete":
		out, err = handleVolumeCommand(msg)
	default:
		err = fmt.Errorf("unknown action %q", msg.Action)
	}

	output := string(out)
	if err != nil && output == "" {
		output = err.Error()
	}
	result := tunnelMessage{Type: "command_result", RequestID: msg.RequestID, OK: err == nil, Output: output}

	writeMu.Lock()
	defer writeMu.Unlock()
	if err := wsjson.Write(ctx, conn, result); err != nil {
		log.Println("tunnel: write command result:", err)
	}
}

// volumeHelperImage mounts the target volume read-write for the
// duration of one operation, nothing else on the host -- a path
// traversal bug here can at worst escape into this throwaway
// container's own ephemeral filesystem, never the real host or another
// volume. alpine:3.24.1 specifically (not "latest") because it's the
// exact tag this agent's own image is already built on (cf.
// agent/Dockerfile) -- already cached on any host that's pulled the
// agent, so this never needs its own separate pull.
const volumeHelperImage = "alpine:3.24.1"

// volumeListScript lists one directory's immediate children as
// tab-separated "type\tsize\tmtime\tname" lines. NUL-delimited find/read
// so a filename containing spaces (or almost anything except a literal
// newline) round-trips correctly -- verified against a real volume
// while building this, including a "file with spaces.txt".
const volumeListScript = `cd "$1" 2>&1 || exit 1
find . -mindepth 1 -maxdepth 1 -print0 | while IFS= read -r -d '' p; do
  n=$(basename "$p")
  if [ -d "$p" ]; then t=d; sz=0; else t=f; sz=$(stat -c%s "$p" 2>/dev/null || echo 0); fi
  mt=$(stat -c%Y "$p" 2>/dev/null || echo 0)
  printf '%s\t%s\t%s\t%s\n' "$t" "$sz" "$mt" "$n"
done`

// handleVolumeCommand runs one browsing/editing operation against
// msg.VolumeName in a fresh, single-purpose container -- cf.
// volumeHelperImage. Every path (Path/NewPath) is the controller's
// already-validated full in-container path (always under /vol); this
// function trusts them as-is and only decides *how* to invoke the
// helper per action. Path/NewPath/msg.Data are passed as positional
// shell parameters ($1/$2), never interpolated into the script text
// itself, specifically so a path or a rename target can never be
// read as shell syntax -- confirmed with a real "/vol/x; rm -rf /vol/y"
// attempt while building this, which landed as a single (failed,
// harmless) literal filename, not a second command.
func handleVolumeCommand(msg tunnelMessage) ([]byte, error) {
	mount := msg.VolumeName + ":/vol"
	switch msg.Action {
	case "volume_list":
		return exec.Command("docker", "run", "--rm", "-v", mount, volumeHelperImage,
			"sh", "-c", volumeListScript, "sh", msg.Path).CombinedOutput()
	case "volume_read":
		// -w0: always a single line, regardless of file size -- the
		// controller decodes it directly, no newline-stripping needed.
		return exec.Command("docker", "run", "--rm", "-v", mount, volumeHelperImage,
			"sh", "-c", `base64 -w0 "$1" 2>&1`, "sh", msg.Path).CombinedOutput()
	case "volume_write":
		// The content travels over stdin, not as a shell argument --
		// an argv-based payload risks the kernel's ARG_MAX for anything
		// but a small file; stdin has no such ceiling. -i keeps the
		// container's stdin open long enough to receive it (docker run
		// closes stdin immediately without it, confirmed while building
		// this -- the file would otherwise always come out empty).
		cmd := exec.Command("docker", "run", "--rm", "-i", "-v", mount, volumeHelperImage,
			"sh", "-c", `base64 -d > "$1"`, "sh", msg.Path)
		cmd.Stdin = strings.NewReader(msg.Data)
		return cmd.CombinedOutput()
	case "volume_rename":
		return exec.Command("docker", "run", "--rm", "-v", mount, volumeHelperImage,
			"sh", "-c", `mv "$1" "$2"`, "sh", msg.Path, msg.NewPath).CombinedOutput()
	case "volume_delete":
		// -- : a filename that happens to start with "-" is still a
		// literal filename, never an rm flag.
		return exec.Command("docker", "run", "--rm", "-v", mount, volumeHelperImage,
			"sh", "-c", `rm -rf -- "$1"`, "sh", msg.Path).CombinedOutput()
	default:
		return nil, fmt.Errorf("unknown volume action %q", msg.Action)
	}
}

// watchDockerEvents streams `docker events` (containers, images, volumes
// and networks -- a create/remove should refresh the networks list the
// same way a start/stop refreshes containers) and signals changed on
// every one. The channel is buffered 1 and a pending signal is left
// alone if one's already queued, since the caller only cares that
// *something* changed, not how many times or of which kind -- one
// send() always refreshes all four snapshots together anyway. Restarts
// the subprocess if it ever exits; stops (and kills the subprocess) when
// ctx is cancelled.
func watchDockerEvents(ctx context.Context, changed chan<- struct{}) {
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "docker", "events",
			"--filter", "type=container", "--filter", "type=image",
			"--filter", "type=volume", "--filter", "type=network", "--format", "{{json .}}")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Println("tunnel: docker events pipe:", err)
			time.Sleep(3 * time.Second)
			continue
		}
		if err := cmd.Start(); err != nil {
			log.Println("tunnel: docker events start:", err)
			time.Sleep(3 * time.Second)
			continue
		}

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case changed <- struct{}{}:
			default:
			}
		}
		cmd.Wait()

		if ctx.Err() == nil {
			log.Println("tunnel: docker events stream ended, restarting in 3s")
			time.Sleep(3 * time.Second)
		}
	}
}

// dockerPsSnapshot runs `docker ps -a --no-trunc`, one JSON object per
// line -- a long-stable Docker CLI format. --no-trunc matters here, not
// just cosmetically: docker ps silently truncates long field values with
// an ellipsis even in JSON format (caught by testing -- a volume name
// like "voltest_vol-data" came back as "voltest_vol-da…", silently
// breaking the exact-string match against host_volumes.name). Without
// it, Mounts (used for the Volumes page's "used by") and Command would
// both be susceptible to the same silent truncation.
// com.docker.compose.project/.service labels (always present on
// anything this agent deployed, since every compose invocation already
// passes -p <stackID>) are parsed out here to link a container back to
// its stack for free, no new convention needed.
func dockerPsSnapshot() ([]containerReport, error) {
	out, err := exec.Command("docker", "ps", "-a", "--no-trunc", "--format", "{{json .}}").Output()
	if err != nil {
		return nil, err
	}

	var reports []containerReport
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw struct {
			ID        string `json:"ID"`
			Names     string `json:"Names"`
			Image     string `json:"Image"`
			State     string `json:"State"`
			Status    string `json:"Status"`
			Ports     string `json:"Ports"`
			CreatedAt string `json:"CreatedAt"`
			Labels    string `json:"Labels"`
			Mounts    string `json:"Mounts"`
			Networks  string `json:"Networks"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue // one malformed line shouldn't drop the whole snapshot
		}
		stackID, service := parseComposeLabels(raw.Labels)
		reports = append(reports, containerReport{
			ID:          raw.ID,
			Name:        raw.Names,
			Image:       raw.Image,
			State:       raw.State,
			Status:      raw.Status,
			Ports:       raw.Ports,
			Created:     raw.CreatedAt,
			StackID:     stackID,
			ServiceName: service,
			Mounts:      splitCommaList(raw.Mounts),
			Networks:    splitCommaList(raw.Networks),
		})
	}
	return reports, scanner.Err()
}

// splitCommaList splits one of docker ps's comma-separated fields
// (Mounts, Networks). Mounts lists named volumes by name -- anonymous
// volumes and bind mounts show up as hashes/paths rather than a usable
// volume name, which is fine here since only named volumes are what the
// Volumes page tracks or could ever link "used by" against.
func splitCommaList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// dockerImagesSnapshot runs `docker images -a`. Plain `docker images`
// (no `-a`) turns out to silently exclude dangling/untagged images on
// a real host -- found by testing: after several rebuilds of the same
// tag, `docker images` reported 50 images while `docker images -a`
// reported 89, the extra 39 all real, addressable images (old builds
// that got un-tagged when the tag moved to a newer build), not the
// flood of untagged intermediate build-step layers `-a` implied on
// older, non-BuildKit Docker -- BuildKit's own build cache lives
// outside `docker images` entirely (`docker system df`/`buildx du`),
// so `-a` here doesn't reintroduce that noise. Without it, the
// Images page could never show what Docker itself calls "<none>" --
// exactly the images a "Clean up unused" button exists to remove.
func dockerImagesSnapshot() ([]imageReport, error) {
	out, err := exec.Command("docker", "images", "-a", "--format", "{{json .}}").Output()
	if err != nil {
		return nil, err
	}

	var reports []imageReport
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw struct {
			ID         string `json:"ID"`
			Repository string `json:"Repository"`
			Tag        string `json:"Tag"`
			Size       string `json:"Size"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue // one malformed line shouldn't drop the whole snapshot
		}
		reports = append(reports, imageReport{
			ID:         raw.ID,
			Repository: raw.Repository,
			Tag:        raw.Tag,
			Size:       raw.Size,
		})
	}
	return reports, scanner.Err()
}

// dockerVolumesSnapshot runs `docker volume ls`. Deliberately not
// `docker system df -v`, which does report real per-volume disk usage --
// but by actually walking each volume's files, ~1.5s+ on a host with a
// meaningful amount of data in this environment, measured while
// designing this. Fine for a one-off inspection, not for something
// re-run on every docker events line. Size is left for a later, more
// targeted chunk (cf. ARCHITECTURE.md) rather than slowing down every
// snapshot for a column most ticks don't need refreshed.
func dockerVolumesSnapshot() ([]volumeReport, error) {
	out, err := exec.Command("docker", "volume", "ls", "--format", "{{json .}}").Output()
	if err != nil {
		return nil, err
	}

	var reports []volumeReport
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw struct {
			Name   string `json:"Name"`
			Driver string `json:"Driver"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue // one malformed line shouldn't drop the whole snapshot
		}
		reports = append(reports, volumeReport{Name: raw.Name, Driver: raw.Driver})
	}
	return reports, scanner.Err()
}

// dockerNetworksSnapshot runs `docker network ls`, same lightweight
// metadata-only approach as containers/images/volumes -- no per-network
// inspect, "which containers use this network" is derived from
// containerReport.Networks instead, same pattern as volume "used by".
func dockerNetworksSnapshot() ([]networkReport, error) {
	out, err := exec.Command("docker", "network", "ls", "--format", "{{json .}}").Output()
	if err != nil {
		return nil, err
	}

	var reports []networkReport
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw struct {
			Name   string `json:"Name"`
			Driver string `json:"Driver"`
			Scope  string `json:"Scope"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue // one malformed line shouldn't drop the whole snapshot
		}
		reports = append(reports, networkReport{Name: raw.Name, Driver: raw.Driver, Scope: raw.Scope})
	}
	return reports, scanner.Err()
}

func parseComposeLabels(labels string) (stackID, service string) {
	for _, kv := range strings.Split(labels, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "com.docker.compose.project":
			stackID = v
		case "com.docker.compose.service":
			service = v
		}
	}
	return stackID, service
}
