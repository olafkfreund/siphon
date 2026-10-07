// Package siphon holds the repository-root files the portal serves.
package siphon

import _ "embed"

// LLMsTxt and LLMsFullTxt are llms.txt and llms-full.txt (llmstxt.org).
var (
	//go:embed llms.txt
	LLMsTxt []byte
	//go:embed llms-full.txt
	LLMsFullTxt []byte
)
