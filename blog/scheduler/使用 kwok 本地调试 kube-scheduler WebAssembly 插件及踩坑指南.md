## 使用 kwok 本地调试 kube-scheduler WebAssembly 插件及踩坑指南

### 使用 kwok 创建 k8s 集群

#### 安装kwok

Mac 环境

```bash
✗ brew install kwok
```

更多安装 kwok 方式请参考[官方安装文档](https://kwok.sigs.k8s.io/docs/user/installation/)

安装完以后查看 kwokctl 命令使用说明

```bash
✗ kwokctl --help
kwokctl is a tool to streamline the creation and management of clusters, with nodes simulated by kwok

Usage:
  kwokctl [command] [flags]
  kwokctl [command]

Available Commands:
  completion  Generate the autocompletion script for the specified shell
  config      Manage [reset, tidy, view] default config
  create      Creates one of [cluster]
  delete      Deletes one of [cluster]
  etcdctl     etcdctl in cluster
  export      Exports one of [logs]
  get         Gets one of [artifacts, clusters, components, kubeconfig]
  hack        [experimental] Hack [get, put, delete] resources in etcd without apiserver
  help        Help about any command
  kubectl     kubectl in cluster
  logs        Logs one of [audit, etcd, kube-apiserver, kube-controller-manager, kube-scheduler, kwok-controller, dashboard, metrics-server, prometheus, jaeger]
  scale       Scale a resource in cluster
  snapshot    Snapshot [save, restore, record, replay, export] one of cluster
  start       Start one of [cluster]
  stop        Stop one of [cluster]

Flags:
  -c, --config strings   config path (default [~/.kwok/kwok.yaml])
      --dry-run          Print the command that would be executed, but do not execute it
  -h, --help             help for kwokctl
      --name string      cluster name (default "kwok")
  -v, --v log-level      number for the log level verbosity (DEBUG, INFO, WARN, ERROR) or (-4, 0, 4, 8) (default INFO)
      --version          version for kwokctl

Use "kwokctl [command] --help" for more information about a command.
```

总结一下 kwokctl 命令主要功能：

- 集群管理：创建、删除、启动、停止、获取集群列表
- kubectl 命令平替
-  集群资源扩所容，这里不仅可以操作 pod，还能操作 node



#### 创建 k8s 集群

##### 最快的方式

使用 kwok 默认参数创建集群，只需要一条命令。

```bash
✗ kwok create cluter
```

##### 自定义集群配置

使用 kwok 配置文件创建集群

```bash
✗ kwok create cluter -c ./kwok.yaml
```

kwork 配置文件配置定义文档参考：https://kwok.sigs.k8s.io/docs/generated/apis/



创建出来的 k8s 集群默认是最新版本，包括 kube-apiserver、kube-scheduler、kube-controller-manager、etcd 和 kwok-controller等组件，这些组件都是使用 docker 启动。

```bash
✗ docker ps
CONTAINER ID   IMAGE                                                                  COMMAND                  CREATED      STATUS         PORTS                                                   NAMES
ec9ca844177f   registry.k8s.io/kube-scheduler:v1.27.3                              "kube-scheduler --co…"   5 days ago   Up 4 minutes                                                           kwok-kwok-kube-scheduler
47064aa667ae   registry.k8s.io/kwok/kwok:v0.6.0                                       "kwok --manage-all-n…"   5 days ago   Up 4 minutes                                                           kwok-kwok-kwok-controller
a58c84571d22   registry.k8s.io/kube-controller-manager:v1.27.3                        "kube-controller-man…"   5 days ago   Up 4 minutes                                                           kwok-kwok-kube-controller-manager
dbbe08789990   registry.k8s.io/kube-apiserver:v1.27.3                                 "kube-apiserver --et…"   5 days ago   Up 4 minutes   0.0.0.0:32766->6443/tcp                                 kwok-kwok-kube-apiserver
86e17212af9a   registry.k8s.io/etcd:3.5.11-0                                          "etcd --name=node0 -…"   5 days ago   Up 4 minutes   2380/tcp, 4001/tcp, 7001/tcp, 0.0.0.0:32765->2379/tcp   kwok-kwok-etcd
```



### 部署 kube-scheduler WebAssembly 插件

[kube-scheduler-wasm-extension]([kube-scheduler-wasm-extension](https://github.com/kubernetes-sigs/kube-scheduler-wasm-extension)) 是一个使用 WebAssembly 扩展 kube-scheduler 的项目。本次主要验证该项目用于 demo 的 [NodeNumber 调度插件](https://github.com/kubernetes-sigs/kube-scheduler-wasm-extension/tree/main/examples/nodenumber)。

#### kube-scheduler 支持 WebAssembly 插件

kube-scheduler 代码启动时默认不支持加载 WebAssembly 插件，kube-scheduler-wasm-extension 项目中的 [scheduler 包](https://github.com/kubernetes-sigs/kube-scheduler-wasm-extension/tree/main/scheduler)帮我们实现了在 kube-scheduler 启动时加载 WebAssembly 插件的功能，所以需要重新编译一下 kube-scheduler。

1、使用 kube-scheduler-wasm-extension 项目中 scheduler 包重新编译 kube-scheduler 镜像。

注意需要更新 Dockerfile

```go
diff --git a/scheduler/Dockerfile b/scheduler/Dockerfile
index 88d562c..98deed3 100644
--- a/scheduler/Dockerfile
+++ b/scheduler/Dockerfile
@@ -11,11 +11,11 @@ COPY go.mod go.sum ./
 RUN go mod download

 COPY . .
-RUN go build -v -o ./bin/kube-scheduler-wasm-extension ./cmd/scheduler
+RUN go build -v -o ./bin/kube-scheduler ./cmd/scheduler

 FROM alpine:3.19.1

-COPY --from=build-env /go/src/kube-scheduler-wasm-extension/bin/kube-scheduler-wasm-extension /kube-scheduler-wasm-extension
-RUN chmod a+x /kube-scheduler-wasm-extension
+COPY --from=build-env /go/src/kube-scheduler-wasm-extension/bin/kube-scheduler /usr/local/bin/kube-scheduler
+RUN chmod a+x /usr/local/bin/kube-scheduler

-CMD ["/kube-scheduler-wasm-extension"]
+CMD ["kube-scheduler"]
```

2、准备 kube-scheduler 配置文件

```yaml
✗ cat kube-scheduler-config.yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
- plugins:
    multiPoint:
      enabled:
      - name: NodeNumber
  pluginConfig:
  - name: NodeNumber
    args:
      guestURL: "file:///main.wasm"
```



#### 编译 NodeNumber 调度插件

##### 安装 tinygo 环境

```bash
✗ brew install tinygo
✗ brew install binaryen
```

##### 编译 NodeNumber 调度插件为 wasm 文件

kube-scheduler-wasm-extension 项目 Makefile 文件增加以下配置

```makefile
examples/nodenumber/main.wasm: examples/nodenumber/main.go
	@(cd $(@D); tinygo build -o main.wasm -scheduler=none --no-debug -target=wasi .)
```

编译wasm文件

```bash
✗ make examples/nodenumber/main.wasm
```



#### k8s集群自定义调度器配置

##### 自定义调度器配置

kwok 配置文件如下

```yaml
✗ cat kwok.yaml
kind: KwokctlConfiguration
apiVersion: config.kwok.x-k8s.io/v1alpha1
name: dev-kube-scheduler
options:
  kubeSchedulerConfig: /Users/marswang/Deploy/k8s/kwok/kube-scheduler-config.yaml
  kubeSchedulerImage: kube-scheduler-wasm-extension:v1.27.3
  kubeVersion: v1.27.3
componentsPatches:
- name: kube-scheduler
  extraVolumes:
  - hostPath: /Users/marswang/Deploy/k8s/kwok/main.wasm 
    mountPath: /main.wasm
    pathType: File
```

> k8s 版本指定为 v1.27.3 是因为 kube-scheduler-wasm-extension 项目使用的 k8s 包版本是 v1.27.3，后续再验证其他版本 k8s。

卷配置主要是为了将前面编译的 wasm 二进制文件挂载到 kube-scheduler 容器中。



##### 重建 k8s 集群

```bash
✗ kwok create cluter -c ./kwok.yaml
```



### 踩坑指南

按照上面的步骤重建 k8s 集群以后，本以为可以开始愉快的测试 kube-scheduler功能了，谁知道这只是个开始......

##### 问题一：kube-scheduler 容器启动以后会一直重启？

报错日志如下：

```bash
✗ docker logs -f 18980108b8ed
flag provided but not defined: -authorization-always-allow-paths
Usage of kube-scheduler:
  -config string
```

看到这个问题是个启动参数有关，我看了一下 scheduler 包中的加载参数配置的代码，一会儿就发现了问题：

```go
# scheduler/cmd/scheduler/config.go

// getWasmPluginsFromConfig parses the scheduler configuration specified with --config option,
// and return the wasm plugins enabled by the user.
func getWasmPluginsFromConfig() ([]string, error) {
	// // In the scheduler, the path to the scheduler configuration is specified with --config option.
	configFile := flag.String("config", "", "")
	flag.Parse()

	if *configFile == "" {
		// Users don't have the own configuration. do nothing.
		return nil, nil
	}
	cfg, err := loadConfigFromFile(*configFile)
	if err != nil {
		return nil, err
	}

	return getWasmPluginNames(cfg), nil
}
```

这里为了加载解析配置文件，竟然自己重新定义了启动参数导致把 kube-scheudler 默认的启动参数给覆盖了，果然这项目现在还是个玩具......

只能去扒一下 kube-scheudler 初始化参数的代码了，幸好这个也不麻烦，毕竟之前看过很多遍调度器的代码。

改完之后的代码是这样子。

```go
# scheduler/cmd/scheduler/config.go

import (
	...
	"k8s.io/kubernetes/cmd/kube-scheduler/app/options"
	...
)

// getWasmPluginsFromConfig parses the scheduler configuration specified with --config option,
// and return the wasm plugins enabled by the user.
func getWasmPluginsFromConfig() ([]string, error) {
	options := options.NewOptions()
	configFile := options.ConfigFile

	if configFile == "" {
		// Users don't have the own configuration. do nothing.
		return nil, nil
	}
	cfg, err := loadConfigFromFile(configFile)
	if err != nil {
		return nil, err
	}

	return getWasmPluginNames(cfg), nil
}
```

本来想着这次总该可以了吧，谁知道构建完镜像以后重新部署集群调度器还是起不来，又被打脸了。

##### 问题二：kube-scheduler 容器启动还是会一直重启？

容器日志如下：

```bash
✗ docker logs cbc31a9ea658
I1018 08:15:58.370421       1 serving.go:348] Generated self-signed cert in-memory
W1018 08:15:59.374438       1 authentication.go:339] No authentication-kubeconfig provided in order to lookup client-ca-file in configmap/extension-apiserver-authentication in kube-system, so client certificate authentication won't work.
W1018 08:15:59.374545       1 authentication.go:363] No authentication-kubeconfig provided in order to lookup requestheader-client-ca-file in configmap/extension-apiserver-authentication in kube-system, so request-header client certificate authentication won't work.
W1018 08:15:59.374917       1 authorization.go:193] No authorization-kubeconfig provided, so SubjectAccessReview of authorization tokens won't work.
E1018 08:15:59.474846       1 run.go:74] "command failed" err="initializing profiles: creating profile for scheduler name default-scheduler: PreFilterPlugin \"NodeNumber\" does not exist"
```

就这个错误日志报错的位置我就找了好一会儿，我在 kube-scheduler 代码启动流程里找了半天也没有找到调用 run.go 文件的地方，过了好一会儿才查到这个错误是 [cobra](github.com/spf13/cobra) 包里面报出来的，最后我直接全文搜索错误日志内容最后才找到报错的代码。

刚开始我以为可能和前面加的参数解析的代码有关，但是使用本地构建的调度器二进制查看help说明以后看到所有 kube-scheduler 的启动参数都生效了，当时又感觉这块代码应该没什么问题。

```bash
✗ ./bin/kube-scheduler --help
The Kubernetes scheduler is a control plane process which assigns
Pods to Nodes. The scheduler determines which Nodes are valid placements for
each Pod in the scheduling queue according to constraints and available
resources. The scheduler then ranks each valid Node and binds the Pod to a
suitable Node. Multiple different schedulers may be used within a cluster;
kube-scheduler is the reference implementation.
See [scheduling](https://kubernetes.io/docs/concepts/scheduling-eviction/)
for more information about scheduling and the kube-scheduler component.

Usage:
  kube-scheduler [flags]

Misc flags:

      --config string
                The path to the configuration file.
      --master string
                The address of the Kubernetes API server (overrides any value in kubeconfig)
      --write-config-to string
                If set, write the configuration values to this file and exit.

Secure serving flags:

      --bind-address ip
                The IP addres
      .....
```

接下来又在 kube-scheduler 代码里 debug 了好长时间，最终才定位到是前面增加 kube-scheduler 参数解析代码引入的问题，前面加的代码只是在二进制里加了启动参数，但是还没有讲启动参数解析到代码内部的数据结构上，导致 kube-scheduler  启动过程时没有拿到 `--config` 参数传进来的值，`getWasmPluginsFromConfig()` 函数返回的 pluginNames 列表为空。

造成这个问题的原因还是因为对 cobra 包命令行参数初始化这块代码逻辑了解的不够，后面又理了一下 cobra 包参数定义和参数解析的逻辑，最后又魔改了一版。

```go
# scheduler/cmd/scheduler/config.go

func getWasmPluginsFromConfig() ([]string, error) {
	options := options.NewOptions()
	flagSets := pflag.NewFlagSet("kube-scheduler", pflag.ContinueOnError)
	for _, f := range options.Flags.FlagSets {
		flagSets.AddFlagSet(f)
	}
	flagSets.Parse(os.Args[1:])
	configFile := options.ConfigFile

	if configFile == "" {
		// Users don't have the own configuration. do nothing.
		return nil, nil
	}
	cfg, err := loadConfigFromFile(configFile)
	if err != nil {
		return nil, err
	}

	return getWasmPluginNames(cfg), nil
}
```

最终 kube-scheduler 终于启动成功了。

```bash
✗ docker logs dadd08eb4a9e
2024-10-18 16:56:57 I1018 08:56:57.232823       1 serving.go:348] Generated self-signed cert in-memory
2024-10-18 16:57:00 W1018 08:57:00.909134       1 authentication.go:339] No authentication-kubeconfig provided in order to lookup client-ca-file in configmap/extension-apiserver-authentication in kube-system, so client certificate authentication won't work.
2024-10-18 16:57:00 W1018 08:57:00.909244       1 authentication.go:363] No authentication-kubeconfig provided in order to lookup requestheader-client-ca-file in configmap/extension-apiserver-authentication in kube-system, so request-header client certificate authentication won't work.
2024-10-18 16:57:00 W1018 08:57:00.909691       1 authorization.go:193] No authorization-kubeconfig provided, so SubjectAccessReview of authorization tokens won't work.
2024-10-18 16:57:04 I1018 08:57:04.093281       1 server.go:154] "Starting Kubernetes Scheduler" version="v0.0.0-master+$Format:%H$"
2024-10-18 16:57:04 I1018 08:57:04.093616       1 server.go:156] "Golang settings" GOGC="" GOMAXPROCS="" GOTRACEBACK=""
2024-10-18 16:57:04 I1018 08:57:04.113839       1 tlsconfig.go:240] "Starting DynamicServingCertificateController"
2024-10-18 16:57:04 I1018 08:57:04.114503       1 secure_serving.go:210] Serving securely on [::]:10259
2024-10-18 16:57:04 I1018 08:57:04.222602       1 leaderelection.go:245] attempting to acquire leader lease kube-system/kube-scheduler...
2024-10-18 16:57:04 I1018 08:57:04.235971       1 leaderelection.go:255] successfully acquired lease kube-system/kube-scheduler
```



### 调度插件功能验证

终于有机会展现 kwok 的强大的力量了。

##### 一键扩容集群节点

```bash
✗ kwokctl scale node --replicas=3
```

##### 创建 pod

```bash
✗ k run nettools-demo busybox:lastet
pod/nettools-demo created
✗ k get pods
NAME            READY   STATUS    RESTARTS   AGE
nettools-demo   1/1     Running   0          4s
```

调度日志如下

```bash
I1018 09:05:32.676496       1 host.go:404] "execute PreScore on NodeNumber plugin" pod="default/nettools-demo"
E1018 09:05:32.689075       1 event_recorder.go:60] Unsupported event type: 'PreScore'
I1018 09:05:32.694369       1 host.go:404] "execute Score on NodeNumber plugin" pod="default/nettools-demo"
I1018 09:05:32.694753       1 host.go:404] "execute Score on NodeNumber plugin" pod="default/nettools-demo"
I1018 09:05:32.694859       1 host.go:404] "execute Score on NodeNumber plugin" pod="default/nettools-demo"
```

至此使用 WebAssembly 扩展 kube-scheduler 流程终于验证通了。