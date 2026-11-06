# Oclket cluster

Each process stores a local replica, accepts authenticated peer connections, and broadcasts local writes to its connected peers. Configure a connected peer graph; peers exchange snapshots on connection and reconnect automatically.

## Configuration

- `OCLKET_ENV`: environment file path, default `/etc/oclket/oclket.env`.
- `OCLKET_LISTEN`: TCP listen address, default `:8445`.
- `OCLKET_PEERS`: comma-separated peer addresses, for example `10.0.0.2:8445,10.0.0.3:8445`.
- `OCLKET_SOCKET`: local Unix socket path, default `/tmp/oclket.sock`.
- `OCLKET_SOCKET_MODE`: Unix socket permissions in octal, default `0666`.
- `OCLKET_PASSWORD`: shared peer-authentication secret; required. Installation generates a random secret in the config file if it does not exist.

The installer creates `/etc/oclket/oclket.env` and its parent directory only when needed. It never replaces an existing config. The generated config is restricted to mode `0600`. Existing configs must be updated with `OCLKET_PASSWORD`; startup rejects invalid addresses, socket modes, malformed config lines, or an empty password.

All nodes in a cluster must use the same build and the same `OCLKET_PASSWORD`; copy the generated secret to the other nodes. Peer addresses must be reachable between nodes. Data is held in memory and is lost when a process restarts.

The default socket mode `0666` permits every local user to use the unauthenticated local GET/SET/DEL API. Use `0600` or `0660` and configure the service/PHP worker user or group to restrict access.

`oclket.env.example` is a template for manual configuration. Replace its empty password before starting the service.

Install or update the systemd service with `./oclket -install`. The installer creates the config directory and a random-password config only when the file is absent; it does not print or overwrite the secret.

## Local socket protocol

Every frame is an 8-byte big-endian header (`command uint32`, `payload length uint32`) followed by the payload. `L_GET` and `L_DEL` use raw key bytes. `L_SET` is binary: `[TTL int64][KEY_LEN uint32][KEY][VALUE_LEN uint32][VALUE]`; TTL is in seconds and `0` means no expiration. Local `A_ROW` contains the raw value bytes, `A_EMP` means the key is missing, `A_OK` confirms a successful write, and `A_ERR` contains a UTF-8 message. Peer replication also uses binary payloads and carries timestamps so concurrent updates and deletes can be reconciled.

## Performance profiling

Run the tests and available storage/network benchmarks with latency and allocation statistics:

```sh
go test ./...
go test -run '^$' -bench '^(BenchmarkProfile|BenchmarkReadPacket|BenchmarkSendSnapshot)' -benchmem -count=5
```

Capture CPU and allocation profiles for those operations:

```sh
go test -run '^$' -bench '^(BenchmarkProfileGetParallel|BenchmarkSendSnapshot)$' -benchtime=5s -cpuprofile=cpu.out -memprofile=heap.out
go tool pprof -top cpu.out
go tool pprof -sample_index=alloc_space -top heap.out
```