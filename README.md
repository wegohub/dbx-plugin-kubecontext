# KubeContext for DBX

一个连接对应一个 kubeconfig 汇总文件。在原版 DBX 中使用，无需修改或替换宿主。

## 使用

1. 「新建连接」→ KubeContext → 选择汇总文件，例如 `~/.kube/config`。
2. 工作台顶部选择 Context，自动选中该 context 的 namespace；未配置时使用 `default`。选择 context 会更新连接文件的 `current-context`。
3. 「添加 context」选择 YAML 或直接拖入工作台，合并到连接文件，写入前备份，完成后刷新选项。
4. 列表支持按名称、节点、IP、所属对象和镜像过滤。Pod 显示状态、就绪、重启次数、CPU/内存用量、Request/Limit、节点、IP、所属对象、镜像和存活时间，横向滚动查看所有列，操作列固定在右侧；工作负载、Service、Node、PVC 使用各自的详细列。
5. 点击资源打开详情，拖动抽屉左边界调整宽度；方向键微调，双击恢复默认，宽度会记住。Pod 日志实时渲染，默认保留 100 行，可改为 1–5000 行。
6. Pod 详情「监控」每 5 秒刷新 CPU、内存、临时磁盘及网络曲线，可改为 10/30 秒，支持暂停与手动刷新。保留最近 60 个采样点，显示指标来源、时间及容器资源配置；不可用指标显示原因。
7. Pod 右侧「进入」选择运行中的容器及 sh/bash，在独立终端标签中打开。可同时打开多个容器和本地终端，每个标签单独关闭/重启；「＋ 本地」新建 shell，可以直接执行 `kubectl get pod`。切换 context 后保留原标签，关闭工作台结束全部终端。

长字段悬停显示完整内容，提示支持换行且不会跑出视口。配置文件路径和 context 数量位于标题栏，不再单独占一行。按钮背景和文字使用 DBX 配套主题色，悬停/按下及复选框勾选也保持对比度。下拉菜单、输入框、复选框跟随 DBX 主题；下拉菜单支持键盘操作、输入定位，选项较多时提供搜索。

## 资源只读边界

**插件资源界面和后端 API 永久禁止删除、扩缩容和滚动重启**。旧连接的 `read_only=false` 也不能开启写操作，表单不再提供关闭只读的开关。HTTP 层拒绝资源写请求；身份及权限自检使用 Kubernetes 的非持久化 self-review 接口。

按用户要求保留本地交互终端。终端执行用户手动输入的命令，遵循本机权限和 Kubernetes RBAC，不属于资源 API 的只读限制。kubeconfig 导入和默认 context 切换仍可修改本地配置文件。

## 开发与验证

```sh
cd backend
go test -race ./...
go vet ./...
cd ..
npm ci
npm test
python3 tests/integration.py
dbx-plugin package .
```

黑盒测试使用临时 kubeconfig 和假集群，需要 Go、Python、kubectl。调试使用标准 `dbx-plugin dev --path . --port 5190`，不修改 DBX 或 SDK。

## 边界

- CPU/内存实际用量来自 Metrics API，列表用量随列表刷新读取；监控页持续采样。Request/Limit 来自 Pod 配置，包含 init、sidecar 与 overhead 的有效预算；部分容器未设置 Limit 时明确标注，不误报为有限配额。Node 列仍展示 allocatable。
- 临时磁盘、挂载卷及网络需要 kubelet Summary API（`nodes/proxy` 读取权限）。缺少指标服务或权限时保留可读指标并显示具体原因，不自动授权、不填假 0。网络速率由相邻累计采样计算，首点、计数回退不显示速率；数据仅保留在当前打开的监控页，不提供历史存储。
- 容器终端依赖本地 kubectl、`pods/exec` 权限及镜像内的 sh/bash，已结束的 Pod 禁止进入。
- kubeconfig exec 凭据默认关闭，暂不支持需要交互登录的凭据插件。
- 合并重排 YAML 并丢失注释，备份保留原始字节。含相对证书路径的配置可从新建/编辑连接表单选择源文件导入，或改为内嵌证书。
- 已有终端使用独立配置快照；导入更新凭据后需重启相应终端。
- PTY 支持 macOS/Linux，未实现 Windows ConPTY。随包提供 xterm.js 与许可证。
- 不提交 `dist/`、`.dbx-dev/`、`node_modules/`；调试目录可能含凭据。

[集成与交互](docs/host-integration.md) · [验证记录](VALIDATION.md)
