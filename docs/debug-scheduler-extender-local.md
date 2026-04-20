# 本地调试 HAMi Scheduler Extender

本文说明如何使用仓库根目录下的脚本 [`hack/debug-scheduler-extender-local.sh`](../hack/debug-scheduler-extender-local.sh)，在本地运行 Scheduler Extender，并通过 [Telepresence](https://www.telepresence.io/) 将集群内调度器对该服务的流量拦截到本机，便于断点调试或查看请求日志。

## 适用场景

- 已在 Kubernetes 集群中安装 HAMi Chart（含 scheduler deployment 与 device ConfigMap）。
- 希望在本地进程（例如 VS Code / GoLand 调试）中运行 extender 的 HTTPS 服务，接收来自集群内 `kube-scheduler` 容器的 `/filter`、`/bind` 等请求。

## 前置条件

| 依赖 | 说明 |
|------|------|
| `kubectl` | 已配置上下文，能访问目标集群。 |
| `telepresence` | v2 客户端；脚本会执行 `telepresence connect` 与 `telepresence intercept`。 |
| `openssl` | 用于在本地生成自签名 TLS 证书（仅当证书目录中尚无 `tls.crt` / `tls.key` 时）。 |
| 集群中的 HAMi | 存在带标签 `app.kubernetes.io/component=hami-scheduler` 的 Deployment；且存在名为 `{deploymentName}-device` 的 ConfigMap（与 Chart 默认命名一致）。 |

安装 Telepresence 请参考官方文档：[Install Telepresence](https://www.telepresence.io/docs/latest/install/)。

## 工作原理（简要）

1. 从集群导出 device 插件相关配置到仓库下的 `.tmp/device-config.yaml`。
2. 在 `.tmp/scheduler-debug-certs/` 生成本地 TLS 证书（或使用已有证书）。
3. 写入 `.tmp/scheduler-extender-debug.env`，供本地 extender 进程读取（绑定地址、证书路径、设备配置文件路径等）。
4. `telepresence connect` 连到集群，并对 scheduler Deployment 做 `telepresence intercept`，将 Pod 内 **远端端口**（默认 `443`）映射到本机 **本地端口**（默认 `8443`）。
5. 你在本机启动 extender（监听 `0.0.0.0:8443` 等），流量即可进入本地进程。

**注意**：集群内 `kube-scheduler` 通过 extender 的 `urlPrefix` 访问 extender。默认 Chart 使用集群内 Service（如 `https://127.0.0.1:443`）。要在拦截后让调度器访问本机 extender，需要让 `urlPrefix` 指向本机 HTTPS 地址（见下一节）。

## 集群与 Helm 配置

拦截后，extender 容器内的 `443` 被转发到你本机的 `8443`。调度器进程在 **同一 Pod 的网络命名空间** 内访问 extender，通常应使用 **带端口的 HTTPS URL**，例如：

```yaml
scheduler:
  extender:
    urlPrefixOverride: "https://127.0.0.1:8443"
```

在 `charts/hami/values.yaml` 中设置上述字段并升级 Release 后，`kube-scheduler` 的调度配置（ConfigMap 中的 `config.yaml` / `config.json`）会使用该 `urlPrefix`，并在 URL 为 `https://` 时启用对应的 TLS 配置。

若未设置 `urlPrefixOverride`，调度器仍会访问集群内默认地址，**不会**自动走到本机调试端口。

其他说明：

- `HAMI_SCHEDULER_NAME` 等环境变量由脚本写入 env 文件；若你修改了 Chart 中的 `schedulerName`，请同时调整本地启动参数或 env 文件，与集群一致。
- 本地证书为自签名；调度侧 Chart 已对 extender 使用 `tlsConfig.insecure: true` 一类配置时，才易与自签证书配合（以当前 Chart 行为为准）。

## 使用方法

在仓库根目录执行（脚本内 `ROOT_DIR` 为仓库根路径）：

```bash
./hack/debug-scheduler-extender-local.sh
```

默认会：

1. 自动查找全局唯一一个 `app.kubernetes.io/component=hami-scheduler` 的 Deployment（若不止一个，必须手动指定命名空间与名称）。
2. 生成本地证书与 env、导出 device ConfigMap、`telepresence connect`、并创建 intercept（默认 `8443:443`）。

### 命令行参数

| 参数 | 说明 |
|------|------|
| `-n`, `--namespace <ns>` | Scheduler Deployment 所在命名空间；省略时在可唯一自动发现的前提下由脚本解析。 |
| `-d`, `--deployment <name>` | Scheduler Deployment 名称；同上。 |
| `--local-port <port>` | 本机监听端口，默认 `8443`。需与 `urlPrefixOverride` 中的端口及本地 extender 监听一致。 |
| `--remote-port <port>` | Pod 内被拦截的端口，默认 `443`（与 extender 容器暴露端口一致）。 |
| `--cert-dir <path>` | 存放 `tls.crt` / `tls.key` 的目录；默认 `<repo>/.tmp/scheduler-debug-certs`。 |
| `--connect-only` | 只准备证书、导出配置、写 env、`telepresence connect`，**不**执行 `telepresence intercept`。 |
| `--leave` | 仅执行 `telepresence leave <deployment>` 后退出，用于结束调试。 |
| `-h`, `--help` | 打印帮助。 |

示例：指定命名空间与 Deployment，并改用本机 `9443`：

```bash
./hack/debug-scheduler-extender-local.sh \
  -n kube-system \
  -d my-release-hami-scheduler \
  --local-port 9443
```

此时请将 Helm 中 `urlPrefixOverride` 设为 `https://127.0.0.1:9443`，并在本地让 extender 监听 `9443`。

### 脚本生成的文件（均在仓库下 `.tmp/`，仓库 `.gitignore` 已忽略该目录）

| 路径 | 说明 |
|------|------|
| `.tmp/scheduler-extender-debug.env` | 本地运行 extender 时可 `source` 或按 IDE 配置加载的环境变量。 |
| `.tmp/device-config.yaml` | 从 ConfigMap `{deployment}-device` 的 `device-config.yaml` 键导出。 |
| `.tmp/scheduler-debug-certs/tls.crt`、`tls.key` | 本地 HTTPS 证书；已存在则复用，不覆盖。 |

## 本地启动 Extender

1. 执行脚本直至打印 `Ready for local debugging.`。
2. 在 IDE 中加载 `.tmp/scheduler-extender-debug.env`（或使用同等环境变量）启动 extender 进程，使其监听脚本中的 `HAMI_HTTP_BIND`（默认 `0.0.0.0:8443`）。
3. 在集群中触发调度（创建/调整使用 HAMi 调度器的 Pod），观察本机是否收到 `/filter`、`/bind` 等请求。

脚本末尾提示的 VS Code `launch.json` 任务名称可能因项目而异，以你仓库中实际配置为准。

## 结束调试

```bash
./hack/debug-scheduler-extender-local.sh --leave -n <ns> -d <deployment>
```

若未传 `-n`/`-d`，脚本仍会尝试自动解析目标（与正常运行时相同规则）。结束后可再执行 `telepresence quit`（脚本在每次 `connect` 前会尝试 `telepresence quit`）。

## 常见问题

**1. 报错：无法自动发现 Deployment**

集群中不存在带 `app.kubernetes.io/component=hami-scheduler` 的 Deployment，或存在多个。请使用 `-n`、`-d` 明确指定。

**2. 导出 device 配置失败**

确认 ConfigMap 名为 `{Deployment 名称}-device`，且其中包含键 `device-config.yaml`。若你修改过 Chart 的命名规则，需自行对齐或改脚本中的 `cm_name` 逻辑。

**3. 调度请求仍不到本机**

检查是否已设置 `scheduler.extender.urlPrefixOverride` 为 `https://127.0.0.1:<local-port>`，且与 `--local-port`、本地进程监听端口一致；并确认 Helm 已升级、`kube-scheduler` 容器已加载新 ConfigMap。

**4. Telepresence 权限或流量问题**

确认当前用户对目标命名空间有权限，且 Telepresence 版本与集群侧 traffic-manager 兼容；详见 Telepresence 官方故障排查文档。

---

更多实现细节见脚本源码：[`hack/debug-scheduler-extender-local.sh`](../hack/debug-scheduler-extender-local.sh)。
