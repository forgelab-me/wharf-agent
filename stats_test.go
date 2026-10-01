package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateRefsRefusesWhatDockerCouldReadAsAnOption(t *testing.T) {
	if err := validateRefs([]string{"abc123", "blog-web-1", "my_app.v2"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"-a", "--all", "a b", "", "../x", "id;rm -rf", "$(x)", "x\n", ".hidden", "_x"} {
		if err := validateRefs([]string{"ok", bad}); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestValidateRefsBounds(t *testing.T) {
	if err := validateRefs(nil); err == nil {
		t.Error("an empty list is refused")
	}
	var ids []string
	for i := 0; i < maxStatsContainers; i++ {
		ids = append(ids, fmt.Sprint("c", i))
	}
	if err := validateRefs(ids); err != nil {
		t.Errorf("the limit itself is allowed: %v", err)
	}
	if err := validateRefs(append(ids, "one-more")); err == nil {
		t.Error("one more than the limit is refused")
	}
}

func TestStatsArgs(t *testing.T) {
	if got := strings.Join(statsArgs([]string{"a", "b"}), " "); got != "stats --no-stream --format {{json .}} a b" {
		t.Errorf("args = %q", got)
	}
}

// what the engine answers on cgroup v2: the cache is "inactive_file", the
// operations of the block I/O are lowercase.
const engineV2 = `{"id":"aaaaaaaaaaaa1111","name":"/blog-web-1",
"cpu_stats":{"cpu_usage":{"total_usage":2000000000},"system_cpu_usage":20000000000,"online_cpus":4},
"precpu_stats":{"cpu_usage":{"total_usage":1900000000},"system_cpu_usage":19000000000},
"memory_stats":{"usage":300000000,"limit":8000000000,"stats":{"inactive_file":50000000,"file":60000000}},
"networks":{"eth0":{"rx_bytes":1000,"tx_bytes":500},"eth1":{"rx_bytes":24,"tx_bytes":12}},
"blkio_stats":{"io_service_bytes_recursive":[
 {"major":8,"minor":0,"op":"read","value":4096},{"major":8,"minor":0,"op":"write","value":8192},
 {"major":8,"minor":16,"op":"read","value":100},{"major":8,"minor":0,"op":"total","value":99999}]}}`

// and on cgroup v1: "total_inactive_file", capitalised operations, no online_cpus
const engineV1 = `{"id":"bbbbbbbbbbbb2222","name":"/blog-db-1",
"cpu_stats":{"cpu_usage":{"total_usage":500,"percpu_usage":[1,2]},"system_cpu_usage":2000},
"precpu_stats":{"cpu_usage":{"total_usage":400},"system_cpu_usage":1000},
"memory_stats":{"usage":1000,"limit":5000,"stats":{"total_inactive_file":200,"inactive_file":999}},
"networks":{},
"blkio_stats":{"io_service_bytes_recursive":[{"op":"Read","value":7},{"op":"Write","value":9},{"op":"Sync","value":16}]}}`

func decode(t *testing.T, raw string) *rawStat {
	t.Helper()
	var b engineStatsBody
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatal(err)
	}
	return statFromEngine(b)
}

func TestStatFromEngineCgroupV2(t *testing.T) {
	s := decode(t, engineV2)
	if s.ID != "aaaaaaaaaaaa1111" || s.Name != "blog-web-1" {
		t.Errorf("id/name: %+v", s)
	}
	if s.CPU != 40 {
		t.Errorf("(2.0-1.9)/(20-19) * 4 cpus * 100 = 40, got %v", s.CPU)
	}
	if s.MemUsage != 250000000 || s.MemLimit != 8000000000 {
		t.Errorf("the cache is taken off the memory in use: %+v", s)
	}
	if s.NetRx != 1024 || s.NetTx != 512 {
		t.Errorf("network is summed over the interfaces: %+v", s)
	}
	if s.BlkRead != 4196 || s.BlkWrite != 8192 {
		t.Errorf("block I/O ignores the totals and sums the devices: %+v", s)
	}
}

func TestStatFromEngineCgroupV1(t *testing.T) {
	s := decode(t, engineV1)
	if s.CPU != 20 { // (500-400)/(2000-1000) * 2 cpus (from percpu_usage) * 100
		t.Errorf("cpu = %v", s.CPU)
	}
	if s.MemUsage != 800 {
		t.Errorf("v1 uses total_inactive_file, got %v", s.MemUsage)
	}
	if s.BlkRead != 7 || s.BlkWrite != 9 || s.NetRx != 0 {
		t.Errorf("%+v", s)
	}
}

func TestStatFromEngineWithoutAPreviousSample(t *testing.T) {
	s := decode(t, `{"id":"x","name":"/x","cpu_stats":{"cpu_usage":{"total_usage":5},"system_cpu_usage":10,"online_cpus":2},"precpu_stats":{},"memory_stats":{"usage":10,"limit":100}}`)
	if s.CPU != 0 || s.MemUsage != 10 {
		t.Errorf("no previous sample: no CPU figure, not a crash: %+v", s)
	}
}

func engineServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/aaaaaaaaaaaa1111/stats":
			if r.URL.Query().Get("stream") != "false" {
				t.Errorf("one sample is asked for, not a stream: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, engineV2)
		case "/containers/bbbbbbbbbbbb2222/stats":
			fmt.Fprint(w, engineV1)
		default:
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEngineStatsFromKeepsTheOrderAndSkipsWhatVanished(t *testing.T) {
	srv := engineServer(t)
	got, err := engineStatsFrom(srv.Client(), srv.URL, []string{"bbbbbbbbbbbb2222", "gone", "aaaaaaaaaaaa1111"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "blog-db-1" || got[1].Name != "blog-web-1" {
		t.Errorf("a vanished container is left out, the others keep their order: %+v", got)
	}

	got, err = engineStatsFrom(srv.Client(), srv.URL, []string{"gone", "also-gone"})
	if err != nil || len(got) != 0 {
		t.Errorf("an engine that answers but knows none of them is not an error: %v %v", got, err)
	}
}

func TestEngineStatsFromReportsAnEngineThatCannotBeReached(t *testing.T) {
	srv := engineServer(t)
	client, base := srv.Client(), srv.URL
	srv.Close()
	if _, err := engineStatsFrom(client, base, []string{"aaaaaaaaaaaa1111", "bbbbbbbbbbbb2222"}); err == nil {
		t.Error("nothing could be reached: the caller must be told, so that it falls back")
	}
}

func TestStatsManyFallsBackToTheCLIWhenTheSocketIsNotAUnixOne(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.1:2375")
	if sock := dockerSocket(); sock != "" {
		t.Errorf("socket = %q", sock)
	}
	if _, err := engineStats([]string{"abc"}); err == nil {
		t.Error("a remote docker host is not read through a socket")
	}
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	if sock := dockerSocket(); sock != "/run/user/1000/docker.sock" {
		t.Errorf("socket = %q", sock)
	}
	t.Setenv("DOCKER_HOST", "")
	if sock := dockerSocket(); sock != "/var/run/docker.sock" {
		t.Errorf("default socket = %q", sock)
	}
}

func TestParseCLIStats(t *testing.T) {
	out := []byte(strings.Join([]string{
		`{"Container":"cace491b6955","Name":"grafana","CPUPerc":"0.92%","MemUsage":"97.1MiB / 7.8GiB","NetIO":"331MB / 1.5kB","BlockIO":"128MB / 0B"}`,
		`Error response from daemon: No such container: gone`,
		`{"Container":"1809f7cd0c75","Name":"prometheus","CPUPerc":"--","MemUsage":"0B / 0B","NetIO":"0B / 0B","BlockIO":"0B / 0B"}`,
		``,
	}, "\n"))
	got := parseCLIStats(out)
	if len(got) != 2 {
		t.Fatalf("the error line is skipped, both containers kept: %+v", got)
	}
	g := got[0]
	if g.ID != "cace491b6955" || g.CPU != 0.92 || g.MemUsage != 97.1*1048576 || g.MemLimit != 7.8*1073741824 ||
		g.NetRx != 331e6 || g.NetTx != 1500 || g.BlkRead != 128e6 || g.BlkWrite != 0 {
		t.Errorf("rounded figures, in the right units: %+v", g)
	}
	if got[1].CPU != 0 {
		t.Errorf("a stopped container prints -- for its CPU: %+v", got[1])
	}
}

func TestCLIBytes(t *testing.T) {
	for in, want := range map[string]float64{"0B": 0, "512B": 512, "1kB": 1000, "1KiB": 1024, "2MB": 2e6, "3GiB": 3 * 1073741824, "--": 0, "": 0} {
		if got := cliBytes(in); got != want {
			t.Errorf("cliBytes(%q) = %v, want %v", in, got, want)
		}
	}
}
