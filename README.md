# Static PostgreSQL Builder

This repository contains a Go replacement for `build-postgres-portable-static.sh`. The new tool compiles PostgreSQL and its static dependencies from source using a JSON configuration that lists the download URLs and SHA-256 checksums.

## Features

- Builds static zlib, GNU Readline, OpenSSL (compiled with `OPENSSL_NO_ATEXIT`), ICU, and PostgreSQL from the specified sources while relaxing libpq's exit-symbol sanity check to allow static OpenSSL libs.
- Writes everything into a configurable build workspace (default: `./build`) and produces a bundle under an output directory (default: `./dist/postgresql-<os>-<arch>-<version>`).
- Downloads are checksum-verified when hashes are provided.
- Incremental: each dependency is stamped after a successful build, so re-running after a failure resumes at the first incomplete stage instead of starting over.
- Reusable: previously downloaded archives and extracted source directories are reused automatically.
- Bootstraps PostgreSQL clusters with configurable credentials (defaults to `postgres` / `postgres`).

## Requirements

- Go 1.21+
- Standard build toolchain (clang/LLVM on macOS, gcc/clang + development headers on Linux).
- `tar`, `make`, and other typical Unix build utilities.

## Quick Start

1. Adjust `build-config.json` if you need different versions, URLs, or checksums.
2. Build or run the Go tool:

   ```bash
   go run ./cmd/staticpg --config build-config.json
   ```

   Add `--build-dir=/custom/build` to change the workspace location, `--output-dir=/custom/dist` to change the bundle parent directory (the tool appends the default bundle name), or `--action=clean` to delete the build output and bundle.

3. After a successful run, the bundle lives in `dist/postgresql-<os>-<arch>-<pg_version>` (or under your custom output directory). Use the helper scripts there (`init-database.sh`, `start-server.sh`, etc.) to work with the embedded PostgreSQL instance.

## Failure Recovery

The builder is designed to be resumable:

- Downloaded archives are saved under `<root>/src` and reused on subsequent runs.
- Each dependency build drops a stamp file in `<root>/build/.<component>-built`. Successful steps are skipped on the next run, so only the failed or incomplete component is rebuilt.
- PostgreSQL itself is skipped once `<root>/pgsql/bin/postgres` exists.
- The packaging step checks whether the final bundle already contains `pgsql/bin` and only reassembles it when necessary.

In practice, re-running the same command after fixing the underlying issue will resume from the last incomplete stage.

To force a component to rebuild, remove its stamp file. For example, if you change the OpenSSL configuration, delete `build/build/.openssl-built` (relative to the workspace you chose with `--build-dir`) before rerunning the builder. Removing the extracted source directory (e.g. `build/src/openssl-3.2.1`) is optional but ensures a pristine rebuild.

## Manager Package (`pkg/embpg`)

The `pkg/embpg` package turns a generated bundle into an embeddable PostgreSQL instance that your Go application can manage directly.

**Highlights**
- Validates bundle, data directory, and port configuration before any subprocess is spawned.
- Streams `pg_ctl` output through your logger and mirrors the final log tail into returned errors so failures include the relevant PostgreSQL diagnostics.
- Verifies the chosen TCP port is available before starting.
- Initialises clusters with configurable credentials (defaults to `postgres` / `postgres`) and removes the temporary password file securely.
- Supports structured `StartParameters` with sane defaults (e.g. `max_connections=101`) and allows additional `postgres` flags via `PostgresArgs`.

Typical usage:

```go
mgr, err := embpg.New(embpg.Config{
    BundlePath:    "dist/postgresql-darwin-arm64-16.2",
    DataDir:       filepath.Join(os.TempDir(), "my-app-pg"),
    Port:          55432,
    Username:      "postgres",
    Password:      "postgres",
    Database:      "app_db",
    StartParameters: map[string]string{
        "shared_buffers": "32MB",
    },
})
if err != nil {
    log.Fatal(err)
}

ctx := context.Background()
if err := mgr.Start(ctx); err != nil {
    log.Fatal(err)
}
defer mgr.Stop(ctx)
```

`Manager.Start` waits for the cluster to come online, creating log directories as needed, and `Stop` gracefully shuts PostgreSQL down when you are finished.

## Examples

The `examples/simple` program wires everything together with CLI flags and default environment overrides. Launch it with:

```bash
go run ./examples/simple \
  --bundle dist/postgresql-<os>-<arch>-<version> \
  --data /tmp/embedded-pg \
  --port 55432 \
  --user postgres \
  --password postgres \
  --database app_db
```

Omit flags to fall back to the defaults (`EMBPG_BUNDLE`, `EMBPG_PORT`, `EMBPG_USER`, `EMBPG_PASSWORD`, temp data dir). The example starts PostgreSQL, keeps it running for a short window, and prints connection information so you can validate the bundle manually.

## Testing

Run the full suite from the repository root. If your environment restricts access to the default Go build cache, point `GOCACHE` at a writable directory inside the repo:

```bash
GOCACHE=$(pwd)/.gocache go test ./...
```

This exercises the builder CLI, the manager package, and the example program.
