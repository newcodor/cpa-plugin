# CPA 插件仓库

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 插件集合。当前提供 **WorkBuddy / CodeBuddy** 与 **QoderWork (CN)** 两个 OAuth Provider。

## 插件

| ID | 说明 | 源码 |
|---|---|---|
| `workbuddy` | Tencent CodeBuddy OAuth、动态模型、executor、CN 每日签到、Global 专家包、积分面板、可选积分调度 | [workbuddy/](workbuddy/) |
| `qoderwork` | QoderWork CN（qoder.com.cn）：OAuth 设备授权 + PAT 双登录（可共存）、COSY 签名推理、动态模型、每日签到、积分面板、token 保活 | [qoderwork/](qoderwork/) |

## 多架构 Release

每个插件独立版本发 Release（tag `<id>-v*`），产物为 CPA 插件商店标准格式：

```text
<id>_<version>_linux_amd64.zip      # zip 根目录: <id>.so
<id>_<version>_linux_arm64.zip
<id>_<version>_darwin_amd64.zip     # <id>.dylib
<id>_<version>_darwin_arm64.zip
<id>_<version>_windows_amd64.zip    # <id>.dll
<id>_<version>_windows_arm64.zip
<id>_<version>_freebsd_amd64.zip
checksums.txt
```

命名规则与官方一致：`ArchiveName(id, version, goos, goarch) = {id}_{version}_{goos}_{goarch}.zip`
（见 CLIProxyAPI `internal/pluginstore`）。

CI：push / PR 全量构建（只出 artifacts）；tag `<id>-v*`（如 `qoderwork-v0.2.6`）或 dispatch 触发**该插件独立版本**的 Release。

## 安装（linux/amd64 示例）

```bash
# 从 Release 下载
unzip qoderwork_0.2.6_linux_amd64.zip
# 扁平 plugins 目录（常见 docker 挂载）
cp qoderwork.so /path/to/cliproxyapi/plugins/qoderwork.so
# 或平台子目录布局
# mkdir -p plugins/linux/amd64 && cp qoderwork.so plugins/linux/amd64/
```

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy:
      enabled: true
    qoderwork:
      enabled: true
```

## 本地构建（WSL / Linux）

插件是 Go `c-shared` 库，必须开 CGO 并用目标平台的 C 编译器交叉编译，
所以在 Windows 上最省事的做法是**进 WSL 构建**。下面以 `workbuddy` 为例，
换成 `qoderwork` 只需替换目录名和产物名。

```bash
# 进入 WSL（发行版名用 wsl -l 查看）
wsl -d Ubuntu
```

```bash
# 1) Go 环境：WSL 里的 go 装在 /usr/local/go/bin，默认不在 PATH 上
export PATH=/usr/local/go/bin:$PATH
export HOME=${HOME:-/root}
go version          # 需要 go1.26+，与 CPA 主程序一致

# 2) c-shared 必备：CGO + 目标平台的 C 编译器
export CGO_ENABLED=1
export GOOS=linux GOARCH=amd64 CC=gcc

# 3) 模块代理（国内推荐；离线环境改成 GOPROXY=off）
export GOPROXY=https://goproxy.cn,direct

# 4) 缓存目录：WSL 的 /tmp 常被清空，先建好，否则报
#    "go: creating work dir: stat /tmp/gotmp: no such file or directory"
export GOCACHE=/tmp/gocache GOTMPDIR=/tmp/gotmp
mkdir -p /tmp/gocache /tmp/gotmp

# 5) 编译。VERSION 文件是唯一的版本来源
cd /mnt/d/git/cpa-plugin/workbuddy
VER=$(cat VERSION)
go build -trimpath -buildmode=c-shared \
  -ldflags "-s -w -X main.version=$VER" \
  -o workbuddy_$VER.so .

# 6) 验证四个 C ABI 入口确实导出
nm -D --defined-only workbuddy_$VER.so | grep cliproxy
```

第 6 步应输出 8 行（4 个业务符号 + 4 个 cgo 跳板）：

```text
cliproxy_plugin_init
cliproxyPluginCall
cliproxyPluginFree
cliproxyPluginShutdown
_cgoexp_<hash>_cliproxy_plugin_init
_cgoexp_<hash>_cliproxyPluginCall
_cgoexp_<hash>_cliproxyPluginFree
_cgoexp_<hash>_cliproxyPluginShutdown
```

### 坑

- **会附带生成一个同名 `.h`** —— cgo 产物，运行时用不到，直接删：
  `rm -f workbuddy_$VER.h`。仓库 `.gitignore` 已忽略 `*.so` 与 `*.h`，
  两者都不会进版本库。
- **不要加 `-race`** —— c-shared 模式不支持。
- **`os.Executable()` 在插件里返回宿主 CPA 可执行文件的路径**，不是 `.so`
  自身路径，别用它定位插件自己的配置文件。
- **Windows 原生构建过不去**，两个原因：Go for Windows 默认不带 C 编译器；
  且老版本 Go（如 1.20）无法解析 `go 1.26.0` 的 go.mod，直接报
  `go.mod requires go >= 1.26.0`。所以走 WSL。
- **只在 Windows 上做 `go vet` / `go test` 语法校验**（不产出 `.so`）可以用
  Windows 的 go1.26.6，但要绕开默认缓存目录的权限问题：

  ```powershell
  $env:GOPROXY='off'
  $env:GOCACHE='D:\git\cpa-plugin\.gocache'
  $env:GOTMPDIR='D:\git\cpa-plugin\.gotmp'
  go vet ./...
  ```

- **stdout 可能被吞**：从 Windows 调 `wsl -e bash` 时输出经常拿不回来，
  把构建命令写进 `.sh` 文件、再重定向到文件读结果更稳：

  ```powershell
  cmd.exe /c 'wsl.exe -e bash /mnt/d/git/cpa-plugin/build.sh > build.log 2>&1'
  ```

## 远程更新（插件商店自定义源）

CPA 插件商店源添加：

```text
https://raw.githubusercontent.com/Sliverkiss/cpa-plugin/main/registry.json
```

然后在商店 UI 安装/更新 **workbuddy** 和 **qoderwork**。
