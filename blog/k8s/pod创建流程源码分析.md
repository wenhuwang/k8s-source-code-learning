## Pod创建流程

#### Replicaset Controller创建pod

RsController主要逻辑：

```go
// pkg/controller/replicaset/replica_set.go
func (rsc *ReplicaSetController) syncReplicaSet(key string) error {
  ...
  // manageReplicas函数主要负责新Pod创建
	var manageReplicasErr error
	if rsNeedsSync && rs.DeletionTimestamp == nil {
		manageReplicasErr = rsc.manageReplicas(filteredPods, rs)
	}
	
	// 后续代码主要用于生成rs status
	...
	return manageReplicasErr
}


// manageReplicas checks and updates replicas for the given ReplicaSet.
// Does NOT modify <filteredPods>.
// It will requeue the replica set in case of an error while creating/deleting pods.
func (rsc *ReplicaSetController) manageReplicas(filteredPods []*v1.Pod, rs *apps.ReplicaSet) error {
	diff := len(filteredPods) - int(*(rs.Spec.Replicas))
	rsKey, err := controller.KeyFunc(rs)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for %v %#v: %v", rsc.Kind, rs, err))
		return nil
	}
	if diff < 0 {
    // 创建需要新增的pod
		diff *= -1
		if diff > rsc.burstReplicas {
			diff = rsc.burstReplicas
		}
		// TODO: Track UIDs of creates just like deletes. The problem currently
		// is we'd need to wait on the result of a create to record the pod's
		// UID, which would require locking *across* the create, which will turn
		// into a performance bottleneck. We should generate a UID for the pod
		// beforehand and store it via ExpectCreations.
		rsc.expectations.ExpectCreations(rsKey, diff)
		klog.V(2).InfoS("Too few replicas", "replicaSet", klog.KObj(rs), "need", *(rs.Spec.Replicas), "creating", diff)
		// Batch the pod creates. Batch sizes start at SlowStartInitialBatchSize
		// and double with each successful iteration in a kind of "slow start".
		// This handles attempts to start large numbers of pods that would
		// likely all fail with the same error. For example a project with a
		// low quota that attempts to create a large number of pods will be
		// prevented from spamming the API service with the pod create requests
		// after one of its pods fails.  Conveniently, this also prevents the
		// event spam that those failures would generate.
		successfulCreations, err := slowStartBatch(diff, controller.SlowStartInitialBatchSize, func() error {
			// 调用podControl接口的方法创建pod
			err := rsc.podControl.CreatePodsWithControllerRef(rs.Namespace, &rs.Spec.Template, rs, metav1.NewControllerRef(rs, rsc.GroupVersionKind))
			if err != nil {
				if errors.HasStatusCause(err, v1.NamespaceTerminatingCause) {
					// if the namespace is being terminated, we don't have to do
					// anything because any creation will fail
					return nil
				}
			}
			return err
		})

		...
		return err
	} else if diff > 0 {
		// 清理需要删除的Pod
    ...
	}

	return nil
}

// pkg/controller/controller_util.go 
func (r RealPodControl) CreatePodsWithControllerRef(namespace string, template *v1.PodTemplateSpec, controllerObject runtime.Object, controllerRef *metav1.OwnerReference) error {
	...
  // 创建pod
	return r.createPods("", namespace, template, controllerObject, controllerRef)
}

func (r RealPodControl) createPods(nodeName, namespace string, template *v1.PodTemplateSpec, object runtime.Object, controllerRef *metav1.OwnerReference) error {
  // 生成Pod数据结构
	pod, err := GetPodFromTemplate(template, object, controllerRef)
	if err != nil {
		return err
	}
	if len(nodeName) != 0 {
		pod.Spec.NodeName = nodeName
	}
	if len(labels.Set(pod.Labels)) == 0 {
		return fmt.Errorf("unable to create pods, no labels")
	}
  // 调用apiserver创建Pod
	newPod, err := r.KubeClient.CoreV1().Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		// only send an event if the namespace isn't terminating
		if !apierrors.HasStatusCause(err, v1.NamespaceTerminatingCause) {
			r.Recorder.Eventf(object, v1.EventTypeWarning, FailedCreatePodReason, "Error creating: %v", err)
		}
		return err
	}
	...
	return nil
}
```

RealPodControl 对象的 createPods 负责根据 PodTemplateSpec 及其他参数生成 Pod 对象并提交到 apiserver，生成 Pod 对象逻辑为GetPodFromTemplate 函数。

```go
// pkg/controller/controller_util.go 
func GetPodFromTemplate(template *v1.PodTemplateSpec, parentObject runtime.Object, controllerRef *metav1.OwnerReference) (*v1.Pod, error) {
	desiredLabels := getPodsLabelSet(template)
	desiredFinalizers := getPodsFinalizers(template)
	desiredAnnotations := getPodsAnnotationSet(template)
	accessor, err := meta.Accessor(parentObject)
	if err != nil {
		return nil, fmt.Errorf("parentObject does not have ObjectMeta, %v", err)
	}
  
  // 这里并没生成pod name，只是通过rs name生成了pod GenerateName，所以可以判断最终的pod name是有apiserver根据GenerateName生成的
	prefix := getPodsPrefix(accessor.GetName())
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels:       desiredLabels,
			Annotations:  desiredAnnotations,
			GenerateName: prefix,
			Finalizers:   desiredFinalizers,
		},
	}
	if controllerRef != nil {
		pod.OwnerReferences = append(pod.OwnerReferences, *controllerRef)
	}
	pod.Spec = *template.Spec.DeepCopy()
	return pod, nil
}

// 这里pod GenerateName生成逻辑是由rs name和 - 拼接而成
func getPodsPrefix(controllerName string) string {
	// use the dash (if the name isn't too long) to make the pod name a bit prettier
	prefix := fmt.Sprintf("%s-", controllerName)
	if len(validation.ValidatePodName(prefix, true)) != 0 {
		prefix = controllerName
	}
	return prefix
}
```

Apiserver 创建资源时根据 GenerateName 生成资源名的逻辑：

```go
// staging/src/k8s.io/apiserver/pkg/registry/rest/create.go

// BeforeCreate主要用于在创建资源前做一些预处理逻辑
// BeforeCreate ensures that common operations for all resources are performed on creation. It only returns
// errors that can be converted to api.Status. It invokes PrepareForCreate, then GenerateName, then Validate.
// It returns nil if the object should be created.
func BeforeCreate(strategy RESTCreateStrategy, ctx context.Context, obj runtime.Object) error {
	objectMeta, kind, kerr := objectMetaAndKind(strategy, obj)
	if kerr != nil {
		return kerr
	}

	if strategy.NamespaceScoped() {
		if !ValidNamespace(ctx, objectMeta) {
			return errors.NewBadRequest("the namespace of the provided object does not match the namespace sent on the request")
		}
	} else if len(objectMeta.GetNamespace()) > 0 {
		objectMeta.SetNamespace(metav1.NamespaceNone)
	}
	objectMeta.SetDeletionTimestamp(nil)
	objectMeta.SetDeletionGracePeriodSeconds(nil)
	strategy.PrepareForCreate(ctx, obj)
	FillObjectMetaSystemFields(objectMeta)
  // 根据GenerateName生成name
	if len(objectMeta.GetGenerateName()) > 0 && len(objectMeta.GetName()) == 0 {
		objectMeta.SetName(strategy.GenerateName(objectMeta.GetGenerateName()))
	}

	// Ensure managedFields is not set unless the feature is enabled
	if !utilfeature.DefaultFeatureGate.Enabled(features.ServerSideApply) {
		objectMeta.SetManagedFields(nil)
	}

	// ClusterName is ignored and should not be saved
	if len(objectMeta.GetClusterName()) > 0 {
		objectMeta.SetClusterName("")
	}

	if errs := strategy.Validate(ctx, obj); len(errs) > 0 {
		return errors.NewInvalid(kind.GroupKind(), objectMeta.GetName(), errs)
	}

	// Custom validation (including name validation) passed
	// Now run common validation on object meta
	// Do this *after* custom validation so that specific error messages are shown whenever possible
	if errs := genericvalidation.ValidateObjectMetaAccessor(objectMeta, strategy.NamespaceScoped(), path.ValidatePathSegmentName, field.NewPath("metadata")); len(errs) > 0 {
		return errors.NewInvalid(kind.GroupKind(), objectMeta.GetName(), errs)
	}

	strategy.Canonicalize(obj)

	return nil
}

// staging/src/k8s.io/apiserver/pkg/storage/names/generate.go

const (
	// TODO: make this flexible for non-core resources with alternate naming rules.
	maxNameLength          = 63
	randomLength           = 5
	maxGeneratedNameLength = maxNameLength - randomLength
)

// 生成name时如果base字段长度超过58，则只保留前58位。deployment创建rs时会在name后面拼接中划线和10位hash值, base值=中划线 + 10位hash值 + 中划线， 所以deployment name长度不能超过46
func (simpleNameGenerator) GenerateName(base string) string {
	if len(base) > maxGeneratedNameLength {
		base = base[:maxGeneratedNameLength]
	}
	return fmt.Sprintf("%s%s", base, utilrand.String(randomLength))
}
```

参考：
https://blog.csdn.net/qq_21816375/article/details/85810825
