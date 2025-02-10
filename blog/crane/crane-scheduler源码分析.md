### 一、crane-scheduler
crane-scheduler 扩展了 k8s 原生调度器使其支持实时感知节点实际资源负载并且优化了热点调度，还支持基于节点拓扑的调度策略（待验证）。

**主要组件包含**：

##### 1、crane-scheduler-controller

该组件会间歇性通过 prometheus 获取各节点的监控指标以得到实时负载，并且会通过 kube-apiserver 获取节点对应的调度事件以判断该节点是否出现热点调度，最终将实时负载和热点调度信息通过注解形式更新到对应节点。

源码调用逻辑如下图所示

![](../../images/crane-scheduler-controller流程图.png)

上图中存放 event 信息的 heap 是一个 BindingHeap 结构，该结构定义如下：

```go
//// pkg/controller/annotator/binding.go

// Binding is a concise struction of pod binding records,
// which consists of pod name, namespace name, node name and accurate timestamp.
// Note that we only record temporary Binding imformation.
type Binding struct {
	Node      string
	Namespace string
	PodName   string
	Timestamp int64
}

// BindingHeap is a Heap struction storing Binding imfromation.
type BindingHeap []*Binding
```

当 event Contoller 收到 event 以后会提取出 nodeName、namespace、podName、时间戳等信息并构造一个 Binding 对象推到栈上；node Controller 更新节点注解时会计算该节点 Hot value 值，计算逻辑是遍历调度策略配置里面 hotValue 数组，调用`**GetLastNodeBindingCount**`函数计算每一个配置的热点值最终进行累加，代码如下：

```go
//// pkg/controller/annotator/node.go - annotateNodeHotValue()
func annotateNodeHotValue(kubeClient clientset.Interface, br *BindingRecords, node *v1.Node, policy policy.DynamicSchedulerPolicy) error {
	var value int

    // 遍历调度策略配置HotValue字段，根据每一个策略计算出来的pod数量/p.count作为热点值，并进行累加
	for _, p := range policy.Spec.HotValue {
		value += br.GetLastNodeBindingCount(node.Name, p.TimeRange.Duration) / p.Count
	}

	return patchNodeAnnotation(kubeClient, node, HotValueKey, strconv.Itoa(value))
}

//// pkg/controller/annotator/node.go - GetLastNodeBindingCount()
// 计算对应节点在最近一段时间调度的pod数量
func (br *BindingRecords) GetLastNodeBindingCount(node string, timeRange time.Duration) int {
	br.rw.RLock()
	defer br.rw.RUnlock()

	cnt, timeline := 0, time.Now().UTC().Unix()-int64(timeRange.Seconds())

	for _, binding := range *br.bindings {
		if binding.Timestamp > timeline && binding.Node == node {
			cnt++
		}
	}

	klog.V(4).Infof("The total Binding count is %d, while node[%s] count is %d",
		len(*br.bindings), node, cnt)

	return cnt
}
```

##### 2、crane-scheduler

扩展了 kube-scheduler 的 `Filter`、`Score`等阶段，使得调度过程可以通过注解中的负载信息感知到该节点的实时负载。[官方文档](https://gocrane.io/zh-cn/docs/tutorials/dynamic-scheduler-plugin/)

crane-scheduler 也支持基于节点拓扑资源感知进行调度，该扩展插件默认为开启，需要 crane-agent 组件配合使用。[官方文档](https://gocrane.io/zh-cn/docs/tutorials/node-resource-tpolology-scheduler-plugins/)


#### 二、crane-agent

功能包含：收集节点拓扑信息、根据 pod 注解同步更新容器 cpuset 配置等等，以下代码主要分析这两个功能实现。

##### 调用栈概览

```
main()                                                         // cmd/crane-agent/main.go
|- NewAgentCommand()						        	       // cmd/crane-agent/app/agent.go
    |- Run()
        |- NewAgent()                                          // pkg/agent/agent.go
            |- runtime.GetCRIRuntimeService()                  // pkg/ensurance/runtime/runtime.go
            |- agent.CreateNodeResourceTopology()              // pkg/agent/agent.go
                 |- BuildNodeResourceTopology()
                     |- ghw.Topology()
                     |- parse kubelet reserved
                     |- init cpuManagerPolicy
                     |- build nrt
                 |- CreateOrUpdateNodeResourceTopology()
            |- cpumanager.NewCPUManager()                      // pkg/ensurance/cm/cpumanager/cpu_manager.go
            |- analyzer.NewAnomalyAnalyzer()
            |- executor.NewActionExecutor()
            |- collector.NewStateCollector()
            |- appendManagerIfNotNil()                         // pkg/agent/agent.go
        |- newAgent.Run()                                      // pkg/agent/agent.go
    |- AddFlags()                                              // cmd/crane-agent/app/options/option.go

```

##### 源码分析

1、初始化 crane-agent 命令行参数、agent对象，然后运行 agent.Run() 函数启动所有注册的 manager。

```go
//// cmd/crane-agent/app/agent.go - NewAgentCommand()
func NewAgentCommand(ctx context.Context) *cobra.Command {
	opts := options.NewOptions()

	cmd := &cobra.Command{
		Use:  "crane-agent",
		Long: `The crane agent is running in each node and responsible for QoS ensurance`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := opts.Complete(); err != nil {
				klog.Exitf("Opts complete failed: %v", err)
			}
			if err := opts.Validate(); err != nil {
				klog.Exitf("Opts validate failed: %v", err)
			}

			cmd.Flags().VisitAll(func(flag *pflag.Flag) {
				klog.Infof("FLAG: --%s=%q\n", flag.Name, flag.Value)
			})

			if err := Run(ctx, opts); err != nil {
				klog.Exit(err)
			}
		},
	}
	:
	return cmd
}

//// cmd/crane-agent/app/agent.go - Run()
func Run(ctx context.Context, opts *options.Options) error {
	// pod, node, crane, nodeQOS, podOQS, action, tsp, nrt informer init
    :

    // init agent object
	newAgent, err := agent.NewAgent(ctx, hostname, opts.RuntimeEndpoint, opts.CgroupDriver, opts.SysPath,
		opts.KubeletRootPath, kubeClient, craneClient, podInformer, nodeInformer, nodeQOSInformer, podQOSInformer,
		actionInformer, tspInformer, nrtInformer, opts.NodeResourceReserved, opts.Ifaces, healthCheck,
		opts.CollectInterval, opts.ExecuteExcess, opts.CPUManagerReconcilePeriod, opts.DefaultCPUPolicy)

	if err != nil {
		return err
	}

    // all informer Start and WaitForCacheSync
    :

    // agent start(cycle start all of the manager)
	newAgent.Run(healthCheck, opts.EnableProfiling, opts.BindAddr)
	return nil
}

//// pkg/agent/agent.go - NewAgent()
func NewAgent(ctx context.Context,
	nodeName, runtimeEndpoint, cgroupDriver, sysPath, kubeletRootPath string,
	kubeClient kubernetes.Interface,
	craneClient craneclientset.Interface,
	podInformer coreinformers.PodInformer,
	nodeInformer coreinformers.NodeInformer,
	nodeQOSInformer v1alpha1.NodeQOSInformer,
	podQOSInformer v1alpha1.PodQOSInformer,
	actionInformer v1alpha1.AvoidanceActionInformer,
	tspInformer predictionv1.TimeSeriesPredictionInformer,
	nrtInformer topologyinformer.NodeResourceTopologyInformer,
	nodeResourceReserved map[string]string,
	ifaces []string,
	healthCheck *metrics.HealthCheck,
	collectInterval time.Duration,
	executeExcess string,
	cpuManagerReconcilePeriod time.Duration,
	defaultCPUPolicy string,
) (*Agent, error) {
	var managers []manager.Manager
	var noticeCh = make(chan executor.AvoidanceExecutor)
	agent := &Agent{
		ctx:         ctx,
		name:        getAgentName(nodeName),
		nodeName:    nodeName,
		kubeClient:  kubeClient,
		craneClient: craneClient,
	}

    // initial remote criRuntime service
	runtimeService, err := runtime.GetCRIRuntimeService(runtimeEndpoint)
	if err != nil {
		return nil, err
	}

	utilruntime.Must(ensuranceapi.AddToScheme(scheme.Scheme))
	utilruntime.Must(topologyapi.AddToScheme(scheme.Scheme))
	cadvisorManager := cadvisor.NewCadvisorManager(cgroupDriver)
	exclusiveCPUSet := cpumanager.DefaultExclusiveCPUSet
	if utilfeature.DefaultFeatureGate.Enabled(features.CraneNodeResourceTopology) {
        // collector node resource topology and update to noderesourcetopology crd
		if err := agent.CreateNodeResourceTopology(sysPath); err != nil {
			return nil, err
		}
        // init CPUManager and append to agent.managers
		if utilfeature.DefaultFeatureGate.Enabled(features.CraneCPUManager) {
			cpuManager, err := cpumanager.NewCPUManager(nodeName, defaultCPUPolicy, cpuManagerReconcilePeriod, cadvisorManager, runtimeService, kubeletRootPath, podInformer, nrtInformer)
			if err != nil {
				return nil, fmt.Errorf("failed to new cpumanager: %v", err)
			}
			exclusiveCPUSet = cpuManager.GetExclusiveCPUSet
			managers = appendManagerIfNotNil(managers, cpuManager)
		}
	}

	stateCollector := collector.NewStateCollector(nodeName, sysPath, kubeClient, craneClient, nodeQOSInformer.Lister(), nrtInformer.Lister(), podInformer.Lister(), nodeInformer.Lister(), ifaces, healthCheck, collectInterval, exclusiveCPUSet, cadvisorManager)
	managers = appendManagerIfNotNil(managers, stateCollector)
	analyzerManager := analyzer.NewAnomalyAnalyzer(kubeClient, nodeName, podInformer, nodeInformer, nodeQOSInformer, podQOSInformer, actionInformer, stateCollector.AnalyzerChann, noticeCh)
	managers = appendManagerIfNotNil(managers, analyzerManager)
	avoidanceManager := executor.NewActionExecutor(kubeClient, nodeName, podInformer, nodeInformer, noticeCh, runtimeEndpoint, stateCollector.State, executeExcess)
	managers = appendManagerIfNotNil(managers, avoidanceManager)

	if nodeResource := utilfeature.DefaultFeatureGate.Enabled(features.CraneNodeResource); nodeResource {
		tspName := agent.CreateNodeResourceTsp()
		nodeResourceManager, err := resource.NewNodeResourceManager(kubeClient, nodeName, nodeResourceReserved, tspName, nodeInformer, tspInformer, stateCollector.NodeResourceChann)
		if err != nil {
			return agent, err
		}
		managers = appendManagerIfNotNil(managers, nodeResourceManager)
	}

	if podResource := utilfeature.DefaultFeatureGate.Enabled(features.CranePodResource); podResource {
		podResourceManager := resource.NewPodResourceManager(kubeClient, nodeName, podInformer, runtimeEndpoint, stateCollector.PodResourceChann, stateCollector.GetCadvisorManager())
		managers = appendManagerIfNotNil(managers, podResourceManager)
	}

	agent.managers = managers

	return agent, nil
}
```

2、收集node resource topology相关信息，包含：k8s node、kubelet reserved config、node topo info、cpuManagerPolicy，创建或更新 noderesourcetopology crd资源

```go
//// pkg/agent/agent.go
func (a *Agent) CreateNodeResourceTopology(sysPath string) error {
    :

	node, err := a.kubeClient.CoreV1().Nodes().Get(context.TODO(), a.nodeName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get node: %v", err)
		return err
	}

	kubeletConfig, err := utils.GetKubeletConfig(context.TODO(), a.kubeClient, a.nodeName)
	if err != nil {
		klog.Errorf("Failed to get kubelet config: %v", err)
		return err
	}

	newNrt, err := noderesourcetopology.BuildNodeResourceTopology(sysPath, kubeletConfig, node)
	if err != nil {
		klog.Errorf("Failed to build node resource topology: %v", err)
		return err
	}

	if err = noderesourcetopology.CreateOrUpdateNodeResourceTopology(a.craneClient, nrt, newNrt); err != nil {
		klog.Errorf("Failed to create or update node resource topology: %v", err)
		return err
	}
	return nil
}
```

3、cpu manager 初始化和启动

```go
//// pkg/ensurance/cm/cpumanager/cpu_manager.go

func NewCPUManager(
	nodeName string,
	defaultCPUPolicy string,
	reconcilePeriod time.Duration,
	cadvisorManager cadvisor.Manager,
	containerRuntime criapis.RuntimeService,
	stateFileDirectory string,
	podInformer coreinformers.PodInformer,
	nrtInformer topologyinformer.NodeResourceTopologyInformer,
) (CPUManager, error) {
    :

    // construct cpuManager object
	cm := &cpuManager{
		nodeName:         nodeName,
		workqueue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "cpumanager"),
		podLister:        podInformer.Lister(),
		nrtLister:        nrtInformer.Lister(),
		podSync:          podInformer.Informer().HasSynced,
		nrtSync:          nrtInformer.Informer().HasSynced,
		defaultCPUPolicy: defaultCPUPolicy,
		reconcilePeriod:  reconcilePeriod,
		lastUpdateState:  cpumanagerstate.NewMemoryState(),
		containerRuntime: containerRuntime,
		containerMap:     containermap.NewContainerMap(),
	}

    // add new index key to podInformer indexers for quick lookup
	_ = podInformer.Informer().AddIndexers(cache.Indexers{
		cpuPolicyKeyIndex: func(obj interface{}) ([]string, error) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return []string{}, nil
			}
			policyName := cm.getPodCPUPolicyOrDefault(pod)
			return []string{policyName}, nil
		},
	})

    // construct cm policy getPodFunc field
	podIndexer := podInformer.Informer().GetIndexer()
	getPodFunc := func(cpuPolicy string) ([]*corev1.Pod, error) {
		objs, err := podIndexer.ByIndex(cpuPolicyKeyIndex, cpuPolicy)
		if err != nil {
			return nil, err
		}
		pods := make([]*corev1.Pod, 0, len(objs))
		for _, obj := range objs {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				continue
			}
			// Succeeded and failed pods are not considered because they don't occupy any resource.
			// See https://github.com/kubernetes/kubernetes/blob/f61ed439882e34d9dad28b602afdc852feb2337a/pkg/scheduler/scheduler.go#L756-L763
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				pods = append(pods, pod)
			}
		}
		return pods, nil
	}

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: cm.enqueuePod,
	})

	cm.activePods = func() ([]*corev1.Pod, error) {
		allPods, err := cm.podLister.List(labels.Everything())
		if err != nil {
			return nil, err
		}
		activePods := make([]*corev1.Pod, 0, len(allPods))
		for _, pod := range allPods {
			if !utils.IsPodTerminated(pod) {
				activePods = append(activePods, pod)
			}
		}
		return activePods, nil
	}

    // init policy
	cm.policy = NewStaticPolicy(topo, getPodFunc)

    // init cm.state
	cm.state, err = cpumanagerstate.NewCheckpointState(
		stateFileDirectory,
		cpuManagerStateFileName,
		cm.policy.Name(),
		initialContainers,
	)
    :
	return cm, nil
}

// Run running cpu manager
func (cm *cpuManager) Run(stopCh <-chan struct{}) {
    :
    // 消费cm.workqueue中的pod事件，通过注解获取该pod的cpu policy及topology result，并根据cpu policy和topology result等信息更新该容器的cpuset到cm.state中
	go wait.Until(cm.runWorker, time.Second, stopCh)

	nodeTopologyResult, err := cm.getNodeTopologyResult()

    // 验证cm.state数据
	if err := cm.policy.Start(cm.state, nodeTopologyResult); err != nil {
		klog.Fatalf("Failed to start cpumanager policy: %v", err)
	}

    // 间歇低调用cm.reconcileState()以保证容器 cgroup中的cpusets与分配给它的cpu一致。
	// Periodically call m.reconcileState() to continue to keep the CPU sets of
	// all pods in sync with and guaranteed CPUs handed out among them.
	go wait.Until(func() {
		nrt, err := cm.nrtLister.Get(cm.nodeName)
		if err != nil || nrt.CraneManagerPolicy.CPUManagerPolicy != topologyapi.CPUManagerPolicyStatic {
			return
		}
		cm.reconcileState()
	}, cm.reconcilePeriod, stopCh)
}
```

同步cm.state和linux cgroup中的cpusets

```go
//// pkg/ensurance/cm/cpumanager/cpu_manager.go
func (cm *cpuManager) reconcileState() (success []reconciledContainer, failure []reconciledContainer) {
	success = []reconciledContainer{}
	failure = []reconciledContainer{}

	// Get the list of active pods.
	activePods, err := cm.activePods()
	if err != nil {
		klog.ErrorS(err, "Failed to get active pods when reconcileState in cpuManager")
		return
	}

	cm.renewState(activePods)

	cm.Lock()
	defer cm.Unlock()

	for _, pod := range activePods {
		allContainers := pod.Spec.InitContainers
		allContainers = append(allContainers, pod.Spec.Containers...)
		for _, container := range allContainers {
			containerID, _, err := findRunningContainerStatus(&pod.Status, container.Name)
			if err != nil {
				klog.V(4).InfoS("ReconcileState: skipping container", "pod", klog.KObj(pod), "containerName", container.Name, "err", err)
				failure = append(failure, reconciledContainer{pod.Name, container.Name, ""})
				continue
			}

			cset := cm.state.GetCPUSetOrDefault(string(pod.UID), container.Name)
			if cset.IsEmpty() {
				// NOTE: This should not happen outside of tests.
				klog.V(4).InfoS("ReconcileState: skipping container; assigned cpuset is empty", "pod", klog.KObj(pod), "containerName", container.Name)
				failure = append(failure, reconciledContainer{pod.Name, container.Name, containerID})
				continue
			}

			podUID, containerName, err := cm.containerMap.GetContainerRef(containerID)
			updated := err == nil && podUID == string(pod.UID) && containerName == container.Name

			lcset := cm.lastUpdateState.GetCPUSetOrDefault(string(pod.UID), container.Name)
			if !cset.Equals(lcset) || !updated {
				klog.V(4).InfoS("ReconcileState: updating container", "pod", klog.KObj(pod), "containerName", container.Name, "containerID", containerID, "cpuSet", cset)
				err = cm.updateContainerCPUSet(containerID, cset)
				if err != nil {
					klog.ErrorS(err, "ReconcileState: failed to update container", "pod", klog.KObj(pod), "containerName", container.Name, "containerID", containerID, "cpuSet", cset)
					failure = append(failure, reconciledContainer{pod.Name, container.Name, containerID})
					continue
				}
				cm.lastUpdateState.SetCPUSet(string(pod.UID), container.Name, cset)
				cm.containerMap.Add(string(pod.UID), container.Name, containerID)
			}
			success = append(success, reconciledContainer{pod.Name, container.Name, containerID})
		}
	}
	return success, failure
}

// 远程调用cri runtime接口更新容器资源
func (cm *cpuManager) updateContainerCPUSet(containerID string, cpus cpuset.CPUSet) error {
	return cm.containerRuntime.UpdateContainerResources(
		containerID,
		&runtimeapi.LinuxContainerResources{
			CpusetCpus: cpus.String(),
		})
}
```





### 优秀源码总结

1、适用于 k8s 资源不同版本数据对比，如果使用Golang 语言提供的 reflect.DeepEqual方法，当预期资源与已有资源默认值不同时，会导致此种方式失效。

equality.Semantic.DeepEqual()，可以配置一些参数来过滤掉空值、或指定的字段。

```go
//// pkg/ensurance/collector/noderesourcetopology/noderesourcetopology.go
import ""k8s.io/apimachinery/pkg/api/equality""

func CreateOrUpdateNodeResourceTopology(craneClient craneclientset.Interface, old, new *topologyapi.NodeResourceTopology) error {
	:
	if equality.Semantic.DeepEqual(old, new) {
		return nil
	}
	:
}
```

2、通用的patch资源实现

```go
//// pkg/ensurance/collector/noderesourcetopology/noderesourcetopology.go
func CreateOrUpdateNodeResourceTopology(craneClient craneclientset.Interface, old, new *topologyapi.NodeResourceTopology) error {
	:
	oldData, err := json.Marshal(old)
	if err != nil {
		return err
	}
	newData, err := json.Marshal(new)
	if err != nil {
		return err
	}
	patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
	if err != nil {
		return fmt.Errorf("failed to create merge patch: %v", err)
	}
	_, err = craneClient.TopologyV1alpha1().NodeResourceTopologies().Patch(context.TODO(), new.Name, patchtypes.MergePatchType, patchBytes, metav1.PatchOptions{})
	return err
}
```

3、Builder pattern设计

```go
//// pkg/topology/node_resource_topology.go
type NRTBuilder struct {
	node                  *corev1.Node
	cpuManagerPolicy      topologyapi.CPUManagerPolicy
	topologyManagerPolicy topologyapi.TopologyManagerPolicy
	reserved              corev1.ResourceList
	reservedCPUs          int
	topologyInfo          *topology.Info
}

// NewNRTBuilder returns a new NRTBuilder.
func NewNRTBuilder() *NRTBuilder {
	return &NRTBuilder{}
}

func (b *NRTBuilder) WithNode(node *corev1.Node) {
    :
}

func (b *NRTBuilder) WithCPUManagerPolicy(cpuManagerPolicy topologyapi.CPUManagerPolicy) {
    :
}

func (b *NRTBuilder) WithReserved(reserved corev1.ResourceList) {
    :
}

func (b *NRTBuilder) Build() *topologyapi.NodeResourceTopology {
	nrt := &topologyapi.NodeResourceTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name: b.node.Name,
		},
	}
	b.buildNodeScopeFields(nrt)
	b.buildZoneScopeFields(nrt)
	return nrt
}
```

