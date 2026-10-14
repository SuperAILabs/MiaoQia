// Package policy 实现策略 DSL 的编译与求值（规范见 docs/03-architecture/11-policy-dsl.md）。
//
// CEL 环境（变量、平台函数、规模上界与限制）以 contracts/policy/cel-environment.yaml 为准，
// 测试校验本包与之一致。customer-context 用 Compile 校验与编译策略，detection 与 learning
// 用 Evaluate 求值，三者共用同一份代码。
//
// 求值是输入快照上的纯函数：相同的 Input 产生相同的 Result。
package policy
