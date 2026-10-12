# Benchmarks

Run the benchmark suite for each sibling kit that currently defines benchmark functions:

```bash
./benchmarks/run.sh
```

The runner uses `go test -bench=. -benchmem -count=1 -run='^$' ./...` in each kit. To run one kit manually, use the same command from its module directory. Results depend on the Go version, hardware, and dependency versions; this repository does not publish fixed timing claims.

go-pgkit currently has no `Benchmark...` functions and is therefore not listed by `run.sh`.
