package rules

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

func TestAutoClaims(t *testing.T) {
	callers := func(ch model.SymbolChange, theirs ...itemOpt) *vb {
		return view().mine(item(myItem, myIntent, changes(ch))).peer(bob, item(bobItem, bobIntent, theirs...))
	}
	tests := []struct {
		name   string
		v      View
		keys   []string
		reason string
	}{
		{"breaking change meets resolved references", callers(cfgBreaking, refs(callCfg)).v, []string{cfg.Key}, "meets references in codex-ci-bbbbbb's item"},
		{"breaking change clashes", clashView(cfgBreaking, bobSig).v, []string{cfg.Key}, "clashes with codex-ci-bbbbbb"},
		{"compatible change", callers(cfgCompat, refs(callCfg)).v, nil, ""},
		{"unresolved references never claim", callers(sigChange(loadM, "load(): void", true), refs(callLoadM)).v, nil, ""},
		{"nobody references it", callers(cfgBreaking).v, nil, ""},
		{"removed symbols have nothing to publish", callers(removedChange(cfg), refs(callCfg)).v, nil, ""},
		{"already claimed by me", callers(cfgBreaking, refs(callCfg)).claim(cfg.Key, me, myItem, 0).v, nil, ""},
		{"already claimed by someone else", callers(cfgBreaking, refs(callCfg)).claim(cfg.Key, carol, carolItem, 0).v, nil, ""},
		{"autoClaim off", callers(cfgBreaking, refs(callCfg)).with(func(v *View) { v.Limits.AutoClaim = false }).v, nil, ""},
		{"I need it", callers(cfgBreaking, refs(callCfg)).need(me, myNeed(t0)).v, nil, ""},
		{"my other item references it", view().mine(item(myItem, myIntent, changes(cfgBreaking)), item(myItem2, "other", refs(callCfg))).v,
			[]string{cfg.Key}, "references"},
		{"the thread gave it to them", clashView(cfgBreaking, bobSig).
			msg(say(me, myItem, "m1", 0, "you own it", propBob)).accept(bob, sigID, propBob.ID).v, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AutoClaims(tt.v)
			var keys []string
			for _, a := range got {
				keys = append(keys, a.Symbol.Key)
				if a.Item != myItem || a.Symbol.Signature != cfgBreaking.Symbol.Signature || !strings.Contains(a.Reason, tt.reason) {
					t.Fatalf("auto claim = %+v", a)
				}
			}
			if !slices.Equal(keys, tt.keys) {
				t.Fatalf("keys = %v, want %v", keys, tt.keys)
			}
		})
	}
}

func TestReleases(t *testing.T) {
	closed := t0.Add(-time.Minute)
	tests := []struct {
		name   string
		b      *vb
		reason string // "" for no release
	}{
		{"item landed", view().mine(item(myItem, myIntent, status(model.ItemLanded), func(it *model.WorkItem) { it.ClosedAt = &closed })).
			claim(cfg.Key, me, myItem, 0), "landed"},
		{"item abandoned", view().mine(item(myItem, myIntent, status(model.ItemAbandoned))).claim(cfg.Key, me, myItem, 0), "abandoned"},
		{"item gone", view().mine(item(myItem2, "other")).claim(cfg.Key, me, myItem, 0), "gone"},
		{"contract landed", view().mine(item(myItem, myIntent, changes(cfgBreaking))).
			claim(cfg.Key, me, myItem, 0, withContract(cfgBreaking.Symbol.Signature), landed()), "contract landed"},
		{"automatic, no contract, no longer changed", view().mine(item(myItem, myIntent, changes(bodyChange(cfg)))).
			claim(cfg.Key, me, myItem, 0, autoClaim()), "automatic"},
		{"automatic, still changed", view().mine(item(myItem, myIntent, changes(cfgBreaking))).claim(cfg.Key, me, myItem, 0, autoClaim()), ""},
		{"automatic with a contract", view().mine(item(myItem, myIntent)).
			claim(cfg.Key, me, myItem, 0, autoClaim(), withContract("x")), ""},
		{"manual, no longer changed", view().mine(item(myItem, myIntent)).claim(cfg.Key, me, myItem, 0), ""},
		{"submitted item keeps its claim", view().mine(item(myItem, myIntent, submitted(), changes(cfgBreaking))).claim(cfg.Key, me, myItem, 0), ""},
		{"someone else's claim", view().mine(item(myItem2, "other")).claim(cfg.Key, bob, bobItem, 0), ""},
		{"state not loaded", view().with(func(v *View) { v.Mine = model.AgentState{} }).claim(cfg.Key, me, myItem, 0), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Releases(tt.b.v)
			if tt.reason == "" {
				if len(got) != 0 {
					t.Fatalf("releases = %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Key != cfg.Key || got[0].OID != "oid-"+cfg.Key[:6] || !strings.Contains(got[0].Reason, tt.reason) {
				t.Fatalf("releases = %+v, want reason %q", got, tt.reason)
			}
		})
	}
}
