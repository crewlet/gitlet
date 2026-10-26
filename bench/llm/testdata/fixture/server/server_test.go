package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"example.com/kvsvc/client"
	"example.com/kvsvc/config"
	"example.com/kvsvc/store"
)

func TestRoundTrip(t *testing.T) {
	srv := httptest.NewServer(New(config.Default(), store.New(0)).Handler())
	defer srv.Close()
	c := client.New(srv.URL)
	ctx := context.Background()
	if err := c.Put(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	got, err := c.Fetch(ctx, "k")
	if err != nil || got != "v" {
		t.Fatalf("Fetch = %q, %v", got, err)
	}
	if _, err := c.Fetch(ctx, "missing"); err == nil {
		t.Fatal("want an error for a missing key")
	}
}
