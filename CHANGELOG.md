# Changelog

## Unreleased
- Self-update on the panel's request: signed (Ed25519) release binaries, verified, test-run, installed under data/bin, hand-over by exec, automatic fallback after 3 failed starts; `d256-sign` tool and the Release workflow.
- docker-compose: builds the node and the panel's proto module from GitHub, state in ./data; the Dockerfile context is now this repository.
- Stage 6: dispatcher wrapper (no xray-core patches) records client, inbound, source IP, destination, port, network and bytes per connection; sends live connections and hourly aggregates (journaled, compacted, exactly-once); enforces the device limit, offline too.
- Stage 5: sealed state cache (starts Xray without the panel), encrypted bbolt traffic journal (written only while offline and on shutdown), sequence epochs, hour-wise compaction, offline quota/expiry enforcement, exactly-once delivery after reconnect.
- Stage 4: embedded xray-core; atomic apply of the panel's desired state with rollback; restart-free user/inbound updates planned by the panel; online users; geo files in the image.
- Stage 3: `enroll`, mTLS gRPC client with reconnect/backoff, heartbeat + RTT, host metrics, `status`/`logs`/`reload`/`reset`.
- Stage 2: repository skeleton.
