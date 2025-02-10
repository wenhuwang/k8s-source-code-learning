### 准入控制器介绍

引用 K8S 官方文档的介绍：

准入 Webhook 是一种用于接收准入请求并对其进行处理的 HTTP 回调机制。 可以定义两种类型的准入 Webhook， 即**验证性质的准入 Webhook**和**变更性质的准入 Webhook**。 变更性质的准入 Webhook 会先被调用。它们可以修改发送到 API 服务器的对象以执行自定义的设置默认值操作。

在完成了所有对象修改并且 API 服务器也验证了所传入的对象之后， 验证性质的 Webhook 会被调用，并通过拒绝请求的方式来强制实施自定义的策略。

### kubewebhook

我们使用 [kubewebhook](https://github.com/slok/kubewebhook) 库来开发自己的准入控制器，kubewebhook 库支持以下特性：

- 简单上手、可扩展以及使用灵活
- 支持验证类型和变更类型的 Webhook
- 支持 K8S 原生资源和用户 CRD 资源
- 灵活组装多个 Webhook 在同一个服务
- 提供 Webhook metrics 指标和 Tracing 指标

#### 部署

##### 官方部署步骤

首先在集群中部署 kubewebhook 库的 pod-annotate 例子来验证一下功能，K8S集群版本是 v1.22，部署步骤如下所示：

- 更新 SSL 证书：`cd examples && bash create-certs.sh`
- 部署 Webhook 证书资源：`kubectl apply -f ./pod-annotate/deploy/webhook-certs.yaml`
- 部署 Webhook 服务：`kubectl apply -f ./pod-annotate/deploy/webhook.yaml`
- 注册 mutating webhook 配置到 apiserver：`kubectl apply -f ./pod-annotate/deploy/webhook-registration.yaml`

使用验证的 deployment 验证 Webhook 功能：`kubectl apply -f ./pod-annotate/deploy/test-deployment.yaml`，发现 pod 无法成功创建。

nginx-test replicas 事件如下：

```shell
✗ k describe rs nginx-test-6799fc88d8
...
Conditions:
  Type             Status  Reason
  ----             ------  ------
  ReplicaFailure   True    FailedCreate
Events:
  Type     Reason        Age                From                   Message
  ----     ------        ----               ----                   -------
  Warning  FailedCreate  3s (x13 over 23s)  replicaset-controller  Error creating: Internal error occurred: failed calling webhook "pod-annotate-webhook.slok.dev": failed to call webhook: Post "https://pod-annotate-webhook.default.svc:443/mutate?timeout=10s": x509: certificate relies on legacy Common Name field, use SANs or temporarily enable Common Name matching with GODEBUG=x509ignoreCN=0
```

webhook 服务日志：

```shell
✗ k logs -f pod-annotate-webhook-6b69f49879-7thgt
2024/08/20 05:24:07 [INFO] Listening on :8080
2024/08/20 05:24:47 http: TLS handshake error from 10.165.112.169:25265: remote error: tls: bad certificate
2024/08/20 05:24:47 http: TLS handshake error from 10.165.112.169:25267: remote error: tls: bad certificate
2024/08/20 05:24:47 http: TLS handshake error from 10.165.112.169:25271: remote error: tls: bad certificate
2024/08/20 05:24:47 http: TLS handshake error from 10.165.112.169:25273: remote error: tls: bad certificate
```

##### 问题排查

结合 [Kubernetes Admission Webhook 部署和调试](https://ch3n9w.github.io/posts/tech-kubernetes-webhook/) 和 [阳明大佬的 admission-webhook-example 项目](https://github.com/cnych/admission-webhook-example) 等资料，找到了解决方法。

创建证书的脚本 `create-certs.sh` 文件需要做如下调整：

1、生成 `webhookCA.csr` 证书文件需要引入 `csr.conf` 配置文件

```bash
cat <<EOF >> ./csr.conf
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[ v3_req ]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names
[alt_names]
DNS.1 = ${WEBHOOK_SVC}
DNS.2 = ${WEBHOOK_SVC}.${WEBHOOK_NS}
DNS.3 = ${WEBHOOK_SVC}.${WEBHOOK_NS}.svc
EOF

openssl req -new -key ./webhookCA.key -days 36500 -subj "/CN=system:node:${WEBHOOK_SVC}.${WEBHOOK_NS}.svc/O=system:nodes" -out ./webhookCA.csr -config csr.conf
```

2、使用 k8s 默认的证书管理机制颁发 `webhook.crt` 证书

```bash
CSR_NAME="${WEBHOOK_SVC}-${WEBHOOK_NS}"
kubectl delete csr ${CSR_NAME} 2>/dev/null || true

cat <<EOF | kubectl create -f -
apiVersion: certificates.k8s.io/v1
kind: CertificateSigningRequest
metadata:
  name: ${CSR_NAME}
spec:
  signerName: kubernetes.io/kubelet-serving
  groups:
  - system:authenticated
  request: $(cat ./webhookCA.csr | base64 | tr -d '\n')
  usages:
  - digital signature
  - key encipherment
  - server auth
EOF

kubectl certificate approve ${CSR_NAME}

kubectl get csr ${CSR_NAME} -o jsonpath='{.status.certificate}' | openssl base64 -d -A -out ./webhook.crt
```

3、获取 ca 证书方式更新

```bash
CA_BUNDLE=$(kubectl get configmap -n kube-system extension-apiserver-authentication  -o=jsonpath='{.data.client-ca-file}' | base64 -w0)
```

更新完 `create-certs.sh` 脚本文件后最终部署成功，nginx-test 应用 pod 注解也自动注入成功。



### 开发自己的准入控制器

以我们自己的一个需求为例：给 deployment 资源中 logkit 容器环境变量中注入一些环境信息。

项目位置：[deployment-inject-env](https://github.com/wenhuwang/K8S-Learning-And-Pratice/tree/main/webhook/mutating/deployment-inject-env)
#### 更新 webhook 代码

直接照着 pod-annotate 示例代码改：

```diff
diff --git a/examples/pod-annotate/main.go b/examples/pod-annotate/main.go
index 135490e..0138444 100644
--- a/examples/pod-annotate/main.go
+++ b/examples/pod-annotate/main.go
@@ -7,6 +7,7 @@ import (
 	"net/http"
 	"os"

+	appsv1 "k8s.io/api/apps/v1"
 	corev1 "k8s.io/api/core/v1"
 	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

@@ -17,22 +18,26 @@ import (
 	kwhmutating "github.com/slok/kubewebhook/v2/pkg/webhook/mutating"
 )

-func annotatePodMutator(_ context.Context, _ *kwhmodel.AdmissionReview, obj metav1.Object) (*kwhmutating.MutatorResult, error) {
-	pod, ok := obj.(*corev1.Pod)
+func injectLogkitEnvMutator(_ context.Context, _ *kwhmodel.AdmissionReview, obj metav1.Object) (*kwhmutating.MutatorResult, error) {
+	deploy, ok := obj.(*appsv1.Deployment)
 	if !ok {
 		// If not a pod just continue the mutation chain(if there is one) and don't do nothing.
 		return &kwhmutating.MutatorResult{}, nil
 	}

-	// Mutate our object with the required annotations.
-	if pod.Annotations == nil {
-		pod.Annotations = make(map[string]string)
+	containers := deploy.Spec.Template.Spec.Containers
+	for i, c := range containers {
+		if c.Name == "logkit" {
+			c.Env = append(c.Env, corev1.EnvVar{
+				Name:  "FOO",
+				Value: "BAR",
+			})
+			containers[i].Env = c.Env
+		}
 	}
-	pod.Annotations["mutated"] = "true"
-	pod.Annotations["mutator"] = "pod-annotate"

 	return &kwhmutating.MutatorResult{
-		MutatedObject: pod,
+		MutatedObject: deploy,
 	}, nil
 }

@@ -60,11 +65,11 @@ func main() {
 	cfg := initFlags()

 	// Create our mutator
-	mt := kwhmutating.MutatorFunc(annotatePodMutator)
+	mt := kwhmutating.MutatorFunc(injectLogkitEnvMutator)

 	mcfg := kwhmutating.WebhookConfig{
-		ID:      "podAnnotate",
-		Obj:     &corev1.Pod{},
+		ID:      "injectLogkitEnv",
+		Obj:     &appsv1.Deployment{},
 		Mutator: mt,
 		Logger:  logger,
 	}
```



#### 更新 webhook 配置

```diff
diff --git a/examples/pod-annotate/deploy/webhook-registration.yaml.tpl b/examples/pod-annotate/deploy/webhook-registration.yaml.tpl
index ff11b68..09a21f8 100644
--- a/examples/pod-annotate/deploy/webhook-registration.yaml.tpl
+++ b/examples/pod-annotate/deploy/webhook-registration.yaml.tpl
@@ -1,4 +1,4 @@
-apiVersion: admissionregistration.k8s.io/v1beta1
+apiVersion: admissionregistration.k8s.io/v1
 kind: MutatingWebhookConfiguration
 metadata:
   name: pod-annotate-webhook
@@ -7,6 +7,9 @@ metadata:
     kind: mutator
 webhooks:
   - name: pod-annotate-webhook.slok.dev
+    admissionReviewVersions:
+    - v1
+    - v1beta1
     clientConfig:
       service:
         name: pod-annotate-webhook
@@ -15,7 +18,11 @@ webhooks:
       caBundle: CA_BUNDLE
     rules:
       - operations: [ "CREATE" ]
-        apiGroups: [""]
+        apiGroups: ["apps"]
         apiVersions: ["v1"]
-        resources: ["pods"]
+        resources: ["deployments"]
+    namespaceSelector:
+      matchLabels:
+        kubernetes.io/metadata.name: demo
+    sideEffects: None
```

