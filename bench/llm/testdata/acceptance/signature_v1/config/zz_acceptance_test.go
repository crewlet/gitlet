package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAcceptance(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kvsvc.conf")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAcceptanceStrictLoad(t *testing.T) {
	p := writeAcceptance(t, "addr=:9000\nbogus=1\n")
	if _, err := Load(p, true); err == nil {
		t.Error("strict Load accepted an unknown key")
	}
	c, err := Load(p, false)
	if err != nil {
		t.Fatalf("non-strict Load: %v", err)
	}
	if c.Addr != ":9000" {
		t.Errorf("Addr = %q, want :9000", c.Addr)
	}
}

func TestAcceptanceLoadAndValidate(t *testing.T) {
	good := writeAcceptance(t, "addr=:9000\nlog_level=debug\n")
	if _, err := LoadAndValidate(good); err != nil {
		t.Errorf("LoadAndValidate(good): %v", err)
	}
	bad := writeAcceptance(t, "log_level=loud\n")
	if _, err := LoadAndValidate(bad); err == nil {
		t.Error("LoadAndValidate accepted an invalid log_level")
	}
}
