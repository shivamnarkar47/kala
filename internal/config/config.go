// Package config holds the wire-protocol constants shared by the Go host,
// mirroring harness/config.py.
package config

const (
	// MaxOutputTokens caps the completion output (deliberately far below the
	// catalog's 384k budget).
	MaxOutputTokens = 32_000

	// ReasoningField is the delta field that carries streamed reasoning.
	ReasoningField = "reasoning_content"

	// RequestTimeout is the gateway connection/header timeout in seconds.
	RequestTimeout = 120
)
