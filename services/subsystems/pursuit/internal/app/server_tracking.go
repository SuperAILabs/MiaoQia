package app

import (
	"context"

	pur "github.com/superailabs/miaoqia/services/contracts/apis/pursuit"
)

// 持续跟踪与创建缺省值（C-10）的服务端实现。

func trackingResult(res *TrackingResult, err error) (*pur.TrackingCommandResult, error) {
	if err != nil {
		return nil, err
	}
	return convert[pur.TrackingCommandResult](res)
}

func (s *Server) GetCreateDefaults(ctx context.Context, p pur.GetCreateDefaultsParams) (*pur.CreateDefaults, error) {
	d, err := s.Service.GetCreateDefaults(ctx, string(p.ObjectiveID), string(p.SignalID.Or("")))
	if err != nil {
		return nil, err
	}
	return convert[pur.CreateDefaults](d)
}

func (s *Server) CreateTracking(ctx context.Context, req *pur.CreateTrackingRequest, p pur.CreateTrackingParams) (*pur.TrackingCommandResult, error) {
	in, err := convert[CreateTrackingRequest](req)
	if err != nil {
		return nil, err
	}
	return trackingResult(s.Service.CreateTracking(ctx, p.IdempotencyKey, *in))
}

func (s *Server) GetTracking(ctx context.Context, p pur.GetTrackingParams) (*pur.Tracking, error) {
	d, err := s.Service.GetTracking(ctx, string(p.PursuitID))
	if err != nil {
		return nil, err
	}
	return convert[pur.Tracking](d)
}

func (s *Server) ListTracking(ctx context.Context, p pur.ListTrackingParams) (*pur.TrackingList, error) {
	list, err := s.Service.ListTracking(ctx, string(p.ObjectiveID.Or("")), p.Limit.Or(50), string(p.Cursor.Or("")))
	if err != nil {
		return nil, err
	}
	return convert[pur.TrackingList](list)
}

func (s *Server) ListTrackingPeriods(ctx context.Context, p pur.ListTrackingPeriodsParams) (*pur.TrackingPeriodList, error) {
	list, err := s.Service.ListTrackingPeriods(ctx, string(p.PursuitID), p.Limit.Or(50), string(p.Cursor.Or("")))
	if err != nil {
		return nil, err
	}
	return convert[pur.TrackingPeriodList](list)
}

func (s *Server) GetTrackingPeriod(ctx context.Context, p pur.GetTrackingPeriodParams) (*pur.TrackingPeriod, error) {
	d, err := s.Service.GetTrackingPeriod(ctx, string(p.PursuitID), p.Seq)
	if err != nil {
		return nil, err
	}
	return convert[pur.TrackingPeriod](d)
}

func (s *Server) PauseTracking(ctx context.Context, req *pur.VersionRequest, p pur.PauseTrackingParams) (*pur.TrackingCommandResult, error) {
	in, err := convert[VersionRequest](req)
	if err != nil {
		return nil, err
	}
	return trackingResult(s.Service.PauseTracking(ctx, string(p.PursuitID), p.IdempotencyKey, *in))
}

func (s *Server) ResumeTracking(ctx context.Context, req *pur.VersionRequest, p pur.ResumeTrackingParams) (*pur.TrackingCommandResult, error) {
	in, err := convert[VersionRequest](req)
	if err != nil {
		return nil, err
	}
	return trackingResult(s.Service.ResumeTracking(ctx, string(p.PursuitID), p.IdempotencyKey, *in))
}

func (s *Server) StopTracking(ctx context.Context, req *pur.VersionRequest, p pur.StopTrackingParams) (*pur.TrackingCommandResult, error) {
	in, err := convert[VersionRequest](req)
	if err != nil {
		return nil, err
	}
	return trackingResult(s.Service.StopTracking(ctx, string(p.PursuitID), p.IdempotencyKey, *in))
}

func (s *Server) UpdateTrackingMembers(ctx context.Context, req *pur.TrackingMembersRequest, p pur.UpdateTrackingMembersParams) (*pur.TrackingCommandResult, error) {
	in, err := convert[TrackingMembersRequest](req)
	if err != nil {
		return nil, err
	}
	return trackingResult(s.Service.UpdateTrackingMembers(ctx, string(p.PursuitID), p.IdempotencyKey, *in))
}
