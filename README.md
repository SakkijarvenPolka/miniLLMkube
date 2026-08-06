# minillmkube
用 Kubernetes CRD 声明式部署 GGUF 大模型推理服务。
一份 YAML 描述模型来源，Operator 负责下载权重、拉起 llama.cpp、暴露 OpenAI 兼容接口。

## Description
// TODO(user): An in-depth paragraph about your project and overview of use

## Goal
qiai_@Qiairesia:~$ curl -s localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"用一句话介绍你自己 /no_think"}],"max_tokens":128}' \
  | python3 -m json.tool
{
    "choices": [
        {
            "finish_reason": "stop",
            "index": 0,
            "message": {
                "role": "assistant",
                "content": "\u6211\u662fAI\u52a9\u624b\uff0c\u80fd\u5e2e\u52a9\u4f60\u89e3\u51b3\u95ee\u9898\u6216\u63d0\u4f9b\u652f\u6301\u3002"
            }
        }
    ],
    "created": 1786005915,
    "model": "/models/model.gguf",
    "system_fingerprint": "b10200-5f55650a7",
    "object": "chat.completion",
    "usage": {
        "completion_tokens": 17,
        "prompt_tokens": 16,
        "total_tokens": 33,
        "prompt_tokens_details": {
            "cached_tokens": 0
        }
    },
    "id": "chatcmpl-1eYJbCFlIWWDzCivPe1KonlSZWmM2Xty",
    "timings": {
        "cache_n": 0,
        "prompt_n": 16,
        "prompt_ms": 57.936,
        "prompt_per_token_ms": 3.621,
        "prompt_per_second": 276.16680475006905,
        "predicted_n": 17,
        "predicted_ms": 181.29,
        "predicted_per_token_ms": 10.664117647058823,
        "predicted_per_second": 93.77240884770258
    }
}

## Getting Started
1. kind create cluster --config kind-config.yaml --image kindest/node:v1.32.2
2. make install
3. （可选，加速）预下载模型到节点：
   docker exec <node> curl -fL -o /var/lib/llmkube-models/model.gguf <url>
4. MODEL_CACHE_HOST_PATH=/var/lib/llmkube-models make run
5. kubectl apply -f config/samples/
6. kubectl port-forward svc/qwen3-0-6b 8080:8080
7. curl localhost:8080/v1/chat/completions ...

### Prerequisites
- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/minillmkube:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/minillmkube:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/minillmkube:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/minillmkube/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

