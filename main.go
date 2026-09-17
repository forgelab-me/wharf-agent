// wharf-agent — enrolls with the controller over a self-signed, fingerprint
// pinned mTLS connection ("connect-first, approve-later") and, once
// approved, polls for and executes deploy commands: local (non-Git)
// stacks run directly, Git stacks are cloned with a transient deploy key
// first. Cf. ARCHITECTURE.md, "Enrôlement d'un nouvel agent" and "Flow de
// déploiement".
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/forgelab-me/wharf-agent/internal/identity"
	"gopkg.in/yaml.v3"
)

const stacksDir = "/opt/wharf-agent/stacks"

// version is overridden at build time via
// -ldflags "-X main.version=$VERSION" (cf. Dockerfile) -- CI sets it from
// the git tag on a release build, a branch+sha otherwise. "dev" is what
// anyone building straight from source without that flag actually sees.
var version = "dev"

type enrollResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type registryAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type commandResponse struct {
	DeploymentID   string                  `json:"deployment_id"`
	StackID        string                  `json:"stack_id"`
	SourceType     string                  `json:"source_type"`
	Action         string                  `json:"action"` // "up" | "down"
	ComposeContent string                  `json:"compose_content"`
	Env            map[string]string       `json:"env"`
	RepoURL        string                  `json:"repo_url"`
	Branch         string                  `json:"branch"`
	ComposePath    string                  `json:"compose_path"`
	AuthKind       string                  `json:"auth_kind"`
	SSHPrivateKey  string                  `json:"ssh_private_key"`
	HTTPUsername   string                  `json:"http_username"`
	HTTPPassword   string                  `json:"http_password"`
	RegistryAuths  map[string]registryAuth `json:"registry_auths"`
}

func main() {
	log.Println("wharf-agent", version)
	controllerURL := flag.String("controller", "", "controller agent endpoint, e.g. https://wharf.example.internal:8443")
	expectedFingerprint := flag.String("controller-fingerprint", "", "expected sha256 fingerprint of the controller's certificate (recommended; TOFU-only if omitted)")
	identityDir := flag.String("identity-dir", "/var/lib/wharf-agent/identity", "directory holding this agent's persistent TLS identity")
	flag.Parse()

	if *controllerURL == "" {
		log.Fatal("--controller is required")
	}

	cert, err := identity.LoadOrGenerate(*identityDir)
	if err != nil {
		log.Fatal("identity: ", err)
	}
	log.Println("agent identity fingerprint:", identity.Fingerprint(cert.Certificate[0]))

	if *expectedFingerprint == "" {
		log.Println("WARNING: no --controller-fingerprint given, trusting the controller's certificate on first sight (TOFU) with no verification afterwards. Fine for local testing, not for anything reachable over an untrusted network.")
	}

	client := newPinnedClient(cert, *expectedFingerprint)

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "wharf-agent"
	}

	idFile := filepath.Join(*identityDir, "host_id")

	for {
		id, err := loadOrEnroll(client, *controllerURL, idFile, hostname)
		if err != nil {
			log.Println("enrollment failed, retrying in 10s:", err)
			time.Sleep(10 * time.Second)
			continue
		}
		pollLoop(client, *controllerURL, idFile, id)
		// pollLoop only returns on a 404 (controller no longer knows this
		// id) — loop back around to re-enroll.
	}
}

// loadOrEnroll returns the cached host id if one exists on disk, or
// enrolls once and caches the id the controller assigns. The identity
// certificate itself now travels as the TLS client certificate (see
// newPinnedClient) rather than a JSON field — the controller derives the
// fingerprint from the connection itself, not from a claim in the body.
func loadOrEnroll(client *http.Client, controllerURL, idFile, hostname string) (string, error) {
	if b, err := os.ReadFile(idFile); err == nil {
		return strings.TrimSpace(string(b)), nil
	}

	body, err := json.Marshal(struct {
		Hostname string `json:"hostname"`
	}{Hostname: hostname})
	if err != nil {
		return "", fmt.Errorf("marshal enrollment request: %w", err)
	}

	resp, err := client.Post(controllerURL+"/agent/enrollments", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("POST /agent/enrollments: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("enrollment rejected (%d): %s", resp.StatusCode, string(b))
	}

	var out enrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode enrollment response: %w", err)
	}

	if err := os.WriteFile(idFile, []byte(out.ID), 0o600); err != nil {
		return "", fmt.Errorf("cache host id: %w", err)
	}

	log.Println("enrolled, id:", out.ID, "status:", out.Status)
	return out.ID, nil
}

// pollLoop polls status every 10s and, once connected, also polls for a
// pending deploy command on the same tick. It only returns when the
// controller responds 404 to the status check (id no longer known, e.g.
// its database was reset) — the caller re-enrolls from scratch.
func pollLoop(client *http.Client, controllerURL, idFile, id string) {
	lastStatus := ""
	tunnelStarted := false
	for {
		time.Sleep(10 * time.Second)

		resp, err := client.Get(controllerURL + "/agent/enrollments/" + id)
		if err != nil {
			log.Println("status check failed:", err)
			continue
		}

		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			log.Println("controller no longer recognizes this host id — re-enrolling")
			os.Remove(idFile)
			return
		}

		var out enrollResponse
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil {
			log.Println("decode status response:", err)
			continue
		}

		if out.Status != lastStatus {
			log.Println("status:", out.Status)
			lastStatus = out.Status
		}

		if out.Status == "connected" {
			checkAndRunCommand(client, controllerURL)
			if !tunnelStarted {
				tunnelStarted = true
				go runHostStateTunnel(client, controllerURL)
			}
		}
	}
}

// checkAndRunCommand polls for one pending deploy command and, if there is
// one, runs it and reports the result. Cf. ARCHITECTURE.md, "Flow de
// déploiement" — Option C, no ephemeral container: this process runs
// `docker compose` directly.
func checkAndRunCommand(client *http.Client, controllerURL string) {
	resp, err := client.Get(controllerURL + "/agent/commands")
	if err != nil {
		log.Println("command check failed:", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		log.Println("command check error:", resp.StatusCode, string(b))
		return
	}

	var cmd commandResponse
	if err := json.NewDecoder(resp.Body).Decode(&cmd); err != nil {
		log.Println("decode command:", err)
		return
	}

	var status, output, composeContent string
	switch {
	case cmd.Action == "down":
		log.Println("undeploying stack", cmd.StackID, "(deployment", cmd.DeploymentID+")")
		status, output = runComposeDown(cmd)
	case cmd.SourceType == "git":
		log.Println("deploying stack", cmd.StackID, "(deployment", cmd.DeploymentID+")")
		status, output, composeContent = runGitDeploy(client, controllerURL, cmd)
	default:
		log.Println("deploying stack", cmd.StackID, "(deployment", cmd.DeploymentID+")")
		status, output, composeContent = runLocalDeploy(cmd)
	}
	log.Println("deployment", cmd.DeploymentID, status)
	reportResult(client, controllerURL, cmd.DeploymentID, status, output, composeContent)
}

// runComposeDown tears down whatever is already on disk from the last
// deploy under stacksDir/<stack_id>/ -- never re-clones, never fetches a
// credential (cf. the plan for this chunk: a revoked deploy key is a
// reason to undeploy, not a blocker to it). Local stacks always have
// their compose file rewritten first since cmd.ComposeContent is cheap to
// carry and covers the case where the on-disk copy was somehow lost;
// Git stacks use whatever compose file is left from the last successful
// clone (the directory persists until the *next* deploy's cleanup).
func runComposeDown(cmd commandResponse) (status, output string) {
	dir := filepath.Join(stacksDir, cmd.StackID)
	composeFile := "compose.yaml"
	if cmd.SourceType == "git" {
		composeFile = cmd.ComposePath
	} else if cmd.ComposeContent != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "failed", "create stack directory: " + err.Error()
		}
		if err := os.WriteFile(filepath.Join(dir, composeFile), []byte(cmd.ComposeContent), 0o644); err != nil {
			return "failed", "write compose file: " + err.Error()
		}
	}

	if _, err := os.Stat(filepath.Join(dir, composeFile)); err != nil {
		return "succeeded", "nothing local found for this stack on this host — nothing to tear down"
	}

	return runComposeDownCmd(dir, composeFile, cmd.StackID)
}

// runLocalDeploy writes the compose file, a transient ".env" covering
// `${KEY}` substitution and any `environment:`-sourced secret (deleted
// right after `docker compose up`, cf. removeEnvFile -- it only has to
// exist for that one invocation), and a secrets/<KEY> file for whichever
// keys the compose file actually consumes via `secrets: <name>: file:
// ./secrets/<KEY>` (cf. writeSecretFiles) -- a key never referenced that
// way gets no file at all, so nothing sits on disk for a secret nothing
// uses.
func runLocalDeploy(cmd commandResponse) (status, output, composeContent string) {
	dir := filepath.Join(stacksDir, cmd.StackID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "failed", "create stack directory: " + err.Error(), ""
	}

	composePath := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(cmd.ComposeContent), 0o644); err != nil {
		return "failed", "write compose file: " + err.Error(), ""
	}

	if err := writeEnvFile(dir, cmd.Env); err != nil {
		return "failed", err.Error(), ""
	}
	if err := writeSecretFiles(dir, cmd.Env, cmd.ComposeContent); err != nil {
		return "failed", err.Error(), ""
	}

	status, output = runComposeUp(dir, "compose.yaml", cmd.StackID, cmd.RegistryAuths)
	output += removeEnvFile(dir)
	return status, output, cmd.ComposeContent
}

// cloneRepo clones cmd.RepoURL into dir, branching on cmd.AuthKind. Both
// paths receive their credential transiently in cmd (never persisted
// beyond this call) — per ARCHITECTURE.md's documented "v1 simple"
// tradeoff for SSH, and the equivalent for HTTP: a real Git host, not
// Wharf, minted the credential in the first place.
func cloneRepo(cmd commandResponse, dir string) (output string, err error) {
	gitArgs := []string{}
	env := os.Environ()

	switch cmd.AuthKind {
	case "http_password":
		// Never embedded in the remote URL: that leaks into git's own
		// error messages and process listings more readily than a header.
		auth := base64.StdEncoding.EncodeToString([]byte(cmd.HTTPUsername + ":" + cmd.HTTPPassword))
		gitArgs = append(gitArgs, "-c", "http.extraHeader=Authorization: Basic "+auth)
	default: // "ssh_key"
		keyFile, err := os.CreateTemp("", "wharf-deploy-key-*")
		if err != nil {
			return "", fmt.Errorf("create temp key file: %w", err)
		}
		keyPath := keyFile.Name()
		defer os.Remove(keyPath) // never persisted beyond this function
		if err := keyFile.Chmod(0o600); err != nil {
			keyFile.Close()
			return "", fmt.Errorf("chmod temp key file: %w", err)
		}
		if _, err := keyFile.WriteString(cmd.SSHPrivateKey); err != nil {
			keyFile.Close()
			return "", fmt.Errorf("write temp key file: %w", err)
		}
		keyFile.Close()

		// No host-key verification -- a real, separate gap from the mTLS
		// pinning used for the agent<->controller channel. Documented,
		// not hidden; cf. ARCHITECTURE.md.
		sshCommand := fmt.Sprintf("ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null", keyPath)
		env = append(env, "GIT_SSH_COMMAND="+sshCommand)
	}

	gitArgs = append(gitArgs, "clone", "--depth", "1", "--branch", cmd.Branch, cmd.RepoURL, dir)
	cloneCmd := exec.Command("git", gitArgs...)
	cloneCmd.Env = env

	out, err := cloneCmd.CombinedOutput()
	return string(out), err
}

// runGitDeploy clones the stack's repo (cf. cloneRepo), looks for
// secrets.enc.yaml next to the compose file and, if present, relays the
// ciphertext to the controller for decryption (the agent is the one that
// discovers it here, unlike the local-stack flow).
func runGitDeploy(client *http.Client, controllerURL string, cmd commandResponse) (status, output, composeContent string) {
	dir := filepath.Join(stacksDir, cmd.StackID)
	// Re-clone from scratch every deploy -- simplest correct thing, cf.
	// plan; revisit if clone time becomes a real problem.
	if err := os.RemoveAll(dir); err != nil {
		return "failed", "clear previous clone: " + err.Error(), ""
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "failed", "create stacks directory: " + err.Error(), ""
	}

	if out, err := cloneRepo(cmd, dir); err != nil {
		return "failed", "git clone: " + out + "\n" + err.Error(), ""
	}

	composeFullPath := filepath.Join(dir, cmd.ComposePath)
	composeBytes, err := os.ReadFile(composeFullPath)
	if err != nil {
		return "failed", "read compose file: " + err.Error(), ""
	}
	composeContent = string(composeBytes)

	secretsPath := filepath.Join(filepath.Dir(composeFullPath), "secrets.enc.yaml")
	if ciphertext, err := os.ReadFile(secretsPath); err == nil {
		plaintext, err := decryptViaController(client, controllerURL, cmd.DeploymentID, ciphertext)
		if err != nil {
			return "failed", "decrypt secrets.enc.yaml: " + err.Error(), composeContent
		}
		env := parseEnvLines(string(plaintext))
		if err := writeEnvFile(dir, env); err != nil {
			return "failed", err.Error(), composeContent
		}
		if err := writeSecretFiles(dir, env, composeContent); err != nil {
			return "failed", err.Error(), composeContent
		}
	}

	status, output = runComposeUp(dir, cmd.ComposePath, cmd.StackID, cmd.RegistryAuths)
	output += removeEnvFile(dir)
	return status, output, composeContent
}

// decryptViaController relays a Git stack's secrets.enc.yaml ciphertext to
// the controller — cf. ARCHITECTURE.md's DecryptRequest/DecryptResponse.
// Scoped by deployment_id: the controller verifies this agent owns that
// deployment before decrypting anything.
func decryptViaController(client *http.Client, controllerURL, deploymentID string, ciphertext []byte) ([]byte, error) {
	body, err := json.Marshal(struct {
		DeploymentID     string `json:"deployment_id"`
		CiphertextBase64 string `json:"ciphertext_base64"`
	}{DeploymentID: deploymentID, CiphertextBase64: base64.StdEncoding.EncodeToString(ciphertext)})
	if err != nil {
		return nil, fmt.Errorf("marshal decrypt request: %w", err)
	}

	resp, err := client.Post(controllerURL+"/agent/decrypt", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("POST /agent/decrypt: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("decrypt rejected (%d): %s", resp.StatusCode, string(b))
	}

	var out struct {
		PlaintextBase64 string `json:"plaintext_base64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode decrypt response: %w", err)
	}
	return base64.StdEncoding.DecodeString(out.PlaintextBase64)
}

// parseEnvLines parses "KEY=value" lines (blank lines and #-comments
// skipped), mirroring the controller's own parser for the local-stack
// path.
func parseEnvLines(s string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			env[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return env
}

// writeEnvFile always reconciles dir/.env to exactly env, current deploy's
// full set -- never a merge with whatever was already there. A Git stack
// gets this for free (runGitDeploy wipes dir before every clone), but a
// local stack's dir persists across deploys, so removing (or renaming) a
// secret has to actively delete the stale file rather than just not
// re-adding it -- otherwise a value removed from the UI keeps sitting in
// plaintext on the host indefinitely. cf. writeSecretFiles, same fix.
func writeEnvFile(dir string, env map[string]string) error {
	envPath := filepath.Join(dir, ".env")
	if len(env) == 0 {
		if err := os.Remove(envPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale .env: %w", err)
		}
		return nil
	}
	var b strings.Builder
	for k, v := range env {
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	// 0600: the only other reader that matters is docker compose itself,
	// reading this same file from disk at `up` time.
	if err := os.WriteFile(envPath, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write .env: %w", err)
	}
	return nil
}

// removeEnvFile deletes dir/.env right after the `docker compose up` call
// that needed it -- .env only has to exist *during* that one invocation,
// for `${KEY}` substitution and any `environment:`-sourced secret, both of
// which Compose reads once at that point and bakes into the container's
// own config; nothing about a later restart, `docker compose down`, or
// `docker inspect` depends on .env still being there (verified directly:
// a container using an environment:-sourced secret survives `docker
// restart` fine even with .env already gone, and `down` still works,
// warning-but-not-failing on any leftover `${KEY}` in the compose file).
// Leaving it around after this point would be a plaintext copy of every
// secret serving no purpose -- unlike secrets/<KEY> (cf. writeSecretFiles),
// which a file:-sourced secret's bind mount genuinely needs to keep
// working across restarts. Best-effort: a failed cleanup doesn't undo an
// already-successful deploy, but it's worth surfacing in the log.
func removeEnvFile(dir string) string {
	if err := os.Remove(filepath.Join(dir, ".env")); err != nil && !os.IsNotExist(err) {
		return "\nwarning: failed to remove .env after deploy: " + err.Error()
	}
	return ""
}

// composeSecretsDoc is only ever used to read the top-level "secrets:"
// mapping back out of a stack's own compose file -- every other field is
// ignored, so a compose file this can't fully parse (anchors aside) still
// works fine here as long as that one section is well-formed.
type composeSecretsDoc struct {
	Secrets map[string]struct {
		File string `yaml:"file"`
	} `yaml:"secrets"`
}

// fileSecretKeys returns the set of secret keys a compose file actually
// references via Compose's file-based form ("secrets: <name>: file:
// ./secrets/<KEY>") -- the only form that needs a file on disk at all.
// A key sourced via "environment:" or plain "${KEY}" substitution never
// appears here, even if it's one of the stack's configured secrets:
// nothing in the compose file ever reads it from a file, so writing one
// would just be a plaintext copy of a value nothing uses. Malformed
// compose content yields an empty set rather than an error -- `docker
// compose up` right after this is already the real validator for the
// compose file itself.
func fileSecretKeys(composeContent string) map[string]bool {
	var doc composeSecretsDoc
	if err := yaml.Unmarshal([]byte(composeContent), &doc); err != nil {
		return nil
	}
	used := map[string]bool{}
	for _, secret := range doc.Secrets {
		if secret.File == "" {
			continue
		}
		used[filepath.Base(secret.File)] = true
	}
	return used
}

// writeSecretFiles writes one file under dir/secrets/ for each key the
// compose file actually consumes via Compose's file-based secrets
// convention (cf. fileSecretKeys) -- bind-mounted at /run/secrets/<name>
// in the container; this works outside Swarm too, it's plain Compose.
// A key the compose file sources some other way (environment:, ${KEY})
// gets no file here at all: there's nothing to gain and a real exposure
// to lose in writing an unused secret's plaintext to disk. dir is the
// same project directory `docker compose` runs from (cf. runCompose's
// execCmd.Dir), so a compose file's relative "./secrets/x" resolves the
// same way "./.env" already does.
//
// secretsDir is fully wiped before rewriting, not merged -- cf.
// writeEnvFile's comment: a local stack's dir isn't recreated from
// scratch on every deploy, so a key removed, renamed, or switched away
// from file: sourcing since the last deploy would otherwise leave its
// old plaintext file behind forever.
func writeSecretFiles(dir string, env map[string]string, composeContent string) error {
	secretsDir := filepath.Join(dir, "secrets")
	if err := os.RemoveAll(secretsDir); err != nil {
		return fmt.Errorf("clear stale secrets directory: %w", err)
	}
	used := fileSecretKeys(composeContent)
	toWrite := map[string]string{}
	for key, value := range env {
		if used[key] {
			toWrite[key] = value
		}
	}
	if len(toWrite) == 0 {
		return nil
	}
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		return fmt.Errorf("create secrets directory: %w", err)
	}
	for key, value := range toWrite {
		// Keys come from the stack's own secrets.enc.yaml/form -- the
		// same trust level as the compose file it's paired with -- but a
		// stray "/" would still write outside secretsDir by accident, so
		// it's rejected rather than silently escaping.
		if strings.ContainsAny(key, "/\\") {
			return fmt.Errorf("invalid secret key %q: must not contain a path separator", key)
		}
		if err := os.WriteFile(filepath.Join(secretsDir, key), []byte(value), 0o600); err != nil {
			return fmt.Errorf("write secret file %q: %w", key, err)
		}
	}
	return nil
}

// runComposeUp pulls before recreating so a mutable tag (e.g. "latest")
// actually picks up whatever's newest on the registry -- `up -d` alone
// never re-pulls on its own. Pull failures are logged into the output but
// never block the deploy: a private/unreachable image or a build-only
// service is exactly what `--ignore-pull-failures` exists for, and this
// path runs on every "up" regardless of why it was triggered.
// --remove-orphans on the actual recreate cleans up a service that was
// removed from the compose file since the last deploy, which a bare
// `up -d` leaves running.
//
// --force-recreate: found by testing that a plain `up -d` silently skips
// a service whose only change is a secret's *value* -- a service that
// consumes it purely via `secrets: - name` (Compose's file-based
// delivery) rather than `${VAR}` in `environment:` has nothing in its
// own hashed config that changed (same secret name/target), so Compose
// decides there's nothing to do and leaves the old container -- and its
// stale mounted secret file -- running untouched. Confirmed on a real
// deploy: two consecutive secret updates followed by plain deploys left
// the exact same container (same id, same start time) serving the
// *first* updated value, never the second. `${VAR}`-substituted
// `environment:` entries were never affected (their resolved value is
// part of the hash), only secrets delivered as files. Rather than try to
// special-case which delivery mechanism needs forcing, every deploy now
// unconditionally recreates every service: "Deploy now" means the
// running containers match the current stack right now, full stop, no
// path that can go silently stale. The cost is every container in the
// stack restarting on every deploy rather than only the ones that
// changed -- acceptable for the small, self-hosted stacks Wharf targets,
// and a predictable rule beats a clever one that occasionally lies.
func runComposeUp(dir, composeFile, stackID string, registryAuths map[string]registryAuth) (status, output string) {
	loginOut := dockerLogin(registryAuths)

	pullCmd := exec.Command("docker", "compose", "-f", composeFile, "-p", stackID, "pull", "--ignore-pull-failures")
	pullCmd.Dir = dir
	pullOut, _ := pullCmd.CombinedOutput()

	status, output = runCompose(dir, composeFile, stackID, "up", "-d", "--remove-orphans", "--force-recreate")
	return status, loginOut + string(pullOut) + output
}

// dockerLogin authenticates to every registry credential the controller
// sent along with this deploy (cf. commandResponse.RegistryAuths and
// ARCHITECTURE.md's "Identifiants de registre envoyés à chaque
// déploiement") before the pull that follows. A failed login for one
// host doesn't abort the deploy -- the pull right after this already
// runs with --ignore-pull-failures, so a bad or revoked credential just
// degrades that one image to the same "unavailable" outcome an
// anonymous pull of a private image would hit anyway, not a hard stop
// for the whole stack. Failures are folded into the deployment's output
// log so they're visible without being fatal; a successful login prints
// nothing extra.
func dockerLogin(auths map[string]registryAuth) string {
	var out strings.Builder
	for host, auth := range auths {
		cmd := exec.Command("docker", "login", host, "-u", auth.Username, "--password-stdin")
		cmd.Stdin = strings.NewReader(auth.Password)
		if result, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(&out, "docker login %s: %s\n%s\n", host, err, result)
		}
	}
	return out.String()
}

func runComposeDownCmd(dir, composeFile, stackID string) (status, output string) {
	return runCompose(dir, composeFile, stackID, "down")
}

func runCompose(dir, composeFile, stackID string, args ...string) (status, output string) {
	fullArgs := append([]string{"compose", "-f", composeFile, "-p", stackID}, args...)
	execCmd := exec.Command("docker", fullArgs...)
	execCmd.Dir = dir
	out, err := execCmd.CombinedOutput()
	if err != nil {
		return "failed", string(out) + "\n" + err.Error()
	}
	return "succeeded", string(out)
}

func reportResult(client *http.Client, controllerURL, deploymentID, status, output, composeContent string) {
	body, err := json.Marshal(struct {
		Status         string `json:"status"`
		Output         string `json:"output"`
		ComposeContent string `json:"compose_content,omitempty"`
	}{Status: status, Output: output, ComposeContent: composeContent})
	if err != nil {
		log.Println("marshal deployment result:", err)
		return
	}

	resp, err := client.Post(controllerURL+"/agent/deployments/"+deploymentID+"/result", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Println("report deployment result failed:", err)
		return
	}
	resp.Body.Close()
}

// newPinnedClient builds an HTTP client that presents this agent's own
// identity as a TLS client certificate and, instead of normal chain
// validation, checks the *server's* presented certificate fingerprint
// against expectedFingerprint — cf. ARCHITECTURE.md, "épinglés par
// empreinte plutôt que validés par une chaîne de confiance". An empty
// expectedFingerprint accepts anything (TOFU, logged once at startup).
func newPinnedClient(cert tls.Certificate, expectedFingerprint string) *http.Client {
	verify := func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if expectedFingerprint == "" || len(rawCerts) == 0 {
			return nil
		}
		got := identity.Fingerprint(rawCerts[0])
		if got != expectedFingerprint {
			return fmt.Errorf("controller certificate fingerprint mismatch: got %s, expected %s", got, expectedFingerprint)
		}
		return nil
	}

	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates:          []tls.Certificate{cert},
				InsecureSkipVerify:    true, // pinning replaces chain validation, cf. verify() above
				VerifyPeerCertificate: verify,
			},
		},
	}
}
