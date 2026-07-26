package d1

import "testing"

func TestClassifySQLConservatively(t *testing.T) {
	t.Parallel()

	cases := []struct {
		sql  string
		want SQLClass
	}{
		{"SELECT * FROM users", SQLRead},
		{" -- comment\nEXPLAIN QUERY PLAN SELECT 1", SQLRead},
		{"PRAGMA table_info(users)", SQLRead},
		{"WITH x AS (SELECT 1) SELECT * FROM x", SQLUnknown},
		{"UPDATE users SET active = 1", SQLWrite},
		{"DROP TABLE users", SQLWrite},
		{"SELECT 1; DELETE FROM users", SQLWrite},
		{"SELECT '-- not a comment'; DELETE FROM users", SQLWrite},
		{"SELECT '/* not a comment */'; UPDATE users SET active = 0", SQLWrite},
		{"PRAGMA table_info(users)", SQLRead},
		{"PRAGMA journal_mode = WAL", SQLWrite},
		{"PRAGMA writable_schema=ON", SQLWrite},
	}
	for _, tc := range cases {
		if got := ClassifySQL(tc.sql); got != tc.want {
			t.Errorf("ClassifySQL(%q) = %v, want %v", tc.sql, got, tc.want)
		}
	}
}
