# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in Ichnos, please report it responsibly. **Do not open a public GitHub issue.**

To report a vulnerability, use [GitHub Private Vulnerability Reporting](https://github.com/ichnos-project/ichnos/security/advisories/new).

Please include in your report:

* Description of the vulnerability
* Steps to reproduce the issue
* Affected versions
* Any potential impact you have identified

## Response Timeline

The Ichnos security team will acknowledge receipt of your report within **5 business days** and will provide an estimated timeline for a fix within **10 business days**.

The team will keep you informed of progress toward a fix and may ask for additional information. We aim to release a fix for vulnerabilities of medium or higher severity within 60 days.

## Supported Versions

Ichnos has not reached v1.0. Until then, only the latest release receives security fixes.

| Version | Supported |
| ------- | --------- |
| Latest release | Yes |
| Older releases | No |

## Disclosure Policy

When a security issue is confirmed, the Ichnos team will:

1. Develop and test a fix
2. Assign a CVE identifier if appropriate
3. Release a patched version
4. Publish a security advisory via [GitHub Security Advisories](https://github.com/ichnos-project/ichnos/security/advisories)

## Security Response Team

The security response team handles all reports of security vulnerabilities according to this policy. See [GOVERNANCE.md](GOVERNANCE.md) for how the security response team is appointed and maintained.

Current security response team members are listed in [MAINTAINERS.md](MAINTAINERS.md).

## Security Best Practices

* [OpenSSF Scorecard](https://scorecard.dev/viewer/?uri=github.com/ichnos-project/ichnos) runs on every push to `main` and every week.
* The project is working towards the [OpenSSF Best Practices Badge](https://www.bestpractices.dev/).
* Dependencies are updated by an automated tool, and every update is reviewed before merge.
* Every GitHub Action is pinned to a full commit SHA, and workflow tokens have the least privilege they need.
* From the first release, binaries and container images are signed with [Sigstore](https://www.sigstore.dev/) and published with SLSA provenance and a Software Bill of Materials (SBOM).
