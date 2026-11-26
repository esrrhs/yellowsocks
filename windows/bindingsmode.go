//go:build !bindings

package main

// runningBindingsGeneration is false for the real application build; see
// bindingsmode_bindings.go for the other half.
const runningBindingsGeneration = false
