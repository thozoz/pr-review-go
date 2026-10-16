package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"
)

// DenialVerdict is the three-valued result of classifying a command result for
// confinement denial. It is deliberately not a boolean: an ambiguous
// "permission denied" must never be promoted to a security denial, and an
// ordinary in-workspace EACCES must stay an ordinary failure (CONFINE-04).
type DenialVerdict string

const (
	// DenialProvenDenied — a denial is proven and attributable to the rank.
	DenialProvenDenied DenialVerdict = "proven_denied"
	// DenialProvenNotDenied — the denial (if any) is the project's own, or
	// there is no denial at all. The ordinary build/test verdict stands.
	DenialProvenNotDenied DenialVerdict = "proven_not_denied"
	// DenialIndeterminate — the evidence is ambiguous; the caller must keep
	// its generic verdict and carry an advisory.
	DenialIndeterminate DenialVerdict = "indeterminate"
)

// DenialKind names the mechanism behind a classification.
type DenialKind string

const (
	// DenialKindNone — no confinement denial keyword matched.
	DenialKindNone DenialKind = ""
	// DenialKindHelperNegativeControl — the helper's exit-74 negative control
	// proved the read-only root was writable, so the domain was not enforcing.
	DenialKindHelperNegativeControl DenialKind = "helper_negative_control"
	// DenialKindReadOnlyFilesystem — EROFS, a mount refusal.
	DenialKindReadOnlyFilesystem DenialKind = "read_only_filesystem"
	// DenialKindPermissionDenied — EACCES, the Landlock LSM/DAC signature.
	DenialKindPermissionDenied DenialKind = "permission_denied"
	// DenialKindCrossDeviceLink — EXDEV, a missing or denied REFER.
	DenialKindCrossDeviceLink DenialKind = "cross_device_link"
	// DenialKindOperationNotPermitted — EPERM, seccomp/capability.
	DenialKindOperationNotPermitted DenialKind = "operation_not_permitted"
)

// ConfinementDeniedLabel is the exact operator-facing label for a proven
// denial. It states the sandbox as the owner so a denial is never read as a
// project defect.
const ConfinementDeniedLabel = "CONFINEMENT-DENIED: the confinement sandbox denied a filesystem operation; this is a sandbox finding, not a build or test failure"

// DenialClassification is the pure result of ClassifyConfinementDenial. Every
// Reason/Advisory string is server-authored: raw stdout/stderr text never
// enters a reason, only the extracted path and a keyword-family name.
type DenialClassification struct {
	Verdict  DenialVerdict
	Kind     DenialKind
	Path     string
	Reason   string
	Advisory string
}

// Found reports whether the classification proves a confinement denial.
func (c DenialClassification) Found() bool { return c.Verdict == DenialProvenDenied }

// DenialInput is the pure input to the classifier: the governing rank, the
// observed exit code, both output streams, and the exact writable roots the
// rank granted. WriteRoots must be the same set the argv builder granted, or
// the verdict describes a different sandbox than the one that ran.
type DenialInput struct {
	Rank       ConfinementRank
	ExitCode   int
	Stdout     string
	Stderr     string
	WriteRoots []string
}

// denialKeyword is one fixed keyword family in match order.
type denialKeyword struct {
	marker string
	kind   DenialKind
}

// denialKeywords is the fixed, documented keyword order. EROFS is checked
// before EACCES, and the cross-device wording before generic permission text.
var denialKeywords = []denialKeyword{
	{"read-only file system", DenialKindReadOnlyFilesystem},
	{"invalid cross-device link", DenialKindCrossDeviceLink},
	{"cross-device link", DenialKindCrossDeviceLink},
	{"permission denied", DenialKindPermissionDenied},
	{"operation not permitted", DenialKindOperationNotPermitted},
}

// ClassifyConfinementDenial is the pure, rank-aware, three-valued denial
// classifier. It reads the exit code, BOTH output streams, and the
// attributable offending path against the sandbox's writable roots.
//
// Order: a first ProvenDenied match wins in stream order stdout-then-stderr,
// then line order; otherwise the first Indeterminate; otherwise
// ProvenNotDenied.
func ClassifyConfinementDenial(in DenialInput) DenialClassification {
	// 1. The helper's pre-exec negative control is a proof, not a log scan.
	if in.Rank == RankLandlock && in.ExitCode == HelperExitNegativeControl {
		reason, _ := HelperExitReason(HelperExitNegativeControl)
		return DenialClassification{
			Verdict: DenialProvenDenied,
			Kind:    DenialKindHelperNegativeControl,
			Reason:  fmt.Sprintf("%s; the domain was not enforcing", reason),
		}
	}

	// 2. Any other helper pre-exec code is an environment fact, never a
	// denial. The caller routes these first; the classifier still refuses to
	// claim a denial for them.
	if _, ok := helperExitUnavailable(in.ExitCode); ok {
		return DenialClassification{
			Verdict:  DenialIndeterminate,
			Kind:     DenialKindNone,
			Advisory: fmt.Sprintf("helper exit %d failed before the target ran; map it to StatusUnavailable, never to a denial", in.ExitCode),
		}
	}

	// 3. No rank governed the run, so no denial can be attributed.
	if in.Rank != RankBwrapRO && in.Rank != RankLandlock {
		return DenialClassification{
			Verdict:  DenialIndeterminate,
			Kind:     DenialKindNone,
			Advisory: "no confinement rank governed this run; denial attribution is impossible",
		}
	}

	firstIndeterminate := DenialClassification{}
	haveIndeterminate := false
	record := func(c DenialClassification) {
		if !haveIndeterminate {
			firstIndeterminate = c
			haveIndeterminate = true
		}
	}

	for _, stream := range []string{in.Stdout, in.Stderr} {
		if stream == "" {
			continue
		}
		for _, line := range strings.Split(stream, "\n") {
			for _, kw := range denialKeywords {
				if !strings.Contains(strings.ToLower(line), kw.marker) {
					continue
				}
				path := ""
				if p, ok := firstAbsolutePathToken(line); ok {
					path = p
				}
				verdict := classifyDenialMatch(in.Rank, kw, path, in.WriteRoots)
				if verdict.Verdict == DenialProvenDenied {
					return verdict
				}
				if verdict.Verdict == DenialIndeterminate {
					record(verdict)
				}
				// A matching keyword decides the line; do not also test the
				// less specific families against it.
				break
			}
		}
	}

	if haveIndeterminate {
		return firstIndeterminate
	}
	return DenialClassification{Verdict: DenialProvenNotDenied, Kind: DenialKindNone}
}

// classifyDenialMatch applies the exact rank-aware discrimination table.
func classifyDenialMatch(rank ConfinementRank, kw denialKeyword, path string, roots []string) DenialClassification {
	attributed := path != ""
	inside := attributed && pathWithinAnyRoot(path, roots)

	switch kw.kind {
	case DenialKindReadOnlyFilesystem:
		// EROFS is a mount refusal that any read-only filesystem produces.
		// Only bwrap-ro's private read-only root makes it that rank's
		// signature; under landlock it stays indeterminate.
		if !attributed {
			return ambiguous(kw, path)
		}
		if inside {
			return DenialClassification{
				Verdict:  DenialIndeterminate,
				Kind:     DenialKindReadOnlyFilesystem,
				Path:     path,
				Advisory: "read-only file system inside a declared writable root; the bind/rule was not in effect, so neither PR code nor a policy denial can be asserted",
			}
		}
		if rank == RankBwrapRO {
			return DenialClassification{
				Verdict: DenialProvenDenied,
				Kind:    DenialKindReadOnlyFilesystem,
				Path:    path,
				Reason:  "confinement denied write to the read-only root (read-only file system)",
			}
		}
		return DenialClassification{
			Verdict:  DenialIndeterminate,
			Kind:     DenialKindReadOnlyFilesystem,
			Path:     path,
			Advisory: "read-only file system is a mount refusal, not this rank's signature; any read-only filesystem produces it",
		}

	case DenialKindPermissionDenied:
		if !attributed {
			return ambiguous(kw, path)
		}
		if inside {
			// The sandbox granted this root, so the denial is the project's
			// own DAC or assertion: an ordinary test/build failure.
			return DenialClassification{Verdict: DenialProvenNotDenied, Kind: DenialKindNone}
		}
		if rank == RankLandlock {
			// The ro root grants reads/exec, so a denied write-class
			// operation outside the rw roots is the LSM hook's signature.
			return DenialClassification{
				Verdict: DenialProvenDenied,
				Kind:    DenialKindPermissionDenied,
				Path:    path,
				Reason:  "confinement denied access outside the writable roots (permission denied)",
			}
		}
		return DenialClassification{
			Verdict:  DenialIndeterminate,
			Kind:     DenialKindPermissionDenied,
			Path:     path,
			Advisory: "permission denied outside the writable roots under bwrap-ro is more likely ordinary DAC/user-namespace ownership than a policy denial; bwrap refuses writes with EROFS",
		}

	case DenialKindCrossDeviceLink:
		if rank == RankLandlock {
			// REFER is missing or denied between the two granted subtrees.
			return DenialClassification{
				Verdict: DenialProvenDenied,
				Kind:    DenialKindCrossDeviceLink,
				Path:    path,
				Reason:  "confinement denied a cross-subtree rename (invalid cross-device link); REFER was missing or denied",
			}
		}
		return DenialClassification{
			Verdict:  DenialIndeterminate,
			Kind:     DenialKindCrossDeviceLink,
			Path:     path,
			Advisory: "cross-device link under bwrap-ro is a cross-mount rename, not a REFER statement",
		}

	case DenialKindOperationNotPermitted:
		// seccomp/capability/no_new_privs misconfiguration: a distinct typed
		// failure, never a project denial.
		return DenialClassification{
			Verdict:  DenialIndeterminate,
			Kind:     DenialKindOperationNotPermitted,
			Path:     path,
			Advisory: "operation not permitted points at seccomp or a capability/no_new_privs misconfiguration, not at project code",
		}
	}
	return DenialClassification{Verdict: DenialProvenNotDenied, Kind: DenialKindNone}
}

// ambiguous is the load-bearing "never a false sandbox denial" rule: a marker
// keyword with no attributable path stays indeterminate.
func ambiguous(kw denialKeyword, path string) DenialClassification {
	return DenialClassification{
		Verdict:  DenialIndeterminate,
		Kind:     kw.kind,
		Path:     path,
		Advisory: fmt.Sprintf("ambiguous %q message with no attributable path; denial attribution is impossible", kw.marker),
	}
}

// firstAbsolutePathToken extracts the first token that begins with "/" at a
// token boundary, trims trailing punctuation, rejects non-absolute results and
// any "../" escape, and returns the cleaned path. A path introduced by a quote
// is read up to its matching closing quote, so a quoted path containing spaces
// ("/usr/x y") survives whole.
func firstAbsolutePathToken(line string) (string, bool) {
	for i := 0; i < len(line); i++ {
		if line[i] != '/' {
			continue
		}
		quoted := byte(0)
		if i > 0 {
			prev := line[i-1]
			switch prev {
			case '"', '\'':
				quoted = prev
			case ' ', '\t', '(', '=', ':':
				// ordinary token boundary
			default:
				continue
			}
		}

		var token string
		if quoted != 0 {
			closeAt := -1
			for j := i; j < len(line); j++ {
				if line[j] == quoted {
					closeAt = j
					break
				}
			}
			if closeAt < 0 {
				continue
			}
			token = line[i:closeAt]
		} else {
			end := i
			for end < len(line) {
				c := line[end]
				if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
					break
				}
				end++
			}
			token = line[i:end]
		}

		token = strings.TrimRight(token, ":\",')")
		if token == "" || !strings.HasPrefix(token, "/") {
			continue
		}
		cleaned := filepath.Clean(token)
		if !filepath.IsAbs(cleaned) || hasDotDotSegment(cleaned) {
			continue
		}
		return cleaned, true
	}
	return "", false
}

// hasDotDotSegment reports whether a cleaned absolute path still contains a
// ".." segment (which filepath.Clean would only leave at the front of a
// relative path, but check defensively).
func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(p, string(filepath.Separator)) {
		if seg == ".." {
			return true
		}
	}
	return false
}

// pathWithinAnyRoot reports whether path is contained by one of the cleaned
// roots. A root contains itself; a sibling sharing a name prefix does not.
func pathWithinAnyRoot(path string, roots []string) bool {
	if path == "" {
		return false
	}
	cleaned := filepath.Clean(path)
	for _, root := range roots {
		if root == "" {
			continue
		}
		cleanRoot := filepath.Clean(root)
		if !filepath.IsAbs(cleanRoot) {
			continue
		}
		rel, err := filepath.Rel(cleanRoot, cleaned)
		if err != nil {
			continue
		}
		if rel == "." {
			return true
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return true
	}
	return false
}

// directWriteRoots returns the exact writable root set the landlock rank
// grants: workspace, cache, tmp and home, cleaned, non-empty and deduped — the
// same set landlockConfinementSpec.WrapArgv already passes to the helper.
func directWriteRoots(paths DirectJobPaths) []string {
	candidates := []string{paths.WorkspaceDir, paths.CacheDir, paths.TmpDir, paths.HomeDir}
	roots := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, root := range candidates {
		cleaned := filepath.Clean(root)
		if root == "" || cleaned == "." || cleaned == "/" || !filepath.IsAbs(cleaned) {
			continue
		}
		if seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		roots = append(roots, cleaned)
	}
	return roots
}
