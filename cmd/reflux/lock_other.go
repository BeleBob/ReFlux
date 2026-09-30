//go:build !unix

package main

import "time"

// lockFile is a no-op where reflux does not run (it manages Linux hosts);
// it exists so the module builds everywhere.
func lockFile(string, time.Duration) (func(), error) { return func() {}, nil }
