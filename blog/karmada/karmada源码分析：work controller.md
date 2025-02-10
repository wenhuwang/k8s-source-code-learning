### controller注册逻辑
```go
// SetupWithManager creates a controller and register to controller manager.
func (c *Controller) SetupWithManager(mgr controllerruntime.Manager) error {
	return controllerruntime.NewControllerManagedBy(mgr).
		For(&workv1alpha1.Work{}, builder.WithPredicates(c.PredicateFunc)).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controller.Options{
			RateLimiter: ratelimiterflag.DefaultControllerRateLimiter(c.RatelimiterOptions),
		}).
		Complete(c)
}

```

### reconciler函数逻辑

```go
// Reconcile performs a full reconciliation for the object referred to by the Request.
// The Controller will requeue the Request to be processed again if an error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (c *Controller) Reconcile(ctx context.Context, req controllerruntime.Request) (controllerruntime.Result, error) {
	klog.V(4).Infof("Reconciling Work %s", req.NamespacedName.String())

	work := &workv1alpha1.Work{}
	if err := c.Client.Get(ctx, req.NamespacedName, work); err != nil {
		// The resource may no longer exist, in which case we stop processing.
		if apierrors.IsNotFound(err) {
			return controllerruntime.Result{}, nil
		}

		return controllerruntime.Result{Requeue: true}, err
	}

	clusterName, err := names.GetClusterName(work.Namespace)
	if err != nil {
		klog.Errorf("Failed to get member cluster name for work %s/%s", work.Namespace, work.Name)
		return controllerruntime.Result{Requeue: true}, err
	}

	cluster, err := util.GetCluster(c.Client, clusterName)
	if err != nil {
		klog.Errorf("Failed to get the given member cluster %s", clusterName)
		return controllerruntime.Result{Requeue: true}, err
	}

	if !work.DeletionTimestamp.IsZero() {
		// Abort deleting workload if cluster is unready when unjoining cluster, otherwise the unjoin process will be failed.
		if util.IsClusterReady(&cluster.Status) {
			err := c.tryDeleteWorkload(clusterName, work)
			if err != nil {
				klog.Errorf("Failed to delete work %v, namespace is %v, err is %v", work.Name, work.Namespace, err)
				return controllerruntime.Result{Requeue: true}, err
			}
		} else if cluster.DeletionTimestamp.IsZero() { // cluster is unready, but not terminating
			return controllerruntime.Result{Requeue: true}, fmt.Errorf("cluster(%s) not ready", cluster.Name)
		}

		return c.removeFinalizer(work)
	}

	if !util.IsClusterReady(&cluster.Status) {
		klog.Errorf("Stop sync work(%s/%s) for cluster(%s) as cluster not ready.", work.Namespace, work.Name, cluster.Name)
		return controllerruntime.Result{Requeue: true}, fmt.Errorf("cluster(%s) not ready", cluster.Name)
	}

	return c.syncWork(clusterName, work)
}


func (c *Controller) syncWork(clusterName string, work *workv1alpha1.Work) (controllerruntime.Result, error) {
	start := time.Now()
	err := c.syncToClusters(clusterName, work)
	metrics.ObserveSyncWorkloadLatency(err, start)
	if err != nil {
		msg := fmt.Sprintf("Failed to sync work(%s) to cluster(%s): %v", work.Name, clusterName, err)
		klog.Errorf(msg)
		c.EventRecorder.Event(work, corev1.EventTypeWarning, events.EventReasonSyncWorkloadFailed, msg)
		return controllerruntime.Result{Requeue: true}, err
	}
	msg := fmt.Sprintf("Sync work (%s) to cluster(%s) successful.", work.Name, clusterName)
	klog.V(4).Infof(msg)
	c.EventRecorder.Event(work, corev1.EventTypeNormal, events.EventReasonSyncWorkloadSucceed, msg)
	return controllerruntime.Result{}, nil
}

// syncToClusters ensures that the state of the given object is synchronized to member clusters.
func (c *Controller) syncToClusters(clusterName string, work *workv1alpha1.Work) error {
	var errs []error
	syncSucceedNum := 0
	for _, manifest := range work.Spec.Workload.Manifests {
		workload := &unstructured.Unstructured{}
		err := workload.UnmarshalJSON(manifest.Raw)
		util.MergeLabel(workload, workv1alpha2.WorkUIDLabel, string(work.UID))
		if err != nil {
			klog.Errorf("Failed to unmarshal workload, error is: %v", err)
			errs = append(errs, err)
			continue
		}

		if err = c.tryCreateOrUpdateWorkload(clusterName, workload); err != nil {
			klog.Errorf("Failed to create or update resource(%v/%v) in the given member cluster %s, err is %v", workload.GetNamespace(), workload.GetName(), clusterName, err)
			c.eventf(workload, corev1.EventTypeWarning, events.EventReasonSyncWorkloadFailed, "Failed to create or update resource(%s) in member cluster(%s): %v", klog.KObj(workload), clusterName, err)
			errs = append(errs, err)
			continue
		}
		c.eventf(workload, corev1.EventTypeNormal, events.EventReasonSyncWorkloadSucceed, "Successfully applied resource(%v/%v) to cluster %s", workload.GetNamespace(), workload.GetName(), clusterName)
		syncSucceedNum++
	}

	if len(errs) > 0 {
		total := len(work.Spec.Workload.Manifests)
		message := fmt.Sprintf("Failed to apply all manifests (%d/%d): %s", syncSucceedNum, total, errors.NewAggregate(errs).Error())
		err := c.updateAppliedCondition(work, metav1.ConditionFalse, "AppliedFailed", message)
		if err != nil {
			klog.Errorf("Failed to update applied status for given work %v, namespace is %v, err is %v", work.Name, work.Namespace, err)
			errs = append(errs, err)
		}
		return errors.NewAggregate(errs)
	}

	err := c.updateAppliedCondition(work, metav1.ConditionTrue, "AppliedSuccessful", "Manifest has been successfully applied")
	if err != nil {
		klog.Errorf("Failed to update applied status for given work %v, namespace is %v, err is %v", work.Name, work.Namespace, err)
		return err
	}

	return nil
}


func (c *Controller) tryCreateOrUpdateWorkload(clusterName string, workload *unstructured.Unstructured) error {
	fedKey, err := keys.FederatedKeyFunc(clusterName, workload)
	if err != nil {
		klog.Errorf("Failed to get FederatedKey %s, error: %v", workload.GetName(), err)
		return err
	}

	clusterObj, err := helper.GetObjectFromCache(c.RESTMapper, c.InformerManager, fedKey)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			klog.Errorf("Failed to get resource %v from member cluster, err is %v ", workload.GetName(), err)
			return err
		}
		err = c.ObjectWatcher.Create(clusterName, workload)
		if err != nil {
			klog.Errorf("Failed to create resource(%v/%v) in the given member cluster %s, err is %v", workload.GetNamespace(), workload.GetName(), clusterName, err)
			return err
		}
		return nil
	}

	err = c.ObjectWatcher.Update(clusterName, workload, clusterObj)
	if err != nil {
		klog.Errorf("Failed to update resource in the given member cluster %s, err is %v", clusterName, err)
		return err
	}
	return nil
}




func (o *objectWatcherImpl) Update(clusterName string, desireObj, clusterObj *unstructured.Unstructured) error {
	updateAllowed := o.allowUpdate(clusterName, desireObj, clusterObj)
	if !updateAllowed {
		// The existing resource is not managed by Karmada, and no conflict resolution found, avoid updating the existing resource by default.
		return fmt.Errorf("resource(kind=%s, %s/%s) already exist in cluster %v and the %s strategy value is empty, karmada will not manage this resource",
			desireObj.GetKind(), desireObj.GetNamespace(), desireObj.GetName(), clusterName, workv1alpha2.ResourceConflictResolutionAnnotation,
		)
	}

	dynamicClusterClient, err := o.ClusterClientSetFunc(clusterName, o.KubeClientSet)
	if err != nil {
		klog.Errorf("Failed to build dynamic cluster client for cluster %s.", clusterName)
		return err
	}

	gvr, err := restmapper.GetGroupVersionResource(o.RESTMapper, desireObj.GroupVersionKind())
	if err != nil {
		klog.Errorf("Failed to update resource(kind=%s, %s/%s) in cluster %s as mapping GVK to GVR failed: %v", desireObj.GetKind(), desireObj.GetNamespace(), desireObj.GetName(), clusterName, err)
		return err
	}

	desireObj, err = o.retainClusterFields(desireObj, clusterObj)
	if err != nil {
		klog.Errorf("Failed to retain fields for resource(kind=%s, %s/%s) in cluster %s: %v", clusterObj.GetKind(), clusterObj.GetNamespace(), clusterObj.GetName(), clusterName, err)
		return err
	}

	resource, err := dynamicClusterClient.DynamicClientSet.Resource(gvr).Namespace(desireObj.GetNamespace()).Update(context.TODO(), desireObj, metav1.UpdateOptions{})
	if err != nil {
		klog.Errorf("Failed to update resource(kind=%s, %s/%s) in cluster %s, err is %v ", desireObj.GetKind(), desireObj.GetNamespace(), desireObj.GetName(), clusterName, err)
		return err
	}

	klog.Infof("Updated resource(kind=%s, %s/%s) on cluster: %s", desireObj.GetKind(), desireObj.GetNamespace(), desireObj.GetName(), clusterName)

	// record version
	o.recordVersion(resource, clusterName)
	return nil
}



// 处理member集群保留字段
func (o *objectWatcherImpl) retainClusterFields(desired, observed *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	// Pass the same ResourceVersion as in the cluster object for update operation, otherwise operation will fail.
	desired.SetResourceVersion(observed.GetResourceVersion())

	// Retain finalizers since they will typically be set by
	// controllers in a member cluster.  It is still possible to set the fields
	// via overrides.
	desired.SetFinalizers(observed.GetFinalizers())

	// Retain ownerReferences since they will typically be set by controllers in a member cluster.
	desired.SetOwnerReferences(observed.GetOwnerReferences())

	// Retain annotations since they will typically be set by controllers in a member cluster
	// and be set by user in karmada-controller-plane.
	util.RetainAnnotations(desired, observed)

	// Retain labels since they will typically be set by controllers in a member cluster
	// and be set by user in karmada-controller-plane.
	util.RetainLabels(desired, observed)

	if o.resourceInterpreter.HookEnabled(desired.GroupVersionKind(), configv1alpha1.InterpreterOperationRetain) {
		return o.resourceInterpreter.Retain(desired, observed)
	}

	return desired, nil
}



// 合并desired对象和observed对象注解
// RetainAnnotations merges the annotations that added by controllers running
// in member cluster to avoid overwriting.
// Following keys will be ignored if :
//   - the keys were previous propagated to member clusters(that are tracked
//     by "resourcetemplate.karmada.io/managed-annotations" annotation in observed)
//     but have been removed from Karmada control plane(don't exist in desired anymore).
//   - the keys that exist in both desired and observed even those been accidentally modified
//     in member clusters.
func RetainAnnotations(desired *unstructured.Unstructured, observed *unstructured.Unstructured) {
	objectAnnotation := desired.GetAnnotations()
	if objectAnnotation == nil {
		objectAnnotation = make(map[string]string, 0)
	}
  // 遍历observed注解，如果deletedAnnotationKeys和objectAnnotation中该key都不存在，则将该注解加入最终desired对象注解中
	deletedAnnotationKeys := getDeletedAnnotationKeys(desired, observed)
	for key, value := range observed.GetAnnotations() {
		if deletedAnnotationKeys.Has(key) {
			continue
		}
		if _, exist := objectAnnotation[key]; exist {
			continue
		}
		objectAnnotation[key] = value
	}
	if len(objectAnnotation) > 0 {
		desired.SetAnnotations(objectAnnotation)
	}
}


// 从member集群对应资源注解ManagedAnnotation中找到所有被karmada管理的注解的key，利用字符串分隔符放入recordKeys集合；遍历desired对象注解并将注解key从recordKeys集合中删除
const (
  ManagedAnnotation = "resourcetemplate.karmada.io/managed-annotations"
)
func getDeletedAnnotationKeys(desired, observed *unstructured.Unstructured) sets.Set[string] {
	recordKeys := sets.New[string](strings.Split(observed.GetAnnotations()[workv1alpha2.ManagedAnnotation], ",")...)
	for key := range desired.GetAnnotations() {
		recordKeys.Delete(key)
	}
	return recordKeys
}
```

