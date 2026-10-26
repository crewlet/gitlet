// Package client talks to a kvsvc server over HTTP.
package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client is a kvsvc client.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client for the server at base, such as http://localhost:8080.
func New(base string) *Client {
	return &Client{BaseURL: strings.TrimRight(base, "/"), HTTP: http.DefaultClient}
}

func (c *Client) url(key string) string {
	return c.BaseURL + "/kv/" + url.PathEscape(key)
}

// Fetch returns the value stored under key.
func (c *Client) Fetch(ctx context.Context, key string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(key), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: %s", key, resp.Status)
	}
	return string(body), nil
}

// Put stores value under key.
func (c *Client) Put(ctx context.Context, key, value string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url(key), strings.NewReader(value))
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("put %s: %s", key, resp.Status)
	}
	return nil
}
