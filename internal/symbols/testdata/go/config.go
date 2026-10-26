// Package config loads and stores settings.
package config

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	. "strings"

	"example.com/mod/v2"
	yaml "gopkg.in/yaml.v3"

	"github.com/crewlet/gitlet/internal/model"
)

// MaxSize bounds a config file.
const MaxSize int = 1 << 20

// Kind is a config source.
type Kind int

const (
	KindFile Kind = iota
	KindEnv
	kindNone
)

var (
	Default       *Config
	loaded, Count = false, 0
)

var _ Store = (*memStore)(nil)

// Config is the loaded settings.
type Config struct {
	Name, Alias string `yaml:"name"`
	Port        int
	secret      string
	Base
	*model.Agent
	Labels map[string]List[string]
}

// Base holds shared fields.
type Base struct{ ID ID }

type (
	ID          string
	Alias       = model.Symbol
	List[T any] struct {
		Items []T
		size  int
	}
)

// Store reads and writes configs.
type Store interface {
	Get(ctx context.Context, key string) ([]byte, error)
	put(key string, value []byte) error
	io.Closer
}

// Number is a numeric constraint.
type Number interface {
	~int | ~float64
}

type memStore struct{ m map[string][]byte }

func init() { Default = &Config{Port: 8080} }

func init() { mod.Register(KindEnv) }

// Load reads the config at path.
func Load(path string, strict bool) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("load %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, err
	}
	c.Name = TrimSpace(c.Name)
	return c, nil
}

// Map applies f to each element.
func Map[T any, U any](xs []T, f func(T) U) []U {
	out := make([]U, 0, len(xs))
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}

// Printf formats like fmt.Printf.
func Printf(format string, args ...any) { fmt.Printf(format, args...) }

// Load reloads c from path.
func (c *Config) Load(path string, strict bool) (Config, error) {
	return Load(path, strict)
}

func (l List[T]) Len() int { return len(l.Items) }

func (s *memStore) Get(_ context.Context, key string) ([]byte, error) {
	return s.m[key], nil
}

func (s *memStore) put(key string, value []byte) error {
	s.m[key] = value
	return nil
}

func (s *memStore) Close() error { return nil }
