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
