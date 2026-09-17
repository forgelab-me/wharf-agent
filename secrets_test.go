package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A local stack's directory persists across deploys (unlike a Git stack's,
// which is wiped before every clone -- cf. runGitDeploy). writeEnvFile and
// writeSecretFiles must therefore reconcile dir/.env and dir/secrets/ to
// exactly the current deploy's state on every call, never leaving a
// previously-removed, renamed, or now-unused key's plaintext behind.

func composeFileSecret(key string) string {
	return "services:\n  app:\n    image: alpine\n    secrets:\n      - sec\nsecrets:\n  sec:\n    file: ./secrets/" + key + "\n"
}

const composeNoFileSecret = `services:
  app:
    image: alpine
    secrets:
      - sec
secrets:
  sec:
    environment: SOME_VAR
`

func TestWriteEnvFileRemovesStaleFileWhenEmptied(t *testing.T) {
	dir := t.TempDir()

	if err := writeEnvFile(dir, map[string]string{"DB_PASSWORD": "s3cret"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	envPath := filepath.Join(dir, ".env")
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf(".env not written: %v", err)
	}

	if err := writeEnvFile(dir, map[string]string{}); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatalf("stale .env still present after secrets removed: err=%v", err)
	}
}

func TestWriteSecretFilesOnlyWritesKeysUsedViaFile(t *testing.T) {
	dir := t.TempDir()

	env := map[string]string{"DB_PASSWORD": "s3cret", "API_TOKEN": "sk-live"}
	// Only DB_PASSWORD is referenced by the compose file's "file:" source
	// -- API_TOKEN is one of the stack's configured secrets but isn't
	// consumed as a file by anything here, so it must get no file at all.
	if err := writeSecretFiles(dir, env, composeFileSecret("DB_PASSWORD")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "DB_PASSWORD")); err != nil {
		t.Fatalf("used secret file not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "API_TOKEN")); !os.IsNotExist(err) {
		t.Fatalf("unused secret got a file written anyway: err=%v", err)
	}
}

func TestWriteSecretFilesWritesNothingWhenComposeNeverUsesFile(t *testing.T) {
	dir := t.TempDir()

	env := map[string]string{"SOME_VAR": "value"}
	if err := writeSecretFiles(dir, env, composeNoFileSecret); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets")); !os.IsNotExist(err) {
		t.Fatalf("secrets dir created even though no key is file-sourced: err=%v", err)
	}
}

func TestWriteSecretFilesRemovesRenamedKey(t *testing.T) {
	dir := t.TempDir()

	if err := writeSecretFiles(dir, map[string]string{"API_TOKEN": "sk-old"}, composeFileSecret("API_TOKEN")); err != nil {
		t.Fatalf("write: %v", err)
	}
	oldPath := filepath.Join(dir, "secrets", "API_TOKEN")
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("secret file not written: %v", err)
	}

	if err := writeSecretFiles(dir, map[string]string{"NEW_API_TOKEN": "sk-new"}, composeFileSecret("NEW_API_TOKEN")); err != nil {
		t.Fatalf("write renamed: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("stale secret file for renamed key still present: err=%v", err)
	}
	newPath := filepath.Join(dir, "secrets", "NEW_API_TOKEN")
	if b, err := os.ReadFile(newPath); err != nil || string(b) != "sk-new" {
		t.Fatalf("new secret file wrong or missing: content=%q err=%v", b, err)
	}
}

func TestWriteSecretFilesRemovesFileWhenComposeStopsUsingIt(t *testing.T) {
	dir := t.TempDir()

	env := map[string]string{"API_TOKEN": "sk-old"}
	if err := writeSecretFiles(dir, env, composeFileSecret("API_TOKEN")); err != nil {
		t.Fatalf("write: %v", err)
	}
	secretsDir := filepath.Join(dir, "secrets")
	if _, err := os.Stat(secretsDir); err != nil {
		t.Fatalf("secrets dir not created: %v", err)
	}

	// Same key, same value, but the compose file switched to environment:
	// sourcing -- the file must go away even though the secret itself
	// still exists.
	if err := writeSecretFiles(dir, env, composeNoFileSecret); err != nil {
		t.Fatalf("write after switching source: %v", err)
	}
	if _, err := os.Stat(secretsDir); !os.IsNotExist(err) {
		t.Fatalf("stale secrets dir still present after compose stopped using file:: err=%v", err)
	}
}

func TestFileSecretKeysIgnoresEnvironmentSourced(t *testing.T) {
	compose := `services:
  app:
    image: alpine
secrets:
  a:
    file: ./secrets/FILE_KEY
  b:
    environment: ENV_KEY
`
	used := fileSecretKeys(compose)
	if !used["FILE_KEY"] {
		t.Fatalf("expected FILE_KEY to be considered used, got %v", used)
	}
	if used["ENV_KEY"] {
		t.Fatalf("environment:-sourced secret must not be treated as file-used, got %v", used)
	}
}
