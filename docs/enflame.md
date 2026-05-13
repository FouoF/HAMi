任务：参考燧原原生调度器实现在HAMi上支持燧原新的DRS切分方案，并替换旧方案

DRS: 一种硬切分方式，类似NVIDIA-mig和ascend

燧原原生调度器实现方式：
从“用户创建 Pod”到“绑定 DRS instance”的调用链
用户创建 Pod(schedulerName: gcushare-scheduler，并请求 enflame.com/drs-gcu)。
kube-scheduler 调用 gcushare-scheduler-plugin --> 
Filter / filterNodeForDRS：读取 Node 注解里的 DRS capacity + profiles，检查是否有匹配 profile。
Reserve：选定设备 minor，并在 patchAssignedResult 给 Pod 添加 annotaion --> 
分配设备 minor/index --> 
assigned-containers(每个容器的 request/profileName/profileID/allocated=false) --> 
Pod 绑定到节点后，kubelet 为容器拉起前调用 device-plugin 的 Allocate。

gcushare-device-plugin 在 Allocate -> getAssumePod -> PodExistContainerCanBind：
找到该容器在 assigned-containers 的记录 --> 取出其中 ProfileName/ProfileID --> 调用 drs.CreateDRSInstance(index, profileName, profileID) 创建实例--> 将 instanceID/instanceUUID 回写到 assigned-containers --> Allocate 返回容器环境变量：
ENFLAME_VISIBLE_DEVICES=<minor>
TOPS_VISIBLE_DEVICES=DRS-<instanceUUID>(DRS 场景)

参考资源：
apiVersion: v1
kind: Pod
metadata:
  annotations:
    assigned-containers: '{"pod-gcu-example1":{"allocated":true,"request":3,"profileID":"1","profileName":"3g.20gb","instanceID":"2","instanceUUID":"7325501d-02bd-4da2-9289-24d8521736b9"}}'
    enflame.com/gcu-assigned: "true"
    enflame.com/gcu-assigned-index: "0"
    enflame.com/gcu-assigned-minor: "0"
    enflame.com/gcu-assigned-time: "1778573037904666832"
    enflame.com/gcu-request-size: "3"
  generation: 1
  name: gcushare-pod
  namespace: default
  resourceVersion: "1440488"
  uid: 0c2896ab-d506-45ed-99b2-b5e517bf90fe
spec:
  containers:
  - args:
    - "100000"
    command:
    - sleep
    image: docker.m.daocloud.io/library/ubuntu:18.04
    imagePullPolicy: IfNotPresent
    name: pod-gcu-example1
    resources:
      limits:
        enflame.com/drs-gcu: "3"
      requests:
        enflame.com/drs-gcu: "3"

apiVersion: v1
kind: Node
metadata:
  annotations:
    enflame.com/gcu-drs-capacity: '{"devices":[{"index":"0","minor":"0","capacity":6}],"profiles":{"1g.6gb":"0","3g.20gb":"1","6g.40gb":"2"}}'
    enflame.com/gcu-shared-capacity: '{"0":6}'
    nfd.node.kubernetes.io/feature-labels: cpu-cpuid.ADX,cpu-cpuid.AESNI,cpu-cpuid.AVX,cpu-cpuid.AVX2,cpu-cpuid.CMPXCHG8,cpu-cpuid.FLUSH_L1D,cpu-cpuid.FMA3,cpu-cpuid.FXSR,cpu-cpuid.FXSROPT,cpu-cpuid.HLE,cpu-cpuid.IA32_ARCH_CAP,cpu-cpuid.IBPB,cpu-cpuid.LAHF,cpu-cpuid.MD_CLEAR,cpu-cpuid.MOVBE,cpu-cpuid.MPX,cpu-cpuid.OSXSAVE,cpu-cpuid.RTM,cpu-cpuid.RTM_ALWAYS_ABORT,cpu-cpuid.SPEC_CTRL_SSBD,cpu-cpuid.SRBDS_CTRL,cpu-cpuid.STIBP,cpu-cpuid.SYSCALL,cpu-cpuid.SYSEE,cpu-cpuid.VMX,cpu-cpuid.X87,cpu-cpuid.XGETBV1,cpu-cpuid.XSAVE,cpu-cpuid.XSAVEC,cpu-cpuid.XSAVEOPT,cpu-cpuid.XSAVES,cpu-cstate.enabled,cpu-hardware_multithreading,cpu-model.family,cpu-model.id,cpu-model.vendor_id,cpu-pstate.scaling_governor,cpu-pstate.status,cpu-pstate.turbo,enflame.com/gcu.count,enflame.com/gcu.driverVer,enflame.com/gcu.family,enflame.com/gcu.machine,enflame.com/gcu.memory,enflame.com/gcu.model,enflame.com/gcu.product,enflame.com/gfd.latestLabeledTimestamp,enflame.com/gfd.timestamp,kernel-config.NO_HZ,kernel-config.NO_HZ_IDLE,kernel-version.full,kernel-version.major,kernel-version.minor,kernel-version.revision,pci-0300_8086.present,pci-1200_1e36.present,pci-1200_1e36.sriov.capable,storage-nonrotationaldisk,system-os_release.ID,system-os_release.VERSION_ID,system-os_release.VERSION_ID.major,system-os_release.VERSION_ID.minor
    node.alpha.kubernetes.io/ttl: "0"
    volumes.kubernetes.io/controller-managed-attach-detach: "true"
  creationTimestamp: "2026-05-06T06:56:17Z"
  labels:
    beta.kubernetes.io/arch: amd64
    beta.kubernetes.io/os: linux
    enflame.com/gcu.count: "1"
    enflame.com/gcu.driverVer: 1.8.7
    enflame.com/gcu.family: SCORPIO
    enflame.com/gcu.machine: System-Product-Name
    enflame.com/gcu.memory: "41984"
    enflame.com/gcu.model: S60
    enflame.com/gcu.present: "true"
    enflame.com/gcu.product: scorpio
    enflame.com/gcushare: "true"
    enflame.com/gfd.latestLabeledTimestamp: 2026-05-13-06-24-14
    enflame.com/gfd.timestamp: 2026-05-11-08-48-02
spec: {}
status:
  addresses:
  - address: 10.12.215.196
    type: InternalIP
  - address: sse-jq-111-65
    type: Hostname
  allocatable:
    cpu: "8"
    enflame-tech.com/gcu: "0"
    enflame.com/drs-gcu: "6"
    enflame.com/gcu: "0"
    enflame.com/gcu-count: "1"
    enflame.com/shared-gcu: "6"
    ephemeral-storage: "220887878496"
    hugepages-1Gi: "0"
    hugepages-2Mi: "0"
    memory: 32595844Ki
    pods: "110"
  capacity:
    cpu: "8"
    enflame-tech.com/gcu: "0"
    enflame.com/drs-gcu: "6"
    enflame.com/gcu: "0"
    enflame.com/gcu-count: "1"
    enflame.com/shared-gcu: "6"
    ephemeral-storage: 239678688Ki
    hugepages-1Gi: "0"
    hugepages-2Mi: "0"
    memory: 32698244Ki
    pods: "110"