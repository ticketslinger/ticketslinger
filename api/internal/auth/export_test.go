package auth

// SetBeforeHash lets tests observe the moment recovery codes start hashing.
func SetBeforeHash(s *Service, f func()) { s.beforeHash = f }
