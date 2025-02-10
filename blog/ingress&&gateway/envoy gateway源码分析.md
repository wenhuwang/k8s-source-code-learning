### Gateway启动流程
> 注意：以下代码使用分支为v0.5

```go
main()                                                                 // cmd/envoy-gateway/main.go
 |- GetRootCommand()                                                   // internal/cmd/root.go
     |- getServerCommand()                                             // internal/cmd/server.go
         |- server()                                                           
             |- load config file
             |- setupRunners()
                 |- providerRunner.Start()                            // internal/provider/runner/runner.go
                     |- kubernetes.New()                       // internal/provider/kubernetes/kubernetes.go
                         ｜- newGatewayAPIController()         // internal/provider/kubernetes/controller.go
                              |- r.watchResources()
                              |- r.Reconcile()
                     |- p.Start()
                 |- gwRunner.Start()                                 // internal/gatewayapi/runner/runner.go
                     ｜- r.subscribeAndTranslate()
                         ｜- HandleSubscription()                 // internal/message/watchutil.go 
                 |- xdsTranslatorRunner.Start()                  // internal/xds/translator/runner/runner.go
                 |- infraRunner.Start()                          // internal/infrastructure/runner/runner.go
                 |- xdsServerRunner.Start()                      // internal/xds/server/runner/runner.go
                 |- setupAdminServer()                           // internal/cmd/server.go
```



### 主要模块

#### 1. providerRunner 

从 provider 收集资源信息并发布出去供其他服务订阅；provider 暂时只支持 k8s

##### 模块源码逻辑

实例化 providerRunner 模块以及启动 Runner, 这里创建的 pResources 变量用于从 providerRunner 模块中收集 gateway 相关的所有资源信息并提供其他模块订阅的能力。

```go
// internal/cmd/server.go
func setupRunners(cfg *config.Server) error {
	pResources := new(message.ProviderResources)
	ePatchPolicyStatuses := new(message.EnvoyPatchPolicyStatuses)
	// Start the Provider Service
	// It fetches the resources from the configured provider type
	// and publishes it
	// It also subscribes to status resources and once it receives
	// a status resource back, it writes it out.
	providerRunner := providerrunner.New(&providerrunner.Config{
		Server:                   *cfg,
		ProviderResources:        pResources,
		EnvoyPatchPolicyStatuses: ePatchPolicyStatuses,
	})
	if err := providerRunner.Start(ctx); err != nil {
		return err
	}
}
```

providerRunner 模块启动逻辑

```go
// internal/provider/runner/runner.go
// 启动provider runner
func (r *Runner) Start(ctx context.Context) error {
	if r.EnvoyGateway.Provider.Type == v1alpha1.ProviderTypeKubernetes {
		...
    // 实例化kubernetes provider
		p, err := kubernetes.New(cfg, &r.Config.Server, r.ProviderResources, r.EnvoyPatchPolicyStatuses)
	  ...
    // 启动
		go func() {
			err := p.Start(ctx)
			...
		}()
		return nil
	}
	// Unsupported provider.
	return fmt.Errorf("unsupported provider type %v", r.EnvoyGateway.Provider.Type)
}

// internal/provider/kubernetes/kubernetes.go
func New(cfg *rest.Config, svr *config.Server, resources *message.ProviderResources, eStatuses *message.EnvoyPatchPolicyStatuses) (*Provider, error) {
	// 初始化manager options
	mgrOpts := manager.Options{
		Scheme:                 envoygateway.GetScheme(),
		Logger:                 svr.Logger.Logger,
		LeaderElection:         false,
		HealthProbeBindAddress: ":8081",
		LeaderElectionID:       "5b9825d2.gateway.envoyproxy.io",
		Metrics: metricsserver.Options{
			BindAddress: ":8080",
		},
	}

  // 如果配置了k8s watch的namespace,则配置manager只watch这些namespace的资源
	if svr.EnvoyGateway.Provider != nil &&
		svr.EnvoyGateway.Provider.Kubernetes != nil &&
		(svr.EnvoyGateway.Provider.Kubernetes.Watch != nil) &&
		(len(svr.EnvoyGateway.Provider.Kubernetes.Watch.Namespaces) > 0) {
		mgrOpts.Cache.DefaultNamespaces = make(map[string]cache.Config)
		for _, watchNS := range svr.EnvoyGateway.Provider.Kubernetes.Watch.Namespaces {
			mgrOpts.Cache.DefaultNamespaces[watchNS] = cache.Config{}
		}
	}

	mgr, err := ctrl.NewManager(cfg, mgrOpts)
  ...

	updateHandler := status.NewUpdateHandler(mgr.GetLogger(), mgr.GetClient())
	if err := mgr.Add(updateHandler); err != nil {
		return nil, fmt.Errorf("failed to add status update handler %v", err)
	}

	// 创建并注册controller到manager.
	if err := newGatewayAPIController(mgr, svr, updateHandler.Writer(), resources, eStatuses); err != nil {
		return nil, fmt.Errorf("failted to create gatewayapi controller: %w", err)
	}
	...
	return &Provider{
		manager: mgr,
		client:  mgr.GetClient(),
	}, nil
}


// internal/provider/kubernetes/controller.go
func newGatewayAPIController(mgr manager.Manager, cfg *config.Server, su status.Updater,
	resources *message.ProviderResources, eStatuses *message.EnvoyPatchPolicyStatuses) error {
	...
	r := &gatewayAPIReconciler{
		client:                   mgr.GetClient(),
		log:                      cfg.Logger,
		classController:          gwapiv1b1.GatewayController(cfg.EnvoyGateway.Gateway.ControllerName),
		namespace:                cfg.Namespace,
		statusUpdater:            su,
		resources:                resources,
		envoyPatchPolicyStatuses: eStatuses,
		extGVKs:                  extGVKs,
		store:                    newProviderStore(),
		envoyGateway:             cfg.EnvoyGateway,
	}

	c, err := controller.New("gatewayapi", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}
	r.log.Info("created gatewayapi controller")

	// Subscribe to status updates
	r.subscribeAndUpdateStatus(ctx)

	// controller watch资源逻辑
	if err := r.watchResources(ctx, mgr, c); err != nil {
		return err
	}
	return nil
}
```

控制器 watch 资源逻辑，配置 controller 监听资源、事件过滤函数、资源入队列前的重写函数等；这里所有资源事件进入队列前的重写函数都是 `r.enqueueClass` , 这个函数会把所有进入队列资源名改为`cfg.EnvoyGateway.Gateway.ControllerName`

```go
// internal/provider/kubernetes/controller.go
// watchResources watches gateway api resources.
func (r *gatewayAPIReconciler) watchResources(ctx context.Context, mgr manager.Manager, c controller.Controller) error {
	// Only enqueue GatewayClass objects that match this Envoy Gateway's controller name.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &gwapiv1b1.GatewayClass{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
		predicate.NewPredicateFuncs(r.hasMatchingController),
	); err != nil {
		return err
	}

	// Only enqueue EnvoyProxy objects that match this Envoy Gateway's GatewayClass.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &egcfgv1a1.EnvoyProxy{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
		predicate.ResourceVersionChangedPredicate{},
		predicate.NewPredicateFuncs(r.hasManagedClass),
	); err != nil {
		return err
	}

	// Watch Gateway CRUDs and reconcile affected GatewayClass.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &gwapiv1b1.Gateway{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
		predicate.NewPredicateFuncs(r.validateGatewayForReconcile),
	); err != nil {
		return err
	}
	if err := addGatewayIndexers(ctx, mgr); err != nil {
		return err
	}

	// Watch HTTPRoute CRUDs and process affected Gateways.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &gwapiv1b1.HTTPRoute{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
	); err != nil {
		return err
	}
	if err := addHTTPRouteIndexers(ctx, mgr); err != nil {
		return err
	}

	// Watch GRPCRoute CRUDs and process affected Gateways.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &gwapiv1a2.GRPCRoute{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
	); err != nil {
		return err
	}
	if err := addGRPCRouteIndexers(ctx, mgr); err != nil {
		return err
	}

	// Watch TLSRoute CRUDs and process affected Gateways.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &gwapiv1a2.TLSRoute{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
	); err != nil {
		return err
	}
	if err := addTLSRouteIndexers(ctx, mgr); err != nil {
		return err
	}

	// Watch UDPRoute CRUDs and process affected Gateways.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &gwapiv1a2.UDPRoute{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
	); err != nil {
		return err
	}
	if err := addUDPRouteIndexers(ctx, mgr); err != nil {
		return err
	}

	// Watch TCPRoute CRUDs and process affected Gateways.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &gwapiv1a2.TCPRoute{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
	); err != nil {
		return err
	}
	if err := addTCPRouteIndexers(ctx, mgr); err != nil {
		return err
	}

	// Watch Service CRUDs and process affected *Route objects.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &corev1.Service{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
		predicate.NewPredicateFuncs(r.validateServiceForReconcile)); err != nil {
		return err
	}

	serviceImportCRDExists := r.serviceImportCRDExists(mgr)
	if !serviceImportCRDExists {
		r.log.Info("ServiceImport CRD not found, skipping ServiceImport watch")
	}

	// Watch ServiceImport CRUDs and process affected *Route objects.
	if serviceImportCRDExists {
		if err := c.Watch(
			source.Kind(mgr.GetCache(), &mcsapi.ServiceImport{}),
			handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
			predicate.NewPredicateFuncs(r.validateServiceImportForReconcile)); err != nil {
			// ServiceImport is not available in the cluster, skip the watch and not throw error.
			r.log.Info("unable to watch ServiceImport: %s", err.Error())
		}
	}

	// Watch EndpointSlice CRUDs and process affected *Route objects.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &discoveryv1.EndpointSlice{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
		predicate.NewPredicateFuncs(r.validateEndpointSliceForReconcile)); err != nil {
		return err
	}

	// Watch Node CRUDs to update Gateway Address exposed by Service of type NodePort.
	// Node creation/deletion and ExternalIP updates would require update in the Gateway
	// resource address.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &corev1.Node{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
		predicate.NewPredicateFuncs(r.handleNode),
	); err != nil {
		return err
	}

	// Watch Secret CRUDs and process affected Gateways.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &corev1.Secret{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
		predicate.NewPredicateFuncs(r.validateSecretForReconcile),
	); err != nil {
		return err
	}

	// Watch ReferenceGrant CRUDs and process affected Gateways.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &gwapiv1a2.ReferenceGrant{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
	); err != nil {
		return err
	}
	if err := addReferenceGrantIndexers(ctx, mgr); err != nil {
		return err
	}

	// Watch Deployment CRUDs and process affected Gateways.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &appsv1.Deployment{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
		predicate.NewPredicateFuncs(r.validateDeploymentForReconcile),
	); err != nil {
		return err
	}

	// Watch AuthenticationFilter CRUDs and enqueue associated HTTPRoute objects.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &egv1a1.AuthenticationFilter{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
		predicate.NewPredicateFuncs(r.httpRoutesForAuthenticationFilter)); err != nil {
		return err
	}

	// Watch RateLimitFilter CRUDs and enqueue associated HTTPRoute objects.
	if err := c.Watch(
		source.Kind(mgr.GetCache(), &egv1a1.RateLimitFilter{}),
		handler.EnqueueRequestsFromMapFunc(r.enqueueClass),
		predicate.NewPredicateFuncs(r.httpRoutesForRateLimitFilter)); err != nil {
		return err
	}

	// Watch EnvoyPatchPolicy if enabled in config
	if r.envoyGateway.ExtensionAPIs != nil && r.envoyGateway.ExtensionAPIs.EnableEnvoyPatchPolicy {
		// Watch EnvoyPatchPolicy CRUDs
		if err := c.Watch(
			source.Kind(mgr.GetCache(), &egv1a1.EnvoyPatchPolicy{}),
			handler.EnqueueRequestsFromMapFunc(r.enqueueClass)); err != nil {
			return err
		}
	}

	r.log.Info("Watching gatewayAPI related objects")

	// Watch any additional GVKs from the registered extension.
	...
}
```

 Reconciler 函数逻辑将所有 gateway 相关所有资源的收集到 resourceTree 变量，然后更新到 r.resources.GatewayAPIResources

```go
// Reconcile handles reconciling all resources in a single call. Any resource event should enqueue the
// same reconcile.Request containing the gateway controller name. This allows multiple resource updates to
// be handled by a single call to Reconcile. The reconcile.Request DOES NOT map to a specific resource.
func (r *gatewayAPIReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
 // 遍历所有gatewayClass，如果controllerName和r.classController一样的将gatewayclass加入到cc.matchedClasses
	var gatewayClasses gwapiv1b1.GatewayClassList
	if err := r.client.List(ctx, &gatewayClasses); err != nil {
		return reconcile.Result{}, fmt.Errorf("error listing gatewayclasses: %v", err)
	}
	var cc controlledClasses
	for _, gwClass := range gatewayClasses.Items {
		gwClass := gwClass
		if gwClass.Spec.ControllerName == r.classController {
			// The gatewayclass was marked for deletion and the finalizer removed,
			// so clean-up dependents.
			if !gwClass.DeletionTimestamp.IsZero() &&
				!slice.ContainsString(gwClass.Finalizers, gatewayClassFinalizer) {
				r.log.Info("gatewayclass marked for deletion")
				cc.removeMatch(&gwClass)

				// Delete the gatewayclass from the watchable map.
				r.resources.GatewayAPIResources.Delete(gwClass.Name)
				continue
			}

			cc.addMatch(&gwClass)
		}
	}

	...
	// Update status for all gateway classes
	for _, gc := range cc.notAcceptedClasses() {
		if err := r.gatewayClassUpdater(ctx, gc, false, string(status.ReasonOlderGatewayClassExists),
			status.MsgOlderGatewayClassExists); err != nil {
			r.resources.GatewayAPIResources.Delete(acceptedGC.Name)
			return reconcile.Result{}, err
		}
	}

	// Initialize resource types.
	resourceTree := gatewayapi.NewResources()
	resourceMap := newResourceMapping()

  // 获取到acceptedGC相关的gateway，遍历所有gateway资源将所有开启tls的gateway的secret资源以及TLSRoute/HTTPRoute/GRPCRoute/TCPRoute/UDPRoute/gateway/namespace/backendRef资源追加到resourceMap和resourceTree
	if err := r.processGateways(ctx, acceptedGC, resourceMap, resourceTree); err != nil {
		return reconcile.Result{}, err
	}

  // 遍历allAssociatedBackendRefs将所有后端服务对应的service/serviceimport/endpointslice资源追加到resourceTree，并更新namespace到resourceMap
	for backendRef := range resourceMap.allAssociatedBackendRefs {
		backendRefKind := gatewayapi.KindDerefOr(backendRef.Kind, gatewayapi.KindService)
		r.log.Info("processing Backend", "kind", backendRefKind, "namespace", string(*backendRef.Namespace),
			"name", string(backendRef.Name))

		var endpointSliceLabelKey string
		switch backendRefKind {
		case gatewayapi.KindService:
			service := new(corev1.Service)
			err := r.client.Get(ctx, types.NamespacedName{Namespace: string(*backendRef.Namespace), Name: string(backendRef.Name)}, service)
			if err != nil {
				...
			} else {
				resourceMap.allAssociatedNamespaces[service.Namespace] = struct{}{}
				resourceTree.Services = append(resourceTree.Services, service)
				r.log.Info("added Service to resource tree", "namespace", string(*backendRef.Namespace),
					"name", string(backendRef.Name))
			}
			endpointSliceLabelKey = discoveryv1.LabelServiceName

		case gatewayapi.KindServiceImport:
			serviceImport := new(mcsapi.ServiceImport)
			err := r.client.Get(ctx, types.NamespacedName{Namespace: string(*backendRef.Namespace), Name: string(backendRef.Name)}, serviceImport)
			if err != nil {
				...
			} else {
				resourceMap.allAssociatedNamespaces[serviceImport.Namespace] = struct{}{}
				resourceTree.ServiceImports = append(resourceTree.ServiceImports, serviceImport)
				r.log.Info("added ServiceImport to resource tree", "namespace", string(*backendRef.Namespace),
					"name", string(backendRef.Name))
			}
			endpointSliceLabelKey = mcsapi.LabelServiceName
		}

		// Retrieve the EndpointSlices associated with the service
		endpointSliceList := new(discoveryv1.EndpointSliceList)
		opts := []client.ListOption{
			client.MatchingLabels(map[string]string{
				endpointSliceLabelKey: string(backendRef.Name),
			}),
			client.InNamespace(string(*backendRef.Namespace)),
		}
		if err := r.client.List(ctx, endpointSliceList, opts...); err != nil {
			r.log.Error(err, "failed to get EndpointSlices", "namespace", string(*backendRef.Namespace),
				backendRefKind, string(backendRef.Name))
		} else {
			for _, endpointSlice := range endpointSliceList.Items {
				endpointSlice := endpointSlice
				r.log.Info("added EndpointSlice to resource tree", "namespace", endpointSlice.Namespace,
					"name", endpointSlice.Name)
				resourceTree.EndpointSlices = append(resourceTree.EndpointSlices, &endpointSlice)
			}
		}
	}

	// Add all ReferenceGrants to the resourceTree
	for _, referenceGrant := range resourceMap.allAssociatedRefGrants {
		resourceTree.ReferenceGrants = append(resourceTree.ReferenceGrants, referenceGrant)
	}

	// Add all EnvoyPatchPolicies
	...

	// 检查resourceMap中的namespace是否存在，存在的话加入resourceTree
	for ns := range resourceMap.allAssociatedNamespaces {
		namespace, err := r.getNamespace(ctx, ns)
		if err != nil {
			...
		}
		resourceTree.Namespaces = append(resourceTree.Namespaces, namespace)
	}

	// Process the parametersRef of the accepted GatewayClass.
	if acceptedGC.Spec.ParametersRef != nil && acceptedGC.DeletionTimestamp == nil {
		if err := r.processParamsRef(ctx, acceptedGC, resourceTree); err != nil {
			msg := fmt.Sprintf("%s: %v", status.MsgGatewayClassInvalidParams, err)
			if err := r.gatewayClassUpdater(ctx, acceptedGC, false, string(gwapiv1b1.GatewayClassReasonInvalidParameters), msg); err != nil {
				r.log.Error(err, "unable to update GatewayClass status")
			}
			r.log.Error(err, "failed to process parametersRef for gatewayclass", "name", acceptedGC.Name)
			return reconcile.Result{}, err
		}
	}

	if err := r.gatewayClassUpdater(ctx, acceptedGC, true, string(gwapiv1b1.GatewayClassReasonAccepted), status.MsgValidGatewayClass); err != nil {
		r.log.Error(err, "unable to update GatewayClass status")
		return reconcile.Result{}, err
	}

	// Update finalizer on the gateway class based on the resource tree.
	...
  
  // 更新resourceTree资源到r.resources.GatewayAPIResources
	// The Store is triggered even when there are no Gateways associated to the
	// GatewayClass. This would happen in case the last Gateway is removed and the
	// Store will be required to trigger a cleanup of envoy infra resources.
	r.resources.GatewayAPIResources.Store(acceptedGC.Name, resourceTree)

	r.log.Info("reconciled gateways successfully")
	return reconcile.Result{}, nil
}
```



#### 2. gwRunner

订阅 provider 服务维护的资源信息，转换为 xds IR 和 infra IR 资源并发布出去

##### 模块源码逻辑

###### gwRunner 入口

实例化 gwRunner 并启动 Runner 服务，并通过 pResources 变量订阅 provider 更新的资源信息

```go
func setupRunners(cfg *config.Server) error {
  ...
  xdsIR := new(message.XdsIR)
	infraIR := new(message.InfraIR)
	// Start the GatewayAPI Translator Runner
	// It subscribes to the provider resources, translates it to xDS IR
	// and infra IR resources and publishes them.
	gwRunner := gatewayapirunner.New(&gatewayapirunner.Config{
		Server:            *cfg,
		ProviderResources: pResources,
		XdsIR:             xdsIR,
		InfraIR:           infraIR,
		ExtensionManager:  extMgr,
	})
	if err := gwRunner.Start(ctx); err != nil {
		return err
	}
  ...
}
```

###### gwRunner 模块定义

gwRunner 结构体及 subscribeAndTranslate 定义

```go
// internal/gatewayapi/runner/runner.go
// Start starts the gateway-api translator runner
func (r *Runner) Start(ctx context.Context) error {
	...
	go r.subscribeAndTranslate(ctx)
	...
}

// internal/gatewayapi/runner/runner.go
func (r *Runner) subscribeAndTranslate(ctx context.Context) {
  // 订阅r.ProviderResources.GatewayAPIResources资源消息，通过handle函数处理该消息
	message.HandleSubscription(r.ProviderResources.GatewayAPIResources.Subscribe(ctx),
		func(update message.Update[string, *gatewayapi.Resources]) {
			r.Logger.Info("received an update")

			val := update.Value

			if update.Delete || val == nil {
				return
			}

			// Translate and publish IRs.
			t := &gatewayapi.Translator{
				GatewayControllerName:  r.Server.EnvoyGateway.Gateway.ControllerName,
				GatewayClassName:       v1beta1.ObjectName(update.Key),
				GlobalRateLimitEnabled: r.EnvoyGateway.RateLimit != nil,
			}

			// If an extension is loaded, pass its supported groups/kinds to the translator
			...
      
			// 把gatewayapi.Resources转换为xdsIR和infraIR，并把gateways/httpRoutes/grpcRoutes/tlsRoutes/tcpRoutes/udpRoutes/xdsIR/infraIR等所有资源更新到result中
			result := t.Translate(val)

			yamlXdsIR, _ := yaml.Marshal(&result.XdsIR)
			r.Logger.WithValues("output", "xds-ir").Info(string(yamlXdsIR))
			yamlInfraIR, _ := yaml.Marshal(&result.InfraIR)
			r.Logger.WithValues("output", "infra-ir").Info(string(yamlInfraIR))

			var curKeys, newKeys []string
			// Get current IR keys
			for key := range r.InfraIR.LoadAll() {
				curKeys = append(curKeys, key)
			}

      // 将InfraIR/XdsIR资源存储到Runner对象中，利用watchable.Map[]对象的发布/订阅功能让其他Runner获取到数据更新
			// Publish the IRs.
			// Also validate the ir before sending it.
			for key, val := range result.InfraIR {
				if err := val.Validate(); err != nil {
					r.Logger.Error(err, "unable to validate infra ir, skipped sending it")
				} else {
					r.InfraIR.Store(key, val)
					newKeys = append(newKeys, key)
				}
			}

			for key, val := range result.XdsIR {
				if err := val.Validate(); err != nil {
					r.Logger.Error(err, "unable to validate xds ir, skipped sending it")
				} else {
					r.XdsIR.Store(key, val)
				}
			}

      // 从InfraIR/XdsIR map中把上面步骤验证错误的key删除掉
			// Delete keys
			// There is a 1:1 mapping between infra and xds IR keys
			delKeys := getIRKeysToDelete(curKeys, newKeys)
			for _, key := range delKeys {
				r.InfraIR.Delete(key)
				r.XdsIR.Delete(key)
			}

			// Update Status
			for _, gateway := range result.Gateways {
				key := utils.NamespacedName(gateway)
				r.ProviderResources.GatewayStatuses.Store(key, &gateway.Status)
			}
			for _, httpRoute := range result.HTTPRoutes {
				key := utils.NamespacedName(httpRoute)
				r.ProviderResources.HTTPRouteStatuses.Store(key, &httpRoute.Status)
			}
			for _, grpcRoute := range result.GRPCRoutes {
				key := utils.NamespacedName(grpcRoute)
				r.ProviderResources.GRPCRouteStatuses.Store(key, &grpcRoute.Status)
			}

			for _, tlsRoute := range result.TLSRoutes {
				key := utils.NamespacedName(tlsRoute)
				r.ProviderResources.TLSRouteStatuses.Store(key, &tlsRoute.Status)
			}
			for _, tcpRoute := range result.TCPRoutes {
				key := utils.NamespacedName(tcpRoute)
				r.ProviderResources.TCPRouteStatuses.Store(key, &tcpRoute.Status)
			}
			for _, udpRoute := range result.UDPRoutes {
				key := utils.NamespacedName(udpRoute)
				r.ProviderResources.UDPRouteStatuses.Store(key, &udpRoute.Status)
			}
		},
	)
	r.Logger.Info("shutting down")
}
```

###### gatewayapi Resources 解析

Translate 函数把 gatewayapi.Resources 转换为 xdsIR 和 infraIR

```go
// internal/gaetwayapi/translator.go
func (t *Translator) Translate(resources *Resources) *TranslateResult {
	xdsIR := make(XdsIRMap)
	infraIR := make(InfraIRMap)

	// Get Gateways belonging to our GatewayClass.
	gateways := t.GetRelevantGateways(resources.Gateways)

	// 验证所有gateway中的listeners配置并将其转化为xdsIR.HTTP和infraIR.Proxy.Listeners[0].Ports
	t.ProcessListeners(gateways, xdsIR, infraIR, resources)

	// Process EnvoyPatchPolicies
	t.ProcessEnvoyPatchPolicies(resources.EnvoyPatchPolicies, xdsIR)

	// 验证gateway的address配置并将其更新到infraIR.Proxy.Addresses
	t.ProcessAddresses(gateways, xdsIR, infraIR, resources)

	// Process all relevant HTTPRoutes.
	httpRoutes := t.ProcessHTTPRoutes(resources.HTTPRoutes, gateways, resources, xdsIR)

	// Process all relevant GRPCRoutes.
	grpcRoutes := t.ProcessGRPCRoutes(resources.GRPCRoutes, gateways, resources, xdsIR)

	// Process all relevant TLSRoutes.
	tlsRoutes := t.ProcessTLSRoutes(resources.TLSRoutes, gateways, resources, xdsIR)

	// Process all relevant TCPRoutes.
	tcpRoutes := t.ProcessTCPRoutes(resources.TCPRoutes, gateways, resources, xdsIR)

	// Process all relevant UDPRoutes.
	udpRoutes := t.ProcessUDPRoutes(resources.UDPRoutes, gateways, resources, xdsIR)

	// Sort xdsIR based on the Gateway API spec
	sortXdsIRMap(xdsIR)

  // 构造TranslateResult, 将gateways/httpRoutes/grpcRoutes/tlsRoutes/tcpRoutes/udpRoutes/xdsIR/infraIR资源全部填充到translateResult结构体
	return newTranslateResult(gateways, httpRoutes, grpcRoutes, tlsRoutes, tcpRoutes, udpRoutes, xdsIR, infraIR)
}
```

###### HTTPRoutes 资源解析

```go
// internal/gatewayapi/route.go
func (t *Translator) ProcessHTTPRoutes(httpRoutes []*v1beta1.HTTPRoute, gateways []*GatewayContext, resources *Resources, xdsIR XdsIRMap) []*HTTPRouteContext {
	var relevantHTTPRoutes []*HTTPRouteContext

	for _, h := range httpRoutes {
		...
		httpRoute := &HTTPRouteContext{
			GatewayControllerName: t.GatewayControllerName,
			HTTPRoute:             h.DeepCopy(),
		}

		// 判断httpRoute是否属于gateways管理并生成status
		relevantRoute := t.processAllowedListenersForParentRefs(httpRoute, gateways, resources)
		if !relevantRoute {
			continue
		}

		relevantHTTPRoutes = append(relevantHTTPRoutes, httpRoute)

    // 更新xdsIR.HTTP.Route
		t.processHTTPRouteParentRefs(httpRoute, resources, xdsIR)
	}

	return relevantHTTPRoutes
}

// internal/gatewayapi/route.go
func (t *Translator) processHTTPRouteParentRefs(httpRoute *HTTPRouteContext, resources *Resources, xdsIR XdsIRMap) {
  // 不同的ParentRefs对应不同的gateway，需要单独生成配置
	for _, parentRef := range httpRoute.ParentRefs {
		// 根据httpRoute资源backendRefs、Matches、Filters等配置生成routeRoutes
		routeRoutes := t.processHTTPRouteRules(httpRoute, parentRef, resources)

		// If no negative condition has been set for ResolvedRefs, set "ResolvedRefs=True"
		...

    // 遍历parentRef.listeners，先根据listener.Hostname和httpRoute.Spec.Hostnames生成hosts，然后两层遍历hosts和routeRoutes数组使用host变量以及routeRoute生成最终的routeRoutes对象，最后把routeRoutes添加到xdsIR.HTTP.Route
		var hasHostnameIntersection = t.processHTTPRouteParentRefListener(httpRoute, routeRoutes, parentRef, xdsIR)
		...

		// If no negative conditions have been set, the route is considered "Accepted=True".
		...
	}
}


```

processHTTPRouteRules 函数会根据httpRoute资源backendRefs、Matches、Filters等配置生成 routeRoutes 列表数据，这里会使用 httpRoute 资源关联的 Service 资源的 ClusterIP、servicePort 、weight 生成 ir.HTTPRoute.Destination.Endpoints 结构，这个 Endpoints 可以理解为 envoy 代理的后端节点配置。可以看出来当前 Envoy Gateway v0.5 版本还是使用 k8s Service IP/Port 作为 envoy 后端节点，在社区 issues [#1256](https://github.com/envoyproxy/gateway/issues/1256) 有看到社区正在计划迁移到 EndpointSlice 方案。

```go
// internal/gatewayapi/route.go
func (t *Translator) processHTTPRouteRules(httpRoute *HTTPRouteContext, parentRef *RouteParentContext, resources *Resources) []*ir.HTTPRoute {
	var routeRoutes []*ir.HTTPRoute

	// compute matches, filters, backends
	for ruleIdx, rule := range httpRoute.Spec.Rules {
    // 遍历检查rule.Filters配置并将所有过滤器(URLRewrite/RequestRedirect/RequestHeaderModifier/ResponseHeaderModifier等等)配置更新到httpFiltersContext
		httpFiltersContext := t.ProcessHTTPFilters(parentRef, httpRoute, rule.Filters, ruleIdx, resources)

		// 遍历检查rule.Matches配置并将其配置转化为[]*ir.HTTPRoute
		var ruleRoutes = t.processHTTPRouteRule(httpRoute, ruleIdx, httpFiltersContext, rule)

		for _, backendRef := range rule.BackendRefs {
      // 根据backendRef对应的service ip、port、weight构造出enpoints和backendWeight
			endpoints, backendWeight := t.processDestEndpoints(backendRef.BackendRef, parentRef, httpRoute, resources)
			for _, route := range ruleRoutes {
				// If the route already has a direct response or redirect configured, then it was from a filter so skip
				// processing any destinations for this route.
				if route.DirectResponse == nil && route.Redirect == nil {
					if len(endpoints) > 0 {
						if route.Destination == nil {
							route.Destination = &ir.RouteDestination{
								Name: irRouteDestinationName(httpRoute, ruleIdx),
							}
						}
						route.Destination.Endpoints = append(route.Destination.Endpoints, endpoints...)
						route.BackendWeights.Valid += backendWeight

					} else {
						route.BackendWeights.Invalid += backendWeight
					}
				}
			}
		}

		// 遍历ruleRoutes如果存在ruleRoute没有endpoint则将响应code设置为500
		for _, ruleRoute := range ruleRoutes {
			if ruleRoute.BackendWeights.Invalid > 0 && ruleRoute.Destination == nil {
				ruleRoute.DirectResponse = &ir.DirectResponse{
					StatusCode: 500,
				}
			}
		}
		routeRoutes = append(routeRoutes, ruleRoutes...)
	}

	return routeRoutes
}
```

###### GRPCRoutes 资源解析

###### TLSRoutes  资源解析

###### TCPRoutes  资源解析

###### UDPRoutes  资源解析



#### 3. xdsTranslatorRunner 

订阅 xds IR 资源，转换为 xds 资源并发布出去

##### 模块源码逻辑

###### xdsTranslatorRunner 入口

实例化 xdsTranslatorRunner 并订阅 xdsIR 资源的更新

```go
func setupRunners(cfg *config.Server) error {
  ...
 xds := new(message.Xds)
	// Start the Xds Translator Service
	// It subscribes to the xdsIR, translates it into xds Resources and publishes it.
	// It also computes the EnvoyPatchPolicy statuses and publishes it.
	xdsTranslatorRunner := xdstranslatorrunner.New(&xdstranslatorrunner.Config{
		Server:                   *cfg,
		XdsIR:                    xdsIR,
		Xds:                      xds,
		ExtensionManager:         extMgr,
		EnvoyPatchPolicyStatuses: ePatchPolicyStatuses,
	})
	if err := xdsTranslatorRunner.Start(ctx); err != nil {
		return err
	}
  ...
}
```

###### xdsTranslatorRunner 模块定义

xdsTranslatorRunner 结构体及 subscribeAndTranslate 定义

```go
// internal/xds/translator/runner/runner.go
type Config struct {
	config.Server
	XdsIR                    *message.XdsIR
	Xds                      *message.Xds
	EnvoyPatchPolicyStatuses *message.EnvoyPatchPolicyStatuses
	ExtensionManager         extension.Manager
}

type Runner struct {
	Config
}

func New(cfg *Config) *Runner {
	return &Runner{Config: *cfg}
}

func (r *Runner) subscribeAndTranslate(ctx context.Context) {
	// Subscribe to resources
	message.HandleSubscription(r.XdsIR.Subscribe(ctx),
		func(update message.Update[string, *ir.Xds]) {
			r.Logger.Info("received an update")
			key := update.Key
			val := update.Value

			if update.Delete {
				r.Xds.Delete(key)
			} else {
				// Translate to xds resources
				t := &translator.Translator{}

				// Set the extension manager if an extension is loaded
				if r.ExtensionManager != nil {
					t.ExtensionManager = &r.ExtensionManager
				}

				// Set the rate limit service URL if global rate limiting is enabled.
				if r.EnvoyGateway.RateLimit != nil {
					t.GlobalRateLimit = &translator.GlobalRateLimitSettings{
						ServiceURL: ratelimit.GetServiceURL(r.Namespace, r.DNSDomain),
						FailClosed: r.EnvoyGateway.RateLimit.FailClosed,
					}
					if r.EnvoyGateway.RateLimit.Timeout != nil {
						t.GlobalRateLimit.Timeout = r.EnvoyGateway.RateLimit.Timeout.Duration
					}
				}

				result, err := t.Translate(val)

				// Publish EnvoyPatchPolicyStatus
				for _, e := range result.EnvoyPatchPolicyStatuses {
					key := ktypes.NamespacedName{
						Name:      e.Name,
						Namespace: e.Namespace,
					}
					r.EnvoyPatchPolicyStatuses.Store(key, e.Status)
				}
				// Discard the EnvoyPatchPolicyStatuses to reduce memory footprint
				result.EnvoyPatchPolicyStatuses = nil

				if err != nil {
					r.Logger.Error(err, "failed to translate xds ir")
				} else {
					// Publish
					r.Xds.Store(key, result)
				}
			}
		},
	)
	r.Logger.Info("subscriber shutting down")
}
```

###### XdsIR 资源解析

```go
// Translate translates the XDS IR into xDS resources
func (t *Translator) Translate(ir *ir.Xds) (*types.ResourceVersionTable, error) {
	if ir == nil {
		return nil, errors.New("ir is nil")
	}

	tCtx := new(types.ResourceVersionTable)

  //
	if err := t.processHTTPListenerXdsTranslation(tCtx, ir.HTTP, ir.AccessLog, ir.Tracing); err != nil {
		return nil, err
	}

	if err := processTCPListenerXdsTranslation(tCtx, ir.TCP, ir.AccessLog); err != nil {
		return nil, err
	}

	if err := processUDPListenerXdsTranslation(tCtx, ir.UDP, ir.AccessLog); err != nil {
		return nil, err
	}

	if err := processJSONPatches(tCtx, ir.EnvoyPatchPolicies); err != nil {
		return nil, err
	}

	if err := processClusterForAccessLog(tCtx, ir.AccessLog); err != nil {
		return nil, err
	}
	if err := processClusterForTracing(tCtx, ir.Tracing); err != nil {
		return nil, err
	}

	// Check if an extension want to inject any clusters/secrets
	// If no extension exists (or it doesn't subscribe to this hook) then this is a quick no-op
	if err := processExtensionPostTranslationHook(tCtx, t.ExtensionManager); err != nil {
		return nil, err
	}

	return tCtx, nil
}
```

###### XdsIR HTTPListener 资源解析

```go
func (t *Translator) processHTTPListenerXdsTranslation(tCtx *types.ResourceVersionTable, httpListeners []*ir.HTTPListener,
	accesslog *ir.AccessLog, tracing *ir.Tracing) error {
	for _, httpListener := range httpListeners {
		addFilterChain := true
		var xdsRouteCfg *routev3.RouteConfiguration

    // 根据httpListener Address、Port、协议从tCtx查找XdsListener，如果没有就创建一个并添加到tCtx.XdsResources数组
		// Search for an existing listener, if it does not exist, create one.
		xdsListener := findXdsListenerByHostPort(tCtx, httpListener.Address, httpListener.Port, corev3.SocketAddress_TCP)
		if xdsListener == nil {
			xdsListener = buildXdsTCPListener(httpListener.Name, httpListener.Address, httpListener.Port, accesslog)
      // 先验证xdsListener资源并添加到tCtx.XdsResources数组
			if err := tCtx.AddXdsResource(resourcev3.ListenerType, xdsListener); err != nil {
				return err
			}
		} else if httpListener.TLS == nil {
			// Find the route config associated with this listener that
			// maps to the default filter chain for http traffic
			routeName := findXdsHTTPRouteConfigName(xdsListener)
			if routeName != "" {
				// If an existing listener exists, dont create a new filter chain
				// for HTTP traffic, match on the Domains field within VirtualHosts
				// within the same RouteConfiguration instead
				addFilterChain = false
				xdsRouteCfg = findXdsRouteConfig(tCtx, routeName)
				if xdsRouteCfg == nil {
					return errors.New("unable to find xds route config")
				}
			}
		}

		if addFilterChain {
			if err := t.addXdsHTTPFilterChain(xdsListener, httpListener, accesslog, tracing); err != nil {
				return err
			}
		}

		// Create a route config if we have not found one yet
		if xdsRouteCfg == nil {
			xdsRouteCfg = &routev3.RouteConfiguration{
				IgnorePortInHostMatching: true,
				Name:                     httpListener.Name,
			}

			if err := tCtx.AddXdsResource(resourcev3.RouteType, xdsRouteCfg); err != nil {
				return err
			}
		}

		// 1:1 between IR TLSListenerConfig and xDS Secret
		if httpListener.TLS != nil {
			for t := range httpListener.TLS {
				secret := buildXdsDownstreamTLSSecret(httpListener.TLS[t])
				if err := tCtx.AddXdsResource(resourcev3.SecretType, secret); err != nil {
					return err
				}
			}
		}

		protocol := DefaultProtocol
		if httpListener.IsHTTP2 {
			protocol = HTTP2
		}

		// store virtual hosts by domain
		vHosts := map[string]*routev3.VirtualHost{}
		// keep track of order by using a list as well as the map
		var vHostsList []*routev3.VirtualHost

		// Check if an extension is loaded that wants to modify xDS Routes after they have been generated
		for _, httpRoute := range httpListener.Routes {
			// 1:1 between IR HTTPRoute Hostname and xDS VirtualHost.
			vHost := vHosts[httpRoute.Hostname]
			if vHost == nil {
				// Remove dots from the hostname before appending it to the virtualHost name
				// since dots are special chars used in stats tag extraction in Envoy
				underscoredHostname := strings.ReplaceAll(httpRoute.Hostname, ".", "_")
				// Allocate virtual host for this httpRoute.
				vHost = &routev3.VirtualHost{
					Name:    fmt.Sprintf("%s/%s", httpListener.Name, underscoredHostname),
					Domains: []string{httpRoute.Hostname},
				}
				vHosts[httpRoute.Hostname] = vHost
				vHostsList = append(vHostsList, vHost)
			}

			// 1:1 between IR HTTPRoute and xDS config.route.v3.Route
			xdsRoute := buildXdsRoute(httpRoute, xdsListener)

			// Check if an extension want to modify the route we just generated
			// If no extension exists (or it doesn't subscribe to this hook) then this is a quick no-op.
			if err := processExtensionPostRouteHook(xdsRoute, vHost, httpRoute, t.ExtensionManager); err != nil {
				return err
			}

			vHost.Routes = append(vHost.Routes, xdsRoute)

			if httpRoute.Destination != nil {
				if err := addXdsCluster(tCtx, addXdsClusterArgs{
					name:         httpRoute.Destination.Name,
					endpoints:    httpRoute.Destination.Endpoints,
					tSocket:      nil,
					protocol:     protocol,
					endpointType: Static,
				}); err != nil && !errors.Is(err, ErrXdsClusterExists) {
					return err
				}
			}

			if httpRoute.Mirror != nil {
				if err := addXdsCluster(tCtx, addXdsClusterArgs{
					name:         httpRoute.Mirror.Name,
					endpoints:    httpRoute.Mirror.Endpoints,
					tSocket:      nil,
					protocol:     protocol,
					endpointType: Static,
				}); err != nil && !errors.Is(err, ErrXdsClusterExists) {
					return err
				}
			}
		}

		for _, vHost := range vHostsList {
			// Check if an extension want to modify the Virtual Host we just generated
			// If no extension exists (or it doesn't subscribe to this hook) then this is a quick no-op.
			if err := processExtensionPostVHostHook(vHost, t.ExtensionManager); err != nil {
				return err
			}
		}
		xdsRouteCfg.VirtualHosts = append(xdsRouteCfg.VirtualHosts, vHostsList...)

		// TODO: Make this into a generic interface for API Gateway features.
		//       https://github.com/envoyproxy/gateway/issues/882
		// Check if a ratelimit cluster exists, if not, add it, if its needed.
		if err := t.createRateLimitServiceCluster(tCtx, httpListener); err != nil {
			return err
		}

		// Create authn jwks clusters, if needed.
		if err := createJwksClusters(tCtx, httpListener.Routes); err != nil {
			return err
		}
		// Check if an extension want to modify the listener that was just configured/created
		// If no extension exists (or it doesn't subscribe to this hook) then this is a quick no-op
		if err := processExtensionPostListenerHook(tCtx, xdsListener, t.ExtensionManager); err != nil {
			return err
		}
	}

	return nil
}
```





#### 4. infraRunner

订阅 infra IR 资源， 转换为 envoy 基础设施资源，例如k8s deployment/service

#### 5. xdsServerRunner

启动一个 xds server，订阅 xds 资源然后通过 xds 协议远程配置 envoy 代理
