package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wahrwelt-kit/go-cachekit"
	"github.com/wahrwelt-kit/go-jwtkit"
	"github.com/wahrwelt-kit/go-logkit"
	"github.com/wahrwelt-kit/go-pgkit/pgutil"
	"github.com/wahrwelt-kit/go-pgkit/postgres"
)

type User struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Email string    `json:"email"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log, err := logkit.New(
		logkit.WithLevel(logkit.InfoLevel),
		logkit.WithOutput(logkit.ConsoleOutput),
		logkit.WithServiceName("gin-rest"),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logger: %v\n", err)
		os.Exit(1)
	}

	pool, err := postgres.New(ctx, &postgres.Config{
		URL:      env("DATABASE_URL", "postgres://app:app@localhost:5432/app?sslmode=disable"),
		MaxConns: 10,
	})
	if err != nil {
		log.Fatal("postgres", logkit.Error(err))
	}
	defer pool.Close()

	rdb, err := cachekit.NewRedisClient(ctx, &cachekit.RedisConfig{
		Host: env("REDIS_HOST", "localhost"),
		Port: 6379,
	})
	if err != nil {
		log.Fatal("redis", logkit.Error(err))
	}
	defer rdb.Close()

	cache := cachekit.New(rdb)

	jwtSvc, err := jwtkit.NewJWTService(jwtkit.Config{
		AccessKeys:  []jwtkit.KeyEntry{{Kid: "1", Secret: []byte(env("JWT_SECRET", "change-me-32-bytes-long-secret!!"))}},
		RefreshKeys: []jwtkit.KeyEntry{{Kid: "1", Secret: []byte(env("JWT_SECRET", "change-me-32-bytes-long-secret!!"))}},
		AccessTTL:   15 * time.Minute,
		RefreshTTL:  7 * 24 * time.Hour,
		Issuer:      "gin-rest",
	})
	if err != nil {
		log.Fatal("jwt", logkit.Error(err))
	}

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(ginLogger(log))

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.POST("/auth/login", ginLogin(jwtSvc))

	auth := r.Group("/", ginJWTAuth(jwtSvc))
	auth.GET("/users/:id", ginGetUser(pool, cache))

	srv := &http.Server{Addr: ":8080", Handler: r}

	go func() {
		log.Info("listening on :8080")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("server", logkit.Error(err))
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

func ginLogger(log logkit.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Info("request",
			logkit.Fields{
				"method":   c.Request.Method,
				"path":     c.Request.URL.Path,
				"status":   c.Writer.Status(),
				"duration": time.Since(start).String(),
			},
		)
	}
}

func ginJWTAuth(svc *jwtkit.JWTService) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := jwtkit.ExtractRaw(c.Request)
		if raw == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid token"})
			return
		}
		claims, err := svc.ValidateAccessToken(c.Request.Context(), raw)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		ctx := jwtkit.ClaimsIntoContext(c.Request.Context(), claims)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func ginLogin(jwtSvc *jwtkit.JWTService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			UserID string `json:"user_id" binding:"required"`
			Role   string `json:"role"    binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		uid, err := uuid.Parse(req.UserID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
			return
		}
		pair, err := jwtSvc.GenerateTokenPair(c.Request.Context(), uid, req.Role)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
			return
		}
		c.JSON(http.StatusOK, pair)
	}
}

func ginGetUser(pool *pgxpool.Pool, cache *cachekit.Cache) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
			return
		}
		user, loadErr := cachekit.GetOrLoad(cache, c.Request.Context(), fmt.Sprintf("user:%s", id), 5*time.Minute, func(ctx context.Context) (User, error) {
			var u User
			err := pool.QueryRow(ctx, "SELECT id, name, email FROM users WHERE id = $1", id).Scan(&u.ID, &u.Name, &u.Email)
			return u, err
		})
		if loadErr != nil {
			if pgutil.IsNoRows(loadErr) {
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
			return
		}
		c.JSON(http.StatusOK, user)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
