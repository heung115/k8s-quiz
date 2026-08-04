package problem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/models"
)

type mutableImageResolver struct {
	mu   sync.Mutex
	id   string
	err  error
	refs []string
}

func (r *mutableImageResolver) ResolveImage(_ context.Context, ref string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
	return r.id, r.err
}

func imageID(digit byte) string {
	return "sha256:" + strings.Repeat(string(digit), 64)
}

func setupTestProblems(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "images.lock"), []byte("test=test/image:v1@"+imageID('f')+"\n"), 0644)

	problemDir := filepath.Join(dir, "test-problem")
	os.MkdirAll(problemDir, 0755)

	yamlContent := `id: test-problem
title: "Test Problem"
description: "A test problem"
category: pod
difficulty: easy
type: fix
timeout_minutes: 15
verify_type: script
base_image: k3s-base:latest
`
	os.WriteFile(filepath.Join(problemDir, "problem.yaml"), []byte(yamlContent), 0644)
	os.WriteFile(filepath.Join(problemDir, "setup.sh"), []byte("#!/bin/sh\necho setup"), 0755)
	os.WriteFile(filepath.Join(problemDir, "verify.sh"), []byte("#!/bin/sh\nexit 0"), 0755)
	os.WriteFile(filepath.Join(problemDir, "hint.md"), []byte("# Hint\nTry kubectl describe"), 0644)

	choiceDir := filepath.Join(dir, "choice-problem")
	os.MkdirAll(choiceDir, 0755)

	choiceYaml := `id: choice-problem
title: "Choice Problem"
description: "A choice problem"
category: config
difficulty: easy
type: find
timeout_minutes: 10
verify_type: choice
base_image: k3s-base:latest
choices:
  - id: a
    text: "Option A"
  - id: b
    text: "Option B"
correct_choice: b
`
	os.WriteFile(filepath.Join(choiceDir, "problem.yaml"), []byte(choiceYaml), 0644)

	return dir
}

func TestLoadAll(t *testing.T) {
	dir := setupTestProblems(t)
	loader := NewGitLoader(dir)

	problems, err := loader.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}

	if len(problems) != 2 {
		t.Fatalf("expected 2 problems, got %d", len(problems))
	}

	found := make(map[string]bool)
	for _, p := range problems {
		found[p.ID] = true
		if p.ID == "test-problem" {
			if p.Title != "Test Problem" {
				t.Errorf("expected title 'Test Problem', got '%s'", p.Title)
			}
			if p.Category != "pod" {
				t.Errorf("expected category 'pod', got '%s'", p.Category)
			}
			if p.TimeoutMinutes != 15 {
				t.Errorf("expected timeout 15, got %d", p.TimeoutMinutes)
			}
			if p.Hint != "# Hint\nTry kubectl describe" {
				t.Errorf("expected hint content, got '%s'", p.Hint)
			}
		}
		if p.ID == "choice-problem" {
			if p.VerifyType != "choice" {
				t.Errorf("expected verify_type 'choice', got '%s'", p.VerifyType)
			}
			if len(p.Choices) != 2 {
				t.Errorf("expected 2 choices, got %d", len(p.Choices))
			}
			if p.CorrectChoice != "b" {
				t.Errorf("expected correct_choice 'b', got '%s'", p.CorrectChoice)
			}
		}
	}

	if !found["test-problem"] || !found["choice-problem"] {
		t.Error("missing expected problems")
	}
}

func TestLoadAllEmptyDir(t *testing.T) {
	dir := t.TempDir()
	loader := NewGitLoader(dir)

	problems, err := loader.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("expected 0 problems, got %d", len(problems))
	}
}

func TestLoadAllInvalidDir(t *testing.T) {
	loader := NewGitLoader("/nonexistent/path")
	_, err := loader.LoadAll(context.Background())
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestHasSetupScript(t *testing.T) {
	dir := setupTestProblems(t)
	loader := NewGitLoader(dir)

	if !loader.HasSetupScript("test-problem") {
		t.Error("expected setup script to exist")
	}
	if loader.HasSetupScript("choice-problem") {
		t.Error("expected no setup script for choice-problem")
	}
}

func TestHasVerifyScript(t *testing.T) {
	dir := setupTestProblems(t)
	loader := NewGitLoader(dir)

	if !loader.HasVerifyScript("test-problem") {
		t.Error("expected verify script to exist")
	}
}

func TestGetSetupScript(t *testing.T) {
	dir := setupTestProblems(t)
	loader := NewGitLoader(dir)

	script, err := loader.GetSetupScript("test-problem")
	if err != nil {
		t.Fatalf("GetSetupScript failed: %v", err)
	}
	if script != "#!/bin/sh\necho setup" {
		t.Errorf("unexpected script content: %s", script)
	}
}

func TestGetVerifyScript(t *testing.T) {
	dir := setupTestProblems(t)
	loader := NewGitLoader(dir)

	script, err := loader.GetVerifyScript("test-problem")
	if err != nil {
		t.Fatalf("GetVerifyScript failed: %v", err)
	}
	if script != "#!/bin/sh\nexit 0" {
		t.Errorf("unexpected script content: %s", script)
	}
}

func TestRuntimeBundleRevisionIsDeterministicAndTracksRuntimeFiles(t *testing.T) {
	dir := setupTestProblems(t)
	loader := NewGitLoader(dir)
	first, err := loader.GetRuntimeBundle("test-problem")
	if err != nil {
		t.Fatal(err)
	}
	second, err := loader.GetRuntimeBundle("test-problem")
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision == "" || first.Revision != second.Revision || first.Problem.Revision != first.Revision {
		t.Fatalf("revision is not stable/bound to problem: first=%q second=%q problem=%q", first.Revision, second.Revision, first.Problem.Revision)
	}

	verifyPath := filepath.Join(dir, "test-problem", "verify.sh")
	if err := os.WriteFile(verifyPath, []byte("#!/bin/sh\nexit 1"), 0755); err != nil {
		t.Fatal(err)
	}
	changed, err := loader.GetRuntimeBundle("test-problem")
	if err != nil {
		t.Fatal(err)
	}
	if changed.Revision == first.Revision {
		t.Fatal("verify.sh change did not change runtime revision")
	}

	if err := os.Remove(verifyPath); err != nil {
		t.Fatal(err)
	}
	missing, err := loader.GetRuntimeBundle("test-problem")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(verifyPath, nil, 0755); err != nil {
		t.Fatal(err)
	}
	empty, err := loader.GetRuntimeBundle("test-problem")
	if err != nil {
		t.Fatal(err)
	}
	if missing.Revision == empty.Revision {
		t.Fatal("missing and present-empty verify.sh must have different revisions")
	}

	hintPath := filepath.Join(dir, "test-problem", "hint.md")
	if err := os.WriteFile(hintPath, []byte("changed approved hint"), 0644); err != nil {
		t.Fatal(err)
	}
	hintChanged, err := loader.GetRuntimeBundle("test-problem")
	if err != nil {
		t.Fatal(err)
	}
	if hintChanged.Revision == empty.Revision {
		t.Fatal("hint.md change did not change approved problem revision")
	}
}

func TestResolveRuntimeRejectsRevisionMismatch(t *testing.T) {
	dir := setupTestProblems(t)
	loader := NewGitLoader(dir)
	bundle, err := loader.GetRuntimeBundle("test-problem")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := loader.ResolveRuntime(context.Background(), runner.ProblemRef{ID: "test-problem", Revision: bundle.Revision})
	if err != nil {
		t.Fatalf("ResolveRuntime approved revision: %v", err)
	}
	if runtime.Revision != bundle.Revision || runtime.VerifyScript != bundle.VerifyScript {
		t.Fatalf("runtime is not the approved bundle snapshot: %+v", runtime)
	}
	if _, err := loader.ResolveRuntime(context.Background(), runner.ProblemRef{ID: "test-problem", Revision: "stale"}); !errors.Is(err, runner.ErrInvalidRevision) {
		t.Fatalf("stale revision error = %v, want ErrInvalidRevision", err)
	}
}

func TestRuntimeLoaderBindsImmutableImageAcrossRestart(t *testing.T) {
	dir := setupTestProblems(t)
	firstResolver := &mutableImageResolver{id: imageID('1')}
	firstLoader, err := NewRuntimeGitLoader(dir, firstResolver)
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstLoader.GetRuntimeBundle("test-problem")
	if err != nil {
		t.Fatal(err)
	}
	again, err := firstLoader.GetRuntimeBundle("test-problem")
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != again.Revision || first.RuntimeImage != imageID('1') {
		t.Fatalf("same files and image ID were not stable: first=%+v again=%+v", first, again)
	}
	activateSnapshotProblem(t, firstLoader, "test-problem")
	runtime, err := firstLoader.ResolveRuntime(context.Background(), runner.ProblemRef{ID: "test-problem", Revision: first.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Image != imageID('1') {
		t.Fatalf("runtime image = %q, want immutable content ID", runtime.Image)
	}

	// A new loader models a server restart after the authored tag moved.
	secondResolver := &mutableImageResolver{id: imageID('2')}
	secondLoader, err := NewRuntimeGitLoader(dir, secondResolver)
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondLoader.GetRuntimeBundle("test-problem")
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision == first.Revision {
		t.Fatal("moved image tag did not change the approved problem revision")
	}
	if _, err := secondLoader.ResolveRuntime(context.Background(), runner.ProblemRef{ID: "test-problem", Revision: first.Revision}); !errors.Is(err, runner.ErrInvalidRevision) {
		t.Fatalf("old revision after image move error = %v, want ErrInvalidRevision", err)
	}
}

func TestRuntimeLoaderFailsClosedOnImageResolution(t *testing.T) {
	dir := setupTestProblems(t)
	if _, err := NewRuntimeGitLoader(dir, nil); err == nil {
		t.Fatal("nil image resolver was accepted")
	}

	resolver := &mutableImageResolver{err: errors.New("image missing")}
	loader, err := NewRuntimeGitLoader(dir, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.BuildCandidate(context.Background()); err == nil || !strings.Contains(err.Error(), "image missing") {
		t.Fatalf("strict candidate error = %v, want image resolution failure", err)
	}

	resolver.err = nil
	resolver.id = "sha256:not-a-digest"
	if _, err := loader.GetRuntimeBundle("test-problem"); err == nil || !strings.Contains(err.Error(), "non-content-addressed") {
		t.Fatalf("non-content-addressed image error = %v", err)
	}
}

func TestRuntimeLoaderSkipsSupportDirectoriesWithoutProblemManifest(t *testing.T) {
	dir := setupTestProblems(t)
	if err := os.MkdirAll(filepath.Join(dir, "hack"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hack", "validate.py"), []byte("print('ok')\n"), 0644); err != nil {
		t.Fatal(err)
	}
	loader, err := NewRuntimeGitLoader(dir, &mutableImageResolver{id: imageID('a')})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := loader.BuildCandidate(context.Background())
	if err != nil {
		t.Fatalf("runtime loader rejected support directory: %v", err)
	}
	problems := candidate.Problems()
	if len(problems) != 2 {
		t.Fatalf("runtime loader returned %d problems, want 2", len(problems))
	}
}

func TestRuntimeLoaderFailsClosedOnManifestIdentityMismatch(t *testing.T) {
	dir := setupTestProblems(t)
	manifest := filepath.Join(dir, "test-problem", "problem.yaml")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "id: test-problem", "id: different-problem", 1))
	if err := os.WriteFile(manifest, data, 0644); err != nil {
		t.Fatal(err)
	}
	loader, err := NewRuntimeGitLoader(dir, &mutableImageResolver{id: imageID('a')})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.BuildCandidate(context.Background()); err == nil || !strings.Contains(err.Error(), "contains id") {
		t.Fatalf("runtime manifest identity mismatch error = %v", err)
	}
}

func TestStrictRuntimeLoadAllRequiresCatalogCoordinator(t *testing.T) {
	loader := newSnapshotRuntimeLoader(t, setupTestProblems(t))
	if _, err := loader.LoadAll(context.Background()); !errors.Is(err, ErrRuntimeCatalogCoordinator) {
		t.Fatalf("strict LoadAll error = %v, want ErrRuntimeCatalogCoordinator", err)
	}
}

func TestRuntimeLoaderRejectsMutableWorkloadImages(t *testing.T) {
	dir := setupTestProblems(t)
	loader, err := NewRuntimeGitLoader(dir, &mutableImageResolver{id: imageID('a')})
	if err != nil {
		t.Fatal(err)
	}
	setupPath := filepath.Join(dir, "test-problem", "setup.sh")
	if err := os.WriteFile(setupPath, []byte("#!/bin/sh\ncat <<'YAML'\n  image: nginx:alpine\nYAML\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := loader.GetRuntimeBundle("test-problem"); err == nil || !strings.Contains(err.Error(), "not an exact images.lock entry") {
		t.Fatalf("mutable workload image error = %v", err)
	}

	pinned := "nginx:alpine@" + imageID('b')
	if err := os.WriteFile(filepath.Join(dir, "images.lock"), []byte("nginx="+pinned+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setupPath, []byte("#!/bin/sh\ncat <<'YAML'\n  image: "+pinned+"\nYAML\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := loader.GetRuntimeBundle("test-problem"); err != nil {
		t.Fatalf("digest-pinned workload image rejected: %v", err)
	}
}

func TestApprovedImagePolicyCoversFlagsAndUnsupportedMutations(t *testing.T) {
	pinned := "nginx:stable@" + imageID('c')
	policy, err := parseApprovedImagePolicy([]byte("nginx=" + pinned + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"setup.sh":     []byte("kubectl create deployment web --image=nginx:latest\n"),
		"verify.sh":    nil,
		"problem.yaml": nil,
		"hint.md":      nil,
	}
	if err := validateApprovedImagePolicy(files, policy); err == nil || !strings.Contains(err.Error(), "nginx:latest") {
		t.Fatalf("mutable --image flag error = %v", err)
	}
	files["setup.sh"] = []byte("kubectl create deployment web --image=" + pinned + "\n")
	if err := validateApprovedImagePolicy(files, policy); err != nil {
		t.Fatalf("pinned --image flag rejected: %v", err)
	}
	for _, script := range []string{
		"kubectl set image deployment/web web=" + pinned,
		"helm upgrade web chart --set image.repository=nginx",
		"kustomize build . | kubectl apply -f -",
		"kubectl apply -f https://example.invalid/workload.yaml",
		"cat <<YAML\nimage: $IMAGE\nYAML",
	} {
		files["setup.sh"] = []byte(script + "\n")
		if err := validateApprovedImagePolicy(files, policy); err == nil {
			t.Fatalf("unsupported image path was accepted: %q", script)
		}
	}
}

func TestRuntimeRevisionTracksImagesLock(t *testing.T) {
	dir := setupTestProblems(t)
	loader, err := NewRuntimeGitLoader(dir, &mutableImageResolver{id: imageID('a')})
	if err != nil {
		t.Fatal(err)
	}
	first, err := loader.GetRuntimeBundle("test-problem")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "images.lock"), []byte("test=test/image:v2@"+imageID('e')+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	second, err := loader.GetRuntimeBundle("test-problem")
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision == second.Revision {
		t.Fatal("images.lock change did not change runtime revision")
	}
}

func TestDefaultValues(t *testing.T) {
	dir := t.TempDir()
	problemDir := filepath.Join(dir, "minimal")
	os.MkdirAll(problemDir, 0755)

	yamlContent := `id: minimal
title: "Minimal"
description: "Minimal problem"
category: pod
difficulty: easy
type: fix
verify_type: script
`
	os.WriteFile(filepath.Join(problemDir, "problem.yaml"), []byte(yamlContent), 0644)

	loader := NewGitLoader(dir)
	problems, err := loader.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	if len(problems) != 1 {
		t.Fatalf("expected 1 problem, got %d", len(problems))
	}
	if problems[0].TimeoutMinutes != 30 {
		t.Errorf("expected default timeout 30, got %d", problems[0].TimeoutMinutes)
	}
	if problems[0].BaseImage != "k3s-base:latest" {
		t.Errorf("expected default base_image, got '%s'", problems[0].BaseImage)
	}
}

// PROB-12: invalid problems are skipped (with a log) and the valid subset
// still loads.
func TestLoadAllSkipsInvalidProblems(t *testing.T) {
	dir := t.TempDir()
	write := func(name, yaml string) {
		d := filepath.Join(dir, name)
		os.MkdirAll(d, 0755)
		os.WriteFile(filepath.Join(d, "problem.yaml"), []byte(yaml), 0644)
	}

	write("good", `id: good
title: Good
category: pod
difficulty: easy
type: fix
verify_type: script
`)
	write("bad-enum", `id: bad-enum
title: Bad Enum
category: not-a-category
difficulty: easy
type: fix
verify_type: script
`)
	write("bad-verify-type", `id: bad-verify-type
title: Bad Verify
category: pod
difficulty: easy
type: fix
verify_type: vibes
`)
	write("bad-id", `id: Bad_ID!
title: Bad ID
category: pod
difficulty: easy
type: fix
verify_type: script
`)
	write("choice-one", `id: choice-one
title: One Choice
category: pod
difficulty: easy
type: find
verify_type: choice
choices:
  - id: a
    text: Only
correct_choice: a
`)
	write("choice-wrong-answer", `id: choice-wrong-answer
title: Wrong Answer
category: pod
difficulty: easy
type: find
verify_type: choice
choices:
  - id: a
    text: A
  - id: b
    text: B
correct_choice: zzz
`)
	write("choice-good", `id: choice-good
title: Good Choice
category: pod
difficulty: easy
type: find
verify_type: choice
choices:
  - id: a
    text: A
  - id: b
    text: B
correct_choice: b
`)

	loader := NewGitLoader(dir)
	problems, err := loader.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}

	got := map[string]bool{}
	for _, p := range problems {
		got[p.ID] = true
	}
	if len(problems) != 2 || !got["good"] || !got["choice-good"] {
		t.Errorf("expected exactly [good choice-good], got %v", got)
	}
}

func TestValidateLoadedProblem(t *testing.T) {
	cases := []struct {
		name    string
		problem models.Problem
		wantOK  bool
	}{
		{"valid script", models.Problem{ID: "ok", Category: "pod", Difficulty: "easy", Type: "fix", VerifyType: "script"}, true},
		{"bad id", models.Problem{ID: "UPPER", Category: "pod", Difficulty: "easy", Type: "fix", VerifyType: "script"}, false},
		{"bad category", models.Problem{ID: "ok", Category: "wat", Difficulty: "easy", Type: "fix", VerifyType: "script"}, false},
		{"bad difficulty", models.Problem{ID: "ok", Category: "pod", Difficulty: "extreme", Type: "fix", VerifyType: "script"}, false},
		{"bad type", models.Problem{ID: "ok", Category: "pod", Difficulty: "easy", Type: "yolo", VerifyType: "script"}, false},
		{"bad verify_type", models.Problem{ID: "ok", Category: "pod", Difficulty: "easy", Type: "fix", VerifyType: "oracle"}, false},
		{"choice too few", models.Problem{ID: "ok", Category: "pod", Difficulty: "easy", Type: "find", VerifyType: "choice", Choices: []models.Choice{{ID: "a", Text: "A"}}, CorrectChoice: "a"}, false},
		{"choice answer missing", models.Problem{ID: "ok", Category: "pod", Difficulty: "easy", Type: "find", VerifyType: "choice", Choices: []models.Choice{{ID: "a", Text: "A"}, {ID: "b", Text: "B"}}, CorrectChoice: "c"}, false},
		{"choice valid", models.Problem{ID: "ok", Category: "pod", Difficulty: "easy", Type: "find", VerifyType: "choice", Choices: []models.Choice{{ID: "a", Text: "A"}, {ID: "b", Text: "B"}}, CorrectChoice: "b"}, true},
	}
	for _, tc := range cases {
		msg := validateLoadedProblem(&tc.problem)
		if tc.wantOK && msg != "" {
			t.Errorf("%s: expected valid, got %q", tc.name, msg)
		}
		if !tc.wantOK && msg == "" {
			t.Errorf("%s: expected invalid", tc.name)
		}
	}
}
