package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envMap returns a getenv over m plus the required storage settings.
func envMap(m map[string]string) func(string) string {
	full := map[string]string{
		"R2_ENDPOINT": "http://s3", "R2_ACCESS_KEY_ID": "id", "R2_SECRET_ACCESS_KEY": "secret", "R2_BUCKET": "b",
	}
	for k, v := range m {
		full[k] = v
	}
	return func(k string) string { return full[k] }
}

func TestLoadDefaultsToProductionOnPort8080(t *testing.T) {
	c, err := Load(envMap(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != EnvProduction || c.HTTPAddr != ":8080" {
		t.Fatalf("got env %q addr %q, want production :8080", c.Env, c.HTTPAddr)
	}
}

func TestLoadUsesRenderPortUnlessHTTPAddrIsSet(t *testing.T) {
	c, err := Load(envMap(map[string]string{"DATABASE_URL": "postgres://x", "PORT": "10000"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":10000" {
		t.Fatalf("addr = %q, want :10000", c.HTTPAddr)
	}

	c, err = Load(envMap(map[string]string{"DATABASE_URL": "postgres://x", "PORT": "10000", "HTTP_ADDR": "127.0.0.1:9999"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != "127.0.0.1:9999" {
		t.Fatalf("addr = %q, want HTTP_ADDR to win", c.HTTPAddr)
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	_, err := Load(func(k string) string {
		if k == "APP_ENV" {
			return "staging"
		}
		return ""
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"APP_ENV", "DATABASE_URL", "R2_ENDPOINT", "R2_BUCKET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadReadsTheStorageCap(t *testing.T) {
	c, err := Load(envMap(map[string]string{"DATABASE_URL": "postgres://x", "STORAGE_CAP_BYTES": "9000000000"}))
	if err != nil || c.StorageCapBytes != 9_000_000_000 {
		t.Fatalf("cap %d err %v, want 9000000000", c.StorageCapBytes, err)
	}
	if _, err := Load(envMap(map[string]string{"DATABASE_URL": "postgres://x", "STORAGE_CAP_BYTES": "9GB"})); err == nil {
		t.Fatal("a cap that is not a byte count was accepted")
	}
}

func TestLoadEnvFileKeepsRealEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.env")
	content := "# comment\n\nH4H_TEST_FROM_FILE=\"quoted value\"\nH4H_TEST_ALREADY_SET=from-file\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("H4H_TEST_ALREADY_SET", "from-env")
	t.Setenv("H4H_TEST_FROM_FILE", "")
	os.Unsetenv("H4H_TEST_FROM_FILE")

	if err := LoadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("H4H_TEST_FROM_FILE"); got != "quoted value" {
		t.Errorf("H4H_TEST_FROM_FILE = %q, want quoted value", got)
	}
	if got := os.Getenv("H4H_TEST_ALREADY_SET"); got != "from-env" {
		t.Errorf("H4H_TEST_ALREADY_SET = %q, want the real environment to win", got)
	}
}

func TestLoadEnvFileRejectsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.env")
	if err := os.WriteFile(path, []byte("NOT_A_PAIR\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadEnvFile(path); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("err = %v, want a line 1 error", err)
	}
}
