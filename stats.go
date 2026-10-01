package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxStatsContainers = 200

// containerRef is what a container id or name looks like. It is checked here
// because the references come from the controller: one starting with "-" would
// otherwise be read by docker as an option.
var containerRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// rawStat is what a "stats_many" command answers for one container, one JSON
// object per line. All sizes are bytes, exact: `docker stats` prints them with
// three significant digits, so a total of 331 MB only ever moves by 1 MB, which
// is useless to compute a rate over a few seconds.
type rawStat struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	CPU      float64 `json:"cpu"` // percent of one core
	MemUsage float64 `json:"mem_usage"`
	MemLimit float64 `json:"mem_limit"`
	NetRx    float64 `json:"net_rx"`
	NetTx    float64 `json:"net_tx"`
	BlkRead  float64 `json:"blk_read"`
	BlkWrite float64 `json:"blk_write"`
}

func validateRefs(ids []string) error {
	if len(ids) == 0 {
		return errors.New("no container given")
	}
	if len(ids) > maxStatsContainers {
		return fmt.Errorf("too many containers (%d, at most %d)", len(ids), maxStatsContainers)
	}
	for _, id := range ids {
		if !containerRef.MatchString(id) {
			return fmt.Errorf("invalid container reference %q", id)
		}
	}
	return nil
}

// statsMany measures several containers. It asks the Docker engine directly,
// through the socket the agent already has, for exact counters; if that socket
// cannot be reached (a remote DOCKER_HOST, say) it falls back to the docker
// CLI, whose figures are rounded.
func statsMany(ids []string) ([]byte, error) {
	if err := validateRefs(ids); err != nil {
		return nil, err
	}
	stats, err := engineStats(ids)
	if err != nil {
		log.Println("stats: docker engine API unavailable, using the CLI:", err)
		stats, err = cliStats(ids)
		if err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	for _, s := range stats {
		if err := enc.Encode(s); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

// ---- the engine's API

// dockerSocket is the unix socket of the docker daemon, or "" when the agent
// talks to it some other way.
func dockerSocket() string {
	host := os.Getenv("DOCKER_HOST")
	switch {
	case host == "":
		return "/var/run/docker.sock"
	case strings.HasPrefix(host, "unix://"):
		return strings.TrimPrefix(host, "unix://")
	}
	return ""
}

func unixClient(sock string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
	}
}

func engineStats(ids []string) ([]rawStat, error) {
	sock := dockerSocket()
	if sock == "" {
		return nil, errors.New("DOCKER_HOST is not a unix socket")
	}
	return engineStatsFrom(unixClient(sock), "http://docker", ids)
}

var errEngineUnreachable = errors.New("cannot reach the docker engine")

// engineStatsFrom measures each container with one request to the engine. A
// container that has vanished is skipped; an engine that cannot be reached at
// all is an error, so the caller can fall back.
func engineStatsFrom(client *http.Client, base string, ids []string) ([]rawStat, error) {
	results := make([]*rawStat, len(ids))
	var wg sync.WaitGroup
	var mu sync.Mutex
	unreachable := 0
	sem := make(chan struct{}, 8)
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s, err := engineStatOne(client, base, id)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, errEngineUnreachable):
				unreachable++
			case err == nil:
				results[i] = s
			}
		}()
	}
	wg.Wait()
	if unreachable == len(ids) {
		return nil, errEngineUnreachable
	}
	var out []rawStat
	for _, s := range results {
		if s != nil {
			out = append(out, *s)
		}
	}
	return out, nil
}

type engineStatsBody struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	CPUStats struct {
		CPUUsage struct {
			Total uint64   `json:"total_usage"`
			Per   []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		System uint64 `json:"system_cpu_usage"`
		Online uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			Total uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		System uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		Rx uint64 `json:"rx_bytes"`
		Tx uint64 `json:"tx_bytes"`
	} `json:"networks"`
	Blkio struct {
		IO []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

func engineStatOne(client *http.Client, base, id string) (*rawStat, error) {
	resp, err := client.Get(base + "/containers/" + url.PathEscape(id) + "/stats?stream=false")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errEngineUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("engine answered %s for %s", resp.Status, id)
	}
	var body engineStatsBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return statFromEngine(body), nil
}

// statFromEngine does what the docker CLI does with the engine's raw figures.
func statFromEngine(b engineStatsBody) *rawStat {
	s := &rawStat{ID: b.ID, Name: strings.TrimPrefix(b.Name, "/"), MemLimit: float64(b.MemoryStats.Limit)}

	cpuDelta := float64(b.CPUStats.CPUUsage.Total) - float64(b.PreCPUStats.CPUUsage.Total)
	sysDelta := float64(b.CPUStats.System) - float64(b.PreCPUStats.System)
	cpus := float64(b.CPUStats.Online)
	if cpus == 0 {
		cpus = float64(len(b.CPUStats.CPUUsage.Per))
	}
	// without a previous sample (counters at zero) the deltas mean nothing
	if b.PreCPUStats.System > 0 && cpuDelta > 0 && sysDelta > 0 && cpus > 0 {
		s.CPU = cpuDelta / sysDelta * cpus * 100
	}

	// the cache is not memory a container "uses": the CLI takes it off
	usage := b.MemoryStats.Usage
	cache := b.MemoryStats.Stats["inactive_file"] // cgroup v2
	if v, ok := b.MemoryStats.Stats["total_inactive_file"]; ok {
		cache = v // cgroup v1
	}
	if cache < usage {
		usage -= cache
	}
	s.MemUsage = float64(usage)

	for _, n := range b.Networks {
		s.NetRx += float64(n.Rx)
		s.NetTx += float64(n.Tx)
	}
	for _, e := range b.Blkio.IO {
		switch strings.ToLower(e.Op) {
		case "read":
			s.BlkRead += float64(e.Value)
		case "write":
			s.BlkWrite += float64(e.Value)
		}
	}
	return s
}

// ---- the CLI fallback

// cliLine is one line of `docker stats --format {{json .}}`.
type cliLine struct {
	Container string `json:"Container"`
	Name      string `json:"Name"`
	CPUPerc   string `json:"CPUPerc"`
	MemUsage  string `json:"MemUsage"`
	NetIO     string `json:"NetIO"`
	BlockIO   string `json:"BlockIO"`
}

func statsArgs(ids []string) []string {
	return append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, ids...)
}

// cliStats measures with the docker CLI. A container that vanished since the
// controller's last snapshot makes docker exit non-zero while the others are
// still printed; that output is kept, or one removed container would blind the
// whole list.
func cliStats(ids []string) ([]rawStat, error) {
	out, err := exec.Command("docker", statsArgs(ids)...).CombinedOutput()
	stats := parseCLIStats(out)
	if err != nil && len(stats) == 0 {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return stats, nil
}

func parseCLIStats(out []byte) []rawStat {
	var stats []rawStat
	for _, line := range bytes.Split(out, []byte("\n")) {
		var l cliLine
		if json.Unmarshal(bytes.TrimSpace(line), &l) != nil || l.Container == "" {
			continue
		}
		used, limit, _ := strings.Cut(l.MemUsage, " / ")
		rx, tx, _ := strings.Cut(l.NetIO, " / ")
		rd, wr, _ := strings.Cut(l.BlockIO, " / ")
		cpu, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(l.CPUPerc), "%"), 64)
		stats = append(stats, rawStat{
			ID: l.Container, Name: l.Name, CPU: cpu,
			MemUsage: cliBytes(used), MemLimit: cliBytes(limit),
			NetRx: cliBytes(rx), NetTx: cliBytes(tx), BlkRead: cliBytes(rd), BlkWrite: cliBytes(wr),
		})
	}
	return stats
}

var cliSizeRe = regexp.MustCompile(`^([\d.]+)\s*([a-zA-Z]*)$`)

var cliUnits = map[string]float64{
	"": 1, "b": 1,
	"kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12, // network and disk: decimal
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40, // memory: binary
}

// cliBytes reads one of the CLI's rounded sizes ("331MB", "97.1MiB").
func cliBytes(s string) float64 {
	m := cliSizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	return v * cliUnits[strings.ToLower(m[2])]
}
