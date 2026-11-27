# Security policy

## Supported versions

| Version | Supported |
|---|---|
| 1.0.x | yes |
| older | no |

## Reporting a vulnerability

Please do not report security problems in public issues.

1. **Preferred:** use GitHub's private vulnerability reporting. On the repository, open the
   **Security** tab, then **Report a vulnerability**. Only the maintainers see the report.
2. If you can't use GitHub, email **support@ksolves.com** with "nifi-operator security" in the
   subject.

Include a description of the problem, the affected version and, if possible, steps to reproduce.

We acknowledge reports within 3 working days, keep you informed while we work on a fix, and credit
you in the release notes unless you prefer otherwise.

## What to expect

These are goals, not guarantees:

| Step | Target |
|---|---|
| Acknowledge your report | 3 working days |
| First assessment (confirmed or not, severity) | 10 working days |
| Fix released for critical and high severity | 30 days after confirmation |
| Fix released for medium and low severity | the next minor release |

Vulnerabilities in Apache NiFi itself belong to the Apache NiFi project:
<https://nifi.apache.org/security.html>.

## Internal process

How we triage a report, the patch and backport flow, and advisory publishing are documented
internally in [docs/security-response.md](docs/security-response.md).
