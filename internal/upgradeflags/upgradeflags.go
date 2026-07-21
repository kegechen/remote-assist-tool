// Package upgradeflags is the single source of truth for the flag policy the
// make-before-break upgrade applies to an old share's command line: rejecting
// standalone/no-auth shares, locating --root (with remote-OS path semantics),
// and reading flag values. Both the help/GUI orchestrator (internal/gui) and
// the remote stage helper (cmd/remote) import it so the policy can't drift
// between them. It has no dependencies on either package (leaf package).
package upgradeflags

import (
	"fmt"
	"path"
	"strconv"
	"strings"
)

// ExplicitBoolFlag reports whether arg is the "name=value" form of a boolean
// flag and, if so, its parsed value. matched=false means arg is not "name=...".
func ExplicitBoolFlag(arg, name string) (matched, enabled bool, err error) {
	prefix := name + "="
	if !strings.HasPrefix(arg, prefix) {
		return false, false, nil
	}
	value := strings.TrimPrefix(arg, prefix)
	enabled, err = strconv.ParseBool(value)
	if err != nil {
		return true, false, fmt.Errorf("invalid value for %s: %q", name, value)
	}
	return true, enabled, nil
}

// HasValuePrefix reports whether arg is the "name=..." form of a flag.
func HasValuePrefix(arg, name string) bool {
	return len(arg) > len(name) && arg[:len(name)+1] == name+"="
}

// RejectStandaloneNoAuth returns an error if argv enables --standalone or
// --no-auth (either bare or =true). A standalone/no-auth share embeds its relay
// / uses a fixed code, so a freshly staged copy can't reuse the old channel.
func RejectStandaloneNoAuth(argv []string) error {
	for _, a := range argv {
		for _, name := range []string{"--standalone", "--no-auth"} {
			if a == name {
				return fmt.Errorf("standalone/no-auth share cannot use make-before-break upgrade")
			}
			matched, enabled, err := ExplicitBoolFlag(a, name)
			if err != nil {
				return err
			}
			if matched && enabled {
				return fmt.Errorf("standalone/no-auth share cannot use make-before-break upgrade")
			}
		}
	}
	return nil
}

// FlagValue returns the value of --name in argv, accepting both "--name value"
// and "--name=value". It returns "" when the flag is absent or has an empty
// value. Later occurrences win. The "=" guard means FlagValue(_, "--code-file")
// does NOT match "--code-file-mirror".
func FlagValue(argv []string, name string) string {
	value := ""
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == name {
			if i+1 < len(argv) {
				value = argv[i+1]
				i++
			}
			continue
		}
		if HasValuePrefix(a, name) {
			value = strings.TrimPrefix(a, name+"=")
		}
	}
	return value
}

// HostCodeFile returns the code-file path a host manager reads for this share.
// A share that was itself produced by a prior in-channel upgrade carries the
// real host path in --code-file-mirror (its --code-file points at that upgrade's
// transient staging dir), so the mirror wins when present. Empty if the old
// share had no code-file at all.
func HostCodeFile(argv []string) string {
	if mirror := FlagValue(argv, "--code-file-mirror"); mirror != "" {
		return mirror
	}
	return FlagValue(argv, "--code-file")
}

// StageRoot computes the directory under which the upgrade may stage files,
// from the old share's argv and working directory, interpreted with remoteOS
// path semantics (the remote may run a different OS than the help side). It
// rejects standalone/no-auth shares. explicit reports whether the old share set
// a non-empty --root (vs. defaulting to cwd, i.e. unrestricted file access).
func StageRoot(remoteOS string, argv []string, cwd string) (root string, explicit bool, err error) {
	if err := RejectStandaloneNoAuth(argv); err != nil {
		return "", false, err
	}
	// argv[0] is the executable; flags start at index 1.
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "--root" {
			if i+1 >= len(argv) {
				return "", false, fmt.Errorf("old share --root is missing its value")
			}
			root = argv[i+1]
			i++
		} else if HasValuePrefix(a, "--root") {
			root = strings.TrimPrefix(a, "--root=")
		}
	}
	// Empty --root value == unrestricted (share treats "" as no limit), so fall
	// back to cwd and report not-explicit.
	if root == "" {
		return cleanPath(remoteOS, cwd), false, nil
	}
	if !isAbsPath(remoteOS, root) {
		root = joinPath(remoteOS, cwd, root)
	}
	return cleanPath(remoteOS, root), true, nil
}

func isWindows(remoteOS string) bool { return strings.EqualFold(remoteOS, "windows") }

func isAbsPath(remoteOS, p string) bool {
	if isWindows(remoteOS) {
		return isWindowsAbs(p)
	}
	return path.IsAbs(p)
}

func isDriveLetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// isWindowsAbs reports whether p is a drive-rooted (C:\ or C:/) or UNC
// (\\server\share) absolute path. A bare "C:foo" (drive-relative) is not
// treated as absolute, matching Go's filepath.IsAbs.
func isWindowsAbs(p string) bool {
	if len(p) >= 2 && (p[0] == '\\' || p[0] == '/') && (p[1] == '\\' || p[1] == '/') {
		return true
	}
	if len(p) >= 3 && isDriveLetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	return false
}

func joinPath(remoteOS, base, name string) string {
	if isWindows(remoteOS) {
		return strings.TrimRight(base, `\/`) + `\` + strings.TrimLeft(name, `\/`)
	}
	return path.Join(base, name)
}

func cleanPath(remoteOS, p string) string {
	if isWindows(remoteOS) {
		return cleanWindows(p)
	}
	return path.Clean(p)
}

// cleanWindows normalizes separators to backslash and resolves "." / ".."
// segments without touching the filesystem, keeping any drive or UNC prefix
// intact (".." can't climb above it). It mirrors what the old PowerShell probe
// obtained via [IO.Path]::GetFullPath, but runs on the help side regardless of
// the help machine's own OS.
func cleanWindows(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	vol := windowsVolume(p)
	rest := p[len(vol):]
	rooted := strings.HasPrefix(rest, `\`)
	out := make([]string, 0, 8)
	for _, seg := range strings.Split(rest, `\`) {
		switch seg {
		case "", ".":
			// drop empty and current-dir segments
		case "..":
			if len(out) > 0 && out[len(out)-1] != ".." {
				out = out[:len(out)-1]
			} else if !rooted {
				out = append(out, "..")
			}
		default:
			out = append(out, seg)
		}
	}
	prefix := vol
	if rooted {
		prefix += `\`
	}
	result := prefix + strings.Join(out, `\`)
	if result == "" {
		return "."
	}
	return result
}

// windowsVolume returns the leading volume prefix of p: a drive ("C:") or a UNC
// share root ("\\server\share"), or "" if neither. p must already use
// backslash separators.
func windowsVolume(p string) string {
	if len(p) >= 2 && isDriveLetter(p[0]) && p[1] == ':' {
		return p[:2]
	}
	if len(p) >= 2 && p[0] == '\\' && p[1] == '\\' {
		rest := p[2:]
		i := strings.IndexByte(rest, '\\')
		if i < 0 {
			return p // \\server (no share component)
		}
		j := strings.IndexByte(rest[i+1:], '\\')
		if j < 0 {
			return p // \\server\share (no trailing path)
		}
		return p[:2+i+1+j]
	}
	return ""
}
