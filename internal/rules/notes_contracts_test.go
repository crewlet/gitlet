package rules

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

func bobNeeds(req string) model.Need {
	return model.Need{Key: cfg.Key, Name: cfg.Ref(), Item: bobItem, Requirement: req, CreatedAt: t0.Add(-time.Minute)}
}

func myNeed(created time.Time) model.Need {
	return model.Need{Key: cfg.Key, Name: cfg.Ref(), Item: myItem, CreatedAt: created}
}

func TestNeeded(t *testing.T) {
	base := func(mine ...itemOpt) *vb {
		return view().mine(item(myItem, myIntent, mine...)).peer(bob, item(bobItem, bobIntent)).need(bob, bobNeeds("keep returning Config"))
	}
	runSev(t, model.NoteNeeded, []sevCase{
		{"I change it", base(changes(cfgBreaking)).v, model.SeverityAct},
		{"I own it", base().claim(cfg.Key, me, myItem, 0, withSymbol(cfg)).v, model.SeverityAct},
		{"published", base(changes(cfgBreaking)).claim(cfg.Key, me, myItem, 0, withContract(cfgBreaking.Symbol.Signature)).v, ""},
		{"someone else holds it", base(changes(cfgBreaking)).claim(cfg.Key, carol, carolItem, 0).v, ""},
		{"I change only its body", base(changes(bodyChange(cfg))).v, ""},
		{"nobody needs it", view().mine(item(myItem, myIntent, changes(cfgBreaking))).v, ""},
		{"lapsed needer", base(changes(cfgBreaking)).lapsed(bob, t0.Add(-time.Hour)).v, model.SeverityWarn},
		{"needer out of view", base(changes(cfgBreaking)).lapsed(bob, t0.Add(-30*time.Hour)).v, ""},
		{"need without an item belongs to their first item", view().mine(item(myItem, myIntent, changes(cfgBreaking))).
			peer(bob, item(bobItem, bobIntent)).need(bob, model.Need{Key: cfg.Key, Name: cfg.Ref()}).v, model.SeverityAct},
		{"need naming another agent's item", view().mine(item(myItem, myIntent, changes(cfgBreaking))).
			peer(bob, item(bobItem, bobIntent)).peer(carol, item(carolItem, "x")).need(bob, model.Need{Key: cfg.Key, Item: carolItem}).v, ""},
	})
	n := one(t, eval(t, base(changes(cfgBreaking)).v), model.NoteNeeded)
	if !slices.Equal(tools(n), []string{"publish", "discuss"}) {
		t.Fatalf("options %v", tools(n))
	}
	wantArgs(t, optionFor(t, n, "publish"), map[string]any{"symbol": "src/config.ts:parseConfig", "note": n.ID})
	if len(n.Quotes) != 1 || n.Quotes[0] != (model.Quote{From: bob, Text: "keep returning Config"}) {
		t.Fatalf("quotes = %+v", n.Quotes)
	}
	if !strings.Contains(n.Title, "which you change; publish its signature") {
		t.Fatalf("title = %q", n.Title)
	}
	// A new requirement is material.
	n2 := one(t, eval(t, view().mine(item(myItem, myIntent, changes(cfgBreaking))).peer(bob, item(bobItem, bobIntent)).
		need(bob, bobNeeds("must accept a strict flag")).v), model.NoteNeeded)
	if n2.Version == n.Version {
		t.Fatal("version did not change with the requirement")
	}
}

func TestReady(t *testing.T) {
	base := func(n model.Need, claim ...func(*ClaimView)) *vb {
		return view().mine(item(myItem, myIntent, refs(callCfg))).peer(bob, item(bobItem, bobIntent, changes(cfgBreaking))).
			need(me, n).claim(cfg.Key, bob, bobItem, 0, append([]func(*ClaimView){withSymbol(cfg)}, claim...)...)
	}
	published := withContract(cfgBreaking.Symbol.Signature)
	pinned := myNeed(t0)
	pinned.PinnedSigHash = hashOf(cfgBreaking.Symbol.Signature)
	runSev(t, model.NoteReady, []sevCase{
		{"published, not used", base(myNeed(t0), published).v, model.SeverityAct},
		{"not published", base(myNeed(t0)).v, ""},
		{"already used", base(pinned, published).v, ""},
		{"owner idle", base(myNeed(t0), published).lapsed(bob, t0.Add(-time.Hour)).v, model.SeverityWarn},
	})
	n := one(t, eval(t, base(myNeed(t0), published).v), model.NoteReady)
	if !slices.Equal(tools(n), []string{"use", "dismiss"}) {
		t.Fatalf("options %v", tools(n))
	}
	wantArgs(t, optionFor(t, n, "use"), map[string]any{"symbol": "src/config.ts:parseConfig", "note": n.ID})
	if n.Evidence[0] != "contract: "+cfgBreaking.Symbol.Signature || !strings.Contains(n.Title, "published src/config.ts:parseConfig") {
		t.Fatalf("evidence %q title %q", n.Evidence, n.Title)
	}
	l := one(t, eval(t, base(myNeed(t0), published, landed()).v), model.NoteReady)
	if !slices.Equal(tools(l), []string{"update", "dismiss"}) {
		t.Fatalf("landed contract options %v", tools(l))
	}
	// Callee notes stay suppressed while I need the symbol.
	none(t, eval(t, base(myNeed(t0), published).v), model.NoteCalleeChanged)
	// Republishing the same signature from a new stub is not material.
	restub := base(myNeed(t0), published, func(cv *ClaimView) { cv.Claim.Contract.StubCommit = "stub2" })
	if one(t, eval(t, restub.v), model.NoteReady).Version != n.Version {
		t.Fatal("version changed with the stub commit")
	}
}

func TestDrift(t *testing.T) {
	pinned := myNeed(t0)
	pinned.PinnedSigHash = hashOf(cfgCompat.Symbol.Signature)
	base := func(contract string) *vb {
		return view().mine(item(myItem, myIntent)).peer(bob, item(bobItem, bobIntent, changes(cfgBreaking))).
			need(me, pinned).claim(cfg.Key, bob, bobItem, 1, withSymbol(cfg), withContract(contract))
	}
	runSev(t, model.NoteDrift, []sevCase{
		{"contract changed", base(cfgBreaking.Symbol.Signature).v, model.SeverityAct},
		{"contract as pinned", base(cfgCompat.Symbol.Signature).v, ""},
		{"never pinned", view().mine(item(myItem, myIntent)).need(me, myNeed(t0)).
			claim(cfg.Key, bob, bobItem, 1, withContract(cfgBreaking.Symbol.Signature)).v, ""},
	})
	n := one(t, eval(t, base(cfgBreaking.Symbol.Signature).v), model.NoteDrift)
	if !slices.Equal(tools(n), []string{"use", "discuss"}) || n.Thread == "" {
		t.Fatalf("options %v thread %q", tools(n), n.Thread)
	}
	none(t, eval(t, base(cfgBreaking.Symbol.Signature).v), model.NoteReady)
}

func TestDiverged(t *testing.T) {
	mine := func(ch model.SymbolChange, claim ...func(*ClaimView)) *vb {
		return view().mine(item(myItem, myIntent, changes(ch))).peer(bob, item(bobItem, bobIntent)).
			need(bob, bobNeeds("")).claim(cfg.Key, me, myItem, 0, append([]func(*ClaimView){withSymbol(cfg)}, claim...)...)
	}
	runSev(t, model.NoteDiverged, []sevCase{
		{"differs from my contract", mine(cfgBreaking, withContract(cfgCompat.Symbol.Signature)).v, model.SeverityAct},
		{"matches my contract", mine(cfgBreaking, withContract(cfgBreaking.Symbol.Signature)).v, ""},
		{"contract landed", mine(cfgBreaking, withContract(cfgCompat.Symbol.Signature), landed()).v, ""},
		{"no contract, no agreement", mine(cfgBreaking).v, ""},
		{"not mine", view().mine(item(myItem, myIntent, changes(cfgBreaking))).
			claim(cfg.Key, bob, bobItem, 0, withContract(cfgCompat.Symbol.Signature)).v, ""},
	})
	n := one(t, eval(t, mine(cfgBreaking, withContract(cfgCompat.Symbol.Signature)).v), model.NoteDiverged)
	if !slices.Equal(tools(n), []string{"publish", "discuss"}) || len(n.Parties) != 1 || n.Parties[0].Agent != bob {
		t.Fatalf("options %v parties %v", tools(n), n.Parties)
	}
	if n.Evidence[0] != "agreed: "+cfgCompat.Symbol.Signature || n.Evidence[1] != "yours now: "+cfgBreaking.Symbol.Signature {
		t.Fatalf("evidence %q", n.Evidence)
	}
	// Editing my signature again while it still differs is not re-sent.
	again := mine(sigChange(cfg, "function parseConfig(number): Config", true), withContract(cfgCompat.Symbol.Signature))
	if one(t, eval(t, again.v), model.NoteDiverged).Version != n.Version {
		t.Fatal("version changed with my own edits")
	}
}

func TestDivergedFromProposal(t *testing.T) {
	other := sigChange(cfg, "function parseConfig(string, Options): Config", true)
	thread := model.SigThreadID(cfg.Key, 0)
	agreed := func(sig string) *vb {
		return clashView(cfgBreaking, other).claim(cfg.Key, me, myItem, 0, withSymbol(cfg)).
			msg(model.Message{ID: "m1", Thread: thread, From: bob, Item: bobItem, Items: []model.ItemID{myItem, bobItem}, Text: "you own it",
				Proposal: &model.Proposal{ID: "p-000000000001", Owner: me, Signature: sig, Summary: "strict flag"}, At: t0}).
			accept(me, thread, "p-000000000001")
	}
	runSev(t, model.NoteDiverged, []sevCase{
		{"matches loosely", agreed("parseConfig(string,  boolean): Config").v, ""},
		{"matches with parameter names", agreed("function parseConfig(text: string, strict: boolean): Config;").v, ""},
		{"optional where mine is required", agreed("parseConfig(text: string, strict?: boolean): Config").v, model.SeverityAct},
		{"differs", agreed("parseConfig(string, Options): Config").v, model.SeverityAct},
		// Bob recorded his need: the clash and its thread are gone, the
		// agreement in the messages still counts.
		{"differs after they deferred", agreed("parseConfig(string, Options): Config").need(bob, bobNeeds("")).v, model.SeverityAct},
		{"matches after they deferred", agreed("parseConfig(string, boolean): Config").need(bob, bobNeeds("")).v, ""},
	})
}

func TestStillWaiting(t *testing.T) {
	base := func(created time.Time) *vb {
		return view().mine(item(myItem, myIntent)).peer(bob, item(bobItem, bobIntent, changes(cfgBreaking))).need(me, myNeed(created))
	}
	runSev(t, model.NoteStillWaiting, []sevCase{
		{"past the publish timeout", base(t0.Add(-20 * time.Minute)).v, model.SeverityAct},
		{"within the timeout", base(t0.Add(-5 * time.Minute)).v, ""},
		{"published", base(t0.Add(-20*time.Minute)).claim(cfg.Key, bob, bobItem, 0, withContract("x")).v, ""},
		{"custom timeout", base(t0.Add(-5 * time.Minute)).with(func(v *View) { v.Limits.PublishTimeout = time.Minute }).v, model.SeverityAct},
		{"no creation time", base(time.Time{}).v, ""},
	})
	n := one(t, eval(t, base(t0.Add(-20*time.Minute)).v), model.NoteStillWaiting)
	if !slices.Equal(tools(n), []string{"release", "discuss"}) || !strings.Contains(n.Title, "after 20m") {
		t.Fatalf("options %v title %q", tools(n), n.Title)
	}
	wantArgs(t, optionFor(t, n, "release"), map[string]any{"symbol": "src/config.ts:parseConfig", "note": n.ID})
	if later := one(t, eval(t, base(t0.Add(-40*time.Minute)).v), model.NoteStillWaiting); later.Version != n.Version {
		t.Fatal("version changed with the waiting time")
	}
}

func TestLapsed(t *testing.T) {
	idle := t0.Add(-time.Hour)
	runSev(t, model.NoteLapsed, []sevCase{
		{"owner of a claim I need", view().mine(item(myItem, myIntent)).need(me, myNeed(t0)).
			claim(cfg.Key, bob, bobItem, 0, withSymbol(cfg), claimLapsed(idle)).v, model.SeverityWarn},
		{"owner of a claim I contest", view().mine(item(myItem, myIntent, changes(cfgBreaking))).
			claim(cfg.Key, bob, bobItem, 0, withSymbol(cfg), claimLapsed(idle)).v, model.SeverityWarn},
		{"owner alive", view().mine(item(myItem, myIntent)).need(me, myNeed(t0)).claim(cfg.Key, bob, bobItem, 0, withSymbol(cfg)).v, ""},
		{"I only edit its body", view().mine(item(myItem, myIntent, changes(bodyChange(cfg)))).
			claim(cfg.Key, bob, bobItem, 0, withSymbol(cfg), claimLapsed(idle)).v, ""},
		{"my own claim", view().mine(item(myItem, myIntent, changes(cfgBreaking))).
			claim(cfg.Key, me, myItem, 0, withSymbol(cfg), claimLapsed(idle)).v, ""},
	})
	n := one(t, eval(t, view().mine(item(myItem, myIntent)).need(me, myNeed(t0)).
		claim(cfg.Key, bob, bobItem, 0, withSymbol(cfg), claimLapsed(idle)).v), model.NoteLapsed)
	if !slices.Equal(tools(n), []string{"claim", "dismiss"}) || n.Parties[0].IdleSince == nil || !strings.Contains(n.Title, "idle since 2026-10-26 11:00 UTC") {
		t.Fatalf("options %v parties %+v title %q", tools(n), n.Parties, n.Title)
	}
	wantArgs(t, optionFor(t, n, "claim"), map[string]any{"symbol": "src/config.ts:parseConfig", "note": n.ID})
}
