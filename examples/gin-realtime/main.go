// Package main demonstrates a real-time WebSocket broadcast server using Gin and go-wskit
package main

import (
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
	defer func() {
		if err := rdb.Close(); err != nil {
			log.Error("redis close", logkit.Error(err))
		}
	}()

	hub := wskit.NewHub(
		wskit.WithRedis(rdb, "ws:events"),
		wskit.WithOnConnect(func(s wskit.Subscriber) {
			data, err := json.Marshal(wskit.NewEvent("welcome", map[string]string{
				"message": "connected to realtime server",
			}), jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
			if err != nil {
				log.Error("marshal welcome event", logkit.Error(err))
				return
			}
			s.Send(data)
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
		// ReadPump uses the request context, so keep this handler alive until it returns.
		go client.WritePump()
		client.ReadPump()
	})

	r.POST("/broadcast", func(c *gin.Context) {
		var req struct {
			Type    string `json:"type"`
			Payload any    `json:"payload"`
		}
		if err := decodeJSONBody(c, &req); err != nil {
			writeJSON(c, http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		if err := hub.BroadcastJSON(c.Request.Context(), req.Type, req.Payload); err != nil {
			writeJSON(c, http.StatusInternalServerError, gin.H{"error": "broadcast failed"})
			return
		}
		c.Status(http.StatusNoContent)
	})

	r.GET("/stats", func(c *gin.Context) {
		writeJSON(c, http.StatusOK, gin.H{
			"connected_subscribers": hub.SubscriberCount(),
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
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("server shutdown", logkit.Error(err))
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
