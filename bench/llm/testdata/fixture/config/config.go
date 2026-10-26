// Package config loads the service configuration from a key=value file.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the service configuration.
type Config struct {
	Addr     string
	DataDir  string
	MaxKeys  int
	LogLevel string
}

// Default returns the configuration used when no file is given.
func Default() *Config {
	return &Config{Addr: ":8080", DataDir: "data", MaxKeys: 0, LogLevel: "info"}
}

// Load reads a configuration file. Each non-empty line is key=value; lines
// starting with # are comments. Keys not listed in Config are ignored.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c := Default()
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected key=value", path, line)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "addr":
			c.Addr = value
		case "data_dir":
			c.DataDir = value
		case "max_keys":
			n, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: max_keys: %v", path, line, err)
			}
			c.MaxKeys = n
		case "log_level":
			c.LogLevel = value
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate reports the first problem with a configuration.
func (c *Config) Validate() error {
	if c.Addr == "" {
		return errors.New("addr is empty")
	}
	if c.MaxKeys < 0 {
		return errors.New("max_keys is negative")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("unknown log_level %q", c.LogLevel)
	}
	return nil
}
