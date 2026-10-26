# miniLLMkube

miniLLMkube 是一个用 Kubebuilder 编写的 Kubernetes Operator，用来声明式部署大模型推理服务。

你只需要写一份 `Model` YAML，告诉 Kubernetes 模型文件在哪里、要几个副本、上下文长度是多少。Operator 会自动创建 `Deployment` 和 `Service`，下载模型权重，启动 `llama.cpp` 或 vLLM，并暴露 OpenAI 兼容接口。


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

- 使用 `Model` CRD 声明一个推理服务，支持 `llama.cpp`（GGUF）和 vLLM（safetensors）两种后端。
- `spec.runtime` / `spec.format` 默认 `auto`：能从 `source` 推断的就不用手写，解析结果写进 `status.resolvedRuntime` / `status.resolvedFormat`；`vllm + gguf`、`llamacpp + safetensors` 这类组合会被 CEL 直接拒绝。
- 校验 `spec.source`：GGUF 必须是主机名非空的 HTTPS URL 且路径以 `.gguf` 结尾；vLLM 用 `hf://<org>/<repo>[@<revision>]`。
- 通过 initContainer 下载模型：GGUF 使用 `curlimages/curl:8.11.1`，safetensors 使用固定 vLLM 镜像内的 Hugging Face CLI（镜像实际工具可用性待 T0 核验）。
- 每个 `source` 有自己的缓存目录 `/models/<cacheKey>/`（GGUF 文件位于该目录内）：`cacheKey` 取 `source` 的 sha256 前 12 位，换模型不会互相覆盖，`status.cacheKey` 会把它暴露出来。
- 下载时先写 `<文件名>.tmp`，校验通过后再改名，避免半成品模型被主容器读取。
- 同一缓存目录用 `flock` 串行化：多个副本同时启动时只有一个 Pod 真正下载，其余 Pod 等锁后直接命中。
- 支持断点续传：已有 `.tmp` 时先 `HEAD` 比对 `Content-Length`，再 `curl -C -` 续传；`EXPECT_FORMAT=gguf` 时会校验 `GGUF` 魔数。
- 主容器运行 `llama.cpp` server 或 vLLM，并提供 OpenAI 兼容 API；vLLM 会自动给 `/dev/shm` 挂内存卷、关掉 service link 注入（vLLM 会校验未知的 `VLLM_*` 变量）。
- vLLM 下载器仅在完整下载成功并通过配置、权重及分片、tokenizer 检查后写入 `.complete` 完成标记。缓存命中必须有该标记，并重新检查配置、索引引用的全部分片和至少一种非空 tokenizer 文件；缺标记的目录会重新下载，不能只凭 `config.json` 和单个权重文件判定完成。格式不符报 `FORMAT_MISMATCH`。
- 支持 GPU 资源声明：`spec.gpu` 会请求 `nvidia.com/gpu`；llama.cpp 默认改用 CUDA 镜像、追加 `--n-gpu-layers`，并从推理日志里确认 GPU 是否真的生效。vLLM 必须声明有效 GPU 资源，实际推理可用性待集群验证。
- 创建 ClusterIP Service，集群内访问地址会写入 `status.endpoint`。
- 根据 Deployment 状态维护 `Pending`、`Deploying`、`Ready`、`Failed` 阶段。
- Pod 卡住时会从 Pod 状态里给出原因：`DownloadFailed`、`FormatMismatch`、`ImagePullFailed`、`InsufficientGPU`、`Unschedulable`、`RuntimeCrashing`，并只在原因变化时发一条 Warning 事件。
- 支持通过 `MODEL_CACHE_HOST_PATH` 在开发环境复用宿主机上的模型缓存。

## 架构简图

```text
Model YAML
   |
   v
Model Controller
   |
   +--> Deployment
   |      |
   |      +--> initContainer: 下载 GGUF 到 /models/<cacheKey>/<文件名>
   |      +--> llama.cpp server: 加载同一路径
   |
   +--> Service: 暴露 8080 端口
   |
   +--> Model Status: phase / endpoint / readyReplicas / resolvedRuntime / conditions
```

## Model 示例

示例文件在 `config/samples/llm_v1alpha1_model.yaml`：

```yaml
apiVersion: llm.example.io/v1alpha1
kind: Model
metadata:
  name: qwen3-0-6b
spec:
  source: https://hf-mirror.com/Qwen/Qwen3-0.6B-GGUF/resolve/main/Qwen3-0.6B-Q8_0.gguf
  replicas: 1
  contextSize: 4096
  resources:
    requests:
      cpu: "1"
      memory: "2Gi"
    limits:
      memory: "4Gi"
```

用 vLLM 跑 safetensors 仓库时换成 `hf://`（代码已实现，M3-T0 集群验证仍待执行；建议把下例 source 固定到实际 commit）：

```yaml
spec:
  source: hf://Qwen/Qwen3-0.6B
  runtime: vllm       # 可省略：hf:// 会自动推断为 vllm
  format: safetensors # 可省略：vllm 只接受 safetensors
  contextSize: 4096
  gpu: {}
```

需要 GPU 时加一段 `gpu`：

```yaml
spec:
  gpu:
    count: 1     # 目前上限为 1
    layers: 99   # 对应 --n-gpu-layers，99 表示全部层卸载
```

字段说明：

- `source`：模型来源。GGUF 是主机名非空的 HTTPS 下载地址且 URL path 以 `.gguf` 结尾；vLLM 是 `hf://<org>/<repo>[@<revision>]`。缓存键只由完整 source 字符串计算，不检测上游可变内容；应使用不可变 GGUF URL 或 `hf://<org>/<repo>@<commit>` 固定 revision。`main` 等可变分支同样可能命中旧缓存；需要升级时修改 source/revision。
- `runtime`：可选，`auto`（默认）、`llamacpp`、`vllm`。`auto` 时会从 `format` 和 `source` 推断：`.gguf` 结尾或 `format: gguf` 走 llama.cpp，`hf://` 或 `format: safetensors` 走 vLLM，推断不出来直接报 `InvalidSpec`。推断结果只写 status，不改 spec。
- `format`：可选，`auto`（默认）、`gguf`、`safetensors`。
- `replicas`：推理 Pod 数量。可以设为 `0`，表示缩容到零。
- `contextSize`：上下文窗口大小，单位是 token。
- `image`：可选。留空时按后端和是否请求 GPU 选默认镜像：llama.cpp GPU 用 `ghcr.io/ggml-org/llama.cpp:server-cuda`、CPU 用 `ghcr.io/ggml-org/llama.cpp:server`；vLLM 固定 `vllm/vllm-openai:v0.32.0-cu129`。
- `resources`：推理容器的 CPU/内存请求和限制。建议显式配置，避免模型进程因为资源不足被 OOM Kill。
- `gpu`：llama.cpp 可选；vLLM 必须设置 `gpu: {}` 或有效正整数 `resources.limits["nvidia.com/gpu"]`。写 `gpu: {}` 即请求一张 GPU（`count` 默认 1、上限 1）；`layers` 仅对 llama.cpp 生效，默认 99，翻译成 `--n-gpu-layers 99`。显式 GPU limit 优先，不会被 `gpu` 覆盖，零值、负值和小数不满足 vLLM 的 GPU 要求。
- `runtimeClassName`：可选。显式值始终透传（CPU 模式也一样）；GPU 场景一般填 `nvidia`。未设置时，只有请求 GPU 才会回落到 controller 的环境变量 `GPU_RUNTIME_CLASS`；CPU 模式不使用该环境变量。
- `extraArgs`：可选。原样追加在推理进程参数末尾。vLLM 对其自动生成的同名长 flag 去重（识别 `--x v` 与 `--x=v`）；llama.cpp 目前只对 GPU 层数及日志级别 flag（含已支持别名）去重，基础 flag 如 `--model`、`--host`、`--ctx-size` 仍可能重复。不要依赖所有参数都能安全覆盖。
- `nodeSelector` / `tolerations` / `affinity`：可选。原样写进 Pod 模板，用来把推理 Pod 钉到特定节点（GPU 型号、专用节点池、污点容忍）。
- `schedulerName`：可选。留给外部调度器（比如自研 GPU 调度插件）；留空时仍用集群默认的 `default-scheduler`。

## 本地快速体验

下面以 kind 集群为例。

### 1. 创建 kind 集群

```sh
kind create cluster --config kind-config.yaml --image kindest/node:v1.32.2
```

### 2. 安装 CRD

```sh
make install
```

### 3. 启动本地 controller

直接使用 `emptyDir` 存模型：

```sh
make run
```

如果你希望开发时复用已经下载过的模型，可以在 kind 节点里准备一个目录，然后通过 `MODEL_CACHE_HOST_PATH` 告诉 controller 使用 hostPath：

```sh
MODEL_CACHE_HOST_PATH=/var/lib/llmkube-models make run
```

当该变量为空时，Pod 会继续使用默认的 `emptyDir`。

`MODEL_CACHE_HOST_PATH` 应该指向一个**专用目录**（例如 `/var/lib/llmkube-models`）：controller 会先起一个 `cache-prep` initContainer，把挂载点根目录 `chown 100:101`（只改根目录，不递归）以便推理容器可写。把它指向 `/var/lib` 这类已有目录会改动宿主机上其他文件的属主。

### 4. 创建 Model

```sh
kubectl apply -f config/samples/llm_v1alpha1_model.yaml
```

查看状态：

```sh
kubectl get models
kubectl describe model qwen3-0-6b
kubectl get pods
```

第一次启动会下载模型，时间取决于网络和模型大小。为了避免大模型下载时间较长导致 Deployment 过早判定失败，controller 将 `progressDeadlineSeconds` 设置为 `1800` 秒。

### 5. 访问推理接口

把 Service 转发到本机：

```sh
kubectl port-forward svc/qwen3-0-6b 8080:8080
```

发送 OpenAI 兼容请求：

```sh
curl -s http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "messages": [
      {
        "role": "user",
        "content": "用一句话介绍你自己 /no_think"
      }
    ],
    "max_tokens": 128
  }' | python3 -m json.tool
```

正常情况下会返回类似 `chat.completion` 的 JSON 响应。

## 部署到真实集群

如果要把 controller 作为 Deployment 跑在集群里，需要先构建并推送镜像：

```sh
make docker-build docker-push IMG=<你的镜像仓库>/minillmkube:tag
```

然后部署 controller：

```sh
make deploy IMG=<你的镜像仓库>/minillmkube:tag
```

再创建示例模型：

```sh
kubectl apply -f config/samples/llm_v1alpha1_model.yaml
```

如果只是本地开发，更推荐使用 `make run`，这样调试 controller 更方便。

## 状态说明

`Model.status.phase` 当前有四种状态：

- `Pending`：Deployment 已创建，但还没有被 Kubernetes controller 观测到状态。
- `Deploying`：正在创建或滚动更新，副本还没全部可用。
- `Ready`：当前 Deployment generation 已观测，期望副本都已经可用。
- `Failed`：规格非法，或 Deployment 报告了失败条件。

`Model.status.conditions` 里的 `Ready` 会带上失败原因：Pod 下载失败是 `DownloadFailed`、文件格式不对是 `FormatMismatch`、镜像拉不下来是 `ImagePullFailed`、GPU 排不上是 `InsufficientGPU`、其它调度失败是 `Unschedulable`、推理进程反复崩溃是 `RuntimeCrashing`。原因变化时会额外发一条 Warning 事件，可以用 `kubectl describe model` 或 `kubectl get events` 看到。

请求 GPU 时还会有 `GPUOffload`：

- `True` / `OffloadDetected`：推理日志里读到 `llama_prepare_model_devices: using device CUDA0`，message 会带上原始整行。
- `False` / `CPUFallback`：镜像已经启动，但 llama.cpp 退回了 CPU（例如驱动或 RuntimeClass 没配好）。这是最容易踩的坑：Pod 是 Ready 的，速度却只有 CPU 水平。
- `Unknown` / `NotDetected`：Pod 还没 Ready、日志里还没有设备行，或者日志读不到。

请求 GPU 的非 llama.cpp 后端（包括 vLLM）仍保留 `GPUOffload` 条件，但为 `Unknown` / `NotDetected`，表示此检测器不支持该后端，并不表示 GPU 未使用。vLLM 启动失败（如显存不足）可落到 `RuntimeCrashing`。

可以通过下面命令查看更详细的 condition：

```sh
kubectl describe model qwen3-0-6b
```

## 开发常用命令

生成 CRD、RBAC 和 deepcopy 代码：

```sh
make manifests generate
```

构建 controller：

```sh
make build
```

运行测试：

```sh
make test
```

一次性执行生成、构建和测试：

```sh
make manifests generate build test
```

查看更多可用命令：

```sh
make help
```

## 清理

删除示例 Model：

```sh
kubectl delete -f config/samples/llm_v1alpha1_model.yaml
```

卸载 CRD：

```sh
make uninstall
```

如果用 `make deploy` 部署过 controller，可以卸载 controller：

```sh
make undeploy
```

如果用 kind 创建了测试集群，可以删除集群：

```sh
kind delete cluster --name llmops
```

## 当前边界

这个项目目前聚焦在最小可用的推理部署流程，还没有覆盖完整生产能力：

- 暂未做多模型路由。
- GPU 只做到「资源请求 + RuntimeClass 透传 + 日志确认」，不感知显卡型号、驱动版本，也不做多卡切分；GPU 副本目前按 `Recreate` 更新，避免旧 Pod 占着显卡不放。
- vLLM 后端代码已实现，但 M3-T0 的 Blackwell + WSL2 手动 Pod 探查和集群推理验收尚未执行；不能把参数渲染/本地测试通过当作后端实际可用性证据。固定镜像的 CLI、CUDA 内核和 WSL2 pin-memory 行为均待 T0 核验。启动探针目前是估计值 240 × 5s（20 分钟）；`--gpu-memory-utilization` 固定 0.85，未做显存自适应。operator 环境变量 `ENABLE_WSL2_VLLM_PIN_MEMORY=true` 才会注入 `VLLM_WSL2_ENABLE_PIN_MEMORY=1`。
- 放置字段（`nodeSelector` / `tolerations` / `affinity` / `schedulerName`）只透传不校验：填错 `nodeSelector` 会表现成 `Unschedulable`，不会在 admission 阶段被拒绝。
- 暂未提供 Helm Chart。

## 许可证

Copyright 2026.

Licensed under the Apache License, Version 2.0. See the Apache License for details.
