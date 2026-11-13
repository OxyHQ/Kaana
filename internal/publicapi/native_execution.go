package publicapi

import (
	"context"
	"errors"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// executeNative holds only the terminal framing in memory, never output. The
// provider's content-free usage and cost evidence is committed before success
// is acknowledged. A lost process can replay settlement without re-execution.
func (s *Server) executeNative(ctx context.Context, a *admission, request *contract.Request, sink kaana.Sink) kaana.Result {
	callerContext := ctx
	ctx, cancel := context.WithDeadline(ctx, a.native.LeaseUntil)
	defer cancel()
	if a.native.Plan.OutputTokens > 0 {
		request.MaxOutputTokens = &a.native.Plan.OutputTokens
	}
	var terminal contract.StreamEvent
	nextSequence := 0
	held := func(event contract.StreamEvent) error {
		if start, ok := event.(*contract.StreamStartEvent); ok && start.GenerationID != nil {
			if err := s.native.Store.BindGeneration(ctx, a.native.ID, *start.GenerationID); err != nil {
				return err
			}
		}
		nextSequence = event.Sequence() + 1
		switch event.(type) {
		case *contract.StreamDoneEvent, *contract.StreamErrorEvent:
			terminal = event
			return nil
		}
		return sink(event)
	}
	started := time.Now()
	result := s.executor.Execute(kaana.WithCacheWritePriced(kaana.DeferCostRecording(ctx), s.cacheWritePricedAdmission(a)), request, held)
	traceFrom(ctx).executed(time.Since(started), result.Failure)
	report := result.Report
	var persistErr error
	if report == nil {
		if len(result.UpstreamCost.Attempts) > 0 {
			persistErr = errors.New("native completion lacks provider usage")
		} else {
			report = a.unservedReport(result.Failure, s.now())
		}
	}
	if persistErr == nil {
		persistErr = s.completeNative(ctx, a, *report, result.UpstreamCost)
	}
	if persistErr != nil {
		s.logger.Error("native completion was not persisted", "requestId", a.requestID, "admissionId", a.native.ID, "errorType", "completion_persistence")
		result.Failure = contract.NewError(a.requestID, contract.CodeServiceUnavailable, "the completion could not be durably recorded; do not repeat inference with another idempotency key")
		sequence := nextSequence
		if terminal != nil {
			sequence = terminal.Sequence()
		}
		terminal = &contract.StreamErrorEvent{SchemaVersion: contract.SchemaVersion, Type: contract.EventError, RequestID: a.requestID, Seq: sequence, Error: *result.Failure}
	}
	if result.Failure != nil {
		s.logFailure(a, result)
	} else if a.affinity != nil {
		for _, attempt := range result.UpstreamCost.Attempts {
			if attempt.Served {
				s.affinity.set(*a.affinity, attempt.DeploymentID, s.now())
			}
		}
	}
	if terminal != nil && callerContext.Err() == nil {
		if err := sink(terminal); err != nil && result.Failure == nil {
			result.Failure = contract.NewError(a.requestID, contract.CodeCancelled, contract.PublicMessage("cancelled"))
		}
	}
	return result
}

// RunNativeReconciliation resumes durable completion records after startup and
// periodically while serving. Its caller owns the process shutdown context.
func (s *Server) RunNativeReconciliation(ctx context.Context) error {
	if s.native == nil {
		return nil
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		bounded, cancel := context.WithTimeout(ctx, settlementWriteTimeout)
		var err error
		if s.native.Recover != nil {
			err = s.native.Recover(bounded, 100)
		} else {
			_, err = s.native.Store.ReplayCompletions(bounded, 100)
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("native completion replay pending", "errorType", "settlement_pending")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// completeNative durably records only usage evidence, then attempts settlement.
// Settlement can safely be retried by the durable queue; inference cannot.
func (s *Server) completeNative(ctx context.Context, a *admission, report contract.UsageReport, cost providercost.Record) error {
	events, err := providercost.Events(cost)
	if err != nil {
		return err
	}
	writing, stop := context.WithTimeout(context.WithoutCancel(ctx), settlementWriteTimeout)
	defer stop()
	if err := s.native.Store.StageCompletion(writing, a.native.ID, report, events); err != nil {
		return err
	}
	if err := s.native.Store.Settle(writing, a.native.ID, report, events); err != nil {
		s.logger.Warn("native completion awaits durable replay", "requestId", a.requestID, "admissionId", a.native.ID, "errorType", "settlement_pending")
	}
	return nil
}
