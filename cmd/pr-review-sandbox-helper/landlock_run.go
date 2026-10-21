package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

// landlockRunUsage is the exact argument contract the server emits and the
// probe reproduces. It is exported to nothing, but both platform files and the
// packaging smoke test depend on the `-- <command>` shape.
const landlockRunUsage = "Usage: pr-review-sandbox-helper landlock-run [--ro <abs-path>]... [--rw <abs-path>]... -- <command> [args...]"

// stringList is a repeatable flag.Value so `--ro /a --ro /b` and
// `--rw /x --rw /y` accumulate instead of overwriting.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// landlockRunOptions is the parsed landlock-run invocation.
type landlockRunOptions struct {
	ro      []string
	rw      []string
	command []string
}

// parseLandlockRunArgs parses the landlock-run argument contract.
//
// The flag set uses ContinueOnError (never ExitOnError: that exits 2, which is
// the helper's `prepare` INCOMPLETE convention, not the usage code 75) and
// discards the stdlib usage output so the helper controls every byte it
// prints.
func parseLandlockRunArgs(args []string) (landlockRunOptions, error) {
	fs := flag.NewFlagSet("landlock-run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var ro, rw stringList
	fs.Var(&ro, "ro", "absolute path to grant read-only access beneath (repeatable)")
	fs.Var(&rw, "rw", "absolute path to grant read-write access beneath (repeatable)")

	// The stdlib accepts both -ro and --ro, and consumes a trailing "--"
	// separator; fs.Args() is therefore exactly the target command.
	if err := fs.Parse(args); err != nil {
		return landlockRunOptions{}, fmt.Errorf("%s: %w", landlockRunUsage, err)
	}
	command := fs.Args()
	if len(command) == 0 {
		return landlockRunOptions{}, fmt.Errorf("%s: a target command after -- is required", landlockRunUsage)
	}
	if len(rw) == 0 {
		return landlockRunOptions{}, fmt.Errorf("%s: at least one --rw root is required", landlockRunUsage)
	}
	for _, roots := range []struct {
		flag string
		list stringList
	}{{"--ro", ro}, {"--rw", rw}} {
		for _, root := range roots.list {
			if strings.TrimSpace(root) == "" {
				return landlockRunOptions{}, fmt.Errorf("%s: %s path must not be empty", landlockRunUsage, roots.flag)
			}
			if !filepath.IsAbs(root) {
				return landlockRunOptions{}, fmt.Errorf("%s: %s path %q must be absolute", landlockRunUsage, roots.flag, root)
			}
		}
	}
	// A root listed as both read-only and read-write is ambiguous, and an
	// accidental overlap between two explicit job roots is a policy bug rather
	// than a policy. The host root "/" is exempt: it is always the read-only
	// root and always an ancestor of every writable job root, and Landlock
	// resolves that pair by granting the most specific rule.
	if err := validateLandlockRoots(ro, rw); err != nil {
		return landlockRunOptions{}, err
	}
	return landlockRunOptions{ro: ro, rw: rw, command: command}, nil
}

// validateLandlockRoots refuses a read-only root that overlaps a read-write
// root. Both sides are symlink-resolved before the comparison because the
// applier resolves every root when it opens it: a lexical-only check would let
// a symlink alias bypass the overlap refusal (10-REVIEW IN-01). A root whose
// symlinks cannot be resolved falls back to its cleaned lexical form.
func validateLandlockRoots(ro, rw []string) error {
	resolve := func(root string) string {
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			return filepath.Clean(resolved)
		}
		return filepath.Clean(root)
	}
	for _, roRoot := range ro {
		resolvedRO := resolve(roRoot)
		if resolvedRO == "/" {
			continue
		}
		for _, rwRoot := range rw {
			if landlockRootsOverlap(resolvedRO, resolve(rwRoot)) {
				return fmt.Errorf(
					"%s: --ro root %q overlaps --rw root %q; a path cannot be both", landlockRunUsage, roRoot, rwRoot)
			}
		}
	}
	return nil
}

// landlockRootsOverlap reports whether a and b are the same path or one
// contains the other (both directions).
func landlockRootsOverlap(a, b string) bool {
	return landlockRootContains(a, b) || landlockRootContains(b, a)
}

// landlockRootContains reports whether path b is a at or beneath path a.
func landlockRootContains(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// landlockUnsupportedExit is the build-tag-free typed unavailable result for a
// platform that has no Landlock. It is shared by the !linux stub so the code
// and the reason are unit-testable everywhere.
func landlockUnsupportedExit(goos string) (int, string) {
	return sandbox.HelperExitUnavailable,
		"landlock-run: Landlock filesystem confinement requires Linux (GOOS=" + goos + "); the landlock rank is unavailable"
}
