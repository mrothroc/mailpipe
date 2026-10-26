package store

import "context"

// Exec runs raw SQL; tests only, to set up states the API cannot produce.
func (s *Store) Exec(ctx context.Context, query string, args ...any) error {
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}
