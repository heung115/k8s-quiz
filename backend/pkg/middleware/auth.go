package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/pkg/models"
)

const UserContextKey = "current_user"

type TokenValidator interface {
	ValidateAccessToken(tokenString string) (*models.User, error)
}

// AccessTokenCookie is the httpOnly cookie carrying the access JWT for
// browser clients (FRONT-3). Bearer headers stay supported for scripts and
// admin tooling.
const AccessTokenCookie = "access_token"

// Auth accepts the access_token cookie (browser flow) OR an
// Authorization: Bearer header (scripts/tooling).
func Auth(validator TokenValidator) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c)
		if token == "" {
			token, _ = c.Cookie(AccessTokenCookie)
		}
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}

		u, err := validator.ValidateAccessToken(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		c.Set(UserContextKey, u)
		c.Next()
	}
}

func bearerToken(c *gin.Context) string {
	header := c.GetHeader("Authorization")
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || parts[0] != "Bearer" {
		return ""
	}
	return parts[1]
}

func AdminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		u, exists := c.Get(UserContextKey)
		if !exists {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		usr := u.(*models.User)
		if usr.Role != models.RoleAdmin {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin access required"})
			return
		}
		c.Next()
	}
}

func GetUser(c *gin.Context) *models.User {
	u, _ := c.Get(UserContextKey)
	return u.(*models.User)
}
