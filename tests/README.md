# Test suite

ModelMesh is implemented in Go. The maintained tests therefore live beside the
Go package as `*_test.go`; the old Python `pytest` files targeted modules that
are not part of this repository and have been removed.

Run the full suite from the repository root:

```sh
go test ./...
go test -race ./...
go vet ./...
go test -run '^$' -bench . ./...
```

Seeded fuzz tests are run by `go test ./...`. For longer fuzz runs, use one of:

```sh
go test -fuzz FuzzValidateNodeID -fuzztime 30s
go test -fuzz FuzzParseObject -fuzztime 30s
go test -fuzz FuzzValidateOllamaURL -fuzztime 30s
go test -fuzz FuzzParseNetworks -fuzztime 30s
go test -fuzz FuzzValidatePercent -fuzztime 30s
```

Coverage includes configuration validation, URL/registry allowlists, admin and
agent authentication, registration/heartbeat validation, routing and streaming,
non-replay semantics, model-pull limits and cancellation, body limits, static
security headers, race-heavy registry access, and concurrent HTTP load.
