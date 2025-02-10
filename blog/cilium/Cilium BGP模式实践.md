## Cilium BGP模式实践

#### cilium 路由介绍

##### Encapsulation

当没有任何配置更改时，cilium 将会运行这种依赖最少的 underlay 网络架构；在这种模式下，所有的集群节点作为隧道网格使用基于 UDP 封装的协议：`VXLAN` 或者 `Geneve`，所有集群节点之间的流量都会被封装。

###### 网络依赖

- 封装模式依赖节点之间的连通性。
- underlay 网络和防火墙允许封装模式的包通过：

| Encapsulation Mode | Port Range / Protocol |
| ------------------ | --------------------- |
| VXLAN (Default)    | 8472/UDP              |
| Geneve             | 6081/UDP              |

##### Native-Routing模式

原生路由模式开启需要 `tunnel: disabled` 并且开启原生包转发模式，原生包转发模式依赖 cilium 实现路由能力而不是使用封装的方式。

![](../../images/native_routing.png)

在原生路由模式下，cilium 将会把所有的没有处理的数据包委派给 Linux 路由子系统，这就意味着数据包会直接被路由出该主机，因此集群节点必须能够路由 pod CIDR。

当开启原生路由配置时 cilium 会自动打开 Linux IP 转发能力。

###### 网络依赖

- 运行原生路由模式，cilium 所在的节点必须能够直接转发所有 pod 或者其他工作负载的 IP 流量；
-  每一个节点 Linux 内核都必须知道如何转发所有 cilium 节点上 pod 或者其他工作负载的数据包，可以通过以下两种方式实现：
  - 节点本身并不知道如何路由到所有 pod IP，但是上层网络存在到所有 pod IP 的路由。在这种情况下，节点上会根据默认路由去外部网络设备寻找路由路径；
  - 每个节点知道其他所有节点 pod IP 并且节点上有对应 pod IP 的路由。如果所有节点都在一个2层网络，可以通过 `auto-direct-node-routes: true` 配置达到效果；否则必须通过一个额外的 BGP daemon 组件来实现路由器的功能。可以参考下面 `kube-router` 是如何实现的。

cilium 官方提供了几种使用实现原生路由模式的方案，如下所示：

- [Using kube-router to run BGP](https://docs.cilium.io/en/stable/gettingstarted/kube-router/)
- [Using BIRD to run BGP](https://docs.cilium.io/en/stable/gettingstarted/bird/)
- [BGP模式（本次介绍）](https://docs.cilium.io/en/stable/gettingstarted/bgp/)
- [Cilium BGP Control Plane](https://docs.cilium.io/en/stable/gettingstarted/bgp-control-plane/)

#### 部署（BGP模式）

##### 环境

1、k8s 版本：v1.22.10

2、linux 内核版本 >= 5.10.0

3、容器运行时：containerd://1.6.8

##### 安装 cilium

Helm 安装之前，需要提前通过 configmap 创建相关 BGP 配置，否则 cilium 节点无法与物理设备建立 BGP 邻居。

创建 BGP Configmap配置，可以配置全局的 BGP peer，也可以根据主机组配置 BGP peer。如下所示

```yaml
$ kubectl  -n kube-system get cm bgp-config -oyaml
apiVersion: v1
data:
  config.yaml: |
    peers:
      - peer-address: 10.4.36.239
        peer-asn: 64524
        my-asn: 64513
      - peer-address: 10.4.36.238
        peer-asn: 64524
        my-asn: 64513
      - peer-address: 10.4.36.250
        peer-asn: 64512
        my-asn: 64512
        node-selectors:
        - match-expressions:
          - key: kubernetes/route
            operator: In
            values: ["bgp-peer-83"]
      - peer-address: 10.4.36.251
        peer-asn: 64512
        my-asn: 64512
        node-selectors:
        - match-expressions:
          - key: kubernetes/route
            operator: In
            values: ["bgp-peer-83"]
    address-pools:
      - name: default
        protocol: bgp
        addresses:
          - 10.5.112.0/21
kind: ConfigMap
metadata:
  name: bgp-config
  namespace: kube-system
```

设置 helm 仓库源

```
helm repo add cilium https://helm.cilium.io/
```

cilium版本为 v1.11.9，value.yaml 主要配置如下：

```yaml
# -- Enable installation of PodCIDR routes between worker
# nodes if worker nodes share a common L2 network segment.
autoDirectNodeRoutes: true

# -- Configure BGP
bgp:
  # -- Enable BGP support inside Cilium; embeds a new ConfigMap for BGP inside
  # cilium-agent and cilium-operator
  enabled: true
  announce:
    # -- Enable allocation and announcement of service LoadBalancer IPs
    loadbalancerIP: true
    # -- Enable announcement of node pod CIDR
    podCIDR: true

# -- Configure whether to install iptables rules to allow for TPROXY
# (L7 proxy injection), iptables-based masquerading and compatibility
# with kube-proxy.
installIptablesRules: true

ipam:
  # -- Configure IP Address Management mode.
  # ref: https://docs.cilium.io/en/stable/concepts/networking/ipam/
  mode: "cluster-pool"
  operator:
    # -- Deprecated in favor of ipam.operator.clusterPoolIPv4PodCIDRList.
    # IPv4 CIDR range to delegate to individual nodes for IPAM.
    clusterPoolIPv4PodCIDR: "10.5.112.0/21"
    # -- IPv4 CIDR list range to delegate to individual nodes for IPAM.
    clusterPoolIPv4PodCIDRList: []
    # -- IPv4 CIDR mask size to delegate to individual nodes for IPAM.
    clusterPoolIPv4MaskSize: 24
    # -- Deprecated in favor of ipam.operator.clusterPoolIPv6PodCIDRList.
    # IPv6 CIDR range to delegate to individual nodes for IPAM.
    clusterPoolIPv6PodCIDR: "fd00::/104"
     # -- IPv6 CIDR list range to delegate to individual nodes for IPAM.
    clusterPoolIPv6PodCIDRList: []
    # -- IPv6 CIDR mask size to delegate to individual nodes for IPAM.
    clusterPoolIPv6MaskSize: 120

# -- Configure the eBPF-based ip-masq-agent
ipMasqAgent:
  enabled: false

# iptablesLockTimeout defines the iptables "--wait" option when invoked from Cilium.
# iptablesLockTimeout: "5s"

ipv4:
  # -- Enable IPv4 support.
  enabled: true
  
# -- Configure the kube-proxy replacement in Cilium BPF datapath
# Valid options are "disabled", "probe", "partial", "strict".
# ref: https://docs.cilium.io/en/stable/gettingstarted/kubeproxy-free/
kubeProxyReplacement: "strict"

l2NeighDiscovery:
  # -- Enable L2 neighbor discovery in the agent
  enabled: true
  # -- Override the agent's default neighbor resolution refresh period.
  refreshPeriod: "30s"

# -- Enable Layer 7 network policy.
l7Proxy: true

# -- Enable Local Redirect Policy.
localRedirectPolicy: false

# -- Configure the encapsulation configuration for communication between nodes.
# Possible values:
#   - disabled
#   - vxlan (default)
#   - geneve
tunnel: "disabled"
```

安装/更新cilium

```shell
// 安装
$ helm -n kube-system install cilium -f values.yaml ./
// 升级
$ helm -n kube-system upgrade cilium -f values.yaml ./
```

#### 网络架构

网络架构图如下所示：

![](../../images/cilium%20BGP网络架构.png)

k8s 宿主机属于不同的两个网段 10.4.94.0/24 和 10.4.83.0/24，不同网段节点的 BGP peer 配置也不同，因为开启了 autoDirectNodeRoutes 配置，同一个二层网络的宿主机之间 pod CIDR 路由时下一跳为对应的宿主机 IP，不同不同二层网络的宿主机之间 pod CIDR路由时则走的默认路由。

同网段宿主机之间 pod 路由路径：

```shell
/ # traceroute 10.5.114.1
traceroute to 10.5.114.1 (10.5.114.1), 30 hops max, 46 byte packets
 1  *  *  *
 2  *  *  *
 3  10.5.114.1 (10.5.114.1)  0.240 ms  0.129 ms  0.141 ms
```

![](../../images/cilium%20bgp同网段主机流量路径.png)

不通网络宿主机之间 pod 路由路径：

```shell
/ # traceroute 10.5.119.30
traceroute to 10.5.119.30 (10.5.119.30), 30 hops max, 46 byte packets
 1  *  *  *
 2  10.4.94.1 (10.4.94.1)  5.620 ms  4.881 ms  4.888 ms
 3  10.4.83.1 (10.4.83.1)  36.239 ms  2.600 ms  195.274 ms
 4  *  *  *
 5  10.5.119.30 (10.5.119.30)  0.285 ms  0.205 ms  0.164 ms
```

![](../../images/cilium%20bgp不同网段主机流量路径.png)



参考链接：
https://github.com/cilium/cilium/blob/fbc53d084ce34159a3fde3b19e26fc2fbbef9e52/Documentation/network/concepts/routing.rst
https://docs.cilium.io/en/stable/gettingstarted/bgp/