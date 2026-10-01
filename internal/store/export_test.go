package store

// export_test.go exposes raw SQL to this package's external tests (store_test) and to
// nothing else: a _test.go file in package store compiles only into this package's own
// test binary, so these never become API another package could reach.
//
// It exists for the F30 tests, which must write timestamps EXPLICITLY — byte-identical,
// or deliberately inverted — to prove the display timestamps no longer decide anything.
// Relying on the wall clock to produce a same-millisecond collision is exactly the
// one-in-thirty flake those tests replace.

// ExecForTest runs one statement against the store's database.
func (s *Store) ExecForTest(query string, args ...any) error {
	_, err := s.db.Exec(query, args...)

	return err
}

// QueryStringForTest runs a single-row, single-column query and scans it as a string.
func (s *Store) QueryStringForTest(query string, args ...any) (string, error) {
	var out string
	err := s.db.QueryRow(query, args...).Scan(&out)

	return out, err
}
