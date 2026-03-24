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

	"github.com/gin-gonic/gin"
	"github.com/wahrwelt-kit/go-cachekit"
	"github.com/wahrwelt-kit/go-logkit"
	"github.com/wahrwelt-kit/go-wskit"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log, err := logkit.New(
		logkit.WithLevel(logkit.DebugLevel),
		logkit.WithOutput(logkit.ConsoleOutput),
		logkit.WithServiceName("gin-realtime"),
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

	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/ws", func(c *gin.Context) {
		client, err := wskit.Accept(c.Request.Context(), c.Writer, c.Request, hub, nil)
		if err != nil {
			log.Warn("ws accept failed", logkit.Error(err))
			return
		}
		go client.ReadPump()
		go client.WritePump()
	})

	r.POST("/broadcast", func(c *gin.Context) {
		var req struct {
			Type    string `json:"type"`
			Payload any    `json:"payload"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		if err := hub.BroadcastJSON(c.Request.Context(), req.Type, req.Payload); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "broadcast failed"})
			return
		}
		c.Status(http.StatusNoContent)
	})

	r.GET("/stats", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
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
