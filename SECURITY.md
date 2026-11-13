# Security policy

## Supported versions

arednlink-hf is alpha software. Only the latest release receives fixes.

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's private vulnerability
reporting: <https://github.com/25Charlie-Dev/arednlink-hf/security/advisories/new>
(or the repository's **Security** tab, **Report a vulnerability**). Do not
open a public issue for a suspected vulnerability.

Include the release or commit, the radio adapter in use (KISS, JS8Call or
none), and the steps or input that trigger the problem. Reports about
unintended radio transmission are treated as the highest priority.

## Disclosure timeline

This project follows coordinated disclosure:

| Step | Target |
|---|---|
| Acknowledge the report | within 7 days |
| Confirm or decline, with an initial severity assessment | within 14 days |
| Release a fix for a confirmed issue | within 90 days of the report |
| Publish a GitHub security advisory, crediting the reporter if they wish | when the fix is released |

If a fix needs more than 90 days, the reporter is told why and given a new
date before the deadline passes. If a vulnerability is already public or
being exploited, a fix and advisory are released as quickly as possible
instead. Reporters are asked not to disclose publicly before the advisory
is published or the 90 days have passed, whichever comes first.

## Release integrity

Each release carries build provenance and SBOM attestations, a CycloneDX
SBOM, an OpenVEX statement for known Go vulnerabilities, and SHA-256
checksums. Verify a download with:

    gh attestation verify <file> --repo 25Charlie-Dev/arednlink-hf

## Known limitation

On AREDN the daemon runs as the shared `nobody` account, as other AREDN
services do. Its local control socket and configuration file are protected
only by that account, so another compromised process running as `nobody`
could enable beaconing, effective at the next restart. Beaconing is off by
default and transmit is rate limited.
