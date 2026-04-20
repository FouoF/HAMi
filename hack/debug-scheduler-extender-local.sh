#!/usr/bin/env bash
# Copyright 2026 The HAMi Authors.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="${ROOT_DIR}/.tmp"
CERT_DIR="${TMP_DIR}/scheduler-debug-certs"
ENV_FILE="${TMP_DIR}/scheduler-extender-debug.env"
LOCAL_PORT="8443"
REMOTE_PORT="443"
NAMESPACE=""
DEPLOYMENT=""
LEAVE_ONLY="false"
CONNECT_ONLY="false"

function usage() {
  cat <<EOF
Usage:
  $(basename "$0") [options]

Options:
  -n, --namespace <ns>       Namespace of scheduler deployment (auto-detect if omitted)
  -d, --deployment <name>    Deployment name for scheduler extender (auto-detect if omitted)
      --local-port <port>    Local listening port for debug service (default: 8443)
      --remote-port <port>   Remote workload port to intercept (default: 443)
      --cert-dir <path>      Directory to store local debug certs
      --connect-only         Only prepare cert/env and connect cluster, skip intercept
      --leave                Leave existing intercept and exit
  -h, --help                 Show help

Outputs:
  - TLS cert/key and device config file under .tmp/
  - env file: .tmp/scheduler-extender-debug.env
EOF
}

function require_cmd() {
  local name="$1"
  if ! command -v "${name}" >/dev/null 2>&1; then
    echo "Error: missing command '${name}'"
    exit 1
  fi
}

function parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -n|--namespace)
        NAMESPACE="$2"
        shift 2
        ;;
      -d|--deployment)
        DEPLOYMENT="$2"
        shift 2
        ;;
      --local-port)
        LOCAL_PORT="$2"
        shift 2
        ;;
      --remote-port)
        REMOTE_PORT="$2"
        shift 2
        ;;
      --cert-dir)
        CERT_DIR="$2"
        shift 2
        ;;
      --leave)
        LEAVE_ONLY="true"
        shift
        ;;
      --connect-only)
        CONNECT_ONLY="true"
        shift
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        echo "Error: unknown option '$1'"
        usage
        exit 1
        ;;
    esac
  done
}

function resolve_target() {
  if [[ -n "${NAMESPACE}" && -n "${DEPLOYMENT}" ]]; then
    return
  fi

  local target
  target="$(kubectl get deploy -A -l app.kubernetes.io/component=hami-scheduler -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}')"
  if [[ -z "${target}" ]]; then
    echo "Error: cannot auto-detect scheduler deployment, please pass --namespace and --deployment"
    exit 1
  fi

  local count
  count="$(printf "%s\n" "${target}" | sed '/^$/d' | wc -l | tr -d ' ')"
  if [[ "${count}" -gt 1 && ( -z "${NAMESPACE}" || -z "${DEPLOYMENT}" ) ]]; then
    echo "Detected multiple scheduler deployments:"
    printf "  %s\n" "${target}"
    echo "Please specify --namespace and --deployment"
    exit 1
  fi

  local ns_and_deploy
  ns_and_deploy="$(printf "%s\n" "${target}" | sed -n '1p')"
  if [[ -z "${NAMESPACE}" ]]; then
    NAMESPACE="${ns_and_deploy%/*}"
  fi
  if [[ -z "${DEPLOYMENT}" ]]; then
    DEPLOYMENT="${ns_and_deploy#*/}"
  fi
}

function generate_cert_if_needed() {
  mkdir -p "${CERT_DIR}"

  local cert_file="${CERT_DIR}/tls.crt"
  local key_file="${CERT_DIR}/tls.key"
  if [[ -f "${cert_file}" && -f "${key_file}" ]]; then
    echo "Using existing local debug certs: ${CERT_DIR}"
    return
  fi

  echo "Generating local debug certificate..."
  openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "${key_file}" \
    -out "${cert_file}" \
    -days 30 \
    -subj "/CN=localhost" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
}

function export_device_config() {
  mkdir -p "${TMP_DIR}"
  local cm_name="${DEPLOYMENT}-device"
  local device_config_file="${TMP_DIR}/device-config.yaml"

  kubectl -n "${NAMESPACE}" get configmap "${cm_name}" -o jsonpath='{.data.device-config\.yaml}' > "${device_config_file}"
  if [[ ! -s "${device_config_file}" ]]; then
    echo "Error: failed to export device config from configmap ${NAMESPACE}/${cm_name}"
    exit 1
  fi
}

function write_env_file() {
  local cert_file="${CERT_DIR}/tls.crt"
  local key_file="${CERT_DIR}/tls.key"
  local device_config_file="${TMP_DIR}/device-config.yaml"

  cat > "${ENV_FILE}" <<EOF
HAMI_HTTP_BIND=0.0.0.0:${LOCAL_PORT}
HAMI_CERT_FILE=${cert_file}
HAMI_KEY_FILE=${key_file}
HAMI_DEVICE_CONFIG_FILE=${device_config_file}
HAMI_SCHEDULER_NAME=hami-scheduler
HAMI_METRICS_BIND_ADDRESS=:9395
HAMI_NODE_SCHEDULER_POLICY=binpack
HAMI_GPU_SCHEDULER_POLICY=spread
HAMI_FORCE_OVERWRITE_DEFAULT_SCHEDULER=true
EOF
}

function connect_cluster() {
  if telepresence status >/dev/null 2>&1; then
    telepresence quit || true
  fi
  telepresence connect -n "${NAMESPACE}" --mapped-namespaces "${NAMESPACE}"
}

function ensure_intercept() {
  # Recreate intercept every run to avoid stale NO_AGENT state after workload rollout/restart.
  telepresence leave "${DEPLOYMENT}" >/dev/null 2>&1 || true
  telepresence intercept "${DEPLOYMENT}" --port "${LOCAL_PORT}:${REMOTE_PORT}"
}

function leave_intercept() {
  telepresence leave "${DEPLOYMENT}" || true
}

parse_args "$@"
require_cmd kubectl
require_cmd telepresence
require_cmd openssl
resolve_target

if [[ "${LEAVE_ONLY}" == "true" ]]; then
  leave_intercept
  echo "Left intercept for ${NAMESPACE}/${DEPLOYMENT}"
  exit 0
fi

generate_cert_if_needed
export_device_config
write_env_file
connect_cluster

if [[ "${CONNECT_ONLY}" == "false" ]]; then
  ensure_intercept
fi

cat <<EOF
Ready for local debugging.
  Namespace:       ${NAMESPACE}
  Deployment:      ${DEPLOYMENT}
  Intercept:       ${LOCAL_PORT}:${REMOTE_PORT}
  Env file:        ${ENV_FILE}
  Local cert file: ${CERT_DIR}/tls.crt
  Local key file:  ${CERT_DIR}/tls.key

Next step:
  1) Start "HAMi Scheduler Extender" in your IDE
  2) Verify local logs receive /filter and /bind requests
EOF
