package store

import "testing"

func TestPutGetDelete(t *testing.T) {
	s := New(0)
	if err := s.Put("a", "1"); err != nil {
		t.Fatal(err)
	}
	if v, ok := s.Get("a"); !ok || v != "1" {
		t.Fatalf("Get = %q, %v", v, ok)
	}
	if !s.Delete("a") || s.Delete("a") {
		t.Fatal("Delete")
	}
}

func TestKeys(t *testing.T) {
	s := New(0)
	for _, k := range []string{"b", "a", "c"} {
		_ = s.Put(k, k)
	}
	got := s.Keys()
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("Keys = %v", got)
	}
}
