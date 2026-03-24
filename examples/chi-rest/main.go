package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wahrwelt-kit/go-cachekit"
	"github.com/wahrwelt-kit/go-httpkit/httputil"
	"github.com/wahrwelt-kit/go-httpkit/httputil/middleware"
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
		logkit.WithServiceName("chi-rest"),
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
		Issuer:      "chi-rest",
	})
	if err != nil {
		log.Fatal("jwt", logkit.Error(err))
	}

	r := chi.NewRouter()

	r.Use(middleware.RequestID())
	r.Use(middleware.Logger(log, nil))
	r.Use(middleware.Recoverer(log))
	r.Use(middleware.SecurityHeaders(false))

	r.Get("/health", httputil.HealthHandler(nil))

	r.Post("/auth/login", loginHandler(jwtSvc))

	r.Group(func(r chi.Router) {
		r.Use(jwtkit.JWTAuth(jwtSvc, jwtkit.WithLogger(log)))
		r.Get("/users/{id}", getUserHandler(pool, cache))
	})

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

func loginHandler(jwtSvc *jwtkit.JWTService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UserID string `json:"user_id"`
			Role   string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httputil.RenderError(w, r, http.StatusBadRequest, "invalid request body")
			return
		}
		uid, err := uuid.Parse(req.UserID)
		if err != nil {
			httputil.RenderError(w, r, http.StatusBadRequest, "invalid user_id")
			return
		}
		pair, err := jwtSvc.GenerateTokenPair(r.Context(), uid, req.Role)
		if err != nil {
			httputil.RenderError(w, r, http.StatusInternalServerError, "token generation failed")
			return
		}
		httputil.RenderJSON(w, r, http.StatusOK, pair)
	}
}

func getUserHandler(pool *pgxpool.Pool, cache *cachekit.Cache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httputil.ParseUUIDField(w, r, chi.URLParam(r, "id"), "id")
		if !ok {
			return
		}

		user, err := cachekit.GetOrLoad(cache, r.Context(), fmt.Sprintf("user:%s", id), 5*time.Minute, func(ctx context.Context) (User, error) {
			var u User
			err := pool.QueryRow(ctx, "SELECT id, name, email FROM users WHERE id = $1", id).Scan(&u.ID, &u.Name, &u.Email)
			return u, err
		})
		if err != nil {
			if pgutil.IsNoRows(err) {
				httputil.RenderError(w, r, http.StatusNotFound, "user not found")
				return
			}
			httputil.RenderError(w, r, http.StatusInternalServerError, "failed to load user")
			return
		}
		httputil.RenderJSON(w, r, http.StatusOK, user)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
