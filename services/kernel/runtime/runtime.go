// Package runtime 是部署单元的进程角色组装：每个子系统提供一个 Module，进程角色按部署清单加载其中若干个
// （docs/03-architecture/07-deployment-topology.md 第 2、3 节）。子系统之间不共享数据库连接与事务。
package runtime

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/scheduler"
)

// Module 描述一个子系统在各进程角色中的职责。未使用的字段留空。
type Module struct {
	Name       string // 子系统名，与子系统索引一致，例如 customer-context
	Database   string // 逻辑数据库代码，例如 ctx；无数据库的子系统为空
	Migrations fs.FS  // goose 迁移

	// Start 在子系统所属的每个角色启动时调用一次，完成依赖装配。
	Start func(ctx context.Context, env *Env) error
	// Routes 在 api 角色中挂载对外接口（经 gateway）与内部接口（内部监听端口）。
	Routes func(env *Env, public, internal *http.ServeMux)
	// Consumers 在 consumer 角色中运行的事件消费者。
	Consumers func(env *Env) []*consumer.Consumer
	// Jobs 在 scheduler 角色中运行的周期任务（kernel/scheduler），在 Start 之后调用；任务只调用本子系统可重复执行的维护入口。
	Jobs func(env *Env) ([]scheduler.Job, error)
}

// Env 是子系统运行环境：本子系统的数据库与配置、事件总线地址、其他子系统接口的绑定。
type Env struct {
	DataScope string // 本实例 public / tenant / platform；独立于逻辑平面
	Cell      string // tenant 实例所属单元
	Role      string
	DB        *db.DB // 本子系统数据库（应用账号）；无数据库时为 nil
	Brokers   []string
	Log       *slog.Logger
	Config    func(key string) string // 本子系统配置（环境变量 MIAOQIA_<子系统>_<KEY>）
	Bindings  *Bindings
}

// Bindings 保存同语言子系统接口的绑定（本地实现或远程客户端），由组装代码按部署配置填充。
type Bindings struct {
	values map[string]any
}

// NewBindings 返回空的绑定表。
func NewBindings() *Bindings { return &Bindings{values: map[string]any{}} }

// Provide 登记接口绑定，name 形如 identity.Authorizer。
func (b *Bindings) Provide(name string, impl any) { b.values[name] = impl }

// Lookup 取接口绑定。
func Lookup[T any](b *Bindings, name string) (T, bool) {
	v, ok := b.values[name].(T)
	return v, ok
}
