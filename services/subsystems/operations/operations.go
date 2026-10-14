// Package operations 组装 operations 子系统（人工复核队列、数据运营、托管服务、客户成功）。M0 提供链路探针。
package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/operations/api"
	"github.com/superailabs/miaoqia/services/subsystems/operations/internal/app"
	"github.com/superailabs/miaoqia/services/subsystems/operations/migrations"
)

// Module 返回 operations 的模块定义。
func Module() runtime.Module {
	probes := &app.Probes{}
	return runtime.Module{
		Name:       api.Name,
		Database:   "ops",
		Migrations: migrations.FS,
		Start: func(ctx context.Context, env *runtime.Env) error {
			probes.DB = env.DB
			a, ok := runtime.Lookup[identity.Authorizer](env.Bindings, identity.AuthorizerBinding)
			if !ok {
				return fmt.Errorf("operations：缺少 %s 绑定", identity.AuthorizerBinding)
			}
			probes.Authz = a
			return nil
		},
		Routes: func(env *runtime.Env, public, _ *http.ServeMux) {
			public.HandleFunc("POST /v1/operations/probes", func(w http.ResponseWriter, r *http.Request) {
				pr, err := probes.Trigger(r.Context())
				if err != nil {
					problem(w, err)
					return
				}
				reply(w, http.StatusAccepted, pr)
			})
			public.HandleFunc("GET /v1/operations/probes/{id}", func(w http.ResponseWriter, r *http.Request) {
				if err := authz.Require(r.Context(), probes.Authz, api.ProbeRelation, "platform:miaoqia"); err != nil {
					problem(w, err)
					return
				}
				pr, err := probes.Get(r.Context(), r.PathValue("id"))
				if err != nil {
					problem(w, err)
					return
				}
				reply(w, http.StatusOK, pr)
			})
		},
		Consumers: func(env *runtime.Env) []*consumer.Consumer {
			return []*consumer.Consumer{{
				Name: "operations.probe-completion", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
				Topics:   []string{"public.evidence"},
				Handlers: map[string]consumer.Handler{api.ProbeForwarded: probes.Complete},
			}}
		},
	}
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// problem 以 RFC 9457 Problem Details 返回错误。
func problem(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, authz.ErrForbidden):
		status, code = http.StatusForbidden, "forbidden"
	case errors.Is(err, app.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://errors.miaoqia.com/" + code, "title": http.StatusText(status),
		"status": status, "code": code})
}
