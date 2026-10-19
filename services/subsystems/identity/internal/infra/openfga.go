// Package infra 是 identity 的外部适配：授权模型与关系存放在 OpenFGA。
package infra

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	fga "github.com/openfga/go-sdk"
	"github.com/openfga/go-sdk/client"
	"github.com/openfga/language/pkg/go/transformer"

	"github.com/superailabs/miaoqia/services/kernel/authz"
)

//go:embed model.fga
var modelDSL string

// StoreName 是平台使用的 OpenFGA store。
const StoreName = "miaoqia"

// OpenFGA 是基于 OpenFGA 的授权决策点。
type OpenFGA struct {
	c *client.OpenFgaClient
}

// Model 返回内置授权模型（model.fga 转换为 OpenFGA 的 JSON 结构）。
func Model() (fga.WriteAuthorizationModelRequest, error) {
	var m fga.WriteAuthorizationModelRequest
	js, err := transformer.TransformDSLToJSON(modelDSL)
	if err != nil {
		return m, fmt.Errorf("授权模型：%w", err)
	}
	err = json.Unmarshal([]byte(js), &m)
	return m, err
}

// Connect 连接 OpenFGA 的平台 store，确保最新的授权模型与内置模型一致（不一致时写入新模型版本）。
func Connect(ctx context.Context, url string) (*OpenFGA, error) {
	return ConnectStore(ctx, url, StoreName)
}

// ConnectStore 同 Connect，使用指定名称的 store（测试使用独立 store）。
func ConnectStore(ctx context.Context, url, storeName string) (*OpenFGA, error) {
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
		if s.Name == storeName {
			storeID = s.Id
		}
	}
	if storeID == "" {
		res, err := c.CreateStore(ctx).Body(client.ClientCreateStoreRequest{Name: storeName}).Execute()
		if err != nil {
			return nil, err
		}
		storeID = res.Id
	}
	if err := c.SetStoreId(storeID); err != nil {
		return nil, err
	}
	want, err := Model()
	if err != nil {
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
	o := &OpenFGA{c: c}
	if err := o.reconcileServiceGrants(ctx); err != nil {
		return nil, fmt.Errorf("服务授权：%w", err)
	}
	return o, nil
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
	// 带到期时间的关系（not_expired 条件）以决策时刻判断
	now := map[string]any{"current_time": time.Now().UTC().Format(time.RFC3339Nano)}
	res, err := o.c.Check(ctx).Body(client.ClientCheckRequest{User: req.Subject, Relation: req.Relation, Object: req.Object, Context: &now}).Execute()
	if err != nil {
		return false, err
	}
	return res.GetAllowed(), nil
}

// GrantAll 写入多条关系（初始化与测试使用）。
func (o *OpenFGA) GrantAll(ctx context.Context, tuples [][3]string) error {
	for i := 0; i < len(tuples); i += 100 { // OpenFGA 单次写入上限 100 条
		end := min(i+100, len(tuples))
		var body client.ClientWriteTuplesBody
		for _, t := range tuples[i:end] {
			body = append(body, client.ClientTupleKey{User: t[0], Relation: t[1], Object: t[2]})
		}
		if _, err := o.c.WriteTuples(ctx).Body(body).Options(client.ClientWriteOptions{Conflict: client.ClientWriteConflictOptions{
			OnDuplicateWrites: client.CLIENT_WRITE_REQUEST_ON_DUPLICATE_WRITES_IGNORE}}).Execute(); err != nil {
			return err
		}
	}
	return nil
}

// Grant 写入一条关系（管理与初始化用；授予本身是高影响动作，经 identity 的审批流程调用）。
func (o *OpenFGA) Grant(ctx context.Context, subject, relation, object string) error {
	_, err := o.c.WriteTuples(ctx).Body(client.ClientWriteTuplesBody{{User: subject, Relation: relation, Object: object}}).
		Options(client.ClientWriteOptions{Conflict: client.ClientWriteConflictOptions{OnDuplicateWrites: client.CLIENT_WRITE_REQUEST_ON_DUPLICATE_WRITES_IGNORE}}).Execute()
	return err
}

// GrantUntil 写入一条到期自动失效的关系（外部协作者等；关系须在模型中声明 with not_expired）。
func (o *OpenFGA) GrantUntil(ctx context.Context, subject, relation, object string, expiresAt time.Time) error {
	cond := map[string]any{"expires_at": expiresAt.UTC().Format(time.RFC3339Nano)}
	_, err := o.c.WriteTuples(ctx).Body(client.ClientWriteTuplesBody{{User: subject, Relation: relation, Object: object,
		Condition: &fga.RelationshipCondition{Name: "not_expired", Context: &cond}}}).Execute()
	return err
}

// Evaluate 以上下文关系（contextual tuples）求值一次授权：目录中的成员资格、角色、单元层级与资源所在单元由调用方按
// 当次决策给出，持久化关系只补充业务资源上的参与关系与平台上的服务授权。带到期时间的关系以决策时刻判断。
// 关系或资源类型不在模型中时返回 authz.ErrInvalid（请求无效，不重试）。
func (o *OpenFGA) Evaluate(ctx context.Context, subject, relation, object string, tuples [][3]string) (bool, error) {
	if len(tuples) > maxContextual {
		return false, fmt.Errorf("上下文关系 %d 条，超过上限 %d", len(tuples), maxContextual)
	}
	contextual := make([]client.ClientContextualTupleKey, 0, len(tuples))
	for _, t := range tuples {
		contextual = append(contextual, client.ClientContextualTupleKey{User: t[0], Relation: t[1], Object: t[2]})
	}
	now := map[string]any{"current_time": time.Now().UTC().Format(time.RFC3339Nano)}
	result, err := o.c.Check(ctx).Body(client.ClientCheckRequest{User: subject, Relation: relation, Object: object, ContextualTuples: contextual, Context: &now}).Execute()
	if err != nil {
		var invalid fga.FgaApiValidationError
		if errors.As(err, &invalid) {
			return false, authz.ErrInvalid
		}
		return false, err
	}
	return result.GetAllowed(), nil
}

// maxContextual 是 OpenFGA 单次 Check 的上下文关系上限（服务端默认 100）。
const maxContextual = 100

// MaxWrite 是 OpenFGA 单次写入（一个事务）的关系上限（服务端默认 100）。
const MaxWrite = 100

// ReadObject 读出资源上的全部持久化关系（用户、关系、资源）。
func (o *OpenFGA) ReadObject(ctx context.Context, object string) ([][3]string, error) {
	return o.read(ctx, client.ClientReadRequest{Object: &object})
}

// ReadUser 读出主体在某类资源上的全部持久化关系；objectType 例如 objective。
func (o *OpenFGA) ReadUser(ctx context.Context, user, objectType string) ([][3]string, error) {
	object := objectType + ":"
	return o.read(ctx, client.ClientReadRequest{User: &user, Object: &object})
}

func (o *OpenFGA) read(ctx context.Context, body client.ClientReadRequest) ([][3]string, error) {
	var out [][3]string
	var token *string
	size := int32(100)
	for {
		res, err := o.c.Read(ctx).Body(body).Options(client.ClientReadOptions{PageSize: &size, ContinuationToken: token}).Execute()
		if err != nil {
			return nil, err
		}
		for _, t := range res.GetTuples() {
			k := t.GetKey()
			out = append(out, [3]string{k.GetUser(), k.GetRelation(), k.GetObject()})
		}
		next := res.GetContinuationToken()
		if next == "" {
			return out, nil
		}
		token = &next
	}
}

// Write 在一次 OpenFGA 写入（一个事务）中加入与删除关系；重复加入与删除不存在的关系被忽略，重试因此安全。
func (o *OpenFGA) Write(ctx context.Context, adds, deletes [][3]string) error {
	if len(adds) == 0 && len(deletes) == 0 {
		return nil
	}
	if len(adds)+len(deletes) > MaxWrite {
		return fmt.Errorf("一次写入 %d 条关系，超过上限 %d", len(adds)+len(deletes), MaxWrite)
	}
	body := client.ClientWriteRequest{}
	for _, t := range adds {
		body.Writes = append(body.Writes, client.ClientTupleKey{User: t[0], Relation: t[1], Object: t[2]})
	}
	for _, t := range deletes {
		body.Deletes = append(body.Deletes, client.ClientTupleKeyWithoutCondition{User: t[0], Relation: t[1], Object: t[2]})
	}
	_, err := o.c.Write(ctx).Body(body).Options(client.ClientWriteOptions{Conflict: client.ClientWriteConflictOptions{
		OnDuplicateWrites: client.CLIENT_WRITE_REQUEST_ON_DUPLICATE_WRITES_IGNORE,
		OnMissingDeletes:  client.CLIENT_WRITE_REQUEST_ON_MISSING_DELETES_IGNORE}}).Execute()
	return err
}

func clientRead(relation, object *string) client.ClientReadRequest {
	return client.ClientReadRequest{Relation: relation, Object: object}
}
