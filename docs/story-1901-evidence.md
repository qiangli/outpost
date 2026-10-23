# Story 1901 implementation evidence

Sprint: #495

Story-ID: 702c08bd82e2

The outpost implementation adds boot-service defaults on every supported OS,
explicit no-sleep policy with `--keep-sleep`, OS readbacks, and durable reboot
verification tied to an accepted server heartbeat. The separate bashy setup
recipe is outside this change. Operator instructions are in [service.md](service.md)
and are synchronized with the embedded documentation.

## Automated gates

- RED: the new platform command/readback tests failed to compile before the
  `postinstall` implementation existed (`undefined: PowerCommands`, `Readback`).
- GREEN: platform table tests and fake Windows/macOS/Linux reboot lifecycles pass.
  They cover AC/DC settings, incomplete output, persistent versus runtime masks,
  boot versus session services, power permission failures, dry-run and opt-out,
  same-boot restarts, missing evidence storage, denied reboot scheduling,
  preservation of login observations across restart, and successful server ack.
- `go test -race ./internal/agent/postinstall ./internal/agent/heartbeat`: PASS.
- `go test -tags fb_archives -p 2 ./...`: PASS, 58 packages with tests and eight
  packages without tests. The initial unconstrained run encountered deadlines in
  `ollama/TestWatcher_Status_TracksPushAndError` and
  `shard/TestStart_WiresLaunchesAndStops`; both passed isolated reruns and the
  complete lower-concurrency gate.
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -p 2 ./...`: PASS.
- `GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -p 2 ./...`: PASS.
- `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -p 2 ./...`: PASS.
- `go vet ./...` and `go vet -tags fb_archives ./...`: PASS, no diagnostics.
- `git diff --check`: PASS.

No live service registrations, power changes, or reboots were performed by this
worker. Live acceptance remains with the manager: install and verify on each of
the three approved non-production platforms, including one unattended reboot,
server visibility, and configured SSH reachability. Session evidence consists
of conservative startup and acknowledgement snapshots, not a historical audit;
unknown observations cannot yield PASS.
