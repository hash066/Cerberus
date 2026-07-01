package main

import "encoding/base64"

// decodeBase64 accepts both standard and URL-safe base64, with or without
// padding, since MCP clients (and humans hand-crafting a tool call) may
// produce either.
func decodeBase64(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}
