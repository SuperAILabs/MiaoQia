package infra

import (
	"context"
	"fmt"
	"time"

	fga "github.com/openfga/go-sdk"
	"github.com/openfga/go-sdk/client"

	"github.com/superailabs/miaoqia/services/kernel/authz"
)

// CheckBatch 在一次请求中求值多项授权。调用方应按返回切片下标对应输入顺序消费结果。
// OpenFGA SDK 会按服务端上限自动拆分批次并并发提交，适合权限矩阵等验收工具；业务决策仍使用 Check。
func (o *OpenFGA) CheckBatch(ctx context.Context, reqs []authz.Request) ([]bool, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	now := map[string]interface{}{"current_time": time.Now().UTC().Format(time.RFC3339Nano)}
	items := make([]client.ClientBatchCheckItem, len(reqs))
	for i, req := range reqs {
		items[i] = client.ClientBatchCheckItem{
			User:          req.Subject,
			Relation:      req.Relation,
			Object:        req.Object,
			CorrelationId: fmt.Sprintf("check-%d", i),
			Context:       &now,
		}
	}
	res, err := o.c.BatchCheck(ctx).Body(client.ClientBatchCheckRequest{Checks: items}).Options(client.BatchCheckOptions{
		MaxBatchSize:        fga.PtrInt32(50),
		MaxParallelRequests: fga.PtrInt32(8),
	}).Execute()
	if err != nil {
		return nil, err
	}
	out := make([]bool, len(reqs))
	for i := range reqs {
		result, ok := res.GetResult()[fmt.Sprintf("check-%d", i)]
		if !ok {
			return nil, fmt.Errorf("OpenFGA 批量检查缺少结果 %d", i)
		}
		if result.HasError() {
			checkErr := result.GetError()
			return nil, fmt.Errorf("OpenFGA 批量检查 %d：%s", i, checkErr.GetMessage())
		}
		out[i] = result.GetAllowed()
	}
	return out, nil
}
