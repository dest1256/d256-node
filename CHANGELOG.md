# Changelog

## Unreleased
- Stage 5: sealed state cache (starts Xray without the panel), encrypted bbolt traffic journal (written only while offline and on shutdown), sequence epochs, hour-wise compaction, offline quota/expiry enforcement, exactly-once delivery after reconnect.
- Stage 4: embedded xray-core; atomic apply of the panel's desired state with rollback; restart-free user/inbound updates planned by the panel; online users; geo files in the image.
- Stage 3: `enroll`, mTLS gRPC client with reconnect/backoff, heartbeat + RTT, host metrics, `status`/`logs`/`reload`/`reset`.
- Stage 2: repository skeleton.
