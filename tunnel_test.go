package main

import "testing"

// parseComposeLabels is what links a container back to its stack on the
// controller side (cf. store.HostContainer.StackID/ServiceName) -- a
// typo in either label key here would silently stop every container
// from ever being recognized as belonging to a stack, without any
// error anywhere to point at why.
func TestParseComposeLabels(t *testing.T) {
	cases := []struct {
		name        string
		labels      string
		wantStackID string
		wantService string
	}{
		{
			name:        "both present",
			labels:      "com.docker.compose.project=pi-hole,com.docker.compose.service=web",
			wantStackID: "pi-hole",
			wantService: "web",
		},
		{
			name:        "unrelated labels mixed in, order doesn't matter",
			labels:      "maintainer=someone,com.docker.compose.service=db,other.label=x,com.docker.compose.project=myapp",
			wantStackID: "myapp",
			wantService: "db",
		},
		{
			name:        "no compose labels at all -- a container Wharf didn't deploy",
			labels:      "maintainer=someone,other.label=x",
			wantStackID: "",
			wantService: "",
		},
		{
			name:        "empty label string",
			labels:      "",
			wantStackID: "",
			wantService: "",
		},
		{
			name:        "malformed entry (no '=') is skipped, not fatal to the rest",
			labels:      "not-a-kv-pair,com.docker.compose.project=orphan",
			wantStackID: "orphan",
			wantService: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStackID, gotService := parseComposeLabels(c.labels)
			if gotStackID != c.wantStackID || gotService != c.wantService {
				t.Errorf("parseComposeLabels(%q) = (%q, %q), want (%q, %q)",
					c.labels, gotStackID, gotService, c.wantStackID, c.wantService)
			}
		})
	}
}

func TestParseImageIDs(t *testing.T) {
	out := "aaa111 sha256:1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b\n" +
		"bbb222 sha256:short\n" +
		"\n" +
		"malformed-line-without-a-space\n"
	got := parseImageIDs(out)
	if got["aaa111"] != "1a2b3c4d5e6f" {
		t.Errorf("aaa111 = %q, want the 12-char image id", got["aaa111"])
	}
	if got["bbb222"] != "short" {
		t.Errorf("bbb222 = %q", got["bbb222"])
	}
	if len(got) != 2 {
		t.Errorf("got %v, want the malformed and blank lines skipped", got)
	}
}

func TestAttachImageIDs(t *testing.T) {
	reports := []containerReport{{ID: "aaa111"}, {ID: "missing"}}
	attachImageIDs(reports, map[string]string{"aaa111": "1a2b3c4d5e6f"})
	if reports[0].ImageID != "1a2b3c4d5e6f" || reports[1].ImageID != "" {
		t.Fatalf("reports = %+v", reports)
	}
	attachImageIDs(reports, nil) // a failed inspect must not panic
}

func TestParseImageLinesDigests(t *testing.T) {
	out := `{"Digest":"sha256:9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0","ID":"1a2b3c4d5e6f","Repository":"grafana/grafana","Size":"245MB","Tag":"11.2.0"}
{"Digest":"\u003cnone\u003e","ID":"7f8e9d0c1b2a","Repository":"wharf-server","Size":"108MB","Tag":"latest"}
not json
{"ID":"3c4d5e6f7a8b","Repository":"\u003cnone\u003e","Size":"50MB","Tag":"\u003cnone\u003e"}
`
	got, err := parseImageLines(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d images, want 3 (the malformed line skipped): %+v", len(got), got)
	}
	if got[0].Digest != "sha256:9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0" {
		t.Errorf("registry digest lost: %q", got[0].Digest)
	}
	if got[1].Digest != "" || got[2].Digest != "" {
		t.Errorf("a locally built image or a missing digest must be empty: %q %q", got[1].Digest, got[2].Digest)
	}
}

func TestNormalizeArch(t *testing.T) {
	for in, want := range map[string]string{
		"x86_64\n": "amd64", "amd64": "amd64", "aarch64": "arm64", "ARM64": "arm64",
		"armv7l": "arm", "riscv64": "riscv64", "": "",
	} {
		if got := normalizeArch(in); got != want {
			t.Errorf("normalizeArch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseImageLinesMergesDigestsOfTheSameImage(t *testing.T) {
	// docker lists an image once per registry digest, and one tag can have two
	out := `{"Digest":"sha256:be80","ID":"1102bfe49106","Repository":"node","Size":"200MB","Tag":"24-alpine"}
{"Digest":"sha256:50c8","ID":"1102bfe49106","Repository":"node","Size":"200MB","Tag":"24-alpine"}
{"Digest":"sha256:be80","ID":"1102bfe49106","Repository":"node","Size":"200MB","Tag":"lts-alpine"}
{"Digest":"\u003cnone\u003e","ID":"1102bfe49106","Repository":"node","Size":"200MB","Tag":"lts-alpine"}
`
	got, err := parseImageLines(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows %+v, want one per (id, repository, tag)", len(got), got)
	}
	if got[0].Tag != "24-alpine" || got[0].Digest != "sha256:50c8" {
		t.Errorf("the smallest digest must win, deterministically: %+v", got[0])
	}
	if got[1].Tag != "lts-alpine" || got[1].Digest != "sha256:be80" {
		t.Errorf("a missing digest must not hide a real one: %+v", got[1])
	}
}
