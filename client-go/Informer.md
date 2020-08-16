## Informer介绍

Kubernetes系统组件之间通过HTTP协议通信，在不依赖任何中间件的情况下如何保证消息的实时性、可靠性、顺序性呢？答案就是Informer机制，Kubernetes的其他组件都是通过client-go的Informer机制与Kubernetes API Server进行通信的。

### 架构设计

Informer运行原理图如下所示：

![](client-go/Informer.png)

在架构设计中核心组件分别如下：

#### 1. Reflector

Reflector用于监控（Watch）指定的 Kubernetes 资源，当监控资源发生变化时，触发相应的变更事件，例如 Added 事件、

Updated 事件、Deleted 事件，并将其资源对象存放到本地缓存 DeltaFIFO 中。

***问题： Reflector和DeltaFIFO 之间是什么关系？他们之间是否存在包含？***

#### 2. DeltaFIFO

DeltaFIFO 可以分开理解，FIFO 是一个先进先出队列，拥有 Add、Update、Delete、List、Pop、Close 等方法，而 Delta 是一个资源对象存储，它可以保存资源对象的操作类型，例如 Added 、Updated、Deleted、Sync 等操作类型。

#### 3. Indexer

Indexer 是 client-go 用来存储资源对象并自带索引功能的本地存储，Reflector 从 DeltaFIFO 中将消费出来的资源对象存储至 Indexer。Indexer 与 Etcd 集群中的数据完全保持一致。client-go 可以方便的从本地存储中读取资源数据，而无需每次远程通过 Kubernetes API Server从Etcd集群中读取。

### 源码分析

Informer Example代码示例：

```go
package main

import (
	"fmt"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"log"
	"time"
)

func main(){
	config, err := clientcmd.BuildConfigFromFlags("", "/home/mars/.kube/config")
	if err != nil {
		panic(err)
	}

	// 创建clientset对象，用于informer和api server交互
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}
	
	// informer是一个持久运行的goroutine，创建stopCh对象用于程序退出之前通知informer提前退出
	stopCh := make(chan struct{})
	defer close(stopCh)

	// NewSharedInformerFactory函数第一个参数用于与api server交互，第二个参数defaultResync用于设置多久进行一次resync，resync会周期性的执行List操作，将所有资源存放在Store中，该参数为0意味着禁用resync功能。
	shareInformers := informers.NewSharedInformerFactory(clientset, 2 * time.Minute)
	informer := shareInformers.Core().V1().Pods().Informer()
	
	// informer.AddEventHandler函数可以为Pod资源添加资源事件回调，支持以下三种事件方法回调
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}){
			mObj := obj.(v1.Object)
			log.Printf("New pod Added to Store: %s", mObj.GetName())
		},
		UpdateFunc: func(oldObj, newObj interface{}){
			oObj := oldObj.(v1.Object)
			nObj := newObj.(v1.Object)
			log.Printf("%s Pod UPdated to %s", oObj.GetName(), nObj.GetName())
		},
		DeleteFunc: func(obj interface{}){
			mObj := obj.(*corev1.Pod)
			log.Printf("Pod Deleted from Store: %s", mObj.GetName())
			fmt.Println(mObj.GroupVersionKind())
		},
	})

    // 启动informer
	informer.Run(stopCh)
}

```

运行输出：

```
mars@mars:~/go/src/k8sCodingDemo$ go  run informer-demo.go 
# 全量同步一次Pods数据
2020/08/15 17:08:53 New pod Added to Store: porter-agent-7cjnk
2020/08/15 17:08:53 New pod Added to Store: redis-service-0
...
# 根据NewSharedInformerFactory(client, defaultResync)函数第二个参数defaultResync设置的时间进行定时全量更新Pod数据
2020/08/15 17:10:53 porter-agent-7cjnk Pod UPdated to porter-agent-7cjnk
2020/08/15 17:10:53 redis-service-0 Pod UPdated to redis-service-0
...
```

#### Informer 启动流程分析

从示例代码 informer.run(stopCh) 作为入口点分析 Infomer 启动流程，informer.Run() 函数实现如下所示：

```
// 路径：vendor/k8s.io/client-go/informers/tools/cache/shared_informer.go
func (s *sharedIndexInformer) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	// 实例化DeltaFIFO对象，作为controller字段Config属性的Queue属性的实现
	fifo := NewDeltaFIFO(MetaNamespaceKeyFunc, s.indexer)

	cfg := &Config{
		Queue:            fifo,
		ListerWatcher:    s.listerWatcher,
		ObjectType:       s.objectType,
		FullResyncPeriod: s.resyncCheckPeriod,
		RetryOnError:     false,
		ShouldResync:     s.processor.shouldResync,

		// 消费DeltaFIFO时的回调函数
		Process: s.HandleDeltas,
	}

	func() {
		s.startedLock.Lock()
		defer s.startedLock.Unlock()
		// 使用cfg实例化一个controller
		s.controller = New(cfg)
		s.controller.(*controller).clock = s.clock
		s.started = true
	}()

	// Separate stop channel because Processor should be stopped strictly after controller
	processorStopCh := make(chan struct{})
	var wg wait.Group
	defer wg.Wait()              // Wait for Processor to stop
	defer close(processorStopCh) // Tell Processor to stop
	wg.StartWithChannel(processorStopCh, s.cacheMutationDetector.Run)
	wg.StartWithChannel(processorStopCh, s.processor.run)

	defer func() {
		s.startedLock.Lock()
		defer s.startedLock.Unlock()
		s.stopped = true // Don't want any new listeners
	}()
	// 启动controller
	s.controller.Run(stopCh)
}
```

我们看看 controller.Run() 函数的实现：

```
// 路径：vendor/k8s.io/client-go/informers/tools/cache/controller.go
func (c *controller) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	go func() {
		<-stopCh
		c.config.Queue.Close()
	}()
	
	// 实例化Reflector对象，c.config.Queue其实就是DeltaFIFO
	r := NewReflector(
		c.config.ListerWatcher,
		c.config.ObjectType,
		c.config.Queue,
		c.config.FullResyncPeriod,
	)
	r.ShouldResync = c.config.ShouldResync
	r.clock = c.clock

	c.reflectorMutex.Lock()
	c.reflector = r
	c.reflectorMutex.Unlock()

	var wg wait.Group
	defer wg.Wait()

	// 这里其实就是启动一个goroutine执行r.Run函数，r.Run函数会接收stopch作为结束信号
	wg.StartWithChannel(stopCh, r.Run)

	// 周期性调用c.processLoop消费DeltaFIFO中的消息
	wait.Until(c.processLoop, time.Second, stopCh)
}
```

这里分两条线：r.Run() 和 c.processLoop()

##### r.Run() 函数实现

我们先看一下 r.Run() 函数的实现：

```
# 路径：vendor/k8s.io/client-go/tools/cache/reflector.go
func (r *Reflector) Run(stopCh <-chan struct{}) {
	klog.V(3).Infof("Starting reflector %v (%s) from %s", r.expectedType, r.resyncPeriod, r.name)
	// 重复执行r.ListAndWatch(),间隔时间为r.period，直到收到stopch信号
	wait.Until(func() {
		if err := r.ListAndWatch(stopCh); err != nil {
			utilruntime.HandleError(err)
		}
	}, r.period, stopCh)
}
```

ListAndWatch  函数分为两部分：第一部分获取资源列表数据，第二部分监控资源对象。

###### 1. 获取资源列表数据

ListAndWatch List 部分代码示例如下：

```go
# 路径： vendor/k8s.io/client-go/tools/cache/reflector.go
func (r *Reflector) ListAndWatch(stopCh <-chan struct{}) error {
	klog.V(3).Infof("Listing and watching %v from %s", r.expectedType, r.name)
	var resourceVersion string
	options := metav1.ListOptions{ResourceVersion: "0"}
	if err := func() error {
		...
		go func() {
			defer func() {
			...
			// 获取资源下所有对象数据，例如获取所有Pod的资源数据是由options的ResourceVersion参数控制，如果ResourceVersion为0，则表示获取所有Pod资源数据；如果ResourceVersion为非0，则表示根据ResourceVersion继续获取，功能类似于文件传输过程中的断点传输，当传输过程遇到网络故障导致中断下次再连接时，会根据ResourceVersion继续传输未完成的部分。可以使本地缓存与Etcd集群中数据保持一致。
			pager := pager.New(pager.SimplePageFunc(func(opts metav1.ListOptions) (runtime.Object, error) {
				return r.listerWatcher.List(opts)
			}))
			...
			list, err = pager.List(context.Background(), options)
			close(listCh)
		}()
		...
		listMetaInterface, err := meta.ListAccessor(list)
		...
		// 获取资源版本号，Kubernetes所有资源都拥有该字段，它标识当前资源的版本号。每次修改资源时，api server都会更改当前ResourceVersion，使得client-go执行Watch操作时可以根据ResourceVersion来确定当前资源对象是否发生改变。
		resourceVersion = listMetaInterface.GetResourceVersion()
		initTrace.Step("Resource version extracted")
		// 用于将资源对象数据runtime.Object转换为资源对象列表[]runtime.Object，因为r.listerWatcher.List获取的是资源的所有对象数据。
		items, err := meta.ExtractList(list)
		...
		// 用于将将资源对象列表中的资源对象和资源版本号存储至DeltaFIFO中，并替换已存在的对象。
		if err := r.syncWith(items, resourceVersion); err != nil {
			return fmt.Errorf("%s: Unable to sync list result: %v", r.name, err)
		}
		initTrace.Step("SyncWith done")
		//用于设置最新的资源版本号
		r.setLastSyncResourceVersion(resourceVersion)
		...
	}(); err != nil {
		return err
	}
	...
}
```

###### 2. 监控资源对象

Watch 操作通过 HTTP 协议与 Kubernetes API Server 建立长连接，接收 API Server 发来的资源变更事件， Watch 操作的实现机制是使用 HTTP 协议的分块传输编码（Chunked Transfer Encoding）。当 client-go 调用 Kubernetes API Server时，API Server 在 Response 的 HTTP Header 中设置 Transfer-Encoding 的值为 chunked，表示分块传输编码，客户端收到该消息后，便于服务端进行连接并等待下一个数据块。

ListAndWatch Watch 代码示例如下：

```
# 路径： vendor/k8s.io/client-go/tools/cache/reflector.go
func (r *Reflector) ListAndWatch(stopCh <-chan struct{}) error {
    for {
		...
		options = metav1.ListOptions{
			ResourceVersion: resourceVersion,
			TimeoutSeconds: &timeoutSeconds,
			AllowWatchBookmarks: false,
		}
		// 监控指定资源的变更事件
		w, err := r.listerWatcher.Watch(options)
		...
		// 用于处理资源的变更事件，当初发Added、Updated、Deleted事件时，将对应的资源对象更新到DeltaFIFO中并更新ResouceVersion
		if err := r.watchHandler(w, &resourceVersion, resyncerrc, stopCh); err != nil {
			if err != errorStopRequested {
				klog.Warningf("%s: watch of %v ended with: %v", r.name, r.expectedType, err)
			}
			return nil
		}
	}
}

// watchHandler函数实现如下：
// 主要功能是当Watch资源数据事件更新时，根据事件类型调用不同的方法把消息加入到DeltaFIFO中
func (r *Reflector) watchHandler(w watch.Interface, resourceVersion *string, errc chan error, stopCh <-chan struct{}) error {
	...

loop:
	for {
		select {
		...
		case event, ok := <-w.ResultChan():
			...
			newResourceVersion := meta.GetResourceVersion()
			switch event.Type {
			case watch.Added:
				err := r.store.Add(event.Object)
				...
			case watch.Modified:
				err := r.store.Update(event.Object)
				...
			case watch.Deleted:
				err := r.store.Delete(event.Object)
				...
			case watch.Bookmark:
			default:
				utilruntime.HandleError(fmt.Errorf("%s: unable to understand watch event %#v", r.name, event))
			}
			*resourceVersion = newResourceVersion
			r.setLastSyncResourceVersion(newResourceVersion)
			eventCount++
		}
	}
```

r.listerWatcher.List 和 r.listerWatcher.Watch 函数实际上调用了 Pod Informer 下的 NewFilteredPodInformer 函数返回值里的 ListFunc 和 WatchFunc 函数，它通过 Clientset 客户端与 Kubernetes API Server 交互并获取 Pod 资源列表数据和监控资源变更事件，这里先不细讲。

DeltaFIFO 队列中的资源对象在 Added、Updated、Deleted事件都调用了 queueActionLocked 函数，它是 DeltaFIFO 实现的关键，queueActionLocked 函数代码如下：

```
# 路径： vendor/k8s.io/client-go/tools/cache/delta_fifo.go
func (f *DeltaFIFO) queueActionLocked(actionType DeltaType, obj interface{}) error {
	// 根据不同资源对象计算出唯一的key
	id, err := f.KeyOf(obj)
	if err != nil {
		return KeyError{obj, err}
	}

	// 将actionType和资源对象构造成Delta,添加到items[id]中，并通过dedupDeltas函数进行去重操作
	newDeltas := append(f.items[id], Delta{actionType, obj})
	newDeltas = dedupDeltas(newDeltas)

	if len(newDeltas) > 0 {
	    // 把资源对象对应的key加入到queue中，并把新的Deltas加入到items中，并通过conf.Broadcast()通知所有消费者解除阻塞
		if _, exists := f.items[id]; !exists {
			f.queue = append(f.queue, id)
		}
		f.items[id] = newDeltas
		f.cond.Broadcast()
	} else {
		// We need to remove this from our map (extra items in the queue are
		// ignored if they are not in the map).
		delete(f.items, id)
	}
	return nil
}
```



##### c.processLoop() 函数实现

processLoop() 函数主要是循环调用了 DeltaFIFO 队列的 Pop() 函数来消费消息，当消费异常时，安全起见会重新把消息加入到 DeltaFIFO 中。

```
// 路径：vendor/k8s.io/client-go/informers/tools/cache/controller.go
func (c *controller) processLoop() {
	for {
		obj, err := c.config.Queue.Pop(PopProcessFunc(c.config.Process))
		if err != nil {
			if err == FIFOClosedError {
				return
			}
			if c.config.RetryOnError {
				// This is the safe way to re-enqueue.
				c.config.Queue.AddIfNotPresent(obj)
			}
		}
	}
}
```

Pop 方法作为消费者方法，作用是从 DeltaFIFO 头部去除最早进入队列的资源对象，Pop 方法必须传入 process 函数用于接收并处理对象的回调方法，代码实现如下：

```
# 路径： vendor/k8s.io/client-go/tools/cache/delta_fifo.go
func (f *DeltaFIFO) Pop(process PopProcessFunc) (interface{}, error) {
	...
	for {
		// 当队列没有数据时，通过f.cond.Wait()函数阻塞等待数据，只有收到f.cond.Broadcast时才说明有数据被添加，解除当前阻塞状态。
		for len(f.queue) == 0 {
			...
			f.cond.Wait()
		}
		// 当队列不为空时，取出队列头部数据，并将该对象传入process回调函数，如果回调函数出错，会把该对象重新存入队列。
		id := f.queue[0]
		f.queue = f.queue[1:]
		if f.initialPopulationCount > 0 {
			f.initialPopulationCount--
		}
		item, ok := f.items[id]
		if !ok {
			// Item may have been deleted subsequently.
			continue
		}
		delete(f.items, id)
		err := process(item)
		if e, ok := err.(ErrRequeue); ok {
			f.addIfNotPresent(id, item)
			err = e.Err
		}
		// Don't need to copyDeltas here, because we're transferring
		// ownership to the caller.
		return item, err
	}
}
```

回调函数在 Controller 初始化时传入，回调函数代码实现如下：

```
// 路径：vendor/k8s.io/client-go/informers/tools/cache/shared_informer.go
func (s *sharedIndexInformer) HandleDeltas(obj interface{}) error {
	s.blockDeltas.Lock()
	defer s.blockDeltas.Unlock()

	// from oldest to newest
	for _, d := range obj.(Deltas) {
		switch d.Type {
		// 当操作类型为Sync, Added, Updated时，将资源对象存储至 Indexer,并通过distribute函数将资源分发至SharedInformer。
		case Sync, Added, Updated:
			isSync := d.Type == Sync
			s.cacheMutationDetector.AddObject(d.Object)
			if old, exists, err := s.indexer.Get(d.Object); err == nil && exists {
				if err := s.indexer.Update(d.Object); err != nil {
					return err
				}
				s.processor.distribute(updateNotification{oldObj: old, newObj: d.Object}, isSync)
			} else {
				if err := s.indexer.Add(d.Object); err != nil {
					return err
				}
				s.processor.distribute(addNotification{newObj: d.Object}, isSync)
			}
		case Deleted:
			if err := s.indexer.Delete(d.Object); err != nil {
				return err
			}
			s.processor.distribute(deleteNotification{oldObj: d.Object}, false)
		}
	}
	return nil
}
```





