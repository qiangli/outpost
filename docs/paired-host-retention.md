# Paired-host local retention

| Store | Limit and behavior |
| --- | --- |
| Supervised child output | Outpost redacts output before storage. Each child gets an 8 MiB active log and one 8 MiB previous log. Rollover closes the active file first. A write failure drops output with a warning and lets the child continue. |
| Detached service output, CLI trace, upgrade and scheduler logs | Each file has a 16 MiB cap. The writer compacts in place under a file lock and keeps complete recent lines. |
| Outpost recovery and post-install evidence | The recovery journal retains up to 64 events and 16 KiB. Post-install evidence uses three names with a 1 MiB limit each and one shared 1 MiB temporary slot. |
| Node collector audit ledger | At 16 MiB the ledger stops accepting writes. The collector checks room for a durable record before each deletion and keeps existing records for its restart-safe budget. |
| Yoke telemetry spool | The current writer compacts in place at 64 MiB. It refuses new records at the 128 MiB hard limit. |
| Go build and module caches | Keep both caches inside a dedicated persistent mount. The WSL build requires the declared mount and byte ceiling, checks 95% headroom before starting, and leaves runtime capacity enforcement to the filesystem. |

[Service diagnostics](service.md#paired-host-disk-pressure) describes
how to verify the cache volume ceiling and read the paired-host disk status.

Umbrella story `a37801b63b47` records a separate 9.8 GB service log incident.
The former append-only supervisor writer explains why the file could grow.
The source of that output volume remains unconfirmed. Yoke story
`e385262ac250` records the earlier telemetry rotation scratch incident.
