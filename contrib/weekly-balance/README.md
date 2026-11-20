# Magpie 周额度均衡

本地策略适用于同一 provider 下的多个订阅账号，也适用于每台远端只提供一个对应订阅账号的 Remote Magpie 模型组。它按各账号自身的 7 天周期比较实际消耗和目标进度，默认目标为 75%。周进度差按原始精度比较，分数相同则沿用配置顺序。有线路的周额度缺失、过期、跨过重置时间或无法区分窗口时，暂按近期用量排序。5 小时额度用于各层 Magpie 的限额保护。路由图显示选路时记录的 7 天额度。

周均衡和其他路由策略共用 Magpie 的按轮粘性时长。默认 60 分钟，在「设置 → 模型 → 路由」修改，或运行 `magpie settings set turnAffinityMinutes 30`。线路设为 `stays=turn` 时，从上次应答起空闲达到该时长，续跑同一轮也会重新选路。

## 分支

- `feature/routing-quota-windows` 包含可提交上游的只读 `seat.quotaWindows`、文档和测试。
- `feature/weekly-balance-plugin` 包含上述接口和可运行的均衡插件及测试。
- `weekly-balance` 增加独立 JS 策略、本地更新器和 Fork 发布工作流。
- [上游需求 #7207](https://github.com/yetone/magpie/issues/7207) 记录了两个应用场景。

## 接管现有安装

需要 ARM64 macOS、Python 3 和已登录的 GitHub CLI。默认网关端口为 `3425`。安装器验证发布包及插件的构建证明，等到网关空闲后，原位替换 `/Applications/magpie.app`。新 App 直接读取 `~/.config/magpie`，沿用已有设置、账号和端口。原版 App 的备份保存在 `~/.config/magpie/weekly-balance/official/`。

```sh
python3 update.py install --in-place --root /Applications
python3 update.py launch-agent --in-place --root /Applications
```

网关端口不同的安装在两条命令中都传 `--addr 127.0.0.1:端口`。在目标模型组中选择 `weekly-balance/weekly-balance` 后即可使用。手动检查和回滚使用相同的 `--in-place`、`--root` 和 `--addr` 参数。本机更新日志位于 `~/.config/magpie/weekly-balance/updater.log`。

## 隔离安装

需要 ARM64 macOS、Python 3 和已登录的 GitHub CLI。安装器验证应用压缩包及插件的 GitHub 构建证明，再装入单独的便携目录并设置独立的网关端口。保留现有官方 App，可继续用原来的网关。先用测试账号或模拟线路验证，再接入真实业务。远端订阅的账号上限应在远端核实。

```sh
cd contrib/weekly-balance
root="$HOME/Applications/Magpie Weekly Balance"
python3 update.py install --root "$root"
open -a "$root/magpie.app" --args tray
magpie="$root/magpie.app/Contents/MacOS/magpie"
"$magpie" status --addr 127.0.0.1:13425
```

在便携实例中配置候选线路和模型组，再设置策略与轮次保持。沿用选路前保存的账号和组设置。

```sh
"$magpie" group set <group-id> routing=weekly-balance/weekly-balance stays=turn
"$magpie" plugin options weekly-balance '{"weeklyTarget":75,"maxSampleAgeMinutes":30}'
```

在 Routing 中观察新轮次的尝试顺序。在用量页核对两条线路的采样时间和 7 天重置时间。在 Plugins 中确认策略有调用记录。需要恢复组的原路由时，通过 `magpie group set` 设回原来的 `routing` 和 `stays`。

## 自动更新与回滚

发布工作流每 6 小时读取官方 Daily 版本。从上游 tag 应用分支补丁，运行 Go 网关验证和 JS 模拟，构建 ARM64 App，再为压缩包和插件生成构建证明。补丁冲突或测试失败会中止发布。修订同一上游版本的策略时，提高工作流中的 `FORK_REVISION`。GitHub 的定时工作流从 Fork 的默认分支运行，默认分支应是 `weekly-balance`。

本地安装器关闭所接管实例的官方更新。本地更新器校验自建包的构建证明，把已验证的包交给 Magpie 自带的本机安装及接续机制，并检查重启后的网关版本和插件。健康检查失败时尝试恢复上一版应用和插件。回滚后会跳过刚撤回的版本，等下一个版本发布。

```sh
python3 update.py update --root "$root"
python3 update.py launch-agent --root "$root"
python3 update.py rollback --root "$root"
```

`launch-agent` 每 6 小时运行一次，日志在便携目录的 `updater.log`。停用更新可运行以下命令。

```sh
label=io.github.zhaozhenghang.magpie.weekly-balance.update
launchctl bootout "gui/$(id -u)/$label"
```

构建脚本也可在本机运行，命令从 Fork 分支取提交，在临时 worktree 构建并把产物写到指定目录。

```sh
bash contrib/weekly-balance/build-release.sh 0.1.4533 1 /tmp/weekly-balance-release
node --test contrib/weekly-balance/weekly-balance.test.mjs
```
