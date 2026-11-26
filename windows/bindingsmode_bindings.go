//go:build bindings

package main

// runningBindingsGeneration is true for the throwaway binary `wails build`
// compiles (and runs) to emit the TypeScript bindings. That binary shares this
// main(), so every side effect before wails.Run happens for real -- and the
// single-instance claim in particular makes it exit early, because the running
// app owns the mutex. The build then reports "Generating bindings: Done."
// while the model file is never rewritten. Skipping the setup keeps the live
// installation untouched and lets wails.Run do its one job.
const runningBindingsGeneration = true
