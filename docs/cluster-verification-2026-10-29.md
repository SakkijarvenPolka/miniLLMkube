# miniLLMkube 单节点集群验收报告

- 日期：2026-10-29
- 分支：`feat/m3-m4`（M1~M5 代码完成；本报告对应提交 `16e0672`、`816ce95`、`2021168`）
- 范围：在自有单节点 K3s 上，用同一个模型家族分别验证 llama.cpp 与 vLLM 两条推理路径，覆盖“apply Model → 下载权重 → 启动 Pod → 就绪探针 → 集群内访问 → 推理”全链路。
- 结论：**两条路径均可用**，llama.cpp 单流更快，vLLM 多并发吞吐更高；期间发现 4 个缺陷/陷阱，均已修复或写入文档。

## 1. 环境

| 项 | 值 |
|---|---|
| 主机 | Windows 11 + WSL2（Ubuntu 24.04），磁盘 `/dev/sdd` 1007 GB |
| 集群 | k3s v1.36.5+k3s1 单节点 `llmops-wsl`，containerd 2.3.4 |
| GPU | NVIDIA GeForce RTX 5070 Ti Laptop，12227 MiB，驱动 591.86，sm_120，带宽 672 GB/s |
| GPU 接入 | NVIDIA device plugin，`nvidia.com/gpu=1`，RuntimeClass `nvidia` |
| 缓存 | hostPath `/var/lib/minillmkube/models` |
| operator | 集群外进程，`MODEL_CACHE_HOST_PATH` + `GPU_RUNTIME_CLASS=nvidia` + `HF_ENDPOINT=https://hf-mirror.com` |

## 2. 方法

1. 按 `config/crd/bases` 安装 CRD，启动 operator。
2. 依次 apply 两个 Model（单卡整数配额，必须串行）：`qwen35-2b-llamacpp`、`qwen35-2b-vllm`。
3. 观察 `status.phase`、`Ready`、`GPUOffload` 条件与 Pod 事件。
4. 从 Pod 内用 Python 标准库直连 `127.0.0.1:8080`（避免 port-forward 与宿主 8081 端口冲突），测 `/v1/models`、`/health`、`/metrics`、聊天与补全接口。
5. 性能取 steady-state：流式输出统计首 token 时间（TTFT）与解码速率，忽略首次 CUDA graph 捕获。

## 3. llama.cpp 侧结果

| 项 | 实测 |
|---|---|
| 模型 | `unsloth/Qwen3.5-2B-GGUF` 的 `Qwen3.5-2B-Q8_0.gguf`（1.92 GB，hf-mirror） |
| 镜像 | `ghcr.io/ggml-org/llama.cpp:server-cuda`（GPU 节点已缓存，2.60 GB） |
| 下载 | 首次在 1.3 GB 处反复中断（见 §5.1）；修复后可断点续传，第二次 Pod 直接 `Cache hit` |
| 上下文 | 32768，`--n-gpu-layers 99`，`-lv 5` |
| 启动 | 到 `Ready` 约 40 秒 |
| GPU 生效 | `GPUOffload=True` / `OffloadDetected`（修复日志读取窗口后，见 §5.2） |
| 吞吐 | 单流约 **140~150 tok/s**（175 个输出 token / 1.17~1.33 s） |
| 集群内访问 | `http://qwen35-2b-llamacpp.default.svc.cluster.local:8080` 正常 |

## 4. vLLM 侧结果

| 项 | 实测 |
|---|---|
| 模型 | `Qwen/Qwen3.5-2B`（safetensors 4.55 GB） |
| 镜像 | `vllm/vllm-openai:v0.32.0`（CUDA 13，压缩 9.21 GB；经华为云 SWR 的 docker.io 镜像站拉取，20 分 40 秒） |
| 启动 | 容器启动到 `Ready` **2 分 57 秒**（引擎初始化 120 秒，含 CUDA graph 捕获）；`/health` 探针通过 |
| 显存 | 约 **10.5 GiB**（权重+非 torch 4.75 GiB、峰值激活 1.9 GiB、CUDA graph 0.13 GiB、KV cache 3.49 GiB） |
| KV cache | 274,370 tokens；32K 上下文并发 8.37x |
| Blackwell | sm_120 内核可用，无 “no kernel image” 报错 |
| TTFT | 冷启动后首请求 160 ms，热请求 23 ms |
| 吞吐 | 单流解码 **约 104 tok/s**（400 token 输出整体 101~103 tok/s）；4 并发聚合 **316 tok/s** |
| 端点 | `/health` 200、`/v1/models` 正常、`/metrics` 暴露 Prometheus 指标；`/v1/stores`、`/stores` 404；`x-prompt-cache-key` 被忽略 |
| 前缀缓存 | 引擎默认 `enable_prefix_caching=True`，但重复相同 prompt 后 `vllm:prefix_cache_hits_total` 始终为 0（Qwen3.5-2B 为混合线性注意力/Mamba 架构，0.32.0 下未观察到实际复用） |
| 条件 | `GPUOffload=Unknown/NotDetected`，符合设计（该条件仅对 llama.cpp 生效） |

## 5. 验证中发现并修复的问题

### 5.1 大文件下载无法续传（已修复）

`curl --retry` 内部的 `-C -` 在重试时偏移被截断，1.3 GB 后每次从 0 重下。脚本改为外层重试（最多 30 次）、用 `curl -r 0-0` 的 `Content-Range` 取总长、退出码 33 删除 `.tmp` 重来，403/416 视为远端已完成。修复后 1.92 GB 模型可断点续传，vLLM 的 4.55 GB 权重同样受益。

### 5.2 `GPUOffload` 恒为 `NotDetected`（已修复）

`-lv 5` 的启动日志有 6000 余行，设备行在第 76/1588 行，而扫描用 `TailLines=1000` 的窗口取不到。改为从 `pod.CreationTimestamp` 起读（`SinceTime`），保留 `LimitBytes` 防止单行过大。

### 5.3 默认 vLLM 镜像 `-cu129` 启动即崩（已修复）

`vllm/vllm-openai:v0.32.0-cu129` 内含的 torchcodec 按 CUDA 13 编译，运行时只有 CUDA 12.9，容器 import 阶段报 `libnvrtc.so.13: cannot open shared object file`（上游 vllm#60406）。默认 tag 改为 CUDA 13 构建 `v0.32.0`（要求驱动 ≥580，本机 591.86 满足）。

### 5.4 下载器镜像不跟随 `spec.image`（已修复）

vLLM 下载器需要镜像内的 `hf` CLI，原先硬编码 `vllmImage`，用户覆盖 `spec.image` 后下载器仍拉旧 tag。改为 `imageFor(model)`；同时注入 `HF_HUB_DISABLE_XET=1`，避免 Xet 影子域名绕过 `HF_ENDPOINT` 后 401。

## 6. 性能分析与预期差距

“为什么单流只有约 100 tok/s，而不是预期的 300~400？”——这是**模型精度与架构决定的上限**，不是配置问题：

1. **带宽是硬上限**。单流解码每生成一个 token 都要把模型权重读一遍：`672 GB/s ÷ 4.55 GB（BF16 权重）≈ 148 tok/s`。实测 104 tok/s 已达到该上限的 **70%**，属正常水平（kernel launch、注意力计算、采样都会吃掉余量）。
2. **2B 模型 decode 不是纯带宽受限**。层数少、每层计算量小，GPU 大部分时间在等 kernel 调度，vLLM 的 PagedAttention/调度开销占比更高；vLLM 的设计目标是**吞吐**（batch），不是单流延迟。
3. **4 并发聚合 316 tok/s** 说明 batching 有效：权重只读一次、服务多个请求。要继续提高聚合吞吐可以加并发；但单卡 12 GB、KV cache 3.49 GiB 决定了并发上限（32K 上下文 8.37x）。
4. **要接近 300~400 tok/s 单流，只有换更小的数值精度**：
   - llama.cpp 跑 Q4_K_M（1.28 GB）时带宽上限约 525 tok/s，实测通常在 250~350；
   - vLLM 用 FP8 权重（约 2.3 GB）上限约 292 tok/s，实测约 200 出头；
   - 再往下是 0.8B 级小模型（Q4 约 0.5 GB，上限 1300+），但质量明显下降；
   - 推测解码（`--speculative-config`）可再提升 1.5~2 倍，但需要草稿模型/额外显存。
5. 另有两个小损耗：笔记本 GPU 的功耗墙与 WSL2 虚拟化（约 5~10%），以及本机桌面占用约 1.4 GB 显存。

**结论**：当前数字与硬件能力一致；如果验收指标是“单流 300+ tok/s”，应改用 Q4 量化 + llama.cpp，而不是继续调 vLLM。

## 7. 未覆盖项

- M5 外部调度器（`schedulerName`）未在真实调度器上验证；当前仅验证字段透传。
- `VLLM_WSL2_PIN_MEMORY` 未开启，未验证其对 vLLM 吞吐的影响。
- 前缀缓存未生效（见 §4），未与 Qwen3 稠密模型对比确认是架构相关还是版本相关。
- `replicas: 0` 后 `status.phase` 仍显示 `Ready`（Deployment 期望副本为 0 时 `readyReplicas >= desired` 成立），属已知体验问题，未在本轮修复。

## 8. 复现步骤

```bash
# 1. 基础设施
sudo systemctl start k3s
kubectl apply -f config/crd/bases/llm.example.io_models.yaml
MODEL_CACHE_HOST_PATH=/var/lib/minillmkube/models GPU_RUNTIME_CLASS=nvidia HF_ENDPOINT=https://hf-mirror.com   ./bin/manager --metrics-bind-address=:8081

# 2. llama.cpp（Qwen3.5-2B Q8_0）
kubectl apply -f - <<'YAML'
apiVersion: llm.example.io/v1alpha1
kind: Model
metadata: {name: qwen35-2b-llamacpp}
spec:
  source: https://hf-mirror.com/unsloth/Qwen3.5-2B-GGUF/resolve/main/Qwen3.5-2B-Q8_0.gguf
  runtime: llamacpp
  format: gguf
  contextSize: 32768
  gpu: {count: 1, layers: 99}
  resources: {requests: {cpu: "2", memory: 4Gi}, limits: {cpu: "4", memory: 8Gi}}
YAML

# 3. vLLM（Qwen3.5-2B safetensors）
kubectl apply -f - <<'YAML'
apiVersion: llm.example.io/v1alpha1
kind: Model
metadata: {name: qwen35-2b-vllm}
spec:
  source: hf://Qwen/Qwen3.5-2B
  runtime: vllm
  format: safetensors
  contextSize: 32768
  image: swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/vllm/vllm-openai:v0.32.0
  gpu: {count: 1}
  resources: {requests: {cpu: "2", memory: 4Gi}, limits: {cpu: "4", memory: 8Gi}}
YAML

# 4. 观察与测试（在 Pod 内执行，避免宿主端口冲突）
kubectl exec -i deploy/qwen35-2b-vllm -- python3 - <<'PY'
import json, urllib.request
body={"model":"qwen35-2b-vllm","messages":[{"role":"user","content":"你好"}],
      "max_tokens":50,"chat_template_kwargs":{"enable_thinking":False}}
req=urllib.request.Request("http://127.0.0.1:8080/v1/chat/completions",
    data=json.dumps(body).encode(), headers={"Content-Type":"application/json"})
print(json.loads(urllib.request.urlopen(req, timeout=120).read())["choices"][0]["message"]["content"])
PY
```

## 9. 结论

- llama.cpp：**可用**，单流约 140~150 tok/s，适合交互式低并发场景。
- vLLM：**可用**，单流约 104 tok/s，4 并发 316 tok/s，适合高并发服务；注意必须用 CUDA 13 镜像。
- 期间修复的 4 个问题均已进入代码与文档；剩余未覆盖项集中在 M5 调度器与混合架构的前缀缓存。
