# K8s Runtime Staging

This directory is the staging root for the production runtime image used by Kubernetes.

## Required Layout

```text
runtime/linux/
  bin/
    gate
    scene
    battle
    zoneinfo/
  generated/
    generated_tables/
```

## Notes

- These must be Linux executables, not Windows `.exe` files.
- `bin/etc/` is not required here because K8s mounts config through `ConfigMap`.
- `bin/logs/` is not required here because K8s mounts a writable `emptyDir`.
- The root `Dockerfile` is not the production K8s runtime image. Use `deploy/k8s/Dockerfile.runtime` instead.
- If your generated table output currently lives under `generated/tables`, stage or copy it into `runtime/linux/generated/generated_tables` for the image build.
