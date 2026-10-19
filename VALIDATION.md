# 验证记录 · 2026-10-19

当前插件 `com.example.kubecontext@0.5.1`，macOS arm64，已更新到用户原版 DBX v0.6.48。只修改插件。

0.5.1 修复主按钮将文字写死为白色的问题：文字与复选框勾选改为 `--color-primary-foreground`，悬停及按下保持主色背景。26 项 UI 回归重新通过，打包通过且 UI 源码与包内文件一致。暗色导入/容器弹窗在官方 DBX 中验证；未重新执行与本次 CSS 修复无关的后端和集成测试，下表 Go/黑盒结果为 0.5.0 基线。

| 检查 | 结果 |
|---|---|
| 插件 UI 回归 | 26 项通过，覆盖原有行为以及 tooltip 安全文本、资源用量匹配、监控部分可用/速率/迟到响应、容器选择及多个独立终端标签 |
| Go 单元测试 `-race` | 11 项通过；新增数量单位、init/sidecar/overhead 预算、Metrics/Summary 读取与 403 降级、同名替换 Pod UID 隔离、容器校验及 argv/Shell 边界 |
| Go vet | 通过 |
| JSON-RPC 黑盒验收 | 9 组通过，新增 PodMetricsList 和 CPU/内存/磁盘/网络读取，保留资源写拒绝与 PTY/context/日志/导入回归 |
| 插件打包 | 通过；3 个 UI 源文件与包内字节一致，manifest 仅由 CLI 正常归一化平台可执行文件路径 |
| DBX 源码 | `git status --short` 为空 |
| 用户原 DBX | 未替换；`codesign --verify --deep --strict` 通过，Developer ID `TVDM965TDL`，notarization ticket 保留 |

## 原版 DBX 实测

在插件中心确认安装版本 0.5.0，恢复用户的 `ht-wego / wego`。真实 Pod 列表显示 CPU/内存实际用量和 Request/Limit，结束的 Pod 无用量时显示 `—`，进入按钮禁用；右侧操作列固定，宽表支持横向滚动。

`wego-extra-0` 详情「监控」每 5 秒自动刷新，观察到采样时间、CPU/内存值及曲线连续变化。磁盘/网络实际返回 `403 nodes/proxy`，卡片显示不可用和原错误，没有伪造 0，没有修改 RBAC。磁盘/网络完整数据通路和网络变化率/计数重置由假服务器及 UI 回归验证，未取得真实集群数据。

「进入」选择 `durable / sh`，容器终端执行 `printf 'DBX_CONTAINER_OK\n'; pwd` 成功，输出标记和根目录。新增本地标签执行 `kubectl config current-context` 返回 `ht-wego`，切回容器标签仍保留原输出。终端标签的插件 tooltip 已在原版 WebKit 中实际显示；列表完整文本及防 HTML 注入由 UI 回归覆盖。

资源变更拒绝只在临时配置/假服务器测试；真实集群仅读取资源和用户要求的交互容器 Shell，无部署修改。

[监控与两个终端标签](artifacts/validation/desktop-monitor-tabs-0.5.0.png) · [容器登录与实时曲线](artifacts/validation/desktop-container-0.5.0.png)

## 产物与日志

- `dist/com.example.kubecontext-0.5.0-darwin-arm64.dbxp`
- SHA-256：`07c25c494b4800c65f2c64f504bdd88fc8302ea0a7d5a7dfca6bae0b1f7b5029`
- `artifacts/validation/ui-tests-0.5.0.log`
- `artifacts/validation/go-tests-0.5.0.log`
- `artifacts/validation/integration-0.5.0.log`

未执行 Linux 实机、Windows PTY 或交互式 kubeconfig 凭据插件登录；Windows ConPTY 尚未实现。

## 0.5.1 配色修复产物

- `dist/com.example.kubecontext-0.5.1-darwin-arm64.dbxp`
- SHA-256：`d6f30408a44189a621b933caabc91cdfa837929f08bdeb77b901c497892e37b9`
- UI 回归：`artifacts/validation/ui-tests-0.5.1.log`，26 项通过。
- [暗色导入按钮](artifacts/validation/desktop-dark-import-0.5.1.png) · [暗色容器弹窗](artifacts/validation/desktop-dark-exec-0.5.1.png)
