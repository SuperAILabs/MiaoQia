// Package infra 是 identity 的外部适配：授权模型与关系存放在 OpenFGA。
package infra

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"

	fga "github.com/openfga/go-sdk"
	"github.com/openfga/go-sdk/client"

	"github.com/superailabs/miaoqia/services/kernel/authz"
)

//go:embed model.json
var modelJSON []byte

// StoreName 是平台使用的 OpenFGA store。
const StoreName = "miaoqia"

// OpenFGA 是基于 OpenFGA 的授权决策点。
type OpenFGA struct {
	c *client.OpenFgaClient
}

// Connect 连接 OpenFGA，确保 store 存在且最新的授权模型与内置模型一致（不一致时写入新模型版本）。
func Connect(ctx context.Context, url string) (*OpenFGA, error) {
	c, err := client.NewSdkClient(&client.ClientConfiguration{ApiUrl: url})
	if err != nil {
		return nil, err
	}
	stores, err := c.ListStores(ctx).Execute()
	if err != nil {
		return nil, fmt.Errorf("OpenFGA：%w", err)
	}
	var storeID string
	for _, s := range stores.Stores {
		if s.Name == StoreName {
			storeID = s.Id
		}
	}
	if storeID == "" {
		res, err := c.CreateStore(ctx).Body(client.ClientCreateStoreRequest{Name: StoreName}).Execute()
		if err != nil {
			return nil, err
		}
		storeID = res.Id
	}
	if err := c.SetStoreId(storeID); err != nil {
		return nil, err
	}
	var want fga.WriteAuthorizationModelRequest
	if err := json.Unmarshal(modelJSON, &want); err != nil {
		return nil, err
	}
	models, err := c.ReadAuthorizationModels(ctx).Execute()
	if err != nil {
		return nil, err
	}
	modelID := ""
	if len(models.AuthorizationModels) > 0 && sameModel(models.AuthorizationModels[0].TypeDefinitions, want.TypeDefinitions) {
		modelID = models.AuthorizationModels[0].Id
	} else {
		res, err := c.WriteAuthorizationModel(ctx).Body(want).Execute()
		if err != nil {
			return nil, err
		}
		modelID = res.AuthorizationModelId
	}
	if err := c.SetAuthorizationModelId(modelID); err != nil {
		return nil, err
	}
	return &OpenFGA{c: c}, nil
}

func sameModel(a, b []fga.TypeDefinition) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	var x, y any
	_ = json.Unmarshal(ja, &x)
	_ = json.Unmarshal(jb, &y)
	return reflect.DeepEqual(x, y)
}

// Check 是授权决策。
func (o *OpenFGA) Check(ctx context.Context, req authz.Request) (bool, error) {
	res, err := o.c.Check(ctx).Body(client.ClientCheckRequest{User: req.Subject, Relation: req.Relation, Object: req.Object}).Execute()
	if err != nil {
		return false, err
	}
	return res.GetAllowed(), nil
}

// Grant 写入一条关系（管理与初始化用；授予本身是高影响动作，经 identity 的审批流程调用）。
func (o *OpenFGA) Grant(ctx context.Context, subject, relation, object string) error {
	_, err := o.c.WriteTuples(ctx).Body(client.ClientWriteTuplesBody{{User: subject, Relation: relation, Object: object}}).
		Options(client.ClientWriteOptions{Conflict: client.ClientWriteConflictOptions{OnDuplicateWrites: client.CLIENT_WRITE_REQUEST_ON_DUPLICATE_WRITES_IGNORE}}).Execute()
	return err
}
