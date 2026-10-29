# Quality gates

Phase 1 is accepted only when:

- backend code builds and unit tests pass;
- auth, ownership isolation, session CRUD, persistence, and SSE failure paths
  have tests;
- frontend lint, security tests, and production build pass;
- no secret or local absolute path is committed;
- no file is copied from the upstream projects;
- the backend and frontend agree on the API contract;
- the coordinator reviews every produced file before integration.

The first phase deliberately uses in-memory repositories in tests. MySQL and
Redis adapters are added only after domain and API behavior is stable.
