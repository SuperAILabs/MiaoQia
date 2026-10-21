// Package learning 组装 learning 反馈学习与评估 子系统。M2 第7段提供策略草案的候选变更仿真
// （docs/01-product/prd/07-policy-simulation-approval.md F3）：数据集与信息领域状态适配器尚未接入（api.NotWired，仿真运行失败），
// 测试与演示使用标明为夹具的内存实现（api/datasettest）。第3段提供反馈集与只读的反馈汇总（F7）；漏报、评估与提案随所属段落实现。
package learning

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.yaml.in/yaml/v3"

	lrn "github.com/superailabs/miaoqia/services/contracts/apis/learning"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/policy"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api"
	"github.com/superailabs/miaoqia/services/subsystems/learning/internal/app"
	"github.com/superailabs/miaoqia/services/subsystems/learning/migrations"
)

// 对外接口（经 gateway）的路由。
var publicRoutes = []string{"POST /v1/learning/simulations", "GET /v1/learning/simulations/{simulationID}",
	"GET /v1/learning/objectives/{objectiveID}/feedback-summary"}

// Adapters 是仿真端口的适配器与业务时钟。零值字段使用缺省：Dataset 与 Domains 为未接入（api.NotWired，仿真运行失败），
// Now 为系统时钟。
type Adapters struct {
	Dataset api.Dataset
	Domains api.DomainStatus
	Now     func() time.Time
}

// Module 返回 learning 的模块定义（仿真适配器未接入）。
func Module() runtime.Module { return ModuleWith(Adapters{}) }

// ModuleWith 返回使用给定适配器的 learning 模块定义。配置：MIAOQIA_LEARNING_PACKS_DIR（扩展包目录，默认 packs）。
func ModuleWith(adapters Adapters) runtime.Module {
	svc := &app.Service{Dataset: adapters.Dataset, Domains: adapters.Domains, Now: adapters.Now}
	if svc.Dataset == nil {
		svc.Dataset = api.NotWired{}
	}
	if svc.Domains == nil {
		svc.Domains = api.NotWired{}
	}
	return runtime.Module{
		Name:       api.Name,
		Database:   "lrn",
		Migrations: migrations.FS,
		Start: func(ctx context.Context, env *runtime.Env) error {
			dir := env.Config("PACKS_DIR")
			if dir == "" {
				dir = "packs"
			}
			features, err := loadFeatures(dir)
			if err != nil {
				return err
			}
			hints, err := loadHints(dir)
			if err != nil {
				return err
			}
			svc.Hints = hints
			access, ok := runtime.Lookup[customercontext.ObjectiveAccess](env.Bindings, customercontext.AccessBinding)
			if !ok {
				return fmt.Errorf("learning：缺少 %s 绑定", customercontext.AccessBinding)
			}
			reader, ok := runtime.Lookup[customercontext.ContextReader](env.Bindings, customercontext.ContextBinding)
			if !ok {
				return fmt.Errorf("learning：缺少 %s 绑定", customercontext.ContextBinding)
			}
			policies, ok := runtime.Lookup[customercontext.PolicyReader](env.Bindings, customercontext.PolicyBinding)
			if !ok {
				return fmt.Errorf("learning：缺少 %s 绑定", customercontext.PolicyBinding)
			}
			svc.DB, svc.Access, svc.Context, svc.Policies, svc.Features, svc.Log = env.DB, access, reader, policies, features, env.Log
			return nil
		},
		Routes: func(env *runtime.Env, public, internal *http.ServeMux) {
			pub, err := lrn.NewServer(&app.Server{Service: svc}, lrn.WithErrorHandler(app.WriteProtocolError))
			if err != nil {
				panic(err)
			}
			for _, pattern := range publicRoutes {
				public.Handle(pattern, pub)
			}
		},
		// 草案保存后自动仿真：消费 customer_context.policy.drafted.v1（只仿真通过校验的修订）。
		// 反馈集：消费 detection.signal.detected.v1 与 detection.signal.triaged.v1（PRD 第3段 F7，以操作标识幂等）。
		Consumers: func(env *runtime.Env) []*consumer.Consumer {
			return []*consumer.Consumer{{
				Name: "learning.policy-simulations", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
				Topics:   []string{outbox.Topic(reqctx.ZoneTenant, env.Cell, "customer_context")},
				Handlers: map[string]consumer.Handler{"customer_context.policy.drafted.v1": svc.PolicyDrafted},
			}, {
				Name: "learning.signal-feedback", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
				Topics: []string{outbox.Topic(reqctx.ZoneTenant, env.Cell, "detection")},
				Handlers: map[string]consumer.Handler{"detection.signal.detected.v1": svc.SignalDetected,
					"detection.signal.triaged.v1": svc.SignalTriaged},
			}}
		},
	}
}

// loadHints 读取全部应用领域包的反馈原因标签可能相关的规则与参数（feedback/*.yaml 的 relates），键为“目标编号/反馈结果/原因标签”。
func loadHints(dir string) (map[string]*app.ReasonHint, error) {
	out := map[string]*app.ReasonHint{}
	files, err := filepath.Glob(filepath.Join(dir, "application_domains", "*", "feedback", "*.yaml"))
	if err != nil {
		return nil, err
	}
	type reason struct {
		Code    string `yaml:"code"`
		Relates struct {
			Rules  []string `yaml:"rules"`
			Params []string `yaml:"params"`
		} `yaml:"relates"`
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var doc struct {
			Feedback struct {
				Objective      string   `yaml:"objective"`
				Irrelevant     []reason `yaml:"irrelevant"`
				ExclusionWrong []reason `yaml:"exclusion_wrong"`
			} `yaml:"feedback"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s：%w", f, err)
		}
		for result, list := range map[string][]reason{"irrelevant": doc.Feedback.Irrelevant, "exclusion_wrong": doc.Feedback.ExclusionWrong} {
			for _, r := range list {
				h := &app.ReasonHint{Rules: r.Relates.Rules, Params: r.Relates.Params}
				if h.Rules == nil {
					h.Rules = []string{}
				}
				if h.Params == nil {
					h.Params = []string{}
				}
				out[doc.Feedback.Objective+"/"+result+"/"+r.Code] = h
			}
		}
	}
	return out, nil
}

// loadFeatures 读取 dir 下全部应用领域包的共享特征（策略合成文档引用的特征定义）。
func loadFeatures(dir string) (map[string]policy.Feature, error) {
	out := map[string]policy.Feature{}
	apps, err := filepath.Glob(filepath.Join(dir, "application_domains", "*", "pack.yaml"))
	if err != nil {
		return nil, err
	}
	for _, manifest := range apps {
		pack, err := policy.LoadPack(filepath.Dir(manifest))
		if err != nil {
			return nil, fmt.Errorf("%s：%w", manifest, err)
		}
		for name, f := range pack.Features {
			if _, dup := out[name]; dup {
				return nil, fmt.Errorf("%s：特征 %s 重复", manifest, name)
			}
			out[name] = f
		}
	}
	return out, nil
}
