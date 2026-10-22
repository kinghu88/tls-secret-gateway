# tls-secret-gateway

一个安全的 HTTP 网关服务，用于受控地获取 Kubernetes 集群中**所有 namespace 下 `type=kubernetes.io/tls` 的 Secret**。

## 解决的问题

Kubernetes RBAC 不支持按 Secret 的 `type` 字段做行级过滤，要么给全部 Secret 的读权限（过度授权），要么完全读不到。

本服务通过一个持有集群级 Secret 读权限的 ServiceAccount 运行，但**只对外暴露 TLS 类型的 Secret**，调用方无法接触到数据库密码、token 等其他敏感 Secret。同时通过 TokenReview 鉴权 + SA 白名单进一步收紧调用入口。

## 架构

```
调用方(SA token) → Ingress → tls-secret-gateway Pod
                           ├── Informer 监听所有 Secret，仅缓存 kubernetes.io/tls 类型
                           └── TokenReview 校验调用方身份 + 白名单过滤
```

- **权限边界**：网关 SA 能读所有 Secret，但缓存层只保留 TLS 类型，API 永远返回不了其他类型
- **调用方鉴权**：调用方必须携带有效的 Kubernetes SA token，通过 TokenReview 校验，可选白名单限制
- **性能**：基于 Informer 本地缓存 + Watch，不直接请求 API Server，无额外压力

## 项目结构

```
.
├── cmd/server/main.go          # 入口：环境变量 + in-cluster config
├── internal/
│   ├── cache/cache.go          # SharedInformer 缓存，仅保留 TLS Secret
│   ├── handler/handler.go      # HTTP API（list / get）
│   ├── auth/auth.go            # TokenReview 鉴权 + 白名单中间件
│   └── server/server.go        # 健康检查、优雅关闭、路由编排
├── deploy/
│   ├── rbac.yaml               # 代理 SA + ClusterRole(读Secret) + TokenReview 权限
│   ├── deployment.yaml         # Deployment 配置
│   ├── service.yaml            # ClusterIP Service
│   └── client-example.yaml     # 调用方 SA 示例
├── Dockerfile                  # 多阶段构建 + distroless nonroot
└── go.mod
```

## 构建镜像

```bash
docker build -t harbor.xxxx.com/library/tls-secret-gateway:latest .
docker push harbor.xxxx.com/library/tls-secret-gateway:latest
```

## 部署

### 1. 确认镜像地址

编辑 `deploy/deployment.yaml`，将 `image` 改为你推送的镜像地址：

```yaml
image: harbor.xxxx.com/library/tls-secret-gateway:latest
```

### 2. 配置鉴权白名单（生产环境必做）

在 `deploy/deployment.yaml` 中配置 `ALLOWED_SERVICE_ACCOUNTS`：

```yaml
- name: AUTH_ENABLED
  value: "true"
- name: ALLOWED_SERVICE_ACCOUNTS
  value: "tls-secret-gateway:tls-secret-gateway,monitoring:prometheus"   # 留空表示允许任意已认证 SA
```

格式：`namespace:serviceaccount-name`，多个用逗号分隔。

### 3. 应用部署

```bash
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/deployment.yaml
kubectl apply -f deploy/service.yaml
```

验证：

```bash
kubectl get pods -n tls-secret-gateway
kubectl logs -n tls-secret-gateway -l app=tls-secret-gateway --tail=20
```

### 4. 配置 Ingress（可选）

创建 Ingress 暴露服务，例如：

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: tls-secret-gateway
  namespace: tls-secret-gateway
spec:
  rules:
    - host: tls-secret-gateway.xxxx.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: tls-secret-gateway
                port:
                  number: 80
```

## 配置项

通过环境变量配置，在 `deploy/deployment.yaml` 的 `env` 中修改：

| 环境变量                   | 默认值  | 说明                                                                  |
| -------------------------- | ------- | --------------------------------------------------------------------- |
| `LISTEN_ADDR`              | `:8080` | 监听地址                                                              |
| `RESYNC_PERIOD`            | `10m`   | Informer 全量重同步周期，`0` 关闭                                     |
| `AUTH_ENABLED`             | `true`  | 是否启用 TokenReview 鉴权，生产环境必须为 `true`                      |
| `ALLOWED_SERVICE_ACCOUNTS` | `""`    | 允许调用的 SA 白名单，格式 `ns:sa`，逗号分隔；空表示允许任意已认证 SA |

修改后重启生效：

```bash
kubectl rollout restart deploy/tls-secret-gateway -n tls-secret-gateway
```

## API 文档

### 鉴权

所有 `/v1/` 接口需要在请求头中携带有效的 Kubernetes ServiceAccount token：

```
Authorization: Bearer <sa-token>
```

`/healthz` 和 `/readyz` 不需要鉴权（供 kubelet 探针使用）。

### 端点

#### `GET /v1/tls-secrets`

列出所有 TLS Secret，支持按 namespace 过滤。

Query 参数：

| 参数        | 说明                                     |
| ----------- | ---------------------------------------- |
| `namespace` | 可选，只返回该 namespace 下的 TLS Secret |

响应示例：

```json
{
  "items": [
    {
      "namespace": "default",
      "name": "my-tls-cert",
      "type": "kubernetes.io/tls",
      "labels": { "app": "web" },
      "annotations": null,
      "data": {
        "tls.crt": "LS0tLS1CRUdJTi...",
        "tls.key": "LS0tLS1CRUdJTi..."
      },
      "resourceVersion": "12345"
    }
  ],
  "count": 1
}
```

`data.tls.crt` 和 `data.tls.key` 是 base64 编码的 PEM 内容（Kubernetes Secret 原生存储格式）。

#### `GET /v1/tls-secrets/{namespace}/{name}`

获取单个 TLS Secret。

响应：单个 `secretView` 对象，结构同上；不存在返回 `404`。

#### `GET /healthz`

存活探针，返回 `200 ok`。

#### `GET /readyz`

就绪探针，缓存同步完成返回 `200`，否则返回 `503`。

## 调用示例

### 集群外调用

#### 1. 创建调用方的 SA ，默认会创建一个token Secret

```bash
kubectl create sa tls-secret-gateway-caller -n tls-secret-gateway
```

```bash
TOKEN=$(kubectl get secret tls-secret-gateway-caller -n tls-secret-gateway -o jsonpath='{.data.token}' | base64 -d)
```

#### 2. 调用 API

```bash
# 列出所有 TLS Secret
curl -sS -H "Authorization: Bearer $TOKEN" \
  https://tls-secret-gateway.xxxx.com/v1/tls-secrets | jq

# 指定 namespace
curl -sS -H "Authorization: Bearer $TOKEN" \
  "https://tls-secret-gateway.xxxx.com/v1/tls-secrets?namespace=cert-manager" | jq

# 获取单个
curl -sS -H "Authorization: Bearer $TOKEN" \
  https://tls-secret-gateway.xxxx.com/v1/tls-secrets/default/my-tls-cert | jq
```

### 集群内 Pod 调用

Pod 会自动挂载自己的 SA token，直接读取即可：

```bash
TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)

# 走 Service 域名（不经 Ingress）
curl -sS -H "Authorization: Bearer $TOKEN" \
  http://tls-secret-gateway.tls-secret-gateway.svc.cluster.local/v1/tls-secrets

# 或走 Ingress 域名
curl -sS -H "Authorization: Bearer $TOKEN" \
  https://tls-secret-gateway.xxxx.com/v1/tls-secrets
```

调用方 SA **不需要**任何 `secrets` 的 RBAC 权限，只需能访问代理 Service 并通过 TokenReview。

## 鉴权白名单机制

### 工作原理

1. 调用方携带 SA token 请求
2. 代理调用 Kubernetes TokenReview API 校验 token 有效性
3. 从 token 解析出身份，格式为 `system:serviceaccount:<namespace>:<name>`
4. 若 `ALLOWED_SERVICE_ACCOUNTS` 非空，检查 `<namespace>:<name>` 是否在白名单中

### 配置示例

只允许 `default:tls-caller` 和 `monitoring:prometheus` 调用：

```yaml
- name: ALLOWED_SERVICE_ACCOUNTS
  value: "default:tls-caller,monitoring:prometheus"
```

### 验证

```bash
# 白名单内 → 200
TOKEN=$(kubectl create token tls-caller -n default)
curl -sS -o /dev/null -w "%{http_code}\n" -H "Authorization: Bearer $TOKEN" \
  https://tls-secret-gateway.xxxx.com/v1/tls-secrets

# 白名单外 → 401
OTHER_TOKEN=$(kubectl create token default -n default)
curl -sS -o /dev/null -w "%{http_code}\n" -H "Authorization: Bearer $OTHER_TOKEN" \
  https://tls-secret-gateway.xxxx.com/v1/tls-secrets
```

## 常见问题排查

### 504 Gateway Time-out

Ingress 转发到后端超时，按以下顺序排查：

```bash
# 1. Pod 是否就绪
kubectl get pods -n tls-secret-gateway
kubectl describe pod -n tls-secret-gateway -l app=tls-secret-gateway | tail -30

# 2. Service 是否有 Endpoints
kubectl get endpoints tls-secret-gateway -n tls-secret-gateway

# 3. 集群内绕过 Ingress 测试
kubectl run curl-test --rm -it -n default --image=curlimages/curl --restart=Never -- \
  curl -sS http://tls-secret-gateway.tls-secret-gateway.svc.cluster.local:80/healthz

# 4. 看代理日志（请求是否到达）
kubectl logs -n tls-secret-gateway -l app=tls-secret-gateway --tail=50 -f
```

### 401 Unauthorized

- 没带 `Authorization` 头或 token 无效/过期
- 白名单未包含该 SA，查看代理日志确认拒绝原因：
  ```bash
  kubectl logs -n tls-secret-gateway -l app=tls-secret-gateway | grep "auth failed"
  ```

### 返回 count: 0

集群中确实没有 `kubernetes.io/tls` 类型的 Secret。检查：

```bash
kubectl get secrets -A --field-selector type=kubernetes.io/tls
```

## 安全建议

1. **生产环境必须配置白名单**：`ALLOWED_SERVICE_ACCOUNTS` 留空会允许任意已认证 SA 读取所有 TLS 证书
2. **优先使用短期 token**：`kubectl create token` 生成的短期 token 比手动创建的长期 Secret 更安全
3. **镜像不可篡改**：使用 image digest 而非 tag 部署
4. **限制代理 SA 所在 namespace**：只允许 `tls-secret-gateway` namespace 运行该服务
5. **TLS 证书保护**：本服务返回的 `tls.key` 是私钥，确保调用方和传输链路可信
6. **审计日志**：通过 klog 输出的请求日志可接入集群日志系统审计
