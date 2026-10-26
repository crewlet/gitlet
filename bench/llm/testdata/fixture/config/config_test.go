package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kvsvc.conf")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	p := write(t, "# test\naddr = :9090\nmax_keys=10\nlog_level=debug\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":9090" || c.MaxKeys != 10 || c.LogLevel != "debug" || c.DataDir != "data" {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadBadLine(t *testing.T) {
	if _, err := Load(write(t, "addr\n")); err == nil {
		t.Fatal("want an error")
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.LogLevel = "loud"
	if err := c.Validate(); err == nil {
		t.Fatal("want an error")
	}
}
