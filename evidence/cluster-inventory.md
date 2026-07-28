# AWS resources created for verification

Account `569190534191` (personal), region `us-west-2`. All tagged
`Project=nma-pne-parity`, `Owner=rrroizma`, `Ephemeral=true`.

| Resource | Identifier | Notes |
|---|---|---|
| EKS cluster | `nma-pne-parity-test` | Kubernetes **1.36**, non-Auto |
| Managed nodegroup | `standard-amd64` | 2 × `t3.large`, AL2023, 40GB gp3 |
| CloudFormation | `eksctl-nma-pne-parity-test-cluster` | VPC, subnets, NAT, IAM, control plane |
| CloudFormation | `eksctl-nma-pne-parity-test-nodegroup-standard-amd64` | nodegroup |
| ECR repository | `eks/eks-node-monitoring-agent` | tag `parity-dev` (chart expects this path) |
| ECR repository | `eks-node-monitoring-agent-parity` | first push, superseded by the above |

In-cluster (no AWS cost): `eks-node-monitoring-agent` DaemonSet (kube-system),
`pne-prometheus-node-exporter` + `prom-prometheus-server` + `grafana` (monitoring).

## Approximate cost

~$0.32/hr (~$8/day): control plane $0.10, 2× t3.large $0.166, NAT gateway ~$0.045.

## Teardown

```bash
ada credentials update --provider isengard --account 569190534191 --role Admin --once

# Deletes the nodegroup, cluster, VPC, NAT and IAM in dependency order.
eksctl delete cluster --name nma-pne-parity-test --region us-west-2 --wait

# ECR repositories (images are billable storage)
aws ecr delete-repository --repository-name eks/eks-node-monitoring-agent \
  --region us-west-2 --force
aws ecr delete-repository --repository-name eks-node-monitoring-agent-parity \
  --region us-west-2 --force
```

Verify nothing is left:

```bash
aws eks list-clusters --region us-west-2
aws cloudformation describe-stacks --region us-west-2 \
  --query 'Stacks[?contains(StackName,`nma-pne-parity`)].StackName'
```

**Not torn down automatically.** The cluster is still running so the dashboards
remain reachable; delete it when you are finished with them.
