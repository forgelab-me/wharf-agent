package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadRepoFile(t *testing.T) {
	dir := t.TempDir()

	if _, found, err := readRepoFile(filepath.Join(dir, "absent.yaml")); found || err != nil {
		t.Fatalf("absent file: found=%v err=%v, want neither", found, err)
	}

	regular := filepath.Join(dir, "secrets.refs.yaml")
	if err := os.WriteFile(regular, []byte("K: ref+sops://secrets.enc.yaml#/K\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, found, err := readRepoFile(regular)
	if !found || err != nil || !strings.HasPrefix(string(data), "K:") {
		t.Fatalf("regular file: data=%q found=%v err=%v", data, found, err)
	}

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("top-secret-marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	data, found, err = readRepoFile(link)
	if found || err == nil || len(data) != 0 {
		t.Fatalf("symlink: data=%q found=%v err=%v, want a refusal", data, found, err)
	}

	big := filepath.Join(dir, "big.yaml")
	if err := os.WriteFile(big, make([]byte, maxSecretsFile+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, found, err := readRepoFile(big); found || err == nil {
		t.Fatalf("oversized file: found=%v err=%v, want a refusal", found, err)
	}
}

func TestResolveViaController(t *testing.T) {
	var got struct {
		DeploymentID, RefsBase64, EncBase64 string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/resolve" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			DeploymentID string `json:"deployment_id"`
			RefsBase64   string `json:"refs_base64"`
			EncBase64    string `json:"enc_base64"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		got.DeploymentID, got.RefsBase64, got.EncBase64 = req.DeploymentID, req.RefsBase64, req.EncBase64
		json.NewEncoder(w).Encode(map[string]any{
			"env":   map[string]string{"API_KEY": "k"},
			"notes": []string{"secrets.enc.yaml keys not referenced (ignored): OTHER"},
		})
	}))
	defer srv.Close()

	env, notes, err := resolveViaController(srv.Client(), srv.URL, "dep1", []byte("refs"), []byte("enc"))
	if err != nil {
		t.Fatal(err)
	}
	if env["API_KEY"] != "k" || len(notes) != 1 {
		t.Fatalf("env=%v notes=%v", env, notes)
	}
	refs, _ := base64.StdEncoding.DecodeString(got.RefsBase64)
	enc, _ := base64.StdEncoding.DecodeString(got.EncBase64)
	if got.DeploymentID != "dep1" || string(refs) != "refs" || string(enc) != "enc" {
		t.Fatalf("request = %+v", got)
	}
}

func TestResolveViaControllerSurfacesRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "1 reference(s) could not be resolved:\n  K (vault://): no provider", http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	env, _, err := resolveViaController(srv.Client(), srv.URL, "dep1", []byte("refs"), nil)
	if err == nil || env != nil {
		t.Fatalf("env=%v err=%v, want a refusal", env, err)
	}
	if !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "K (vault://)") {
		t.Fatalf("err = %v", err)
	}
}
