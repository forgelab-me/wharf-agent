package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseLabelText(t *testing.T) {
	got := parseLabelText("com.docker.compose.project=monitoring,com.docker.compose.volume=grafana-data,wharf.backup=nightly")
	if len(got) != 3 || got["com.docker.compose.project"] != "monitoring" || got["wharf.backup"] != "nightly" {
		t.Errorf("labels = %v", got)
	}
	// a comma inside a value belongs to that value
	got = parseLabelText("note=a,b,c,wharf.backup=nightly")
	if got["note"] != "a,b,c" || got["wharf.backup"] != "nightly" || len(got) != 2 {
		t.Errorf("a comma in a value: %v", got)
	}
	// an equals sign in a value stays in it
	if got := parseLabelText("expr=a=b"); got["expr"] != "a=b" {
		t.Errorf("equals in a value: %v", got)
	}
	if parseLabelText("") != nil || parseLabelText("  ") != nil {
		t.Error("no labels is nil")
	}
}

func TestEngineVolumesReadsTheLabelsAsADictionary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/volumes" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"Volumes":[
			{"Name":"monitoring_grafana-data","Driver":"local","Labels":{"com.docker.compose.project":"monitoring","note":"a,b"}},
			{"Name":"plain","Driver":"local","Labels":null}],"Warnings":null}`))
	}))
	defer srv.Close()
	vols, err := engineVolumes(context.Background(), srv.Client(), srv.URL)
	if err != nil || len(vols) != 2 {
		t.Fatalf("volumes = %+v %v", vols, err)
	}
	if vols[0].Labels["com.docker.compose.project"] != "monitoring" || vols[0].Labels["note"] != "a,b" {
		t.Errorf("labels = %v", vols[0].Labels)
	}
	if vols[1].Labels != nil {
		t.Errorf("a volume without labels has none: %v", vols[1].Labels)
	}
	// null volumes, as docker answers on a host without any
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"Volumes":null}`)) }))
	defer empty.Close()
	if vols, err := engineVolumes(context.Background(), empty.Client(), empty.URL); err != nil || len(vols) != 0 {
		t.Errorf("no volumes: %v %v", vols, err)
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }))
	defer failing.Close()
	if _, err := engineVolumes(context.Background(), failing.Client(), failing.URL); err == nil {
		t.Error("an engine error falls back to the CLI")
	}
}

func TestCapLabels(t *testing.T) {
	in := map[string]string{"ok": "v", "long": strings.Repeat("x", 1000), strings.Repeat("k", 200): "dropped"}
	out := capLabels(in)
	if out["ok"] != "v" || len(out["long"]) != maxVolumeLabelValue || len(out) != 2 {
		t.Errorf("capped = %v", out)
	}
	many := map[string]string{}
	for i := 0; i < 200; i++ {
		many[string(rune('a'+i%26))+strings.Repeat("z", i/26)] = "v"
	}
	if len(capLabels(many)) != maxVolumeLabels {
		t.Errorf("at most %d labels", maxVolumeLabels)
	}
	if capLabels(nil) != nil || capLabels(map[string]string{}) != nil {
		t.Error("nothing to carry")
	}
}
