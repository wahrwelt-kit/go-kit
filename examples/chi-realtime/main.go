// Package main demonstrates a real-time WebSocket + SSE broadcast server using chi and go-wskit
package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

//nolint:funlen
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
	slogLog := slog.New(logkit.SlogHandler(log))

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
		// Redis Pub/Sub - enables broadcast across multiple server instances.
		wskit.WithRedis(rdb, "ws:events"),
		// Larger broadcast buffer for high-throughput scenarios.
		wskit.WithBroadcastBuf(256),
		// Per-operation channel timeout (register / unregister / broadcast).
		wskit.WithChannelTimeout(100*time.Millisecond),
		wskit.WithOnTimeout(func(op string) {
			log.Warn("hub channel timeout", logkit.Fields{"op": op})
		}),
		wskit.WithOnConnect(func(s wskit.Subscriber) {
			data, err := json.Marshal(wskit.NewEvent("welcome", map[string]string{
				"message": "connected to realtime server",
			}), jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
			if err != nil {
				log.Error("marshal welcome event", logkit.Error(err))
				return
			}
			s.Send(data)
			log.Debug("subscriber connected", logkit.Component("hub"))
		}),
		wskit.WithOnDisconnect(func(_ wskit.Subscriber) {
			log.Debug("subscriber disconnected", logkit.Component("hub"))
		}),
	)

	go hub.Run(ctx)
	go hub.SubscribeToRedis(ctx)

	r := chi.NewRouter()

	r.Use(middleware.RequestID())
	r.Use(middleware.Logger(slogLog, nil))
	r.Use(middleware.Recoverer(slogLog))

	// WebSocket endpoint - full-duplex, suitable for interactive clients.
	r.Get("/ws", func(w http.ResponseWriter, r *http.Request) {
		// nil = default websocket.AcceptOptions; ClientOptions follow.
		client, err := wskit.Accept(r.Context(), w, r, hub, nil,
			wskit.WithWriteWait(10*time.Second),
			wskit.WithPingInterval(30*time.Second),
			wskit.WithMaxMessageSize(4096),
		)
		if err != nil {
			log.Warn("ws accept failed", logkit.Error(err))
			return
		}
		// ReadPump uses the request context, so keep this handler alive until it returns.
		go client.WritePump()
		client.ReadPump()
	})

	// SSE endpoint - server-to-client only, works through HTTP/1.1 proxies without upgrade.
	r.Get("/events", func(w http.ResponseWriter, r *http.Request) {
		if err := wskit.AcceptSSE(w, r, hub); err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("sse error", logkit.Error(err))
		}
	})

	// Broadcast an event to all connected subscribers (WebSocket + SSE).
	// BroadcastEvent wraps the payload in a typed Event envelope before publishing.
	r.Post("/broadcast", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Type    string `json:"type"`
			Payload any    `json:"payload"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if err := hub.BroadcastEvent(r.Context(), wskit.NewEvent(req.Type, req.Payload)); err != nil {
			http.Error(w, "broadcast failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	r.Get("/stats", func(w http.ResponseWriter, _ *http.Request) {
		if err := writeJSON(w, http.StatusOK, map[string]int{
			"connected_subscribers": hub.SubscriberCount(),
		}); err != nil {
			log.Error("write stats response", logkit.Error(err))
		}
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

func decodeJSONBody(w http.ResponseWriter, r *http.Request, out any) error {
	dec := jsontext.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
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

func writeJSON(w http.ResponseWriter, status int, value any) error {
	data, err := json.Marshal(
		value,
		jsontext.EscapeForHTML(true),
		jsontext.EscapeForJS(true),
		json.FormatNilMapAsNull(true),
		json.FormatNilSliceAsNull(true),
	)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = w.Write(append(data, '\n'))
	return err
}
