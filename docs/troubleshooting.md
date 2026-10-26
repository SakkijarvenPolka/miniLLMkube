# 状态与排障

## 先看 status

```sh
kubectl get models
kubectl describe model <name>
```

`status.phase` 的四种取值：

| phase | 含义 |
|---|---|
| `Pending` | Deployment 已创建，但状态还没被观测到 |
| `Deploying` | 滚动更新进行中，副本尚未全部可用 |
| `Ready` | 当前 generation 的副本全部可用 |
| `Failed` | 规格非法、Deployment 报失败，或 Pod 卡在某个具体错误上 |

`Ready` 条件的 `reason` 会给出更具体的原因。`describe` 只显示最近的状态，历史事件看 `kubectl get events`。

## Ready 条件的原因

按出现位置分三类。

### 1. 规格与协调错误

| reason | 含义 | 处理 |
|---|---|---|
| `InvalidSpec` | `source`、`runtime`、`format`、GPU 约束等校验失败，message 里是具体字段问题 | 按 message 改 spec |
| `DeploymentReconcileFailed` | 创建/更新 Deployment 出错（通常是 RBAC 或 API 冲突） | 看 controller 日志 |
| `ServiceReconcileFailed` | 创建/更新 Service 出错 | 看 controller 日志 |

### 2. Deployment 层面

| reason | 含义 |
|---|---|
| `DeploymentReady` | 正常完成 |
| `DeploymentPending` | 等待 Deployment 状态被观测 |
| `DeploymentProgressing` | 等待副本变 Ready |
| `ProgressDeadlineExceeded` | 超过 `progressDeadlineSeconds`（30 分钟）仍未完成，通常是镜像拉不动或模型下载太慢 |
| `ReplicaFailure` | Deployment 报告副本创建失败 |

### 3. Pod 层面（最有用的部分）

只要当前 ReplicaSet 的 Pod 有明确失败信号，controller 就会用 Pod 的原因覆盖 Deployment 的泛化状态。优先级从高到低：

| reason | 触发条件 | 常见原因 |
|---|---|---|
| `FormatMismatch` | 下载器 termination message 以 `FORMAT_MISMATCH:` 开头 | 扩展名是 `.gguf` 但内容不是 GGUF，或 safetensors 仓库缺 config/分片/tokenizer |
| `DownloadFailed` | 下载器 initContainer 非零退出 | URL 404、鉴权、断网、磁盘满；message 是脚本写出的错误 |
| `ImagePullFailed` | 任一容器 `ErrImagePull` / `ImagePullBackOff` / `InvalidImageName` | 镜像名写错、私有仓库缺 imagePullSecret、节点拉取失败 |
| `InsufficientGPU` | Pod 调度失败且 message 含 `nvidia.com/gpu` | 节点没有可分配 GPU，或 device plugin 没装 |
| `Unschedulable` | Pod 因其他原因无法调度 | 资源不足、nodeSelector 不匹配、污点未容忍 |
| `RuntimeCrashing` | 推理容器 `CrashLoopBackOff`，message 带上次退出码 | 显存不足、参数不支持、缺 CUDA 库、`/dev/shm` 太小 |

优先级的意义：下载失败时不会报成 `ProgressDeadlineExceeded`，也不会被后面的 `Unschedulable` 覆盖。message 截断到 512 字节，避免把整段 termination message 塞进 status。

诊断只针对当前 ReplicaSet 的 Pod。旧版本残留的失败 Pod 不会污染新版本的状态。

## 事件

当 `phase` 进入 `Failed` 且原因发生变化时，controller 会发一条 Warning 事件，reason 与上表一致，方便 `kubectl get events --field-selector reason=DownloadFailed` 这类筛选。原因不变不会重复刷事件。

## GPUOffload 条件

请求 GPU 的 `Model` 会多一个 `GPUOffload` 条件，用来回答「GPU 真的用上了吗」：

| status / reason | 含义 |
|---|---|
| `True` / `OffloadDetected` | 推理日志里出现了 `llama_prepare_model_devices: using device CUDA0`，message 引用整行 |
| `False` / `CPUFallback` | 日志显示 `using device CPU`：资源请求成功，但 llama.cpp 没用上显卡 |
| `Unknown` / `NotDetected` | 还没 Ready、日志里没有该行、读日志失败，或后端不是 llama.cpp |

只有 llama.cpp 会做这个检查；vLLM 的 GPU 保证来自「必须请求 GPU」的校验。CPU 后退通常意味着镜像不是 `server-cuda`、`--n-gpu-layers` 被覆盖成 0，或节点上 CUDA 初始化失败。

## 常见场景

**Pod 一直 Pending，describe 显示 GPU 不足**

```sh
kubectl describe node <node> | grep nvidia.com/gpu
kubectl get pods -A -o wide | grep -i gpu-operator
```

确认 device plugin 正常、卡没有被别的 Pod 占满。

**下载很慢或反复重试**

下载器有 5 次重试、低速阈值 1024 B/s × 120 秒。慢网络下先确认 `curl -I <source>` 能通；已有 `.tmp` 时会续传，不会从头再来。想看进度：

```sh
kubectl logs <pod> -c model-downloader -f
```

**换了 source，但 Pod 用的还是旧模型**

缓存键是完整 source 字符串的 sha256，改 source 就是新目录。检查 `status.cacheKey` 是否已更新；如果没变，说明 spec 没被真正改到。

**滚动更新卡住**

单卡 GPU 的 `Model` 用 `Recreate` 策略，因为新 Pod 抢不到旧 Pod 占着的显卡。CPU 模型用 `RollingUpdate`（25% / 25%）。如果 GPU 模型卡在旧 Pod 不退出，先看旧 Pod 是否还在处理长请求。

**vLLM 启动即崩**

先看 `/dev/shm` 是否够大（默认是内存 limit 的一半），再看日志里是否有未知的 `VLLM_*` 环境变量——operator 已经关掉 service links，但自定义的 env 仍可能触发校验失败。

## 拿不到结论时

```sh
kubectl logs <pod> -c llama-server        # 或 vllm
kubectl logs <pod> -c model-downloader
kubectl describe pod <pod>
kubectl get deployment <name> -o yaml | sed -n '/status:/,$p'
```

controller 自己的日志里，`Recorded failure event` 会同时打出 name 和 reason。