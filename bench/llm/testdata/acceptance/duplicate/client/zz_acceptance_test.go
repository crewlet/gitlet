package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func zzAcceptanceFlaky(failures int32) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= failures {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("value"))
	}))
	return srv, &n
}

func TestAcceptanceFetchRetries(t *testing.T) {
	srv, n := zzAcceptanceFlaky(2)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, err := New(srv.URL).Fetch(ctx, "k")
	if err != nil {
		t.Fatalf("Fetch after two 503s: %v", err)
	}
	if v != "value" {
		t.Errorf("Fetch = %q, want value", v)
	}
	if got := n.Load(); got != 3 {
		t.Errorf("server saw %d requests, want 3", got)
	}
}

func TestAcceptanceFetchGivesUp(t *testing.T) {
	srv, n := zzAcceptanceFlaky(100)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := New(srv.URL).Fetch(ctx, "k"); err == nil {
		t.Error("Fetch succeeded against a server that always fails")
	}
	if got := n.Load(); got != 3 {
		t.Errorf("server saw %d requests, want 3", got)
	}
}
