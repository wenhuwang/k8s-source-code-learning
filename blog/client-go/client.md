##### 1. client客户端介绍

 - `RESTClient` 客户端

   RESTClient是最基础的客户端。RESTClient对HTTP Request进行了封装，实现了RESTful风格的API。ClientSet、DynamicClient及DiscoveryClient客户端都是基于RESTClient实现的。它具有很强的灵活性，数据不依赖于方法和资源，因此RESTClient能够处理多种类型的调用，返回不同的数据格式。

 - `ClientSet` 客户端

   RESTClient是一种最基础的客户端，使用时需要提前指定Resource和Version等信息。相比RESTClient，ClientSet使用起来更加便捷，通常情况下，开发者对Kubernetes进行二次开发会倾向于使用ClientSet。

   ClientSet在RESTClient基础上封装了对Resource和Version的管理方法。每一个Resource可以理解为一个客户端，而ClientSet则是多个客户端的集合，每一个Resource和Version都以函数方式暴漏给开发者，例如，ClientSet提供的RbacV1、CoreV1、NetworkingV1等接口函数。

   > 注意：ClientSet仅能访问Kubernetes自身内置资源，不能直接访问CRD资源。如果需要ClientSet访问CRD资源可以通过client-gen代码生成器重新生成ClientSet。

   以下代码通过引入sampleController包(已经封装了CRD对应的ClientSet)，测试了通过ClientSet访问自身内置资源和CRD资源：

   ```
   package main
   
   import (
   	"fmt"
   	apiv1 "k8s.io/api/core/v1"
   	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
   	"k8s.io/client-go/kubernetes"
   	"k8s.io/client-go/tools/clientcmd"
   	clientset "k8s.io/sample-controller/pkg/generated/clientset/versioned"
   )
   
   func main(){
   	config, err := clientcmd.BuildConfigFromFlags("", "/home/mars/.kube/config")
   	if err != nil {
   		panic(err)
   	}
   
   	// Get Pods resource
   	clientSet, err := kubernetes.NewForConfig(config)
   	if err != nil {
   		panic(err)
   	}
   	podClient := clientSet.CoreV1().Pods(apiv1.NamespaceDefault)
   	list, err := podClient.List(metav1.ListOptions{Limit: 500})
   	if err != nil {
   		panic(err)
   	}
   	for _, d := range list.Items {
   		fmt.Printf("NAMESPACE: %v \t NAME: %v \t STATUS: %+v\n", d.Namespace, d.Name, d.Status.Phase)
   	}
   
   	// Get Foo resource
   	exampleClientSet, err := clientset.NewForConfig(config)
   	if err != nil {
   		panic(err)
   	}
   	fooList, err := exampleClientSet.SamplecontrollerV1alpha1().Foos(apiv1.NamespaceDefault).List(metav1.ListOptions{Limit: 500})
   	if err != nil {
   		panic(err)
   	}
   	for _, d := range fooList.Items {
   		fmt.Printf("Foo resource NAMESPACE: %v \t NAME: %v \t STATUS: %+v\n", d.Namespace, d.Name, d.Status.AvailableReplicas)
   	}
   }
   ```

- `DynamicClient` 客户端

  DynamicClient是一种动态客户端，他可以对任意资源进行RESTful操作，包括CRD资源。DynamicClient与ClientSet同样封装了RESTClient，同样提供了Create、Update、Delete、Get、List、Watch、Patch等方法。

  DynamicClient与ClientSet最大的不同是，ClientSet仅能访问Kubernetes原生资源，不能直接访问CRD资源。ClientSet需要预先实现每种Resource和Version的操作，其内部数据都是结构化数据。而DynamicClient内部实现了Unstructured，用于处理非结构化数据结构(即无法提前预知数据结构)，这也是DynamicClient能够处理CRD自定义资源的关键。

- DiscoveryClient客户端

  DiscoveryClient是发现客户端，它主要用于发现Kubernetes API Server所支持的资源组、资源版本、资源信息。DiscoveryClient处理可以发现Kubernetes API Server所支持的资源组、资源版本、资源信息，还可以将这些信息存储到本地，用于缓存(Cache),以减轻对Kubernetes API Server的访问压力。缓存信息默认存储于~/.kube/cache和~/.kube/http-cache下。

##### 2.  举个例子

- 集群外部应用访问kube-apiserver，参考上面ClientSet方式

- 集群內部应用访问kube-apiserver， 代码如下所示：

  ```
  package main
  
  import (
  	"k8s.io/client-go/kubernetes"
  	"k8s.io/client-go/rest"
  )
  
  func main() {
  	// creates the in-cluster config
  	config, err := rest.InClusterConfig()
  	if err != nil {
  		panic(err.Error())
  	}
  	// creates the clientset
  	clientset, err := kubernetes.NewForConfig(config)
  	if err != nil {
  		panic(err.Error())
  	}
  	...
  }
  ```

  与一般通过绝对路径读取从`kubeconfig`获取集群信息不同的是，集群內部应用直接通过`rest.InClusterConfig()`函数获取`config`。

  `rest.InClusterConfig()`函数实现如下：

  ```
  func InClusterConfig() (*Config, error) {
  	// kubernetes默认会把Pod所在namespace下默认的secret挂载到每个Pod中，secret中包含token和ca证书
  	const (
  		tokenFile  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
  		rootCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
  	)
  	// 以下环境变量会自动注入到Pod
  	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
  	if len(host) == 0 || len(port) == 0 {
  		return nil, ErrNotInCluster
  	}
  
  	// 读取token内容
  	token, err := ioutil.ReadFile(tokenFile)
  	if err != nil {
  		return nil, err
  	}
  
  	// 读取ca证书内容。ca证书为Config对象非必须参数，若没有证书，TLSClientConfig.Insecure属性需要设置为True。
  	tlsClientConfig := TLSClientConfig{}
  	if _, err := certutil.NewPool(rootCAFile); err != nil {
  		klog.Errorf("Expected to load root CA config from %s, but got err: %v", rootCAFile, err)
  	} else {
  		tlsClientConfig.CAFile = rootCAFile
  	}
  
  	return &Config{
  		// TODO: switch to using cluster DNS.
  		Host:            "https://" + net.JoinHostPort(host, port),
  		TLSClientConfig: tlsClientConfig,
  		BearerToken:     string(token),
  		BearerTokenFile: tokenFile,
  	}, nil
  }
  ```

##### 3. 问题

- `DynamicClient`相对于`ClientSet`主要是实现了对 `CRD`资源的操作，但是很多`CRD`资源已经实现了对应的`ClientSet`，是不是可以理解为`DynamicClient`客户端使用场景很少？