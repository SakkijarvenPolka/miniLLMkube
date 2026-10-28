# Model 字段说明

`Model` 是 miniLLMkube 唯一的自定义资源。示例见 `config/samples/llm_v1alpha1_model.yaml`。

## 最小示例（llama.cpp + GGUF）

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

## vLLM + safetensors

```yaml
spec:
  source: hf://Qwen/Qwen3-0.6B@<commit>   # 建议固定到不可变 commit
  runtime: vllm        # 可省略：hf:// 自动推断为 vllm
  format: safetensors  # 可省略：vllm 只接受 safetensors
  contextSize: 4096
  gpu: {}
```

vLLM 必须能拿到 GPU 资源；没有 GPU 的 Model 会在校验阶段被拒绝。

## GPU 与放置

```yaml
spec:
  gpu:
    count: 1     # 目前上限 1
    layers: 99   # 对应 --n-gpu-layers，仅 llama.cpp 生效
  runtimeClassName: nvidia
  nodeSelector:
    kubernetes.io/hostname: gpu-node-1
  tolerations:
    - key: nvidia.com/gpu
      operator: Exists
      effect: NoSchedule
```

## 字段说明

- `source`：模型来源。GGUF 必须是主机名非空的 HTTPS 地址且路径以 `.gguf` 结尾；vLLM 用 `hf://<org>/<repo>[@<revision>]`。缓存键只由完整 source 字符串计算，不检测上游内容变化：应使用不可变 GGUF URL 或固定 commit 的 `hf://`，否则上游改了内容仍可能命中旧缓存。
- `runtime`：`auto`（默认）、`llamacpp`、`vllm`。`auto` 时按 `format` 和 `source` 推断：`.gguf` 结尾或 `format: gguf` 走 llama.cpp，`hf://` 或 `format: safetensors` 走 vLLM，推断不出来报 `InvalidSpec`。推断结果只写 `status.resolvedRuntime`，不回写 spec。
- `format`：`auto`（默认）、`gguf`、`safetensors`。`vllm + gguf`、`llamacpp + safetensors` 会被 CEL 直接拒绝。
- `replicas`：推理 Pod 数量，可以设为 `0` 表示缩容到零。
- `contextSize`：上下文窗口大小（token），翻译成 llama.cpp 的 `--ctx-size` 或 vLLM 的 `--max-model-len`。
- `image`：可选。留空时按后端和是否请求 GPU 选默认镜像：llama.cpp GPU 用 `server-cuda`、CPU 用 `server`；vLLM 固定 `vllm/vllm-openai:v0.32.0-cu129`。
- `resources`：推理容器的 CPU/内存请求和限制。建议显式配置，避免模型进程被 OOM Kill；vLLM 还会用内存 limit 的一半作为 `/dev/shm` 大小。
- `gpu`：llama.cpp 可选、vLLM 必须。写 `gpu: {}` 即请求一张 GPU（`count` 默认 1、上限 1）；`layers` 默认 99。在 `resources.limits` 里显式写 `nvidia.com/gpu` 优先，不会被 `gpu` 覆盖；零值、负值和小数不满足 vLLM 的要求。
- `runtimeClassName`：可选。显式值始终透传（CPU 模式也一样）；未设置时，只有请求 GPU 才回落到 controller 环境变量 `GPU_RUNTIME_CLASS`。
- `extraArgs`：原样追加在推理进程参数末尾。vLLM 对 operator 生成的同名长 flag 去重（同时识别 `--x v` 和 `--x=v`）；llama.cpp 目前只对 GPU 层数和日志级别相关 flag 及别名去重，`--model`、`--host`、`--ctx-size` 等仍可能重复。不要依赖所有参数都能安全覆盖。
- `nodeSelector` / `tolerations` / `affinity`：原样写进 Pod 模板，用来把推理 Pod 钉到特定节点。
- `schedulerName`：可选，留给外部调度器；留空时仍用 `default-scheduler`。

## 缓存与下载语义

- 每个 `source` 一个目录：`/models/<cacheKey>/`，`cacheKey` 是 source 字符串 sha256 的前 12 位，通过 `status.cacheKey` 暴露。换 source 就是新目录，不会互相覆盖。
- GGUF 落在目录内的文件名取自 URL basename；不符合 `^[A-Za-z0-9._-]{1,200}$` 时回落到 `model.gguf`。
- 下载先写 `<文件名>.tmp`，校验通过才改成最终名，避免主容器读到半成品。
- 同一目录用 `flock` 串行化：多个副本同时启动时只有一个真正下载，其余等锁后直接命中。
- 断点续传：先用 `curl -r 0-0` 的 `Content-Range` 读出总长；已有 `.tmp` 时从当前大小继续，总长不一致或服务端返回 416 才删掉重来。中断由脚本外层重试处理（最多 30 次，单次 2 分钟无进展即判定停滞），本地文件始终保留，不会每次从头下载；`EXPECT_FORMAT=gguf` 时校验 `GGUF` 魔数，不符报 `FORMAT_MISMATCH`。
- safetensors 侧只有在完整下载并通过配置、权重分片和 tokenizer 检查后才写 `.complete` 完成标记；缓存命中必须带该标记，且会重新检查 `config.json`、索引引用的全部分片和至少一种非空 tokenizer 文件。

## 状态字段

- `status.phase`：`Pending`、`Deploying`、`Ready`、`Failed`。
- `status.endpoint`：Service 的集群内地址。
- `status.resolvedRuntime` / `status.resolvedFormat`：auto 解析结果。
- `status.cacheKey`：当前 source 对应的缓存目录名。
- `status.conditions`：`Ready` 与 `GPUOffload`，均为标准 `metav1.Condition`。

诊断与排障见 [troubleshooting.md](troubleshooting.md)。