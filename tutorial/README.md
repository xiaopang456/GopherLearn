# tutorial/ — what is published here

This directory holds the small set of files that the code and its tests actually
depend on. It is deliberately small: every file here is read by a test, by a
deployment script, or by a compose file, and each one is there so that a claim
the repository makes can be checked.

| Path | What it is | Why it is here |
|---|---|---|
| `v2-entry/isolated.example.toml` | the integrated entry's example configuration | read **by the test suite** (`v2/internal/isolated/assembly_test.go`, `hybrid/entry_test.go`, `hybrid/support_test.go`, `hybrid/cmd/gopherlearn-integrated/main_test.go`). Without it the tests cannot run. |
| `v2-entry/deployment-agent-selectors.toml` | the two agent-mode selectors this build supports | read by `v2/internal/isolated/assembly_test.go`. |
| `v2-j1-knowledge/frozen/image-entry-evidence/entrypoint.sh.txt` | the pinned Redis Stack image's own `/entrypoint.sh`, verbatim | `deploy/redis-stack-preflight.sh` and `compose.integrated.yml` cite these exact files and line numbers as the provenance for the preflight wrapper's behaviour. |
| `v2-j1-knowledge/frozen/image-entry-evidence/entry-manifest.json` | the same image's manifest and layer digests | cited by `deploy/redis-stack-preflight.sh` for the same reason. |

Everything else about this project is published in [`docs/`](../docs/) — the
[public release notes](../docs/PUBLIC-RELEASE-NOTES.md), the
[integrated-stack deployment reference](../docs/INTEGRATED-DEPLOYMENT.md), the
[credential handling notes](../docs/CREDENTIAL-HANDLING.md) and the
[third-party dependency inventory](../docs/THIRD-PARTY.md).
