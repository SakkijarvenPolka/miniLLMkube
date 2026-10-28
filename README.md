# miniLLMkube

miniLLMkube 是一个用 Kubebuilder 编写的 Kubernetes Operator，用来声明式部署大模型推理服务。

你只需要写一份 `Model` YAML，告诉 Kubernetes 模型在哪里、要几个副本、上下文多长。Operator 负责创建 `Deployment` 和 `Service`、下载并缓存权重、启动 `llama.cpp` 或 vLLM，暴露 OpenAI 兼容接口。

## 它解决什么问题

手动在 Kubernetes 里跑本地大模型通常要做很多事：

1. 准备模型下载地址。
2. 写容器启动参数。
3. 配置探针和资源限制。
4. 创建 Service。
5. 观察 Deployment 是否真正 Ready。
6. 处理模型下载失败、滚动更新、手动改坏资源等情况。

miniLLMkube 把这些动作收进一个自定义资源里。用户只关心模型声明，Operator 负责把集群状态调到正确结果。

## 核心能力

- **双后端、单 CRD**：支持 `llama.cpp`（GGUF）和 vLLM（safetensors）。`spec.runtime` / `spec.format` 默认 `auto`，能从 `source` 推断的不用手写；`vllm + gguf` 这类非法组合在 CEL 层直接拒绝。两个后端都已在单节点 K3s 上跑通。
- **下载与缓存隔离**：模型由 initContainer 下载到 `/models/<cacheKey>/`，每个 `source` 独立目录；`flock` 让多副本只有一个真正下载，`.tmp` 支持断点续传（外层重试 + `Content-Range` 校验，大文件中断不会从 0 重来），GGUF 校验魔数、safetensors 校验配置与全部分片，通过后才算缓存命中。
- **GPU 推理**：`spec.gpu` 请求 `nvidia.com/gpu`、自动切换 CUDA 镜像、追加 `--n-gpu-layers`，并从推理日志确认 GPU 是否真的生效（`GPUOffload` 条件）。
- **状态与诊断**：维护 `Pending` / `Deploying` / `Ready` / `Failed` / `ScaledToZero`；Pod 卡住时给出 `DownloadFailed`、`FormatMismatch`、`ImagePullFailed`、`InsufficientGPU`、`Unschedulable`、`RuntimeCrashing` 中的具体原因，原因变化时发 Warning 事件。
- **服务暴露**：创建 ClusterIP Service，集群内访问地址写入 `status.endpoint`，接口兼容 OpenAI。
- **放置与缓存复用**：`nodeSelector` / `tolerations` / `affinity` / `schedulerName` 原样透传；`MODEL_CACHE_HOST_PATH` 可让开发环境复用宿主机缓存。

## 架构简图

```text
Model YAML
   |
   v
Model Controller（resolveBackend 决定 llama.cpp 或 vLLM）
   |
   +--> Deployment
   |      +--> initContainer：下载到 /models/<cacheKey>/
   |      +--> 推理容器：llama.cpp server 或 vLLM（vLLM 额外挂 /dev/shm）
   |
   +--> Service：暴露 8080
   |
   +--> Model Status：phase / endpoint / resolvedRuntime / conditions
```

## 选哪个后端

| | llama.cpp | vLLM |
|---|---|---|
| 模型格式 | GGUF | safetensors（`hf://`） |
| 默认镜像 | `ghcr.io/ggml-org/llama.cpp:server` / `:server-cuda` | `vllm/vllm-openai:v0.32.0`（CUDA 13，需驱动 ≥ 580） |
| 镜像压缩体积 | 0.31 GB / 2.60 GB | 9.21 GB |
| 适合 | 单机小模型、CPU 也能跑、单流延迟低、启动快 | 高并发、continuous batching、吞吐优先 |
| 实测（2B，单卡 12 GB） | 单流约 140~150 tok/s，启动约 40 秒 | 单流约 104 tok/s、4 并发 316 tok/s，启动约 3 分钟 |

## 实测结果（单节点 K3s）

环境：k3s v1.36.5 单节点，RTX 5070 Ti Laptop（12227 MiB，驱动 591.86，sm_120，带宽 672 GB/s），WSL2，hostPath 缓存 `/var/lib/minillmkube/models`。同一模型家族分别验证两条路径，覆盖 apply → 下载权重 → 启动 → 就绪 → 集群内访问 → 推理全链路。

| 项 | llama.cpp | vLLM |
|---|---|---|
| 模型 | `unsloth/Qwen3.5-2B-GGUF` 的 `Q8_0`（1.92 GB） | `Qwen/Qwen3.5-2B` safetensors（4.55 GB） |
| 上下文 | 32K | 32K（KV cache 274k tokens，32K 并发 8.37x） |
| 显存 | 由 llama.cpp 自行分配 | 约 10.5 GiB（权重+非 torch 4.75、峰值激活 1.9、CUDA graph 0.13、KV 3.49） |
| 启动到 Ready | 约 40 秒 | 2 分 57 秒（引擎初始化含 CUDA graph 捕获） |
| 单流解码 | 约 140~150 tok/s | 约 104 tok/s |
| 首 token | — | 冷 160 ms / 热 23 ms |
| 4 并发聚合 | — | 316 tok/s |
| `GPUOffload` | `True` / `OffloadDetected` | `Unknown` / `NotDetected`（该条件仅对 llama.cpp 生效） |

关于 vLLM 单流只有 ~104 tok/s：单流解码每生成一个 token 要把权重读一遍，`672 GB/s ÷ 4.55 GB ≈ 148 tok/s` 就是 BF16 权重的带宽上限，实测已到 70%。vLLM 优化的是吞吐而不是单流延迟；要 300+ tok/s 的单流，应该改用 llama.cpp + Q4 量化、FP8 权重或更小的模型。

## 快速开始

以 kind 为例：

```sh
kind create cluster --config kind-config.yaml --image kindest/node:v1.32.2
make install   # 安装 CRD
make run       # 前台运行 controller（默认 emptyDir 缓存）
```

另开一个终端：

```sh
kubectl apply -f config/samples/llm_v1alpha1_model.yaml
kubectl get models
kubectl port-forward svc/qwen3-0-6b 8080:8080
```

```sh
curl -s http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "messages": [
      {"role": "user", "content": "用一句话介绍你自己 /no_think"}
    ],
    "max_tokens": 128
  }' | python3 -m json.tool
```

第一次启动会下载模型，时间取决于网络和模型大小。controller 把 `progressDeadlineSeconds` 设为 1800 秒，避免慢网络下过早判定失败。

需要复用已下载的模型、开启 GPU 或换 vLLM 后端时，参考下面的示例与 `config/samples/`。

## 部署到真实集群

```sh
make docker-build docker-push IMG=<你的镜像仓库>/minillmkube:tag
make deploy IMG=<你的镜像仓库>/minillmkube:tag
kubectl apply -f config/samples/llm_v1alpha1_model.yaml
```

本地开发更推荐 `make run`，调试 controller 更方便。单节点 GPU 验证时用集群外运行也很方便：

```sh
MODEL_CACHE_HOST_PATH=/var/lib/minillmkube/models \
GPU_RUNTIME_CLASS=nvidia \
HF_ENDPOINT=https://hf-mirror.com \
./bin/manager --metrics-bind-address=:8081
```

## GPU 与 vLLM 的坑

- **vLLM 必须用 CUDA 13 镜像**：`v0.32.0-cu129` 内含按 CUDA 13 编译的 torchcodec，而运行时只有 CUDA 12.9，容器 import 阶段就会报 `libnvrtc.so.13: cannot open shared object file`（上游 vllm#60406）。默认 tag 已固定为 `vllm/vllm-openai:v0.32.0`，要求驱动 ≥ 580。
- **下载器镜像跟随 `spec.image`**：vLLM 下载器需要镜像内的 `hf` CLI，覆盖镜像后下载器也会跟着换；同时自动注入 `HF_HUB_DISABLE_XET=1`，避免 Xet 影子域名绕过 `HF_ENDPOINT` 后 401。
- **`/dev/shm` 只有 64 MiB**：vLLM 多进程启动会失败，operator 会挂 `emptyDir{medium: Memory}`，大小取内存 limit 的一半。
- **WSL2 默认关闭 pinned memory**：需要时给 operator 设 `ENABLE_WSL2_VLLM_PIN_MEMORY=true`，它会给 vLLM 容器注入 `VLLM_WSL2_ENABLE_PIN_MEMORY=1`。
- **端口别撞**：operator 自身 metrics 默认 `:8081`，Service 是 8080；宿主被占用时换 `--metrics-bind-address`，服务侧验证最稳的是在 Pod 内直连 `127.0.0.1:8080`。
- **单卡整数配额**：`nvidia.com/gpu` 不可切分，两个 GPU 服务不能同时跑；GPU 副本按 `Recreate` 更新，避免旧 Pod 占着显存不放。

## 状态与排障

`status.phase` 有五种：

- `Pending`：Deployment 已创建、尚未被观测。
- `Deploying`：滚动中。
- `Ready`：当前 generation 全部可用。
- `Failed`：规格非法或 Deployment 报失败。
- `ScaledToZero`：`spec.replicas` 为 0；Ready 条件为 False，缩容过程中 reason 为 `ScalingDown`，副本全部退出后为 `ScaledToZero`。

`status.conditions` 里的 `Ready` 会带上失败原因（下载失败、镜像拉取失败、格式不匹配、调度失败、GPU 不足、反复崩溃等）；请求 GPU 时另有 `GPUOffload` 条件，用来区分「真的用上了 GPU」和「静默退回 CPU」。`status.endpoint` 是集群内访问地址。

## 当前边界

M1～M5 已完成并合入 `main`；llama.cpp 与 vLLM 两条路径都已在单节点 K3s 上跑完整链路（2B 模型）。本地 `make test` / `make lint` 通过，CI 的 lint 任务已修正 Go 版本。

- 暂未做多模型路由，暂未提供 Helm Chart。
- GPU 只做到「资源请求 + RuntimeClass 透传 + 日志确认」，不感知显卡型号和驱动版本，不做多卡切分。
- 单卡 GPU 按整数配额分配，两个 GPU 服务不能并行；需要并行只能一个走 GPU、一个走 CPU。
- M5 的外部调度器只验证了字段透传，尚未在真实调度器上验收。
- 前缀缓存在 Qwen3.5-2B（混合线性注意力/Mamba 架构）上未观察到实际生效。
- 放置字段只透传不校验：填错 `nodeSelector` 会表现成 `Unschedulable`，不会在 admission 阶段被拒绝。

## 开发常用命令

```sh
make manifests generate   # 生成 CRD / RBAC / deepcopy
make build                # 构建 controller
make test                 # 运行测试（envtest）
make lint                 # 静态检查
make help                 # 查看全部命令
```

## 许可证

Copyright 2026.

Licensed under the Apache License, Version 2.0. See the Apache License for details.