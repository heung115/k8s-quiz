package problem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/models"
	"golang.org/x/sys/unix"
)

func TestRuntimeLoaderRejectsSymlinkedCatalogInputs(t *testing.T) {
	t.Run("problem directory", func(t *testing.T) {
		dir := setupTestProblems(t)
		if err := os.Symlink("test-problem", filepath.Join(dir, "linked-problem")); err != nil {
			t.Fatal(err)
		}
		loader := newSnapshotRuntimeLoader(t, dir)

		if _, err := loader.BuildCandidate(context.Background()); err == nil {
			t.Fatal("strict runtime loader accepted a symlinked problem directory")
		}
	})

	for _, name := range []string{"problem.yaml", "setup.sh", "verify.sh", "hint.md"} {
		name := name
		t.Run(name, func(t *testing.T) {
			dir := setupTestProblems(t)
			path := filepath.Join(dir, "test-problem", name)
			target := path + ".target"
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(target), path); err != nil {
				t.Fatal(err)
			}
			loader := newSnapshotRuntimeLoader(t, dir)

			if _, err := loader.BuildCandidate(context.Background()); err == nil {
				t.Fatalf("strict runtime loader accepted symlinked %s", name)
			}
		})
	}

	t.Run("images.lock", func(t *testing.T) {
		dir := setupTestProblems(t)
		path := filepath.Join(dir, "images.lock")
		target := path + ".target"
		if err := os.Rename(path, target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Base(target), path); err != nil {
			t.Fatal(err)
		}
		loader := newSnapshotRuntimeLoader(t, dir)

		if _, err := loader.BuildCandidate(context.Background()); err == nil {
			t.Fatal("strict runtime loader accepted symlinked images.lock")
		}
	})
}

func TestRuntimeLoaderRevisionCannotBeReboundToMutatedCheckout(t *testing.T) {
	dir := setupTestProblems(t)
	loader := newSnapshotRuntimeLoader(t, dir)
	approved := activateSnapshotProblem(t, loader, "test-problem")
	ref := runner.ProblemRef{ID: approved.ID, Revision: approved.Revision}
	wantRuntime := resolveSnapshotRuntime(t, loader, ref)
	wantProblem := resolveSnapshotProblem(t, loader, ref)

	mutateSnapshotProblem(t, dir)

	gotRuntime := resolveSnapshotRuntime(t, loader, ref)
	gotProblem := resolveSnapshotProblem(t, loader, ref)
	assertSameRuntimeSnapshot(t, gotRuntime, wantRuntime)
	assertSameProblemSnapshot(t, gotProblem, wantProblem)
}

func TestRuntimeLoaderSuccessfulReloadRetainsPreviousRevision(t *testing.T) {
	dir := setupTestProblems(t)
	loader := newSnapshotRuntimeLoader(t, dir)
	first := activateSnapshotProblem(t, loader, "test-problem")
	firstRef := runner.ProblemRef{ID: first.ID, Revision: first.Revision}
	wantFirstRuntime := resolveSnapshotRuntime(t, loader, firstRef)
	wantFirstProblem := resolveSnapshotProblem(t, loader, firstRef)

	mutateSnapshotProblem(t, dir)
	second := activateSnapshotProblem(t, loader, "test-problem")
	if second.Revision == first.Revision {
		t.Fatal("successful reload did not publish a new current revision")
	}
	secondRef := runner.ProblemRef{ID: second.ID, Revision: second.Revision}
	secondRuntime := resolveSnapshotRuntime(t, loader, secondRef)
	secondProblem := resolveSnapshotProblem(t, loader, secondRef)
	if secondRuntime.SetupScript != "#!/bin/sh\necho changed setup\n" ||
		secondRuntime.VerifyScript != "#!/bin/sh\necho changed verify\nexit 1\n" {
		t.Fatalf("new revision returned stale runtime bytes: %+v", secondRuntime)
	}
	if secondProblem.Title != "Changed Problem" || secondProblem.Hint != "changed hint\n" {
		t.Fatalf("new revision returned stale problem bytes: %+v", secondProblem)
	}

	gotFirstRuntime := resolveSnapshotRuntime(t, loader, firstRef)
	gotFirstProblem := resolveSnapshotProblem(t, loader, firstRef)
	assertSameRuntimeSnapshot(t, gotFirstRuntime, wantFirstRuntime)
	assertSameProblemSnapshot(t, gotFirstProblem, wantFirstProblem)
}

func TestRuntimeLoaderFailedReloadLeavesPriorCatalogActive(t *testing.T) {
	dir := setupTestProblems(t)
	loader := newSnapshotRuntimeLoader(t, dir)
	approved := activateSnapshotProblem(t, loader, "test-problem")
	approvedRef := runner.ProblemRef{ID: approved.ID, Revision: approved.Revision}
	wantRuntime := resolveSnapshotRuntime(t, loader, approvedRef)
	wantProblem := resolveSnapshotProblem(t, loader, approvedRef)

	mutateSnapshotProblem(t, dir)
	candidate, err := loader.GetRuntimeBundle("test-problem")
	if err != nil {
		t.Fatalf("read candidate bundle: %v", err)
	}
	if candidate.Revision == approved.Revision {
		t.Fatal("mutated candidate unexpectedly retained the approved revision")
	}
	manifestPath := filepath.Join(dir, "choice-problem", "problem.yaml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = []byte(strings.Replace(string(manifest), "id: choice-problem", "id: wrong-problem", 1))
	if err := os.WriteFile(manifestPath, manifest, 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := loader.BuildCandidate(context.Background()); err == nil {
		t.Fatal("reload with an invalid catalog unexpectedly succeeded")
	}

	gotRuntime, err := loader.ResolveRuntime(context.Background(), approvedRef)
	if err != nil {
		t.Errorf("previous runtime revision was lost after failed reload: %v", err)
	} else {
		assertSameRuntimeSnapshot(t, gotRuntime, wantRuntime)
	}
	gotProblem, err := loader.ResolveProblem(context.Background(), approvedRef)
	if err != nil {
		t.Errorf("previous problem revision was lost after failed reload: %v", err)
	} else {
		assertSameProblemSnapshot(t, gotProblem, wantProblem)
	}
	candidateRef := runner.ProblemRef{ID: candidate.Problem.ID, Revision: candidate.Revision}
	if _, err := loader.ResolveRuntime(context.Background(), candidateRef); !errors.Is(err, runner.ErrInvalidRevision) {
		t.Errorf("candidate runtime from failed reload error = %v, want ErrInvalidRevision", err)
	}
	if _, err := loader.ResolveProblem(context.Background(), candidateRef); !errors.Is(err, runner.ErrInvalidRevision) {
		t.Errorf("candidate problem from failed reload error = %v, want ErrInvalidRevision", err)
	}
}

func TestRuntimeLoaderReturnsDeepCopiesOfCatalogProblems(t *testing.T) {
	dir := setupTestProblems(t)
	loader := newSnapshotRuntimeLoader(t, dir)
	choice := activateSnapshotProblem(t, loader, "choice-problem")
	ref := runner.ProblemRef{ID: choice.ID, Revision: choice.Revision}

	first := resolveSnapshotProblem(t, loader, ref)
	if len(first.Choices) != 2 {
		t.Fatalf("choice snapshot has %d choices, want 2", len(first.Choices))
	}
	first.Choices[0].ID = "mutated"
	first.Choices[0].Text = "caller-owned mutation"
	first.Title = "mutated title"

	second := resolveSnapshotProblem(t, loader, ref)
	if second.Title == first.Title || second.Choices[0].ID == "mutated" || second.Choices[0].Text == "caller-owned mutation" {
		t.Fatalf("caller mutation changed immutable catalog: %+v", second)
	}
}

func TestRuntimeLoaderRejectsSpecialHardlinkedAndOversizedInputs(t *testing.T) {
	t.Run("directory in place of script", func(t *testing.T) {
		dir := setupTestProblems(t)
		path := filepath.Join(dir, "test-problem", "setup.sh")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
		loader := newSnapshotRuntimeLoader(t, dir)
		if _, err := loader.BuildCandidate(context.Background()); err == nil {
			t.Fatal("strict runtime loader accepted a directory as setup.sh")
		}
	})

	t.Run("fifo in place of script", func(t *testing.T) {
		dir := setupTestProblems(t)
		path := filepath.Join(dir, "test-problem", "verify.sh")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(path, 0600); err != nil {
			t.Skipf("create FIFO: %v", err)
		}
		loader := newSnapshotRuntimeLoader(t, dir)
		if _, err := loader.BuildCandidate(context.Background()); err == nil {
			t.Fatal("strict runtime loader accepted a FIFO as verify.sh")
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		dir := setupTestProblems(t)
		target := filepath.Join(dir, "outside-script")
		if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "test-problem", "verify.sh")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(target, path); err != nil {
			t.Skipf("create hardlink: %v", err)
		}
		loader := newSnapshotRuntimeLoader(t, dir)
		if _, err := loader.BuildCandidate(context.Background()); err == nil || !strings.Contains(err.Error(), "hard-linked") {
			t.Fatalf("strict runtime loader hardlink error = %v", err)
		}
	})

	t.Run("oversized manifest", func(t *testing.T) {
		dir := setupTestProblems(t)
		path := filepath.Join(dir, "test-problem", "problem.yaml")
		data := make([]byte, maxProblemManifestBytes+1)
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
		loader := newSnapshotRuntimeLoader(t, dir)
		if _, err := loader.BuildCandidate(context.Background()); err == nil || !strings.Contains(err.Error(), "byte limit") {
			t.Fatalf("strict runtime loader oversized manifest error = %v", err)
		}
	})
}

func newSnapshotRuntimeLoader(t *testing.T, dir string) *GitLoader {
	t.Helper()
	canonicalParent, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		t.Fatalf("resolve runtime catalog parent path: %v", err)
	}
	storePath := filepath.Join(canonicalParent, filepath.Base(dir)+"-artifacts")
	store, err := NewFilesystemArtifactStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	loader, err := NewPersistentRuntimeGitLoader(dir, &mutableImageResolver{id: imageID('a')}, store)
	if err != nil {
		t.Fatal(err)
	}
	return loader
}

func activateSnapshotProblem(t *testing.T, loader *GitLoader, problemID string) models.Problem {
	t.Helper()
	candidate, err := loader.BuildCandidate(context.Background())
	if err != nil {
		t.Fatalf("build runtime catalog: %v", err)
	}
	problems := candidate.Problems()
	if err := activateCatalogCandidateForTest(loader, candidate); err != nil {
		t.Fatalf("activate runtime catalog: %v", err)
	}
	for _, problem := range problems {
		if problem.ID == problemID {
			return problem
		}
	}
	t.Fatalf("activated catalog does not contain problem %q", problemID)
	return models.Problem{}
}

func mutateSnapshotProblem(t *testing.T, dir string) {
	t.Helper()
	problemDir := filepath.Join(dir, "test-problem")
	manifestPath := filepath.Join(problemDir, "problem.yaml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = []byte(strings.Replace(string(manifest), `title: "Test Problem"`, `title: "Changed Problem"`, 1))
	if err := os.WriteFile(manifestPath, manifest, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(problemDir, "setup.sh"), []byte("#!/bin/sh\necho changed setup\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(problemDir, "verify.sh"), []byte("#!/bin/sh\necho changed verify\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(problemDir, "hint.md"), []byte("changed hint\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func resolveSnapshotRuntime(t *testing.T, loader *GitLoader, ref runner.ProblemRef) runner.LocalDockerRuntime {
	t.Helper()
	runtime, err := loader.ResolveRuntime(context.Background(), ref)
	if err != nil {
		t.Fatalf("resolve runtime %+v: %v", ref, err)
	}
	return runtime
}

func resolveSnapshotProblem(t *testing.T, loader *GitLoader, ref runner.ProblemRef) models.Problem {
	t.Helper()
	problem, err := loader.ResolveProblem(context.Background(), ref)
	if err != nil {
		t.Fatalf("resolve problem %+v: %v", ref, err)
	}
	return problem
}

func assertSameRuntimeSnapshot(t *testing.T, got, want runner.LocalDockerRuntime) {
	t.Helper()
	if got.Revision != want.Revision || got.Image != want.Image || got.SetupScript != want.SetupScript ||
		got.VerifyScript != want.VerifyScript || got.VerifyType != want.VerifyType {
		t.Errorf("runtime revision mapped to different content\ngot:  %+v\nwant: %+v", got, want)
	}
}

func assertSameProblemSnapshot(t *testing.T, got, want models.Problem) {
	t.Helper()
	if got.Revision != want.Revision || got.Title != want.Title || got.Description != want.Description ||
		got.Hint != want.Hint || got.CorrectChoice != want.CorrectChoice || got.GradingPrompt != want.GradingPrompt {
		t.Errorf("problem revision mapped to different content\ngot:  %+v\nwant: %+v", got, want)
	}
}
