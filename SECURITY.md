# Security Policy

Please do not open a public issue for anything exploitable. Use GitHub's private vulnerability reporting (this repository's **Security**
tab, **Report a vulnerability**) with the version, reproduction steps and impact. Only the latest release receives fixes.

The node's cache and journal are encrypted with a key derived from the node's private key stored on the same disk. This protects against
tampering and casual reading, not against root on the node server; run nodes only on servers you trust. See the README.
