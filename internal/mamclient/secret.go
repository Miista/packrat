package mamclient

// Secret wraps a sensitive string (the MAM session cookie value) so that it
// cannot be accidentally logged or JSON-marshaled in full. Both String() and
// MarshalJSON() always return a redacted placeholder; callers that need the
// real value must use Reveal explicitly.
type Secret string

// Reveal returns the underlying secret value.
func (s Secret) Reveal() string {
	return string(s)
}

// String implements fmt.Stringer, always redacted.
func (s Secret) String() string {
	return "[redacted]"
}

// MarshalJSON always redacts, so a Secret accidentally included in a struct
// that gets json.Marshal'd never leaks the real value.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"[redacted]"`), nil
}
