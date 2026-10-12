# go-kit

Opinionated Go toolkit - a collection of focused libraries for backend services.

Each kit is an independent module with its own versioning, CI, and minimal dependency surface.

## Kits

| Kit | Description | Current release |
| --- | --- | --- |
| [go-logkit](https://github.com/wahrwelt-kit/go-logkit) | Structured logging (zerolog) | v0.7.0 |
| [go-httpkit](https://github.com/wahrwelt-kit/go-httpkit) | HTTP middleware, error handling, health checks | v0.8.0 |
| [go-pgkit](https://github.com/wahrwelt-kit/go-pgkit) | PostgreSQL pool, migrations, error helpers (pgx) | v1.5.0 |
| [go-jwtkit](https://github.com/wahrwelt-kit/go-jwtkit) | JWT auth, middleware, key rotation, revocation | v0.7.0 |
| [go-cachekit](https://github.com/wahrwelt-kit/go-cachekit) | Redis cache, singleflight, KV store, Pub/Sub | v0.8.0 |
| [go-wskit](https://github.com/wahrwelt-kit/go-wskit) | WebSocket hub with Redis Pub/Sub scaling | v0.5.1 |

The `go-httpkit/metrics` and `go-httpkit/localization` modules are also at v0.8.0.

## Dependency Graph

```mermaid
graph LR
    cachekit --> redis["go-redis/v9"]
    jwtkit --> redis
    wskit --> redis
    httpkit --> validator["validator/v10"]
    pgkit --> pgx["pgx/v5"]
    logkit --> zerolog
```

The six kits are independent Go modules; none imports another kit. The diagram shows representative direct dependencies, including the shared Redis client.

## Quick Start

Requires Go 1.27.2 or newer.

```bash
go get github.com/wahrwelt-kit/go-logkit@v0.7.0
go get github.com/wahrwelt-kit/go-httpkit@v0.8.0
go get github.com/wahrwelt-kit/go-pgkit@v1.5.0
go get github.com/wahrwelt-kit/go-jwtkit@v0.7.0
go get github.com/wahrwelt-kit/go-cachekit@v0.8.0
go get github.com/wahrwelt-kit/go-wskit@v0.5.1
```

Minimal chi server with logging, JWT auth, PostgreSQL and Redis cache:

```go
slogLog := slog.New(logkit.SlogHandler(log))

r := chi.NewRouter()
r.Use(middleware.RequestID())
r.Use(middleware.Logger(slogLog, nil))
r.Use(middleware.Recoverer(slogLog))

r.Get("/health", httputil.HealthHandler(nil))

r.Group(func(r chi.Router) {
    r.Use(jwtkit.JWTAuth(jwtSvc, jwtkit.WithLogger(slogLog)))
    r.Get("/users/{id}", getUserHandler(pool, cache))
})
```

Runnable examples are in [`examples/`](examples/). They need the configured external services and JWT secrets described below.

## Examples

| Example                               | Router | Kits used                                |
| ------------------------------------- | ------ | ---------------------------------------- |
| [chi-rest](examples/chi-rest)         | chi    | logkit, httpkit, pgkit, jwtkit, cachekit |
| [chi-realtime](examples/chi-realtime) | chi    | logkit, httpkit, wskit, cachekit         |
| [gin-rest](examples/gin-rest)         | gin    | logkit, pgkit, jwtkit, cachekit          |
| [gin-realtime](examples/gin-realtime) | gin    | logkit, wskit, cachekit                  |

Both REST examples require distinct `JWT_ACCESS_SECRET` and `JWT_REFRESH_SECRET` values of at least 32 bytes. Defaults are PostgreSQL `postgres://app:app@localhost:5432/app?sslmode=disable`, Redis `localhost:6379`, REST `:8080`, and realtime `:8081`.

```bash
export DATABASE_URL="${DATABASE_URL:-postgres://app:app@localhost:5432/app?sslmode=disable}"
export REDIS_HOST="${REDIS_HOST:-localhost}"
export JWT_ACCESS_SECRET="$(openssl rand -base64 32)"
export JWT_REFRESH_SECRET="$(openssl rand -base64 32)"

psql "$DATABASE_URL" <<'SQL'
CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    email TEXT NOT NULL
);
INSERT INTO users (id, name, email)
VALUES ('00000000-0000-0000-0000-000000000001', 'Demo User', 'demo@example.test')
ON CONFLICT (id) DO NOTHING;
SQL

go run ./examples/chi-rest
```

The sample `/auth/login` handler only demonstrates issuing a JWT pair for the supplied user ID and role. It does not verify credentials and is not production authentication. A real application must authenticate the user and authorize the requested role before issuing tokens.

After starting a REST example, request demo tokens with:

```bash
curl -sS http://localhost:8080/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"user_id":"00000000-0000-0000-0000-000000000001","role":"user"}'
```

## Benchmarks

Reproducible benchmark commands for kits with benchmark functions are in [`benchmarks/`](benchmarks/). go-pgkit currently defines no benchmarks and is intentionally omitted.

Run locally:

```bash
./benchmarks/run.sh
```

## Design Principles

- **Independent modules** - each kit has its own `go.mod`, semver, and CI pipeline
- **Functional options** - consistent configuration pattern across all kits
- **Consumer-side interfaces** - interfaces defined where they are used, not where implemented
- **Zero framework lock-in** - middleware works with `http.Handler`; chi is preferred, not required
- **Minimal dependencies** - each kit pulls only what it needs
