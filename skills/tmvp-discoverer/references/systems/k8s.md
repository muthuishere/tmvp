# k8s

Kubernetes clusters: workloads, logs, events, top (EKS, GKE, AKS, k3s, …). Fields and verbs: `tmvp llms types/k8s` · `tmvp verbs k8s`.

## Find it in the codebase
- Manifests: `k8s/`, `deploy/`, `manifests/`, `charts/*/values*.yaml`, `helmfile.yaml`, `kustomization.yaml`, `skaffold.yaml`, ArgoCD `Application`.
- Names: `metadata.namespace`, `Deployment`/`StatefulSet`/`CronJob` names, `kubectl` / `helm upgrade` steps in CI with `--context`/`-n`.
- Infra: terraform `aws_eks_cluster`, `google_container_cluster`, `azurerm_kubernetes_cluster`; secret NAMES like `KUBECONFIG`, `KUBE_CONFIG_DATA`.

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which kube context per env (as in the owner's kubeconfig), or is the kubeconfig stored in sec?
- Which namespace(s) belong to this product?

## Config
```yaml
envs:
  prod:
    destructive: false
    secrets:
      kubeconfig: "$file(sec get acme@production:KUBECONFIG --out ${out})"   # omit → the user's ~/.kube/config
    systems:
      cluster:
        type: k8s
        kubeconfig: "${file:secrets.kubeconfig}"
        context: REPLACE_ME
        namespace: acme
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod cluster health` · `tmvp --skill <dir> prod cluster get deploy`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/cluster.md`: namespaces, deployments/statefulsets/cronjobs per env with what each runs (from manifests), the label selectors to use for `logs`, and env differences (replicas, which jobs only run in prod).
