// Package main demonstrates a REST API server using Gin and go-kit libraries
package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
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

const errorKey = "error"

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
	defer func() {
		if err := rdb.Close(); err != nil {
			log.Error("redis close", logkit.Error(err))
		}
	}()

	cache := cachekit.New(rdb)

	accessSecret, err := requiredJWTSecret("JWT_ACCESS_SECRET")
	if err != nil {
		log.Fatal("jwt configuration", logkit.Error(err))
	}
	refreshSecret, err := requiredJWTSecret("JWT_REFRESH_SECRET")
	if err != nil {
		log.Fatal("jwt configuration", logkit.Error(err))
	}
	if bytes.Equal(accessSecret, refreshSecret) {
		log.Fatal("jwt configuration", logkit.Error(errors.New("JWT_ACCESS_SECRET and JWT_REFRESH_SECRET must differ")))
	}

	jwtSvc, err := jwtkit.NewJWTService(jwtkit.Config{
		AccessKeys:  []jwtkit.KeyEntry{{Kid: "1", Secret: accessSecret}},
		RefreshKeys: []jwtkit.KeyEntry{{Kid: "1", Secret: refreshSecret}},
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
		writeJSON(c, http.StatusOK, gin.H{"status": "ok"})
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
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("server shutdown", logkit.Error(err))
	}
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
			c.Abort()
			writeJSON(c, http.StatusUnauthorized, gin.H{errorKey: "missing or invalid token"})
			return
		}
		claims, err := svc.ValidateAccessToken(c.Request.Context(), raw)
		if err != nil {
			c.Abort()
			writeJSON(c, http.StatusUnauthorized, gin.H{errorKey: "invalid token"})
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
			UserID string `json:"user_id"`
			Role   string `json:"role"`
		}
		if err := decodeJSONBody(c, &req); err != nil || req.UserID == "" || req.Role == "" {
			writeJSON(c, http.StatusBadRequest, gin.H{errorKey: "invalid request body"})
			return
		}
		uid, err := uuid.Parse(req.UserID)
		if err != nil {
			writeJSON(c, http.StatusBadRequest, gin.H{errorKey: "invalid user_id"})
			return
		}
		pair, err := jwtSvc.GenerateTokenPair(c.Request.Context(), uid, req.Role)
		if err != nil {
			writeJSON(c, http.StatusInternalServerError, gin.H{errorKey: "token generation failed"})
			return
		}
		writeJSON(c, http.StatusOK, pair)
	}
}

func ginGetUser(pool *pgxpool.Pool, cache *cachekit.Cache) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			writeJSON(c, http.StatusBadRequest, gin.H{errorKey: "invalid id"})
			return
		}
		user, loadErr := cachekit.GetOrLoad(cache, c.Request.Context(), fmt.Sprintf("user:%s", id), 5*time.Minute, func(ctx context.Context) (User, error) {
			var u User
			err := pool.QueryRow(ctx, "SELECT id, name, email FROM users WHERE id = $1", id).Scan(&u.ID, &u.Name, &u.Email)
			return u, err
		})
		if loadErr != nil {
			if pgutil.IsNoRows(loadErr) {
				writeJSON(c, http.StatusNotFound, gin.H{errorKey: "user not found"})
				return
			}
			writeJSON(c, http.StatusInternalServerError, gin.H{errorKey: "failed to load user"})
			return
		}
		writeJSON(c, http.StatusOK, user)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func requiredJWTSecret(name string) ([]byte, error) {
	secret := os.Getenv(name)
	if secret == "" {
		return nil, fmt.Errorf("%s must be set", name)
	}
	if len(secret) < jwtkit.MinSecretLength {
		return nil, fmt.Errorf("%s must be at least %d bytes", name, jwtkit.MinSecretLength)
	}
	return []byte(secret), nil
}

func decodeJSONBody(c *gin.Context, out any) error {
	dec := jsontext.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	if err := json.UnmarshalDecode(dec, out); err != nil {
		return err
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body contains multiple JSON values")
		}
		return err
	}
	return nil
}

func writeJSON(c *gin.Context, status int, value any) {
	data, err := json.Marshal(
		value,
		jsontext.EscapeForHTML(true),
		jsontext.EscapeForJS(true),
		json.FormatNilMapAsNull(true),
		json.FormatNilSliceAsNull(true),
	)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Status(status)
	_, _ = c.Writer.Write(append(data, '\n'))
}
