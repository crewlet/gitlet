package hook

import "testing"

func TestAddTrailers(t *testing.T) {
	const (
		item    = "8c1f0e2a9b3d"
		agent   = "claude-code-laptop-3fa9c1"
		both    = "Gitlet-Item: 8c1f0e2a9b3d\nGitlet-Agent: claude-code-laptop-3fa9c1\n"
		editor  = "# Please enter the commit message for your changes. Lines starting\n# with '#' will be ignored.\n#\n# On branch main\n"
		cut     = "# ------------------------ >8 ------------------------\n"
		diff    = "# Do not modify or remove the line above.\n# Everything below it will be ignored.\ndiff --git a/b b/b\n+Gitlet-Item: in the diff\n"
		comment = "#"
	)
	tests := []struct {
		name    string
		msg     string
		comment string
		want    string // "" means unchanged
	}{
		{
			name: "subject only",
			msg:  "Add strict mode\n",
			want: "Add strict mode\n\n" + both,
		},
		{
			name: "subject without final newline",
			msg:  "Add strict mode",
			want: "Add strict mode\n\n" + both,
		},
		{
			name: "subject and body",
			msg:  "Add strict mode\n\nParse unknown keys as errors.\n",
			want: "Add strict mode\n\nParse unknown keys as errors.\n\n" + both,
		},
		{
			name: "joins an existing trailer block",
			msg:  "Add strict mode\n\nBody.\n\nSigned-off-by: A U Thor <a@example.com>\n",
			want: "Add strict mode\n\nBody.\n\nSigned-off-by: A U Thor <a@example.com>\n" + both,
		},
		{
			name: "trailer block with continuation line",
			msg:  "Add strict mode\n\nCo-authored-by: A U Thor\n  <a@example.com>\n",
			want: "Add strict mode\n\nCo-authored-by: A U Thor\n  <a@example.com>\n" + both,
		},
		{
			name: "subject that looks like a trailer",
			msg:  "fix: handle nil config\n",
			want: "fix: handle nil config\n\n" + both,
		},
		{
			name: "body paragraph with prose is not a trailer block",
			msg:  "Add strict mode\n\nSee: the issue for details\nand more prose here.\n",
			want: "Add strict mode\n\nSee: the issue for details\nand more prose here.\n\n" + both,
		},
		{
			name: "inserted before trailing comments",
			msg:  "Add strict mode\n\n" + editor,
			want: "Add strict mode\n\n" + both + "\n" + editor,
		},
		{
			name: "comments without a blank line",
			msg:  "Add strict mode\n" + editor,
			want: "Add strict mode\n\n" + both + editor,
		},
		{
			name: "commit -v scissors",
			msg:  "Add strict mode\n\n" + editor + "#\n" + cut + diff,
			want: "Add strict mode\n\n" + both + "\n" + editor + "#\n" + cut + diff,
		},
		{
			name: "scissors without comments",
			msg:  "Add strict mode\n" + cut + diff,
			want: "Add strict mode\n\n" + both + cut + diff,
		},
		{
			name:    "custom comment character",
			msg:     "Add strict mode\n\n; Please enter the commit message\n; ------------------------ >8 ------------------------\ndiff\n",
			comment: ";",
			want:    "Add strict mode\n\n" + both + "\n; Please enter the commit message\n; ------------------------ >8 ------------------------\ndiff\n",
		},
		{
			name: "CRLF line endings",
			msg:  "Add strict mode\r\n\r\nBody.\r\n",
			want: "Add strict mode\r\n\r\nBody.\r\n\r\nGitlet-Item: 8c1f0e2a9b3d\r\nGitlet-Agent: claude-code-laptop-3fa9c1\r\n",
		},
		{
			name: "already present",
			msg:  "Add strict mode\n\n" + both,
		},
		{
			name: "already present in another case, before comments",
			msg:  "Add strict mode\n\ngitlet-item: 8c1f0e2a9b3d\n\n" + editor,
		},
		{
			name: "names another item",
			msg:  "Add strict mode\n\nGitlet-Item: 000000000000\nGitlet-Agent: codex-ci-1a2b3c\n",
		},
		{
			name: "agent present, item missing",
			msg:  "Add strict mode\n\nGitlet-Agent: claude-code-laptop-3fa9c1\n",
			want: "Add strict mode\n\nGitlet-Agent: claude-code-laptop-3fa9c1\nGitlet-Item: 8c1f0e2a9b3d\n",
		},
		{
			name: "trailer only in a comment does not count",
			msg:  "Add strict mode\n\n# Gitlet-Item: 000000000000\n",
			want: "Add strict mode\n\n" + both + "\n# Gitlet-Item: 000000000000\n",
		},
		{
			name: "trailer below the scissors does not count",
			msg:  "Add strict mode\n" + cut + "Gitlet-Item: 000000000000\n",
			want: "Add strict mode\n\n" + both + cut + "Gitlet-Item: 000000000000\n",
		},
		{name: "empty message", msg: ""},
		{name: "blank lines only", msg: "\n\n"},
		{name: "editor with no text yet", msg: "\n" + editor},
		{name: "commit -v -s with no text yet", msg: "\n\nSigned-off-by: A U Thor <a@example.com>\n\n" + editor + cut + diff},
		{name: "comment-like subject is left alone", msg: "#123 fix the parser\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.comment
			if c == "" {
				c = comment
			}
			got, changed := addTrailers(tt.msg, c, item, agent)
			want := tt.want
			if want == "" {
				want = tt.msg
			}
			if got != want || changed != (tt.want != "") {
				t.Fatalf("addTrailers() = %q, %v\nwant %q, %v", got, changed, want, tt.want != "")
			}
			if changed {
				// Running the hook twice changes nothing more.
				if again, ch := addTrailers(got, c, item, agent); ch || again != got {
					t.Fatalf("second run changed the message: %q", again)
				}
			}
		})
	}
}
