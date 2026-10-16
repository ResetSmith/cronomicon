package auth

import "slices"

// ScopeReadable and ScopeWritable as the model had them until I2 (a scope by
// its name alone, against the union of every grant's scopes, AllowedScopes),
// for the tests of that model. Production asks ScopeReadableIn and
// ScopeWritableIn, of a scope in a named agency.
func ScopeReadable(id Identity, scope string) bool {
	return id.Unrestricted() || scope == "" || slices.Contains(id.AllowedScopes, scope)
}

func ScopeWritable(id Identity, targetScope string) bool {
	return id.Unrestricted() || (targetScope != "" && slices.Contains(id.AllowedScopes, targetScope))
}
