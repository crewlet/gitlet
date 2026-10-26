package rules

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

var (
	bobSig   = sigChange(cfg, "function parseConfig(string, Options): Config", true)
	sigID    = model.SigThreadID(cfg.Key, 0)
	propMine = &model.Proposal{ID: "p-00000000000a", Owner: me, Signature: "parseConfig(string, boolean): Config", Summary: "add a strict flag"}
	propBob  = &model.Proposal{ID: "p-00000000000b", Owner: bob, Signature: "parseConfig(string, Options): Config", Summary: "take options"}
)

// clash is me and bob changing parseConfig's signature differently.
func clash() *vb { return clashView(cfgBreaking, bobSig) }

func say(from model.AgentID, it model.ItemID, id string, at time.Duration, text string, p *model.Proposal, seen ...string) model.Message {
	return model.Message{ID: id, Thread: sigID, Kind: model.ThreadSignature, Subject: cfg.Key, SubjectName: cfg.Ref(),
		Items: []model.ItemID{myItem, bobItem}, From: from, Item: it, Text: text, Proposal: p, Seen: seen, At: t0.Add(at)}
}

func sigThread(t *testing.T, v View) model.ThreadState {
	t.Helper()
	for _, th := range Threads(v) {
		if th.Kind == model.ThreadSignature {
			return th
		}
	}
	t.Fatalf("no signature thread in %+v", Threads(v))
	return model.ThreadState{}
}

func actions(ds []Decision) [][]Action {
	var out [][]Action
	for _, d := range ds {
		out = append(out, d.Actions)
	}
	return out
}

func TestSignatureThread(t *testing.T) {
	v := clash().v
	th := sigThread(t, v)
	want := []model.Participant{{Agent: me, Item: myItem}, {Agent: bob, Item: bobItem}}
	slices.SortFunc(want, func(a, b model.Participant) int { return strings.Compare(string(a.Agent), string(b.Agent)) })
	if th.ID != sigID || th.Subject != cfg.Key || th.SubjectName != cfg.Ref() || !slices.Equal(th.Participants, want) {
		t.Fatalf("thread = %+v", th)
	}
	if th.Settled || !th.OpenedAt.Equal(t0) || !th.Deadline.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("unseen clash counts as seen now: opened %v deadline %v settled %t", th.OpenedAt, th.Deadline, th.Settled)
	}
	// The claim holder joins, and the epoch names the thread.
	held := clash().claim(cfg.Key, carol, carolItem, 3).v
	th = sigThread(t, held)
	if th.ID != model.SigThreadID(cfg.Key, 3) || len(th.Participants) != 3 {
		t.Fatalf("held thread = %+v", th)
	}
	// No clash, no thread.
	if got := Threads(clashView(cfgBreaking, cfgBreaking).v); len(got) != 0 {
		t.Fatalf("threads without a clash: %+v", got)
	}
	// Threads I am not part of are not mine.
	others := view().mine(item(myItem, myIntent)).peer(bob, item(bobItem, bobIntent, changes(cfgBreaking))).
		peer(carol, item(carolItem, "x", changes(bobSig))).v
	if got := Threads(others); len(got) != 0 {
		t.Fatalf("threads of others: %+v", got)
	}
}

func TestNegotiation(t *testing.T) {
	timedOut := func(b *vb) *vb {
		return b.with(func(v *View) { v.ClashSeen[sigID] = t0.Add(-10 * time.Minute) })
	}
	exhaust := func(b *vb) *vb {
		for i, a := range []model.AgentID{me, bob, me, bob, me, bob} {
			it := myItem
			if a == bob {
				it = bobItem
			}
			b.msg(say(a, it, "m-"+string(rune('a'+i)), time.Duration(i)*time.Second, "I need my version", nil))
		}
		return b
	}
	tests := []struct {
		name     string
		b        *vb
		settled  bool
		fellBack bool
		owner    model.AgentID
		mine     [][]Action // my decisions
		bobs     [][]Action // bob's decisions, from his view
	}{
		{"open", clash(), false, false, "", nil, nil},
		{"agreement: they propose, I accept",
			clash().msg(say(bob, bobItem, "m1", 0, "you own it", propMine)).accept(me, sigID, propMine.ID),
			true, false, me, [][]Action{{ActClaim}}, [][]Action{{ActNeed}}},
		{"agreement: the proposer accepts implicitly",
			clash().msg(say(me, myItem, "m1", 0, "you own it", propBob)).accept(bob, sigID, propBob.ID),
			true, false, bob, [][]Action{{ActNeed}}, [][]Action{{ActClaim}}},
		{"a proposal alone is no agreement",
			clash().msg(say(me, myItem, "m1", 0, "I own it", propMine)), false, false, "", nil, nil},
		{"accepting another proposal is no agreement",
			clash().msg(say(me, myItem, "m1", 0, "I own it", propMine)).msg(say(bob, bobItem, "m2", time.Second, "no, I do", propBob, "m1")).
				accept(me, sigID, propBob.ID+"x"), false, false, "", nil, nil},
		{"agreement with me holding: I transfer and need",
			clash().claim(cfg.Key, me, myItem, 0).msg(say(me, myItem, "m1", 0, "you own it", propBob)).accept(bob, sigID, propBob.ID),
			true, false, bob, [][]Action{{ActTransfer, ActNeed}}, [][]Action{{ActClaim}}},
		{"bound by messages, nobody holds: race",
			exhaust(clash()), true, true, "", [][]Action{{ActRace}}, [][]Action{{ActRace}}},
		{"bound by timeout, nobody holds: race",
			timedOut(clash()), true, true, "", [][]Action{{ActRace}}, [][]Action{{ActRace}}},
		{"bound by timeout, I hold: I own",
			timedOut(clash().claim(cfg.Key, me, myItem, 0)), true, true, me, nil, [][]Action{{ActNeed}}},
		{"bound by timeout, they hold: I need",
			timedOut(clash().claim(cfg.Key, bob, bobItem, 0)), true, true, bob, [][]Action{{ActNeed}}, nil},
		{"agreement wins over the bound",
			timedOut(clash().claim(cfg.Key, me, myItem, 0)).msg(say(me, myItem, "m1", 0, "you own it", propBob)).accept(bob, sigID, propBob.ID),
			true, false, bob, [][]Action{{ActTransfer, ActNeed}}, [][]Action{{ActClaim}}},
		{"just before the timeout",
			clash().with(func(v *View) { v.ClashSeen[sigID] = t0.Add(-9 * time.Minute) }), false, false, "", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			th := sigThread(t, tt.b.v)
			if th.Settled != tt.settled || th.FellBack != tt.fellBack || th.Owner != tt.owner {
				t.Fatalf("settled %t fellBack %t owner %q, want %t %t %q", th.Settled, th.FellBack, th.Owner, tt.settled, tt.fellBack, tt.owner)
			}
			if got := actions(Decisions(tt.b.v)); !slices.EqualFunc(got, tt.mine, slices.Equal) {
				t.Fatalf("my actions = %v, want %v", got, tt.mine)
			}
			bv := tt.b.as(bob)
			if got := actions(Decisions(bv)); !slices.EqualFunc(got, tt.bobs, slices.Equal) {
				t.Fatalf("bob's actions = %v, want %v", got, tt.bobs)
			}
			// Both servers derive the same thread and outcome.
			bt := sigThread(t, bv)
			if bt.ID != th.ID || bt.Owner != th.Owner || bt.FellBack != th.FellBack ||
				(bt.Agreement == nil) != (th.Agreement == nil) || !slices.Equal(bt.Participants, th.Participants) {
				t.Fatalf("bob sees %+v, I see %+v", bt, th)
			}
		})
	}
}

func TestBoundOutcomeIndependentOfClock(t *testing.T) {
	// My server reaches the timeout; bob's server first saw the clash later
	// and has not. When bob's server gets there it derives the same outcome.
	b := clash().claim(cfg.Key, bob, bobItem, 0).with(func(v *View) { v.ClashSeen[sigID] = t0.Add(-11 * time.Minute) })
	mine := sigThread(t, b.v)
	bv := b.as(bob)
	bv.ClashSeen = map[string]time.Time{sigID: t0.Add(-2 * time.Minute)}
	if sigThread(t, bv).Settled {
		t.Fatal("bob's server should not be at its bound yet")
	}
	bv.Now = t0.Add(9 * time.Minute)
	theirs := sigThread(t, bv)
	if !mine.Settled || !theirs.Settled || mine.Owner != theirs.Owner || mine.Owner != bob || !theirs.FellBack {
		t.Fatalf("outcomes differ: mine %+v theirs %+v", mine, theirs)
	}
}

func TestDecisionDetails(t *testing.T) {
	b := clash().claim(cfg.Key, me, myItem, 0, withSymbol(cfg)).
		msg(say(me, myItem, "m1", 0, "I need it to keep returning Config", nil)).
		msg(say(bob, bobItem, "m2", time.Second, "I need Options", nil, "m1")).
		msg(say(me, myItem, "m3", 2*time.Second, "fine, you own it", propBob, "m2")).
		accept(bob, sigID, propBob.ID)
	ds := Decisions(b.v)
	if len(ds) != 1 {
		t.Fatalf("decisions = %+v", ds)
	}
	d := ds[0]
	if d.Thread != sigID || d.Key != cfg.Key || d.Owner != bob || d.OwnerItem != bobItem || d.FellBack ||
		d.ClaimOID != "oid-"+cfg.Key[:6] || d.Epoch != 0 || d.Proposal == nil || d.Proposal.ID != propBob.ID {
		t.Fatalf("decision = %+v", d)
	}
	if d.Requirement != "fine, you own it" {
		t.Fatalf("requirement = %q", d.Requirement)
	}
	if len(d.Requirements) != 1 || d.Requirements[0] != (model.Requirement{From: me, Text: "fine, you own it"}) {
		t.Fatalf("requirements = %+v", d.Requirements)
	}
	if d.Symbol.Key != cfg.Key || d.Symbol.Qualified != "parseConfig" {
		t.Fatalf("symbol = %+v", d.Symbol)
	}
}

func TestQuota(t *testing.T) {
	// My fourth message is over the quota: its proposal does not count.
	b := clash()
	for i, id := range []string{"m1", "m2", "m3"} {
		b.msg(say(me, myItem, id, time.Duration(i)*time.Second, "thinking", nil))
	}
	b.msg(say(me, myItem, "m4", 5*time.Second, "you own it", propBob)).accept(bob, sigID, propBob.ID)
	th := sigThread(t, b.v)
	if th.Agreement != nil || th.Settled {
		t.Fatalf("over-quota proposal counted: %+v", th)
	}
	if len(th.Messages) != 4 {
		t.Fatalf("over-quota messages are still shown: got %d", len(th.Messages))
	}
	// A custom quota of one message each settles as soon as both spoke.
	one := clash().with(func(v *View) { v.Limits.ThreadMessages = 1 }).
		msg(say(me, myItem, "m1", 0, "mine", nil)).msg(say(bob, bobItem, "m2", time.Second, "mine", nil))
	if th := sigThread(t, one.v); !th.Settled || !th.FellBack {
		t.Fatalf("quota of one: %+v", th)
	}
}

func TestEpochChangeStartsANewThread(t *testing.T) {
	// The agreement at epoch 0 moved the claim to bob at epoch 1. The old
	// thread is no longer current; until I record my need, I still get it.
	b := clashView(cfgBreaking, bobSig).
		msg(say(me, myItem, "m1", 0, "you own it", propBob)).accept(bob, sigID, propBob.ID).
		claim(cfg.Key, bob, bobItem, 1, withSymbol(cfg))
	th := sigThread(t, b.v)
	if th.ID != model.SigThreadID(cfg.Key, 1) || th.ID == sigID || len(th.Messages) != 0 {
		t.Fatalf("current thread = %+v", th)
	}
	ds := Decisions(b.v)
	if len(ds) != 1 || ds[0].Thread != sigID || !slices.Equal(ds[0].Actions, []Action{ActNeed}) || ds[0].Owner != bob {
		t.Fatalf("decisions after transfer = %+v", ds)
	}
	ns := eval(t, b.v)
	none(t, ns, model.NoteSignatureClash)
	if a := one(t, ns, model.NoteAgreement); a.Thread != sigID || !slices.Equal(tools(a), []string{"need"}) {
		t.Fatalf("agreement = %+v", a)
	}
	// Once my need is recorded, I no longer contend: no clash, no thread.
	done := b.need(me, myNeed(t0))
	if got := Decisions(done.v); len(got) != 0 {
		t.Fatalf("decisions once done = %+v", got)
	}
	if got := Threads(done.v); len(got) != 0 {
		t.Fatalf("threads once done = %+v", got)
	}
	ns = eval(t, done.v)
	none(t, ns, model.NoteAgreement)
	none(t, ns, model.NoteSignatureClash)
	// The owner keeps nothing to do but publish (needed).
	if n := one(t, Evaluate(done.as(bob)), model.NoteNeeded); n.Severity != model.SeverityAct {
		t.Fatalf("owner's needed note = %+v", n)
	}
}

func TestActionsAreIdempotentUntilDone(t *testing.T) {
	agreed := clash().msg(say(bob, bobItem, "m1", 0, "you own it", propMine)).accept(me, sigID, propMine.ID)
	if got := actions(Decisions(agreed.v)); !slices.EqualFunc(got, [][]Action{{ActClaim}}, slices.Equal) {
		t.Fatalf("before claim: %v", got)
	}
	if got := actions(Decisions(agreed.v)); !slices.EqualFunc(got, [][]Action{{ActClaim}}, slices.Equal) {
		t.Fatalf("asked again: %v", got)
	}
	claimed := agreed.claim(cfg.Key, me, myItem, 0)
	if got := Decisions(claimed.v); len(got) != 0 {
		t.Fatalf("after claim: %+v", got)
	}
	race := clash().with(func(v *View) { v.ClashSeen[sigID] = t0.Add(-time.Hour) })
	if got := actions(Decisions(race.v)); !slices.EqualFunc(got, [][]Action{{ActRace}}, slices.Equal) {
		t.Fatalf("race: %v", got)
	}
	// Bob won the race: he owns, I need.
	won := race.claim(cfg.Key, bob, bobItem, 0)
	if got := actions(Decisions(won.v)); !slices.EqualFunc(got, [][]Action{{ActNeed}}, slices.Equal) {
		t.Fatalf("after the race: %v", got)
	}
	// My need recorded: I deferred, so the clash and its thread are gone.
	needed := won.need(me, myNeed(t0))
	if got := Decisions(needed.v); len(got) != 0 {
		t.Fatalf("after my need: %+v", got)
	}
	if got := Threads(needed.v); len(got) != 0 {
		t.Fatalf("threads after my need: %+v", got)
	}
}

func TestCausalOrder(t *testing.T) {
	// m2 saw m3 although its clock is earlier: causality wins over time. m4
	// and m5 saw each other (malformed); the cycle waits until nothing else
	// is ready and is then broken at its earliest message.
	msgs := []model.Message{
		{ID: "m2", From: bob, At: t0, Seen: []string{"m3"}},
		{ID: "m3", From: me, At: t0.Add(time.Minute)},
		{ID: "m1", From: me, At: t0.Add(-time.Minute)},
		{ID: "m4", From: bob, At: t0.Add(-time.Hour), Seen: []string{"m5"}},
		{ID: "m5", From: me, At: t0.Add(-time.Hour), Seen: []string{"m4"}}, // a cycle
	}
	var ids []string
	for _, m := range causal(msgs) {
		ids = append(ids, m.ID)
	}
	if !slices.Equal(ids, []string{"m1", "m3", "m2", "m5", "m4"}) {
		t.Fatalf("order = %v", ids)
	}
}

func TestMessageNote(t *testing.T) {
	note := model.NoteThreadID(model.NoteBodyOverlap, cfg.Key, myItem, bobItem)
	bodyMsg := func(id string, from model.AgentID, it model.ItemID, text string, p *model.Proposal, seen ...string) model.Message {
		return model.Message{ID: id, Thread: note, Kind: model.ThreadNote, Subject: cfg.Key, SubjectName: cfg.Ref(),
			NoteKind: model.NoteBodyOverlap, Items: []model.ItemID{myItem, bobItem}, From: from, Item: it, Text: text, Proposal: p, Seen: seen, At: t0}
	}
	base := clashView(bodyChange(cfg), bodyChange(cfg))
	tests := []struct {
		name  string
		b     *vb
		want  bool
		tools []string
	}{
		{"new message", base.msg(bodyMsg("m1", bob, bobItem, "I am editing the loop", nil)), true, []string{"discuss"}},
		{"with a proposal", clashView(bodyChange(cfg), bodyChange(cfg)).msg(bodyMsg("m1", bob, bobItem, "let me own it", propBob)), true, []string{"discuss", "accept"}},
		{"I replied", clashView(bodyChange(cfg), bodyChange(cfg)).msg(bodyMsg("m1", bob, bobItem, "hi", nil)).
			msg(bodyMsg("m2", me, myItem, "ok", nil, "m1")), false, nil},
		{"I replied to a later message", clashView(bodyChange(cfg), bodyChange(cfg)).msg(bodyMsg("m1", bob, bobItem, "hi", nil)).
			msg(bodyMsg("m2", bob, bobItem, "again", nil, "m1")).msg(bodyMsg("m3", me, myItem, "ok", nil, "m2")), false, nil},
		{"I accepted", clashView(bodyChange(cfg), bodyChange(cfg)).msg(bodyMsg("m1", bob, bobItem, "let me own it", propBob)).
			accept(me, note, propBob.ID), false, nil},
		{"only my own messages", clashView(bodyChange(cfg), bodyChange(cfg)).msg(bodyMsg("m1", me, myItem, "hi", nil)), false, nil},
		{"a thread I am not in", clashView(bodyChange(cfg), bodyChange(cfg)).msg(model.Message{ID: "m1", Thread: "t-000000000099",
			Kind: model.ThreadNote, Items: []model.ItemID{bobItem, carolItem}, From: bob, Item: bobItem, Text: "x", At: t0}), false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := eval(t, tt.b.v)
			if !tt.want {
				none(t, ns, model.NoteMessage)
				return
			}
			n := one(t, ns, model.NoteMessage)
			if n.Severity != model.SeverityAct || n.Thread != note || n.Item != myItem || n.Subject != note || !slices.Equal(tools(n), tt.tools) {
				t.Fatalf("note = %+v, tools %v", n, tools(n))
			}
			wantArgs(t, optionFor(t, n, "discuss"), map[string]any{"about": note})
			if n.Quotes[0].From != bob || n.Quotes[0].Text == "" {
				t.Fatalf("quotes = %+v", n.Quotes)
			}
			if slices.Contains(tt.tools, "accept") {
				wantArgs(t, optionFor(t, n, "accept"), map[string]any{"proposal": propBob.ID})
				if !strings.Contains(n.Quotes[1].Text, "take options") {
					t.Fatalf("proposal quote = %+v", n.Quotes[1])
				}
			}
		})
	}
	// A second message re-sends the note.
	first := one(t, eval(t, clashView(bodyChange(cfg), bodyChange(cfg)).msg(bodyMsg("m1", bob, bobItem, "hi", nil)).v), model.NoteMessage)
	second := one(t, eval(t, clashView(bodyChange(cfg), bodyChange(cfg)).msg(bodyMsg("m1", bob, bobItem, "hi", nil)).
		msg(bodyMsg("m2", bob, bobItem, "hello?", nil, "m1")).v), model.NoteMessage)
	if first.ID != second.ID || first.Version == second.Version {
		t.Fatalf("ids %s/%s versions %s/%s", first.ID, second.ID, first.Version, second.Version)
	}
}

func TestSignatureThreadMessage(t *testing.T) {
	b := clash().msg(say(bob, bobItem, "m1", 0, "what do you need?", nil))
	ns := eval(t, b.v)
	n := one(t, ns, model.NoteMessage)
	if n.Thread != sigID || !strings.Contains(strings.Join(n.Evidence, "\n"), "settles after 3 messages each") {
		t.Fatalf("message note = %+v", n)
	}
	one(t, ns, model.NoteSignatureClash)
}

func TestAgreementNotes(t *testing.T) {
	tests := []struct {
		name  string
		b     *vb
		title string
		tools []string
	}{
		{"I own by agreement", clash().msg(say(bob, bobItem, "m1", 0, "you own it; I need Options", propMine)).accept(me, sigID, propMine.ID),
			"You own src/config.ts:parseConfig: publish its signature", []string{"publish"}},
		{"they own by agreement", clash().msg(say(me, myItem, "m1", 0, "you own it", propBob)).accept(bob, sigID, propBob.ID),
			"codex-ci-bbbbbb owns src/config.ts:parseConfig; wait for its ready note", []string{"need"}},
		{"they own, my need recorded", clash().claim(cfg.Key, bob, bobItem, 0).need(me, myNeed(t0)).
			with(func(v *View) { v.ClashSeen[sigID] = t0.Add(-time.Hour) }),
			"", nil},
		{"race", clash().with(func(v *View) { v.ClashSeen[sigID] = t0.Add(-time.Hour) }),
			"Nobody holds src/config.ts:parseConfig: gitlet is trying to claim it for you", []string{"claim"}},
		{"I hold at the bound", clash().claim(cfg.Key, me, myItem, 0).with(func(v *View) { v.ClashSeen[sigID] = t0.Add(-time.Hour) }),
			"You own src/config.ts:parseConfig: publish its signature", []string{"publish"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := eval(t, tt.b.v)
			if tt.title == "" {
				none(t, ns, model.NoteAgreement)
				return
			}
			n := one(t, ns, model.NoteAgreement)
			none(t, ns, model.NoteSignatureClash)
			if n.Title != tt.title || !slices.Equal(tools(n), tt.tools) || n.Thread != sigID || n.Severity != model.SeverityAct {
				t.Fatalf("agreement = %q %v thread %s", n.Title, tools(n), n.Thread)
			}
			for _, o := range n.Options {
				wantArgs(t, o, map[string]any{"symbol": "src/config.ts:parseConfig", "note": n.ID})
			}
		})
	}
	n := one(t, eval(t, clash().msg(say(bob, bobItem, "m1", 0, "you own it; I need Options", propMine)).accept(me, sigID, propMine.ID).v), model.NoteAgreement)
	if !slices.Contains(n.Quotes, model.Quote{From: bob, Text: "you own it; I need Options"}) {
		t.Fatalf("owner's note lacks the others' requirements: %+v", n.Quotes)
	}
}

func TestDuplicateThreadAgreement(t *testing.T) {
	subject := pairSubject(myItem, bobItem)
	thread := model.NoteThreadID(model.NoteDuplicate, subject, myItem, bobItem)
	b := view().mine(item(myItem, myIntent)).peer(bob, item(bobItem, bobIntent)).
		msg(model.Message{ID: "m1", Thread: thread, Kind: model.ThreadNote, Subject: subject, NoteKind: model.NoteDuplicate,
			Items: []model.ItemID{myItem, bobItem}, From: bob, Item: bobItem, Text: "I keep it",
			Proposal: &model.Proposal{ID: "p-0000000000dd", Owner: "me", Summary: "bob keeps the task"}, At: t0}).
		accept(me, thread, "p-0000000000dd")
	ths := Threads(b.v)
	if len(ths) != 1 || ths[0].Kind != model.ThreadNote || !ths[0].Settled || ths[0].Owner != bob || ths[0].NoteKind != model.NoteDuplicate {
		t.Fatalf("threads = %+v", ths)
	}
	n := one(t, eval(t, b.v), model.NoteAgreement)
	if !slices.Equal(tools(n), []string{"abandon", "start"}) || n.Title != "Agreed: codex-ci-bbbbbb keeps the task" {
		t.Fatalf("agreement = %q %v", n.Title, tools(n))
	}
	if got := Decisions(b.v); len(got) != 0 {
		t.Fatalf("note threads have no decisions: %+v", got)
	}
	o := one(t, Evaluate(b.as(bob)), model.NoteAgreement)
	if o.Title != "Agreed: you keep the task" {
		t.Fatalf("owner's agreement = %q", o.Title)
	}
}

func TestUntitled(t *testing.T) {
	runSev(t, model.NoteUntitled, []sevCase{
		{"provisional", view().mine(item(myItem, "fix-thing", provisional())).v, model.SeverityAct},
		{"no intent", view().mine(item(myItem, "")).v, model.SeverityAct},
		{"stated", view().mine(item(myItem, myIntent)).v, ""},
		{"a peer's provisional item", view().peer(bob, item(bobItem, "x", provisional())).v, ""},
	})
	n := one(t, eval(t, view().mine(item(myItem, "fix-thing", provisional())).v), model.NoteUntitled)
	if !slices.Equal(tools(n), []string{"start"}) || n.Options[0].Args != nil {
		t.Fatalf("options = %+v", n.Options)
	}
}

func TestSameWorktree(t *testing.T) {
	none(t, eval(t, view().v), model.NoteSameWorktree)
	n := one(t, eval(t, view().with(func(v *View) {
		v.SameWorktree, v.WorktreeCommand = true, "git worktree add ../gitlet-2 -b work-2"
	}).v), model.NoteSameWorktree)
	if n.Severity != model.SeverityAct || n.Options[0].Command != "git worktree add ../gitlet-2 -b work-2" || n.Options[0].Tool != "" || n.Item != "" {
		t.Fatalf("note = %+v", n)
	}
}
