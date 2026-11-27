# Internal security response process

This page is the internal counterpart to [SECURITY.md](../SECURITY.md): once a report comes in,
it says who does what, how fast, and how a fix reaches users. SECURITY.md is what a reporter
reads; this page is what the team follows.

## Roles

| Role | Does |
|---|---|
| Security triage owner | Owns every report and alert until it's closed: confirms it, sets severity, decides on an embargo. |
| Fix owner | Writes the fix, the backport(s), and a regression test. |
| Release owner | Runs the patch release, the image scan gate, and the push. |
| Verifier | Reproduces the issue before the fix and confirms it's gone after, independently of whoever wrote the fix. |

One person can hold more than one role on a small fix; on anything affecting a supported release,
triage owner and fix owner should be different people.

## Intake

A report can come from: GitHub private vulnerability reporting, the security email in
SECURITY.md, a Dependabot alert, an Apache NiFi or Kubernetes advisory, or an internal finding.
Each becomes a private tracking ticket (security label, affected component, affected versions)
within 1 working day. A security report that lands as a public GitHub issue is moved to a
private channel and the issue is locked.

## Severity and targets

| Severity (CVSS) | Acknowledge | Assessment | Fix released |
|---|---|---|---|
| Critical (9.0+) | 1 working day | 3 working days | 7 days |
| High (7.0–8.9) | 3 working days | 5 working days | 30 days |
| Medium (4.0–6.9) | 3 working days | 10 working days | next minor release (at most 90 days) |
| Low (< 4.0) | 3 working days | 10 working days | next minor release |

A dependency CVE that `govulncheck` reports as actually called takes the CVE's own severity; one
it reports as present but unreachable drops one level. An image CVE with no upstream fix yet is
recorded with the reason (and, where one exists, a VEX statement) and rechecked at the next
release's scan rather than left silent.

## Patch release flow

1. Fix on a private branch (or a GitHub Security Advisory private fork), based on the oldest
   still-supported release line (see SECURITY.md's "Supported versions").
2. The verifier reproduces the issue against that branch before the fix lands, then confirms it's
   gone after.
3. Backport to every supported release line, and forward-port the same fix to `dev` and `main` the
   same day, so a fix never lands on a release line without also landing on the branches the next
   release is cut from.
4. `make release` (which includes the vulnerability scan gate) → `make release-push` → tag → the
   Release workflow. Add a `CHANGELOG.md` "Security" entry naming the CVE or GHSA ID.
5. A third-party CVE (Apache NiFi, a CSI sidecar, a base image) follows the same flow: bump the
   pinned version or digest, scan, patch-release.

## Advisory publishing

- Draft a GitHub Security Advisory while the fix is in progress; request a CVE ID through GitHub
  when the issue is in this project's own code.
- Publish the advisory together with the patch release: affected and fixed versions, severity, a
  workaround if one exists, and credit to the reporter (unless they ask not to be named).
- Default is no public disclosure before the fix ships. Keep the reporter updated throughout, and
  coordinate timing with the Apache NiFi project when the issue involves NiFi itself.

## Dependabot and scanning

- Repository settings (GitHub admin, not code): private vulnerability reporting, Dependabot
  alerts, Dependabot security updates and secret scanning should all be on.
- A Dependabot security-update PR is reviewed within 2 working days and merged within the target
  for its severity above. A routine version-update PR is merged or closed within 14 days.
- Scanning (`govulncheck`, Trivy/Docker Scout) currently runs at build and release time only
  (`make vulncheck` in CI, `make release-scan` before a release is pushed); there is no scheduled
  scan of already-released images between releases yet.
- The triage owner reviews open security tickets against the targets above monthly and reports
  any missed target to the project lead.
