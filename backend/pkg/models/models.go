package models

import "time"

type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

type User struct {
	ID        string    `json:"id"`
	GithubID  int64     `json:"github_id"`
	Username  string    `json:"username"`
	Email     string    `json:"email"`
	AvatarURL string    `json:"avatar_url"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Choice struct {
	ID   string `json:"id" yaml:"id"`
	Text string `json:"text" yaml:"text"`
}

// DefaultBaseImage is the container image used when a problem specifies
// neither image nor base_image.
const DefaultBaseImage = "k3s-base:latest"

// EffectiveImage resolves the container image for a problem session:
// image > base_image > DefaultBaseImage (PROB-13).
func (p *Problem) EffectiveImage() string {
	if p.Image != "" {
		return p.Image
	}
	if p.BaseImage != "" {
		return p.BaseImage
	}
	return DefaultBaseImage
}

type Problem struct {
	ID string `json:"id"`
	// Revision is the content identity of the runtime-relevant problem bundle
	// (problem.yaml, setup.sh, verify.sh, images.lock, and the provider-resolved
	// image content ID in runtime mode). It prevents one identity from resolving
	// to different bytes, but it is not publisher provenance or approval. It is
	// deliberately not exposed to browser clients.
	Revision       string    `json:"-"`
	CatalogActive  bool      `json:"catalog_active,omitempty"`
	Title          string    `json:"title"`
	Description    string    `json:"description"`
	Category       string    `json:"category"`
	Difficulty     string    `json:"difficulty"`
	Type           string    `json:"type"`
	TimeoutMinutes int       `json:"timeout_minutes"`
	VerifyType     string    `json:"verify_type"`
	BaseImage      string    `json:"base_image,omitempty"`
	Image          string    `json:"image,omitempty"`
	Choices        []Choice  `json:"choices,omitempty"`
	CorrectChoice  string    `json:"correct_choice,omitempty"`
	Hint           string    `json:"hint,omitempty"`
	GradingPrompt  string    `json:"grading_prompt,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Attempt struct {
	ID              string     `json:"id"`
	UserID          string     `json:"user_id"`
	ProblemID       string     `json:"problem_id"`
	Status          string     `json:"status"`
	ContainerID     string     `json:"container_id,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	DurationSeconds *int       `json:"duration_seconds,omitempty"`
	VerifyLog       string     `json:"verify_log,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

type ProblemYAML struct {
	ID             string   `yaml:"id"`
	Title          string   `yaml:"title"`
	Description    string   `yaml:"description"`
	Category       string   `yaml:"category"`
	Difficulty     string   `yaml:"difficulty"`
	Type           string   `yaml:"type"`
	TimeoutMinutes int      `yaml:"timeout_minutes"`
	VerifyType     string   `yaml:"verify_type"`
	BaseImage      string   `yaml:"base_image"`
	Image          string   `yaml:"image"`
	Choices        []Choice `yaml:"choices"`
	CorrectChoice  string   `yaml:"correct_choice"`
	GradingPrompt  string   `yaml:"grading_prompt"`
}

type LeaderboardEntry struct {
	Rank          int    `json:"rank"`
	UserID        string `json:"user_id"`
	Username      string `json:"username"`
	AvatarURL     string `json:"avatar_url"`
	SolvedCount   int    `json:"solved_count"`
	TotalAttempts int    `json:"total_attempts"`
}

type Achievement struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Unlocked    bool   `json:"unlocked"`
}
