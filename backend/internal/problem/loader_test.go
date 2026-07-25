package problem

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/k8s-quiz/backend/pkg/models"
)

func setupTestProblems(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

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
