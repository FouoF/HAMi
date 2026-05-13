## Introduction

HAMi now supports Enflame **DRS** hard partition scheduling, aligned with Enflame native scheduler behavior.

DRS is a hard-slice mode similar to NVIDIA MIG and Ascend VNPU templates.

## Prerequisites

* Enflame `gcushare-device-plugin` with DRS support
* driver version >= 1.2.3.14
* kubernetes >= 1.24
* enflame-container-toolkit >= 2.0.50

## Enable Enflame DRS scheduling

* Deploy `gcushare-device-plugin` on Enflame nodes.
* Enable Enflame support in HAMi:

```bash
helm install hami hami-charts/hami --set devices.enflame.enabled=true -n kube-system
```

Default DRS resource:

* `enflame.com/drs-gcu`

## Run DRS workloads

Request a DRS profile by capacity units (for example `1`, `3`, `6`):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gcushare-pod-drs
  namespace: kube-system
spec:
  terminationGracePeriodSeconds: 0
  containers:
    - name: pod-gcu-example1
      image: ubuntu:18.04
      imagePullPolicy: IfNotPresent
      command: ["sleep"]
      args: ["100000"]
      resources:
        limits:
          enflame.com/drs-gcu: 3
        requests:
          enflame.com/drs-gcu: 3
```

During scheduling HAMi writes DRS-compatible annotations such as:

* `assigned-containers`
* `enflame.com/gcu-assigned`
* `enflame.com/gcu-assigned-index`
* `enflame.com/gcu-assigned-minor`
* `enflame.com/gcu-request-size`

These annotations are then consumed by Enflame device-plugin allocate flow.
