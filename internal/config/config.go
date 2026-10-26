// Package config reads gitlet settings from git config (gitlet.*), with the
// defaults from docs/SPEC.md, section 12.
package config

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config is gitlet's effective configuration for one repository
// (docs/SPEC.md, section 15).
type Config struct {
	Remote            string // "" when gitlet runs without a remote
	Main              string // main branch short name
	BranchPrefix      string
	SnapshotInterval  time.Duration
	PollInterval      time.Duration
	Lease             time.Duration
	SnapshotUntracked string // "source", "all" or "none"
	SnapshotInclude   []string
	PublishSnapshots  bool
	MaxSnapshotFile   int64
	ThreadMessages    int
	ThreadTimeout     time.Duration
	AutoClaim         bool
	StaleItem         time.Duration
	PublishTimeout    time.Duration
	BroadFiles        int
	BroadSymbols      int
	OutsideWindow     time.Duration
	OutsideMax        int
	OutsideInterval   time.Duration
	ReapAfter         time.Duration
	Migrations        []string
}

// Defaults returns the built-in defaults (Remote and Main unset).
func Defaults() Config {
	return Config{
		BranchPrefix:      "gitlet/",
		SnapshotInterval:  45 * time.Second,
		PollInterval:      20 * time.Second,
		Lease:             10 * time.Minute,
		SnapshotUntracked: "source",
		PublishSnapshots:  true,
		MaxSnapshotFile:   5 << 20,
		ThreadMessages:    3,
		ThreadTimeout:     10 * time.Minute,
		AutoClaim:         true,
		StaleItem:         24 * time.Hour,
		PublishTimeout:    15 * time.Minute,
		BroadFiles:        50,
		BroadSymbols:      200,
		OutsideWindow:     168 * time.Hour,
		OutsideMax:        50,
		OutsideInterval:   5 * time.Minute,
		ReapAfter:         168 * time.Hour,
	}
}

// Source is what Load needs from the repository. DefaultBranch must not
// touch the network.
type Source interface {
	ConfigGet(ctx context.Context, key string) (string, error)
	ConfigGetAll(ctx context.Context, key string) ([]string, error)
	Remotes(ctx context.Context) ([]string, error)
	DefaultBranch(ctx context.Context, remote string) (string, error)
}

// Load reads gitlet.* settings over the defaults and resolves the remote and
// main branch.
func Load(ctx context.Context, src Source) (Config, error) {
	c := Defaults()
	get := func(key string) (string, error) {
		v, err := src.ConfigGet(ctx, "gitlet."+key)
		return strings.TrimSpace(v), err
	}
	remote, err := get("remote")
	if err != nil {
		return c, err
	}
	if remote == "" {
		remotes, err := src.Remotes(ctx)
		if err != nil {
			return c, err
		}
		for _, r := range remotes {
			if r == "origin" {
				remote = r
			}
		}
		if remote == "" && len(remotes) > 0 {
			sorted := append([]string(nil), remotes...)
			sort.Strings(sorted)
			remote = sorted[0]
		}
	} else if remote == "none" {
		remote = ""
	}
	c.Remote = remote

	main, err := get("main")
	if err != nil {
		return c, err
	}
	if main == "" {
		main, err = src.DefaultBranch(ctx, remote)
		if err != nil || main == "" {
			main = "main"
		}
	}
	c.Main = main

	if v, err := get("branchPrefix"); err != nil {
		return c, err
	} else if v != "" {
		c.BranchPrefix = v
	}

	durations := []struct {
		key string
		dst *time.Duration
	}{
		{"snapshotInterval", &c.SnapshotInterval},
		{"pollInterval", &c.PollInterval},
		{"lease", &c.Lease},
		{"threadTimeout", &c.ThreadTimeout},
		{"staleItem", &c.StaleItem},
		{"publishTimeout", &c.PublishTimeout},
		{"outsideWindow", &c.OutsideWindow},
		{"outsideInterval", &c.OutsideInterval},
		{"reapAfter", &c.ReapAfter},
	}
	for _, d := range durations {
		v, err := get(d.key)
		if err != nil {
			return c, err
		}
		if v == "" {
			continue
		}
		parsed, err := time.ParseDuration(v)
		if err != nil || parsed <= 0 {
			return c, fmt.Errorf("gitlet.%s: invalid duration %q", d.key, v)
		}
		*d.dst = parsed
	}

	if v, err := get("maxSnapshotFile"); err != nil {
		return c, err
	} else if v != "" {
		n, err := ParseSize(v)
		if err != nil {
			return c, fmt.Errorf("gitlet.maxSnapshotFile: %w", err)
		}
		c.MaxSnapshotFile = n
	}
	counts := []struct {
		key string
		dst *int
	}{
		{"threadMessages", &c.ThreadMessages},
		{"broadFiles", &c.BroadFiles},
		{"broadSymbols", &c.BroadSymbols},
		{"outsideMax", &c.OutsideMax},
	}
	for _, n := range counts {
		v, err := get(n.key)
		if err != nil {
			return c, err
		}
		if v == "" {
			continue
		}
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 {
			return c, fmt.Errorf("gitlet.%s: invalid count %q", n.key, v)
		}
		*n.dst = parsed
	}

	bools := []struct {
		key string
		dst *bool
	}{
		{"autoClaim", &c.AutoClaim},
		{"publishSnapshots", &c.PublishSnapshots},
	}
	for _, b := range bools {
		v, err := get(b.key)
		if err != nil {
			return c, err
		}
		if v == "" {
			continue
		}
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("gitlet.%s: invalid boolean %q", b.key, v)
		}
		*b.dst = parsed
	}

	if v, err := get("snapshotUntracked"); err != nil {
		return c, err
	} else if v != "" {
		switch v {
		case "source", "all", "none":
			c.SnapshotUntracked = v
		default:
			return c, fmt.Errorf("gitlet.snapshotUntracked: want source, all or none, got %q", v)
		}
	}

	for _, multi := range []struct {
		key string
		dst *[]string
	}{
		{"snapshotInclude", &c.SnapshotInclude},
		{"migrations", &c.Migrations},
	} {
		vs, err := src.ConfigGetAll(ctx, "gitlet."+multi.key)
		if err != nil {
			return c, err
		}
		for _, v := range vs {
			if v = strings.TrimSpace(v); v != "" {
				*multi.dst = append(*multi.dst, v)
			}
		}
	}
	return c, nil
}

// ParseSize parses sizes like "5MiB", "512KiB", "1048576", "2MB".
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   int64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), 10, 64)
			if err != nil || n < 0 {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			return n * u.mult, nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n, nil
}
