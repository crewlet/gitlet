package bus

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

func TestDocumentRoundTrip(t *testing.T) {
	st := sampleState(agentA)
	_, data, err := encodeState(agentA, st, t0)
	mustNoErr(t, err)
	got, _, err := decodeState(agentA, data)
	mustNoErr(t, err)
	st.Format = model.Format
	if !reflect.DeepEqual(got, st) {
		t.Fatalf("state round trip:\n got %+v\nwant %+v", got, st)
	}

	ob := sampleOutbox(agentA)
	_, data, err = encodeOutbox(agentA, ob)
	mustNoErr(t, err)
	gotOb, err := decodeOutbox(agentA, data)
	mustNoErr(t, err)
	ob.Format = model.Format
	if !reflect.DeepEqual(gotOb, ob) {
		t.Fatalf("outbox round trip:\n got %+v\nwant %+v", gotOb, ob)
	}

	cl := sampleClaim(keyA, agentA, itemA)
	cl.Contract = &model.Contract{Signature: "function parseConfig(string, boolean?): Config", SigHash: "abc", StubCommit: oid("stub"), Breaking: true, PublishedAt: t0}
	_, data, err = encodeClaim(cl)
	mustNoErr(t, err)
	gotCl, err := decodeClaim(keyA, data)
	mustNoErr(t, err)
	cl.Format = model.Format
	if !reflect.DeepEqual(gotCl, cl) {
		t.Fatalf("claim round trip:\n got %+v\nwant %+v", gotCl, cl)
	}
	if !strings.Contains(string(data), `"format":1`) {
		t.Fatalf("encoded claim lacks format: %s", data)
	}
}

func TestEncodeFillsAgent(t *testing.T) {
	st := sampleState("")
	got, _, err := encodeState(agentA, st, t0)
	mustNoErr(t, err)
	if got.Agent.ID != agentA || got.Format != model.Format {
		t.Fatalf("agent/format = %q/%d", got.Agent.ID, got.Format)
	}
	if _, _, err := encodeState(agentA, sampleState(agentB), t0); err == nil {
		t.Fatal("encoding another agent's state succeeded")
	}
	ob, _, err := encodeOutbox(agentA, model.Outbox{})
	mustNoErr(t, err)
	if ob.Agent != agentA {
		t.Fatalf("outbox agent = %q", ob.Agent)
	}
}

func TestDecodeFormat(t *testing.T) {
	tests := []struct {
		name  string
		data  string
		want  string
		newer bool
	}{
		{"newer format", `{"format":2,"agent":{"id":"x"},"items":"a new shape"}`, "format 2", true},
		{"no format", `{"agent":{"id":"claude-code-laptop-3fa9c1"}}`, "no format", false},
		{"format zero", `{"format":0}`, "invalid format 0", false},
		{"not JSON", `{"format":1,`, "invalid JSON", false},
		{"array", `[1,2]`, "invalid JSON", false},
		{"wrong type", `{"format":1,"items":"x"}`, "invalid JSON", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := decodeState(agentA, []byte(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
			if errors.Is(err, errNewerFormat) != tt.newer {
				t.Fatalf("errors.Is(err, errNewerFormat) = %v", !tt.newer)
			}
		})
	}
}

func TestValidateState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*model.AgentState)
		want   string
	}{
		{"valid", func(*model.AgentState) {}, ""},
		{"agent mismatch", func(s *model.AgentState) { s.Agent.ID = agentB }, "does not match the ref"},
		{"bad item id", func(s *model.AgentState) { s.Items[0].ID = "XYZ" }, "invalid item id"},
		{"duplicate item", func(s *model.AgentState) { s.Items = append(s.Items, s.Items[0]) }, "duplicate item id"},
		{"bad status", func(s *model.AgentState) { s.Items[0].Status = "paused" }, "invalid status"},
		{"branch option", func(s *model.AgentState) { s.Items[0].Branch = "--upload-pack=evil" }, "invalid branch"},
		{"branch dots", func(s *model.AgentState) { s.Items[0].Branch = "a/../b" }, "invalid branch"},
		{"snapshot expression", func(s *model.AgentState) { s.Items[0].Snapshot = "HEAD~1" }, "invalid snapshot"},
		{"path escapes", func(s *model.AgentState) { s.Items[0].Footprint.Files[0].Path = "../etc/passwd" }, "invalid path"},
		{"absolute path", func(s *model.AgentState) { s.Items[0].Footprint.Files[0].Path = "/etc/passwd" }, "invalid path"},
		{"unclean path", func(s *model.AgentState) { s.Items[0].Footprint.Files[0].Path = "src/./a.go" }, "invalid path"},
		{"symbol key", func(s *model.AgentState) { s.Items[0].Footprint.Symbols[0].Symbol.Key = "nothex" }, "invalid symbol key"},
		{"symbol path", func(s *model.AgentState) { s.Items[0].Footprint.Symbols[0].Symbol.Path = "a/../../b" }, "invalid path"},
		{"ref path", func(s *model.AgentState) { s.Items[0].Footprint.Refs[0].Path = "a\nb" }, "invalid path"},
		{"negative lease", func(s *model.AgentState) { s.LeaseSeconds = -1 }, "negative leaseSeconds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := sampleState(agentA)
			tt.mutate(&st)
			err := validateState(agentA, &st)
			if tt.want == "" {
				mustNoErr(t, err)
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateOutbox(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*model.Outbox)
		want   string
	}{
		{"valid", func(*model.Outbox) {}, ""},
		{"agent mismatch", func(o *model.Outbox) { o.Agent = agentB }, "does not match the ref"},
		{"need key", func(o *model.Outbox) { o.Needs[0].Key = "short" }, "invalid symbol key"},
		{"need item", func(o *model.Outbox) { o.Needs[0].Item = "" }, "invalid item id"},
		{"message from other", func(o *model.Outbox) { o.Messages[0].From = agentB }, "is not the outbox's agent"},
		{"thread id", func(o *model.Outbox) { o.Messages[0].Thread = "t-xyz" }, "invalid thread"},
		{"sig subject", func(o *model.Outbox) { o.Messages[0].Subject = "src/config.ts" }, "invalid symbol key"},
		{"note subject is free", func(o *model.Outbox) {
			o.Messages[0].Kind, o.Messages[0].Subject = model.ThreadNote, "src/config.ts"
		}, ""},
		{"proposal owner", func(o *model.Outbox) { o.Messages[0].Proposal.Owner = "Bad Agent" }, "invalid proposal owner"},
		{"seen id", func(o *model.Outbox) { o.Messages[0].Seen = []string{"x"} }, "invalid seen id"},
		{"accept proposal", func(o *model.Outbox) { o.Accepts[0].Proposal = "q-dddddddddddd" }, "invalid proposal id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ob := sampleOutbox(agentA)
			tt.mutate(&ob)
			err := validateOutbox(agentA, &ob)
			if tt.want == "" {
				mustNoErr(t, err)
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateClaim(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*model.Claim)
		want   string
	}{
		{"valid", func(*model.Claim) {}, ""},
		{"key mismatch", func(c *model.Claim) { c.Key = keyB }, "does not match the ref"},
		{"symbol key mismatch", func(c *model.Claim) { c.Symbol.Key = keyB }, "does not match the ref"},
		{"owner", func(c *model.Claim) { c.Owner = "" }, "invalid owner"},
		{"item", func(c *model.Claim) { c.Item = "nope" }, "invalid item id"},
		{"stub", func(c *model.Claim) { c.Contract = &model.Contract{StubCommit: "main"} }, "invalid stubCommit"},
		{"requirement", func(c *model.Claim) { c.Requirements[0].From = "UPPER" }, "invalid agent"},
		{"epoch", func(c *model.Claim) { c.Epoch = -1 }, "negative epoch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := sampleClaim(keyA, agentA, itemA)
			tt.mutate(&cl)
			err := validateClaim(keyA, &cl)
			if tt.want == "" {
				mustNoErr(t, err)
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidPath(t *testing.T) {
	for p, want := range map[string]bool{
		"src/config.ts": true, "a": true, "dir/sub/file.go": true, ".github/workflows/ci.yml": true,
		"": false, ".": false, "..": false, "../a": false, "a/..": false, "a/../b": false, "/abs": false,
		"a//b": false, "a/./b": false, "a/": false, "a\x00b": false, "a\tb": false, "\xff": false,
	} {
		if got := validPath(p); got != want {
			t.Errorf("validPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestValidBranch(t *testing.T) {
	for s, want := range map[string]bool{
		"": true, "main": true, "gitlet/add-strict-mode-8c1f": true, "feature/x.y": true,
		"-x": false, "a..b": false, "a b": false, "a~1": false, "a^": false, "a:b": false, "a/": false,
		"/a": false, "a.lock": false, "a//b": false, "a/.hidden": false, "a@{1}": false, "@": false, "a.": false,
	} {
		if got := validBranch(s); got != want {
			t.Errorf("validBranch(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestEncodeStateDropsSettledItems(t *testing.T) {
	now := t0.Add(3 * time.Hour)
	st := sampleState(agentA)
	old, recent := st.Items[0], st.Items[0]
	old.ID, old.Status = "aaaaaaaaaaaa", model.ItemLanded
	closed := now.Add(-2 * time.Hour)
	old.ClosedAt = &closed
	recent.ID, recent.Status = "bbbbbbbbbbbb", model.ItemAbandoned
	recent.UpdatedAt = now.Add(-10 * time.Minute) // no ClosedAt: UpdatedAt counts
	st.Items = append(st.Items, old, recent)

	got, _, err := encodeState(agentA, st, now)
	mustNoErr(t, err)
	var ids []model.ItemID
	for _, it := range got.Items {
		ids = append(ids, it.ID)
	}
	if want := []model.ItemID{itemA, "bbbbbbbbbbbb"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("items = %v, want %v", ids, want)
	}
	if len(st.Items) != 3 {
		t.Fatal("encodeState modified the caller's items")
	}
}

// stateWithBadPaths is sampleState with a footprint entry of each kind
// whose path git accepts but a document cannot carry, after a valid one.
func stateWithBadPaths() model.AgentState {
	st := sampleState(agentA)
	fp := &st.Items[0].Footprint
	fp.Files = append(fp.Files,
		model.FileChange{Path: "Icon\r", Status: "M"},
		model.FileChange{Path: "b.go", OldPath: "caf\xe9.go", Status: "R"})
	sym := fp.Symbols[0]
	sym.Symbol.Path = strings.Repeat("d/", 2100) + "x.ts"
	fp.Symbols = append(fp.Symbols, sym)
	fp.Refs = append(fp.Refs, model.Ref{Name: "f", Kind: model.RefCall, Path: "tab\there.go", Line: 1})
	return st
}

func TestEncodeStateDropsBadPaths(t *testing.T) {
	st := stateWithBadPaths()
	_, data, err := encodeState(agentA, st, t0)
	mustNoErr(t, err)
	got, dropped, err := decodeState(agentA, data)
	mustNoErr(t, err)
	want := sampleState(agentA)
	want.Format = model.Format
	if !reflect.DeepEqual(got, want) || len(dropped) != 0 {
		t.Fatalf("published state:\n got %+v\nwant %+v (dropped on read %q)", got, want, dropped)
	}
	if !reflect.DeepEqual(st, stateWithBadPaths()) {
		t.Fatal("encodeState modified the caller's footprint")
	}
}

func TestDecodeStateSkipsBadPaths(t *testing.T) {
	st := stateWithBadPaths()
	st.Format = model.Format
	st.Items[0].Footprint.Files[2].OldPath = "../b.go" // invalid UTF-8 would not survive JSON
	data, err := json.Marshal(st)
	mustNoErr(t, err)
	got, dropped, err := decodeState(agentA, data)
	mustNoErr(t, err)
	if fp := got.Items[0].Footprint; len(fp.Files) != 1 || len(fp.Symbols) != 1 || len(fp.Refs) != 1 {
		t.Fatalf("footprint kept %d files, %d symbols, %d refs; want 1 each", len(fp.Files), len(fp.Symbols), len(fp.Refs))
	}
	want := []string{
		`items[0].footprint.files[1]: invalid path "Icon\r"`,
		`items[0].footprint.files[2]: invalid oldPath "../b.go"`,
		`items[0].footprint.symbols[1]: invalid path`,
		`items[0].footprint.refs[1]: invalid path "tab\there.go"`,
	}
	if len(dropped) != len(want) {
		t.Fatalf("dropped = %q", dropped)
	}
	for i, w := range want {
		if !strings.HasPrefix(dropped[i], w) {
			t.Errorf("dropped[%d] = %q, want prefix %q", i, dropped[i], w)
		}
	}
}

func TestEncodeSizeLimits(t *testing.T) {
	// Recently settled items are dropped when the document is too large.
	st := sampleState(agentA)
	big := st.Items[0]
	big.ID, big.Status, big.UpdatedAt = "bbbbbbbbbbbb", model.ItemLanded, t0
	big.Intent = strings.Repeat("x", MaxWriteDoc)
	st.Items = append(st.Items, big)
	got, data, err := encodeState(agentA, st, t0)
	mustNoErr(t, err)
	if len(got.Items) != 1 || len(data) > MaxWriteDoc {
		t.Fatalf("settled item kept: %d items, %d bytes", len(got.Items), len(data))
	}

	// Open items are never dropped: the writer refuses.
	st = sampleState(agentA)
	st.Items[0].Intent = strings.Repeat("x", MaxWriteDoc)
	if _, _, err := encodeState(agentA, st, t0); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	ob := sampleOutbox(agentA)
	ob.Messages[0].Text = strings.Repeat("y", MaxWriteDoc)
	if _, _, err := encodeOutbox(agentA, ob); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("outbox err = %v, want ErrTooLarge", err)
	}
	cl := sampleClaim(keyA, agentA, itemA)
	cl.Requirements[0].Text = strings.Repeat("z", MaxWriteDoc)
	if _, _, err := encodeClaim(cl); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("claim err = %v, want ErrTooLarge", err)
	}
}

func TestTrackingRef(t *testing.T) {
	tests := []struct{ remote, ref, want string }{
		{"origin", StateRef(agentA), "refs/gitlet-remote/origin/agents/" + string(agentA) + "/state"},
		{"origin", ClaimRef(keyA), "refs/gitlet-remote/origin/claims/" + keyA},
		{"upstream", WipRef(agentB, itemB), "refs/gitlet-remote/upstream/wip/" + string(agentB) + "/" + string(itemB)},
		{"origin", AgentsPrefix, "refs/gitlet-remote/origin/agents/"},
		{"", StateRef(agentA), StateRef(agentA)},
		{"origin", "refs/heads/main", "refs/heads/main"},
	}
	for _, tt := range tests {
		if got := TrackingRef(tt.remote, tt.ref); got != tt.want {
			t.Errorf("TrackingRef(%q, %q) = %q, want %q", tt.remote, tt.ref, got, tt.want)
		}
	}
}

func TestMainRefAndRemote(t *testing.T) {
	e := newEnv(t)
	b := e.bus(e.clone(), agentA)
	if b.Remote() != "origin" || b.MainRef() != "refs/gitlet-main/origin" {
		t.Fatalf("remote/main ref = %q/%q", b.Remote(), b.MainRef())
	}
	l := New(e.local(), agentA, Options{})
	if l.Remote() != "" || l.MainRef() != "refs/heads/main" {
		t.Fatalf("local remote/main ref = %q/%q", l.Remote(), l.MainRef())
	}
	l = New(e.local(), agentA, Options{Main: "trunk"})
	if l.MainRef() != "refs/heads/trunk" {
		t.Fatalf("main ref = %q", l.MainRef())
	}
}
