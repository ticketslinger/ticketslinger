package auth

// SetBeforeHash lets tests observe the moment recovery codes start hashing.
func SetBeforeHash(s *Service, f func()) { s.beforeHash = f }

// ResetLimits forgets every in-process rate limit, so a test can go past them.
func ResetLimits(s *Service) { s.ipLimit, s.emLimit, s.recLimit = newLimits() }
