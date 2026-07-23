package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

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
	auth.POST("/logout", h.Logout)
	auth.DELETE("/logout", h.Logout)
	auth.GET("/me", middleware.Auth(h.service), h.Me)
}

// cookieSecure is true when the frontend is served over https, so the
// oauth_state cookie gets the Secure flag in production.
func (h *Handler) cookieSecure() bool {
	return strings.HasPrefix(h.service.cfg.FrontendURL, "https://")
}

func (h *Handler) setStateCookie(c *gin.Context, value string, maxAge int) {
	// SameSite=Lax blocks the state cookie from being sent on cross-site
	// POSTs (CSRF); Secure is set when the frontend is https.
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("oauth_state", value, maxAge, "/", "", h.cookieSecure(), true)
}

func (h *Handler) GithubLogin(c *gin.Context) {
	state := generateState()
	h.setStateCookie(c, state, 300)
	c.Redirect(http.StatusTemporaryRedirect, h.service.GetAuthURL(state))
}

func (h *Handler) GithubCallback(c *gin.Context) {
	frontend := h.service.cfg.FrontendURL

	state := c.Query("state")
	cookieState, err := c.Cookie("oauth_state")
	if err != nil || state == "" || state != cookieState {
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

	h.setStateCookie(c, "", -1)

	userJSON, err := json.Marshal(u)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, frontend+"/login?error=auth_failed")
		return
	}

	q := url.Values{}
	q.Set("access_token", accessToken)
	q.Set("refresh_token", refreshToken)
	q.Set("user", url.QueryEscape(string(userJSON)))

	// Tokens go in the URL FRAGMENT, not the query string. The fragment is
	// never sent to the server, so it does not land in nginx/proxy access
	// logs, and it is not leaked via the Referer header. The SPA reads it
	// from location.hash. (Long-term: migrate to httpOnly cookies.)
	c.Redirect(http.StatusTemporaryRedirect, frontend+"/login#"+q.Encode())
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

// Logout revokes the caller's refresh token server-side so it cannot be
// reused after the client clears its local storage.
func (h *Handler) Logout(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = c.ShouldBindJSON(&req)
	h.service.RevokeRefresh(c.Request.Context(), req.RefreshToken)
	c.Status(http.StatusNoContent)
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
