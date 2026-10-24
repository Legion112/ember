package node

import "fmt"

// sprintf exists so secret_test can assert on fmt verbs without importing fmt
// into every table. A stray %s in an error string is one of the three ways key
// material escapes, so it is worth covering explicitly.
func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }
