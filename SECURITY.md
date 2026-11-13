# Security policy

## Supported versions

arednlink-hf is alpha software. Only the latest release receives fixes.

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the
repository's **Security** tab and choose **Report a vulnerability**. Do not
open a public issue for a suspected vulnerability.

Include the release or commit, the radio adapter in use (KISS, JS8Call or
none), and the steps or input that trigger the problem. Reports about
unintended radio transmission are treated as the highest priority.

You can expect an acknowledgement within 7 days. Confirmed issues are fixed
in a new release and disclosed through a GitHub security advisory.

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
