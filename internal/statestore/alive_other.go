//go:build !unix

package statestore

// alive cannot be answered on this platform, so it answers "yes".
//
// The conservative direction: a node is never reported as abandoned on a
// platform where ember cannot check. The alternative would have every record
// read as orphaned the moment this file is compiled in, which is a flood of
// false findings and the fastest way to teach an operator to ignore `ls`.
func alive(int) bool { return true }
