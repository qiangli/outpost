# Paired-host local retention

This inventory records limits verified in source, not a claim that every local
writer is bounded. Limits apply per host and per independently named store.

* Supervised child output (`daemon.log` and `bashy-supervisord.log`) is redacted
  before storage. Each active log and its `.1` predecessor is at most 8 MiB
  (16 MiB per child total). Rotation closes the active handle before renaming;
  oldest output is discarded. A pre-existing oversized active log is truncated
  on opening and an oversized `.1` is removed. Legacy data is not retroactively
  redacted. If opening fails, output falls back to the supervisor's standard
  streams; if rotation or writing fails, output is dropped with a warning so
  the child can continue. Limits require exclusive ownership of the paths and
  successful filesystem operations; failures may leave legacy oversized files.
* Yoke telemetry spool at `d1a8a0a1` defaults to 64 MiB, with bounded
  admission and in-place compaction (`pkg/telemetry/spool.go`). This describes
  that version, not an assertion that every installed producer has upgraded.
* Outpost's recovery journal retains at most 64 events and 16 KiB
  (`internal/agent/recovery/journal.go`). Post-install reports replace named
  JSON files (`internal/agent/postinstall/postinstall.go`); that writer has no
  explicit byte limit, and interrupted temporary files are not a retention
  guarantee.
* `upgrade.log` and `nodegc.log` are append-only and have no size or age cap
  (`internal/agent/upgrade/ledger.go`, `internal/agent/nodegc/ledger.go`). The
  latter also stores the restart-safe deletion budget. These need a separate
  bounded retention design before claiming all diagnostic evidence is bounded.
* Persistent Go build and module caches are intentionally preserved. No size
  or age cap is established by these Outpost sources; monitor their volume and
  manage capacity according to host policy, without deleting them automatically.

Umbrella story `a37801b63b47` records a 9.8 GB `daemon.log` on a full Windows
system drive. The supervisor's former append-only file open explains its
unbounded growth; the specific source of the high output volume remains
unconfirmed. This is distinct from the earlier OTel rotation scratch incident
recorded in Yoke story `e385262ac250`, addressed by Yoke `d1a8a0a1`.
