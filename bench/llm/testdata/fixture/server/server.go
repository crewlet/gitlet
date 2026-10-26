// Package server serves a store over HTTP.
package server

import (
	"io"
	"net/http"
	"strings"

	"example.com/kvsvc/config"
	"example.com/kvsvc/store"
)

// Server serves GET, PUT and DELETE on /kv/{key}.
type Server struct {
	cfg   *config.Config
	store *store.Store
}

// New returns a server for the store.
func New(cfg *config.Config, st *store.Store) *Server {
	return &Server{cfg: cfg, store: st}
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/kv/", s.handleKV)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	return mux
}

func (s *Server) handleKV(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/kv/")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		v, ok := s.store.Get(key)
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, v)
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.store.Put(key, string(body)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if !s.store.Delete(key) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
