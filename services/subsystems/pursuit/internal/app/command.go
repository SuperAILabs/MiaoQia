package app

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// CommandResult 是契约 CommandResult 的 JSON。
type CommandResult struct {
	OperationID       string       `json:"operation_id"`
	Result            string       `json:"result"`
	Pursuit           *Summary     `json:"pursuit,omitempty"`
	ExistingOwner     string       `json:"existing_owner,omitempty"`
	NotWritten        []string     `json:"not_written,omitempty"`
	Authorization     string       `json:"authorization,omitempty"`
	Task              *TaskView    `json:"task,omitempty"`
	Outcome           *OutcomeView `json:"outcome,omitempty"`
	SignalRestoration string       `json:"signal_restoration,omitempty"`
}

// project 按调用者当前的权限投影固定的结果（每次返回前，含重放）：能查看跟进时给出摘要、任务与结果；不能查看已有跟进时只给负责人
// （PRD 第6段 F2 第2条）；其余情况只给操作标识与结果。授权状态为返回时的状态。
func (s *Service) project(ctx context.Context, rc reqctx.Context, f *Fixed) (*CommandResult, error) {
	out := &CommandResult{OperationID: f.OperationID, Result: f.Result, NotWritten: f.NotWritten, SignalRestoration: f.SignalRestoration}
	if f.PursuitID == "" {
		return out, nil
	}
	var p *Pursuit
	var next, task *Task
	var outcome *OutcomeView
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if p, err = loadPursuit(ctx, tx, f.PursuitID, false); err != nil || p == nil {
			return err
		}
		if next, err = nextTask(ctx, tx, p.ID); err != nil {
			return err
		}
		if f.TaskID != "" {
			if task, err = loadTask(ctx, tx, f.TaskID, false); err != nil {
				return err
			}
		}
		if f.OutcomeID != "" {
			if outcome, err = loadOutcome(ctx, tx, f.OutcomeID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || p == nil {
		return out, err
	}
	r, err := s.rights(ctx, rc, p, unitRights{})
	if err != nil {
		return nil, err
	}
	switch {
	case p.State == "voided":
		out.Authorization = "effective"
		if p.Applied < p.Participation {
			out.Authorization = "cleaning"
		}
	case p.Applied < p.Participation:
		out.Authorization = "syncing"
	default:
		out.Authorization = "effective"
	}
	if !r.View {
		if f.Result == "existing" || f.Result == "existing_linked" {
			out.ExistingOwner = p.Owner
		}
		out.Authorization = ""
		return out, nil
	}
	sum := s.summary(p, next)
	out.Pursuit = &sum
	if task != nil {
		v := task.view(p)
		out.Task = &v
	}
	out.Outcome = outcome
	return out, nil
}

// run 执行一个命令：记录尝试，执行，按当前权限投影。
func (s *Service) run(ctx context.Context, rc reqctx.Context, scope, key, fp string, fn func() (*Fixed, error)) (*CommandResult, error) {
	f, err := s.attempt(ctx, rc, scope, key, fp, fn)
	if err != nil {
		return nil, err
	}
	return s.project(ctx, rc, f)
}
