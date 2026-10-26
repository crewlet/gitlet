package store

import "testing"

func TestAcceptanceLen(t *testing.T) {
	s := New(0)
	s.Put("a", "1")
	s.Put("b", "2")
	s.Put("a", "3")
	if got := s.Len(); got != 2 {
		t.Errorf("Len() = %d, want 2", got)
	}
}
