# KubeContext 交接 · 0.5.1

2026-10-19。只改插件，DBX 源码工作区干净，原 `/Applications/DBX.app` 未替换。用户明确要求保留本地终端。

## 关键行为

- 集群资源界面只读。删除、扩缩容、重启入口全部移除，后端方法明确拒绝，HTTP 层也拦截资源写入。旧 `read_only=false` 无效。
- 本地终端仍执行用户命令，权限由本机和集群 RBAC 决定；不要把 API 只读保证扩大到本地 shell。
- 一个连接对应一个 kubeconfig。顶部 Context 选择器更新默认 context，并使用所选 context 的 namespace；忽略旧连接级 namespace，缺省为 default。
- 主按钮和复选框使用宿主成对的 primary / primary-foreground；悬停、按下使用主色渐变，避免暗色主题浅底白字。
- 自定义下拉菜单跟随 DBX 主题，支持搜索、键盘和外部点击关闭。原 select 仅作隐藏数据模型。配置路径与 context 数量置于标题栏。
- 详情抽屉支持拖动、键盘调宽、双击重置、存储宽度。Pod 列表补充重启、节点、IP、所属对象、镜像；按资源类型展示不同列，异常容器状态优先于笼统 Running。
- 切换 context 清空旧详情、资源与日志，迟到响应通过版本号隔离。每个 context 保留多个本地/容器 PTY 标签，配置快照固定默认 context。
- 导入 YAML 合并到连接配置文件，备份后原子写入，并刷新下拉列表。弹窗可通过关闭按钮、遮罩、Esc 关闭。

- 所有长字段由插件 tooltip 展示全文。Pod 列表增加 CPU/内存用量与 Request/Limit；详情「监控」以 5/10/30 秒采样，SVG 曲线保留 60 点，离开页面立即停止。Metrics API 提供 CPU/内存，kubelet Summary 提供磁盘/网络，缺权限显示原因。
- Pod「进入」只列运行中的容器，选择 sh/bash，新标签启动 `kubectl exec -it`；直接构造 argv，不拼接命令。每个标签独立进程/输出/关闭，工作台关闭清理全部。

## 代码入口

| 范围 | 文件 |
|---|---|
| 协议与只读方法拦截 | `backend/main.go` |
| HTTP 层资源写入拦截 | `backend/client.go` |
| 资源列表与详细摘要 | `backend/resources.go` / `backend/summary_fields.go` |
| namespace 与会话隔离 | `backend/session.go` |
| 配置合并 | `backend/merge.go` |
| 工作台与列表 | `ui/index.html` |
| 指标 API 与资源数量解析 | `backend/metrics.go` / `backend/quantities.go` |
| 监控轮询、图表、速率计算 | `ui/monitor.js` |
| 本地 / 容器 PTY | `backend/terminal.go` |
| 自定义菜单、tooltip 与抽屉拖动 | `ui/controls.js` |
| 安全/摘要回归 | `backend/safety_summary_test.go` |
| UI / JSON-RPC 验收 | `tests/ui.test.mjs` / `tests/integration.py` |

## 继续工作约束

1. 不修改 DBX、SDK，不使用先前的宿主补丁或开发版应用。
2. 测试只写临时 kubeconfig；不得向用户文件导入测试 context。
3. 不恢复资源变更代码或“关闭只读”开关。HTTP POST 仅允许现有三类 self-review 路径。
4. 日志/PTY 事件可能先于 RPC 响应，必须提前注册 stream id。
5. 保留本地 Go SDK replace。UI 不接收凭据；终端快照权限维持 0600、目录 0700。
6. Windows PTY、交互式 kubeconfig exec 凭据登录和 CRD discovery 尚未实现；不要将其与已实现的 kubectl exec 容器终端混淆。

[使用与构建](README.md) · [集成说明](docs/host-integration.md) · [验证记录](VALIDATION.md)
