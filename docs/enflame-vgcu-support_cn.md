## 简介

HAMi 现已支持燧原 **DRS 硬切分**调度，并对齐燧原原生调度器链路。

DRS 是类似 NVIDIA MIG / Ascend VNPU 的硬切分方案。

## 节点需求

* Enflame gcushare-device-plugin >= 2.1.6
* driver version >= 1.2.3.14
* kubernetes >= 1.24
* enflame-container-toolkit >=2.0.50

## 开启GCU复用

* 部署 `gcushare-device-plugin`，并确保版本支持 DRS。

> **注意:** *只需要安装gcushare-device-plugin，不要安装gcushare-scheduler-plugin.*

* 在安装HAMi时配置参数'devices.enflame.enabled=true'

```
helm install hami hami-charts/hami --set devices.enflame.enabled=true -n kube-system
```

> **说明:** 默认 DRS 资源名称为 `enflame.com/drs-gcu`。

## 运行GCU任务

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
      command:
        - sleep
      args:
        - '100000'
      resources:
        limits:
          enflame.com/drs-gcu: 3
        requests:
          enflame.com/drs-gcu: 3
```
> **注意:** *查看更多的[用例](../examples/enflame/).*

## 注意事项

HAMi 在调度阶段会写入与 DRS 兼容的注解，包括：

* `assigned-containers`
* `enflame.com/gcu-assigned`
* `enflame.com/gcu-assigned-index`
* `enflame.com/gcu-assigned-minor`
* `enflame.com/gcu-request-size`

这些注解将由燧原 device-plugin 在 Allocate 阶段继续处理并完成 DRS instance 绑定。