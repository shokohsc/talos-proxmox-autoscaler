#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="${1:-default}"
NODE_COUNT=0
TARGET_NODES=5

echo "=== Talos Autoscaler Load Test ==="
echo "Namespace: $NAMESPACE"
echo "Target nodes: $TARGET_NODES"
echo ""

get_node_count() {
  kubectl get nodes --no-headers 2>/dev/null | grep -v "control-plane" | wc -l
}

get_pending_pods() {
  kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Pending --no-headers 2>/dev/null | wc -l
}

echo "Current worker nodes:"
get_node_count
echo ""

echo "Deploying 20 pods to trigger scale-up..."
kubectl apply -n "$NAMESPACE" -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: load-test
  namespace: $NAMESPACE
spec:
  replicas: 20
  selector:
    matchLabels:
      app: load-test
  template:
    metadata:
      labels:
        app: load-test
    spec:
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          resources:
            requests:
              cpu: "1"
              memory: "1Gi"
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions:
                  - key: node-role.kubernetes.io/worker
                    operator: Exists
EOF

echo ""
echo "Waiting for pods to be created..."
sleep 5

echo ""
echo "Monitoring scaling (Ctrl+C to stop)..."
echo "$(date): $(get_node_count) nodes, $(get_pending_pods) pending pods"

for i in $(seq 1 60); do
  sleep 10
  NODE_COUNT=$(get_node_count)
  PENDING=$(get_pending_pods)
  echo "$(date): ${NODE_COUNT} nodes, ${PENDING} pending pods"

  if [ "$NODE_COUNT" -ge "$TARGET_NODES" ] && [ "$PENDING" -eq 0 ]; then
    echo ""
    echo "Target reached: $NODE_COUNT nodes, 0 pending pods."
    break
  fi
done

echo ""
echo "Final state:"
kubectl get nodes -o wide
echo ""
echo "Cleaning up test deployment..."
kubectl delete deployment load-test -n "$NAMESPACE" --ignore-not-found

echo ""
echo "Done. Pods will be evicted as nodes scale down."
echo "Monitor with: kubectl get machinedeployments -n talos-system -w"
