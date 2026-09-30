package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

const (
	refsFileName    = "secrets.refs.yaml"
	encFileName     = "secrets.enc.yaml"
	maxSecretsFile  = 1 << 20
	maxResolveReply = 1 << 20
)

// readRepoFile reads a secrets file from the clone. Only a regular file
// counts: a symlink (which could point outside the clone) is refused.
func readRepoFile(path string) (data []byte, found bool, err error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file", info.Name())
	}
	if info.Size() > maxSecretsFile {
		return nil, false, fmt.Errorf("%s is larger than %d KiB", info.Name(), maxSecretsFile>>10)
	}
	data, err = os.ReadFile(path)
	return data, err == nil, err
}

// resolveViaController relays secrets.refs.yaml (and secrets.enc.yaml, if
// any) to the controller, which resolves every reference and returns the
// environment to deploy with. Scoped by deployment_id like decryptViaController.
func resolveViaController(client *http.Client, controllerURL, deploymentID string, refs, enc []byte) (env map[string]string, notes []string, err error) {
	body, err := json.Marshal(struct {
		DeploymentID string `json:"deployment_id"`
		RefsBase64   string `json:"refs_base64"`
		EncBase64    string `json:"enc_base64"`
	}{
		DeploymentID: deploymentID,
		RefsBase64:   base64.StdEncoding.EncodeToString(refs),
		EncBase64:    base64.StdEncoding.EncodeToString(enc),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal resolve request: %w", err)
	}

	resp, err := client.Post(controllerURL+"/agent/resolve", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("POST /agent/resolve: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		return nil, nil, fmt.Errorf("controller refused (%d): %s", resp.StatusCode, b)
	}

	var out struct {
		Env   map[string]string `json:"env"`
		Notes []string          `json:"notes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResolveReply)).Decode(&out); err != nil {
		return nil, nil, fmt.Errorf("decode resolve response: %w", err)
	}
	return out.Env, out.Notes, nil
}
