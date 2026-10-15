package auth

import "time"

// SetBeforeHash lets tests observe the moment recovery codes start hashing.
func SetBeforeHash(s *Service, f func()) { s.beforeHash = f }

// SetClock replaces the clock the sign-in limits read.
func SetClock(s *Service, now func() time.Time) { s.now = now }
