package ledger

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestObserveRefs(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	const ns = "refs/gitlet-remote/origin/"
	t1, t2, t3 := t0, t0.Add(20*time.Second), t0.Add(40*time.Second)

	steps := []struct {
		name    string
		current map[string]string
		at      time.Time
		want    map[string]time.Time
	}{
		{"first refresh", map[string]string{"agents/a": "1", "agents/b": "2", "claims/k": "3"}, t1,
			map[string]time.Time{"agents/a": t1, "agents/b": t1, "claims/k": t1}},
		{"unchanged keep, changed and new reset, missing forgotten",
			map[string]string{"agents/a": "1", "agents/b": "2b", "outbox/a": "4"}, t2,
			map[string]time.Time{"agents/a": t1, "agents/b": t2, "outbox/a": t2}},
		{"a forgotten ref that comes back is new", map[string]string{"agents/a": "1", "claims/k": "3"}, t3,
			map[string]time.Time{"agents/a": t1, "claims/k": t3}},
		{"empty refresh forgets everything", map[string]string{}, t3.Add(time.Second), map[string]time.Time{}},
	}
	for _, s := range steps {
		obs, err := l.ObserveRefs(ctx, ns, s.current, s.at.In(time.FixedZone("CEST", 2*3600)))
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if !reflect.DeepEqual(obs.FirstSeen, s.want) || !obs.LastFetch.Equal(s.at) {
			t.Fatalf("%s: ObserveRefs = %v, %v; want %v, %v", s.name, obs.FirstSeen, obs.LastFetch, s.want, s.at)
		}
		stored, err := l.Observed(ctx, ns)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(stored, obs) {
			t.Fatalf("%s: Observed = %+v, want %+v", s.name, stored, obs)
		}
		if stored.LastFetch.Location() != time.UTC {
			t.Fatalf("%s: LastFetch not in UTC", s.name)
		}
	}
}

func TestObserveRefsNamespaces(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	if _, err := l.ObserveRefs(ctx, "refs/gitlet/", map[string]string{"refs/gitlet/agents/a": "1"}, t0); err != nil {
		t.Fatal(err)
	}
	// A refresh of another namespace leaves the first alone.
	t1 := t0.Add(time.Minute)
	if _, err := l.ObserveRefs(ctx, "refs/heads/", map[string]string{"refs/heads/main": "m"}, t1); err != nil {
		t.Fatal(err)
	}
	obs, err := l.Observed(ctx, "refs/gitlet/")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Observation{FirstSeen: map[string]time.Time{"refs/gitlet/agents/a": t0}, LastFetch: t0}); !reflect.DeepEqual(obs, want) {
		t.Fatalf("Observed(refs/gitlet/) = %+v, want %+v", obs, want)
	}
	empty, err := l.Observed(ctx, "refs/unknown/")
	if err != nil || empty.FirstSeen == nil || len(empty.FirstSeen) != 0 || !empty.LastFetch.IsZero() {
		t.Fatalf("Observed(unknown) = %+v, %v", empty, err)
	}

	// After the machine slept every first-seen time restarts, in every
	// namespace; last fetch times stay.
	woke := t0.Add(3 * time.Hour)
	if err := l.ResetFirstSeen(ctx, woke); err != nil {
		t.Fatal(err)
	}
	for ns, want := range map[string]Observation{
		"refs/gitlet/": {FirstSeen: map[string]time.Time{"refs/gitlet/agents/a": woke}, LastFetch: t0},
		"refs/heads/":  {FirstSeen: map[string]time.Time{"refs/heads/main": woke}, LastFetch: t1},
	} {
		got, err := l.Observed(ctx, ns)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Observed(%s) after reset = %+v, %v; want %+v", ns, got, err, want)
		}
	}
	// An unchanged oid keeps the reset time on the next refresh.
	obs, err = l.ObserveRefs(ctx, "refs/gitlet/", map[string]string{"refs/gitlet/agents/a": "1"}, woke.Add(time.Minute))
	if err != nil || !obs.FirstSeen["refs/gitlet/agents/a"].Equal(woke) {
		t.Fatalf("refresh after reset = %+v, %v", obs, err)
	}
}
