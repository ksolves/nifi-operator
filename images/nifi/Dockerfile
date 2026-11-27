# Copyright 2026 Ksolves India Limited.
# SPDX-License-Identifier: Apache-2.0
# Pinned by digest for reproducible builds; bump both together. 2.12.0 is the version this image
# is built and tested with. The other supported lines (NiFi 1.14.0 and later, docs/compatibility.md)
# are built from the same file with --build-arg NIFI_IMAGE (make docker-build-nifi-version).
ARG NIFI_IMAGE=apache/nifi:2.12.0@sha256:6f7f2840dbf178c6dd3fa13435b9b358f3be3c6fb7417de467a35a267f4d28ad
FROM ${NIFI_IMAGE}

# Run as a fixed, but non-privileged, numeric UID with GID 0 ("root group"), the
# OpenShift/arbitrary-UID convention: the actual runtime UID may be overridden by the Pod's
# SecurityContext (or an OpenShift SCC), and entrypoint.sh reconciles /etc/passwd for whichever
# UID actually shows up at container start. No UID/GID is hardcoded into file ownership checks.
USER root

COPY scripts/entrypoint.sh /opt/nifi/entrypoint.sh

# Pull in Debian security fixes released after the pinned base image was built. Older NiFi lines
# sit on Debian releases that no longer get updates (their mirrors are gone), so their builds pass
# OS_UPGRADE=best-effort and keep the base packages as they are.
# DEBIAN_MIRROR replaces deb.debian.org (e.g. http://ftp.de.debian.org/debian) where that CDN is
# slow; empty keeps the image's own sources.
ARG OS_UPGRADE=required
ARG DEBIAN_MIRROR=
RUN if [ -n "$DEBIAN_MIRROR" ]; then \
        sed -i "s|http://deb.debian.org/debian|$DEBIAN_MIRROR|g" /etc/apt/sources.list.d/*.sources /etc/apt/sources.list 2>/dev/null || true; \
    fi && \
    if apt-get update && \
        DEBIAN_FRONTEND=noninteractive apt-get -y --no-install-recommends upgrade; then \
        :; \
    elif [ "$OS_UPGRADE" = best-effort ]; then \
        echo "WARNING: no Debian updates for this base image; its packages stay as released"; \
    else \
        exit 1; \
    fi && \
    rm -rf /var/lib/apt/lists/*

# The nifi user's primary group is 0 (it was 1000): runtimes take the primary group from
# /etc/passwd when only the UID is set, and the files NiFi writes then belong to group 0, so the
# pods can later run as any other UID in group 0 on any storage.
RUN usermod -g 0 nifi && \
    chmod +x /opt/nifi/entrypoint.sh && \
    chgrp -R 0 /opt/nifi/nifi-current && \
    chmod -R g=u /opt/nifi/nifi-current && \
    chmod g=u /etc/passwd

USER 1000:0

ENTRYPOINT ["/opt/nifi/entrypoint.sh"]
