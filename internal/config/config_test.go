package config

import (
	"context"
	"testing"
	"time"
)

type fakeSource struct {
	values  map[string][]string
	remotes []string
	head    string
}

func (f fakeSource) ConfigGet(_ context.Context, key string) (string, error) {
	if v := f.values[key]; len(v) > 0 {
		return v[len(v)-1], nil
	}
	return "", nil
}

func (f fakeSource) ConfigGetAll(_ context.Context, key string) ([]string, error) {
	return f.values[key], nil
}

func (f fakeSource) Remotes(context.Context) ([]string, error) { return f.remotes, nil }

func (f fakeSource) DefaultBranch(context.Context, string) (string, error) { return f.head, nil }

func TestLoadDefaults(t *testing.T) {
	c, err := Load(context.Background(), fakeSource{remotes: []string{"upstream", "origin"}, head: "trunk"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Remote != "origin" || c.Main != "trunk" {
		t.Fatalf("remote/main = %q/%q", c.Remote, c.Main)
	}
	d := Defaults()
	if c.Lease != d.Lease || c.BranchPrefix != "gitlet/" || !c.PublishSnapshots || c.SnapshotUntracked != "source" {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestLoadRemoteChoice(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string][]string
		remotes []string
		want    string
	}{
		{"no remotes", nil, nil, ""},
		{"single non-origin", nil, []string{"github"}, "github"},
		{"several without origin picks first sorted", nil, []string{"zeta", "alpha"}, "alpha"},
		{"explicit none", map[string][]string{"gitlet.remote": {"none"}}, []string{"origin"}, ""},
		{"explicit", map[string][]string{"gitlet.remote": {"fork"}}, []string{"origin", "fork"}, "fork"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(context.Background(), fakeSource{values: tt.values, remotes: tt.remotes})
			if err != nil {
				t.Fatal(err)
			}
			if c.Remote != tt.want {
				t.Fatalf("remote = %q, want %q", c.Remote, tt.want)
			}
			if c.Main != "main" {
				t.Fatalf("main = %q, want main", c.Main)
			}
		})
	}
}

func TestLoadOverrides(t *testing.T) {
	src := fakeSource{values: map[string][]string{
		"gitlet.lease":             {"5m"},
		"gitlet.threadMessages":    {"5"},
		"gitlet.autoClaim":         {"false"},
		"gitlet.publishSnapshots":  {"false"},
		"gitlet.snapshotUntracked": {"none"},
		"gitlet.maxSnapshotFile":   {"1MiB"},
		"gitlet.migrations":        {"db/changes", " sql/patches "},
		"gitlet.branchPrefix":      {"agents/"},
		"gitlet.main":              {"develop"},
	}}
	c, err := Load(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if c.Lease != 5*time.Minute || c.ThreadMessages != 5 || c.AutoClaim || c.PublishSnapshots ||
		c.SnapshotUntracked != "none" || c.MaxSnapshotFile != 1<<20 || c.BranchPrefix != "agents/" || c.Main != "develop" {
		t.Fatalf("overrides not applied: %+v", c)
	}
	if len(c.Migrations) != 2 || c.Migrations[1] != "sql/patches" {
		t.Fatalf("migrations = %q", c.Migrations)
	}
}

func TestLoadInvalid(t *testing.T) {
	for key, v := range map[string]string{
		"gitlet.lease":             "soon",
		"gitlet.threadMessages":    "0",
		"gitlet.autoClaim":         "maybe",
		"gitlet.snapshotUntracked": "some",
		"gitlet.maxSnapshotFile":   "lots",
	} {
		_, err := Load(context.Background(), fakeSource{values: map[string][]string{key: {v}}})
		if err == nil {
			t.Errorf("%s=%q: expected an error", key, v)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"5MiB": 5 << 20, "512KiB": 512 << 10, "1048576": 1 << 20, "2MB": 2e6, "3K": 3 << 10} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}
