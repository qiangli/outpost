#!/usr/bin/env bash
#
# Verify that Ready nodes participate in one Kubernetes network rather than
# acting as isolated, overlapping single-node networks.

set -uo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/cluster-crossnode-check.sh [OPTIONS]

Run DNS, cross-node Service, cross-node pod-IP, API Service, and duplicate-IP
checks against a Kubernetes cluster. A temporary namespace is created by
default and removed on exit.

Options:
  --kubeconfig PATH   Kubeconfig to use (default: $KUBECONFIG, then
                      ~/.kube/config)
  --namespace NAME    Existing namespace for temporary check resources
                      (default: create a temporary namespace)
  -h, --help          Show this help

The script requires bash and kubectl. It creates only labeled probe Pods and
Services in the selected namespace (plus the namespace when no --namespace is
given), and removes everything it creates even when a check fails.
EOF
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 2
}

kubeconfig=""
namespace=""
namespace_owned=false

while [ "$#" -gt 0 ]; do
  case "$1" in
    --kubeconfig)
      [ "$#" -ge 2 ] || die "--kubeconfig requires a path"
      kubeconfig=$2
      shift 2
      ;;
    --namespace)
      [ "$#" -ge 2 ] || die "--namespace requires a name"
      namespace=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1 (try --help)"
      ;;
  esac
done

command -v kubectl >/dev/null 2>&1 || die "kubectl is required but was not found"

if [ -z "$kubeconfig" ]; then
  if [ -n "${KUBECONFIG:-}" ]; then
    kubeconfig=$KUBECONFIG
  else
    kubeconfig=${HOME:?HOME is not set}/.kube/config
  fi
fi

[ -r "$kubeconfig" ] || die "kubeconfig is not readable: $kubeconfig"

KUBECTL=(kubectl --kubeconfig "$kubeconfig" --request-timeout=10s)
name_suffix="$$-${RANDOM:-0}"
run_id="crossnode-$name_suffix"
resource_label="outpost.dhnt.io/crossnode-run=$run_id"
cleanup_started=false

cleanup() {
  cleanup_status=$?
  if [ "$cleanup_started" = true ]; then
    return
  fi
  cleanup_started=true
  trap - EXIT HUP INT TERM

  if [ -n "$namespace" ]; then
    if [ "$namespace_owned" = true ]; then
      "${KUBECTL[@]}" delete namespace "$namespace" \
        --ignore-not-found --wait=false >/dev/null 2>&1 || true
    else
      "${KUBECTL[@]}" -n "$namespace" delete pod,service \
        -l "$resource_label" --ignore-not-found --wait=false \
        >/dev/null 2>&1 || true
    fi
  fi
  exit "$cleanup_status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

printf 'Checking API connectivity (10s timeout)...\n'
node_listing=$("${KUBECTL[@]}" get nodes \
  -o 'jsonpath={range .items[*]}{.metadata.name}{"|"}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"|"}{.metadata.labels.kubernetes\.io/hostname}{"\n"}{end}' \
  2>&1) || die "cannot list cluster nodes: $node_listing"

ready_nodes=()
ready_selectors=()
not_ready_nodes=()
while IFS='|' read -r node ready_status hostname_selector; do
  [ -n "$node" ] || continue
  if [ "$ready_status" = "True" ]; then
    if [ -z "$hostname_selector" ]; then
      die "Ready node $node has no kubernetes.io/hostname label"
    fi
    ready_nodes+=("$node")
    ready_selectors+=("$hostname_selector")
  else
    not_ready_nodes+=("$node")
  fi
done <<< "$node_listing"

[ "${#ready_nodes[@]}" -gt 0 ] || die "the cluster has no Ready nodes"

result_nodes=()
result_checks=()
result_statuses=()
result_details=()
failures=0

add_result() {
  result_nodes+=("$1")
  result_checks+=("$2")
  result_statuses+=("$3")
  result_details+=("$4")
  if [ "$3" = "FAIL" ]; then
    failures=$((failures + 1))
  fi
}

for node in "${not_ready_nodes[@]}"; do
  printf 'Skipping NotReady node: %s\n' "$node"
  add_result "$node" "NODE" "SKIP" "NotReady"
done

printf 'Ready nodes: %s; skipped NotReady nodes: %s\n' \
  "${#ready_nodes[@]}" "${#not_ready_nodes[@]}"

check_duplicate_pod_ips() {
  local listing ip ref i found duplicate_details duplicate_count
  local -a seen_ips seen_refs

  listing=$("${KUBECTL[@]}" get pods --all-namespaces \
    -o 'jsonpath={range .items[?(@.status.podIP)]}{.status.podIP}{"\t"}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}') || {
      add_result "cluster" "DUPLICATE_POD_IPS" "FAIL" "kubectl get pods failed"
      return
    }

  seen_ips=()
  seen_refs=()
  found=false
  duplicate_details=""
  duplicate_count=0
  while IFS=$'\t' read -r ip ref; do
    [ -n "$ip" ] || continue
    for ((i = 0; i < ${#seen_ips[@]}; i++)); do
      if [ "${seen_ips[$i]}" = "$ip" ]; then
        found=true
        duplicate_count=$((duplicate_count + 1))
        if [ "$duplicate_count" -le 5 ]; then
          duplicate_details="${duplicate_details}${duplicate_details:+; }$ip (${seen_refs[$i]}, $ref)"
        fi
        break
      fi
    done
    seen_ips+=("$ip")
    seen_refs+=("$ref")
  done <<< "$listing"

  if [ "$found" = true ]; then
    if [ "$duplicate_count" -gt 5 ]; then
      duplicate_details="$duplicate_details; +$((duplicate_count - 5)) more"
    fi
    add_result "cluster" "DUPLICATE_POD_IPS" "FAIL" "$duplicate_details"
  else
    add_result "cluster" "DUPLICATE_POD_IPS" "PASS" "all assigned pod IPs are unique"
  fi
}

check_duplicate_endpoint_ips() {
  local listing object addresses address i j found duplicate_details
  local duplicate_count
  local -a address_list

  listing=$("${KUBECTL[@]}" get endpoints --all-namespaces \
    -o 'jsonpath={range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"\t"}{range .subsets[*]}{range .addresses[*]}{.ip}{" "}{end}{range .notReadyAddresses[*]}{.ip}{" "}{end}{end}{"\n"}{end}') || {
      add_result "cluster" "DUPLICATE_ENDPOINT_IPS" "FAIL" "kubectl get endpoints failed"
      return
    }

  found=false
  duplicate_details=""
  duplicate_count=0
  while IFS=$'\t' read -r object addresses; do
    [ -n "$object" ] || continue
    read -r -a address_list <<< "$addresses"
    for ((i = 0; i < ${#address_list[@]}; i++)); do
      address=${address_list[$i]}
      for ((j = 0; j < i; j++)); do
        if [ "${address_list[$j]}" = "$address" ]; then
          found=true
          duplicate_count=$((duplicate_count + 1))
          if [ "$duplicate_count" -le 5 ]; then
            duplicate_details="${duplicate_details}${duplicate_details:+; }$object ($address)"
          fi
          break
        fi
      done
    done
  done <<< "$listing"

  if [ "$found" = true ]; then
    if [ "$duplicate_count" -gt 5 ]; then
      duplicate_details="$duplicate_details; +$((duplicate_count - 5)) more"
    fi
    add_result "cluster" "DUPLICATE_ENDPOINT_IPS" "FAIL" "$duplicate_details"
  else
    add_result "cluster" "DUPLICATE_ENDPOINT_IPS" "PASS" "no Endpoints object repeats an address"
  fi
}

check_duplicate_pod_ips
check_duplicate_endpoint_ips

if [ -z "$namespace" ]; then
  namespace="outpost-crossnode-$$-${RANDOM:-0}"
  namespace_owned=true
  namespace_output=$("${KUBECTL[@]}" create namespace "$namespace" 2>&1) ||
    die "cannot create temporary namespace $namespace: $namespace_output"
  "${KUBECTL[@]}" label namespace "$namespace" "$resource_label" \
    --overwrite >/dev/null 2>&1 ||
    die "cannot label temporary namespace $namespace"
  printf 'Using temporary namespace: %s\n' "$namespace"
else
  namespace_output=$("${KUBECTL[@]}" get namespace "$namespace" -o name 2>&1) ||
    die "namespace does not exist or is not readable: $namespace ($namespace_output)"
  printf 'Using existing namespace: %s\n' "$namespace"
fi

api_service_ip=$("${KUBECTL[@]}" -n default get service kubernetes \
  -o 'jsonpath={.spec.clusterIP}' 2>&1) ||
  die "cannot discover kubernetes.default Service IP: $api_service_ip"
[ -n "$api_service_ip" ] && [ "$api_service_ip" != "None" ] ||
  die "kubernetes.default has no ClusterIP"

target_services=()
target_ips=()
target_tokens=()
target_ready=()

create_target() {
  local index node hostname_selector pod service token output
  local attempt endpoint_ips
  index=$1
  node=$2
  hostname_selector=$3
  pod=$(printf 'crossnode-target-%03d-%s' "$index" "$name_suffix")
  service=$(printf 'crossnode-target-%03d-%s' "$index" "$name_suffix")
  token="$run_id-$index"

  output=$("${KUBECTL[@]}" -n "$namespace" apply -f - 2>&1 <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  labels:
    outpost.dhnt.io/crossnode-run: $run_id
    outpost.dhnt.io/crossnode-target: "$index"
spec:
  restartPolicy: Never
  activeDeadlineSeconds: 300
  nodeSelector:
    kubernetes.io/hostname: $hostname_selector
  tolerations:
    - operator: Exists
  containers:
    - name: target
      image: curlimages/curl:latest
      command: ["/bin/sh", "-c"]
      args:
        - |
          mkdir -p /tmp/crossnode
          printf '%s\n' '$token' > /tmp/crossnode/index.html
          exec busybox httpd -f -p 8080 -h /tmp/crossnode
---
apiVersion: v1
kind: Service
metadata:
  name: $service
  labels:
    outpost.dhnt.io/crossnode-run: $run_id
spec:
  selector:
    outpost.dhnt.io/crossnode-run: $run_id
    outpost.dhnt.io/crossnode-target: "$index"
  ports:
    - name: http
      port: 8080
      targetPort: 8080
EOF
  ) || {
    printf 'WARN: cannot create target on %s: %s\n' "$node" "$output" >&2
    target_services+=("$service")
    target_ips+=("")
    target_tokens+=("$token")
    target_ready+=("false")
    return
  }

  target_services+=("$service")
  target_tokens+=("$token")
  if ! "${KUBECTL[@]}" -n "$namespace" wait --for=condition=Ready \
    "pod/$pod" --timeout=60s --request-timeout=65s >/dev/null 2>&1; then
    printf 'WARN: target Pod %s on %s did not become Ready\n' "$pod" "$node" >&2
    target_ips+=("")
    target_ready+=("false")
    return
  fi

  output=$("${KUBECTL[@]}" -n "$namespace" get pod "$pod" \
    -o 'jsonpath={.status.podIP}' 2>&1) || output=""
  if [ -z "$output" ]; then
    printf 'WARN: target Pod %s on %s has no pod IP\n' "$pod" "$node" >&2
    target_ips+=("")
    target_ready+=("false")
    return
  fi
  target_ips+=("$output")

  # Do not race the Endpoints controller: the Service check must really use
  # the intended remote Pod before a probe starts.
  for ((attempt = 0; attempt < 30; attempt++)); do
    endpoint_ips=$("${KUBECTL[@]}" -n "$namespace" get endpoints "$service" \
      -o 'jsonpath={range .subsets[*].addresses[*]}{.ip}{" "}{end}' \
      2>/dev/null || true)
    case " $endpoint_ips " in
      *" $output "*)
        target_ready+=("true")
        return
        ;;
    esac
    sleep 1
  done
  printf 'WARN: Service %s did not acquire target endpoint %s\n' \
    "$service" "$output" >&2
  target_ready+=("false")
}

if [ "${#ready_nodes[@]}" -gt 1 ]; then
  for ((node_index = 0; node_index < ${#ready_nodes[@]}; node_index++)); do
    create_target "$node_index" "${ready_nodes[$node_index]}" \
      "${ready_selectors[$node_index]}"
  done
else
  printf 'Only one Ready node; cross-node Service and pod-IP checks will be skipped.\n'
fi

run_probe() {
  local source_index source_node target_index cross_enabled
  local source_selector
  local target_service target_ip target_token pod output phase logs
  local dns_status service_status pod_ip_status api_status
  local dns_detail service_detail pod_ip_detail api_detail
  local attempt check status detail

  source_index=$1
  source_node=${ready_nodes[$source_index]}
  source_selector=${ready_selectors[$source_index]}
  target_index=0
  cross_enabled=false
  target_service=""
  target_ip=""
  target_token=""

  if [ "${#ready_nodes[@]}" -gt 1 ]; then
    target_index=$(((source_index + 1) % ${#ready_nodes[@]}))
    cross_enabled=true
    target_service=${target_services[$target_index]}
    target_ip=${target_ips[$target_index]}
    target_token=${target_tokens[$target_index]}
  fi

  pod=$(printf 'crossnode-probe-%03d-%s' "$source_index" "$name_suffix")
  output=$("${KUBECTL[@]}" -n "$namespace" apply -f - 2>&1 <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  labels:
    outpost.dhnt.io/crossnode-run: $run_id
spec:
  restartPolicy: Never
  activeDeadlineSeconds: 45
  nodeSelector:
    kubernetes.io/hostname: $source_selector
  tolerations:
    - operator: Exists
  containers:
    - name: probe
      image: curlimages/curl:latest
      env:
        - {name: CROSS_ENABLED, value: "$cross_enabled"}
        - {name: TARGET_SERVICE, value: "$target_service"}
        - {name: TARGET_IP, value: "$target_ip"}
        - {name: TARGET_TOKEN, value: "$target_token"}
        - {name: API_SERVICE_IP, value: "$api_service_ip"}
      command: ["/bin/sh", "-c"]
      args:
        - |
          failed=0
          if busybox timeout 8 nslookup kubernetes.default.svc.cluster.local >/dev/null 2>&1; then
            echo 'DNS|PASS|kubernetes.default.svc.cluster.local resolved'
          else
            echo 'DNS|FAIL|cluster DNS lookup failed'
            failed=1
          fi

          if [ "\$CROSS_ENABLED" = true ]; then
            body=\$(curl -fsS --connect-timeout 5 --max-time 8 \
              "http://\$TARGET_SERVICE:8080/" 2>/dev/null)
            if [ "\$body" = "\$TARGET_TOKEN" ]; then
              echo 'CROSS_NODE_SERVICE|PASS|remote Service returned target token'
            else
              echo 'CROSS_NODE_SERVICE|FAIL|remote Service unreachable or misrouted'
              failed=1
            fi

            body=\$(curl -fsS --connect-timeout 5 --max-time 8 \
              "http://\$TARGET_IP:8080/" 2>/dev/null)
            if [ "\$body" = "\$TARGET_TOKEN" ]; then
              echo 'CROSS_NODE_POD_IP|PASS|remote pod IP returned target token'
            else
              echo 'CROSS_NODE_POD_IP|FAIL|remote pod IP unreachable or misrouted'
              failed=1
            fi
          else
            echo 'CROSS_NODE_SERVICE|SKIP|only one Ready node'
            echo 'CROSS_NODE_POD_IP|SKIP|only one Ready node'
          fi

          if curl -ksS --connect-timeout 5 --max-time 8 \
            "https://\$API_SERVICE_IP:443/" >/dev/null 2>&1; then
            echo 'APISERVER_SERVICE|PASS|kubernetes.default:443 reachable'
          else
            echo 'APISERVER_SERVICE|FAIL|kubernetes.default:443 unreachable'
            failed=1
          fi
          exit "\$failed"
EOF
  ) || {
    add_result "$source_node" "DNS" "FAIL" "could not create probe Pod: $output"
    if [ "$cross_enabled" = true ]; then
      add_result "$source_node" "CROSS_NODE_SERVICE" "FAIL" "could not create probe Pod"
      add_result "$source_node" "CROSS_NODE_POD_IP" "FAIL" "could not create probe Pod"
    else
      add_result "$source_node" "CROSS_NODE_SERVICE" "SKIP" "only one Ready node"
      add_result "$source_node" "CROSS_NODE_POD_IP" "SKIP" "only one Ready node"
    fi
    add_result "$source_node" "APISERVER_SERVICE" "FAIL" "could not create probe Pod"
    return
  }

  dns_status=""
  service_status=""
  pod_ip_status=""
  api_status=""
  dns_detail=""
  service_detail=""
  pod_ip_detail=""
  api_detail=""

  for ((attempt = 0; attempt < 65; attempt++)); do
    phase=$("${KUBECTL[@]}" -n "$namespace" get pod "$pod" \
      -o 'jsonpath={.status.phase}' 2>/dev/null || true)
    case "$phase" in
      Succeeded|Failed)
        break
        ;;
    esac
    sleep 1
  done

  logs=$("${KUBECTL[@]}" -n "$namespace" logs "$pod" 2>&1 || true)
  while IFS='|' read -r check status detail; do
    case "$check" in
      DNS)
        dns_status=$status
        dns_detail=$detail
        ;;
      CROSS_NODE_SERVICE)
        service_status=$status
        service_detail=$detail
        ;;
      CROSS_NODE_POD_IP)
        pod_ip_status=$status
        pod_ip_detail=$detail
        ;;
      APISERVER_SERVICE)
        api_status=$status
        api_detail=$detail
        ;;
    esac
  done <<< "$logs"

  printf '\nProbe results from %s:\n' "$source_node"
  if [ -n "$dns_status" ]; then
    printf '  DNS|%s|%s\n' "$dns_status" "$dns_detail"
    add_result "$source_node" "DNS" "$dns_status" "$dns_detail"
  else
    add_result "$source_node" "DNS" "FAIL" "probe produced no DNS result (phase: ${phase:-unknown})"
  fi
  if [ -n "$service_status" ]; then
    printf '  CROSS_NODE_SERVICE|%s|%s\n' "$service_status" "$service_detail"
    if [ "$cross_enabled" = true ] && [ "${target_ready[$target_index]:-false}" != true ]; then
      service_status=FAIL
      service_detail="remote target on ${ready_nodes[$target_index]} was not Ready"
    fi
    add_result "$source_node" "CROSS_NODE_SERVICE" "$service_status" "$service_detail"
  elif [ "$cross_enabled" = true ]; then
    add_result "$source_node" "CROSS_NODE_SERVICE" "FAIL" "probe produced no Service result"
  else
    add_result "$source_node" "CROSS_NODE_SERVICE" "SKIP" "only one Ready node"
  fi
  if [ -n "$pod_ip_status" ]; then
    printf '  CROSS_NODE_POD_IP|%s|%s\n' "$pod_ip_status" "$pod_ip_detail"
    if [ "$cross_enabled" = true ] && [ "${target_ready[$target_index]:-false}" != true ]; then
      pod_ip_status=FAIL
      pod_ip_detail="remote target on ${ready_nodes[$target_index]} was not Ready"
    fi
    add_result "$source_node" "CROSS_NODE_POD_IP" "$pod_ip_status" "$pod_ip_detail"
  elif [ "$cross_enabled" = true ]; then
    add_result "$source_node" "CROSS_NODE_POD_IP" "FAIL" "probe produced no pod-IP result"
  else
    add_result "$source_node" "CROSS_NODE_POD_IP" "SKIP" "only one Ready node"
  fi
  if [ -n "$api_status" ]; then
    printf '  APISERVER_SERVICE|%s|%s\n' "$api_status" "$api_detail"
    add_result "$source_node" "APISERVER_SERVICE" "$api_status" "$api_detail"
  else
    add_result "$source_node" "APISERVER_SERVICE" "FAIL" "probe produced no API Service result"
  fi
}

for ((node_index = 0; node_index < ${#ready_nodes[@]}; node_index++)); do
  run_probe "$node_index"
done

printf '\n%-28s %-25s %-6s %s\n' "NODE" "CHECK" "STATUS" "DETAIL"
printf '%-28s %-25s %-6s %s\n' "----------------------------" \
  "-------------------------" "------" "------"
for ((result_index = 0; result_index < ${#result_nodes[@]}; result_index++)); do
  printf '%-28s %-25s %-6s %s\n' \
    "${result_nodes[$result_index]}" \
    "${result_checks[$result_index]}" \
    "${result_statuses[$result_index]}" \
    "${result_details[$result_index]}"
done

if [ "$failures" -gt 0 ]; then
  printf '\nFAIL: %d applicable check(s) failed.\n' "$failures"
  exit 1
fi

printf '\nPASS: all applicable checks passed.\n'
exit 0
