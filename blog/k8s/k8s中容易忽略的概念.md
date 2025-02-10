## 知识梳理

### 关于`pod.Status.StartTime`如何理解？

##### 定义

`RFC 3339 date and time at which the object was acknowledged by the Kubelet. This is before the Kubelet pulled the container image(s) for the pod.`

API文档：https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#podstatus-v1-core

该字段相关讨论issues：

https://github.com/kubernetes/kubernetes/issues/9792

https://github.com/kubernetes/kubernetes/pull/7868

##### 源码实现

```
# kubelet v1.22
syncPod --> kl.statusManager.SetPodStatus --> m.updateStatusInternal(初始化status.StartTime)

```

![](https://pic2.zhimg.com/80/v2-e565aa5843306d1987a2d0cbd7a748f9_1440w.webp)

参考链接：

https://github.com/kubernetes/kubernetes/pull/7868/files#diff-0948099152a755b2bfbf0af54664ab916f5f118ca00045b3511865821b1c7482

https://zhuanlan.zhihu.com/p/338462784

