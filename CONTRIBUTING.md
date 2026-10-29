# Contributing

1. Do not add personal notes, generated evaluation artifacts, credentials or local database dumps.
2. Add tests for changes to memory authorization, Agent tool execution, retrieval filtering or learning workflow transitions.
3. Run the verification commands before opening a pull request:
   - root module: `go vet ./cmd/... ./internal/... ./conversation/...` and `go test -count=1 ./cmd/... ./internal/... ./conversation/...`
   - `v2/`: `go vet ./...` and `go test -count=1 ./...`
   - `hybrid/`: `go vet ./...` and `go test -count=1 ./...`
   - `web/`: `npm ci`, `npm run lint`, `npm run test`, `npm run build`
4. Keep configuration examples secret-free and document any new environment variable in `.env.example`.
5. **The `hybrid` suite needs a POSIX shell on `PATH`.** Three of its cases execute the
   deployment scripts in `deploy/`, and they fail - rather than skip - when no shell can be
   found, so that the deployment seam cannot go unexecuted while the suite reads green. On
   Windows, put a real shell on `PATH` before running them (for example
   `C:\Program Files\Git\usr\bin`); the `bash` that ships in `C:\Windows\System32` is the
   WSL launcher and fails unless the distribution has `/bin/bash` installed.
