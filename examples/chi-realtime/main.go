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
	"github.com/wahrwelt-kit/go-cachekit"
	"github.com/wahrwelt-kit/go-httpkit/httputil/middleware"
	"github.com/wahrwelt-kit/go-logkit"
	"github.com/wahrwelt-kit/go-wskit"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log, err := logkit.New(
		logkit.WithLevel(logkit.DebugLevel),
		logkit.WithOutput(logkit.ConsoleOutput),
		logkit.WithServiceName("chi-realtime"),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logger: %v\n", err)
		os.Exit(1)
	}

	rdb, err := cachekit.NewRedisClient(ctx, &cachekit.RedisConfig{
		Host: env("REDIS_HOST", "localhost"),
		Port: 6379,
	})
	if err != nil {
		log.Fatal("redis", logkit.Error(err))
	}
	defer rdb.Close()

	hub := wskit.NewHub(
		wskit.WithRedis(rdb, "ws:events"),
		wskit.WithOnConnect(func(c *wskit.Client) {
			data, _ := json.Marshal(wskit.NewEvent("welcome", map[string]string{
				"message": "connected to realtime server",
			}))
			c.Send(data)
			log.Debug("client connected", logkit.Component("hub"))
		}),
	)

	go hub.Run(ctx)
	go hub.SubscribeToRedis(ctx)

	r := chi.NewRouter()

	r.Use(middleware.RequestID())
	r.Use(middleware.Logger(log, nil))
	r.Use(middleware.Recoverer(log))

	r.Get("/ws", func(w http.ResponseWriter, r *http.Request) {
		client, err := wskit.Accept(r.Context(), w, r, hub, nil)
		if err != nil {
			log.Warn("ws accept failed", logkit.Error(err))
			return
		}
		go client.ReadPump()
		go client.WritePump()
	})

	r.Post("/broadcast", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Type    string `json:"type"`
			Payload any    `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if err := hub.BroadcastJSON(r.Context(), req.Type, req.Payload); err != nil {
			http.Error(w, "broadcast failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	r.Get("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{
			"connected_clients": hub.ClientCount(),
		})
	})

	srv := &http.Server{Addr: ":8081", Handler: r}

	go func() {
		log.Info("listening on :8081")
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

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
