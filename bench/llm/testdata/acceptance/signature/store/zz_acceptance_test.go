package store

import (
	"errors"
	"testing"
)

func TestAcceptanceReadOnly(t *testing.T) {
	rw := New(0, false)
	if err := rw.Put("a", "1"); err != nil {
		t.Fatalf("Put on a writable store: %v", err)
	}
	ro := New(0, true)
	if err := ro.Put("a", "1"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Put on a read-only store = %v, want ErrReadOnly", err)
	}
	if ro.Delete("a") {
		t.Error("Delete on a read-only store reported a deletion")
	}
}

func TestAcceptanceFromPairs(t *testing.T) {
	s, err := FromPairs(map[string]string{"a": "1", "b": "2"}, 0)
	if err != nil {
		t.Fatalf("FromPairs: %v", err)
	}
	for k, want := range map[string]string{"a": "1", "b": "2"} {
		if got, ok := s.Get(k); !ok || got != want {
			t.Errorf("Get(%q) = %q, %v; want %q", k, got, ok, want)
		}
	}
	if err := s.Put("c", "3"); err != nil {
		t.Errorf("FromPairs returned a store that refuses Put: %v", err)
	}
}
