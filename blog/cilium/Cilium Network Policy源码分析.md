
### daemon初始化

```go
// daemon/cmd/daemon_main.go
var (
	log = logging.DefaultLogger.WithField(logfields.LogSubsys, daemonSubsys)

	bootstrapTimestamp = time.Now()

	// RootCmd represents the base command when called without any subcommands
	RootCmd = &cobra.Command{
		Use:   "cilium-agent",
		Short: "Run the cilium agent",
		Run: func(cmd *cobra.Command, args []string) {
			cmdRefDir := viper.GetString(option.CMDRef)
			if cmdRefDir != "" {
				genMarkdown(cmd, cmdRefDir)
				os.Exit(0)
			}

			// Open socket for using gops to get stacktraces of the agent.
			addr := fmt.Sprintf("127.0.0.1:%d", viper.GetInt(option.GopsPort))
			addrField := logrus.Fields{"address": addr}
			if err := gops.Listen(gops.Options{
				Addr:                   addr,
				ReuseSocketAddrAndPort: true,
			}); err != nil {
				log.WithError(err).WithFields(addrField).Fatal("Cannot start gops server")
			}
			log.WithFields(addrField).Info("Started gops server")

			bootstrapStats.earlyInit.Start()
			initEnv(cmd)
			bootstrapStats.earlyInit.End(true)
			runDaemon()
		},
	}

	bootstrapStats = bootstrapStatistics{}
)

func runDaemon() {
	datapathConfig := linuxdatapath.DatapathConfiguration{
		HostDevice: option.Config.HostDevice,
	}

	log.Info("Initializing daemon")

	option.Config.RunMonitorAgent = true

	if err := enableIPForwarding(); err != nil {
		log.WithError(err).Fatal("Error when enabling sysctl parameters")
	}

	iptablesManager := &iptables.IptablesManager{}
	iptablesManager.Init()

	var wgAgent *wireguard.Agent
	if option.Config.EnableWireguard {
		switch {
		case option.Config.EnableIPSec:
			log.Fatalf("Wireguard (--%s) cannot be used with IPSec (--%s)",
				option.EnableWireguard, option.EnableIPSecName)
		case option.Config.EnableL7Proxy:
			log.Fatalf("Wireguard (--%s) is not compatible with L7 proxy (--%s)",
				option.EnableWireguard, option.EnableL7Proxy)
		}

		var err error
		privateKeyPath := filepath.Join(option.Config.StateDir, wireguardTypes.PrivKeyFilename)
		wgAgent, err = wireguard.NewAgent(privateKeyPath)
		if err != nil {
			log.WithError(err).Fatal("Failed to initialize wireguard")
		}

		cleaner.cleanupFuncs.Add(func() {
			_ = wgAgent.Close()
		})
	} else {
		// Delete wireguard device from previous run (if such exists)
		link.DeleteByName(wireguardTypes.IfaceName)
	}

	if k8s.IsEnabled() {
		bootstrapStats.k8sInit.Start()
		if err := k8s.Init(option.Config); err != nil {
			log.WithError(err).Fatal("Unable to initialize Kubernetes subsystem")
		}
		bootstrapStats.k8sInit.End(true)
	}

    // 实例化daemon
	ctx, cancel := context.WithCancel(server.ServerCtx)
	d, restoredEndpoints, err := NewDaemon(ctx, cancel,
		WithDefaultEndpointManager(ctx, endpoint.CheckHealth),
		linuxdatapath.NewDatapath(datapathConfig, iptablesManager, wgAgent))
	if err != nil {
		select {
		case <-server.ServerCtx.Done():
			log.WithError(err).Debug("Error while creating daemon")
		default:
			log.WithError(err).Fatal("Error while creating daemon")
		}
		return
	}

	// This validation needs to be done outside of the agent until
	// datapath.NodeAddressing is used consistently across the code base.
	log.Info("Validating configured node address ranges")
	if err := node.ValidatePostInit(); err != nil {
		log.WithError(err).Fatal("postinit failed")
	}

	bootstrapStats.enableConntrack.Start()
	log.Info("Starting connection tracking garbage collector")
	gc.Enable(option.Config.EnableIPv4, option.Config.EnableIPv6,
		restoredEndpoints.restored, d.endpointManager)
	bootstrapStats.enableConntrack.End(true)

	bootstrapStats.k8sInit.Start()
	if k8s.IsEnabled() {
		// Wait only for certain caches, but not all!
		// (Check Daemon.InitK8sSubsystem() for more info)
		<-d.k8sCachesSynced
	}
	bootstrapStats.k8sInit.End(true)
	restoreComplete := d.initRestore(restoredEndpoints)
	if wgAgent != nil {
		if err := wgAgent.RestoreFinished(); err != nil {
			log.WithError(err).Error("Failed to set up wireguard peers")
		}
	}

	if d.endpointManager.HostEndpointExists() {
		d.endpointManager.InitHostEndpointLabels(d.ctx)
	} else {
		log.Info("Creating host endpoint")
		if err := d.endpointManager.AddHostEndpoint(
			d.ctx, d, d, d.l7Proxy, d.identityAllocator,
			"Create host endpoint", nodeTypes.GetName(),
		); err != nil {
			log.WithError(err).Fatal("Unable to create host endpoint")
		}
	}

	if option.Config.EnableIPMasqAgent {
		ipmasqAgent, err := ipmasq.NewIPMasqAgent(option.Config.IPMasqAgentConfigPath)
		if err != nil {
			log.WithError(err).Fatal("Failed to create ip-masq-agent")
		}
		ipmasqAgent.Start()
	}

	if !option.Config.DryMode {
		go func() {
			if restoreComplete != nil {
				<-restoreComplete
			}
			d.dnsNameManager.CompleteBootstrap()

			ms := maps.NewMapSweeper(&EndpointMapManager{
				EndpointManager: d.endpointManager,
			})
			ms.CollectStaleMapGarbage()
			ms.RemoveDisabledMaps()

			if len(d.restoredCIDRs) > 0 {
				// Release restored CIDR identities after a grace period (default 10
				// minutes).  Any identities actually in use will still exist after
				// this.
				//
				// This grace period is needed when running on an external workload
				// where policy synchronization is not done via k8s. Also in k8s
				// case it is prudent to allow concurrent endpoint regenerations to
				// (re-)allocate the restored identities before we release them.
				time.Sleep(option.Config.IdentityRestoreGracePeriod)
				log.Debugf("Releasing reference counts for %d restored CIDR identities", len(d.restoredCIDRs))

				ipcache.ReleaseCIDRIdentitiesByCIDR(d.restoredCIDRs)
				// release the memory held by restored CIDRs
				d.restoredCIDRs = nil
			}
		}()
		d.endpointManager.Subscribe(d)
		defer d.endpointManager.Unsubscribe(d)
	}

	// Migrating the ENI datapath must happen before the API is served to
	// prevent endpoints from being created. It also must be before the health
	// initialization logic which creates the health endpoint, for the same
	// reasons as the API being served. We want to ensure that this migration
	// logic runs before any endpoint creates.
	if option.Config.IPAM == ipamOption.IPAMENI {
		migrated, failed := linuxrouting.NewMigrator(
			&eni.InterfaceDB{},
		).MigrateENIDatapath(option.Config.EgressMultiHomeIPRuleCompat)
		switch {
		case failed == -1:
			// No need to handle this case specifically because it is handled
			// in the call already.
		case migrated >= 0 && failed > 0:
			log.Errorf("Failed to migrate ENI datapath. "+
				"%d endpoints were successfully migrated and %d failed to migrate completely. "+
				"The original datapath is still in-place, however it is recommended to retry the migration.",
				migrated, failed)

		case migrated >= 0 && failed == 0:
			log.Infof("Migration of ENI datapath successful, %d endpoints were migrated and none failed.",
				migrated)
		}
	}

	bootstrapStats.healthCheck.Start()
	if option.Config.EnableHealthChecking {
		d.initHealth()
	}
	bootstrapStats.healthCheck.End(true)

	d.startStatusCollector()

	metricsErrs := initMetrics()

	d.startAgentHealthHTTPService()
	if option.Config.KubeProxyReplacementHealthzBindAddr != "" {
		if option.Config.KubeProxyReplacement != option.KubeProxyReplacementDisabled {
			d.startKubeProxyHealthzHTTPService(fmt.Sprintf("%s", option.Config.KubeProxyReplacementHealthzBindAddr))
		}
	}

	bootstrapStats.initAPI.Start()
	srv := server.NewServer(d.instantiateAPI())
	srv.EnabledListeners = []string{"unix"}
	srv.SocketPath = option.Config.SocketPath
	srv.ReadTimeout = apiTimeout
	srv.WriteTimeout = apiTimeout
	defer srv.Shutdown()

	srv.ConfigureAPI()
	bootstrapStats.initAPI.End(true)

	err = d.SendNotification(monitorAPI.StartMessage(time.Now()))
	if err != nil {
		log.WithError(err).Warn("Failed to send agent start monitor message")
	}

	if !d.datapath.Node().NodeNeighDiscoveryEnabled() {
		// Remove all non-GC'ed neighbor entries that might have previously set
		// by a Cilium instance.
		d.datapath.Node().NodeCleanNeighbors(false)
	} else {
		// If we came from an agent upgrade, migrate entries.
		d.datapath.Node().NodeCleanNeighbors(true)
		// Start periodical refresh of the neighbor table from the agent if needed.
		if option.Config.ARPPingRefreshPeriod != 0 && !option.Config.ARPPingKernelManaged {
			d.nodeDiscovery.Manager.StartNeighborRefresh(d.datapath.Node())
		}
	}

	log.WithField("bootstrapTime", time.Since(bootstrapTimestamp)).
		Info("Daemon initialization completed")

	if option.Config.WriteCNIConfigurationWhenReady != "" {
		input, err := os.ReadFile(option.Config.ReadCNIConfiguration)
		if err != nil {
			log.WithError(err).Fatal("Unable to read CNI configuration file")
		}

		if err = os.WriteFile(option.Config.WriteCNIConfigurationWhenReady, input, 0644); err != nil {
			log.WithError(err).Fatalf("Unable to write CNI configuration file to %s", option.Config.WriteCNIConfigurationWhenReady)
		} else {
			log.Infof("Wrote CNI configuration file to %s", option.Config.WriteCNIConfigurationWhenReady)
		}
	}

	errs := make(chan error, 1)

	go func() {
		errs <- srv.Serve()
	}()

	bootstrapStats.overall.End(true)
	bootstrapStats.updateMetrics()
	go d.launchHubble()

	err = option.Config.StoreInFile(option.Config.StateDir)
	if err != nil {
		log.WithError(err).Error("Unable to store Cilium's configuration")
	}

	err = option.StoreViperInFile(option.Config.StateDir)
	if err != nil {
		log.WithError(err).Error("Unable to store Viper's configuration")
	}

	select {
	case err := <-metricsErrs:
		if err != nil {
			log.WithError(err).Fatal("Cannot start metrics server")
		}
	case err := <-errs:
		if err != nil {
			log.WithError(err).Fatal("Error returned from non-returning Serve() call")
		}
	}
}

// daemon/cmd/daemon.go
func NewDaemon(ctx context.Context, cancel context.CancelFunc, epMgr *endpointmanager.EndpointManager, dp datapath.Datapath) (*Daemon, *endpointRestoreState, error) {
    ...
    
   	d := Daemon{
		ctx:               ctx,
		cancel:            cancel,
		prefixLengths:     createPrefixLengthCounter(),
		buildEndpointSem:  semaphore.NewWeighted(int64(numWorkerThreads())),
		compilationMutex:  new(lock.RWMutex),
		netConf:           netConf,
		mtuConfig:         mtuConfig,
		datapath:          dp,
		deviceManager:     NewDeviceManager(),
		nodeDiscovery:     nd,
		endpointCreations: newEndpointCreationManager(),
		apiLimiterSet:     apiLimiterSet,
	}
    ...
    d.identityAllocator = NewCachingIdentityAllocator(&d)
    // 初始化daemon核心组件policy
	if err := d.initPolicy(epMgr); err != nil {
		return nil, nil, fmt.Errorf("error while initializing policy subsystem: %w", err)
	}
	nodeMngr = nodeMngr.WithSelectorCacheUpdater(d.policy.GetSelectorCache()) // must be after initPolicy
	nodeMngr = nodeMngr.WithPolicyTriggerer(d.policyUpdater) 
    ...
    
    d.k8sWatcher = watchers.NewK8sWatcher(
		d.endpointManager,
		d.nodeDiscovery.Manager,
		&d,
		d.policy,
		d.svc,
		d.datapath,
		d.redirectPolicyManager,
		d.bgpSpeaker,
		d.egressGatewayManager,
		option.Config,
	)
	nd.RegisterK8sNodeGetter(d.k8sWatcher)
	ipcache.IPIdentityCache.RegisterK8sSyncedChecker(&d)
    ...
    
    if k8s.IsEnabled() {
		bootstrapStats.k8sInit.Start()
		// Errors are handled inside WaitForCRDsToRegister. It will fatal on a
		// context deadline or if the context has been cancelled, the context's
		// error will be returned. Otherwise, it succeeded.
		if err := d.k8sWatcher.WaitForCRDsToRegister(d.ctx); err != nil {
			return nil, restoredEndpoints, err
		}

		// Launch the K8s node watcher so we can start receiving node events.
		// Launching the k8s node watcher at this stage will prevent all agents
		// from performing Gets directly into kube-apiserver to get the most up
		// to date version of the k8s node. This allows for better scalability
		// in large clusters.
		d.k8sWatcher.NodesInit(k8s.Client())

		if option.Config.IPAM == ipamOption.IPAMClusterPool {
			// Create the CiliumNode custom resource. This call will block until
			// the custom resource has been created
			d.nodeDiscovery.UpdateCiliumNodeResource()
		}

		if err := k8s.WaitForNodeInformation(d.ctx, d.k8sWatcher); err != nil {
			log.WithError(err).Error("unable to connect to get node spec from apiserver")
			return nil, nil, fmt.Errorf("unable to connect to get node spec from apiserver: %w", err)
		}

		// Kubernetes demands that the localhost can always reach local
		// pods. Therefore unless the AllowLocalhost policy is set to a
		// specific mode, always allow localhost to reach local
		// endpoints.
		if option.Config.AllowLocalhost == option.AllowLocalhostAuto {
			option.Config.AllowLocalhost = option.AllowLocalhostAlways
			log.Info("k8s mode: Allowing localhost to reach local endpoints")
		}

		bootstrapStats.k8sInit.End(true)
	}
    ...
    
    // 初始化k8s相关配置
    if k8s.IsEnabled() {
		bootstrapStats.k8sInit.Start()

		// Initialize d.k8sCachesSynced before any k8s watchers are alive, as they may
		// access it to check the status of k8s initialization
		cachesSynced := make(chan struct{})
		d.k8sCachesSynced = cachesSynced

        // 初始换k8s相关核心资源以及cilium资源并开始watch资源修改
		d.k8sWatcher.InitK8sSubsystem(d.ctx, cachesSynced)
		bootstrapStats.k8sInit.End(true)
	}
    ...
}
```

### k8s资源初始化

```go
// pkg/k8s/watchers/watcher.go
func (k *K8sWatcher) InitK8sSubsystem(ctx context.Context, cachesSynced chan struct{}) {
	// 注册所有需要watch的资源
	resources := k.resourceGroups()
	// 启动informer去watch所有注册的资源
	if err := k.EnableK8sWatcher(ctx, resources); err != nil {
		if !errors.Is(err, context.Canceled) {
			log.WithError(err).Fatal("Unable to start K8s watchers for Cilium")
		}
		// If the context was canceled it means the daemon is being stopped
		return
	}

	...
}

func (k *K8sWatcher) EnableK8sWatcher(ctx context.Context, resources []string) error {
	...
	ciliumNPClient := k8s.CiliumClient()
	asyncControllers := &sync.WaitGroup{}

	serviceOptModifier, err := utils.GetServiceListOptionsModifier(k.cfg)
	if err != nil {
		return fmt.Errorf("error creating service list option modifier: %w", err)
	}
	// 包含所有资源informer的创建和启动
	for _, r := range resources {
		switch r {
		// Core Cilium
		case K8sAPIGroupPodV1Core:
			asyncControllers.Add(1)
			go k.podsInit(k8s.WatcherClient(), asyncControllers)
		case k8sAPIGroupNodeV1Core:
			k.NodesInit(k8s.Client())
		case k8sAPIGroupNamespaceV1Core:
			asyncControllers.Add(1)
			go k.namespacesInit(k8s.WatcherClient(), asyncControllers)
		case k8sAPIGroupCiliumNodeV2:
			asyncControllers.Add(1)
			go k.ciliumNodeInit(ciliumNPClient, asyncControllers)
		// Kubernetes built-in resources
		case k8sAPIGroupNetworkingV1Core:
			swgKNP := lock.NewStoppableWaitGroup()
			k.networkPoliciesInit(k8s.WatcherClient(), swgKNP)
		case K8sAPIGroupServiceV1Core:
			swgSvcs := lock.NewStoppableWaitGroup()
			k.servicesInit(k8s.WatcherClient(), swgSvcs, serviceOptModifier)
		case K8sAPIGroupEndpointSliceV1Beta1Discovery:
			// no-op; handled in K8sAPIGroupEndpointV1Core.
		case K8sAPIGroupEndpointSliceV1Discovery:
			// no-op; handled in K8sAPIGroupEndpointV1Core.
		case K8sAPIGroupEndpointV1Core:
			k.initEndpointsOrSlices(k8s.WatcherClient(), serviceOptModifier)
		// Custom resource definitions
        // cilium network policy的informer
		case k8sAPIGroupCiliumNetworkPolicyV2:
			k.ciliumNetworkPoliciesInit(ciliumNPClient)
		case k8sAPIGroupCiliumClusterwideNetworkPolicyV2:
			k.ciliumClusterwideNetworkPoliciesInit(ciliumNPClient)
		case k8sAPIGroupCiliumEndpointV2:
			k.initCiliumEndpointOrSlices(ciliumNPClient, asyncControllers)
		case k8sAPIGroupCiliumEndpointSliceV2Alpha1:
			// no-op; handled in k8sAPIGroupCiliumEndpointV2
		case k8sAPIGroupCiliumLocalRedirectPolicyV2:
			k.ciliumLocalRedirectPolicyInit(ciliumNPClient)
		case k8sAPIGroupCiliumEgressNATPolicyV2:
			k.ciliumEgressNATPolicyInit(ciliumNPClient)
		default:
			log.WithFields(logrus.Fields{
				logfields.Resource: r,
			}).Fatal("Not listening for Kubernetes resource updates for unhandled type")
		}
	}

	asyncControllers.Wait()
	close(k.controllersStarted)

	return nil
}

```



```go
// pkg/k8s/watchers/cilium_network_policy.go
// cilium network policy资源的controller初始化和启动
func (k *K8sWatcher) ciliumNetworkPoliciesInit(ciliumNPClient *k8s.K8sCiliumClient) {
	cnpStore := cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc)

	ciliumV2Controller := informer.NewInformerWithStore(
		cache.NewListWatchFromClient(ciliumNPClient.CiliumV2().RESTClient(),
			cilium_v2.CNPPluralName, v1.NamespaceAll, fields.Everything()),
		&cilium_v2.CiliumNetworkPolicy{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				var valid, equal bool
				defer func() { k.K8sEventReceived(metricCNP, metricCreate, valid, equal) }()
				if cnp := k8s.ObjToSlimCNP(obj); cnp != nil {
					valid = true
					if cnp.RequiresDerivative() {
						return
					}

					// We need to deepcopy this structure because we are writing
					// fields.
					// See https://github.com/cilium/cilium/blob/27fee207f5422c95479422162e9ea0d2f2b6c770/pkg/policy/api/ingress.go#L112-L134
					cnpCpy := cnp.DeepCopy()
					// 处理cnp数据
					err := k.addCiliumNetworkPolicyV2(ciliumNPClient, cnpCpy)
                    // 事件统计
					k.K8sEventProcessed(metricCNP, metricCreate, err == nil)
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				var valid, equal bool
				defer func() { k.K8sEventReceived(metricCNP, metricUpdate, valid, equal) }()
				if oldCNP := k8s.ObjToSlimCNP(oldObj); oldCNP != nil {
					if newCNP := k8s.ObjToSlimCNP(newObj); newCNP != nil {
						valid = true
						if oldCNP.DeepEqual(newCNP) {
							equal = true
							return
						}

						if newCNP.RequiresDerivative() {
							return
						}

						// We need to deepcopy this structure because we are writing
						// fields.
						// See https://github.com/cilium/cilium/blob/27fee207f5422c95479422162e9ea0d2f2b6c770/pkg/policy/api/ingress.go#L112-L134
						oldCNPCpy := oldCNP.DeepCopy()
						newCNPCpy := newCNP.DeepCopy()

						err := k.updateCiliumNetworkPolicyV2(ciliumNPClient, oldCNPCpy, newCNPCpy)
						k.K8sEventProcessed(metricCNP, metricUpdate, err == nil)
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				var valid, equal bool
				defer func() { k.K8sEventReceived(metricCNP, metricDelete, valid, equal) }()
				cnp := k8s.ObjToSlimCNP(obj)
				if cnp == nil {
					return
				}
				valid = true
				err := k.deleteCiliumNetworkPolicyV2(cnp)
				k.K8sEventProcessed(metricCNP, metricDelete, err == nil)
			},
		},
		k8s.ConvertToCNP,
		cnpStore,
	)

	k.blockWaitGroupToSyncResources(wait.NeverStop, nil, ciliumV2Controller.HasSynced, k8sAPIGroupCiliumNetworkPolicyV2)
	go ciliumV2Controller.Run(wait.NeverStop)
	k.k8sAPIGroups.AddAPI(k8sAPIGroupCiliumNetworkPolicyV2)
}

```

以CNP创建事件为例进行分析

```go
// pkg/k8s/watchers/cilium_network_policy.go
func (k *K8sWatcher) addCiliumNetworkPolicyV2(ciliumNPClient clientset.Interface, cnp *types.SlimCNP) error {
	...
    // 解析cnp并返回api.Rules列表
	var rev uint64
	rules, policyImportErr := cnp.Parse()
	if policyImportErr == nil {
		policyImportErr = k8s.PreprocessRules(rules, &k.K8sSvcCache)
		// Replace all rules with the same name, namespace and
		// resourceTypeCiliumNetworkPolicy
		if policyImportErr == nil {
            // 添加rules到policy repository队列，用于分发给daemon
			rev, policyImportErr = k.policyManager.PolicyAdd(rules, &policy.AddOptions{
				ReplaceWithLabels: cnp.GetIdentityLabels(),
				Source:            metrics.LabelEventSourceK8s,
			})
		}
	}

	if policyImportErr != nil {
		metrics.PolicyImportErrorsTotal.Inc()
		scopedLog.WithError(policyImportErr).Warn("Unable to add CiliumNetworkPolicy")
	} else {
		scopedLog.Info("Imported CiliumNetworkPolicy")
	}

	// Upsert to rule revision cache outside of controller, because upsertion
	// *must* be synchronous so that if we get an update for the CNP, the cache
	// is populated by the time updateCiliumNetworkPolicyV2 is invoked.
	importMetadataCache.upsert(cnp, rev, policyImportErr)

	if !option.Config.DisableCNPStatusUpdates {
		updateContext := &k8s.CNPStatusUpdateContext{
			CiliumNPClient:              ciliumNPClient,
			NodeName:                    nodeTypes.GetName(),
			NodeManager:                 k.nodeDiscoverManager,
			UpdateDuration:              spanstat.Start(),
			WaitForEndpointsAtPolicyRev: k.endpointManager.WaitForEndpointsAtPolicyRev,
		}

		ctrlName := cnp.GetControllerName()
		k8sCM.UpdateController(ctrlName,
			controller.ControllerParams{
				DoFunc: func(ctx context.Context) error {
					return updateContext.UpdateStatus(ctx, cnp, rev, policyImportErr)
				},
			},
		)
	}

	return policyImportErr
}
```

```go
// daemon/cmd/policy.go
// rules列表添加到policy repository的RepositoryChangeQueue队列
func (d *Daemon) PolicyAdd(rules policyAPI.Rules, opts *policy.AddOptions) (newRev uint64, err error) {
	p := &PolicyAddEvent{
		rules: rules,
		opts:  opts,
		d:     d,
	}
	polAddEvent := eventqueue.NewEvent(p)
    // 入队改rules的事件对象
	resChan, err := d.policy.RepositoryChangeQueue.Enqueue(polAddEvent)
	if err != nil {
		return 0, fmt.Errorf("enqueue of PolicyAddEvent failed: %s", err)
	}

	res, ok := <-resChan
	if ok {
		pRes := res.(*PolicyAddResult)
		return pRes.newRev, pRes.err
	}
	return 0, fmt.Errorf("policy addition event was cancelled")
}
```

### datapath loader初始化

```go
// daemon/cmd/daemon_main.go
func runDaemon() {
	d, restoredEndpoints, err := NewDaemon(ctx, cancel,
		WithDefaultEndpointManager(ctx, endpoint.CheckHealth),
		linuxdatapath.NewDatapath(datapathConfig, iptablesManager, wgAgent))
}
-->
// pkg/datapath/linux/datapath.go
func NewDatapath(cfg DatapathConfiguration, ruleManager datapath.IptablesManager, wgAgent datapath.WireguardAgent) datapath.Datapath {
	dp := &linuxDatapath{
		ConfigWriter:    &config.HeaderfileWriter{},
		IptablesManager: ruleManager,
		nodeAddressing:  NewNodeAddressing(),
		config:          cfg,
		loader:          loader.NewLoader(canDisableDwarfRelocations),
		wgAgent:         wgAgent,
	}

	dp.node = NewNodeHandler(cfg, dp.nodeAddressing, wgAgent)
	return dp
}
-->
func (l *linuxDatapath) Loader() datapath.Loader {
	return l.loader
}
// pkg/datapath/loader/base.go
func (l *Loader) Reinitialize(ctx context.Context, o datapath.BaseProgramOwner, deviceMTU int, iptMgr datapath.IptablesManager, p datapath.Proxy) error {
}
```



### daemon组件policy repository初始化

```go
// daemon/cmd/policy.go
func (d *Daemon) initPolicy(epMgr *endpointmanager.EndpointManager) error {
	// Reuse policy.TriggerMetrics and PolicyTriggerInterval here since
	// this is only triggered by agent configuration changes for now and
	// should be counted in pol.TriggerMetrics.
	rt, err := trigger.NewTrigger(trigger.Parameters{
		Name:            "datapath-regeneration",
		MetricsObserver: &policy.TriggerMetrics{},
		MinInterval:     option.Config.PolicyTriggerInterval,
		TriggerFunc:     d.datapathRegen,
	})
	if err != nil {
		return fmt.Errorf("failed to create datapath regeneration trigger: %w", err)
	}
	d.datapathRegenTrigger = rt

    // 初始换policy组件
	d.policy = policy.NewPolicyRepository(d.identityAllocator,
		d.identityAllocator.GetIdentityCache(),
		certificatemanager.NewManager(option.Config.CertDirectory, k8s.Client()))
	d.policy.SetEnvoyRulesFunc(envoy.GetEnvoyHTTPRules)
	d.policyUpdater, err = policy.NewUpdater(d.policy, epMgr)
	if err != nil {
		return fmt.Errorf("failed to create policy update trigger: %w", err)
	}

	return nil
}
```



```go
// daemon/cmd/policy.go
// 创建policy repository实例
func NewPolicyRepository(idAllocator cache.IdentityAllocator, idCache cache.IdentityCache, certManager CertificateManager) *Repository {
    // 实例化repoChangeQueue、ruleReactionQueue队列并启动
	repoChangeQueue := eventqueue.NewEventQueueBuffered("repository-change-queue", option.Config.PolicyQueueSize)
	ruleReactionQueue := eventqueue.NewEventQueueBuffered("repository-reaction-queue", option.Config.PolicyQueueSize)
	repoChangeQueue.Run()
	ruleReactionQueue.Run()
	selectorCache := NewSelectorCache(idAllocator, idCache)

	repo := &Repository{
		revision:              1,
		RepositoryChangeQueue: repoChangeQueue,
		RuleReactionQueue:     ruleReactionQueue,
		selectorCache:         selectorCache,
		certManager:           certManager,
	}
	repo.policyCache = NewPolicyCache(repo, true)
	return repo
}

// pkg/eventqueue/eventqueue.go
// 队列启动
func (q *EventQueue) Run() {
	if q.notSafeToAccess() {
		return
	}

	go q.run()
}

func (q *EventQueue) run() {
	q.eventQueueOnce.Do(func() {
		defer close(q.eventsClosed)
		for ev := range q.events {
			select {
			case <-q.drain:
				ev.stats.waitConsumeOffQueue.End(false)
				close(ev.cancelled)
				close(ev.eventResults)
				ev.printStats(q)
			default:
				ev.stats.waitConsumeOffQueue.End(true)
				ev.stats.durationStat.Start()
                // 消费队列数据
				ev.Metadata.Handle(ev.eventResults)
				// Always indicate success for now.
				ev.stats.durationStat.End(true)
				// Ensures that no more results can be sent as the event has
				// already been processed.
				ev.printStats(q)
				close(ev.eventResults)
			}
		}
	})
}

// 处理repoChangeQueue队列数据
func (p *PolicyAddEvent) Handle(res chan interface{}) {
	p.d.policyAdd(p.rules, p.opts, res)
}

// rules规则最终通过该函数通知到所有locally endpoint managed
func (d *Daemon) policyAdd(sourceRules policyAPI.Rules, opts *policy.AddOptions, resChan chan interface{}) {
	policyAddStartTime := time.Now()
	logger := log.WithField("policyAddRequest", uuid.New().String())

	if opts != nil && opts.Generated {
		logger.WithField(logfields.CiliumNetworkPolicy, sourceRules.String()).Debug("Policy Add Request")
	} else {
		logger.WithField(logfields.CiliumNetworkPolicy, sourceRules.String()).Info("Policy Add Request")
	}
	
    // 解析rules里面各种CIDR，无差别加到slice里面返回
	prefixes := policy.GetCIDRPrefixes(sourceRules)
	logger.WithField("prefixes", prefixes).Debug("Policy imported via API, found CIDR prefixes...")

	newPrefixLengths, err := d.prefixLengths.Add(prefixes)
	if err != nil {
		logger.WithError(err).WithField("prefixes", prefixes).Warn(
			"Failed to reference-count prefix lengths in CIDR policy")
		resChan <- &PolicyAddResult{
			newRev: 0,
			err:    api.Error(PutPolicyFailureCode, err),
		}
		return
	}
    // 判断CIDR是否改变
	if newPrefixLengths && !bpfIPCache.BackedByLPM() {
		// 重新编译并初始化基础程序,具体实现参考datapath部分
		if err := d.Datapath().Loader().Reinitialize(d.ctx, d, d.mtuConfig.GetDeviceMTU(), d.Datapath(), d.l7Proxy); err != nil {
			_ = d.prefixLengths.Delete(prefixes)
			err2 := fmt.Errorf("Unable to recompile base programs: %s", err)
			logger.WithError(err2).WithField("prefixes", prefixes).Warn(
				"Failed to recompile base programs due to prefix length count change")
			resChan <- &PolicyAddResult{
				newRev: 0,
				err:    api.Error(PutPolicyFailureCode, err),
			}
			return
		}
	}

	// Any newly allocated identities MUST be upserted to the ipcache if no error is returned.
	// With SelectiveRegeneration this is postponed to the rule reaction queue to be done
	// after the affected endpoints have been regenerated, otherwise new identities are
	// upserted to the ipcache before we return.
	//
	// Release of these identities will be tied to the corresponding policy
	// in the policy.Repository and released upon policyDelete().
	newlyAllocatedIdentities := make(map[string]*identity.Identity)
	if _, err := ipcache.AllocateCIDRs(prefixes, nil, newlyAllocatedIdentities); err != nil {
		_ = d.prefixLengths.Delete(prefixes)
		logger.WithError(err).WithField("prefixes", prefixes).Warn(
			"Failed to allocate identities for CIDRs during policy add")
		resChan <- &PolicyAddResult{
			newRev: 0,
			err:    err,
		}
		return
	}

	// No errors past this point!

	d.policy.Mutex.Lock()

	// removedPrefixes tracks prefixes that we replace in the rules. It is used
	// after we release the policy repository lock.
	var removedPrefixes []*net.IPNet

	// policySelectionWG is used to signal when the updating of all of the
	// caches of endpoints in the rules which were added / updated have been
	// updated.
	var policySelectionWG sync.WaitGroup

	// Get all endpoints at the time rules were added / updated so we can figure
	// out which endpoints to regenerate / bump policy revision.
	allEndpoints := d.endpointManager.GetPolicyEndpoints()

	// Start with all endpoints to be in set for which we need to bump their
	// revision.
	endpointsToBumpRevision := policy.NewEndpointSet(allEndpoints)

	endpointsToRegen := policy.NewEndpointSet(nil)

	if opts != nil {
		if opts.Replace {
			for _, r := range sourceRules {
				oldRules := d.policy.SearchRLocked(r.Labels)
				removedPrefixes = append(removedPrefixes, policy.GetCIDRPrefixes(oldRules)...)
				if len(oldRules) > 0 {
					deletedRules, _, _ := d.policy.DeleteByLabelsLocked(r.Labels)
					deletedRules.UpdateRulesEndpointsCaches(endpointsToBumpRevision, endpointsToRegen, &policySelectionWG)
				}
			}
		}
		if len(opts.ReplaceWithLabels) > 0 {
			oldRules := d.policy.SearchRLocked(opts.ReplaceWithLabels)
			removedPrefixes = append(removedPrefixes, policy.GetCIDRPrefixes(oldRules)...)
			if len(oldRules) > 0 {
				deletedRules, _, _ := d.policy.DeleteByLabelsLocked(opts.ReplaceWithLabels)
				deletedRules.UpdateRulesEndpointsCaches(endpointsToBumpRevision, endpointsToRegen, &policySelectionWG)
			}
		}
	}

	addedRules, newRev := d.policy.AddListLocked(sourceRules)

	// The information needed by the caller is available at this point, signal
	// accordingly.
	resChan <- &PolicyAddResult{
		newRev: newRev,
		err:    nil,
	}

	addedRules.UpdateRulesEndpointsCaches(endpointsToBumpRevision, endpointsToRegen, &policySelectionWG)

	d.policy.Mutex.Unlock()

	if newPrefixLengths && !bpfIPCache.BackedByLPM() {
		// bpf_host needs to be recompiled whenever CIDR policy changed.
		if hostEp := d.endpointManager.GetHostEndpoint(); hostEp != nil {
			logger.Debug("CIDR policy has changed; regenerating host endpoint")
			endpointsToRegen.Insert(hostEp)
			endpointsToBumpRevision.Delete(hostEp)
		}
	}

	// Begin tracking the time taken to deploy newRev to the datapath. The start
	// time is from before the locking above, and thus includes all waits and
	// processing in this function.
	source := ""
	if opts != nil {
		source = opts.Source
	}
	d.endpointManager.CallbackForEndpointsAtPolicyRev(d.ctx, newRev, func(now time.Time) {
		duration, _ := safetime.TimeSinceSafe(policyAddStartTime, logger)
		metrics.PolicyImplementationDelay.WithLabelValues(source).Observe(duration.Seconds())
	})

	// remove prefixes of replaced rules above. Refcounts have been incremented
	// above, so any decrements here will be no-ops for CIDRs that are re-added,
	// and will trigger deletions for those that are no longer used.
	if len(removedPrefixes) > 0 {
		logger.WithField("prefixes", removedPrefixes).Debug("Decrementing replaced CIDR refcounts when adding rules")
		ipcache.ReleaseCIDRIdentitiesByCIDR(removedPrefixes)
		d.prefixLengths.Delete(removedPrefixes)
	}

	logger.WithField(logfields.PolicyRevision, newRev).Info("Policy imported via API, recalculating...")

	labels := make([]string, 0, len(sourceRules))
	for _, r := range sourceRules {
		labels = append(labels, r.Labels.GetModel()...)
	}
	err = d.SendNotification(monitorAPI.PolicyUpdateMessage(len(sourceRules), labels, newRev))
	if err != nil {
		logger.WithError(err).WithField(logfields.PolicyRevision, newRev).Warn("Failed to send policy update as monitor notification")
	}

	if option.Config.SelectiveRegeneration {
		// Only regenerate endpoints which are needed to be regenerated as a
		// result of the rule update. The rules which were imported most likely
		// do not select all endpoints in the policy repository (and may not
		// select any at all). The "reacting" to rule updates enqueues events
		// for all endpoints. Once all endpoints have events queued up, this
		// function will return.
		//
		// With selective regeneration upserting CIDRs to ipcache is performed after
		// endpoint regeneration and serialized with the corresponding ipcache deletes via
		// the policy reaction queue.
		r := &PolicyReactionEvent{
			wg:                &policySelectionWG,
			epsToBumpRevision: endpointsToBumpRevision,
			endpointsToRegen:  endpointsToRegen,
			newRev:            newRev,
			upsertIdentities:  newlyAllocatedIdentities,
		}

		ev := eventqueue.NewEvent(r)
		// This event may block if the RuleReactionQueue is full. We don't care
		// about when it finishes, just that the work it does is done in a serial
		// order.
		_, err := d.policy.RuleReactionQueue.Enqueue(ev)
		if err != nil {
			log.WithError(err).WithField(logfields.PolicyRevision, newRev).Error("enqueue of RuleReactionEvent failed")
		}
	} else {
		// Regenerate all endpoints unconditionally.
		d.TriggerPolicyUpdates(false, "policy rules added")
		// TODO: Remove 'enable-selective-regeneration' agent option.  Without selective
		// regeneration we retain the old behavior of upserting new identities to ipcache
		// before endpoint policy maps have been updated.
		ipcache.UpsertGeneratedIdentities(newlyAllocatedIdentities)
	}

	return
}

// 处理ruleReactionQueue队列数据
func (r *PolicyReactionEvent) Handle(res chan interface{}) {
	// Wait until we have calculated which endpoints need to be selected
	// across multiple goroutines.
	r.wg.Wait()
	reactToRuleUpdates(r.epsToBumpRevision, r.endpointsToRegen, r.newRev, r.upsertIdentities, r.releasePrefixes)
}
```

