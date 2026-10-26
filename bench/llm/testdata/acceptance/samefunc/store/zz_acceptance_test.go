package store

import (
	"errors"
	"testing"
)

func TestAcceptanceEmptyKey(t *testing.T) {
	s := New(0)
	if err := s.Put("", "v"); !errors.Is(err, ErrEmptyKey) {
		t.Errorf("Put(\"\") = %v, want ErrEmptyKey", err)
	}
	if err := New(1).Put("", "v"); !errors.Is(err, ErrEmptyKey) {
		t.Errorf("Put(\"\") on a limited store = %v, want ErrEmptyKey", err)
	}
}

func TestAcceptanceFull(t *testing.T) {
	s := New(1)
	if err := s.Put("a", "1"); err != nil {
		t.Fatalf("Put(a): %v", err)
	}
	if err := s.Put("b", "2"); !errors.Is(err, ErrFull) {
		t.Errorf("Put(b) on a full store = %v, want ErrFull", err)
	}
	if err := s.Put("a", "3"); err != nil {
		t.Errorf("updating an existing key on a full store: %v", err)
	}
	if v, _ := s.Get("a"); v != "3" {
		t.Errorf("Get(a) = %q, want 3", v)
	}
	u := New(0)
	for _, k := range []string{"a", "b", "c"} {
		if err := u.Put(k, "v"); err != nil {
			t.Errorf("unlimited store Put(%s): %v", k, err)
		}
	}
}
