package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/pkg/middleware"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	auth := rg.Group("/auth")
	auth.GET("/github", h.GithubLogin)
	auth.GET("/github/callback", h.GithubCallback)
	auth.POST("/refresh", h.Refresh)
	auth.GET("/me", middleware.Auth(h.service), h.Me)
}

func (h *Handler) GithubLogin(c *gin.Context) {
	state := generateState()
	c.SetCookie("oauth_state", state, 300, "/", "", false, true)
	url := h.service.GetAuthURL(state)
	c.Redirect(http.StatusTemporaryRedirect, url)
}

func (h *Handler) GithubCallback(c *gin.Context) {
	frontend := h.service.cfg.FrontendURL

	state := c.Query("state")
	cookieState, err := c.Cookie("oauth_state")
	if err != nil || state != cookieState {
		c.Redirect(http.StatusTemporaryRedirect, frontend+"/login?error=invalid_state")
		return
	}

	code := c.Query("code")
	if code == "" {
		c.Redirect(http.StatusTemporaryRedirect, frontend+"/login?error=missing_code")
		return
	}

	accessToken, refreshToken, u, err := h.service.HandleCallback(c.Request.Context(), code)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, frontend+"/login?error=auth_failed")
		return
	}

	c.SetCookie("oauth_state", "", -1, "/", "", false, true)

	userJSON, err := json.Marshal(u)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, frontend+"/login?error=auth_failed")
		return
	}

	q := url.Values{}
	q.Set("access_token", accessToken)
	q.Set("refresh_token", refreshToken)
	// Frontend Login reads params.get("user") then decodeURIComponent(), so the
	// value must be URI-encoded JSON (QueryEscape here, Encode() escapes again).
	q.Set("user", url.QueryEscape(string(userJSON)))
	c.Redirect(http.StatusTemporaryRedirect, frontend+"/login?"+q.Encode())
}

func (h *Handler) Refresh(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "refresh_token required"})
		return
	}

	accessToken, refreshToken, err := h.service.RefreshAccessToken(c.Request.Context(), req.RefreshToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
	})
}

func (h *Handler) Me(c *gin.Context) {
	u := middleware.GetUser(c)
	c.JSON(http.StatusOK, u)
}

func generateState() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
