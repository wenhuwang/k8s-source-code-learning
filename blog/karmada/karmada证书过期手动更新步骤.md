## karmada证书过期手动更新步骤

### 背景

测试环境 karmada 证书突然过期导致控制器 pod 不断重启，现象如下：

```
# kubectl -n karmada-system-fat get pods
NAME                                            READY   STATUS             RESTARTS       AGE
etcd-0                                          1/1     Running            0              119d
karmada-aggregated-apiserver-7756b97b9f-crn28   1/1     Running            2 (119d ago)   119d
karmada-apiserver-79c6579dfc-ptk44              1/1     Running            0              119d
karmada-controller-manager-55cf44f589-rj7hc     0/1     CrashLoopBackOff   2 (16s ago)    32s
karmada-scheduler-5d6d98bb84-f7qx2              1/1     Running            2 (24h ago)    365d
karmada-webhook-6945cd5846-wzcls                1/1     Running            0              119d
kube-controller-manager-5b79c467fd-6bx8g        0/1     CrashLoopBackOff   2 (15s ago)    38s


$ kubectl -n karmada-system-fat logs -f karmada-apiserver-79c6579dfc-ptk44 
W0122 07:17:12.851322       1 logging.go:59] [core] [Channel #818161 SubChannel #818162] grpc: addrConn.createTransport failed to connect to {
  "Addr": "etcd-0.etcd.karmada-system-fat.svc.cluster.local:2379",
  "ServerName": "etcd-0.etcd.karmada-system-fat.svc.cluster.local",
  "Attributes": null,
  "BalancerAttributes": null,
  "Type": 0,
  "Metadata": null
}. Err: connection error: desc = "transport: authentication handshake failed: x509: certificate has expired or is not yet valid: current time 2025-01-22T07:17:12Z is after 2025-01-21T08:03:24Z"

$ kubectl -n karmada-system-fat logs -f karmada-controller-manager-55cf44f589-rj7hc 
E0122 08:04:31.280332       1 cluster.go:195] "Failed to get API Group-Resources" err="Get \"https://karmada-apiserver.karmada-system-fat.svc.cluster.local:5443/api?timeout=32s\": tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time 2025-01-22T08:04:31Z is after 2025-01-21T08:03:24Z"
E0122 08:04:31.280953       1 controllermanager.go:160] Failed to build controller manager: Get "https://karmada-apiserver.karmada-system-fat.svc.cluster.local:5443/api?timeout=32s": tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time 2025-01-22T08:04:31Z is after 2025-01-21T08:03:24Z
E0122 08:04:31.282808       1 run.go:74] "command failed" err="Get \"https://karmada-apiserver.karmada-system-fat.svc.cluster.local:5443/api?timeout=32s\": tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time 2025-01-22T08:04:31Z is after 2025-01-21T08:03:24Z"

$ kubectl -n karmada-system-fat logs -f kube-controller-manager-5b79c467fd-6bx8g
...
E0122 08:05:23.401207       1 run.go:74] "command failed" err="unable to load configmap based request-header-client-ca-file: Get \"https://karmada-apiserver.karmada-system-fat.svc.cluster.local:5443/api/v1/namespaces/kube-system/configmaps/extension-apiserver-authentication\": x509: certificate has expired or is not yet valid: current time 2025-01-22T08:05:23Z is after 2025-01-21T08:03:24Z"
```



### 处理步骤

参考官方仓库 issues 评论中 [更新证书思路](https://github.com/karmada-io/karmada/issues/4787#issuecomment-2516618327)，优化并整理了详细操作步骤。

##### 1. 导出 karmada 所有证书文件
主要是指将 karmada 依赖的 secret 中的证书文件导出到本地，以下是我自己写脚本一键导出的步骤，也可以自己手动导出

```shell
./download-karmada-cert -kubeconfig /root/.kube/config -namespace karmada-system-fat -output-dir ../pki

```

一键导出证书脚本源码链接：https://github.com/wenhuwang/k8s-source-code-learning/tree/main/script/download-karmada-cert

##### 2. 查看原证书信息确认证书配置

```shell
openssl x509 -in apiserver.pem -noout -text 
```

##### 3. 创建证书配置文件

证书配置文件模版链接，根据自己情况更新证书配置文件中的信息，主要包括 CN、O、host字段

##### 4. 生成证书

```shell
cfssl gencert -ca=../pki/ca.crt -ca-key=../pki/ca.key -config=ca-config.json -profile=kubernetes apiserver-csr.json | cfssljson -bare apiserver
cfssl gencert -ca=../pki/ca.crt -ca-key=../pki/ca.key -config=ca-config.json -profile=kubernetes karmada-csr.json | cfssljson -bare karmada
cfssl gencert -ca=../pki/etcd-ca.crt -ca-key=../pki/etcd-ca.key -config=ca-config.json -profile=kubernetes etcd-client-csr.json | cfssljson -bare etcd-client
cfssl gencert -ca=../pki/etcd-ca.crt -ca-key=../pki/etcd-ca.key -config=ca-config.json -profile=kubernetes etcd-server-csr.json | cfssljson -bare etcd-server
cfssl gencert -ca=../pki/front-proxy-ca.crt -ca-key=../pki/front-proxy-ca.key -config=ca-config.json -profile=kubernetes front-proxy-client-csr.json | cfssljson -bare front-proxy-client
cfssl gencert -ca=../pki/ca.crt -ca-key=../pki/ca.key -config=ca-config.json -profile=kubernetes tls-csr.json | cfssljson -bare tls
cfssl gencert -ca=../pki/ca.crt -ca-key=../pki/ca.key -config=ca-config.json -profile=kubernetes karmada-api-config-csr.json | cfssljson -bare karmada-api-config
```



##### 5. 更新证书到karmada secret（ca证书不需要更新）

需要更新的 secret 包含：`etc-cert` 、`karmada-cert`、`karmada-webhook-cert`、`kubeconfig`，除了 `kubeconfig` 更新方式要复杂一些，其它的证书直接复制粘贴，对应关系：`a.crt --> c.pem`，`a.key --> a-key.pem`

`kubeconfig` 中的证书文件对应关系：`client-certificate-data-->karmada-api-config.pem`， `client-key-data-->karmada-api-config-key.pem`，更新前需要先把证书文件内容实用 base64 进行编码。

```shell
cat karmada-api-config.pem | base64 -w 0
cat karmada-api-config-key.pem | base64 -w 0
```

##### 6. 重启相关pod

除了 etcd 其它的我都重启了，最终所有组件都正常可以用了