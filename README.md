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

- **双后端、单 CRD**：支持 `llama.cpp`（GGUF）和 vLLM（safetensors）。`spec.runtime` / `spec.format` 默认 `auto`，能从 `source` 推断的不用手写；`vllm + gguf` 这类非法组合在 CEL 层直接拒绝。
- **下载与缓存隔离**：模型由 initContainer 下载到 `/models/<cacheKey>/`，每个 `source` 独立目录；`flock` 让多副本只有一个真正下载，`.tmp` 加断点续传，GGUF 校验魔数、safetensors 校验配置与全部分片，通过后才算缓存命中。
- **GPU 推理**：`spec.gpu` 请求 `nvidia.com/gpu`、自动切换 CUDA 镜像、追加 `--n-gpu-layers`，并从推理日志确认 GPU 是否真的生效（`GPUOffload` 条件）。
- **状态与诊断**：维护 `Pending` / `Deploying` / `Ready` / `Failed`；Pod 卡住时给出 `DownloadFailed`、`FormatMismatch`、`ImagePullFailed`、`InsufficientGPU`、`Unschedulable`、`RuntimeCrashing` 中的具体原因，原因变化时发 Warning 事件。
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
| 默认镜像 | `ghcr.io/ggml-org/llama.cpp:server` / `:server-cuda` | `vllm/vllm-openai:v0.32.0` |
| 镜像压缩体积 | 0.31 GB / 2.60 GB | 9.21 GB |
| 适合 | 单机小模型、CPU 也能跑、启动快 | 大模型、高吞吐、continuous batching |
| 状态 | 已在单节点集群上跑通 | 代码完成，集群验收未执行 |

参数、资源预留和 vLLM 的 `/dev/shm`、环境变量等细节见 [docs/backends.md](docs/backends.md)。

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

需要复用已下载的模型、开启 GPU、或换 vLLM 后端时，见 [docs/model-reference.md](docs/model-reference.md)。

## 部署到真实集群

```sh
make docker-build docker-push IMG=<你的镜像仓库>/minillmkube:tag
make deploy IMG=<你的镜像仓库>/minillmkube:tag
kubectl apply -f config/samples/llm_v1alpha1_model.yaml
```

本地开发更推荐 `make run`，调试 controller 更方便。

## 状态与排障

`status.phase` 有五种：`Pending`（Deployment 已创建、尚未被观测）、`Deploying`（滚动中）、`Ready`（当前 generation 全部可用）、`Failed`（规格非法或 Deployment 报失败）、`ScaledToZero`（`spec.replicas` 为 0，Ready 条件为 False；缩容过程中 reason 为 `ScalingDown`，副本全部退出后为 `ScaledToZero`）。

`status.conditions` 里的 `Ready` 会带上失败原因；请求 GPU 时另有 `GPUOffload` 条件，用来区分「真的用上了 GPU」和「静默退回 CPU」。

完整说明和常见场景见 [docs/troubleshooting.md](docs/troubleshooting.md)。

## 文档

- [docs/ROADMAP.md](docs/ROADMAP.md)：开发计划、任务分解、进度表与边界问题清单
- [docs/backends.md](docs/backends.md)：两种后端的参数、镜像体积与资源要求
- [docs/model-reference.md](docs/model-reference.md)：`Model` 字段说明与缓存语义
- [docs/troubleshooting.md](docs/troubleshooting.md)：状态、诊断原因与排障

## 当前边界

M1～M5 的代码已完成（当前在 `feat/m3-m4` 分支，尚未合入 `main`），本地 `make test` / `make lint` 通过。llama.cpp 侧已在单节点 K3s（RTX 5070 Ti Laptop，12 GB 显存）上跑通完整链路：权重下载与缓存命中 → GPU 推理 → Service 访问 → `GPUOffload=True`；**vLLM 侧的集群验收仍未执行**。

- 暂未做多模型路由，暂未提供 Helm Chart。
- GPU 只做到「资源请求 + RuntimeClass 透传 + 日志确认」，不感知显卡型号和驱动版本，不做多卡切分；GPU 副本按 `Recreate` 更新，避免旧 Pod 占着显卡不放。
- vLLM 后端代码已完成，但 M3-T0 的 Blackwell + WSL2 手动 Pod 探查和集群推理验收尚未执行；启动探针阈值、GPU 显存利用率都还是估计值。
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