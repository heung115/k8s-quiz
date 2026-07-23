package problem

import (
	"context"
	"fmt"
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
}

func NewGitLoader(repoPath string) *GitLoader {
	return &GitLoader{repoPath: repoPath}
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
			p.BaseImage = "k3s-base:latest"
		}

		hintPath := filepath.Join(problemDir, "hint.md")
		if hintData, err := os.ReadFile(hintPath); err == nil {
			p.Hint = string(hintData)
		}

		problems = append(problems, p)
	}

	return problems, nil
}

func (l *GitLoader) GetProblemDir(problemID string) string {
	return filepath.Join(l.repoPath, problemID)
}

func (l *GitLoader) HasSetupScript(problemID string) bool {
	path := filepath.Join(l.repoPath, problemID, "setup.sh")
	_, err := os.Stat(path)
	return err == nil
}

func (l *GitLoader) HasVerifyScript(problemID string) bool {
	path := filepath.Join(l.repoPath, problemID, "verify.sh")
	_, err := os.Stat(path)
	return err == nil
}

func (l *GitLoader) GetSetupScript(problemID string) (string, error) {
	path := filepath.Join(l.repoPath, problemID, "setup.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (l *GitLoader) GetVerifyScript(problemID string) (string, error) {
	path := filepath.Join(l.repoPath, problemID, "verify.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
