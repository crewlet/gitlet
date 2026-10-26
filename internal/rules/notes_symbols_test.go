package rules

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

var (
	cfg         = fn("typescript", "src/config.ts", "parseConfig", "function parseConfig(string): Config")
	cfgBreaking = sigChange(cfg, "function parseConfig(string, boolean): Config", true)
	cfgCompat   = sigChange(cfg, "function parseConfig(string, boolean?): Config", false)
	callCfg     = ref("parseConfig", "typescript", "src/app.ts", 12, "src/config")

	loadM     = fn("typescript", "src/config.ts", "Config.load", "load(string): void")
	callLoadM = methodRef("load", "cfg", "typescript", "src/app.ts", 30)

	tsCtor    = fn("typescript", "src/config.ts", "Config.constructor", "constructor(string)")
	newCfg    = model.Ref{Name: "Config", Kind: model.RefConstruct, Lang: "typescript", Path: "src/app.ts", Line: 40, Module: "src/config", Resolved: true}
	pyInit    = fn("python", "pkg/config.py", "Config.__init__", "def __init__(self, path)")
	callPyCfg = ref("Config", "python", "pkg/app.py", 5, "pkg/config")

	storeGet       = fn("go", "internal/store/store.go", "Store.Get", "func (*Store) Get(string) ([]byte, error)")
	storeGetStrict = sigChange(storeGet, "func (*Store) Get(string, bool) ([]byte, error)", true)
	callPkgGet     = model.Ref{Name: "Get", Qualifier: "store", Kind: model.RefCall, Lang: "go", Path: "cmd/a.go", Line: 3, Module: "internal/store", Resolved: true}
)

// olderCallerView has bob's item make refs from a footprint an older gitlet
// stored without a version, while my item breaks Store.Get. A ref with a
// container, as loadCall("Config"), shows that gitlet recorded containers,
// so the package call callPkgGet names the package and reaches no method.
func olderCallerView(rs ...model.Ref) *vb {
	return view().mine(item(myItem, myIntent, changes(storeGetStrict))).
		peer(bob, item(bobItem, bobIntent, refs(rs...), olderFootprint()))
}

// loadCall is a resolved static call Config.load() into src/config, with
// the container the resolver gave it.
func loadCall(container string) model.Ref {
	r := ref("load", "typescript", "src/app.ts", 31, "src/config")
	r.Qualifier, r.Container = "Config", container
	return r
}

const (
	myIntent  = "Add strict mode to config loading"
	bobIntent = "Rework the config parser for strictness"
)

// sevCase expects one note of a kind with a severity, or none ("").
type sevCase struct {
	name string
	v    View
	want model.Severity
}

func runSev(t *testing.T, kind model.NoteKind, cases []sevCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := eval(t, tc.v)
			if tc.want == "" {
				none(t, ns, kind)
				return
			}
			if n := one(t, ns, kind); n.Severity != tc.want {
				t.Fatalf("severity = %s, want %s: %s", n.Severity, tc.want, n.Title)
			}
		})
	}
}

func callerView(theirs model.SymbolChange, mine ...itemOpt) *vb {
	return view().
		mine(item(myItem, myIntent, append([]itemOpt{refs(callCfg)}, mine...)...)).
		peer(bob, item(bobItem, bobIntent, changes(theirs)))
}

func TestCalleeChanged(t *testing.T) {
	runSev(t, model.NoteCalleeChanged, []sevCase{
		{"breaking and resolved", callerView(cfgBreaking).v, model.SeverityAct},
		{"compatible", callerView(cfgCompat).v, model.SeverityInfo},
		{"removed", callerView(removedChange(cfg)).v, model.SeverityAct},
		{"renamed", callerView(renamedChange(cfg, "parseSettings")).v, model.SeverityAct},
		{"moved to another module", callerView(movedChange(cfg, "src/settings.ts", true)).v, model.SeverityAct},
		{"moved within its module", callerView(movedChange(cfg, "src/config.ts", false)).v, ""},
		{"body only", callerView(bodyChange(cfg)).v, ""},
		{"not referenced", view().mine(item(myItem, myIntent, refs(ref("other", "typescript", "src/app.ts", 1, "src/config")))).
			peer(bob, item(bobItem, bobIntent, changes(cfgBreaking))).v, ""},
		{"unresolved unique method", view().mine(item(myItem, myIntent, refs(callLoadM))).
			peer(bob, item(bobItem, bobIntent, changes(sigChange(loadM, "load(string, boolean): void", true)))).v, model.SeverityInfo},
		{"unresolved method name not unique", view().mine(item(myItem, myIntent, refs(callLoadM))).
			peer(bob, item(bobItem, bobIntent, changes(sigChange(loadM, "load(string, boolean): void", true)))).
			peer(carol, item(carolItem, "Speed up the store", changes(bodyChange(fn("typescript", "src/db.ts", "Store.load", "load(): Row"))))).v, ""},
		{"typescript constructor through new", view().mine(item(myItem, myIntent, refs(newCfg))).
			peer(bob, item(bobItem, bobIntent, changes(sigChange(tsCtor, "constructor(string, number)", true)))).v, model.SeverityAct},
		{"python __init__ through a call of the class", view().mine(item(myItem, myIntent, refs(callPyCfg))).
			peer(bob, item(bobItem, bobIntent, changes(sigChange(pyInit, "def __init__(self, path, mode)", true)))).v, model.SeverityAct},
		{"method through its class", view().mine(item(myItem, myIntent, refs(loadCall("Config")))).
			peer(bob, item(bobItem, bobIntent, changes(sigChange(loadM, "load(string, boolean): void", true)))).v, model.SeverityAct},
		{"method through its class, from an older gitlet's footprint", view().mine(item(myItem, myIntent, refs(loadCall("")), olderFootprint())).
			peer(bob, item(bobItem, bobIntent, changes(sigChange(loadM, "load(string, boolean): void", true)))).v, model.SeverityAct},
		{"no container in a current footprint matches only as unresolved", view().mine(item(myItem, myIntent, refs(loadCall("")))).
			peer(bob, item(bobItem, bobIntent, changes(sigChange(loadM, "load(string, boolean): void", true)))).v, model.SeverityInfo},
		{"suppressed by my need", callerView(cfgBreaking).need(me, model.Need{Key: cfg.Key, Name: cfg.Ref(), Item: myItem}).v, ""},
		{"not suppressed by a need of my other item", callerView(cfgBreaking).need(me, model.Need{Key: cfg.Key, Item: myItem2}).v, model.SeverityAct},
		{"I change it too", callerView(cfgBreaking, changes(bodyChange(cfg))).v, ""},
		{"lapsed and unsubmitted drops a level", callerView(cfgBreaking).lapsed(bob, t0.Add(-time.Hour)).v, model.SeverityWarn},
		{"lapsed but submitted keeps act", view().mine(item(myItem, myIntent, refs(callCfg))).
			peer(bob, item(bobItem, bobIntent, changes(cfgBreaking), submitted())).lapsed(bob, t0.Add(-time.Hour)).v, model.SeverityAct},
		{"lapsed past staleItem is ignored", callerView(cfgBreaking).lapsed(bob, t0.Add(-30*time.Hour)).v, ""},
		{"landed items are out of view", view().mine(item(myItem, myIntent, refs(callCfg))).
			peer(bob, item(bobItem, bobIntent, changes(cfgBreaking), status(model.ItemLanded))).v, ""},
		{"my other item", view().mine(item(myItem, myIntent, refs(callCfg)), item(myItem2, bobIntent, changes(cfgBreaking))).v, model.SeverityAct},
	})
}

func TestCalleeChangedContent(t *testing.T) {
	b := callerView(cfgBreaking).
		peer(carol, item(carolItem, "Make parseConfig async", changes(sigChange(cfg, "async function parseConfig(string): Promise<Config>", true))))
	ns := eval(t, b.v)
	n := one(t, ns, model.NoteCalleeChanged)
	if n.Item != myItem || n.Subject != cfg.Key || n.SubjectName != "src/config.ts:parseConfig" {
		t.Fatalf("note identity: item %s subject %s name %s", n.Item, n.Subject, n.SubjectName)
	}
	if len(n.Parties) != 2 || n.Parties[0].Agent != bob || n.Parties[1].Agent != carol || n.Parties[0].Intent != bobIntent {
		t.Fatalf("parties = %+v", n.Parties)
	}
	for _, want := range []string{string(bob), bobIntent, "src/config.ts:parseConfig", "and 1 more are changing the signature of", "calls"} {
		if !strings.Contains(n.Title, want) {
			t.Errorf("title %q lacks %q", n.Title, want)
		}
	}
	ev := strings.Join(n.Evidence, "\n")
	for _, want := range []string{
		"codex-ci-bbbbbb: was: function parseConfig(string): Config",
		"codex-ci-bbbbbb: now: function parseConfig(string, boolean): Config  (breaking)",
		"cursor-desk-cccccc: now: async function parseConfig(string): Promise<Config>  (breaking)",
		"your references: src/app.ts:12",
	} {
		if !strings.Contains(ev, want) {
			t.Errorf("evidence lacks %q:\n%s", want, ev)
		}
	}
	if !slices.Equal(tools(n), []string{"need", "discuss", "dismiss"}) {
		t.Fatalf("options = %v", tools(n))
	}
	wantArgs(t, optionFor(t, n, "need"), map[string]any{"symbol": "src/config.ts:parseConfig", "note": n.ID})
	wantArgs(t, optionFor(t, n, "discuss"), map[string]any{"about": n.ID})
	wantArgs(t, optionFor(t, n, "dismiss"), map[string]any{"note": n.ID, "as": "not_needed"})
	if want := model.NoteThreadID(model.NoteCalleeChanged, cfg.Key, myItem, bobItem, carolItem); n.Thread != want {
		t.Fatalf("thread = %s, want %s", n.Thread, want)
	}
}

func TestCalleeChangedUsesTheirThread(t *testing.T) {
	// Bob opened a thread from his callers_affected note; my note joins it.
	thread := model.NoteThreadID(model.NoteCallersAffected, cfg.Key, bobItem, myItem)
	b := callerView(cfgBreaking).msg(model.Message{ID: "m00000000001", Thread: thread, Kind: model.ThreadNote,
		Subject: cfg.Key, NoteKind: model.NoteCallersAffected, Items: []model.ItemID{bobItem, myItem},
		From: bob, Item: bobItem, Text: "I am adding a strict flag", At: t0})
	n := one(t, eval(t, b.v), model.NoteCalleeChanged)
	if n.Thread != thread {
		t.Fatalf("thread = %s, want bob's %s", n.Thread, thread)
	}
}

func TestCallersAffected(t *testing.T) {
	mine := func(ch model.SymbolChange) *vb {
		return view().mine(item(myItem, myIntent, changes(ch))).peer(bob, item(bobItem, bobIntent, refs(callCfg)))
	}
	runSev(t, model.NoteCallersAffected, []sevCase{
		{"breaking", mine(cfgBreaking).v, model.SeverityAct},
		{"compatible", mine(cfgCompat).v, model.SeverityInfo},
		{"removed", mine(removedChange(cfg)).v, model.SeverityAct},
		{"body only", mine(bodyChange(cfg)).v, ""},
		{"they need it", mine(cfgBreaking).need(bob, model.Need{Key: cfg.Key, Item: bobItem}).v, ""},
		{"they change it too", view().mine(item(myItem, myIntent, changes(cfgBreaking))).
			peer(bob, item(bobItem, bobIntent, refs(callCfg), changes(bodyChange(cfg)))).v, ""},
		{"unresolved method", view().mine(item(myItem, myIntent, changes(sigChange(loadM, "load(): void", true)))).
			peer(bob, item(bobItem, bobIntent, refs(callLoadM))).v, model.SeverityInfo},
		{"lapsed caller", mine(cfgBreaking).lapsed(bob, t0.Add(-time.Hour)).v, model.SeverityWarn},
		{"package call from an older gitlet that recorded containers", olderCallerView(callPkgGet, loadCall("Config")).v, model.SeverityInfo},
		{"package call from an older gitlet before containers", olderCallerView(callPkgGet).v, model.SeverityAct},
	})

	tests := []struct {
		name  string
		b     *vb
		tools []string
	}{
		{"unclaimed", mine(cfgBreaking), []string{"claim", "publish", "discuss"}},
		{"claimed by me", mine(cfgBreaking).claim(cfg.Key, me, myItem, 0), []string{"publish", "discuss"}},
		{"my contract is current", mine(cfgBreaking).claim(cfg.Key, me, myItem, 0, withContract(cfgBreaking.Symbol.Signature)), []string{"discuss"}},
		{"my contract is old", mine(cfgBreaking).claim(cfg.Key, me, myItem, 0, withContract("function parseConfig(): Config")), []string{"publish", "discuss"}},
		{"claimed by carol", mine(cfgBreaking).claim(cfg.Key, carol, carolItem, 0), []string{"need", "discuss"}},
		{"removed", mine(removedChange(cfg)), []string{"discuss"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := one(t, eval(t, tt.b.v), model.NoteCallersAffected)
			if !slices.Equal(tools(n), tt.tools) {
				t.Fatalf("options = %v, want %v", tools(n), tt.tools)
			}
			for _, o := range n.Options {
				if o.Tool == "claim" || o.Tool == "publish" {
					wantArgs(t, o, map[string]any{"symbol": "src/config.ts:parseConfig", "note": n.ID})
				}
			}
			if !strings.Contains(strings.Join(n.Evidence, "\n"), "codex-ci-bbbbbb references it: src/app.ts:12") {
				t.Fatalf("evidence = %q", n.Evidence)
			}
		})
	}
}

func clashView(mine, theirs model.SymbolChange) *vb {
	return view().mine(item(myItem, myIntent, changes(mine))).peer(bob, item(bobItem, bobIntent, changes(theirs)))
}

func TestSignatureClash(t *testing.T) {
	other := sigChange(cfg, "function parseConfig(string, Options): Config", true)
	// src/util.js renamed to src/util.ts, adding types to parse: the change
	// starts from the JavaScript symbol.
	utilJS := fn("javascript", "src/util.js", "parse", "function parse(_)")
	jsToTS := model.SymbolChange{Change: model.ChangeMoved, SigChanged: true, OldPath: utilJS.Path, OldLang: utilJS.Lang,
		OldSignature: utilJS.Signature, OldSigHash: utilJS.SigHash, Symbol: fn("typescript", "src/util.ts", "parse", "function parse(string)")}
	newSym := fn("go", "internal/x/x.go", "Strict", "func Strict() bool")
	newSym2 := newSym
	newSym2.Signature, newSym2.SigHash = "func Strict(int) bool", hashOf("func Strict(int) bool")
	runSev(t, model.NoteSignatureClash, []sevCase{
		{"different signatures", clashView(cfgBreaking, other).v, model.SeverityAct},
		{"same new signature", clashView(cfgBreaking, cfgBreaking).v, ""},
		{"removed while I change it", clashView(cfgBreaking, removedChange(cfg)).v, model.SeverityAct},
		{"I remove what they change", clashView(removedChange(cfg), other).v, model.SeverityAct},
		{"both remove", clashView(removedChange(cfg), removedChange(cfg)).v, ""},
		{"both add, different signatures", clashView(addedChange(newSym), addedChange(newSym2)).v, model.SeverityAct},
		{"both add, same signature", clashView(addedChange(newSym), addedChange(newSym)).v, ""},
		{"compatible changes clash too", clashView(cfgCompat, other).v, model.SeverityAct},
		{"moved to TypeScript while they change the JavaScript", clashView(jsToTS, sigChange(utilJS, "function parse(_, _)", true)).v, model.SeverityAct},
		{"body against signature is body_overlap", clashView(bodyChange(cfg), other).v, ""},
		{"I need it: I deferred", clashView(cfgBreaking, other).need(me, model.Need{Key: cfg.Key, Item: myItem}).v, ""},
		{"they need it: they deferred", clashView(cfgBreaking, other).need(bob, model.Need{Key: cfg.Key, Item: bobItem}).v, ""},
		{"my need does not count while I hold the claim", clashView(cfgBreaking, other).need(me, model.Need{Key: cfg.Key, Item: myItem}).
			claim(cfg.Key, me, myItem, 0).v, model.SeverityAct},
		{"lapsed rival", clashView(cfgBreaking, other).lapsed(bob, t0.Add(-time.Hour)).v, model.SeverityWarn},
		{"settled thread gives an agreement instead", clashView(cfgBreaking, other).
			with(func(v *View) { v.ClashSeen[model.SigThreadID(cfg.Key, 0)] = t0.Add(-11 * time.Minute) }).v, ""},
	})

	n := one(t, eval(t, clashView(cfgBreaking, other).v), model.NoteSignatureClash)
	thread := model.SigThreadID(cfg.Key, 0)
	if n.Thread != thread || !slices.Equal(tools(n), []string{"discuss", "need", "dismiss"}) {
		t.Fatalf("thread %s options %v", n.Thread, tools(n))
	}
	wantArgs(t, optionFor(t, n, "discuss"), map[string]any{"about": thread})
	wantArgs(t, optionFor(t, n, "need"), map[string]any{"symbol": "src/config.ts:parseConfig", "note": n.ID})
	if !strings.Contains(n.Title, "also changes the signature of src/config.ts:parseConfig") {
		t.Fatalf("title = %q", n.Title)
	}
	ev := strings.Join(n.Evidence, "\n")
	if !strings.Contains(ev, "you: now: function parseConfig(string, boolean): Config") ||
		!strings.Contains(ev, "codex-ci-bbbbbb: now: function parseConfig(string, Options): Config") {
		t.Fatalf("evidence:\n%s", ev)
	}

	held := clashView(cfgBreaking, other).claim(cfg.Key, bob, bobItem, 2)
	if n := one(t, eval(t, held.v), model.NoteSignatureClash); n.Thread != model.SigThreadID(cfg.Key, 2) {
		t.Fatalf("thread at epoch 2 = %s", n.Thread)
	}
}

func TestBodyOverlap(t *testing.T) {
	theirSig := sigChange(cfg, "function parseConfig(string, Options): Config", true)
	runSev(t, model.NoteBodyOverlap, []sevCase{
		{"my body, their signature", clashView(bodyChange(cfg), theirSig).v, model.SeverityAct},
		{"my body, they remove it", clashView(bodyChange(cfg), removedChange(cfg)).v, model.SeverityAct},
		{"my signature, their body", clashView(cfgBreaking, bodyChange(cfg)).v, model.SeverityInfo},
		{"my signature, their body, conflict", clashView(cfgBreaking, bodyChange(cfg)).conflict(myItem, bobItem, "src/config.ts").v, model.SeverityWarn},
		{"both bodies", clashView(bodyChange(cfg), bodyChange(cfg)).v, model.SeverityInfo},
		{"both bodies, conflict in the file", clashView(bodyChange(cfg), bodyChange(cfg)).conflict(myItem, bobItem, "src/config.ts").v, model.SeverityWarn},
		{"conflict in another file", clashView(bodyChange(cfg), bodyChange(cfg)).conflict(myItem, bobItem, "src/other.ts").v, model.SeverityInfo},
		{"different symbols", clashView(bodyChange(cfg), bodyChange(loadM)).v, ""},
		{"both signatures is a clash", clashView(cfgBreaking, theirSig).v, ""},
		{"lapsed", clashView(bodyChange(cfg), theirSig).lapsed(bob, t0.Add(-time.Hour)).v, model.SeverityWarn},
	})
	n := one(t, eval(t, clashView(bodyChange(cfg), theirSig).v), model.NoteBodyOverlap)
	if !slices.Equal(tools(n), []string{"discuss", "dismiss"}) || n.Thread != model.NoteThreadID(model.NoteBodyOverlap, cfg.Key, myItem, bobItem) {
		t.Fatalf("options %v thread %s", tools(n), n.Thread)
	}
	if !strings.Contains(n.Title, "whose body you edit") {
		t.Fatalf("title = %q", n.Title)
	}
}

func TestStale(t *testing.T) {
	onMain := func(chs ...model.SymbolChange) *vb {
		return view().mine(item(myItem, myIntent, refs(callCfg, callLoadM))).
			main(myItem, model.Footprint{Symbols: chs})
	}
	runSev(t, model.NoteStale, []sevCase{
		{"breaking signature", onMain(cfgBreaking).v, model.SeverityAct},
		{"compatible signature", onMain(cfgCompat).v, ""},
		{"removed", onMain(removedChange(cfg)).v, model.SeverityAct},
		{"renamed", onMain(renamedChange(cfg, "parseSettings")).v, model.SeverityAct},
		{"moved, module unchanged", onMain(movedChange(cfg, "src/config.ts", false)).v, ""},
		{"body only", onMain(bodyChange(cfg)).v, ""},
		{"unresolved method", onMain(sigChange(loadM, "load(): void", true)).v, model.SeverityInfo},
		{"skipped", onMain(cfgBreaking).with(func(v *View) {
			mv := v.Main[myItem]
			mv.Skipped = true
			v.Main[myItem] = mv
		}).v, ""},
		{"no main view", view().mine(item(myItem, myIntent, refs(callCfg))).v, ""},
	})
	n := one(t, eval(t, onMain(cfgBreaking).v), model.NoteStale)
	if !slices.Equal(tools(n), []string{"update"}) || n.Thread != "" || len(n.Parties) != 0 {
		t.Fatalf("options %v thread %q parties %v", tools(n), n.Thread, n.Parties)
	}
	wantArgs(t, n.Options[0], map[string]any{"note": n.ID})
	if !strings.HasPrefix(n.Title, "Main changed the signature of src/config.ts:parseConfig") {
		t.Fatalf("title = %q", n.Title)
	}
	if !strings.Contains(n.Body, "fix your references") {
		t.Fatalf("body = %q", n.Body)
	}
	// The stub I use landed: my branch already has main's change.
	used := view().mine(item(myItem, myIntent, changes(cfgBreaking), refs(callCfg))).
		main(myItem, model.Footprint{Symbols: []model.SymbolChange{cfgBreaking}})
	n = one(t, eval(t, used.v), model.NoteStale)
	if n.Severity != model.SeverityAct || !strings.Contains(n.Body, "nothing to fix") || strings.Contains(n.Options[0].Label, "fix") {
		t.Fatalf("severity %s body %q label %q", n.Severity, n.Body, n.Options[0].Label)
	}
}

func TestNewCallers(t *testing.T) {
	mainRefs := func(ch model.SymbolChange, rs ...model.Ref) *vb {
		return view().mine(item(myItem, myIntent, changes(ch))).main(myItem, model.Footprint{Refs: rs})
	}
	cli := ref("parseConfig", "typescript", "src/cli.ts", 4, "src/config")
	// Main changed the symbol too, along with its new callers.
	mainChanged := func(mine, theirs model.SymbolChange) *vb {
		return view().mine(item(myItem, myIntent, changes(mine))).
			main(myItem, model.Footprint{Symbols: []model.SymbolChange{theirs}, Refs: []model.Ref{cli}})
	}
	runSev(t, model.NoteNewCallers, []sevCase{
		{"breaking", mainRefs(cfgBreaking, cli).v, model.SeverityAct},
		{"compatible", mainRefs(cfgCompat, cli).v, ""},
		{"unresolved", mainRefs(sigChange(loadM, "load(): void", true), methodRef("load", "c", "typescript", "src/cli.ts", 9)).v, model.SeverityInfo},
		{"no new references", mainRefs(cfgBreaking).v, ""},
		// A stub I picked landed: main's new callers use my signature.
		{"main made the same change", mainChanged(cfgBreaking, cfgBreaking).v, ""},
		{"main changed it differently", mainChanged(cfgBreaking, cfgCompat).v, model.SeverityAct},
	})
	n := one(t, eval(t, mainRefs(cfgBreaking,
		ref("parseConfig", "typescript", "src/cli.ts", 4, "src/config"),
		ref("parseConfig", "typescript", "src/cli.ts", 9, "src/config")).v), model.NoteNewCallers)
	if !strings.Contains(strings.Join(n.Evidence, "\n"), "new references on main: src/cli.ts:4, src/cli.ts:9") {
		t.Fatalf("evidence = %q", n.Evidence)
	}
	wantArgs(t, optionFor(t, n, "update"), map[string]any{"note": n.ID})
}
