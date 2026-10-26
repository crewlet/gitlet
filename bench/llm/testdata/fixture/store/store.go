// Package store is an in-memory key-value store.
package store

import (
	"sort"
	"sync"
)

// Store holds string values by key. It is safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	data map[string]string
	max  int
}

// New returns an empty store. max is the most keys it should hold; zero or
// less means no limit.
func New(max int) *Store {
	return &Store{data: map[string]string{}, max: max}
}

// Get returns the value for key.
func (s *Store) Get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

// Put sets the value for key.
func (s *Store) Put(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return nil
}

// Delete removes key and reports whether it was there.
func (s *Store) Delete(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[key]
	delete(s.data, key)
	return ok
}

// Keys returns the keys in sorted order.
func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
