package rules

import (
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

// rich builds a view in which most rules fire at once.
func rich() *vb {
	store := fn("go", "internal/store/store.go", "Open", "func Open(string) (*Store, error)")
	return view().
		mine(
			item(myItem, myIntent, changes(cfgBreaking, bodyChange(loadM), bodyChange(store)),
				refs(callLoadM, ref("Open", "go", "cmd/main.go", 3, "internal/store")),
				files(unparsed("README.md"), unparsed("go.sum"), added("db/migrations/0003_users.sql"))),
			item(myItem2, "fix-thing", provisional(), refs(callCfg)),
		).
		peer(bob, item(bobItem, "Make config loading strict with a mode flag",
			changes(bobSig, sigChange(store, "func Open(string, Options) (*Store, error)", true)),
			refs(ref("parseConfig", "typescript", "src/cli.ts", 8, "src/config")),
			files(unparsed("README.md"), unparsed("go.sum"), added("db/migrations/0003_orders.sql")))).
		peer(carol, item(carolItem, "Reformat everything", broadFP(), files(unparsed("README.md")))).
		lapsed(carol, t0.Add(-2*time.Hour)).
		claim(loadM.Key, bob, bobItem, 0, withSymbol(loadM), withContract("load(string, boolean): void"), claimLapsed(t0.Add(-time.Hour))).
		need(me, model.Need{Key: loadM.Key, Name: loadM.Ref(), Item: myItem, CreatedAt: t0.Add(-time.Hour)}).
		need(bob, model.Need{Key: store.Key, Name: store.Ref(), Item: bobItem, Requirement: "keep the error"}).
		conflict(myItem, bobItem, "README.md", "src/config.ts").
		main(myItem, model.Footprint{Symbols: []model.SymbolChange{removedChange(fn("typescript", "src/config.ts", "Config.load", "load(string): void"))}}).
		msg(say(bob, bobItem, "m1", 0, "what do you need from parseConfig?", propBob)).
		with(func(v *View) {
			v.Outside = []Outside{{Branch: "hotfix/store", Head: "h", Footprint: model.Footprint{Symbols: []model.SymbolChange{bodyChange(store)}}}}
			v.SameWorktree = true
		})
}

type outputs struct {
	Notes     []model.Note
	Threads   []model.ThreadState
	Decisions []Decision
	Auto      []AutoClaim
	Releases  []Release
}

func run(v View) outputs {
	return outputs{Evaluate(v), Threads(v), Decisions(v), AutoClaims(v), Releases(v)}
}

// reinsert rebuilds every map of a view in reverse key order.
func reinsert(v View) View {
	rev := func(keys []string) []string { slices.Reverse(keys); return keys }
	peers := map[model.AgentID]Peer{}
	ids := slices.Sorted(maps.Keys(v.Peers))
	slices.Reverse(ids)
	for _, id := range ids {
		peers[id] = v.Peers[id]
	}
	claims := map[string]ClaimView{}
	for _, k := range rev(slices.Sorted(maps.Keys(v.Claims))) {
		claims[k] = v.Claims[k]
	}
	mains := map[model.ItemID]MainView{}
	items := slices.Sorted(maps.Keys(v.Main))
	slices.Reverse(items)
	for _, k := range items {
		mains[k] = v.Main[k]
	}
	seen := map[string]time.Time{}
	for _, k := range rev(slices.Sorted(maps.Keys(v.ClashSeen))) {
		seen[k] = v.ClashSeen[k]
	}
	v.Peers, v.Claims, v.Main, v.ClashSeen = peers, claims, mains, seen
	return v
}

func TestDeterminism(t *testing.T) {
	v := rich().v
	first := run(v)
	if len(first.Notes) < 15 {
		t.Fatalf("rich view should fire most rules, got %s", summary(first.Notes))
	}
	checkNotes(t, first.Notes)
	for i := 0; i < 25; i++ {
		if got := run(v); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs", i)
		}
		if got := run(reinsert(v)); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d with reinserted maps differs", i)
		}
	}
	for _, n := range first.Notes {
		if !n.CreatedAt.Equal(t0) {
			t.Fatalf("%s: CreatedAt %v is not View.Now", n.Kind, n.CreatedAt)
		}
	}
}

func TestEvaluateDoesNotMutateTheView(t *testing.T) {
	v := rich().v
	before := run(v)
	snapshot := reflect.ValueOf(v.Peers[bob].Outbox.Messages[0].Proposal).Elem().Interface()
	_ = run(v)
	if after := reflect.ValueOf(v.Peers[bob].Outbox.Messages[0].Proposal).Elem().Interface(); after != snapshot {
		t.Fatalf("proposal mutated: %+v -> %+v", snapshot, after)
	}
	if !reflect.DeepEqual(run(v), before) {
		t.Fatal("output changed between runs")
	}
}

func versions(ns []model.Note) map[string]string {
	m := map[string]string{}
	for _, n := range ns {
		m[n.ID] = n.Version
	}
	return m
}

func TestVersionStability(t *testing.T) {
	base := versions(Evaluate(rich().v))
	churn := []struct {
		name string
		f    func(v *View)
	}{
		{"timestamps and oids", func(v *View) {
			p := v.Peers[bob]
			p.State.Heartbeat = t0.Add(time.Minute)
			items := slices.Clone(p.State.Items)
			items[0].Head, items[0].Snapshot, items[0].UpdatedAt = "head2", "snap2", t0
			items[0].Footprint.ComputedAt, items[0].Footprint.Head = t0, "head2"
			p.State.Items = items
			v.Peers[bob] = p
			cv := v.Claims[loadM.Key]
			cv.OID, cv.Claim.UpdatedAt = "oid-renewed", t0
			v.Claims[loadM.Key] = cv
			mv := v.Main[myItem]
			mv.Main = "main2"
			v.Main[myItem] = mv
		}},
		{"reference lines moved", func(v *View) {
			p := v.Peers[bob]
			items := slices.Clone(p.State.Items)
			items[0].Footprint.Refs = slices.Clone(items[0].Footprint.Refs)
			items[0].Footprint.Refs[0].Line = 99
			p.State.Items = items
			v.Peers[bob] = p
		}},
		{"a few seconds later", func(v *View) { v.Now = v.Now.Add(30 * time.Second) }},
	}
	for _, c := range churn {
		t.Run(c.name, func(t *testing.T) {
			b := rich()
			c.f(&b.v)
			if got := versions(Evaluate(b.v)); !maps.Equal(got, base) {
				t.Fatalf("versions changed:\n%v\n%v", base, got)
			}
		})
	}
}

func TestVersionRedelivery(t *testing.T) {
	calleeVersion := func(theirs model.SymbolChange) (string, string) {
		n := one(t, Evaluate(callerView(theirs).v), model.NoteCalleeChanged)
		return n.ID, n.Version
	}
	id1, v1 := calleeVersion(cfgBreaking)
	id2, v2 := calleeVersion(sigChange(cfg, "function parseConfig(string, number): Config", true))
	id3, v3 := calleeVersion(cfgCompat)
	if id1 != id2 || id1 != id3 {
		t.Fatal("note id changed with the signature")
	}
	if v1 == v2 || v1 == v3 || v2 == v3 {
		t.Fatalf("versions should differ when their signature changes: %s %s %s", v1, v2, v3)
	}
	// A new counterpart is material; so is a published contract.
	more := one(t, Evaluate(callerView(cfgBreaking).peer(carol, item(carolItem, "x", changes(cfgBreaking))).v), model.NoteCalleeChanged)
	published := one(t, Evaluate(callerView(cfgBreaking).claim(cfg.Key, bob, bobItem, 0, withContract(cfgBreaking.Symbol.Signature)).v), model.NoteCalleeChanged)
	if more.Version == v1 || published.Version == v1 || more.ID != id1 {
		t.Fatal("counterparts and contracts must change the version")
	}
}

func TestPartialViews(t *testing.T) {
	views := map[string]View{
		"zero":        {},
		"nil maps":    {Me: me, Mine: model.AgentState{Items: []model.WorkItem{item(myItem, myIntent, changes(cfgBreaking), refs(callCfg))}}, Now: t0},
		"empty peers": view().mine(item(myItem, myIntent)).with(func(v *View) { v.Peers[bob] = Peer{} }).v,
		"inconsistent": view().
			mine(item(myItem, myIntent, changes(cfgBreaking, model.SymbolChange{}), refs(model.Ref{})),
				model.WorkItem{}, item(myItem, "duplicate id")).
			peer(bob, item(myItem, "bob reuses my id"), item(bobItem, "")).
			with(func(v *View) {
				v.Peers[me] = Peer{State: model.AgentState{Items: []model.WorkItem{item("ffffffffffff", "ghost")}}}
				v.Claims["unknown"] = ClaimView{Claim: model.Claim{Owner: me, Item: "nope", Auto: true}}
				v.Claims["empty"] = ClaimView{}
				v.Claims[cfg.Key] = ClaimView{Claim: model.Claim{Owner: bob, Epoch: 5, Contract: &model.Contract{}}, Lapsed: true}
				v.Conflicts = []Conflict{{Mine: myItem, Other: "missing", Paths: []string{"a"}}, {Mine: myItem, Other: bobItem}}
				v.Main[myItem] = MainView{Footprint: model.Footprint{Symbols: []model.SymbolChange{{Change: model.ChangeRenamed}}}}
				v.Outside = []Outside{{}, {Branch: "x"}}
				v.ClashSeen["t-x"] = t0
			}).
			need(me, model.Need{Key: "nokey", Item: "missing"}).need(me, model.Need{}).
			need(bob, model.Need{Key: cfg.Key}).
			msg(model.Message{ID: "m1", Thread: "t-1", Kind: model.ThreadNote, Items: []model.ItemID{"zzz", myItem}, From: bob,
				Proposal: &model.Proposal{ID: "p-1", Owner: "them"}}).
			msg(model.Message{ID: "m1", Thread: "t-1", From: bob}).
			msg(model.Message{Thread: "t-2", From: bob}).
			msg(model.Message{ID: "m3", From: bob, Proposal: &model.Proposal{}}).
			accept(bob, "", "").accept(carol, "t-1", "p-1").v,
	}
	for name, v := range views {
		t.Run(name, func(t *testing.T) {
			out := run(v)
			checkNotes(t, out.Notes)
		})
	}
}
