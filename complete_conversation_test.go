package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// shrinkCompleteRetry makes completeConversationWithRetry fast for a test.
func shrinkCompleteRetry(t *testing.T) {
	t.Helper()
	attempts, timeout, backoff := completeConversationAttempts, completeConversationAttemptTimeout, completeConversationBackoff
	completeConversationAttempts = 3
	completeConversationAttemptTimeout = 200 * time.Millisecond
	completeConversationBackoff = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	t.Cleanup(func() {
		completeConversationAttempts, completeConversationAttemptTimeout, completeConversationBackoff = attempts, timeout, backoff
	})
}

func TestCompleteConversationWithRetry(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "try again")
	tests := []struct {
		name      string
		errs      []error // returned by successive attempts; nil = success
		wantCalls int
		wantErr   error
	}{
		{name: "succeeds first try", errs: []error{nil}, wantCalls: 1},
		{name: "retries unavailable then succeeds", errs: []error{unavailable, nil}, wantCalls: 2},
		{name: "retries deadline exceeded", errs: []error{status.Error(codes.DeadlineExceeded, "slow"), nil}, wantCalls: 2},
		{name: "retries bare context deadline", errs: []error{context.DeadlineExceeded, nil}, wantCalls: 2},
		{name: "gives up after max attempts", errs: []error{unavailable, unavailable, unavailable, nil}, wantCalls: 3, wantErr: unavailable},
		{name: "not found is not retried", errs: []error{status.Error(codes.NotFound, "gone"), nil}, wantCalls: 1, wantErr: status.Error(codes.NotFound, "gone")},
		{name: "failed precondition is not retried", errs: []error{status.Error(codes.FailedPrecondition, "done"), nil}, wantCalls: 1, wantErr: status.Error(codes.FailedPrecondition, "done")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shrinkCompleteRetry(t)
			calls := 0
			err := completeConversationWithRetry(context.Background(), func(ctx context.Context) error {
				_, hasDeadline := ctx.Deadline()
				assert.True(t, hasDeadline, "each attempt must run under its own timeout")
				e := tt.errs[calls]
				calls++
				return e
			})
			assert.Equal(t, tt.wantCalls, calls)
			if tt.wantErr == nil {
				assert.NoError(t, err)
			} else {
				assert.Equal(t, status.Code(tt.wantErr), status.Code(err))
			}
		})
	}
}

func TestCompleteConversationWithRetry_StopsWhenParentDone(t *testing.T) {
	shrinkCompleteRetry(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := completeConversationWithRetry(ctx, func(context.Context) error {
		calls++
		cancel()
		return status.Error(codes.Unavailable, "down")
	})
	assert.Equal(t, 1, calls, "no retries once the parent ctx is done")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestStopAgentAssist_CompletesDespiteCancelledRequestContext(t *testing.T) {
	srv, _, fake := newTestAgentAssistServer(t)
	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil, agentAssistTarget{})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client already gave up

	_, err = srv.StopAgentAssist(ctx, "call-1")
	require.NoError(t, err)
	require.Equal(t, 1, fake.completions())
	assert.NoError(t, fake.completeCtxErrs[0], "Complete must not see the request's cancellation")
}

func TestAgentAssistAPI_StopCompletesAfterClientDisconnect(t *testing.T) {
	srv, _, fake := newTestAgentAssistServer(t)
	cfg := *srv.cfg
	api := NewAPIServer(&cfg, srv, testLogger())

	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil, agentAssistTarget{})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/agent-assist/stop", strings.NewReader(`{"call_id":"call-1"}`)).WithContext(ctx)
	api.server.Handler.ServeHTTP(httptest.NewRecorder(), req)

	require.Equal(t, 1, fake.completions())
	assert.NoError(t, fake.completeCtxErrs[0])
}

func TestReapOnce_FinalizesStaleSession(t *testing.T) {
	srv, sess, fake := newTestAgentAssistServer(t)
	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil, agentAssistTarget{})
	require.NoError(t, err)
	metaBefore := len(srv.metaUploader.(*captureUploader).Enqueued())

	srv.reapOnce(sess.CreatedAt.Add(time.Duration(srv.cfg.MaxCallDurationHours)*time.Hour + time.Minute))
	srv.reaperWG.Wait()

	assert.False(t, srv.sessions.Exists("call-1"), "a stale session must be removed from the store")
	assert.Equal(t, 1, fake.completions(), "a stale session's Agent Assist conversation must be completed")
	assert.Greater(t, len(srv.metaUploader.(*captureUploader).Enqueued()), metaBefore, "its final segment metadata must be enqueued")
	sess.mu.Lock()
	assert.True(t, sess.closed)
	sess.mu.Unlock()
}

func TestReapOnce_FlagsOnlyWhenFinalizeDisabled(t *testing.T) {
	srv, sess, fake := newTestAgentAssistServer(t)
	srv.cfg.FinalizeStaleSessions = false
	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil, agentAssistTarget{})
	require.NoError(t, err)

	srv.reapOnce(sess.CreatedAt.Add(time.Duration(srv.cfg.MaxCallDurationHours)*time.Hour + time.Minute))
	srv.reaperWG.Wait()

	assert.True(t, srv.sessions.Exists("call-1"))
	assert.Equal(t, 0, fake.completions())
}

func TestReapOnce_FinalizesIdleSessionOnlyWhenAllLegsIdle(t *testing.T) {
	srv, sess, fake := newTestAgentAssistServer(t)
	srv.cfg.SessionIdleTimeoutSec = 30
	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil, agentAssistTarget{})
	require.NoError(t, err)

	now := time.Now()
	stale := now.Add(-time.Minute).UnixNano()

	// Only one leg idle: the call is still alive.
	sess.Legs[0].lastPacketAt.Store(stale)
	sess.Legs[1].lastPacketAt.Store(now.UnixNano())
	srv.reapOnce(now)
	srv.reaperWG.Wait()
	assert.True(t, srv.sessions.Exists("call-1"))
	assert.Equal(t, 0, fake.completions())

	// Both legs idle: finalize.
	sess.Legs[1].lastPacketAt.Store(stale)
	srv.reapOnce(now)
	srv.reaperWG.Wait()
	assert.False(t, srv.sessions.Exists("call-1"))
	assert.Equal(t, 1, fake.completions())
}

func TestReapOnce_IdleDisabledByDefault(t *testing.T) {
	srv, sess, _ := newTestAgentAssistServer(t)
	require.Zero(t, srv.cfg.SessionIdleTimeoutSec)
	for _, leg := range sess.Legs {
		leg.lastPacketAt.Store(time.Now().Add(-time.Hour).UnixNano())
	}
	srv.reapOnce(time.Now())
	srv.reaperWG.Wait()
	assert.True(t, srv.sessions.Exists("call-1"))
}

// TestClaimSession_ByeRacingReaperFinalizesOnce races a BYE-style claim
// against the reaper's claim: exactly one wins, and the Agent Assist
// conversation is completed exactly once.
func TestClaimSession_ByeRacingReaperFinalizesOnce(t *testing.T) {
	for i := 0; i < 50; i++ {
		srv, sess, fake := newTestAgentAssistServer(t)
		_, err := srv.StartAgentAssist(context.Background(), "call-1", nil, agentAssistTarget{})
		require.NoError(t, err)

		var wins atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if claimed, ok := srv.claimSession("call-1"); ok {
				wins.Add(1)
				srv.finalizeSession(context.Background(), claimed, time.Now().UTC().Format(time.RFC3339Nano), nil, "bye")
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			srv.reapOnce(sess.CreatedAt.Add(time.Duration(srv.cfg.MaxCallDurationHours)*time.Hour + time.Minute))
		}()
		close(start)
		wg.Wait()
		srv.reaperWG.Wait()

		assert.LessOrEqual(t, wins.Load(), int32(1))
		assert.Equal(t, 1, fake.completions(), "trial %d: conversation must be completed exactly once", i)
	}
}

func TestStop_CompletesAgentAssistSessionsInParallel(t *testing.T) {
	const n = 8
	const delay = 200 * time.Millisecond

	srv, _, fake := newTestAgentAssistServer(t)
	srv.reaperStop = make(chan struct{})
	fake.completeDelay = delay

	// Gather n Agent Assist sessions into srv's store.
	srv.sessions.DrainAll()
	for i := 0; i < n; i++ {
		other, sess := newTestSplitServerWithCallID(t, fmt.Sprintf("call-%d", i))
		other.assist = fake
		_, err := other.StartAgentAssist(context.Background(), sess.CallID, nil, agentAssistTarget{})
		require.NoError(t, err)
		srv.sessions.Set(sess.CallID, sess)
	}

	started := time.Now()
	srv.Stop()
	elapsed := time.Since(started)

	assert.Equal(t, n, fake.completions(), "every session's conversation must be completed on shutdown")
	assert.Less(t, elapsed, time.Duration(n)*delay/2, "shutdown must finalize sessions in parallel, not one at a time")
}

func TestCompleteAgentAssist_LogsFailureWithoutPanicking(t *testing.T) {
	srv, _, fake := newTestAgentAssistServer(t)
	fake.completeErr = errors.New("boom")
	_, err := srv.StartAgentAssist(context.Background(), "call-1", nil, agentAssistTarget{})
	require.NoError(t, err)

	_, err = srv.StopAgentAssist(context.Background(), "call-1")
	require.NoError(t, err, "a failed Complete is logged, not surfaced as a stop failure")
	assert.Equal(t, 1, fake.completions())

	// nil run is a no-op.
	srv.completeAgentAssist(context.Background(), nil, "call-1", "test")
}

// TestGoogleStart_InvalidMetadataFailsBeforeCreatingConversation guards the
// ordering in googleAgentAssistClient.Start: metadata that can't be converted
// must be rejected before CreateConversation, since a conversation created
// and then abandoned on that error was never completed and sat open on
// Google's side until its 24h timeout. The Dialogflow clients are nil here,
// so reaching CreateConversation at all would panic.
func TestGoogleStart_InvalidMetadataFailsBeforeCreatingConversation(t *testing.T) {
	c := &googleAgentAssistClient{cfg: &Config{AgentAssistProjectID: "p", AgentAssistConversationProfileID: "prof"}, log: testLogger()}

	var run *agentAssistRun
	var err error
	require.NotPanics(t, func() {
		run, err = c.Start(context.Background(), AgentAssistStartRequest{
			CallID:   "call-1",
			Labels:   []string{"inbound", "outbound"},
			Metadata: map[string]any{"bad": make(chan int)}, // not representable as a structpb value
		})
	}, "Start must not reach CreateConversation with invalid metadata")
	assert.Nil(t, run)
	assert.ErrorContains(t, err, "convert agent assist metadata")
}
