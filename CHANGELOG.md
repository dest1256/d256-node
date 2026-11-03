# Changelog

## Unreleased
- Stage 4: embedded xray-core; atomic apply of the panel's desired state with rollback; restart-free user/inbound updates planned by the panel; traffic usage as sequenced batches (in memory until stage 5); online users; geo files in the image.
- Stage 3: `enroll` (local key generation, CSR, one-time token), mTLS gRPC client with reconnect/backoff, heartbeat + RTT, host metrics, `status`/`logs`/`reload`/`reset`.
- Stage 2: repository skeleton.
