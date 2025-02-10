#### 一、增加相关监控指标

在 prometheus rules 配置中加入以下规则

```
- name: node.rules
  rules:
  - record: cpu_usage_active
    expr: 100 - (avg by (cluster, instance) (irate(node_cpu_seconds_total{mode="idle"}[30s])) * 100)
  - record: mem_usage_active
    expr: 100*(1-node_memory_MemAvailable_bytes/node_memory_MemTotal_bytes)
  - record: cpu_usage_max_avg_1h
    expr: max_over_time(cpu_usage_avg_5m[1h])
  - record: cpu_usage_max_avg_1d
    expr: max_over_time(cpu_usage_avg_5m[1d])
  - record: cpu_usage_avg_5m
    expr: avg_over_time(cpu_usage_active[5m])
  - record: mem_usage_max_avg_1h
    expr: max_over_time(mem_usage_avg_5m[1h])
  - record: mem_usage_max_avg_1d
    expr: max_over_time(mem_usage_avg_5m[1d])
  - record: mem_usage_avg_5m
    expr: avg_over_time(mem_usage_active[5m])
```

重启 prometheus，确保以上规则生效。

**注意**：如果重启 prometheus 以后 CPU 相关指标没有结果，需要确认一下 node-exporter 相关指标获取数据的频率是否小于 30s(推荐设置为10s)。

#### 二、安装 crane-scheduler 最为第二个调度器

**注意**：创建 deployment 资源时，需要根据自己环境修改 prometheus 或者 images 地址

1、安装 controller 组件，[yaml 文件位置](deploy/crane-scheduler-controller/)

```shell
# ll
total 32
-rw-r--r-- 1 root root 1515 Jan 11 17:17 controller-configmap.yaml
-rw-r--r-- 1 root root 1698 Jan 13 10:20 controller-deployment.yaml
-rw-r--r-- 1 root root  781 Jan 11 17:23 controller-rbac.yaml
-rw-r--r-- 1 root root  107 Jan 11 17:16 controller-serviceaccount.yaml
# kubectl crete ns crane-scheduler && kubectl apply -f ./
```

2、安装 scheduler 组件，[yaml 文件位置](deploy/crane-scheduler/)

```shell
# ll
total 32
-rw-r--r-- 1 root root  580 Jan 11 17:18 scheduler-configmap.yaml
-rw-r--r-- 1 root root 1464 Jan 13 10:29 scheduler-deployment.yaml
-rw-r--r-- 1 root root 2063 Jan 11 17:23 scheduler-rbac.yaml
-rw-r--r-- 1 root root   90 Jan 11 17:16 scheduler-serviceaccount.yaml
# kubectl apply -f ./
```

#### 三、测试 crane-scheduler 

使用以下应用指定 crane-scheduler 作为调度器

```yaml
kind: Deployment
apiVersion: apps/v1
metadata:
  name: nettools-deploy
  namespace: default
spec:
  selector:
    matchLabels:
      app: nettools-deploy
  template:
    metadata:
      labels:
        app: nettools-deploy
    spec:
      containers:
        - name: nettools-deploy
          image: 'docker.io/gocrane/stress:latest'
      schedulerName: crane-scheduler
```
