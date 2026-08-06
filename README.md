# miniLLMkube

miniLLMkube 是一个用 Kubebuilder 编写的 Kubernetes Operator，用来声明式部署 GGUF 格式的大模型推理服务。

你只需要写一份 `Model` YAML，告诉 Kubernetes 模型文件在哪里、要几个副本、上下文长度是多少。Operator 会自动创建 `Deployment` 和 `Service`，下载模型权重，启动 `llama.cpp` server，并暴露 OpenAI 兼容接口。


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

- 使用 `Model` CRD 声明一个 GGUF 模型推理服务。
- 校验 `spec.source`：必须是 HTTPS URL，路径以 `.gguf` 结尾。
- 通过 initContainer 下载模型，使用 `curlimages/curl:8.11.1`。
- 下载时先写入 `/models/model.gguf.tmp`，成功后再移动为 `/models/model.gguf`，避免半成品模型被主容器读取。
- 如果 `/models/model.gguf` 已存在且非空，会跳过下载，方便 Pod 重启时复用模型文件。
- 主容器运行 `llama.cpp` server，并提供 OpenAI 兼容 API。
- 创建 ClusterIP Service，集群内访问地址会写入 `status.endpoint`。
- 根据 Deployment 状态维护 `Pending`、`Deploying`、`Ready`、`Failed` 阶段。
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
   |      +--> initContainer: 下载 GGUF 到 /models/model.gguf
   |      +--> llama.cpp server: 加载 /models/model.gguf
   |
   +--> Service: 暴露 8080 端口
   |
   +--> Model Status: phase / endpoint / readyReplicas / conditions
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

字段说明：

- `source`：GGUF 模型文件下载地址。必须是 HTTPS，并且 URL path 以 `.gguf` 结尾。
- `replicas`：推理 Pod 数量。可以设为 `0`，表示缩容到零。
- `contextSize`：上下文窗口大小，单位是 token。
- `image`：可选。默认使用 `ghcr.io/ggml-org/llama.cpp:server`。
- `resources`：推理容器的 CPU/内存请求和限制。建议显式配置，避免模型进程因为资源不足被 OOM Kill。

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

这个项目目前聚焦在最小可用的 GGUF 推理部署流程，还没有覆盖完整生产能力：

- 暂未做多模型路由。
- 暂未做 GPU 调度策略封装。
- 暂未做 Pod 级别失败原因下钻。
- 暂未提供 Helm Chart。
- 目前默认推理后端是 `llama.cpp` server。

## 许可证

Copyright 2026.

Licensed under the Apache License, Version 2.0. See the Apache License for details.
