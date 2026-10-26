# 后端对比与资源要求

miniLLMkube 支持两种推理后端。`spec.runtime` 默认 `auto`：GGUF 地址走 llama.cpp，`hf://` 或 safetensors 走 vLLM；也可以显式指定。

## 什么时候用哪个

| 场景 | 建议 |
|---|---|
| 单机、单卡跑 7B 以下模型 | llama.cpp |
| 没有 GPU，纯 CPU 推理 | llama.cpp |
| 需要 GGUF 量化（Q4、Q8 等） | llama.cpp |
| 只有 safetensors 仓库 | vLLM |
| 需要高吞吐、continuous batching | vLLM |

## llama.cpp

- 默认镜像：CPU `ghcr.io/ggml-org/llama.cpp:server`，GPU `ghcr.io/ggml-org/llama.cpp:server-cuda`。
- 下载器镜像：`curlimages/curl:8.11.1`，把 GGUF 下载到 `/models/<cacheKey>/<文件名>`。
- 启动参数：`--model <文件> --host :: --port 8080 --ctx-size <contextSize> --alias <Model 名>`。
- 请求 GPU 时追加 `--n-gpu-layers <layers>`（默认 99）和 `-lv 5`；后者是为了让 llama.cpp 打印实际加载设备，`GPUOffload` 条件从这行日志解析。
- 启动探针按 120 次 × 5 秒（10 分钟）估算，给加载模型和创建 CUDA 上下文留时间。

## vLLM

- 默认镜像：`vllm/vllm-openai:v0.32.0-cu129`（固定 tag，nightly 需要更新的驱动）。
- 必须请求 GPU：写 `gpu: {}` 或在 `resources.limits` 里给有效的 `nvidia.com/gpu`。
- 启动参数：`--model <目录> --served-model-name <Model 名> --host :: --port 8080 --max-model-len <contextSize> --gpu-memory-utilization 0.85`。
- 下载器复用同一个镜像里的 Hugging Face CLI，按 `hf://<org>/<repo>[@<revision>]` 取模型；设置 operator 环境变量 `HF_ENDPOINT` 可走镜像站。
- Pod 级调整：给 `/dev/shm` 挂内存 emptyDir（大小取内存 limit 的一半，没有 limit 时 1Gi），并把 `enableServiceLinks` 关掉——vLLM 会校验未知的 `VLLM_*` 变量，service link 注入的变量会让它启动失败。
- 只有 operator 环境变量 `ENABLE_WSL2_VLLM_PIN_MEMORY=true` 时才注入 `VLLM_WSL2_ENABLE_PIN_MEMORY=1`（WSL2 下需要，普通 Linux 节点不要开）。
- 启动探针按 240 次 × 5 秒（20 分钟）估算，尚未用真实冷启动时间校准。

## 镜像体积与磁盘

| 后端 | 默认镜像 | Registry 压缩大小（amd64） | 落盘估算 |
|---|---|---|---|
| llama.cpp CPU | `ghcr.io/ggml-org/llama.cpp:server` | 0.31 GB | 约 1 GB |
| llama.cpp GPU | `ghcr.io/ggml-org/llama.cpp:server-cuda` | 2.60 GB | 约 5–6 GB |
| vLLM | `vllm/vllm-openai:v0.32.0-cu129` | 11.18 GB | 约 20–25 GB |

vLLM 体积大，是因为镜像里打进了整套运行时：CUDA runtime（cu129）、PyTorch 自带的另一份 CUDA 库，以及 vLLM 自己编译的 FlashAttention、paged-attention 和量化 kernel。llama.cpp 镜像里只有一个静态编译的二进制和最小运行库，所以小一个数量级。

压缩大小取自镜像仓库的 manifest；落盘大小是解包后的估算（CUDA 类镜像通常膨胀到 2 倍左右）。按 vLLM 规划节点时至少预留：

- 镜像解包：约 25 GB
- 模型权重：按仓库大小，`Qwen3-0.6B` 约 1.5 GB
- `/dev/shm`：上限是推理容器内存 limit 的一半
- 建议合计预留 **40 GB 以上**

拉取耗时由带宽决定：11.18 GB 在 20 MB/s 下约 10 分钟，5 MB/s 下约 37 分钟。先确认空间：

```sh
df -h /var/lib/rancher   # k3s 的 containerd 镜像默认落在这里
```

## GPU 节点准备

- 节点要装 NVIDIA 驱动和 device plugin，让 `nvidia.com/gpu` 成为可调度资源。
- 默认运行时类名是 `nvidia`（k3s 的 GPU 节点常见配置）。可以在 `Model.spec.runtimeClassName` 显式指定，或由 controller 的环境变量 `GPU_RUNTIME_CLASS` 兜底；只有请求 GPU 时才使用该环境变量。
- 想在开发环境复用模型缓存，把 `MODEL_CACHE_HOST_PATH` 指向一个**专用目录**：controller 会起 `cache-prep` initContainer 把挂载根目录 `chown 100:101`（只改根目录，不递归）。指向 `/var/lib` 这类已有目录会改动宿主机上其他文件的属主。

> vLLM 分支的 Blackwell、CUDA 内核、CLI 可用性和冷启动时间仍在等 M3-T0 的集群探查，详见 [ROADMAP.md](ROADMAP.md)。