package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/policy"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	searchapi "github.com/superailabs/miaoqia/services/subsystems/search/api"
)

const (
	PolicyActivated  = "customer_context.policy.activated.v1"
	PolicyRolledBack = "customer_context.policy.rolled_back.v1"
	ObjectiveChanged = "customer_context.objective.changed.v1"
)

// PolicyChanged refreshes search's percolator registrations after activation or rollback.
// The event payload intentionally carries no policy document; the authoritative active-policy
// read supplies the complete document and generation for the tenant in this event context.
func (s *Service) PolicyChanged(ctx context.Context, _ pgx.Tx, _ *outbox.Envelope) error {
	if s.Watches == nil {
		return errors.New("订阅反查索引未装配")
	}
	policies, err := s.readPolicies(ctx)
	if err != nil {
		return err
	}
	rc, _ := reqctx.From(ctx)
	var profileVersion *customercontext.Version
	snapshots := map[string]*customercontext.Snapshot{}
	for _, active := range policies {
		if active.ObjectiveState != "active" {
			continue
		}
		var document policy.Policy
		raw, err := policyDocumentJSON(active.Document)
		if err != nil {
			return fmt.Errorf("目标 %s 策略无法编码：%w", active.ObjectiveID, err)
		}
		if err := json.Unmarshal(raw, &document); err != nil {
			return fmt.Errorf("目标 %s 策略无法解析：%w", active.ObjectiveID, err)
		}
		factTypes := document.Scope.FactTypes
		if len(factTypes) == 0 {
			factTypes = document.FactTypes
		}
		if len(factTypes) == 0 {
			continue
		}
		regions := stringList(document.ParamValues["regions"])
		categories := stringList(document.ParamValues["categories"])
		if (len(regions) == 0 || len(categories) == 0) && s.Context != nil {
			if profileVersion == nil {
				var err error
				profileVersion, err = s.Context.ContextVersion(ctx)
				if err != nil {
					return fmt.Errorf("读取上下文版本：%w", err)
				}
			}
			snapshot := snapshots[active.OrgUnitID]
			if snapshot == nil {
				var err error
				snapshot, err = s.Context.ContextSnapshot(ctx, active.OrgUnitID, profileVersion.ContextVersion)
				if err != nil {
					return fmt.Errorf("读取上下文快照：%w", err)
				}
				snapshots[active.OrgUnitID] = snapshot
			}
			if len(regions) == 0 {
				regions = append([]string(nil), snapshot.PolicyView.ServiceRegions...)
			}
			if len(categories) == 0 {
				categories = append([]string(nil), snapshot.PolicyView.OfferingCategories...)
			}
		}
		if err := s.Watches.Register(ctx, searchapi.WatchQuery{ID: active.ObjectiveID, Tenant: rc.Tenant, Cell: rc.Cell,
			FactTypes: factTypes, Regions: regions, Categories: categories}); err != nil {
			return fmt.Errorf("目标 %s 订阅注册失败：%w", active.ObjectiveID, err)
		}
	}
	return nil
}

// policyDocumentJSON normalizes the generated PolicyDocument representation.
// Its additional JSON properties are emitted under the Go field name by the
// generated encoder, while kernel/policy expects those properties at the root.
func policyDocumentJSON(document any) ([]byte, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if extras, ok := fields["AdditionalProps"]; ok {
		delete(fields, "AdditionalProps")
		var props map[string]json.RawMessage
		if err := json.Unmarshal(extras, &props); err != nil {
			return nil, err
		}
		for key, value := range props {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}

// ObjectiveChanged removes a target's reverse-match registration when the target
// is deactivated or disabled. The active policy remains in customer-context for
// history, so relying on ActivePolicies alone would leave a stale watch query.
func (s *Service) ObjectiveChanged(ctx context.Context, _ pgx.Tx, env *outbox.Envelope) error {
	if s.Watches == nil {
		return errors.New("订阅反查索引未装配")
	}
	var event struct {
		ObjectiveID string `json:"objective_id"`
		State       string `json:"state"`
	}
	if err := json.Unmarshal(env.Data, &event); err != nil {
		return fmt.Errorf("目标变更事件无法解析：%w", err)
	}
	if event.ObjectiveID == "" {
		event.ObjectiveID = env.Subject
	}
	if event.ObjectiveID == "" {
		return errors.New("目标变更事件缺少 objective_id")
	}
	if event.State != "inactive" && event.State != "disabled" {
		return nil
	}
	if err := s.Watches.Remove(ctx, event.ObjectiveID); err != nil {
		return fmt.Errorf("目标 %s 订阅移除失败：%w", event.ObjectiveID, err)
	}
	return nil
}

func stringList(value any) []string {
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, item := range values {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
