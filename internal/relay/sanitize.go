package relay

import (
	"strings"
	"unicode"
)

// maxPeerStringLen caps peer-reported display strings. Real values are
// ~40-60 chars ("user@host Windows 11 amd64"); the cap only guards against a
// hostile client bloating logs / downstream consumers.
const maxPeerStringLen = 120

// sanitizePeerString cleans a peer-controlled display string (Host identity,
// Version) at ingestion: trims whitespace, drops control characters
// (log/JSON hygiene) and Unicode format characters (Cf: bidi overrides,
// zero-width chars — identity strings are a trust surface and U+202E-style
// display spoofing survives plain-text rendering), and caps the length at
// maxPeerStringLen runes.
func sanitizePeerString(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
		n++
		if n >= maxPeerStringLen {
			break
		}
	}
	return b.String()
}
