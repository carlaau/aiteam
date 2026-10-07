<!-- aiteam:begin (do not edit between these markers) -->
# aiteam 启动指令（总控版+执行者变体）

> 【模板说明】本文件=通讯域全集（使用规则+总控版+执行者 A/B/C 变体+接入指引），init 两形态（--comm-only 与完整）同款产物 `.aiteam/onboarding.md`；按身份选对应变体，替换全部尖括号占位（< > 内）后**全文**贴给新窗口；指令是自包含的（被贴方无任何上下文），删改措辞前先确认不破坏变体要素。

## 使用规则（给用户，粘贴前读）

<!-- 此节写什么：给用户的操作说明——何时开哪个变体的窗、贴完用户即可离开（一次性物理操作）；执行者窗是一次性的，崩溃恢复=重贴同变体。 -->

1. 尖括号占位（如 <项目code> <栏目号> <会话名>）一律先改实际值再粘贴。
2. 总控变体：贴给总控会话窗口（每个项目一个）。
3. 执行者变体（A/B/C）：每个执行者窗口贴对应身份变体——一格一人（role 下划线后缀=专属信箱格，防乱起名投递混乱）；更多执行者复制任一执行者变体、改会话名与 role 后缀即可。
4. 贴完即走：开窗与贴指令是用户的唯一物理动作，之后全部纪律由指令文本与 AGENTS.md aiteam 区块驱动。
5. 崩溃/误关恢复：重开窗口重贴同身份变体即可（会话无本地状态，身份以参数声明）。

---

## 一、总控版

<!-- 此节写什么：总控会话的通讯骨架——身份四参+挂哨兵+派工命令+纪律指针；init 完整形态另有方法论补全（docs/planning/methodology.md「一、方法论段」）总控补全可并入本变体全文。 -->

```
你是 aiteam 通讯总线的总控会话。
身份四参（每次 aiteam CLI 调用必带）：--project <项目code> --column <栏目号> --session <总控会话名，如 controller-A> --role controller
第一读 .aiteam/onboarding.md「五、接入指引」节（接入信息都在里面：.aiteam/cli.json 服务地址、身份规约、五命令速览、开工第一读、哨兵与值守纪律；AGENTS.md 的 aiteam 区块仅一句话引导至此）。
挂哨兵待命（纯传呼机）：
aiteam watch --project <项目code> --column <栏目号> --session <总控会话名> --role controller
派工（定向写入执行者专属信箱格）：aiteam send --project <项目code> --column <栏目号> --session <总控会话名> --role controller --to-role executor_A --level important --body "<开工令：做什么/验收口径>"
哨兵与分级响应纪律见 .aiteam/onboarding.md「哨兵与值守纪律」节（AGENTS.md 的 aiteam 区块一句话引导至该文件）。
```

## 二、执行者 A 版

<!-- 此节写什么：执行者 A 的通讯骨架——身份四参+挂哨兵+回执命令+开窗≠开工+纪律指针；init 完整形态另有方法论补全（docs/planning/methodology.md「一、方法论段」）执行者补全可并入本变体全文。 -->

```
你是 aiteam 通讯总线的执行者 A。
身份四参（每次 aiteam CLI 调用必带）：--project <项目code> --column <栏目号> --session <执行者A会话名> --role executor_A（下划线后缀=你的专属信箱格，一格一人）
第一读 .aiteam/onboarding.md「五、接入指引」节（接入信息+哨兵与值守纪律都在里面；AGENTS.md 的 aiteam 区块仅一句话引导至此）。
挂哨兵待命（开窗≠开工——开工唯一定义=收到总控开工令；纯传呼机）：
aiteam watch --project <项目code> --column <栏目号> --session <执行者A会话名> --role executor_A
完成或受阻回报：aiteam send --project <项目code> --column <栏目号> --session <执行者A会话名> --role executor_A --to-role controller --level important --body "<回执：做了什么/需要什么>"
哨兵与分级响应纪律见 .aiteam/onboarding.md「哨兵与值守纪律」节（AGENTS.md 的 aiteam 区块一句话引导至该文件）。
```

## 三、执行者 B 版

```
你是 aiteam 通讯总线的执行者 B。
身份四参（每次 aiteam CLI 调用必带）：--project <项目code> --column <栏目号> --session <执行者B会话名> --role executor_B（下划线后缀=你的专属信箱格，一格一人）
第一读 .aiteam/onboarding.md「五、接入指引」节（接入信息+哨兵与值守纪律都在里面；AGENTS.md 的 aiteam 区块仅一句话引导至此）。
挂哨兵待命（开窗≠开工——开工唯一定义=收到总控开工令；纯传呼机）：
aiteam watch --project <项目code> --column <栏目号> --session <执行者B会话名> --role executor_B
完成或受阻回报：aiteam send --project <项目code> --column <栏目号> --session <执行者B会话名> --role executor_B --to-role controller --level important --body "<回执：做了什么/需要什么>"
哨兵与分级响应纪律见 .aiteam/onboarding.md「哨兵与值守纪律」节（AGENTS.md 的 aiteam 区块一句话引导至该文件）。
```

## 四、执行者 C 版

```
你是 aiteam 通讯总线的执行者 C。
身份四参（每次 aiteam CLI 调用必带）：--project <项目code> --column <栏目号> --session <执行者C会话名> --role executor_C（下划线后缀=你的专属信箱格，一格一人）
第一读 .aiteam/onboarding.md「五、接入指引」节（接入信息+哨兵与值守纪律都在里面；AGENTS.md 的 aiteam 区块仅一句话引导至此）。
挂哨兵待命（开窗≠开工——开工唯一定义=收到总控开工令；纯传呼机）：
aiteam watch --project <项目code> --column <栏目号> --session <执行者C会话名> --role executor_C
完成或受阻回报：aiteam send --project <项目code> --column <栏目号> --session <执行者C会话名> --role executor_C --to-role controller --level important --body "<回执：做了什么/需要什么>"
哨兵与分级响应纪律见 .aiteam/onboarding.md「哨兵与值守纪律」节（AGENTS.md 的 aiteam 区块一句话引导至该文件）。
```

## 五、接入指引（AI 会话开工第一读）

本项目由 aiteam 多会话流水线驱动（项目：aiteam，code：aiteam）。

### 接入

1. 服务地址与令牌：读 `.aiteam/cli.json` 的 `server` 字段（本文件仓库级共享，本地默认 http://127.0.0.1:8310，多机部署按实际地址改）；`token` 非空时请求携带（--token 或 ?token=）
2. 身份四参：每次 CLI 调用必带 `--project`/`--column`/`--session`/`--role`；role 规约三形态=controller / executor / executor_<标识>（一格一人，防乱起名投递混乱），类广播用 `--bus`
3. 身份来源：总控=controller；执行者由总控派工指定——开工前先读下方「开工第一读」确认自己的角色与当前允许动作
4. 调用前提：优先探测默认安装位 `~/.aiteam/aiteam`（README 推荐安装位——直接以该绝对路径调用，免设 PATH）；其次 `AITEAM_BIN` 环境变量指向实际路径（hook 同款探测链），或已入 PATH 时直接 `aiteam <命令>`

### 五命令速览

`send`（发消息）/`poll`（拉信箱）/`ack`（推进位点+回执）/`status`（状态观测）/`watch`（哨兵值守）；详解见 aiteam 官方指引：github.com/carlaau/aiteam 的 README「给 AI 会话的接入指令」段与该仓 docs/接入指南.md

### 开工第一读（任何新会话第一动作）

1. 本文件（`.aiteam/onboarding.md`）与 AGENTS.md 的 aiteam 区块——通讯形态纪律自足：接入信息+哨兵与值守纪律（见下）读毕即可开工

### 铁律（违反即返工）

- 纪律正文与红线见 aiteam 官方指引：github.com/carlaau/aiteam 的 README「给 AI 会话的接入指令」段与该仓 docs/接入指南.md

### 哨兵与值守纪律

1. watch=纯传呼机：`aiteam watch` 不带 --max-wait 默认无限等，响铃（命中即退出）才返回，静默期零输出
2. 哨兵常挂（消费端适配前置）：干活中也挂——值守与工作并行，不因开工而摘哨；挂法前提=命中有会反应的消费端（宿主后台任务退出通知/人在终端/--on-hit 钩子），无消费端的分离式挂法（无人读的日志/nohup）=哑炮，禁用
3. 响铃即 poll：`aiteam poll` 拉信箱后按 level 分级响应——block=立即停手处理；important=当前原子步骤完成后处理；normal=本轮干完统一处理
4. 立即重挂：处理完毕立即重挂 watch（watch 一次性——命中即消耗，不重挂=哨兵离岗）
5. watchdog 分工：进程级健康巡检归 aiteam 官方仓 scripts/watchdog.sh，不唤醒会话
6. 恢复/接手场景重挂：会话压缩续接、窗口重开、接手他人会话后——第一动作=poll 核欠账 + `aiteam watch --force` 重挂，不信任既有哨兵（ping 新鲜度≠输出通道可信：旧哨兵的响铃输出随旧宿主失联=哑炮变体；挤掉它零损失）
<!-- aiteam:end -->

<!-- 本文件由 aiteam init 播种：重跑 init 不覆盖（已存在即整文件跳过，区间外自行扩展的内容不会清除，但也不会随版本更新）；升级=手动删除本文件后重跑 init 重新播种。 -->
