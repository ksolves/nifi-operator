#!/bin/sh
# Copyright 2026 Ksolves India Limited.
# SPDX-License-Identifier: Apache-2.0
# Entrypoint of the NiFi pods. The operator ships this file in each node group's ConfigMap
# (internal/controller/nifi-entrypoint.sh, kept identical to this copy) and runs it as the "nifi"
# container's command, so stock apache/nifi images work; images/nifi bakes the same file in for
# custom node images.
#
# Responsibilities:
#  1. Make the image work under an arbitrary runtime UID (no numeric UID/GID is assumed anywhere
#     else in this image or in the operator) by giving the current UID a /etc/passwd entry if one
#     doesn't already exist - the OpenShift "arbitrary UID" pattern.
#  2. Patch the operator's overrides into the image's own nifi.properties *in place*, plus the
#     per-pod values that can't come from a ConfigMap shared by every pod in a node group (this
#     node's own address), and mirror the coordination settings into state-management.xml.
#
# The image's own conf/nifi.properties already carries every property NiFi needs to start (NAR
# paths, work directories, etc.) - it is patched, never replaced outright, the same way the base
# image's own start.sh applies its own overrides.
set -eu

# Files NiFi creates stay group-writable (group 0 through the volumes' setgid directories, see the
# operator's init container), so the data keeps working if the pods later run as another UID in
# group 0, on any storage.
umask 0002

if ! whoami >/dev/null 2>&1; then
  if [ -w /etc/passwd ]; then
    echo "nifi:x:$(id -u):0:nifi user:${HOME:-/opt/nifi/nifi-current}:/sbin/nologin" >>/etc/passwd
  else
    echo "FATAL: running as an unrecognized UID and /etc/passwd is not writable - cannot register a user entry, which NiFi/the JVM require." >&2
    exit 1
  fi
fi

NIFI_HOME=${NIFI_HOME:-/opt/nifi/nifi-current}
CONF_TEMPLATE_DIR=${CONF_TEMPLATE_DIR:-/opt/nifi/conf-template}
CONF_SECRET_DIR=${CONF_SECRET_DIR:-/opt/nifi/conf-secret}
ENTRYPOINT_DIR=${ENTRYPOINT_DIR:-/opt/nifi/entrypoint}

# require_file fails loudly (rather than letting a missing overrides file surface as a raw,
# unhelpful shell I/O error deep inside apply_overrides) when the operator's ConfigMap-mounted
# overrides aren't where they're expected - almost always a sign the ConfigMap volume mount
# itself is misconfigured.
require_file() {
  if [ ! -f "$1" ]; then
    echo "FATAL: expected overrides file $1 not found (ConfigMap not mounted correctly?)" >&2
    exit 1
  fi
}

# Appending a property to a file that doesn't end in a newline would glue it onto the last line.
# A stray blank line is harmless in a properties file, so always ensure one before patching.
printf '\n' >>"$NIFI_HOME/conf/nifi.properties"
printf '\n' >>"$NIFI_HOME/conf/bootstrap.conf"

# apply_property patches (or appends, if absent) a single "key=value" line in a properties
# file. Uses awk with -v (key/value passed as awk variables, never interpolated into the awk
# program text) so a value containing sed-special characters (|, &, \) can't corrupt the file
# or the command - unlike the naive `sed -i "s|^key=.*|key=value|"` this replaced, where such a
# character either breaks the sed expression outright or (worse, silently) substitutes the
# wrong text (a bare "&" in a sed replacement means "the whole matched line").
#
# POSIX awk's -v assignment itself interprets backslash escape sequences (\n, \t, \\, ...) in
# the assigned string, so a literal backslash in $value must be doubled first - otherwise e.g.
# a value of "a\nb" (a literal backslash-n, not a newline) would be silently turned into an
# actual newline by awk before it's ever written out. Doubling every backslash neutralizes
# every such escape: awk's own unescaping then collapses each doubled pair back to the single
# literal backslash the value actually had.
apply_property() {
  file=$1
  key=$2
  value=$3
  escaped_value=$(printf '%s' "$value" | sed 's/\\/\\\\/g')
  awk -v k="$key" -v v="$escaped_value" '
    BEGIN { prefix = k "="; found = 0 }
    index($0, prefix) == 1 { print prefix v; found = 1; next }
    { print }
    END { if (!found) print prefix v }
  ' "$file" >"$file.tmp" && mv "$file.tmp" "$file"
}

# apply_overrides applies every "key=value" line from an overrides file onto a target
# properties file via apply_property.
apply_overrides() {
  overrides_file=$1
  target_file=$2
  while IFS= read -r line; do
    case "$line" in
    '' | '#'*) continue ;;
    esac
    key="${line%%=*}"
    value="${line#*=}"
    apply_property "$target_file" "$key" "$value"
  done <"$overrides_file"
}

require_file "$CONF_TEMPLATE_DIR/nifi.properties.overrides"
apply_overrides "$CONF_TEMPLATE_DIR/nifi.properties.overrides" "$NIFI_HOME/conf/nifi.properties"

# Secret-sourced nifi.properties overrides (spec.configOverrides.secretName), mounted from the
# operator-owned config Secret only when a node group references one. Their keys never overlap
# the ConfigMap's above (the operator assigns each key to exactly one of the two), so the order of
# the two applies doesn't matter.
if [ -f "$CONF_SECRET_DIR/nifi.properties.overrides" ]; then
  apply_overrides "$CONF_SECRET_DIR/nifi.properties.overrides" "$NIFI_HOME/conf/nifi.properties"
fi

# bootstrap.conf.overrides sets things like JVM heap settings and java.arg.N - the same code
# execution risk the entrypoint script itself carries, so it is shipped in the entrypoint Secret
# (always present, possibly empty - a harmless no-op applied here), not the ConfigMap (NIFI-248
# B2; see configmap.go's buildConfigMap).
require_file "$ENTRYPOINT_DIR/bootstrap.conf.overrides"
apply_overrides "$ENTRYPOINT_DIR/bootstrap.conf.overrides" "$NIFI_HOME/conf/bootstrap.conf"

# logback.xml.override/authorizers.xml.override are full-file replacements, not key=value merges
# like the overrides above - logback.xml/authorizers.xml are structured XML, so there's no
# per-key patch to apply. Shipped in the entrypoint Secret for the same reason as
# bootstrap.conf.overrides above (authorizers.xml controls access policy); only present in the
# mounted volume at all when the user actually supplied a replacement (see
# secret.go's buildEntrypointSecret), so a plain existence check is enough to tell "no replacement
# requested" apart from "replace with this content".
if [ -f "$ENTRYPOINT_DIR/logback.xml.override" ]; then
  cp "$ENTRYPOINT_DIR/logback.xml.override" "$NIFI_HOME/conf/logback.xml"
fi
if [ -f "$ENTRYPOINT_DIR/authorizers.xml.override" ]; then
  cp "$ENTRYPOINT_DIR/authorizers.xml.override" "$NIFI_HOME/conf/authorizers.xml"
fi

# set_state_provider_property sets a <property name="NAME">...</property> element of the image's
# conf/state-management.xml, whatever its current value (the stock zk-provider ships "Root Node"
# as /nifi). Every name used here occurs in exactly one provider. The cluster state providers
# validate their own properties and do NOT fall back to the matching nifi.properties values, so
# they are mirrored from the nifi.properties just patched above. sed's replacement side treats \,
# the delimiter (|) and & as special, so the value - operator-generated, but escaped on principle
# rather than assumed safe - has those characters backslash-escaped first.
set_state_provider_property() {
  name=$1
  value=$2
  escaped=$(printf '%s' "$value" | sed 's/[\\|&]/\\&/g')
  sed -i "s|<property name=\"${name}\">[^<]*</property>|<property name=\"${name}\">${escaped}</property>|" \
    "$NIFI_HOME/conf/state-management.xml"
  # sed succeeds even when nothing matched. If the image's stock file ever stops shipping the
  # empty element, carrying on would leave the provider at its default and let two clusters in one
  # namespace share state, so fail loudly instead.
  if ! grep -qF "<property name=\"${name}\">${value}</property>" "$NIFI_HOME/conf/state-management.xml"; then
    echo "FATAL: could not set state provider property \"${name}\" in state-management.xml" \
      "(expected a <property name=\"${name}\"> element in the image's stock file)" >&2
    exit 1
  fi
}

# set_single_user_credentials sets the single-user-provider's Username/Password <property>
# elements in conf/login-identity-providers.xml (self-closing and empty in the stock file, or
# already filled in by an earlier run) to NIFI_AUTH_USERNAME and the operator-computed bcrypt
# NIFI_AUTH_PASSWORD_HASH. awk reads both from ENVIRON - never -v, never the program text, never
# any command line - so neither value ever appears in a process's argv. Both are XML-escaped (the
# hash's bcrypt alphabet needs none, but costs nothing to escape the same way), and spliced in with
# substr() rather than sub(), whose replacement text treats & as "the whole matched text".
set_single_user_credentials() {
  lip="$NIFI_HOME/conf/login-identity-providers.xml"
  require_file "$lip"
  awk '
    function xml_escape(s) {
      gsub(/&/, "\\&amp;", s)
      gsub(/</, "\\&lt;", s)
      gsub(/>/, "\\&gt;", s)
      gsub(/"/, "\\&quot;", s)
      gsub("\047", "\\&apos;", s)
      return s
    }
    BEGIN {
      user = xml_escape(ENVIRON["NIFI_AUTH_USERNAME"])
      hash = xml_escape(ENVIRON["NIFI_AUTH_PASSWORD_HASH"])
    }
    match($0, /<property name="Username"( *\/>|>[^<]*<\/property>)/) {
      $0 = substr($0, 1, RSTART - 1) "<property name=\"Username\">" user "</property>" substr($0, RSTART + RLENGTH)
    }
    match($0, /<property name="Password"( *\/>|>[^<]*<\/property>)/) {
      $0 = substr($0, 1, RSTART - 1) "<property name=\"Password\">" hash "</property>" substr($0, RSTART + RLENGTH)
    }
    { print }
  ' "$lip" >"$lip.tmp"
  mv "$lip.tmp" "$lip"
  # awk succeeds even when nothing matched; a stock file that stops shipping these elements (or
  # ships them twice) would leave NiFi on its random credentials, so fail loudly instead.
  user_lines=$(grep -c '<property name="Username">[^<]' "$lip" || true)
  pass_lines=$(grep -c '<property name="Password">[^<]' "$lip" || true)
  if [ "$user_lines" != 1 ] || [ "$pass_lines" != 1 ]; then
    echo "FATAL: could not set single-user credentials in login-identity-providers.xml" \
      "(expected exactly one Username and one Password <property> element in the image's stock file)" >&2
    exit 1
  fi
}

# Kubernetes coordination: the ConfigMap state provider's name prefix must match the Lease prefix
# the operator chose for this cluster (its NiFiCluster name), so two clusters in one namespace
# never share state ConfigMaps.
lease_prefix=$(sed -n 's/^nifi.cluster.leader.election.kubernetes.lease.prefix=//p' "$NIFI_HOME/conf/nifi.properties")
if [ -n "$lease_prefix" ]; then
  set_state_provider_property "ConfigMap Name Prefix" "$lease_prefix"
fi

# ZooKeeper coordination: the zk-provider must use the same ensemble and root node as leader
# election, or a cluster's state would live somewhere its nodes do not coordinate.
zk_connect=$(sed -n 's/^nifi.zookeeper.connect.string=//p' "$NIFI_HOME/conf/nifi.properties")
if [ -n "$zk_connect" ]; then
  set_state_provider_property "Connect String" "$zk_connect"
  set_state_provider_property "Root Node" "$(sed -n 's/^nifi.zookeeper.root.node=//p' "$NIFI_HOME/conf/nifi.properties")"
fi

# NiFi refuses to start in cluster mode at all without this set (encrypts sensitive processor
# properties at rest; unrelated to TLS). Comes from a Secret (NIFI_SENSITIVE_PROPS_KEY env var,
# see statefulset.go), never written into the ConfigMap.
if [ -n "${NIFI_SENSITIVE_PROPS_KEY:-}" ]; then
  apply_property "$NIFI_HOME/conf/nifi.properties" nifi.sensitive.props.key "$NIFI_SENSITIVE_PROPS_KEY"
fi

# The TLS keystore/truststore password, on TLS node groups only. Comes from the cluster's CA
# Secret (NIFI_KEYSTORE_PASSWORD env var, see statefulset.go), never written into the ConfigMap
#. The CSI driver encodes both PKCS12 files with the same password.
if [ -n "${NIFI_KEYSTORE_PASSWORD:-}" ]; then
  apply_property "$NIFI_HOME/conf/nifi.properties" nifi.security.keystorePasswd "$NIFI_KEYSTORE_PASSWORD"
  apply_property "$NIFI_HOME/conf/nifi.properties" nifi.security.truststorePasswd "$NIFI_KEYSTORE_PASSWORD"
fi

POD_NAME=${POD_NAME:-$(hostname)}
ORDINAL=${POD_NAME##*-}
case "$ORDINAL" in
'' | *[!0-9]*) ORDINAL=0 ;;
esac

# Every node ends up looking identical ("localhost", or some other ambiguous self-address)
# when nifi.cluster.node.address is left blank, which makes each node's own identity trivially
# match whatever address is registered as the current Cluster Coordinator - so every node
# thinks it's being asked to connect to itself, even when a different node genuinely holds
# that role. This is per-pod (ordinal-dependent), so it can't come from the shared ConfigMap.
STATEFULSET_NAME=${POD_NAME%-"$ORDINAL"}
# POD_NAMESPACE comes from the downward API (statefulset.go); the token mount only exists for
# Kubernetes-coordinated pods.
NAMESPACE=${POD_NAMESPACE:-$(cat /var/run/secrets/kubernetes.io/serviceaccount/namespace 2>/dev/null || echo default)}
# NIFI_CLUSTER_DOMAIN comes from the operator's --cluster-domain (default cluster.local).
POD_FQDN="${POD_NAME}.${STATEFULSET_NAME}-headless.${NAMESPACE}.svc.${NIFI_CLUSTER_DOMAIN:-cluster.local}"
apply_property "$NIFI_HOME/conf/nifi.properties" nifi.cluster.node.address "$POD_FQDN"

# nifi.web.http.host isn't just the bind address - other nodes also use it as this node's
# advertised REST API address (e.g. for cross-node NAR-manager calls). The ConfigMap sets it
# to 0.0.0.0 for binding flexibility, but 0.0.0.0 obviously isn't dialable from another pod,
# so every node ends up trying to call itself instead of the actual target node. Confirmed by
# testing: leaving this at 0.0.0.0 and relying on nifi.cluster.node.address alone is NOT
# enough - the NarManager cross-node calls still fail the same way. Overriding it to this
# pod's own FQDN fixes that; the tradeoff is `kubectl port-forward` directly to a pod no
# longer works (it always proxies via 127.0.0.1 inside the pod, and the app no longer listens
# there) - acceptable, since real Service-routed traffic and `kubectl exec ... curl` both
# still work fine.
apply_property "$NIFI_HOME/conf/nifi.properties" nifi.web.http.host "$POD_FQDN"
# Same problem, same fix, for the HTTPS listener used when a node group has TLS enabled
# (tlsProperties sets nifi.web.https.host=0.0.0.0 for the same bind-flexibility reason as
# http.host above): confirmed live, an unfixed 0.0.0.0 here makes every node try to reach
# "itself" at https://0.0.0.0:8443 for cross-node calls (e.g. WebClientServiceException on
# /nifi-api/controller/cluster), exactly the http.host bug already fixed above.
apply_property "$NIFI_HOME/conf/nifi.properties" nifi.web.https.host "$POD_FQDN"

# Sets NiFi's single-user login credentials from the operator-injected Secret (see
# statefulset.go's authEnvVars) instead of leaving NiFi's own auto-generated random
# username/password in place. The credentials live only in conf/login-identity-providers.xml on
# disk (verified live for set-single-user-credentials) - that file is not persisted across a pod restart
# (it lives on the image's own filesystem, not the data/logs PVCs), so this must run on every
# single boot, not just the first, or credentials would silently reset to random on any restart.
# The bcrypt hash is computed by the operator (Secret <cluster>-single-user-hash,
# injected as NIFI_AUTH_PASSWORD_HASH) and written straight into the XML by
# set_single_user_credentials, so no secret appears in any process's argv. NIFI_AUTH_PASSWORD
# itself stays in the env for the probes, which pipe it to curl on stdin.
if [ -n "${NIFI_AUTH_USERNAME:-}" ] && [ -n "${NIFI_AUTH_PASSWORD_HASH:-}" ]; then
  # NiFi's own 12-character minimum, which set-single-user-credentials used to enforce (the
  # operator's webhook rejects shorter ones, but a referenced Secret can still be edited after
  # admission).
  # An unset or empty password fails too (set -u would stop on ${#NIFI_AUTH_PASSWORD} unset).
  auth_password=${NIFI_AUTH_PASSWORD:-}
  if [ "${#auth_password}" -lt 12 ]; then
    echo "ERROR: failed to apply single-user credentials from the authentication Secret" \
      "(NiFi requires a password of at least 12 characters)" >&2
    exit 1
  fi
  set_single_user_credentials
elif [ -n "${NIFI_AUTH_USERNAME:-}" ]; then
  # Fallback for an operator older than 0.12.0, which injects no hash. This exposes the password
  # in nifi.sh's argv (visible via ps). set -eu would already stop here on failure, but with no
  # hint why; the usual cause is a password under NiFi's 12-character minimum.
  if ! "$NIFI_HOME/bin/nifi.sh" set-single-user-credentials "$NIFI_AUTH_USERNAME" "$NIFI_AUTH_PASSWORD"; then
    echo "ERROR: failed to apply single-user credentials from the authentication Secret" \
      "(NiFi requires a password of at least 12 characters)" >&2
    exit 1
  fi
fi

# Not "exec nifi.sh run" directly: nifi.sh's own "run" case shells out to the bootstrap
# process for a "get-run-command" lookup and execs whatever that returns - which silently
# fails (banner printed, clean exit 0, no JVM, no error) when invoked this way, but works
# when run in the background exactly as the base image's own default entrypoint
# (/opt/nifi/scripts/start.sh) does it. Mirror that proven pattern instead of guessing at
# why the direct exec form breaks.
"$NIFI_HOME/bin/nifi.sh" run &
nifi_pid=$!

trap 'echo "received signal, stopping NiFi..."; "$NIFI_HOME/bin/nifi.sh" stop; exit 0' TERM INT

# Surface NiFi's own log file as this container's stdout, so `kubectl logs` shows something.
tail -F --pid="$nifi_pid" "$NIFI_HOME/logs/nifi-app.log" &

wait "$nifi_pid"
