package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const directTestSHA = "0123456789abcdef0123456789abcdef01234567"

func writeDirectModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func requireGoToolchain(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not installed")
	}
}

func TestDirectRunner_Passing(t *testing.T) {
	requireGoToolchain(t)
	dir := writeDirectModule(t, map[string]string{
		"go.mod":      "module example.com/directpass\n\ngo 1.24\n",
		"main.go":     "package main\n\nfunc Add(a, b int) int { return a + b }\n\nfunc main() {}\n",
		"main_test.go": "package main\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"bad add\")\n\t}\n}\n",
	})

	d := NewDirectRunner(2 * time.Minute)
	report, err := d.VerifyDir(context.Background(), dir, directTestSHA)
	if err != nil {
		t.Fatalf("VerifyDir error: %v", err)
	}
	if report.Status != StatusPassed {
		t.Fatalf("expected StatusPassed, got %q (%s)\nstdout: %s\nstderr: %s",
			report.Status, report.Reason, report.Results[0].Stdout, lastResultStderr(report))
	}
	if len(report.Results) != 2 {
		t.Fatalf("expected 2 stage results, got %d", len(report.Results))
	}
	for _, res := range report.Results {
		if !res.Passed {
			t.Errorf("expected stage %q passed", res.Command)
		}
	}
}

func TestDirectRunner_BuildFailure(t *testing.T) {
	requireGoToolchain(t)
	dir := writeDirectModule(t, map[string]string{
		"go.mod":  "module example.com/directbuildfail\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc broken( {\n",
	})

	d := NewDirectRunner(2 * time.Minute)
	report, err := d.VerifyDir(context.Background(), dir, directTestSHA)
	if err != nil {
		t.Fatalf("VerifyDir error: %v", err)
	}
	if report.Status != StatusBuildFailed {
		t.Fatalf("expected StatusBuildFailed, got %q (%s)", report.Status, report.Reason)
	}
	if report.FailedStage != "build" {
		t.Errorf("expected FailedStage build, got %q", report.FailedStage)
	}
	if len(report.Results) != 1 {
		t.Errorf("expected 1 stage result (build only), got %d", len(report.Results))
	}
}

func TestDirectRunner_TestFailure(t *testing.T) {
	requireGoToolchain(t)
	dir := writeDirectModule(t, map[string]string{
		"go.mod":      "module example.com/directtestfail\n\ngo 1.24\n",
		"main.go":     "package main\n\nfunc Add(a, b int) int { return a + b }\n\nfunc main() {}\n",
		"main_test.go": "package main\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 99 {\n\t\tt.Fatal(\"intentional failure\")\n\t}\n}\n",
	})

	d := NewDirectRunner(2 * time.Minute)
	report, err := d.VerifyDir(context.Background(), dir, directTestSHA)
	if err != nil {
		t.Fatalf("VerifyDir error: %v", err)
	}
	if report.Status != StatusTestFailed {
		t.Fatalf("expected StatusTestFailed, got %q (%s)", report.Status, report.Reason)
	}
	if report.FailedStage != "test" {
		t.Errorf("expected FailedStage test, got %q", report.FailedStage)
	}
	if len(report.Results) != 2 {
		t.Errorf("expected 2 stage results, got %d", len(report.Results))
	}
}

func TestDirectRunner_NodePass(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not installed")
	}
	dir := writeDirectModule(t, map[string]string{
		"package.json": `{"name":"directnodepass","version":"1.0.0","scripts":{"test":"node -e \"process.exit(0)\""}}`,
	})

	d := NewDirectRunner(2 * time.Minute)
	report, err := d.VerifyDir(context.Background(), dir, directTestSHA)
	if err != nil {
		t.Fatalf("VerifyDir error: %v", err)
	}
	if report.Status != StatusPassed {
		t.Fatalf("expected StatusPassed, got %q (%s)", report.Status, report.Reason)
	}
	if report.DetectedType != "node" {
		t.Errorf("expected DetectedType node, got %q", report.DetectedType)
	}
}

func TestDirectRunner_UnknownDirPassesWithNotice(t *testing.T) {
	d := NewDirectRunner(time.Minute)
	report, err := d.VerifyDir(context.Background(), t.TempDir(), directTestSHA)
	if err != nil {
		t.Fatalf("VerifyDir error: %v", err)
	}
	if report.Status != StatusPassed {
		t.Fatalf("expected StatusPassed for unknown project, got %q", report.Status)
	}
	if len(report.Results) != 0 {
		t.Errorf("expected 0 stage results for unknown project, got %d", len(report.Results))
	}
}

func TestDetectDirectProject(t *testing.T) {
	cases := []struct {
		name     string
		marker   string
		lang     string
		wantTest string
		hasBuild bool
	}{
		{"go", "go.mod", "go", "go", true},
		{"node", "package.json", "node", "npm", false},
		{"rust", "Cargo.toml", "rust", "cargo", false},
		{"python-pyproject", "pyproject.toml", "python", "pytest", false},
		{"python-requirements", "requirements.txt", "python", "pytest", false},
		{"python-poetry", "poetry.lock", "python", "poetry", false},
		{"java-maven", "pom.xml", "java", "mvn", false},
		{"java-gradle", "build.gradle", "java", "gradle", false},
		{"make", "Makefile", "make", "make", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeDirectModule(t, map[string]string{tc.marker: ""})
			proj := detectDirectProject(dir)
			if proj == nil {
				t.Fatalf("expected project detected for %s", tc.marker)
			}
			if proj.lang != tc.lang {
				t.Errorf("got lang=%q, want %q", proj.lang, tc.lang)
			}
			if len(proj.testOptions) == 0 || proj.testOptions[0][0] != tc.wantTest {
				t.Errorf("got first test tool=%v, want %q", proj.testOptions, tc.wantTest)
			}
			if (len(proj.buildArgs) > 0) != tc.hasBuild {
				t.Errorf("build step presence mismatch for %s", tc.lang)
			}
		})
	}
	t.Run("unknown", func(t *testing.T) {
		if proj := detectDirectProject(t.TempDir()); proj != nil {
			t.Errorf("expected nil for empty dir, got %+v", proj)
		}
	})
}

func lastResultStderr(report *VerificationReport) string {
	if len(report.Results) == 0 {
		return ""
	}
	return report.Results[len(report.Results)-1].Stderr
}

// TestDirectGoOfflineIncludesGoEnvOff pins the GOENV=off port: without it a
// go.env file setting GOPROXY=https would re-enable fetch where env alone is
// the boundary.
func TestDirectGoOfflineIncludesGoEnvOff(t *testing.T) {
	dir := writeDirectModule(t, map[string]string{"go.mod": "module example.com/goenvpin\n\ngo 1.24\n"})
	proj := detectDirectProject(dir)
	if proj == nil || proj.lang != "go" {
		t.Fatalf("expected go project, got %+v", proj)
	}
	for _, want := range []string{"GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOVCS=off", "GOENV=off"} {
		found := false
		for _, have := range proj.offline {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("go offline list must contain %q, got %q", want, proj.offline)
		}
	}
}

// TestDirectStageEnvScrubbed proves the keyless build: secret-bearing keys and
// Go override keys are dropped from inheritance while the pinned offline list
// and innocent vars survive.
func TestDirectStageEnvScrubbed(t *testing.T) {
	secrets := map[string]string{
		"GH_TOKEN": "gh-test", "GITHUB_TOKEN": "ghs-test", "GITHUB_APP_KEY": "k",
		"LLM_API_KEY": "llm", "LLM_BASE_URL": "u", "APP_BOT_TOKEN": "b",
		"OPENAI_API_KEY": "o", "ANTHROPIC_API_KEY": "a",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "https://proxy.golang.org")
	t.Setenv("GONOSUMDB", "*")
	t.Setenv("GONOSUMCHECK", "1")
	t.Setenv("GOPRIVATE", "*")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("MYAPP_FOO", "innocent")

	env := buildDirectStageEnv([]string{"GOPROXY=off", "GOENV=off"})
	has := func(key string) (string, bool) {
		for _, kv := range env {
			if kv == key || strings.HasPrefix(kv, key+"=") {
				return kv, true
			}
		}
		return "", false
	}
	for k := range secrets {
		if kv, found := has(k); found {
			t.Errorf("stage env must not contain secret key %q (got %q)", k, kv)
		}
	}
	for _, k := range []string{"GOFLAGS", "GONOSUMDB", "GONOSUMCHECK", "GOPRIVATE", "GOSUMDB"} {
		if kv, found := has(k); found {
			t.Errorf("stage env must not inherit override %q (got %q)", k, kv)
		}
	}
	if kv, found := has("GOPROXY"); !found || kv != "GOPROXY=off" {
		t.Errorf("stage env must pin GOPROXY=off exactly, got %q found=%v", kv, found)
	}
	if kv, found := has("GOENV"); !found || kv != "GOENV=off" {
		t.Errorf("stage env must pin GOENV=off exactly, got %q found=%v", kv, found)
	}
	// Exactly one GOPROXY entry: the inherited override was dropped, not doubled.
	count := 0
	for _, kv := range env {
		if kv == "GOPROXY=off" || strings.HasPrefix(kv, "GOPROXY=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one GOPROXY entry, got %d in %q", count, env)
	}
	if _, found := has("MYAPP_FOO"); !found {
		t.Error("innocent non-matching vars must survive the scrub")
	}
	if _, found := has("PATH"); !found {
		t.Error("PATH must survive the scrub (toolchain must resolve)")
	}
}

// TestDirectBwrapWrapPrefix proves the Linux wrap shape: kernel network
// removal flags first, original argv as tail, and no socket bind anywhere.
func TestDirectBwrapWrapPrefix(t *testing.T) {
	argv := []string{"go", "build", "./..."}
	wrapped := bwrapNetIsolateArgv(argv)
	expectedPrefix := []string{
		"bwrap",
		"--unshare-user",
		"--unshare-net",
		"--die-with-parent",
		"--new-session",
		"--dev-bind", "/", "/",
	}
	if len(wrapped) != len(argv)+len(expectedPrefix) {
		t.Fatalf("wrapped argv length = %d, want %d", len(wrapped), len(argv)+len(expectedPrefix))
	}
	for i, want := range expectedPrefix {
		if wrapped[i] != want {
			t.Fatalf("wrapped argv[%d] = %q, want %q (full %q)", i, wrapped[i], want, wrapped)
		}
	}
	for i, want := range argv {
		if wrapped[i+len(expectedPrefix)] != want {
			t.Fatalf("wrapped tail[%d] = %q, want %q", i, wrapped[i+len(expectedPrefix)], want)
		}
	}
	joined := strings.Join(wrapped, " ")
	for _, banned := range []string{"gateway.sock", "depgw-", "--socket"} {
		if strings.Contains(joined, banned) {
			t.Errorf("wrapped stage-2 argv must contain no %q (got %q)", banned, joined)
		}
	}
}

// TestDirectBwrapUnavailableIsUnavailable proves D-11 honest degradation: on
// Linux without bwrap the wrap decision fails with the typed sentinel (the
// caller maps it to StatusUnavailable + REDUCED-GUARANTEE, never a pass),
// while off-Linux argv passes through for env-only execution.
func TestDirectBwrapUnavailableIsUnavailable(t *testing.T) {
	failAttest := func(context.Context) error {
		return errors.Join(ErrBwrapUnavailable, errors.New("userns probe failed in test"))
	}
	argv := []string{"go", "test", "-race", "./..."}
	if _, err := wrapDirectStage2ArgvForOS("linux", failAttest, context.Background(), argv); !errors.Is(err, ErrBwrapUnavailable) {
		t.Fatalf("linux without bwrap must yield ErrBwrapUnavailable, got: %v", err)
	}
	report := BwrapUnavailableReport("userns probe failed in test")
	if report.Status != StatusUnavailable {
		t.Fatalf("degraded report must carry StatusUnavailable, got %q", report.Status)
	}
	if !strings.Contains(report.Reason, ReducedGuaranteeLabel) || !strings.Contains(report.Summary, ReducedGuaranteeLabel) {
		t.Fatalf("degraded report must carry the REDUCED-GUARANTEE label, got reason=%q summary=%q", report.Reason, report.Summary)
	}
	if report.Status == StatusPassed {
		t.Fatal("degraded report must never carry StatusPassed")
	}
	got, err := wrapDirectStage2ArgvForOS("windows", failAttest, context.Background(), argv)
	if err != nil {
		t.Fatalf("non-Linux must not attest, got error: %v", err)
	}
	if strings.Join(got, " ") != strings.Join(argv, " ") {
		t.Fatalf("non-Linux argv must pass through unchanged, got %q", got)
	}
	okAttest := func(context.Context) error { return nil }
	got, err = wrapDirectStage2ArgvForOS("linux", okAttest, context.Background(), argv)
	if err != nil {
		t.Fatalf("linux with bwrap must wrap, got error: %v", err)
	}
	if len(got) == 0 || got[0] != "bwrap" || !strings.Contains(strings.Join(got, " "), "--unshare-net") {
		t.Fatalf("linux with bwrap must return the unshare-net wrap, got %q", got)
	}
}

// TestInLoopNoGenerateRow pins the plan invariant: go generate is never an
// allowed in-loop row (it executes arbitrary directives).
func TestInLoopNoGenerateRow(t *testing.T) {
	dir := writeDirectModule(t, map[string]string{"go.mod": "module example.com/nogen\n\ngo 1.24\n"})
	proj := detectDirectProject(dir)
	if proj == nil {
		t.Fatal("expected go project")
	}
	for _, row := range inLoopAllowedCommands(proj) {
		for _, el := range row {
			if el == "generate" {
				t.Fatalf("in-loop matrix must never admit go generate (row %q)", row)
			}
		}
	}
}

// TestKeylessStage2 asserts the built podman and direct stage envs contain no
// secret substrings, even with secrets present in the server process env.
// Manual-only companion proofs (Podman --network=none dial-out on a Linux
// host, CT userns probe) are operator steps per 07-VALIDATION.md
// "Manual-Only Verifications" — this test pins the env half automatically.
func TestKeylessStage2(t *testing.T) {
	t.Setenv("GH_TOKEN", "secret-gh")
	t.Setenv("GITHUB_TOKEN", "secret-ghs")
	t.Setenv("GITHUB_APP_ID", "secret-app")
	t.Setenv("LLM_API_KEY", "secret-llm")
	t.Setenv("APP_BOT_TOKEN", "secret-bot")
	t.Setenv("OPENAI_API_KEY", "secret-openai")
	t.Setenv("ANTHROPIC_API_KEY", "secret-anthropic")

	checkClean := func(name, joined string) {
		t.Helper()
		for _, banned := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_", "LLM_", "APP_", "OPENAI_", "ANTHROPIC_"} {
			if strings.Contains(joined, banned) {
				t.Errorf("%s stage env must not contain %q (got %q)", name, banned, joined)
			}
		}
		for _, secret := range []string{"secret-gh", "secret-ghs", "secret-app", "secret-llm", "secret-bot", "secret-openai", "secret-anthropic"} {
			if strings.Contains(joined, secret) {
				t.Errorf("%s stage env leaked a secret value (got %q)", name, joined)
			}
		}
	}

	checkClean("podman", strings.Join(podmanStageEnv(), " "))
	checkClean("direct", strings.Join(buildDirectStageEnv([]string{"GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOVCS=off", "GOENV=off"}), "\n"))

	// The in-loop exec path adds no env of its own: execArgs carry no -e at
	// all, so the container keeps its --network=none keyless env.
	r := &Runner{}
	_, _, _, err := r.InLoopExecCommand(context.Background(), "", []string{"go", "build", "./..."}, time.Minute, 6000)
	if err == nil || !strings.Contains(err.Error(), "container ID") {
		t.Fatalf("expected container-ID gate error, got: %v", err)
	}
}

// TestTokenContainmentGrep extends the T-05-02 structural grep pattern (server
// jobs.go) to the sandbox stage-env builders: no -e/--setenv line in
// podman.go, direct.go, or bwrap.go may name a secret. The intentional
// blocklist literals in direct.go live on lines without -e/--setenv, so they
// do not trip this pin.
func TestTokenContainmentGrep(t *testing.T) {
	files := []string{"podman.go", "direct.go", "bwrap.go"}
	secrets := []string{"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_", "LLM_", "APP_", "OPENAI_", "ANTHROPIC_"}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("cannot read %s: %v", file, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, `"-e"`) && !strings.Contains(line, "--setenv") {
				continue
			}
			for _, secret := range secrets {
				if strings.Contains(line, secret) {
					t.Errorf("%s:%d: secret literal %q in stage-env flag line: %q", file, i+1, secret, strings.TrimSpace(line))
				}
			}
		}
	}
}
