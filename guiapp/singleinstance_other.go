//go:build !windows

package guiapp

// Single-instance guarding and the "show the existing window" handshake are
// Windows-only (a named mutex and a named event). These stubs keep the shared
// Startup/Shutdown path compiling on macOS and Linux.

func WatchShowRequests(show func()) {}

func ReleaseSingleInstance() {}
