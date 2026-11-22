# A02 固定中标公告研究

`research.a02-award@1` 接收正式中标公告的 `ntc_` ID 和正整数版本。它回答公告披露了哪些中标单位以及如何定位原文。固定版本可反查同一字段。报告对企业登记主体、总包身份、供货窗口和公告之外的覆盖范围标记未知。

| 步骤 | 只读操作 | 接受条件 |
| --- | --- | --- |
| 1 | `dataread.ReadFact` | 必须是请求的公告 ID、版本及正式中标类型。`current` 是读取视图，显式版本固定选择且由 owner 执行当前授权。 |
| 2 | `dataread.ReadBidResultUnits` | 复用 owner 的 awarded 视图。按同一公告版本和结果 ID 回查 `lots[].bid_results[].bidder`，核对公告中的名称。 |
| 3 | `dataread.ReadEvidence` | 只解析该字段的证据 ID。固定快照必须一致，定位存在，获准展示的引文须与公告字段一致。仅允许显示引用或无法核实时，输出未知项。 |
| 4 | 可选 `ai-platform.Gateway.Generate` | 独立批准模型使用后，将获准的已核实名单作为不可信输入。模型输出仅能从核对主体、核对合同角色、核对供货窗口三种建议中选择。 |

报告的每条中标事实包含字段 JSON Pointer、证据 ID、快照 ID、来源 ID、站点对象稳定键、观察时间、定位和获准引文。原件字节地址及哈希由所有者维护。报告公开的 `source_key` 只用于识别站点对象。任何 owner 明确拒绝读取时，入口以 `ErrA02AccessDenied` 结束。超时、预算用尽、解析失败或证据不足时，保留已核验条目并标明缺口。

## 调用

Cell 的 `platform -role api` 可配置 `MIAOQIA_RESEARCH_DATA_API_URL` 调用 data-api。网关认证后的 `POST /v1/research/a02` 使用请求指定的租户内组织单元及固定公告版本，读取上限为 16，截止时间为 20 秒，模型调用预算为零。data-api 对平台 API 证书配置 `interactive` 用户的 `data.get_fact`、`data.bid_result_units` 和 `data.resolve_evidence` 委托。固定输入和响应见 [OpenAPI](../../../contracts/apis/research/openapi.yaml)。

组装方用配置好的 `*dataread.Client` 与 identity 授权端口调用 `research.NewA02`，将得到的 `research/api.A02Runner` 注入 Cell 请求处理器。下面的公告是合成夹具。`ctx` 由认证入口生成，携带现有 interactive 用户、租户和 Cell。`OrgUnit` 从受信业务目标或已授权的研究范围解析所属组织单元。入口按 [identity 权限矩阵](../../../docs/03-architecture/subsystems/identity.md#72-权限矩阵) 检查该单元的 `can_start_research`。数据所有者仍逐次复核会话及来源权利。

```go
runner, err := research.NewA02(ownerReader, research.A02Admission{
    Authz: identityAuthorizer,
    OrgUnit: resolveAuthorizedOrgUnit,
}, research.A02Model{})
if err != nil {
    return err
}
report, err := runner.RunA02(ctx, researchapi.A02Request{
    Notice: researchapi.NoticeVersion{ID: "ntc_01JA8ZXXXXXXXXXXXXXXXXXXX1", Version: 3},
    Budget: researchapi.A02Budget{Reads: 3, Deadline: time.Now().Add(5 * time.Second)},
})
```

调用方按租户额度设置预算，模板上限为 16 次 owner 逻辑读取、1 次模型网关逻辑调用、30 秒绝对期限，并尊重调用方更早的期限和取消。单条中标证据需 3 次读取。绑定层的重试和网关内部的主备尝试计入同一次逻辑调用，并受期限约束。模型网关另有输入、输出和费用上限。

在仓库根目录可执行合成样本和回归。

```sh
cd services
go test -count=1 -v ./subsystems/research/...
```

样本覆盖通过生成的 data-api 客户端、请求上下文 HTTP 传输及合成 owner 固定读取第 3 版，定位到证据第 2 块。负向用例包括研究启动权限、租户、Cell、用户、用途、撤权、引用或版本不符、预算停止、只准展示，以及模型越出约定输出形状。HTTP 夹具在本机环回运行，身份和权利由合成 owner 判定。

## 后台与真实模型接线

- 按 [ADR-0010](../../../docs/04-decisions/0010-governed-agent-runtime.md) 将同一入口放入 Cell 内的 Temporal 活动。待后台委托获批后，由活动恢复并重新认证发起者，按业务授权取得当前用户、租户、Cell、研究所属组织单元和获准用途。新用途获批后，依登记的协议扩展入口。仅持久化固定输入、发起者绑定与预算状态，活动应从剩余额度和原始期限继续，并持久化已完成的模型调用账本。报告打开和复用时重新核验发起者或当前读取者的权限及全部引用。当前入口为同步、无持久化、无报告复用。各 owner 响应有独立授权确认，多个读取没有共同的授权事务。
- 当前 [来源权利协议](../../../docs/03-architecture/subsystems/source-rights-runtime.md) 的会话读取仅准入现有 interactive 用户，服务主体须按已登记的 monitoring/simulation 委托。后台调用者和模型使用需各自审批用途、委托和来源权利。显式装配 `A02Model.Authorize`，在同一认证上下文中对实际固定事实、证据、完整可信来源闭包和批准的模型路由执行独立裁决；`Gateway` 必须以对应路由约束主备。真实模型可用性、真实 API 账号、评测、输出、费用和降级以 [C3/M1-P5](../../../docs/05-delivery/06-stage-baseline.md#3-m1-前置条件与能力交接) 的验收为准。
- 当前输入已经固定，公共搜索和实体所有者属于后续受权发现与主体核实。实际部署须验证对应 Cell 角色的 mTLS、现行 identity 与来源权利、真实数据的固定证据定位。使用已获准的样本及环境，按来源记录覆盖和访问缺口。合成测试只覆盖上述接口行为。
