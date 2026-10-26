# miniLLMkube 开发计划（v3，agent 可执行版）：GPU 推理 + vLLM/llama.cpp 双引擎

> 状态：规划文档，尚未改代码。基线提交 `cd79cde`，日期 2026-10-25。
> 读者：负责执行开发的 agent。按任务编号推进，一个任务一个提交，完成后更新第 6 章。
> 本版已把上一版所有「实现时再确认」的事项实测落定（第 2 章 F1~F14，全部带证据）；除第 9 章列出的少数优化项外，任务里的代码与命令可以直接照写执行。
> 第 7~9 章沿用上一版并小幅修订（E20），各任务按编号引用。

## 0. 执行须知（每个任务都适用）

### 0.1 环境

- 代码位于 WSL `Ubuntu-24.04` 的 `/home/qiai/workspace/miniLLMkube`。本机 Go 为 1.27.1，go.mod 声明 1.25.7。`GOPROXY=https://goproxy.cn,direct`。
- 集群为 k3s v1.36.5 单节点 `llmops-wsl`，kubeconfig 是 `~/.kube/config`（context `default`）。RuntimeClass `nvidia` 已存在，节点可分配 `nvidia.com/gpu: 1`（12GB，Windows 桌面常驻约 1.2GB，E23）。
- kubectl 一律用绝对路径：`/home/qiai/.local/bin/kubectl --kubeconfig /home/qiai/.kube/config`（下文简写 `kubectl`）。
- 本地模型：`/home/qiai/models/Qwen3-0.6B-Q8_0.gguf`，大小 639446688 字节。
- 节点上已有的镜像：`curlimages/curl:8.11.1`、`ghcr.io/ggml-org/llama.cpp:server`、`ghcr.io/ggml-org/llama.cpp:server-cuda`。vLLM 镜像还没拉。
- Pod 出网已通（hf-mirror.com 可直连，F12）；宿主机 curl 会自动走 `127.0.0.1:7890` 代理。出网依赖 `.wslconfig` 的 `firewall=false`（E37），环境验收时先跑一次 Pod 内 curl。
- kind 已装但没建集群。`make test-e2e` 会创建 kind 集群 `minillmkube-test-e2e`，需要 Docker Desktop 在运行。不要拿 k3s 当 e2e 环境。

### 0.2 项目硬性规则（出自仓库 `AGENTS.md`）

- 以下文件不手改：`config/crd/bases/*.yaml`、`config/rbac/role.yaml`、`api/**/zz_generated.*.go`、`PROJECT`。改了 `*_types.go` 或 RBAC marker 之后，执行 `make manifests generate`。
- 不删除 `// +kubebuilder:scaffold:*` 注释，不移动已有文件。新文件只放在 `internal/controller/` 下，属于 package `controller`。
- 日志风格：首字母大写、末尾不带句号、用过去时、写明对象类型、键值成对。例如 `log.Info("Created Deployment", "name", d.Name)`。
- 状态一律用 `metav1.Condition` 表达。

### 0.3 每个任务的完成标准

1. 改了 API 或 marker 时，执行 `make manifests generate`。
2. 执行 `make lint-fix`，再确认 `make lint` 无报错。第一次运行会 `go install` golangci-lint v2.11.4 并构建带 logcheck 的 custom 版本，耗时较长属正常。
3. `make test` 通过。它基于 envtest，k8s 1.35.0 的二进制在 `bin/k8s`。
4. 任务写了「集群验证」的，在 k3s 上按步骤跑一遍，并把关键输出贴进提交说明。
5. 更新第 6 章的进度表。

### 0.4 git 工作流

- 仓库 origin 是 `https://github.com/SakkijarvenPolka/miniLLMkube.git`，基线 `cd79cde`（main）。**执行者一律不要 push**，除非用户明确要求。
- 每个任务从最新 main 拉分支：`git checkout -b feat/m1-t2`（任务号小写）。里程碑内多个小任务可以合并同一分支，但提交分开。
- 提交信息格式：`M1-T2: <一句话概括>`。进度表更新和代码放同一个提交。

### 0.5 任务领取协议（多 agent 并行时必读）

1. 通读第 0、1、2 章和目标任务全文，以及它引用的 E 条目。
2. `make test` 确认基线绿色（当前 7 个 envtest 用例全过）。
3. 在第 6 章把任务标成「进行中」并提交，防止别的 agent 重复领取。
4. 实现 → 0.3 清单 → 集群验证（如有）→ 更新第 6 章 → 提交。
5. 卡住时把失败证据（命令 + 输出）写进第 6 章备注列再停下，不要静默放弃，也不要擅自改第 0.2 的规则。

### 0.6 运行与排查速查

- 本地跑 operator：`MODEL_CACHE_HOST_PATH=/home/qiai/models GPU_RUNTIME_CLASS=nvidia make run`（两个环境变量按需给）。
- `make run` Ctrl-C 之后检查残留：`pgrep -af 'bin/manager|go run'`，有就 `pkill -f bin/manager`。
- 验证推理：`kubectl port-forward svc/<model名> 8080:8080`，然后 `curl localhost:8080/health`。
- 看 GPU 占用：`kubectl describe node llmops-wsl | grep -A2 nvidia.com/gpu`。
- 集群验证都在 default namespace 做完即删：`kubectl delete model --all`。
- 镜像拉取走 `/etc/rancher/k3s/registries.yaml` 的 daocloud 镜像（docker.io → docker.m.daocloud.io），vLLM 镜像同样适用。

## 1. 现有代码地图（基线 cd79cde）

| 位置 | 内容 | 本计划中的处理 |
|---|---|---|
| `api/v1alpha1/model_types.go:28-55` | `ModelSpec`：`Source`、`Replicas`、`ContextSize`、`Image`（带 `+kubebuilder:default="ghcr.io/ggml-org/llama.cpp:server"`）、`Resources` | M2/M3/M5 加字段；M2 删除 Image 的 default |
| `api/v1alpha1/model_types.go:58-77` | `ModelStatus`：Phase、Endpoint、ReadyReplicas、ObservedGeneration、Conditions | M1/M3 加字段 |
| `internal/controller/model_controller.go:43-59` | 常量 `inferencePort=8080`、`modelPath="/models/model.gguf"`、`downloaderImage`、`progressDeadline=1800`、`modelCacheEnv` | M1 删除 `modelPath` |
| `internal/controller/model_controller.go:85-135` | `Reconcile`：validateSpec → reconcileDeployment → reconcileService → evaluateRollout → updateStatus | M1 加冲突重试；M4 加 Pod 诊断 |
| `internal/controller/model_controller.go:138-145` | `SetupWithManager`：For Model，Owns Deployment/Service | M4 加 Pod watch |
| `internal/controller/model_controller.go:158-224` | `reconcileDeployment` 的 CreateOrUpdate mutate：全部内联；init 脚本是 171-177 行的字符串 | M1-T2 拆到 `pod_builder.go` 并修 F9（当前每轮发多余 Update） |
| `internal/controller/model_controller.go:202-220` | startup/readiness 探针，只设了部分计数字段，其余靠 apiserver 默认值补 | M1-T2 全部显式（F9） |
| `internal/controller/model_controller.go:228-250` | `reconcileService`：ClusterIP，selector=labelsFor | 不变 |
| `internal/controller/model_controller.go:252-277` | `updateStatus`：DeepEqual 判定无变化就不写，否则 MergeFrom 打补丁 | M4 扩展 |
| `internal/controller/model_controller.go:279-291` | `validateSpec`：要求 https 且路径以 `.gguf` 结尾 | M3 改为按格式校验 |
| `internal/controller/model_controller.go:347-353` | `labelsFor`：name/instance/managed-by，同时用作 selector | 不变（E28） |
| `internal/controller/model_controller.go:396-403` | `serverArgs`：`--model --host 0.0.0.0 --port --ctx-size` | M3 移进 llama.cpp 后端 |
| `internal/controller/model_controller.go:405-424` | `modelVolume`：环境变量有值时用 hostPath（DirectoryOrCreate），否则 emptyDir | M1 保留 |
| `internal/controller/model_controller_test.go` | 7 个 envtest 用例，断言了 `/models/model.gguf`、init 容器数为 1、镜像等 | 各任务按需修改 |
| `cmd/main.go:181-187` | 构造 `ModelReconciler{Client, Scheme}` | M2-T4 注入 Clientset，M4 注入 Recorder |

## 2. 写计划前在本机实测的事实（会影响设计，全部带证据）

| # | 事实 | 结论 |
|---|---|---|
| F1 | `curlimages/curl:8.11.1` 以 `uid=100(curl_user) gid=101` 运行；`sh` 和 `flock` 都是 BusyBox v1.37.0 | init 脚本必须兼容 BusyBox ash |
| F2 | BusyBox `flock` 只支持 `-s -x -u -n`，不支持 `-w`。fd 方式 `exec 9>file; flock 9` 的互斥有效 | 取锁无法设超时，只能阻塞等待，靠 curl 的停滞超时兜底 |
| F3 | 镜像内有 flock、sha256sum、head、basename、find、mv、stat、wc、cut、sed、mkdir、rm、cat；curl 支持 `-C -` 和 `--retry-all-errors` | 脚本只用这些命令；要用别的，先在镜像里验证（E8） |
| F4 | 用 `head -c 4 f` 判断 `GGUF` 魔数可行 | M1/M3 的格式后验证直接用它 |
| F5 | `server-cuda` 镜像默认 verbosity 3 下只输出 11 行日志，无任何 CUDA/offload 行 | → 已被 F10 细化并给出解法 |
| F6 | `Image` 字段带 CRD default，apiserver 会自动写入 CPU 镜像名，`imageFor` 的空值分支在集群里永远走不到 | 要按 GPU/引擎选默认镜像，得先删掉这个 default（M2-T1） |
| F7 | controller-runtime v0.23.3 提供 `mgr.GetEventRecorder(name) events.EventRecorder`，旧接口 `GetEventRecorderFor` 已弃用 | M4 用新接口 |
| F8 | hostPath 根目录 `/home/qiai/models` 目前是 777（手工 chmod 过），其中 `model.gguf` 的属主是 uid 100 | M1 上线后建议换专用缓存根（M1-T3） |
| F9 | apiserver 会把默认值写进**存储的** Deployment 模板（2026-10-25 实测）：容器级 `terminationMessagePath=/dev/termination-log`、`terminationMessagePolicy=File`、`imagePullPolicy`；探针级 `periodSeconds/timeoutSeconds/successThreshold/failureThreshold`；Pod 级 `schedulerName=default-scheduler`、`restartPolicy=Always`、`dnsPolicy=ClusterFirst`。而 controller-runtime v0.23.3 的 `CreateOrUpdate` 逻辑是「Get → existing 深拷贝 → mutate → `equality.Semantic.DeepEqual(existing, obj)` → 不同才 Update」。当前代码整体覆盖 Containers/InitContainers、探针只设部分字段，mutate 后的对象必然与 existing 不同 → **每轮 reconcile 都发一次 Update**。apiserver 收到后重新默认化，大概率是无操作写入（未完全验证，见第 9 章），所以没观察到无限滚动，但多余 API 调用、审计噪音和 409 冲突窗口是确定的——此前实测到的 model_controller.go:105 处 409 与此一致 | 构建器必须显式写全 apiserver 会补的默认字段；mutate 只赋值本 operator 拥有的字段。M1-T2 给出完整代码和回归测试 |
| F10 | llama.cpp `server-cuda`（build 11804）实测（2026-10-25）：默认 verbosity 3 时 GPU 与 CPU 完全同日志（11 行，无法区分）；加 `-lv 5` 后出现 `ggml_cuda_init: found 1 CUDA devices`、`llama_prepare_model_devices: using device CUDA0 (NVIDIA GeForce RTX 5070 Ti Laptop GPU)`、`load_tensors: layer N assigned to device CUDA0`。**无 GPU 时同一镜像不报错**，`-lv 5` 下显示 `using device CPU` 和 `assigned to device CPU`（D5 探针）。另外默认就是全量 offload：不加 `--n-gpu-layers` 也用 CUDA（D1/D2 日志与 nvidia-smi 显存占用一致） | M2-T4 检测方案定型：GPU Pod 追加 `-lv 5` + 控制器扫日志。`--n-gpu-layers` 仍显式传（可移植性）。`-lv 5` 启动日志约 4300 行/0.6B 模型，只在 Ready 后扫一次 |
| F11 | client-go v0.35.0 存在 `events.NewFakeRecorder(bufferSize int) *FakeRecorder`（`tools/events/fake.go:48`），`Events` 字段是 `chan string` | M4-T2 测试直接用，不用再确认 |
| F12 | BusyBox 实测（2026-10-25）：`wc -c < f` 输出无前导空格；`find -maxdepth 1 -name '*.tmp' ! -name X -exec rm -f {} +` 语义正确；Pod 内 `curl -fsIL` + sed 解析 hf-mirror 的 Content-Length 得到 639446688 | M1 下载脚本原样可用；等长 `.tmp` 的 `curl -C -` 行为无需再验证（脚本已先删） |
| F13 | vLLM 官方镜像 tag（hub API 经代理查询，2026-10-25）：稳定版 `v0.32.0`，有 `cu129` 变体；nightly 是 cu134 构建（CUDA 13.4 > 本机驱动 591.86/CUDA 13.1，**不能用**）。cu129（12.9 ≤ 13.1）符合驱动向后兼容 | M3-T0 固定用 `vllm/vllm-openai:v0.32.0-cu129`，不要用 latest/nightly |
| F14 | `llama-server --help` 确认存在：`-ngl/--gpu-layers/--n-gpu-layers N`、`-a/--alias`、`--metrics`、`-lv/--verbosity N`（另有 env `LLAMA_ARG_LOG_VERBOSITY`） | E17 用 `--alias` 落地；`hasFlag` 去重必须同时匹配长/短别名（E16） |

## 3. 目标代码结构

全部放在 package `controller` 里，不新增 package，以免和 kubebuilder 的布局冲突。

| 文件 | 职责 | 引入任务 |
|---|---|---|
| `internal/controller/cache.go` | 计算缓存键、文件名和路径 | M1-T1 |
| `internal/controller/scripts.go` | init 脚本常量 | M1-T2 |
| `internal/controller/pod_builder.go` | `buildPodSpec(...)`、`applyPodSpec(...)` 等纯函数 | M1-T2 |
| `internal/controller/runtime.go` | `Backend` 接口与 `resolveBackend` | M3-T2 |
| `internal/controller/runtime_llamacpp.go`、`runtime_vllm.go` | 两个后端实现 | M3 |
| `internal/controller/gpu.go` | GPU 生效检测（日志扫描） | M2-T4 |
| `internal/controller/diagnose.go` | 把 Pod 状态映射为 Reason 的纯函数 | M4 |
| 对应的 `*_test.go` | 纯函数用标准 `testing` 写表驱动测试；涉及 apiserver 的放进现有 Ginkgo 套件 | 各任务 |

原则：Reconcile 只负责编排，所有「spec → 对象」的转换都写成可单测的纯函数。

## 4. 任务分解

### M1 缓存隔离与并发安全

**M1-T1 缓存键与路径（`cache.go`）**

```go
// cacheKeyFor 返回 sha256(source) 的前 12 个十六进制字符。
func cacheKeyFor(source string) string
// modelFileName 取 URL path 的 basename；不匹配 ^[A-Za-z0-9._-]{1,200}$ 时返回 "model.gguf"。
func modelFileName(source string) string
const modelMountPath = "/models"
func modelDirFor(m *llmv1alpha1.Model) string      // /models/<key>
func modelFilePathFor(m *llmv1alpha1.Model) string // /models/<key>/<file>
```

- 删除常量 `modelPath`，`serverArgs` 改用 `modelFilePathFor`。
- `ModelStatus` 新增字段 `CacheKey string`（`json:"cacheKey,omitempty"`），由 `updateStatus` 写入，方便人工到宿主机上找文件。
- 单测要覆盖：同一 source 得到同一 key；query string 不同则 key 不同（`?download=true` 和不带它视为两个源，需在文档中说明）；含 `%2F`、`..` 或 basename 为空时回落到 `model.gguf`。

**M1-T2 下载脚本、PodSpec 构建器与 F9 修复（`scripts.go`、`pod_builder.go`）**

init 容器 `model-downloader` 的环境变量：`MODEL_URL`、`MODEL_DIR`（即 `modelDirFor`）、`MODEL_FILE`（即 basename）、`EXPECT_FORMAT`（M1 固定为 `gguf`）。

```sh
set -eu
fail() { echo "$1: $2" > /dev/termination-log; echo "$1: $2" >&2; exit 1; }
mkdir -p "$MODEL_DIR" || fail DOWNLOAD_FAILED "mkdir $MODEL_DIR"
exec 9>"$MODEL_DIR/.lock"          # 锁文件永不删除（E1）
flock 9                            # BusyBox 无 -w，阻塞等待（F2）
F="$MODEL_DIR/$MODEL_FILE"
if [ -s "$F" ]; then echo "Cache hit $F"; exit 0; fi
find "$MODEL_DIR" -maxdepth 1 -name '*.tmp' ! -name "$MODEL_FILE.tmp" -exec rm -f {} +   # E3
T="$F.tmp"
if [ -f "$T" ]; then               # E4：续传前校验
  R=$(curl -fsIL "$MODEL_URL" | sed -n 's/^[Cc]ontent-[Ll]ength: *\([0-9]*\).*/\1/p' | tail -1)
  L=$(wc -c < "$T")
  if [ -z "$R" ] || [ "$L" -ge "$R" ]; then rm -f "$T"; fi
fi
curl -fL --retry 5 --retry-delay 3 --retry-all-errors \
     --speed-limit 1024 --speed-time 120 -C - -o "$T" "$MODEL_URL" \
  || fail DOWNLOAD_FAILED "curl exit $?"
if [ "$EXPECT_FORMAT" = gguf ] && [ "$(head -c 4 "$T")" != GGUF ]; then
  rm -f "$T"; fail FORMAT_MISMATCH "expected gguf, magic mismatch"
fi
mv "$T" "$F"
```

要点：

- 脚本本身的 BusyBox 兼容性已逐条实测（F12），不需要执行者再验证。
- `/dev/termination-log` 里的 `REASON: message` 格式是和 M4 的约定，不要改。
- 用 `const downloadScriptGGUF = ...` 定义脚本，单独做 golden 测试：脚本存到 `internal/controller/testdata/download_gguf.sh`，测试逐字比较（E33）。

`pod_builder.go` 完整代码（**含 F9 修复**——所有 apiserver 会默认化的字段必须显式写，否则 `CreateOrUpdate` 每轮都判定有差异并发多余 Update）：

```go
const (
	inferenceContainerName  = "llama-server"
	downloaderContainerName = "model-downloader"
	cachePrepContainerName  = "cache-prep"
)

func buildPodSpec(m *llmv1alpha1.Model) corev1.PodSpec {
	return corev1.PodSpec{
		Volumes:        []corev1.Volume{modelVolume()},
		InitContainers: initContainersFor(m),
		Containers:     []corev1.Container{inferenceContainer(m)},
		SchedulerName:  "default-scheduler", // M5 起改为透传用户值
	}
}

func initContainersFor(m *llmv1alpha1.Model) []corev1.Container {
	var cs []corev1.Container
	if usesHostPathCache() { // 与 modelVolume() 同一判定：环境变量 MODEL_CACHE_HOST_PATH 非空
		cs = append(cs, cachePrepContainer())
	}
	return append(cs, downloaderContainer(m)) // init 按序执行：先备目录再下载
}

func downloaderContainer(m *llmv1alpha1.Model) corev1.Container {
	return corev1.Container{
		Name:            downloaderContainerName,
		Image:           downloaderImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"sh", "-c"},
		Args:            []string{downloadScriptGGUF},
		Env: []corev1.EnvVar{
			{Name: "MODEL_URL", Value: m.Spec.Source},
			{Name: "MODEL_DIR", Value: modelDirFor(m)},
			{Name: "MODEL_FILE", Value: modelFileName(m.Spec.Source)},
			{Name: "EXPECT_FORMAT", Value: "gguf"}, // M3 起由后端提供
		},
		VolumeMounts: []corev1.VolumeMount{{Name: modelVolumeName, MountPath: modelMountPath}},
		// F9：TerminationMessage 两个字段是 apiserver 会补的默认值，必须显式
		TerminationMessagePath:   "/dev/termination-log",
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
}

func inferenceContainer(m *llmv1alpha1.Model) corev1.Container {
	return corev1.Container{
		Name:            inferenceContainerName,
		Image:           imageFor(m),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            serverArgs(m),
		Resources:       m.Spec.Resources,
		Ports: []corev1.ContainerPort{{
			Name: "http", ContainerPort: inferencePort, Protocol: corev1.ProtocolTCP,
		}},
		VolumeMounts:           []corev1.VolumeMount{{Name: modelVolumeName, MountPath: modelMountPath}},
		StartupProbe:           startupProbe(),
		ReadinessProbe:         readinessProbe(),
		TerminationMessagePath: "/dev/termination-log",
		TerminationMessagePolicy: corev1.TerminationMessageFile,
	}
}

// F9：探针四个计数字段全显式。startup 保持现值（5s×120）；readiness 用 apiserver
// 原本会补的默认值（10/3/1/3），这样升级后存量 Deployment 不发生滚动。
func startupProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/health", Port: intstr.FromString("http"),
		}},
		PeriodSeconds: 5, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: 120,
	}
}

func readinessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/health", Port: intstr.FromString("http"),
		}},
		PeriodSeconds: 10, TimeoutSeconds: 3, SuccessThreshold: 1, FailureThreshold: 3,
	}
}
```

`reconcileDeployment` 的 mutate 改为（**不要整体替换 PodSpec**，否则 `restartPolicy/dnsPolicy` 等 apiserver 默认值被剥掉，又回到每轮 Update）：

```go
_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
	labels := labelsFor(model)
	deployment.Labels = labels
	deployment.Spec.Replicas = pointerTo(replicasFor(model))
	deployment.Spec.ProgressDeadlineSeconds = pointerTo(int32(progressDeadline))
	deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	deployment.Spec.Template.ObjectMeta.Labels = labels
	applyPodSpec(&deployment.Spec.Template.Spec, buildPodSpec(model))
	return controllerutil.SetControllerReference(model, deployment, r.Scheme)
})

// applyPodSpec 只赋值本 operator 拥有的字段（F9）。
func applyPodSpec(dst *corev1.PodSpec, src corev1.PodSpec) {
	dst.Volumes = src.Volumes
	dst.InitContainers = src.InitContainers
	dst.Containers = src.Containers
	dst.RuntimeClassName = src.RuntimeClassName // apiserver 不默认化，nil 也是合法显式值
	dst.NodeSelector = src.NodeSelector         // M5 起由 spec 透传
	dst.Tolerations = src.Tolerations           // M5 起由 spec 透传
	dst.Affinity = src.Affinity                 // M5 起由 spec 透传
	dst.SchedulerName = src.SchedulerName       // 构建器永远写显式值
	if src.EnableServiceLinks != nil {          // M3 的 vLLM 用；nil 保留原值避免翻转
		dst.EnableServiceLinks = src.EnableServiceLinks
	}
}
```

**M1-T3 cache-prep（只在 hostPath 模式下启用）**

```go
func cachePrepContainer() corev1.Container {
	return corev1.Container{
		Name:  cachePrepContainerName,
		Image: downloaderImage,
		Command: []string{"chown", "100:101", modelMountPath},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: ptr(int64(0)), RunAsNonRoot: ptr(false),
			AllowPrivilegeEscalation: ptr(false),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"CHOWN"}},
		},
		ImagePullPolicy:           corev1.PullIfNotPresent, // F9
		TerminationMessagePath:    "/dev/termination-log",  // F9
		TerminationMessagePolicy:  corev1.TerminationMessageFile,
	}
}
```

- 直接以 `chown` 作为入口进程，不经过 `sh -c`，避免 E10 中能力在 execve 后丢失的问题。
- 只 chown 挂载根，不递归，也不创建子目录：root 只有 CHOWN、没有 DAC_OVERRIDE，若根目录不属于 root，root 在其中建目录会失败。子目录由 uid 100 的 downloader 自己创建。
- 副作用：宿主机上的缓存根会变成 uid 100 所有。README 需写明 `MODEL_CACHE_HOST_PATH` 应指向专用目录（例如 `/var/lib/minillmkube/models`），不要指向用户主目录。
- emptyDir 模式不加 cache-prep，现有用例 `Volumes[0].EmptyDir` 的断言保持不变。

**M1-T4 冲突重试**

把 `reconcileDeployment` 和 `reconcileService` 的调用分别包进 `retry.RetryOnConflict(retry.DefaultRetry, ...)`，包是 `k8s.io/client-go/util/retry`（E31）。

**M1-T5 测试改动**

- `model_controller_test.go:131-133`：期望的模型路径改为 `"/models/" + cacheKeyFor(modelSource) + "/Qwen3-0.6B-Q4_K_M.gguf"`。
- `model_controller_test.go:147` 的 hostPath 用例：断言 InitContainers 长度为 2，`[0].Name == "cache-prep"`，其 Capabilities 为 Drop ALL、Add CHOWN。
- 新增用例：两个 source 不同的 Model，渲染出的 `MODEL_DIR` 不同。
- 新增 F9 回归用例：Model Ready 后，给 Model 打一个无害 annotation 触发再一轮 reconcile，重新 Get Deployment，断言 `metadata.resourceVersion` 不变（CreateOrUpdate 返回 None、没有发 Update）。
- 新增 `scripts_test.go`（构建标签 `//go:build scriptlocal`，默认不进 `make test`）：用 `httptest.NewTLSServer` 提供文件，curl 加 `-k`。宿主机 `sh` 是 dash，只证明逻辑正确；BusyBox 兼容性已由 F12 覆盖，集群验收第 2~5 步会实测。

**M1 集群验收**

1. 设置 `MODEL_CACHE_HOST_PATH=/var/lib/minillmkube/models`（目录由用户 sudo 预建），执行 `make install run`。
2. 同时 apply 两个 Model，source 分别为 Q8_0 和 Q4_K_M。预期：两者都 Ready，宿主机上有两个 `<key>/` 目录，各自只有一个 `.gguf` 和 `.lock`。
3. 删除 Pod，新 Pod 的 init 日志出现 `Cache hit`。
4. 新建 Model，在下载过程中删除 Pod。预期新 Pod 续传，并且最终文件与远端 `sha256` 一致。
5. `replicas: 2`：只有一个 Pod 真正下载，另一个等锁后命中缓存。

### M2 GPU 推理（llama.cpp）

**M2-T1 API**（`model_types.go`）

```go
type GPUSpec struct {
    // +kubebuilder:default=1
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=1
    Count int32 `json:"count,omitempty"`        // 本阶段只支持单卡（E24）
    // +kubebuilder:default=99
    // +kubebuilder:validation:Minimum=0
    Layers int32 `json:"layers,omitempty"`      // 对应 --n-gpu-layers
}
// ModelSpec 新增：
GPU              *GPUSpec `json:"gpu,omitempty"`
RuntimeClassName *string  `json:"runtimeClassName,omitempty"`
ExtraArgs        []string `json:"extraArgs,omitempty"`
```

- 删除 `Image` 上的 `+kubebuilder:default` marker（F6），字段注释改为「为空时由后端按 GPU 选择默认镜像」。
- 兼容性说明（写进发布说明）：存量 CR 的 `spec.image` 已被持久化为 CPU 镜像，启用 GPU 时需要用户清空该字段或改成 CUDA 镜像。
- `Layers` 默认 99 在当前镜像上并非必需（F10：默认已全量 offload），但显式传值保证在其它构建/版本上行为一致。

**M2-T2 渲染逻辑**（`pod_builder.go`）

```go
func wantsGPU(m *Model) bool  // m.Spec.GPU != nil，或 Resources.Limits 中含 "nvidia.com/gpu"
func resourcesFor(m *Model) corev1.ResourceRequirements
// 若 GPU != nil 且用户没写 nvidia.com/gpu，则在 Limits 里补上 Count；用户已写的值不覆盖。先 DeepCopy，不能改动 m。
func imageFor(m *Model) string // Spec.Image 非空就用它；否则 GPU 用 ":server-cuda"，CPU 用 ":server"
func runtimeClassFor(m *Model) *string
// 优先 Spec.RuntimeClassName；否则在 wantsGPU 时读 operator 环境变量 GPU_RUNTIME_CLASS（本机为 nvidia）；都没有就返回 nil
func hasFlag(args []string, names ...string) bool // 同时识别 "--x v" 与 "--x=v"；E16/F14
```

- `serverArgs` 在 GPU 模式下追加两段（都要先过 `hasFlag`，用户在 ExtraArgs 里写过就不追加）：
  - `--n-gpu-layers <Layers>`（别名 `-ngl`、`--gpu-layers`，F14）；
  - `-lv 5`（别名 `--verbosity`、`-v`；F10——这是 M2-T4 检测的信号来源，日志量代价见 F10，只在启动阶段）。
- 之后再追加 `ExtraArgs`（用户参数永远最后，保证能覆盖 operator 默认值）。
- `buildPodSpec` 里设 `RuntimeClassName: runtimeClassFor(m)`；`inferenceContainer` 的 `Resources` 改用 `resourcesFor(m)`。

**M2-T3 测试**

- `pod_builder_test.go` 表驱动，覆盖：无 GPU、`gpu{}`、只写 limits、用户自带 `-ngl 20`、用户自带 `-lv 4`、显式 image、显式 runtimeClass、环境变量兜底。每种情况断言镜像、Limits、RuntimeClassName，以及 `--n-gpu-layers` 和 `-lv` 在参数中出现的次数（各自 0 或 1）。
- envtest：创建带 `gpu: {}` 的 Model，断言 Deployment 中 `RuntimeClassName == nil`（envtest 没设环境变量），且 `Limits["nvidia.com/gpu"] == 1`。

**M2-T4 GPU 生效检测（方案已定型，依据 F10 的 D1/D5 探针，无需再探查）**

1. `ModelReconciler` 新增字段，`cmd/main.go:181` 构造处注入：

```go
Clientset kubernetes.Interface // k8s.io/client-go/kubernetes；import 别名 kubernetes
```

main.go 里：`kubernetes.NewForConfig(mgr.GetConfig())`。

2. RBAC marker（加到 `model_controller.go` 顶部 marker 区，M4-T2 会复用，勿重复添加）：

```go
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
```

3. `gpu.go`：

```go
var gpuDeviceRe = regexp.MustCompile(`llama_prepare_model_devices: using device (CUDA[0-9]+|CPU)`)

// parseGPUDevice 纯函数（表驱动测试的核心）：
// 命中 CUDA0 → ("CUDA0", true)；命中 CPU → ("CPU", true)；无信号 → ("", false)。
func parseGPUDevice(logText string) (string, bool)

// scanGPUOffload 在 rollout Ready 后调用一次：List 属于该 Model 的 Pod，取第一个
// Running 的推理 Pod，读取日志（PodLogOptions{TailLines: 1000, LimitBytes: 512KiB}，E34），
// 交给 parseGPUDevice。日志读失败返回 error，不猜测。
func (r *ModelReconciler) scanGPUOffload(ctx context.Context, m *llmv1alpha1.Model) (string, bool, error)
```

4. condition 写入规则（名字 `GPUOffload`，Reason 单一来源，E30）：

| 扫描结果 | Status / Reason | message |
|---|---|---|
| `CUDA\d+` | True / `OffloadDetected` | 引用命中的整行日志 |
| `CPU` | False / `CPUFallback` | 引用整行；这是 D5 实测到的明确签名，允许写 False（E20 的「拿不准不写 False」指无信号时） |
| 无信号 / 读日志失败 / 非 llamacpp 后端 | Unknown / `NotDetected` | 说明原因 |

5. 防重复扫描：reconciler 内存 map（`map[types.NamespacedName]types.UID`）记录已扫 Pod；Pod 重建（UID 变化）后重扫；operator 重启后重扫无害。

**M2 集群验收**

1. 运行 operator 前设置 `GPU_RUNTIME_CLASS=nvidia`。
2. apply 带 `gpu: {}` 的 Model，等到 Ready。
3. `kubectl exec <pod> -- nvidia-smi` 能看到 RTX 5070 Ti；`kubectl get model <名> -o jsonpath='{.status.conditions[?(@.type=="GPUOffload")}]'` 为 True、message 含 `CUDA0`。
4. 复现静默回退（E20/D5 场景）：同一 Model 改 `spec.image: ghcr.io/ggml-org/llama.cpp:server`（CPU 镜像）并保留 `gpu: {}`，Ready 后 GPUOffload=False、Reason=CPUFallback。
5. 通过 port-forward 调用 `/v1/completions`，生成速度应大于 200 tok/s。本机实测基准为 269~356 tok/s，CPU 约 26.5 tok/s。
6. 删除 `gpu` 字段：Pod 重建为 CPU 镜像，不再请求 GPU，`-lv 5` 也不再追加。

### M3 双引擎与 `runtime: auto`

**M3-T0 vLLM 本机可行性探查（必须先做，结论写回本节）**

先手动起一个 Pod 验证，不经过 operator：`runtimeClassName: nvidia`、`nvidia.com/gpu: 1`，**镜像固定 `vllm/vllm-openai:v0.32.0-cu129`**（F13：稳定版 v0.32.0；cu129=CUDA 12.9 ≤ 本机驱动 13.1；**不要用 latest 或 nightly**——nightly 是 cu134 构建，驱动不够）。模型选 `Qwen/Qwen3-0.6B`（safetensors，经 `HF_ENDPOINT=https://hf-mirror.com` 下载），环境变量 `VLLM_WSL2_ENABLE_PIN_MEMORY=1`，`/dev/shm` 挂内存 emptyDir。镜像约 20GB，经 daocloud 镜像拉取（0.6 节），预留 30~60 分钟。

需要记录（按序）：

1. **第一个验证点：cu129 构建是否包含 Blackwell（sm_120）内核**——启动日志里找 `sm_120` 或 "no kernel image" 报错。
2. 镜像拉取耗时与实际大小。
3. 能否在 Blackwell + WSL2 上启动；冷启动到 `/health` 返回 200 的时间（作为 `FailureThreshold` 依据）。
4. 镜像 entrypoint 与完整参数名（`--model --served-model-name --host --port --max-model-len --gpu-memory-utilization` 是否如 M3-T4 所列）。
5. 是否自带 `huggingface-cli` 或 `hf` 命令（决定下载器能否复用该镜像）。

**若启动失败，M3 暂停 vLLM 部分**，只做 T1~T3 的抽象层与 llama.cpp 后端，并在第 6 章记录失败证据。

**M3-T1 API**

```go
// +kubebuilder:validation:Enum=auto;llamacpp;vllm
// +kubebuilder:default=auto
Runtime string `json:"runtime,omitempty"`
// +kubebuilder:validation:Enum=auto;gguf;safetensors
// +kubebuilder:default=auto
Format string `json:"format,omitempty"`
// Status 新增：ResolvedRuntime、ResolvedFormat（string）
```

在 `ModelSpec` 上加 CEL 规则（E21；默认值先于 CEL 应用，`self.runtime` 永远非空）：

```go
// +kubebuilder:validation:XValidation:rule="!(self.runtime == 'vllm' && self.format == 'gguf')",message="vllm does not support gguf in this operator"
// +kubebuilder:validation:XValidation:rule="!(self.runtime == 'llamacpp' && self.format == 'safetensors')",message="llamacpp requires gguf"
```

- 新增 printcolumn `Runtime`，取 `.status.resolvedRuntime`。
- 「auto 表示未指定」是有意设计，解析结果只写进 status，不回写 spec（E29）。

**M3-T2 后端接口**（`runtime.go`）

```go
type Backend interface {
    Name() string                                    // "llamacpp" | "vllm"
    DefaultImage(gpu bool) string
    Args(m *Model, modelPath string) []string        // 含 ExtraArgs 去重（E16）与 -lv 5 逻辑（F10）
    Env(m *Model) []corev1.EnvVar
    HealthPath() string                              // 两者均为 "/health"
    StartupFailureThreshold() int32                  // llamacpp 120（×5s），vllm 由 T0 实测决定
    ModelPath(m *Model) string                       // llamacpp 指向文件，vllm 指向目录
    Downloader(m *Model) corev1.Container
    PodMutations(spec *corev1.PodSpec, m *Model)     // shm、enableServiceLinks 等
}
func resolveBackend(m *Model) (Backend, string /*format*/, error)
```

解析规则（纯函数，表驱动测试）：

1. 显式 `runtime` 优先，`format` 为 auto 时按 runtime 推断。
2. `runtime=auto` 时：`format=gguf` 或 source 是 https 且以 `.gguf` 结尾 → llamacpp；`format=safetensors` 或 source 以 `hf://` 开头 → vllm；其它返回错误，Reason 为 `InvalidSpec`。
3. `validateSpec` 改为调用 `resolveBackend`，再按格式校验 source：gguf 要求 https 和 `.gguf` 后缀；safetensors 要求 `hf://<org>/<repo>`，可带 `@<revision>`。

**M3-T3 llama.cpp 后端**（`runtime_llamacpp.go`）

- 把 `serverArgs` 和 M2-T2 的 GPU 逻辑（`--n-gpu-layers`、`-lv 5`）迁移进来。
- 改 `--host ::`（E18），加 `--alias <CR 名>`（E17，F14 已确认 flag 存在）。
- 迁移后现有全部测试必须不改语义通过；允许的改动只有参数断言中的 host 值和新增的 alias 断言。

**M3-T4 vLLM 后端**（`runtime_vllm.go`）

- 参数：`--model <dir> --served-model-name <CR 名> --host :: --port 8080 --max-model-len <ctx> --gpu-memory-utilization 0.85`。以 T0 第 4 项结论为准修正。
- 环境变量：只在 operator 设置了 `ENABLE_WSL2_VLLM_PIN_MEMORY=true` 时才注入 `VLLM_WSL2_ENABLE_PIN_MEMORY=1`（E22）。
- PodMutations：
  - `/dev/shm` 挂 `emptyDir{Medium: Memory, SizeLimit: 内存 limit 的一半，无 limit 时 1Gi}`（E14）。
  - `EnableServiceLinks: ptr(false)`（E15；`applyPodSpec` 只在非 nil 时赋值，语义正确）。
- 下载器：若 T0 确认 vLLM 镜像自带 hf CLI，则复用该镜像；脚本同样先 flock，再下载到 `<key>/`，最后检查 `config.json` 和 `*.safetensors` 都存在，否则报 `FORMAT_MISMATCH`。`HF_ENDPOINT` 由 operator 环境变量传入。
- 容器名统一用常量 `inferenceContainerName = "llama-server"`（M1-T2 已引入），改名会触发滚动更新，本阶段不改。

**M3 验收**

- envtest 中 CEL 拒绝 `vllm + gguf`。
- gguf URL 解析为 `ResolvedRuntime=llamacpp`；`hf://Qwen/Qwen3-0.6B` 解析为 `vllm`。
- 集群上两种 Model 都能用 OpenAI 接口回答。单卡环境下两者串行验证（E24）。

### M4 故障可观测

**M4-T1 诊断纯函数**（`diagnose.go`，可与 M1 并行开发）

```go
type diagnosis struct{ reason, message string }
func diagnosePods(pods []corev1.Pod) *diagnosis // 无异常时返回 nil
```

按以下优先级取第一个命中项，message 截断到 512 字节：

| 优先级 | 信号 | Reason |
|---|---|---|
| 1 | init `model-downloader` 的 `LastTerminationState` 或 `State.Terminated`，且 message 以 `FORMAT_MISMATCH:` 开头 | `FormatMismatch` |
| 2 | 同一个容器，exitCode≠0 | `DownloadFailed`，message 取 termination message |
| 3 | 任一容器 `Waiting.Reason` 为 `ErrImagePull`、`ImagePullBackOff` 或 `InvalidImageName` | `ImagePullFailed` |
| 4 | PodScheduled=False 且 Reason 为 `Unschedulable`，message 含 `nvidia.com/gpu` | `InsufficientGPU` |
| 5 | 同上，但 message 不含 `nvidia.com/gpu` | `Unschedulable` |
| 6 | 推理容器 `Waiting.Reason` 为 `CrashLoopBackOff` | `RuntimeCrashing`，message 附 lastState 的 exitCode |

**M4-T2 接入 Reconcile**

- 在 `evaluateRollout` 之后执行 `r.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels(labelsFor(m)))`。如果 rollout 尚未 Ready 且 `diagnosePods` 有结果，就用它覆盖 state：phase 设为 `Failed`，condition `Ready=False`，Reason 取诊断结果。
- `SetupWithManager` 加 `Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(...))`，按 label `app.kubernetes.io/instance` 映射到同名 Model，并用 predicate 只放行 `managed-by=minillmkube` 的 Pod。
- RBAC：pods 的 get/list/watch 已在 M2-T4 加过，**不要重复**；补 events 的 marker：

```go
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
```

- Events：`ModelReconciler` 新增字段 `Recorder events.EventRecorder`（`k8s.io/client-go/tools/events`）。`cmd/main.go:181` 注入 `mgr.GetEventRecorder("model-controller")`（F7）。只在 Ready condition 的 Reason 发生变化时发 Warning，避免事件风暴（E30）。
- 测试：`events.NewFakeRecorder(64)`（F11，已确认存在于 client-go v0.35.0，`Events` 是 `chan string`），断言 Reason 变化时恰好发出一条事件、不变化时不发。

**M4 验收（集群上故意制造故障）**

| 注入的故障 | 期望 Reason |
|---|---|
| source 指向一个 404 的 `.gguf` | `DownloadFailed` |
| source 指向一个以 `.gguf` 结尾的 HTML 页面 | `FormatMismatch` |
| `image: ghcr.io/ggml-org/llama.cpp:nope` | `ImagePullFailed` |
| 两个 `gpu: {}` 的 Model 同时存在 | 后一个为 `InsufficientGPU` |
| `extraArgs: ["--bogus"]` | `RuntimeCrashing` |

`diagnose_test.go` 用构造的 `corev1.Pod` 覆盖每一行，再加一条「多个信号同时出现时取优先级高者」。

### M5 放置与调度透传

`ModelSpec` 新增以下字段，`buildPodSpec` 原样写进 PodSpec（经 `applyPodSpec` 赋值，F9）：

```go
NodeSelector  map[string]string   `json:"nodeSelector,omitempty"`
Tolerations   []corev1.Toleration `json:"tolerations,omitempty"`
Affinity      *corev1.Affinity    `json:"affinity,omitempty"`
SchedulerName string              `json:"schedulerName,omitempty"`
```

- `buildPodSpec` 里 `SchedulerName` 改为：用户值非空用它，否则 `"default-scheduler"`（永远显式，F9）。
- 这些字段只进 PodSpec，不进 `labelsFor` 和 selector（E28）。
- envtest 断言 Deployment 原样透传这四个字段。
- 集群上设置 `schedulerName: does-not-exist`，Pod 应保持 Pending 且没有 Scheduled 事件——说明调度已交给外部调度器，为自研调度插件预留入口。

## 5. 依赖与并行

- 主线依赖：M1 → M2 → M3。M3 的 vLLM 目录缓存依赖 M1 的按源分目录；M3-T2 的接口收编 M2-T2 的全部渲染逻辑。
- 可并行：
  - M4-T1（纯函数）可与 M1 同时开发。
  - M4-T2 依赖 M1-T2 定下的 termination-log 约定；pods 的 RBAC marker 已在 M2-T4 添加，M4-T2 勿重复。
  - M5 不依赖其它里程碑，任何时候可做。
  - M3-T0（vLLM 探查）只依赖集群和镜像拉取，可与 M1 并行；20GB 镜像拉取期间不占 GPU。
- 发布影响：M1 改变缓存路径，存量 Pod 会重建并重新下载一次；M1-T2 显式化默认字段后，存量 Deployment 的存储模板与现在完全一致（readiness 探针数值取自 apiserver 默认值），不会引发额外滚动；M2 删除 Image 的 default，存量 CR 需要用户处理。三点都写进发布说明。

## 6. 进度表（执行者更新）

| 任务 | 状态 | 分支/提交 | 备注 |
|---|---|---|---|
| M1-T1 缓存键与路径 | 已完成 | feat/m1 df6b7ec | `status.cacheKey` 已加；文件名规则 `^[A-Za-z0-9._-]{1,200}$`，不匹配回落 `model.gguf` |
| M1-T2 脚本+构建器+F9 修复 | 已完成 | feat/m1 6981a49 | 含 F9 回归用例；golden `internal/controller/testdata/download_gguf.sh`；`make test` 不含脚本自测（`-tags scriptlocal` 单独跑） |
| M1-T3 cache-prep | 已完成 | feat/m1 5f03af1 | 仅 hostPath 模式；`chown 100:101 /models` 作入口进程（E10）；README 已写明专用目录 |
| M1-T4 冲突重试 | 已完成 | feat/m1 7c9948e | `retry.RetryOnConflict(retry.DefaultRetry, ...)` 包住 Deployment 与 Service |
| M1-T5 测试改动 | 已完成 | feat/m1 612c270 | 含「两个 source → 两个 MODEL_DIR」与 F9 二次 reconcile 不重写 Deployment |
| M2-T1 API | 已完成 | feat/m1 0e9c8d5 | 已删 image 的 `+kubebuilder:default`（F6）；`gpu.count` 默认 1/上限 1，`gpu.layers` 默认 99 |
| M2-T2 渲染逻辑 | 已完成 | feat/m1 4c1fac6 | limits 同步镜像进 requests，否则存储模板与渲染结果永不相等（F9）；`-lv 5` 已加（F10） |
| M2-T3 测试 | 已完成 | feat/m1 d042e51 | pod_builder 表驱动 12 例 + envtest（gpu:{} → limits=1、RuntimeClass=nil、`--n-gpu-layers 99`） |
| M2-T4 GPU 生效检测 | 已完成 | feat/m1 9903109 | `GPUOffload` 条件 + 按 Pod UID 缓存观测；日志读失败记 Unknown 并打日志，不阻断 reconcile |
| M3-T0 vLLM 探查 | 未执行 | | 无集群，未做手动 Pod 探查；镜像 tag 仍固定 `vllm/vllm-openai:v0.32.0-cu129`，`startupFailureThreshold` 取估计值 240 |
| M3-T1 API+CEL | 已完成 | feat/m3-m4 111fde3 | runtime/format 字段、两条 CEL、status.resolved*、printcolumn Runtime |
| M3-T2 后端接口 | 已完成 | feat/m3-m4 94453fe | `runtime.go` 的 Backend 接口与 resolveBackend；validateSpec 改为按解析出的格式校验 source |
| M3-T3 llama.cpp 后端 | 已完成 | feat/m3-m4 94453fe | 迁移 serverArgs 与 GPU 逻辑；绑 `::`；`--alias <CR 名>` |
| M3-T4 vLLM 后端 | 已完成 | feat/m3-m4 94453fe | 参数/Env/PodMutations/下载脚本；`/dev/shm` 内存卷与 `enableServiceLinks=false` |
| M4-T1 诊断纯函数 | 已完成 | feat/m3-m4 f84b17a | `diagnose.go` 六级优先级 + 512 字节截断；单测覆盖每级与优先级 |
| M4-T2 接入 Reconcile | 已完成 | feat/m3-m4 1c16129 | Pod 诊断覆盖状态、Pod watch、Reason 变化发 Warning；envtest 覆盖诊断与事件 |
| M5 透传字段 | 已完成 | feat/m3-m4 e6ee5bd | `SchedulerName` 留空仍写 `default-scheduler`（F9）；放置字段不进 selector（E28）；外部调度器验收留待上集群 |

M1、M2、M3、M4 的「集群验收」步骤均未执行（用户要求暂不上集群），只完成到 `make test` / `make lint` 全绿。验收前需要按各任务节点核对一遍真实集群行为；M3-T0 的 vLLM 可行性探查也因无集群未做。

## 7. 边界问题清单

每条格式：触发条件 → 现象 → 对策。来源标注 LLMKube issue 号或本项目实测。

### A. 下载与缓存

| 编号 | 触发条件与现象 | 对策 | 来源 |
|---|---|---|---|
| E1 | 两个 Pod 共用同一缓存目录，各自 `curl -C -` 从自己的偏移续写同一 partial，文件交错损坏。被终止 Pod 的 curl 在 grace period 内仍在写，与新 Pod 重叠。 | 每个模型独立目录；用 `flock` 锁 fd 9，子进程继承、内核在进程退出时回收；锁文件永不删除（删除后下一个 Pod 会锁到新 inode，锁失效）。 | LLMKube #2110 |
| E2 | 缓存放在 NFS 等网络卷，`flock` 需要 lock daemon，否则失败或无效。 | 文档要求缓存卷支持文件锁；取锁失败要让 init 失败，不能静默跳过。 | LLMKube 错误信息 |
| E3 | 下载中断留下 `*.tmp`，反复失败后堆积占盘。 | 每次下载前无条件清扫同目录 `*.tmp`。 | LLMKube #1435 |
| E4 | 续传时 partial 大小与远端 Content-Length 不符。 | 续传前先 HEAD 校验，不符则删除重来。等大小的交错只能靠 E1 的锁预防。 | LLMKube partialSizeGuard |
| E5 | 上游在同一 URL 重新发布了不同内容，按 URL 哈希命中旧缓存。 | v1 在文档说明此行为；v2 把 ETag/Content-Length 纳入缓存键。 | 本项目设计取舍 |
| E6 | `spec.source` 变更后，旧 source 的元数据或文件仍被使用。 | 缓存键含 source，变更即新目录；status 里的 resolved 值随 generation 重新解析。 | LLMKube #1767 |
| E7 | HF 仓库同时含 `.gguf` 与 `.safetensors`，格式判断出现歧义。 | 固定优先级：用户显式 `format` > 目录布局 > 后缀；结果写进 status。 | 设计约束 |
| E8 | init 镜像缺少 `flock`、`sha256sum` 等工具，脚本在运行时才失败。 | 固定镜像 tag；换镜像时在 CI 里检查这些工具存在。 | LLMKube resume 测试对工具存在性的检查 |
| E9 | 大模型在慢网络下超过 `progressDeadlineSeconds`（现为 1800s），被判失败，实际仍在下载。 | 按后端设置 deadline；下载进度或超时原因写入 status。 | 本项目 |

### B. 权限与 Pod 安全

| 编号 | 触发条件与现象 | 对策 | 来源 |
|---|---|---|---|
| E10 | 非 root 容器用 `capabilities.add` 加 `CHOWN`,再通过 `sh -c chown` 执行。非 root 进程在 `execve` 后丢失 capabilities，除非运行时设置了 ambient caps。kind 上通过,别的集群 `EPERM`。 | `cache-prep` 以 uid 0 运行，`Drop ALL` 只加 `CHOWN`；或把系统调用放进容器入口进程本身。测试里要覆盖运行时行为，不能只检查渲染出的 spec。 | LLMKube #887、AGENTS.md |
| E11 | 命名空间启用 restricted PSA，非 root 的 downloader 写不了 root 属主的缓存根。 | 同 E10，用 cache-prep 把缓存根 chown 给下载用户。 | LLMKube #2057 |
| E12 | emptyDir 权限宽松，掩盖了 hostPath/PVC 上的 uid 100 写权限问题。 | e2e 必须包含 hostPath 与 PVC 用例。 | 本项目实测 |
| E13 | 新增的 Service 端口在集群内任意 Pod 都可访问，推理接口默认无鉴权。 | 文档声明默认安全姿态；后续补 NetworkPolicy 样例。 | LLMKube AGENTS.md |

### C. 引擎与运行时

| 编号 | 触发条件与现象 | 对策 | 来源 |
|---|---|---|---|
| E14 | 容器运行时默认 `/dev/shm` 只有 64MiB，vLLM 多进程启动即失败。 | 挂 `emptyDir{medium: Memory}` 到 `/dev/shm`，大小取内存 limit 的一半；单卡也加，作为保险。 | LLMKube #2099 |
| E15 | vLLM 新版会校验 `VLLM_*` 环境变量。同命名空间内其它 vLLM Service 注入的 service-link 变量会被判为未知变量并告警。 | Pod 设 `enableServiceLinks: false`。 | LLMKube 运行时代码 |
| E16 | operator 自动追加的 flag 与用户 `extraArgs` 重复，如 `--metrics --metrics`。 | 追加前先检查用户参数是否已含该 flag。 | LLMKube #1384 |
| E17 | llama.cpp 的 `/v1/models` 返回磁盘路径而不是模型名。 | 用 `--alias`（llama.cpp）或 `--served-model-name`（vLLM）固定为 CR 名。 | LLMKube #1894 |
| E18 | 绑定地址写 `0.0.0.0`,在双栈集群里 IPv6 访问不到。 | 默认绑定 `::`。SGLang 有例外,本阶段不涉及。 | LLMKube #972/#973 |
| E19 | 进程存活、端口开放，但引擎已卡死不处理请求（stall）。 | startup probe 的 `failureThreshold` 按后端放宽（vLLM 冷启动以分钟计）；M4 之后再评估 stall 检测。 | LLMKube 有独立的 vLLM/SGLang stall 检测文件 |
| E20 | 申请了 GPU 却静默跑在 CPU 上，速度低一个数量级却无报错（F10 实测：默认日志下 GPU 与 CPU 完全同日志，回退无任何报错）。 | M2-T4 定型方案：GPU Pod 追加 `-lv 5`，日志扫描 `llama_prepare_model_devices`；CUDA→True，CPU→False（D5 有明确 CPU 行），无信号→Unknown。 | LLMKube #1197 |
| E21 | vLLM 加载 GGUF 属实验性，还需 `vllm-gguf-plugin` 与独立 tokenizer；某些架构 vLLM 版本不支持。 | CEL 拒绝 `vllm + gguf`；不在 controller 里硬编码支持列表（随版本变化），以启动失败信息作最终裁决并透传到 status。 | vLLM 官方文档 |
| E22 | WSL2 下 vLLM 报 `UVA is not available`。 | 注入 `VLLM_WSL2_ENABLE_PIN_MEMORY=1`，仅在 WSL 环境生效。 | 同款笔记本用户报告，本机未验证 |
| E23 | 12GB 显存中 Windows 桌面已占约 1.2GB；vLLM 默认显存占比过高会 OOM。 | 显式设置 `--gpu-memory-utilization`（默认 0.85）；OOM 归入 `RuntimeCrashing` 并在 message 提示。 | 本机 `nvidia-smi` 观测 |
| E24 | device plugin 在 WSL 只上报 1 个 `all` 设备，两个 GPU Pod 会有一个 Pending；无 MIG/多卡隔离可测。 | e2e 串行；Pending 归入 `InsufficientGPU`。 | 本机实测 |
| E25 | 端口、模型路径等常量散落在多处，改一处漏一处。 | 端口与路径只在 backend 内定义一处；Service 对外统一 8080。 | 本项目设计约束（LLMKube #1366 涉及 containerPort 的处理，细节未逐条核对） |
| E26 | 改缓存路径或参数会触发滚动重建，在途请求中断。 | v1 不做 drain，在发布说明中写明；后续再评估。 | LLMKube 有 drain-before-rollout |
| E27 | 使用 DRA 等独占资源时，滚动更新的新 Pod 拿不到被旧 Pod 占着的资源，死锁。 | 本阶段不引入 DRA；单卡场景若出现，使用 `Recreate` 策略。 | LLMKube #192 |

### D. CRD 与控制器

| 编号 | 触发条件与现象 | 对策 | 来源 |
|---|---|---|---|
| E28 | Deployment 的 selector 不可变。若把可变字段（如将来的模型引用）放进 selector，修改后更新被静默卡住。当前 `labelsFor` 只含 name、instance(=CR 名)、managed-by，CR 名不可变，所以目前安全。 | 在 `labelsFor` 旁加注释：selector 只允许使用不可变标签；新增可变字段只能放 Pod 模板标签。并加测试断言 selector 不含新字段。 | LLMKube #301；本项目 `model_controller.go:347` |
| E29 | CRD 里给 `runtime` 设默认值会吞掉「用户没写」这一语义。 | `auto` 本身就表示未指定，是有意设计；解析结果只写 status，不回写 spec。 | LLMKube #525 |
| E30 | 每次 reconcile 都覆盖 status，会冲掉其它路径写入的原因或造成事件风暴。 | status 无变化不更新；各 Reason 的写入点单一。 | LLMKube #777 |
| E31 | 更新 Deployment 时乐观锁冲突（409）。 | 使用 `retry.RetryOnConflict`。 | 本项目实测（约第 105 行） |
| E32 | spec 变了但 status 中的 resolved 值仍是旧的。 | resolved 值用 `metadata.generation` 判断失效并重新解析。 | LLMKube #1813 |
| E33 | init 脚本由 Go 拼接字符串、由 shell 执行，两端各自测试容易漏掉真实行为。 | 脚本抽成常量并做 golden 测试；用真实字节验证，不用消费方一侧构造的替身。 | LLMKube AGENTS.md |
| E34 | 读 Pod 日志判断 offload，行很长或日志很大时耗内存与时间。 | 限制 `TailLines` 与单行字节数。 | LLMKube 日志读取实现 |
| E35 | 手改生成文件、或样例与 CRD 漂移。 | 只改源码并 `make manifests generate`；CI 检查生成物无差异。 | LLMKube AGENTS.md |

### E. WSL 与单机环境

| 编号 | 触发条件与现象 | 对策 | 来源 |
|---|---|---|---|
| E36 | 新版 NVIDIA Container Toolkit 已让 k3s 自动识别 nvidia runtime；再手写 `config.toml.tmpl` 追加同名 runtime 表，会产生重复 TOML 表而使 containerd 起不来。 | 不手写模板，只确认 `RuntimeClass` 存在；改动前备份并保证可回滚。 | 本项目实测教训 |
| E37 | Pod 出网依赖 `.wslconfig` 的 `firewall=false`，换机器或网络模式后下载全部失败。 | 把出网检查列为环境验收项；init 失败时 message 保留 curl 的退出原因。 | 本项目实测 |
| E38 | Docker Desktop 不随系统自启，kind 用例会因 docker 不可用而失败。 | e2e 脚本先探测 docker，缺失时给出明确提示后退出。 | 本项目实测 |

## 8. 暂缓事项

- Model / InferenceService 拆分：等出现同一模型被多个服务复用的真实需求。
- HPA、多卡张量并行、多机推理、LoRA、Router。
- stall 检测、请求 drain、NetworkPolicy 与鉴权（E13、E19、E26）。
- 自研调度插件：等 M5 字段稳定后在 kind 三节点上开发。

## 9. 已知风险与未验证项

- vLLM 在本机（Blackwell + WSL2）的实际可用性尚未验证，E22 的结论来自第三方报告。
- vLLM 镜像体积约 20GB，首次拉取时间与磁盘占用需实测。
- 本文中的耗时均为估算，不是承诺。
- `-lv 4` 能否在保留检测行的同时把启动日志从约 4300 行降下来（未测；非必需优化，M2-T4 用已实测的 5）。
- apiserver 对「内容无变化的 Update」是否跳过写入与 resourceVersion 递增（F9 的推断部分；可用 M1-T5 的 resourceVersion 稳定性用例间接验证）。
- vLLM v0.32.0 的 cu129 构建是否包含 Blackwell（sm_120）CUDA 内核（M3-T0 第一个验证点）。
