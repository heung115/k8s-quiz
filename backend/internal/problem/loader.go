package problem

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/k8s-quiz/backend/pkg/models"
	"gopkg.in/yaml.v3"
)

type Loader interface {
	LoadAll(ctx context.Context) ([]models.Problem, error)
	GetProblemDir(problemID string) string
}

type GitLoader struct {
	repoPath string
	clean    string
}

func NewGitLoader(repoPath string) *GitLoader {
	return &GitLoader{repoPath: repoPath, clean: filepath.Clean(repoPath)}
}

// safeJoin resolves repoPath/problemID/file and guarantees the result stays
// inside repoPath, defeating path-traversal via a crafted problemID (e.g.
// "../../etc"). Without this, GetSetupScript could read & execute an
// arbitrary host file.
func (l *GitLoader) safeJoin(problemID, file string) (string, error) {
	full := filepath.Clean(filepath.Join(l.repoPath, problemID, file))
	if full != l.clean && !strings.HasPrefix(full, l.clean+string(os.PathSeparator)) {
		return "", fmt.Errorf("problem path escapes repo root")
	}
	return full, nil
}

func (l *GitLoader) LoadAll(ctx context.Context) ([]models.Problem, error) {
	entries, err := os.ReadDir(l.repoPath)
	if err != nil {
		return nil, fmt.Errorf("read problems dir: %w", err)
	}

	var problems []models.Problem
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		// Directory names become problem ids; reject anything that could
		// escape the root or break URLs/paths.
		if !validProblemID(entry.Name()) {
			continue
		}
		problemDir := filepath.Join(l.repoPath, entry.Name())
		yamlPath := filepath.Join(problemDir, "problem.yaml")

		data, err := os.ReadFile(yamlPath)
		if err != nil {
			continue
		}

		var py models.ProblemYAML
		if err := yaml.Unmarshal(data, &py); err != nil {
			continue
		}

		p := models.Problem{
			ID:             py.ID,
			Title:          py.Title,
			Description:    py.Description,
			Category:       py.Category,
			Difficulty:     py.Difficulty,
			Type:           py.Type,
			TimeoutMinutes: py.TimeoutMinutes,
			VerifyType:     py.VerifyType,
			BaseImage:      py.BaseImage,
			Image:          py.Image,
			Choices:        py.Choices,
			CorrectChoice:  py.CorrectChoice,
			GradingPrompt:  py.GradingPrompt,
		}

		if p.TimeoutMinutes == 0 {
			p.TimeoutMinutes = 30
		}
		if p.BaseImage == "" && p.Image == "" {
			p.BaseImage = models.DefaultBaseImage
		}

		// PROB-12: skip invalid problems (bad id/enums/choice rules) with a
		// server log; the valid subset still loads.
		if msg := validateLoadedProblem(&p); msg != "" {
			log.Printf("skipping invalid problem %q: %s", entry.Name(), msg)
			continue
		}

		if hintPath, err := l.safeJoin(entry.Name(), "hint.md"); err == nil {
			if hintData, err := os.ReadFile(hintPath); err == nil {
				p.Hint = string(hintData)
			}
		}

		problems = append(problems, p)
	}

	return problems, nil
}

func (l *GitLoader) GetProblemDir(problemID string) string {
	p, err := l.safeJoin(problemID, "")
	if err != nil {
		return ""
	}
	return p
}

func (l *GitLoader) HasSetupScript(problemID string) bool {
	path, err := l.safeJoin(problemID, "setup.sh")
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func (l *GitLoader) HasVerifyScript(problemID string) bool {
	path, err := l.safeJoin(problemID, "verify.sh")
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func (l *GitLoader) GetSetupScript(problemID string) (string, error) {
	path, err := l.safeJoin(problemID, "setup.sh")
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (l *GitLoader) GetVerifyScript(problemID string) (string, error) {
	path, err := l.safeJoin(problemID, "verify.sh")
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// validateLoadedProblem enforces the id format, enum values, and choice
// rules on a problem parsed from YAML (PROB-12). It returns "" when valid.
func validateLoadedProblem(p *models.Problem) string {
	if msg := validateProblem(p); msg != "" {
		return msg
	}
	if p.VerifyType == "choice" {
		if len(p.Choices) < 2 {
			return "choice problem needs at least 2 choices"
		}
		found := false
		for _, ch := range p.Choices {
			if ch.ID == p.CorrectChoice {
				found = true
				break
			}
		}
		if !found {
			return "correct_choice must reference one of the choices"
		}
	}
	return ""
}
