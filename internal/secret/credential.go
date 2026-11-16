package secret

import "regexp"

// credentialRe matches values that carry a credential even when their key name
// doesn't say so: URLs with user:password@ and token-like query parameters
// (review M7, e.g. proxy URLs and tunnel/headroom URLs).
var credentialRe = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^/\s"'@]+@|[?&](token|access_token|api_key|apikey|key|secret|password|auth|sig|signature)=`)

// LooksLikeCredential reports whether s embeds a credential.
func LooksLikeCredential(s string) bool { return credentialRe.MatchString(s) }
