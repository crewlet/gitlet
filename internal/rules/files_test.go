package rules

import "testing"

func TestHotFile(t *testing.T) {
	tests := map[string]string{
		"package-lock.json":     "lockfile",
		"web/pnpm-lock.yaml":    "lockfile",
		"yarn.lock":             "lockfile",
		"go.sum":                "lockfile",
		"crates/x/Cargo.lock":   "lockfile",
		"poetry.lock":           "lockfile",
		"uv.lock":               "lockfile",
		"Gemfile.lock":          "lockfile",
		"composer.lock":         "lockfile",
		"CHANGELOG.md":          "changelog",
		"docs/CHANGES.rst":      "changelog",
		"HISTORY":               "changelog",
		"CHANGELOG":             "changelog",
		"internal/history.go":   "",
		"changelog.go":          "",
		"go.mod":                "",
		"package.json":          "",
		"src/package-lock.json": "lockfile",
	}
	for p, want := range tests {
		if got := HotFile(p); got != want {
			t.Errorf("HotFile(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestMigrationSeq(t *testing.T) {
	tests := []struct {
		path    string
		globs   []string
		dir     string
		seq     string
		ok      bool
		comment string
	}{
		{"app/migrations/0003_add_user.py", nil, "app/migrations", "0003", true, "django"},
		{"db/migrate/20231010120000_create_users.rb", nil, "db/migrate", "20231010120000", true, "rails"},
		{"alembic/versions/0042_add_index.py", nil, "alembic/versions", "0042", true, "alembic"},
		{"prisma/migrations/20231010_init/migration.sql", nil, "prisma/migrations", "20231010", true, "prisma directory"},
		{"db/migrations/V3__add_users.sql", nil, "db/migrations", "3", true, "flyway"},
		{"db/migrations/V3_1__add_users.sql", nil, "db/migrations", "3", true, "flyway with a minor"},
		{"migrations/postgres/0001_init.sql", nil, "migrations/postgres", "0001", true, "nested directory"},
		{"migrate/7_seed.go", nil, "migrate", "7", true, "migrate segment"},
		{"migrations/__init__.py", nil, "", "", false, "no number"},
		{"migrations/README.md", nil, "", "", false, "no number"},
		{"db/migrations/R__views.sql", nil, "", "", false, "flyway repeatable"},
		{"src/migrations.go", nil, "", "", false, "file named migrations"},
		{"src/0001_x.sql", nil, "", "", false, "not a migration directory"},
		{"versions/0001_x.py", nil, "", "", false, "versions alone"},
		{"db/changes/0005_x.sql", []string{"db/changes"}, "db/changes", "0005", true, "extra directory"},
		{"svc/a/schema/12_x.sql", []string{"**/schema"}, "svc/a/schema", "12", true, "extra glob with **"},
		{"sql/v2/3_x.sql", []string{"sql/*"}, "sql/v2", "3", true, "extra glob with *"},
		{"sql/v2/3_x.sql", []string{"other/*"}, "", "", false, "extra glob that does not match"},
		{"./app/migrations/0003_x.py", nil, "app/migrations", "0003", true, "leading ./"},
	}
	for _, tt := range tests {
		dir, seq, ok := MigrationSeq(tt.path, tt.globs)
		if dir != tt.dir || seq != tt.seq || ok != tt.ok {
			t.Errorf("%s: MigrationSeq(%q) = (%q, %q, %t), want (%q, %q, %t)", tt.comment, tt.path, dir, seq, ok, tt.dir, tt.seq, tt.ok)
		}
	}
	if !sameSeq("V3", "V3") || !sameSeq("0003", "3") || sameSeq("3", "30") || !sameSeq("000", "0") {
		t.Error("sameSeq compares numbers wrongly")
	}
}
